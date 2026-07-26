package mission

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// CreateDraftPlan persists a new editable Plan version and its Task graph.
func (s *Store) CreateDraftPlan(
	ctx context.Context,
	missionID string,
	input PlanInput,
	actor string,
) (*PlanVersion, error) {
	missionID = strings.TrimSpace(missionID)
	actor = strings.TrimSpace(actor)
	if missionID == "" {
		return nil, fmt.Errorf("plan mission ID is required")
	}
	if actor == "" {
		return nil, fmt.Errorf("plan actor is required")
	}
	planInput, err := normalizePlanInput(input)
	if err != nil {
		return nil, err
	}
	tasksJSON, err := json.Marshal(planInput)
	if err != nil {
		return nil, fmt.Errorf("marshal plan graph: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin plan transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	mission, err := scanMission(tx.QueryRowContext(ctx, `
		SELECT id, goal, acceptance_contract, budget_cents, policy_json,
		       current_plan_version, status, created_at, updated_at
		FROM missions WHERE id = ?`, missionID))
	if err != nil {
		return nil, wrapMissionNotFound(missionID, err)
	}
	now := s.currentTime()
	switch mission.Status {
	case MissionDraft, MissionAwaitingPlanApproval:
		if _, err := tx.ExecContext(ctx,
			`UPDATE missions SET status = ?, updated_at = ? WHERE id = ?`,
			MissionPlanning, unixMillis(now), missionID); err != nil {
			return nil, fmt.Errorf("move mission to planning: %w", err)
		}
	case MissionPlanning:
	default:
		return nil, fmt.Errorf(
			"%w: mission %s cannot create a draft plan from %s",
			ErrInvalidTransition,
			missionID,
			mission.Status,
		)
	}

	var version int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM mission_plan_versions WHERE mission_id = ?`,
		missionID,
	).Scan(&version); err != nil {
		return nil, fmt.Errorf("allocate plan version: %w", err)
	}
	planID := newID()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mission_plan_versions (
			id, mission_id, version, status, tasks_json, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		planID,
		missionID,
		version,
		PlanDraft,
		string(tasksJSON),
		unixMillis(now),
		unixMillis(now),
	); err != nil {
		return nil, fmt.Errorf("insert plan version: %w", err)
	}
	if err := insertPlanGraphTx(ctx, tx, missionID, version, planInput, now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE missions
		SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		MissionAwaitingPlanApproval,
		unixMillis(now),
		missionID,
		MissionPlanning,
	); err != nil {
		return nil, fmt.Errorf("move mission to plan approval: %w", err)
	}
	if err := insertPlanEvent(
		ctx,
		tx,
		missionID,
		"plan.draft_created",
		actor,
		version,
		now,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit draft plan: %w", err)
	}
	return s.GetPlan(ctx, missionID, version)
}

// UpdateDraftPlan atomically replaces the graph of an unapproved Plan version.
func (s *Store) UpdateDraftPlan(
	ctx context.Context,
	missionID string,
	version int,
	input PlanInput,
	actor string,
) (*PlanVersion, error) {
	missionID = strings.TrimSpace(missionID)
	actor = strings.TrimSpace(actor)
	if missionID == "" {
		return nil, fmt.Errorf("plan mission ID is required")
	}
	if version <= 0 {
		return nil, fmt.Errorf("plan version must be positive")
	}
	if actor == "" {
		return nil, fmt.Errorf("plan actor is required")
	}
	planInput, err := normalizePlanInput(input)
	if err != nil {
		return nil, err
	}
	tasksJSON, err := json.Marshal(planInput)
	if err != nil {
		return nil, fmt.Errorf("marshal plan graph: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin plan update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var status PlanStatus
	if err := tx.QueryRowContext(ctx, `
		SELECT status
		FROM mission_plan_versions
		WHERE mission_id = ? AND version = ?`,
		missionID,
		version,
	).Scan(&status); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("plan %s/%d: %w", missionID, version, ErrNotFound)
		}
		return nil, fmt.Errorf("read draft plan status: %w", err)
	}
	if status != PlanDraft {
		return nil, fmt.Errorf(
			"%w: plan %s/%d is %s and cannot be edited",
			ErrConflict,
			missionID,
			version,
			status,
		)
	}
	var missionStatus MissionStatus
	if err := tx.QueryRowContext(ctx,
		`SELECT status FROM missions WHERE id = ?`, missionID,
	).Scan(&missionStatus); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("mission %s: %w", missionID, ErrNotFound)
		}
		return nil, fmt.Errorf("read mission status: %w", err)
	}
	if missionStatus != MissionPlanning && missionStatus != MissionAwaitingPlanApproval {
		return nil, fmt.Errorf(
			"%w: mission %s cannot edit a draft plan from %s",
			ErrInvalidTransition,
			missionID,
			missionStatus,
		)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tasks WHERE mission_id = ? AND plan_version = ?`,
		missionID,
		version,
	); err != nil {
		return nil, fmt.Errorf("delete draft plan graph: %w", err)
	}
	now := s.currentTime()
	if err := insertPlanGraphTx(ctx, tx, missionID, version, planInput, now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE mission_plan_versions
		SET tasks_json = ?, updated_at = ?
		WHERE mission_id = ? AND version = ? AND status = ?`,
		string(tasksJSON),
		unixMillis(now),
		missionID,
		version,
		PlanDraft,
	); err != nil {
		return nil, fmt.Errorf("update draft plan: %w", err)
	}
	if missionStatus == MissionPlanning {
		if _, err := tx.ExecContext(ctx, `
			UPDATE missions SET status = ?, updated_at = ? WHERE id = ?`,
			MissionAwaitingPlanApproval,
			unixMillis(now),
			missionID,
		); err != nil {
			return nil, fmt.Errorf("move mission to plan approval: %w", err)
		}
	}
	if err := insertPlanEvent(
		ctx,
		tx,
		missionID,
		"plan.draft_updated",
		actor,
		version,
		now,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit draft plan update: %w", err)
	}
	return s.GetPlan(ctx, missionID, version)
}

// GetPlan returns one Plan version and a detached copy of its persisted graph.
func (s *Store) GetPlan(ctx context.Context, missionID string, version int) (*PlanVersion, error) {
	plan, err := getPlanFromQuerier(ctx, s.db, missionID, version)
	if err != nil {
		return nil, err
	}
	return plan, nil
}

// ListPlanVersions returns every Plan version for a Mission in version order.
func (s *Store) ListPlanVersions(ctx context.Context, missionID string) ([]PlanVersion, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT version
		FROM mission_plan_versions
		WHERE mission_id = ?
		ORDER BY version ASC`,
		missionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list plan versions: %w", err)
	}
	var versions []int
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan plan version number: %w", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate plan versions: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close plan versions: %w", err)
	}

	plans := make([]PlanVersion, 0, len(versions))
	for _, version := range versions {
		plan, err := getPlanFromQuerier(ctx, s.db, missionID, version)
		if err != nil {
			return nil, err
		}
		plans = append(plans, *plan)
	}
	return plans, nil
}

// CreatePlanChangeRequest persists an immutable execution-time Plan proposal.
func (s *Store) CreatePlanChangeRequest(
	ctx context.Context,
	missionID string,
	baseVersion int,
	proposed PlanInput,
	reason string,
	actor string,
) (*PlanChangeRequest, error) {
	missionID = strings.TrimSpace(missionID)
	reason = strings.TrimSpace(reason)
	actor = strings.TrimSpace(actor)
	if missionID == "" {
		return nil, fmt.Errorf("plan change mission ID is required")
	}
	if baseVersion <= 0 {
		return nil, fmt.Errorf("plan change base version must be positive")
	}
	if reason == "" {
		return nil, fmt.Errorf("plan change reason is required")
	}
	if actor == "" {
		return nil, fmt.Errorf("plan change actor is required")
	}
	planInput, err := normalizePlanInput(proposed)
	if err != nil {
		return nil, err
	}
	proposalJSON, err := json.Marshal(changeProposal{
		BaseVersion: baseVersion,
		Plan:        planInput,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal proposed plan: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin plan change request: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	mission, err := scanMission(tx.QueryRowContext(ctx, `
		SELECT id, goal, acceptance_contract, budget_cents, policy_json,
		       current_plan_version, status, created_at, updated_at
		FROM missions WHERE id = ?`, missionID))
	if err != nil {
		return nil, wrapMissionNotFound(missionID, err)
	}
	if mission.Status != MissionRunning && mission.Status != MissionVerifying {
		return nil, fmt.Errorf(
			"%w: mission %s cannot request a plan change from %s",
			ErrInvalidTransition,
			missionID,
			mission.Status,
		)
	}
	if mission.CurrentPlanVersion != baseVersion {
		return nil, fmt.Errorf(
			"%w: plan change base version %d does not match current version %d",
			ErrConflict,
			baseVersion,
			mission.CurrentPlanVersion,
		)
	}
	var baseStatus PlanStatus
	if err := tx.QueryRowContext(ctx, `
		SELECT status
		FROM mission_plan_versions
		WHERE mission_id = ? AND version = ?`,
		missionID,
		baseVersion,
	).Scan(&baseStatus); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("base plan %s/%d: %w", missionID, baseVersion, ErrNotFound)
		}
		return nil, fmt.Errorf("read base plan status: %w", err)
	}
	if baseStatus != PlanApproved {
		return nil, fmt.Errorf(
			"%w: base plan %s/%d is not approved",
			ErrConflict,
			missionID,
			baseVersion,
		)
	}

	now := s.currentTime()
	requestID := newID()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mission_change_requests (
			id, mission_id, reason, impacted_task_ids_json,
			proposed_plan_json, status, created_at, updated_at
		) VALUES (?, ?, ?, '[]', ?, ?, ?, ?)`,
		requestID,
		missionID,
		reason,
		string(proposalJSON),
		ChangeRequestPending,
		unixMillis(now),
		unixMillis(now),
	); err != nil {
		return nil, fmt.Errorf("insert plan change request: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"actor":        actor,
		"base_version": baseVersion,
		"reason":       reason,
		"request_id":   requestID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal plan change event: %w", err)
	}
	if err := insertEvent(ctx, tx, Event{
		ID:        newID(),
		MissionID: missionID,
		Type:      "plan_change.requested",
		Payload:   payload,
		CreatedAt: now,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit plan change request: %w", err)
	}
	return s.GetPlanChangeRequest(ctx, requestID)
}

// GetPlanChangeRequest returns one immutable Plan change proposal by ID.
func (s *Store) GetPlanChangeRequest(ctx context.Context, id string) (*PlanChangeRequest, error) {
	var (
		request      PlanChangeRequest
		impactedJSON string
		proposalJSON string
		createdAt    int64
		updatedAt    int64
		resolvedAt   sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, mission_id, trigger_attempt_id, reason,
		       impacted_task_ids_json, permission_change, budget_change,
		       proposed_plan_json, status, resolution_reason,
		       created_at, updated_at, resolved_at
		FROM mission_change_requests
		WHERE id = ?`,
		id,
	).Scan(
		&request.ID,
		&request.MissionID,
		&request.TriggerAttemptID,
		&request.Reason,
		&impactedJSON,
		&request.PermissionChange,
		&request.BudgetChange,
		&proposalJSON,
		&request.Status,
		&request.ResolutionReason,
		&createdAt,
		&updatedAt,
		&resolvedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("plan change request %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("scan plan change request: %w", err)
	}
	if err := json.Unmarshal([]byte(impactedJSON), &request.ImpactedTaskIDs); err != nil {
		return nil, fmt.Errorf("decode impacted task IDs: %w", err)
	}
	var proposal changeProposal
	if err := json.Unmarshal([]byte(proposalJSON), &proposal); err != nil {
		return nil, fmt.Errorf("decode proposed plan: %w", err)
	}
	request.BaseVersion = proposal.BaseVersion
	request.ProposedPlan = PlanVersion{
		MissionID: request.MissionID,
		Version:   proposal.BaseVersion,
		Status:    PlanDraft,
		Tasks:     cloneTaskInputs(proposal.Plan.Tasks),
	}
	request.CreatedAt = fromUnixMillis(createdAt)
	request.UpdatedAt = fromUnixMillis(updatedAt)
	if resolvedAt.Valid {
		value := fromUnixMillis(resolvedAt.Int64)
		request.ResolvedAt = &value
	}
	return &request, nil
}

func (s *Store) approvePlanTx(
	tx *sql.Tx,
	missionID string,
	version int,
	actor string,
) (*PlanVersion, error) {
	ctx := context.Background()
	mission, err := scanMission(tx.QueryRowContext(ctx, `
		SELECT id, goal, acceptance_contract, budget_cents, policy_json,
		       current_plan_version, status, created_at, updated_at
		FROM missions WHERE id = ?`,
		missionID,
	))
	if err != nil {
		return nil, wrapMissionNotFound(missionID, err)
	}
	if mission.Status != MissionAwaitingPlanApproval {
		return nil, fmt.Errorf(
			"%w: mission %s cannot approve a plan from %s",
			ErrInvalidTransition,
			missionID,
			mission.Status,
		)
	}
	var status PlanStatus
	if err := tx.QueryRowContext(ctx, `
		SELECT status
		FROM mission_plan_versions
		WHERE mission_id = ? AND version = ?`,
		missionID,
		version,
	).Scan(&status); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("plan %s/%d: %w", missionID, version, ErrNotFound)
		}
		return nil, fmt.Errorf("read plan for approval: %w", err)
	}
	if status != PlanDraft {
		return nil, fmt.Errorf(
			"%w: plan %s/%d is %s and cannot be approved",
			ErrConflict,
			missionID,
			version,
			status,
		)
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return nil, fmt.Errorf("plan approval actor is required")
	}
	now := s.currentTime()
	if _, err := tx.ExecContext(ctx, `
		UPDATE mission_plan_versions
		SET status = ?, approved_at = ?, updated_at = ?
		WHERE mission_id = ? AND version = ? AND status = ?`,
		PlanApproved,
		unixMillis(now),
		unixMillis(now),
		missionID,
		version,
		PlanDraft,
	); err != nil {
		return nil, fmt.Errorf("approve plan version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET status = ?, updated_at = ?
		WHERE mission_id = ? AND plan_version = ?`,
		TaskBlocked,
		unixMillis(now),
		missionID,
		version,
	); err != nil {
		return nil, fmt.Errorf("block approved plan tasks: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET status = ?, updated_at = ?
		WHERE mission_id = ? AND plan_version = ?
		  AND NOT EXISTS (
			  SELECT 1 FROM task_dependencies
			  WHERE task_dependencies.task_id = tasks.id
		  )`,
		TaskQueued,
		unixMillis(now),
		missionID,
		version,
	); err != nil {
		return nil, fmt.Errorf("queue approved root tasks: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE missions
		SET current_plan_version = ?, status = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		version,
		MissionReady,
		unixMillis(now),
		missionID,
		MissionAwaitingPlanApproval,
	); err != nil {
		return nil, fmt.Errorf("activate approved plan: %w", err)
	}
	if err := insertPlanEvent(ctx, tx, missionID, "plan.approved", actor, version, now); err != nil {
		return nil, err
	}
	return getPlanFromQuerier(ctx, tx, missionID, version)
}

type planQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getPlanFromQuerier(
	ctx context.Context,
	queryer planQuerier,
	missionID string,
	version int,
) (*PlanVersion, error) {
	var (
		plan       PlanVersion
		createdAt  int64
		updatedAt  int64
		approvedAt sql.NullInt64
	)
	err := queryer.QueryRowContext(ctx, `
		SELECT id, mission_id, version, status, created_at, updated_at, approved_at
		FROM mission_plan_versions
		WHERE mission_id = ? AND version = ?`,
		missionID,
		version,
	).Scan(
		&plan.ID,
		&plan.MissionID,
		&plan.Version,
		&plan.Status,
		&createdAt,
		&updatedAt,
		&approvedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("plan %s/%d: %w", missionID, version, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("scan plan version: %w", err)
	}
	tasks, err := scanPlanGraph(ctx, queryer, missionID, version)
	if err != nil {
		return nil, err
	}
	plan.Tasks = tasks
	plan.CreatedAt = fromUnixMillis(createdAt)
	plan.UpdatedAt = fromUnixMillis(updatedAt)
	if approvedAt.Valid {
		value := fromUnixMillis(approvedAt.Int64)
		plan.ApprovedAt = &value
	}
	return &plan, nil
}

func insertPlanGraphTx(
	ctx context.Context,
	tx *sql.Tx,
	missionID string,
	version int,
	input PlanInput,
	now time.Time,
) error {
	taskIDs := make(map[string]string, len(input.Tasks))
	for _, task := range input.Tasks {
		taskID := newID()
		taskIDs[task.ClientID] = taskID
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tasks (
				id, mission_id, title, status, plan_version,
				client_id, position, contract, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			taskID,
			missionID,
			task.Title,
			TaskBlocked,
			version,
			task.ClientID,
			task.Position,
			task.Contract,
			unixMillis(now),
			unixMillis(now),
		); err != nil {
			return fmt.Errorf("insert plan task %q: %w", task.ClientID, err)
		}
	}
	for _, task := range input.Tasks {
		for _, dependencyClientID := range task.Dependencies {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO task_dependencies (task_id, dependency_id)
				VALUES (?, ?)`,
				taskIDs[task.ClientID],
				taskIDs[dependencyClientID],
			); err != nil {
				return fmt.Errorf(
					"insert plan dependency %q -> %q: %w",
					task.ClientID,
					dependencyClientID,
					err,
				)
			}
		}
	}
	return nil
}

func scanPlanGraph(
	ctx context.Context,
	queryer planQuerier,
	missionID string,
	version int,
) ([]TaskInput, error) {
	rows, err := queryer.QueryContext(ctx, `
		SELECT id, client_id, position, title, contract
		FROM tasks
		WHERE mission_id = ? AND plan_version = ?
		ORDER BY position ASC, id ASC`,
		missionID,
		version,
	)
	if err != nil {
		return nil, fmt.Errorf("list plan tasks: %w", err)
	}
	type persistedTask struct {
		id    string
		input TaskInput
	}
	var persisted []persistedTask
	for rows.Next() {
		var task persistedTask
		if err := rows.Scan(
			&task.id,
			&task.input.ClientID,
			&task.input.Position,
			&task.input.Title,
			&task.input.Contract,
		); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan plan task: %w", err)
		}
		persisted = append(persisted, task)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate plan tasks: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close plan tasks: %w", err)
	}

	tasks := make([]TaskInput, 0, len(persisted))
	for _, task := range persisted {
		dependencyRows, err := queryer.QueryContext(ctx, `
			SELECT dependency.client_id
			FROM task_dependencies dependency_link
			JOIN tasks dependency ON dependency.id = dependency_link.dependency_id
			WHERE dependency_link.task_id = ?
			ORDER BY dependency.position ASC, dependency.id ASC`,
			task.id,
		)
		if err != nil {
			return nil, fmt.Errorf("list plan task dependencies: %w", err)
		}
		for dependencyRows.Next() {
			var clientID string
			if err := dependencyRows.Scan(&clientID); err != nil {
				_ = dependencyRows.Close()
				return nil, fmt.Errorf("scan plan task dependency: %w", err)
			}
			task.input.Dependencies = append(task.input.Dependencies, clientID)
		}
		if err := dependencyRows.Err(); err != nil {
			_ = dependencyRows.Close()
			return nil, fmt.Errorf("iterate plan task dependencies: %w", err)
		}
		if err := dependencyRows.Close(); err != nil {
			return nil, fmt.Errorf("close plan task dependencies: %w", err)
		}
		tasks = append(tasks, task.input)
	}
	return tasks, nil
}

func normalizePlanInput(input PlanInput) (PlanInput, error) {
	if len(input.Tasks) == 0 {
		return PlanInput{}, fmt.Errorf("plan must contain at least one task")
	}
	normalized := PlanInput{Tasks: make([]TaskInput, len(input.Tasks))}
	for index, task := range input.Tasks {
		if task.Position < 0 {
			return PlanInput{}, fmt.Errorf("task %q position cannot be negative", task.ClientID)
		}
		normalized.Tasks[index] = TaskInput{
			ClientID: strings.TrimSpace(task.ClientID),
			Position: task.Position,
			Title:    strings.TrimSpace(task.Title),
			Contract: strings.TrimSpace(task.Contract),
		}
		normalized.Tasks[index].Dependencies = make([]string, len(task.Dependencies))
		seenDependencies := make(map[string]struct{}, len(task.Dependencies))
		for dependencyIndex, dependency := range task.Dependencies {
			dependency = strings.TrimSpace(dependency)
			if _, exists := seenDependencies[dependency]; exists {
				return PlanInput{}, fmt.Errorf(
					"task %q has duplicate dependency %q",
					normalized.Tasks[index].ClientID,
					dependency,
				)
			}
			seenDependencies[dependency] = struct{}{}
			normalized.Tasks[index].Dependencies[dependencyIndex] = dependency
		}
	}
	if err := ValidateTaskInputs(normalized.Tasks); err != nil {
		return PlanInput{}, fmt.Errorf("invalid plan graph: %w", err)
	}
	return normalized, nil
}

func cloneTaskInputs(tasks []TaskInput) []TaskInput {
	cloned := make([]TaskInput, len(tasks))
	for index, task := range tasks {
		cloned[index] = task
		cloned[index].Dependencies = append([]string(nil), task.Dependencies...)
	}
	return cloned
}

func insertPlanEvent(
	ctx context.Context,
	tx *sql.Tx,
	missionID string,
	eventType string,
	actor string,
	version int,
	now time.Time,
) error {
	payload, err := json.Marshal(map[string]any{
		"actor":   actor,
		"version": version,
	})
	if err != nil {
		return fmt.Errorf("marshal %s event: %w", eventType, err)
	}
	return insertEvent(ctx, tx, Event{
		ID:        newID(),
		MissionID: missionID,
		Type:      eventType,
		Payload:   payload,
		CreatedAt: now,
	})
}

func wrapMissionNotFound(missionID string, err error) error {
	if err == ErrNotFound {
		return fmt.Errorf("mission %s: %w", missionID, ErrNotFound)
	}
	return err
}

type changeProposal struct {
	BaseVersion int       `json:"base_version"`
	Plan        PlanInput `json:"plan"`
}
