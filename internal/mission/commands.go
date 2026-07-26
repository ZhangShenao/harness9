package mission

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

const (
	approvePlanCommandName       = "approve_plan"
	submitPlanChangeCommandName  = "submit_plan_change"
	resolvePlanChangeCommandName = "resolve_plan_change"
	pauseMissionCommandName      = "pause_mission"
	cancelMissionCommandName     = "cancel_mission"
)

// CommandService applies operator commands atomically and idempotently.
type CommandService struct {
	store *Store
}

// NewCommandService creates an operator command service backed by store.
func NewCommandService(store *Store) *CommandService {
	return &CommandService{store: store}
}

// ApprovePlanCommand approves one draft Plan version.
type ApprovePlanCommand struct {
	MissionID      string
	Version        int
	Actor          string
	Reason         string
	IdempotencyKey string
}

// SubmitPlanChangeCommand proposes a replacement for the active Plan.
type SubmitPlanChangeCommand struct {
	MissionID      string
	BaseVersion    int
	ProposedPlan   PlanInput
	Actor          string
	Reason         string
	IdempotencyKey string
}

// ResolvePlanChangeCommand accepts or rejects one pending Plan Change Request.
type ResolvePlanChangeCommand struct {
	MissionID       string
	ChangeRequestID string
	Plan            PlanInput
	Approve         bool
	Actor           string
	Reason          string
	IdempotencyKey  string
}

// PauseMissionCommand moves active execution into operator attention.
type PauseMissionCommand struct {
	MissionID      string
	Actor          string
	Reason         string
	IdempotencyKey string
}

// CancelMissionCommand permanently cancels one cancellable Mission.
type CancelMissionCommand struct {
	MissionID      string
	Actor          string
	Reason         string
	IdempotencyKey string
}

// ApprovePlan freezes a draft Plan and makes its root Tasks schedulable.
func (c *CommandService) ApprovePlan(
	ctx context.Context,
	cmd ApprovePlanCommand,
) (*PlanVersion, error) {
	raw, err := c.withIdempotency(
		ctx,
		cmd.MissionID,
		approvePlanCommandName,
		cmd.IdempotencyKey,
		cmd.Actor,
		cmd.Reason,
		func(tx *sql.Tx) (json.RawMessage, error) {
			if cmd.Version <= 0 {
				return nil, fmt.Errorf("plan version must be positive")
			}
			plan, err := c.store.approvePlanTx(
				tx,
				strings.TrimSpace(cmd.MissionID),
				cmd.Version,
				cmd.Actor,
				cmd.Reason,
			)
			if err != nil {
				return nil, err
			}
			return marshalCommandResult(plan)
		},
	)
	if err != nil {
		return nil, err
	}
	var plan PlanVersion
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, fmt.Errorf("decode approve plan result: %w", err)
	}
	return &plan, nil
}

// SubmitPlanChange records a pending proposal without changing the active graph.
func (c *CommandService) SubmitPlanChange(
	ctx context.Context,
	cmd SubmitPlanChangeCommand,
) (*PlanChangeRequest, error) {
	raw, err := c.withIdempotency(
		ctx,
		cmd.MissionID,
		submitPlanChangeCommandName,
		cmd.IdempotencyKey,
		cmd.Actor,
		cmd.Reason,
		func(tx *sql.Tx) (json.RawMessage, error) {
			return c.submitPlanChangeTx(ctx, tx, cmd)
		},
	)
	if err != nil {
		return nil, err
	}
	var request PlanChangeRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decode submit plan change result: %w", err)
	}
	return &request, nil
}

// ResolvePlanChange records a decision and, when approved, activates a new Plan version.
func (c *CommandService) ResolvePlanChange(
	ctx context.Context,
	cmd ResolvePlanChangeCommand,
) (*PlanVersion, error) {
	raw, err := c.withIdempotency(
		ctx,
		cmd.MissionID,
		resolvePlanChangeCommandName,
		cmd.IdempotencyKey,
		cmd.Actor,
		cmd.Reason,
		func(tx *sql.Tx) (json.RawMessage, error) {
			return c.resolvePlanChangeTx(ctx, tx, cmd)
		},
	)
	if err != nil {
		return nil, err
	}
	var plan PlanVersion
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, fmt.Errorf("decode resolve plan change result: %w", err)
	}
	return &plan, nil
}

// PauseMission stops new scheduling and requests operator attention.
func (c *CommandService) PauseMission(
	ctx context.Context,
	cmd PauseMissionCommand,
) (*Mission, error) {
	return c.transitionMission(
		ctx,
		cmd.MissionID,
		pauseMissionCommandName,
		cmd.IdempotencyKey,
		cmd.Actor,
		cmd.Reason,
		MissionNeedsAttention,
		"mission.paused",
	)
}

// CancelMission permanently stops a cancellable Mission.
func (c *CommandService) CancelMission(
	ctx context.Context,
	cmd CancelMissionCommand,
) (*Mission, error) {
	return c.transitionMission(
		ctx,
		cmd.MissionID,
		cancelMissionCommandName,
		cmd.IdempotencyKey,
		cmd.Actor,
		cmd.Reason,
		MissionCancelled,
		"mission.cancelled",
	)
}

func (c *CommandService) withIdempotency(
	ctx context.Context,
	missionID string,
	name string,
	key string,
	actor string,
	reason string,
	apply func(*sql.Tx) (json.RawMessage, error),
) (json.RawMessage, error) {
	if c == nil || c.store == nil {
		return nil, fmt.Errorf("command store is required")
	}
	missionID = strings.TrimSpace(missionID)
	name = strings.TrimSpace(name)
	key = strings.TrimSpace(key)
	if missionID == "" {
		return nil, fmt.Errorf("command mission ID is required")
	}
	if name == "" {
		return nil, fmt.Errorf("command name is required")
	}
	if key == "" {
		return nil, fmt.Errorf("command idempotency key is required")
	}
	storageKey := name + "\x1f" + key

	for attempt := 0; attempt < 5; attempt++ {
		result, err := c.withIdempotencyAttempt(
			ctx,
			missionID,
			name,
			storageKey,
			actor,
			reason,
			apply,
		)
		if err == nil {
			return result, nil
		}
		if !isSQLiteContention(err) {
			return nil, err
		}
		stored, found, readErr := c.readStoredCommandResult(
			ctx,
			missionID,
			name,
			storageKey,
		)
		if readErr == nil && found {
			return stored, nil
		}
		if readErr != nil && !isSQLiteContention(readErr) {
			return nil, readErr
		}
		if attempt == 4 {
			return nil, fmt.Errorf("%s command contention: %w", name, err)
		}
		if err := waitForCommandRetry(ctx, attempt); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%s command retry budget exhausted", name)
}

func (c *CommandService) withIdempotencyAttempt(
	ctx context.Context,
	missionID string,
	name string,
	storageKey string,
	actor string,
	reason string,
	apply func(*sql.Tx) (json.RawMessage, error),
) (json.RawMessage, error) {
	tx, err := c.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin %s command: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()

	var stored json.RawMessage
	err = tx.QueryRowContext(ctx, `
		SELECT payload
		FROM mission_commands
		WHERE mission_id = ? AND type = ? AND idempotency_key = ?`,
		missionID,
		name,
		storageKey,
	).Scan(&stored)
	switch {
	case err == nil:
		return append(json.RawMessage(nil), stored...), nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("read %s command result: %w", name, err)
	}

	result, err := apply(tx)
	if err != nil {
		return nil, err
	}
	now := c.store.currentTime()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mission_commands (
			id, mission_id, idempotency_key, type, actor, reason, payload, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		newID(),
		missionID,
		storageKey,
		name,
		strings.TrimSpace(actor),
		strings.TrimSpace(reason),
		[]byte(result),
		unixMillis(now),
	); err != nil {
		return nil, fmt.Errorf("store %s command result: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit %s command: %w", name, err)
	}
	return append(json.RawMessage(nil), result...), nil
}

func (c *CommandService) readStoredCommandResult(
	ctx context.Context,
	missionID string,
	name string,
	storageKey string,
) (json.RawMessage, bool, error) {
	var stored json.RawMessage
	err := c.store.db.QueryRowContext(ctx, `
		SELECT payload
		FROM mission_commands
		WHERE mission_id = ? AND type = ? AND idempotency_key = ?`,
		missionID,
		name,
		storageKey,
	).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read committed %s command result: %w", name, err)
	}
	return append(json.RawMessage(nil), stored...), true, nil
}

func isSQLiteContention(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") ||
		strings.Contains(message, "database is busy") ||
		strings.Contains(message, "sqlite_busy") ||
		strings.Contains(message, "unique constraint failed: mission_commands")
}

func waitForCommandRetry(ctx context.Context, attempt int) error {
	delay := time.Duration(attempt+1) * 5 * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *CommandService) submitPlanChangeTx(
	ctx context.Context,
	tx *sql.Tx,
	cmd SubmitPlanChangeCommand,
) (json.RawMessage, error) {
	request, err := c.store.createPlanChangeRequestTx(
		ctx,
		tx,
		cmd.MissionID,
		cmd.BaseVersion,
		cmd.ProposedPlan,
		cmd.Reason,
		cmd.Actor,
	)
	if err != nil {
		return nil, err
	}
	return marshalCommandResult(request)
}

func (c *CommandService) resolvePlanChangeTx(
	ctx context.Context,
	tx *sql.Tx,
	cmd ResolvePlanChangeCommand,
) (json.RawMessage, error) {
	missionID := strings.TrimSpace(cmd.MissionID)
	requestID := strings.TrimSpace(cmd.ChangeRequestID)
	actor := strings.TrimSpace(cmd.Actor)
	reason := strings.TrimSpace(cmd.Reason)
	if requestID == "" {
		return nil, fmt.Errorf("plan change request ID is required")
	}
	if actor == "" {
		return nil, fmt.Errorf("plan change resolution actor is required")
	}
	if reason == "" {
		return nil, fmt.Errorf("plan change resolution reason is required")
	}

	request, err := getPlanChangeRequestFromQuerier(ctx, tx, requestID)
	if err != nil {
		return nil, err
	}
	if request.MissionID != missionID {
		return nil, fmt.Errorf(
			"%w: plan change request %s belongs to mission %s",
			ErrConflict,
			requestID,
			request.MissionID,
		)
	}
	if request.Status != ChangeRequestPending {
		return nil, fmt.Errorf(
			"%w: plan change request %s is %s",
			ErrConflict,
			requestID,
			request.Status,
		)
	}
	input, err := normalizePlanInput(cmd.Plan)
	if err != nil {
		return nil, err
	}
	requestedInput, err := normalizePlanInput(PlanInput{
		Tasks: request.ProposedPlan.Tasks,
	})
	if err != nil {
		return nil, fmt.Errorf("normalize stored plan change request: %w", err)
	}
	if !reflect.DeepEqual(input.Tasks, requestedInput.Tasks) {
		return nil, fmt.Errorf(
			"%w: supplied plan does not match plan change request %s",
			ErrConflict,
			requestID,
		)
	}
	mission, err := scanMission(tx.QueryRowContext(ctx, `
		SELECT id, goal, acceptance_contract, budget_cents, policy_json,
		       current_plan_version, status, created_at, updated_at
		FROM missions WHERE id = ?`,
		missionID,
	))
	if err != nil {
		return nil, wrapMissionNotFound(missionID, err)
	}
	if mission.Status != MissionRunning && mission.Status != MissionVerifying {
		return nil, fmt.Errorf(
			"%w: mission %s cannot resolve a plan change from %s",
			ErrInvalidTransition,
			missionID,
			mission.Status,
		)
	}
	if mission.CurrentPlanVersion != request.BaseVersion {
		return nil, fmt.Errorf(
			"%w: plan change base version %d does not match current version %d",
			ErrConflict,
			request.BaseVersion,
			mission.CurrentPlanVersion,
		)
	}
	now := c.store.currentTime()
	if !cmd.Approve {
		if _, err := tx.ExecContext(ctx, `
			UPDATE mission_change_requests
			SET status = ?, resolution_reason = ?, updated_at = ?, resolved_at = ?
			WHERE id = ? AND status = ?`,
			ChangeRequestRejected,
			reason,
			unixMillis(now),
			unixMillis(now),
			requestID,
			ChangeRequestPending,
		); err != nil {
			return nil, fmt.Errorf("reject plan change request: %w", err)
		}
		if err := insertCommandEvent(
			ctx,
			tx,
			missionID,
			"plan_change.rejected",
			actor,
			reason,
			map[string]any{
				"base_version": request.BaseVersion,
				"request_id":   requestID,
			},
			now,
		); err != nil {
			return nil, err
		}
		base, err := getPlanFromQuerier(
			ctx,
			tx,
			missionID,
			request.BaseVersion,
		)
		if err != nil {
			return nil, err
		}
		return marshalCommandResult(base)
	}

	tasksJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal approved plan graph: %w", err)
	}
	version := request.BaseVersion + 1
	planID := newID()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mission_plan_versions (
			id, mission_id, version, status, tasks_json,
			created_at, updated_at, approved_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		planID,
		missionID,
		version,
		PlanApproved,
		string(tasksJSON),
		unixMillis(now),
		unixMillis(now),
		unixMillis(now),
	); err != nil {
		return nil, fmt.Errorf("insert approved plan version: %w", err)
	}
	if err := insertPlanGraphTx(ctx, tx, missionID, version, input, now); err != nil {
		return nil, err
	}
	if err := queuePlanRootsTx(ctx, tx, missionID, version, now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE mission_change_requests
		SET status = ?, resolution_reason = ?, updated_at = ?, resolved_at = ?
		WHERE id = ? AND status = ?`,
		ChangeRequestApproved,
		reason,
		unixMillis(now),
		unixMillis(now),
		requestID,
		ChangeRequestPending,
	); err != nil {
		return nil, fmt.Errorf("approve plan change request: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE missions
		SET current_plan_version = ?, updated_at = ?
		WHERE id = ? AND current_plan_version = ?`,
		version,
		unixMillis(now),
		missionID,
		request.BaseVersion,
	); err != nil {
		return nil, fmt.Errorf("activate approved plan change: %w", err)
	}
	if err := insertCommandEvent(
		ctx,
		tx,
		missionID,
		"plan_change.approved",
		actor,
		reason,
		map[string]any{
			"base_version": request.BaseVersion,
			"request_id":   requestID,
			"version":      version,
		},
		now,
	); err != nil {
		return nil, err
	}
	if err := insertCommandEvent(
		ctx,
		tx,
		missionID,
		"plan.version_activated",
		actor,
		reason,
		map[string]any{
			"from_version": request.BaseVersion,
			"request_id":   requestID,
			"to_version":   version,
		},
		now,
	); err != nil {
		return nil, err
	}
	plan, err := getPlanFromQuerier(ctx, tx, missionID, version)
	if err != nil {
		return nil, err
	}
	return marshalCommandResult(plan)
}

func (c *CommandService) transitionMission(
	ctx context.Context,
	missionID string,
	commandName string,
	idempotencyKey string,
	actor string,
	reason string,
	next MissionStatus,
	eventType string,
) (*Mission, error) {
	raw, err := c.withIdempotency(
		ctx,
		missionID,
		commandName,
		idempotencyKey,
		actor,
		reason,
		func(tx *sql.Tx) (json.RawMessage, error) {
			actor = strings.TrimSpace(actor)
			reason = strings.TrimSpace(reason)
			if actor == "" {
				return nil, fmt.Errorf("mission command actor is required")
			}
			if reason == "" {
				return nil, fmt.Errorf("mission command reason is required")
			}
			mission, err := scanMission(tx.QueryRowContext(ctx, `
				SELECT id, goal, acceptance_contract, budget_cents, policy_json,
				       current_plan_version, status, created_at, updated_at
				FROM missions WHERE id = ?`,
				strings.TrimSpace(missionID),
			))
			if err != nil {
				return nil, wrapMissionNotFound(missionID, err)
			}
			if !mission.Status.CanTransitionTo(next) {
				return nil, fmt.Errorf(
					"%w: mission %s cannot move from %s to %s",
					ErrInvalidTransition,
					mission.ID,
					mission.Status,
					next,
				)
			}
			now := c.store.currentTime()
			if _, err := tx.ExecContext(ctx, `
				UPDATE missions
				SET status = ?, updated_at = ?
				WHERE id = ? AND status = ?`,
				next,
				unixMillis(now),
				mission.ID,
				mission.Status,
			); err != nil {
				return nil, fmt.Errorf("update mission command status: %w", err)
			}
			if err := insertCommandEvent(
				ctx,
				tx,
				mission.ID,
				eventType,
				actor,
				reason,
				map[string]any{
					"from": mission.Status,
					"to":   next,
				},
				now,
			); err != nil {
				return nil, err
			}
			mission.Status = next
			mission.UpdatedAt = now
			return marshalCommandResult(&mission)
		},
	)
	if err != nil {
		return nil, err
	}
	var mission Mission
	if err := json.Unmarshal(raw, &mission); err != nil {
		return nil, fmt.Errorf("decode %s result: %w", commandName, err)
	}
	return &mission, nil
}

func queuePlanRootsTx(
	ctx context.Context,
	tx *sql.Tx,
	missionID string,
	version int,
	now time.Time,
) error {
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
		return fmt.Errorf("queue approved root tasks: %w", err)
	}
	return nil
}

func insertCommandEvent(
	ctx context.Context,
	tx *sql.Tx,
	missionID string,
	eventType string,
	actor string,
	reason string,
	fields map[string]any,
	now time.Time,
) error {
	payload := make(map[string]any, len(fields)+2)
	payload["actor"] = actor
	payload["reason"] = reason
	for key, value := range fields {
		payload[key] = value
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s event: %w", eventType, err)
	}
	return insertEvent(ctx, tx, Event{
		ID:        newID(),
		MissionID: missionID,
		Type:      eventType,
		Payload:   raw,
		CreatedAt: now,
	})
}

func marshalCommandResult(value any) (json.RawMessage, error) {
	result, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal command result: %w", err)
	}
	return result, nil
}
