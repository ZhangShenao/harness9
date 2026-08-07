package mission

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// CreatePlan creates a new draft Plan for a Mission.
func (s *Store) CreatePlan(ctx context.Context, missionID, tasksJSON string) (Plan, error) {
	if tasksJSON == "" {
		return Plan{}, fmt.Errorf("plan tasks JSON is required")
	}
	var version int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM plans WHERE mission_id = ?`, missionID).Scan(&version); err != nil {
		return Plan{}, fmt.Errorf("count plans: %w", err)
	}
	version++
	now := time.Now().UTC()
	plan := Plan{ID: newID(), MissionID: missionID, Version: version, Status: PlanDraft, TasksJSON: tasksJSON, CreatedAt: now, UpdatedAt: now}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO plans (id, mission_id, version, status, tasks_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		plan.ID, plan.MissionID, plan.Version, plan.Status, plan.TasksJSON, unixMillis(now), unixMillis(now)); err != nil {
		return Plan{}, fmt.Errorf("insert plan: %w", err)
	}
	return plan, nil
}

// GetPlan reads a Plan by ID.
func (s *Store) GetPlan(ctx context.Context, id string) (Plan, error) {
	var p Plan
	var createdAt, updatedAt int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, mission_id, version, status, tasks_json, created_at, updated_at FROM plans WHERE id = ?`, id).
		Scan(&p.ID, &p.MissionID, &p.Version, &p.Status, &p.TasksJSON, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return Plan{}, ErrNotFound
	}
	if err != nil {
		return Plan{}, fmt.Errorf("get plan: %w", err)
	}
	p.CreatedAt = fromUnixMillis(createdAt)
	p.UpdatedAt = fromUnixMillis(updatedAt)
	return p, nil
}

// ApprovePlan marks a draft Plan as approved, creates an immutable PlanVersion,
// supersedes any previously active version, and updates the Mission's current_plan_version.
func (s *Store) ApprovePlan(ctx context.Context, planID string) (PlanVersion, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PlanVersion{}, fmt.Errorf("begin approve plan: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var missionID string
	var version int
	var tasksJSON string
	var status PlanStatus
	err = tx.QueryRowContext(ctx,
		`SELECT mission_id, version, tasks_json, status FROM plans WHERE id = ?`, planID).
		Scan(&missionID, &version, &tasksJSON, &status)
	if err == sql.ErrNoRows {
		return PlanVersion{}, ErrNotFound
	}
	if err != nil {
		return PlanVersion{}, fmt.Errorf("load plan: %w", err)
	}
	if status != PlanDraft {
		return PlanVersion{}, fmt.Errorf("%w: plan %s is %s not draft", ErrInvalidTransition, planID, status)
	}

	// supersede previous active versions
	if _, err := tx.ExecContext(ctx,
		`UPDATE plans SET status = ?, updated_at = ? WHERE mission_id = ? AND status = ?`,
		PlanSuperseded, unixMillis(time.Now().UTC()), missionID, PlanApproved); err != nil {
		return PlanVersion{}, fmt.Errorf("supersede old plans: %w", err)
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx,
		`UPDATE plans SET status = ?, updated_at = ? WHERE id = ?`,
		PlanApproved, unixMillis(now), planID); err != nil {
		return PlanVersion{}, fmt.Errorf("approve plan: %w", err)
	}

	pv := PlanVersion{ID: newID(), MissionID: missionID, PlanID: planID, Version: version, TasksJSON: tasksJSON, CreatedAt: now}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO plan_versions (id, mission_id, plan_id, version, tasks_json, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		pv.ID, pv.MissionID, pv.PlanID, pv.Version, pv.TasksJSON, unixMillis(now)); err != nil {
		return PlanVersion{}, fmt.Errorf("insert plan version: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE missions SET current_plan_version = ?, updated_at = ? WHERE id = ?`,
		pv.ID, unixMillis(now), missionID); err != nil {
		return PlanVersion{}, fmt.Errorf("update mission plan version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return PlanVersion{}, fmt.Errorf("commit approve plan: %w", err)
	}
	return pv, nil
}

// GetActivePlanVersion returns the Mission's current (most recently approved) PlanVersion.
func (s *Store) GetActivePlanVersion(ctx context.Context, missionID string) (PlanVersion, error) {
	var pv PlanVersion
	var createdAt int64
	var missionPlanVersion sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT current_plan_version FROM missions WHERE id = ?`, missionID).Scan(&missionPlanVersion)
	if err == sql.ErrNoRows {
		return PlanVersion{}, ErrNotFound
	}
	if err != nil {
		return PlanVersion{}, fmt.Errorf("load mission plan version ref: %w", err)
	}
	if !missionPlanVersion.Valid || missionPlanVersion.String == "" {
		return PlanVersion{}, ErrNotFound
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT id, mission_id, plan_id, version, tasks_json, created_at FROM plan_versions WHERE id = ?`,
		missionPlanVersion.String).
		Scan(&pv.ID, &pv.MissionID, &pv.PlanID, &pv.Version, &pv.TasksJSON, &createdAt)
	if err == sql.ErrNoRows {
		return PlanVersion{}, ErrNotFound
	}
	if err != nil {
		return PlanVersion{}, fmt.Errorf("get plan version: %w", err)
	}
	pv.CreatedAt = fromUnixMillis(createdAt)
	return pv, nil
}

// ListPlanVersions returns all PlanVersions for a Mission in version order.
func (s *Store) ListPlanVersions(ctx context.Context, missionID string) ([]PlanVersion, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, mission_id, plan_id, version, tasks_json, created_at FROM plan_versions WHERE mission_id = ? ORDER BY version`, missionID)
	if err != nil {
		return nil, fmt.Errorf("list plan versions: %w", err)
	}
	defer rows.Close()
	var versions []PlanVersion
	for rows.Next() {
		var pv PlanVersion
		var createdAt int64
		if err := rows.Scan(&pv.ID, &pv.MissionID, &pv.PlanID, &pv.Version, &pv.TasksJSON, &createdAt); err != nil {
			return nil, fmt.Errorf("scan plan version: %w", err)
		}
		pv.CreatedAt = fromUnixMillis(createdAt)
		versions = append(versions, pv)
	}
	return versions, rows.Err()
}

// CreateChangeRequest records a pending Plan change proposal.
func (s *Store) CreateChangeRequest(ctx context.Context, cr PlanChangeRequest) (PlanChangeRequest, error) {
	if cr.MissionID == "" || cr.Reason == "" {
		return PlanChangeRequest{}, fmt.Errorf("mission ID and reason are required")
	}
	affectedJSON, _ := json.Marshal(cr.AffectedTasks)
	addedJSON, _ := json.Marshal(cr.AddedTasks)
	cr.ID = newID()
	cr.Status = ChangePending
	cr.CreatedAt = time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO plan_change_requests (id, mission_id, reason, trigger_attempt_id, affected_tasks, added_tasks, proposed_plan_json, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cr.ID, cr.MissionID, cr.Reason, cr.TriggerAttemptID, string(affectedJSON), string(addedJSON),
		cr.ProposedPlanJSON, cr.Status, unixMillis(cr.CreatedAt)); err != nil {
		return PlanChangeRequest{}, fmt.Errorf("insert change request: %w", err)
	}
	return cr, nil
}

// GetChangeRequest reads a PlanChangeRequest by ID.
func (s *Store) GetChangeRequest(ctx context.Context, id string) (PlanChangeRequest, error) {
	var cr PlanChangeRequest
	var affectedJSON, addedJSON string
	var reviewedAt sql.NullInt64
	var createdAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, mission_id, reason, trigger_attempt_id, affected_tasks, added_tasks,
			proposed_plan_json, status, reviewed_by, reviewed_at, review_reason, created_at
		FROM plan_change_requests WHERE id = ?`, id).
		Scan(&cr.ID, &cr.MissionID, &cr.Reason, &cr.TriggerAttemptID, &affectedJSON, &addedJSON,
			&cr.ProposedPlanJSON, &cr.Status, &cr.ReviewedBy, &reviewedAt, &cr.ReviewReason, &createdAt)
	if err == sql.ErrNoRows {
		return PlanChangeRequest{}, ErrNotFound
	}
	if err != nil {
		return PlanChangeRequest{}, fmt.Errorf("get change request: %w", err)
	}
	_ = json.Unmarshal([]byte(affectedJSON), &cr.AffectedTasks)
	_ = json.Unmarshal([]byte(addedJSON), &cr.AddedTasks)
	if reviewedAt.Valid {
		t := fromUnixMillis(reviewedAt.Int64)
		cr.ReviewedAt = &t
	}
	cr.CreatedAt = fromUnixMillis(createdAt)
	return cr, nil
}

// ReviewChangeRequest approves or rejects a pending PlanChangeRequest.
func (s *Store) ReviewChangeRequest(ctx context.Context, id string, status ChangeRequestStatus, reviewer, reason string) (PlanChangeRequest, error) {
	if status != ChangeApproved && status != ChangeRejected {
		return PlanChangeRequest{}, fmt.Errorf("invalid review status %q", status)
	}
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE plan_change_requests SET status = ?, reviewed_by = ?, reviewed_at = ?, review_reason = ?
		WHERE id = ? AND status = ?`,
		status, reviewer, unixMillis(now), reason, id, ChangePending)
	if err != nil {
		return PlanChangeRequest{}, fmt.Errorf("review change request: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return PlanChangeRequest{}, fmt.Errorf("%w: change request %s is not pending", ErrInvalidTransition, id)
	}
	return s.GetChangeRequest(ctx, id)
}

// ListPendingChangeRequests returns all pending change requests for a Mission.
func (s *Store) ListPendingChangeRequests(ctx context.Context, missionID string) ([]PlanChangeRequest, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, mission_id, reason, trigger_attempt_id, affected_tasks, added_tasks,
			proposed_plan_json, status, reviewed_by, reviewed_at, review_reason, created_at
		FROM plan_change_requests WHERE mission_id = ? AND status = ? ORDER BY created_at`, missionID, ChangePending)
	if err != nil {
		return nil, fmt.Errorf("list pending change requests: %w", err)
	}
	defer rows.Close()
	var reqs []PlanChangeRequest
	for rows.Next() {
		var cr PlanChangeRequest
		var affectedJSON, addedJSON string
		var reviewedAt sql.NullInt64
		var createdAt int64
		if err := rows.Scan(&cr.ID, &cr.MissionID, &cr.Reason, &cr.TriggerAttemptID, &affectedJSON, &addedJSON,
			&cr.ProposedPlanJSON, &cr.Status, &cr.ReviewedBy, &reviewedAt, &cr.ReviewReason, &createdAt); err != nil {
			return nil, fmt.Errorf("scan change request: %w", err)
		}
		_ = json.Unmarshal([]byte(affectedJSON), &cr.AffectedTasks)
		_ = json.Unmarshal([]byte(addedJSON), &cr.AddedTasks)
		if reviewedAt.Valid {
			t := fromUnixMillis(reviewedAt.Int64)
			cr.ReviewedAt = &t
		}
		cr.CreatedAt = fromUnixMillis(createdAt)
		reqs = append(reqs, cr)
	}
	return reqs, rows.Err()
}

// SetPolicy stores a Policy for a Mission (upsert).
func (s *Store) SetPolicy(ctx context.Context, missionID string, p Policy) error {
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal policy: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO policies (mission_id, policy_json) VALUES (?, ?)
		 ON CONFLICT(mission_id) DO UPDATE SET policy_json = excluded.policy_json`,
		missionID, string(data))
	if err != nil {
		return fmt.Errorf("set policy: %w", err)
	}
	return nil
}

// GetPolicy reads a Mission's Policy, returning DefaultPolicy if none set.
func (s *Store) GetPolicy(ctx context.Context, missionID string) (Policy, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT policy_json FROM policies WHERE mission_id = ?`, missionID).Scan(&data)
	if err == sql.ErrNoRows {
		return DefaultPolicy(), nil
	}
	if err != nil {
		return Policy{}, fmt.Errorf("get policy: %w", err)
	}
	var p Policy
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		return Policy{}, fmt.Errorf("unmarshal policy: %w", err)
	}
	return p, nil
}
