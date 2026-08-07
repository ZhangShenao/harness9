package mission

import (
	"context"
	"database/sql"
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
