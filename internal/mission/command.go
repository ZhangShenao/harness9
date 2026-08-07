package mission

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// CommandKind enumerates all state-mutating operations.
type CommandKind string

const (
	CmdSubmitPlanDraft   CommandKind = "submit_plan_draft"
	CmdApprovePlan       CommandKind = "approve_plan"
	CmdRejectPlan        CommandKind = "reject_plan"
	CmdRequestPlanChange CommandKind = "request_plan_change"
	CmdApproveChange     CommandKind = "approve_change_request"
	CmdRejectChange      CommandKind = "reject_change_request"
	CmdPauseMission      CommandKind = "pause_mission"
	CmdResumeMission     CommandKind = "resume_mission"
	CmdCancelMission     CommandKind = "cancel_mission"
	CmdRetryTask         CommandKind = "retry_task"
	CmdEscalateToMission CommandKind = "escalate_to_mission"
	CmdExemptTask        CommandKind = "exempt_task"
)

// Command is a state mutation request submitted to the CommandService.
type Command struct {
	Kind           CommandKind
	Actor          string
	Reason         string
	IdempotencyKey string
	Target         string
	Payload        json.RawMessage
}

// CommandResult is the outcome of executing a Command.
type CommandResult struct {
	Applied bool
	Event   AuditEvent
	Error   error
}

// CommandService is the sole mutation entry point for Mission Control.
type CommandService struct {
	store *Store
}

// NewCommandService creates a CommandService backed by the given Store.
func NewCommandService(store *Store) *CommandService {
	return &CommandService{store: store}
}

// Execute validates and applies a Command with idempotency.
// If a command with the same IdempotencyKey was already applied, it returns
// the original result without re-executing.
func (cs *CommandService) Execute(ctx context.Context, cmd Command) CommandResult {
	if cmd.IdempotencyKey != "" {
		if existing, found, err := cs.store.FindAuditEventByIdempotencyKey(ctx, cmd.Target, cmd.IdempotencyKey); err != nil {
			return CommandResult{Error: fmt.Errorf("idempotency check: %w", err)}
		} else if found {
			return CommandResult{Applied: false, Event: existing}
		}
	}
	result, err := cs.dispatch(ctx, cmd)
	if err != nil {
		event, _ := cs.store.AddAuditEvent(ctx, AuditEvent{
			MissionID: cmd.Target, CommandKind: string(cmd.Kind), Actor: cmd.Actor,
			Target: cmd.Target, Reason: cmd.Reason, IdempotencyKey: cmd.IdempotencyKey,
			Result: "rejected",
		})
		return CommandResult{Applied: false, Event: event, Error: err}
	}
	event, _ := cs.store.AddAuditEvent(ctx, AuditEvent{
		MissionID: result.MissionID, CommandKind: string(cmd.Kind), Actor: cmd.Actor,
		Target: result.TargetID, Reason: cmd.Reason, IdempotencyKey: cmd.IdempotencyKey,
		Result: "applied", BeforeState: result.BeforeState, AfterState: result.AfterState,
	})
	return CommandResult{Applied: true, Event: event}
}

// commandOutcome describes the result of a successfully applied command.
type commandOutcome struct {
	MissionID   string
	TargetID    string
	BeforeState string
	AfterState  string
}

func (cs *CommandService) dispatch(ctx context.Context, cmd Command) (commandOutcome, error) {
	switch cmd.Kind {
	case CmdSubmitPlanDraft:
		return cs.handleSubmitPlanDraft(ctx, cmd)
	case CmdApprovePlan:
		return cs.handleApprovePlan(ctx, cmd)
	case CmdRejectPlan:
		return cs.handleRejectPlan(ctx, cmd)
	case CmdRequestPlanChange:
		return cs.handleRequestPlanChange(ctx, cmd)
	case CmdApproveChange:
		return cs.handleApproveChange(ctx, cmd)
	case CmdRejectChange:
		return cs.handleRejectChange(ctx, cmd)
	case CmdPauseMission:
		return cs.handlePauseMission(ctx, cmd)
	case CmdResumeMission:
		return cs.handleResumeMission(ctx, cmd)
	case CmdCancelMission:
		return cs.handleCancelMission(ctx, cmd)
	default:
		return commandOutcome{}, fmt.Errorf("unsupported command kind %q", cmd.Kind)
	}
}

func (cs *CommandService) handlePauseMission(ctx context.Context, cmd Command) (commandOutcome, error) {
	mission, err := cs.getMission(ctx, cmd.Target)
	if err != nil {
		return commandOutcome{}, err
	}
	before := string(mission.Status)
	if !validMissionTransition(mission.Status, MissionNeedsAttention) {
		return commandOutcome{}, fmt.Errorf("%w: mission %s cannot pause from %s", ErrInvalidTransition, cmd.Target, mission.Status)
	}
	if _, err := cs.store.db.ExecContext(ctx,
		`UPDATE missions SET status = ?, updated_at = ? WHERE id = ?`,
		MissionNeedsAttention, unixMillis(time.Now().UTC()), cmd.Target); err != nil {
		return commandOutcome{}, fmt.Errorf("pause mission: %w", err)
	}
	return commandOutcome{MissionID: cmd.Target, TargetID: cmd.Target,
		BeforeState: before, AfterState: string(MissionNeedsAttention)}, nil
}

func (cs *CommandService) handleResumeMission(ctx context.Context, cmd Command) (commandOutcome, error) {
	mission, err := cs.getMission(ctx, cmd.Target)
	if err != nil {
		return commandOutcome{}, err
	}
	before := string(mission.Status)
	if !validMissionTransition(mission.Status, MissionRunning) {
		return commandOutcome{}, fmt.Errorf("%w: mission %s cannot resume from %s", ErrInvalidTransition, cmd.Target, mission.Status)
	}
	if _, err := cs.store.db.ExecContext(ctx,
		`UPDATE missions SET status = ?, updated_at = ? WHERE id = ?`,
		MissionRunning, unixMillis(time.Now().UTC()), cmd.Target); err != nil {
		return commandOutcome{}, fmt.Errorf("resume mission: %w", err)
	}
	return commandOutcome{MissionID: cmd.Target, TargetID: cmd.Target,
		BeforeState: before, AfterState: string(MissionRunning)}, nil
}

func (cs *CommandService) handleSubmitPlanDraft(ctx context.Context, cmd Command) (commandOutcome, error) {
	plan, err := cs.store.CreatePlan(ctx, cmd.Target, string(cmd.Payload))
	if err != nil {
		return commandOutcome{}, err
	}
	return commandOutcome{MissionID: cmd.Target, TargetID: plan.ID, AfterState: string(PlanDraft)}, nil
}

func (cs *CommandService) handleApprovePlan(ctx context.Context, cmd Command) (commandOutcome, error) {
	plan, err := cs.store.GetPlan(ctx, cmd.Target)
	if err != nil {
		return commandOutcome{}, err
	}
	pv, err := cs.store.ApprovePlan(ctx, cmd.Target)
	if err != nil {
		return commandOutcome{}, err
	}
	return commandOutcome{MissionID: plan.MissionID, TargetID: pv.ID,
		BeforeState: string(PlanDraft), AfterState: string(PlanApproved)}, nil
}

func (cs *CommandService) handleRejectPlan(ctx context.Context, cmd Command) (commandOutcome, error) {
	plan, err := cs.store.GetPlan(ctx, cmd.Target)
	if err != nil {
		return commandOutcome{}, err
	}
	// rejecting a draft plan: mark it superseded (not schedulable)
	_, err = cs.store.db.ExecContext(ctx,
		`UPDATE plans SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		PlanSuperseded, unixMillis(time.Now().UTC()), cmd.Target, PlanDraft)
	if err != nil {
		return commandOutcome{}, fmt.Errorf("reject plan: %w", err)
	}
	return commandOutcome{MissionID: plan.MissionID, TargetID: cmd.Target,
		BeforeState: string(PlanDraft), AfterState: "rejected"}, nil
}

func (cs *CommandService) handleRequestPlanChange(ctx context.Context, cmd Command) (commandOutcome, error) {
	var req PlanChangeRequest
	if err := json.Unmarshal(cmd.Payload, &req); err != nil {
		return commandOutcome{}, fmt.Errorf("parse change request payload: %w", err)
	}
	req.MissionID = cmd.Target
	cr, err := cs.store.CreateChangeRequest(ctx, req)
	if err != nil {
		return commandOutcome{}, err
	}
	return commandOutcome{MissionID: cmd.Target, TargetID: cr.ID, AfterState: string(ChangePending)}, nil
}

func (cs *CommandService) handleApproveChange(ctx context.Context, cmd Command) (commandOutcome, error) {
	cr, err := cs.store.GetChangeRequest(ctx, cmd.Target)
	if err != nil {
		return commandOutcome{}, err
	}
	reviewed, err := cs.store.ReviewChangeRequest(ctx, cmd.Target, ChangeApproved, cmd.Actor, cmd.Reason)
	if err != nil {
		return commandOutcome{}, err
	}
	return commandOutcome{MissionID: cr.MissionID, TargetID: reviewed.ID,
		BeforeState: string(ChangePending), AfterState: string(ChangeApproved)}, nil
}

func (cs *CommandService) handleRejectChange(ctx context.Context, cmd Command) (commandOutcome, error) {
	cr, err := cs.store.GetChangeRequest(ctx, cmd.Target)
	if err != nil {
		return commandOutcome{}, err
	}
	reviewed, err := cs.store.ReviewChangeRequest(ctx, cmd.Target, ChangeRejected, cmd.Actor, cmd.Reason)
	if err != nil {
		return commandOutcome{}, err
	}
	return commandOutcome{MissionID: cr.MissionID, TargetID: reviewed.ID,
		BeforeState: string(ChangePending), AfterState: string(ChangeRejected)}, nil
}

func (cs *CommandService) handleCancelMission(ctx context.Context, cmd Command) (commandOutcome, error) {
	mission, err := cs.getMission(ctx, cmd.Target)
	if err != nil {
		return commandOutcome{}, err
	}
	before := string(mission.Status)
	if !validMissionTransition(mission.Status, MissionCancelled) {
		return commandOutcome{}, fmt.Errorf("%w: mission %s cannot cancel from %s", ErrInvalidTransition, cmd.Target, mission.Status)
	}
	_, err = cs.store.db.ExecContext(ctx,
		`UPDATE missions SET status = ?, updated_at = ? WHERE id = ?`,
		MissionCancelled, unixMillis(time.Now().UTC()), cmd.Target)
	if err != nil {
		return commandOutcome{}, fmt.Errorf("cancel mission: %w", err)
	}
	return commandOutcome{MissionID: cmd.Target, TargetID: cmd.Target,
		BeforeState: before, AfterState: string(MissionCancelled)}, nil
}

func (cs *CommandService) getMission(ctx context.Context, id string) (Mission, error) {
	var m Mission
	var createdAt, updatedAt int64
	err := cs.store.db.QueryRowContext(ctx,
		`SELECT id, goal, status, created_at, updated_at FROM missions WHERE id = ?`, id).
		Scan(&m.ID, &m.Goal, &m.Status, &createdAt, &updatedAt)
	if err != nil {
		return Mission{}, ErrNotFound
	}
	m.CreatedAt = fromUnixMillis(createdAt)
	m.UpdatedAt = fromUnixMillis(updatedAt)
	return m, nil
}
