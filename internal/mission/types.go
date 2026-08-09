// Package mission provides the durable Mission Control domain used to coordinate
// long-running Agent work without relying on an individual Agent's conversation.
package mission

import (
	"errors"
	"time"
)

var (
	// ErrNotFound indicates that a requested durable Mission object does not exist.
	ErrNotFound = errors.New("mission object not found")
	// ErrInvalidTransition indicates that a requested lifecycle transition is unsafe.
	ErrInvalidTransition = errors.New("invalid mission state transition")
)

// MissionStatus describes the lifecycle of a Mission.
type MissionStatus string

const (
	// MissionDraft accepts an initial task graph but cannot dispatch work yet.
	MissionDraft MissionStatus = "draft"
	// MissionPlanning has a proposed task graph awaiting approval.
	MissionPlanning MissionStatus = "planning"
	// MissionReady is eligible for scheduling.
	MissionReady MissionStatus = "ready"
	// MissionRunning has at least one active Task.
	MissionRunning MissionStatus = "running"
	// MissionVerifying waits for final release verification.
	MissionVerifying MissionStatus = "verifying"
	// MissionSucceeded has independently verified all required work.
	MissionSucceeded MissionStatus = "succeeded"
	// MissionFailed has exhausted an execution path.
	MissionFailed MissionStatus = "failed"
	// MissionNeedsAttention requires an operator decision.
	MissionNeedsAttention MissionStatus = "needs_attention"
	// MissionCancelled was explicitly stopped by an operator.
	MissionCancelled MissionStatus = "cancelled"
)

// TaskStatus describes the lifecycle of one schedulable work unit.
type TaskStatus string

const (
	// TaskBlocked is waiting for one or more dependency Tasks.
	TaskBlocked TaskStatus = "blocked"
	// TaskQueued is ready for a compatible Worker.
	TaskQueued TaskStatus = "queued"
	// TaskLeased owns a workspace but has not started the Worker yet.
	TaskLeased TaskStatus = "leased"
	// TaskRunning is currently executing on a Worker.
	TaskRunning TaskStatus = "running"
	// TaskVerifying has Worker output but needs independent acceptance checks.
	TaskVerifying TaskStatus = "verifying"
	// TaskSucceeded passed independent verification.
	TaskSucceeded TaskStatus = "succeeded"
	// TaskFailed did not satisfy its declared acceptance checks.
	TaskFailed TaskStatus = "failed"
	// TaskAwaitingInput needs an explicit human response.
	TaskAwaitingInput TaskStatus = "awaiting_input"
	// TaskIndeterminate was interrupted while side effects might have happened.
	TaskIndeterminate TaskStatus = "indeterminate"
)

// ContractKind describes how a Task is executed by the scheduler.
type ContractKind string

const (
	// ContractImplementation drives a Worker to produce new artifacts.
	ContractImplementation ContractKind = "implementation"
	// ContractVerification drives a Worker to independently verify existing artifacts.
	ContractVerification ContractKind = "verification"
	// ContractIntegration drives a Worker to combine artifacts across Tasks.
	ContractIntegration ContractKind = "integration"
)

// Budget constrains a single Task Attempt's resource usage.
type Budget struct {
	MaxTokens  int `json:"max_tokens"`
	MaxTurns   int `json:"max_turns"`
	MaxSeconds int `json:"max_seconds"`
}

// TaskInput is the Contract that drives a Worker's behavior for one Task.
type TaskInput struct {
	Kind         ContractKind `json:"kind"`
	Goal         string       `json:"goal"`
	DependsOn    []string     `json:"depends_on,omitempty"`
	Acceptance   []string     `json:"acceptance,omitempty"`
	AllowedTools []string     `json:"allowed_tools,omitempty"`
	Budget       Budget       `json:"budget"`
	MaxRetries   int          `json:"max_retries"`
	SettingsPath string       `json:"settings_path,omitempty"`
}

// Mission is the durable unit of long-running user intent.
type Mission struct {
	ID                 string
	Goal               string
	Status             MissionStatus
	PolicyJSON         string
	AcceptanceContract string
	CurrentPlanVersion string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Task is a dependency-aware unit of work within one Mission.
type Task struct {
	ID            string
	MissionID     string
	Title         string
	Status        TaskStatus
	DependsOn     []string
	PlanVersionID string
	ContractKind  ContractKind
	Input         TaskInput
	MaxRetries    int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// TaskAttempt records one Worker execution of a Task.
type TaskAttempt struct {
	ID         string
	TaskID     string
	Worker     string
	Status     string
	LeaseID    string
	ExitReason string
	StartedAt  *time.Time
	FinishedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Artifact is append-only Worker output associated with one execution attempt.
type Artifact struct {
	ID        string
	MissionID string
	TaskID    string
	AttemptID string
	Kind      string
	Content   []byte
	SHA256    string
	CreatedAt time.Time
}

// Evidence is append-only verification output. Only a verifier should create it.
type Evidence struct {
	ID        string
	MissionID string
	TaskID    string
	AttemptID string
	Kind      string
	Content   []byte
	SHA256    string
	Passed    bool
	CreatedAt time.Time
}

// CreateMissionInput describes the minimum user intent needed to create a Mission.
type CreateMissionInput struct {
	Goal string
}

// CreateTaskInput describes a unit of work and its prerequisite Task IDs.
type CreateTaskInput struct {
	MissionID string
	Title     string
	DependsOn []string
}

// CreateArtifactInput describes an immutable Worker artifact.
type CreateArtifactInput struct {
	MissionID string
	TaskID    string
	AttemptID string
	Kind      string
	Content   []byte
}

// CreateEvidenceInput describes an immutable verifier result.
type CreateEvidenceInput struct {
	MissionID string
	TaskID    string
	AttemptID string
	Kind      string
	Content   []byte
	Passed    bool
}

func validTaskTransition(current, next TaskStatus) bool {
	switch current {
	case TaskBlocked:
		return next == TaskQueued
	case TaskQueued:
		return next == TaskLeased || next == TaskFailed || next == TaskAwaitingInput
	case TaskLeased:
		return next == TaskRunning || next == TaskQueued || next == TaskIndeterminate || next == TaskFailed
	case TaskRunning:
		return next == TaskVerifying || next == TaskFailed || next == TaskAwaitingInput || next == TaskIndeterminate
	case TaskVerifying:
		return next == TaskSucceeded || next == TaskFailed || next == TaskAwaitingInput
	default:
		return false
	}
}

// validMissionTransition reports whether a Mission may move from current to next.
// Terminal states (succeeded, failed, cancelled) cannot resume, while
// needs_attention is the recovery hub for operator-driven re-entry.
// Draft -> Planning is a one-way commitment: once planning begins, the draft
// phase is closed and the Mission may only advance to ready or be cancelled.
func validMissionTransition(current, next MissionStatus) bool {
	switch current {
	case MissionDraft:
		return next == MissionPlanning
	case MissionPlanning:
		return next == MissionReady || next == MissionCancelled
	case MissionReady:
		return next == MissionRunning || next == MissionCancelled
	case MissionRunning:
		return next == MissionVerifying || next == MissionNeedsAttention ||
			next == MissionFailed || next == MissionCancelled
	case MissionVerifying:
		return next == MissionSucceeded || next == MissionNeedsAttention ||
			next == MissionFailed || next == MissionCancelled
	case MissionNeedsAttention:
		return next == MissionRunning || next == MissionVerifying ||
			next == MissionFailed || next == MissionCancelled
	default:
		return false
	}
}

// PlanStatus describes the lifecycle of a Plan draft.
type PlanStatus string

const (
	PlanDraft      PlanStatus = "draft"
	PlanApproved   PlanStatus = "approved"
	PlanSuperseded PlanStatus = "superseded"
)

// Plan is an editable task graph draft that becomes an immutable PlanVersion on approval.
type Plan struct {
	ID        string
	MissionID string
	Version   int
	Status    PlanStatus
	TasksJSON string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PlanVersion is an immutable snapshot of an approved Plan -- the only schedulable version.
type PlanVersion struct {
	ID        string
	MissionID string
	PlanID    string
	Version   int
	TasksJSON string
	CreatedAt time.Time
}

// ChangeRequestStatus describes a PlanChangeRequest lifecycle.
type ChangeRequestStatus string

const (
	ChangePending  ChangeRequestStatus = "pending"
	ChangeApproved ChangeRequestStatus = "approved"
	ChangeRejected ChangeRequestStatus = "rejected"
)

// PlanChangeRequest is an execution-time proposal to modify the approved Plan.
type PlanChangeRequest struct {
	ID               string
	MissionID        string
	Reason           string
	TriggerAttemptID string
	AffectedTasks    []string
	AddedTasks       []TaskInput
	ProposedPlanJSON string
	Status           ChangeRequestStatus
	ReviewedBy       string
	ReviewedAt       *time.Time
	ReviewReason     string
	CreatedAt        time.Time
}

// Policy defines per-Mission concurrency, tool scope, budget and retry rules.
type Policy struct {
	MissionConcurrency int      `json:"mission_concurrency"`
	GlobalConcurrency  int      `json:"global_concurrency"`
	MaxRetries         int      `json:"max_retries"`
	AllowedTools       []string `json:"allowed_tools,omitempty"`
	AutoApproveRetries bool     `json:"auto_approve_retries"`
}

// DefaultPolicy returns a conservative default Policy for new Missions.
func DefaultPolicy() Policy {
	return Policy{
		MissionConcurrency: 1,
		GlobalConcurrency:  2,
		MaxRetries:         2,
	}
}

// LeaseStatus describes a WorkspaceLease lifecycle.
type LeaseStatus string

const (
	LeaseActive   LeaseStatus = "active"
	LeaseReleased LeaseStatus = "released"
	LeaseExpired  LeaseStatus = "expired"
)

// WorkspaceLease grants a Task exclusive access to a worktree, branch and sandbox.
type WorkspaceLease struct {
	ID         string
	TaskID     string
	Path       string
	Branch     string
	SandboxID  string
	Status     LeaseStatus
	ExpiresAt  time.Time
	CreatedAt  time.Time
	ReleasedAt *time.Time
}

// AuditEvent is an immutable record of one Command execution.
type AuditEvent struct {
	ID             string
	MissionID      string
	CommandKind    string
	Actor          string
	Target         string
	Reason         string
	IdempotencyKey string
	Result         string
	BeforeState    string
	AfterState     string
	CreatedAt      time.Time
}
