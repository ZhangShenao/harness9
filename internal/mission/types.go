// Package mission provides the durable Mission Control domain used to coordinate
// long-running Agent work without relying on an individual Agent's conversation.
package mission

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrNotFound indicates that a requested durable Mission object does not exist.
	ErrNotFound = errors.New("mission object not found")
	// ErrInvalidTransition indicates that a requested lifecycle transition is unsafe.
	ErrInvalidTransition = errors.New("invalid mission state transition")
	// ErrConflict indicates that a write conflicts with the durable Mission state.
	ErrConflict = errors.New("mission state conflict")
)

// MissionStatus describes the lifecycle of a Mission.
type MissionStatus string

const (
	// MissionDraft accepts an initial task graph but cannot dispatch work yet.
	MissionDraft MissionStatus = "draft"
	// MissionPlanning has a proposed task graph awaiting approval.
	MissionPlanning MissionStatus = "planning"
	// MissionAwaitingPlanApproval has a proposed plan awaiting an operator decision.
	MissionAwaitingPlanApproval MissionStatus = "awaiting_plan_approval"
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

// CanTransitionTo reports whether next is a permitted Mission lifecycle state.
func (s MissionStatus) CanTransitionTo(next MissionStatus) bool {
	return map[MissionStatus]map[MissionStatus]bool{
		MissionDraft: {
			MissionPlanning:  true,
			MissionCancelled: true,
		},
		MissionPlanning: {
			MissionAwaitingPlanApproval: true,
			MissionCancelled:            true,
		},
		MissionAwaitingPlanApproval: {
			MissionReady:     true,
			MissionPlanning:  true,
			MissionCancelled: true,
		},
		MissionReady: {
			MissionRunning:   true,
			MissionCancelled: true,
		},
		MissionRunning: {
			MissionVerifying:      true,
			MissionNeedsAttention: true,
			MissionFailed:         true,
			MissionCancelled:      true,
		},
		MissionVerifying: {
			MissionSucceeded:      true,
			MissionFailed:         true,
			MissionNeedsAttention: true,
		},
	}[s][next]
}

// IsValid reports whether s is a known Mission lifecycle state.
func (s MissionStatus) IsValid() bool {
	switch s {
	case MissionDraft, MissionPlanning, MissionAwaitingPlanApproval, MissionReady,
		MissionRunning, MissionVerifying, MissionSucceeded, MissionFailed,
		MissionNeedsAttention, MissionCancelled:
		return true
	default:
		return false
	}
}

// Scan implements sql.Scanner and rejects unknown persisted Mission states.
func (s *MissionStatus) Scan(value any) error { return scanStatus(value, s.set) }

func (s *MissionStatus) set(value string) error {
	status := MissionStatus(value)
	if !status.IsValid() {
		return fmt.Errorf("unknown mission status %q", value)
	}
	*s = status
	return nil
}

// Value implements driver.Valuer for MissionStatus.
func (s MissionStatus) Value() (driver.Value, error) { return string(s), nil }

// PlanStatus describes the lifecycle of a versioned Mission plan.
type PlanStatus string

const (
	// PlanDraft is still editable by its proposer.
	PlanDraft PlanStatus = "draft"
	// PlanAwaitingApproval awaits an operator decision.
	PlanAwaitingApproval PlanStatus = "awaiting_approval"
	// PlanApproved is immutable and eligible for scheduling.
	PlanApproved PlanStatus = "approved"
	// PlanRejected was declined by an operator.
	PlanRejected PlanStatus = "rejected"
	// PlanSuperseded has been replaced by an approved later version.
	PlanSuperseded PlanStatus = "superseded"
)

// CanTransitionTo reports whether next is a permitted Plan lifecycle state.
func (s PlanStatus) CanTransitionTo(next PlanStatus) bool {
	return map[PlanStatus]map[PlanStatus]bool{
		PlanDraft:            {PlanAwaitingApproval: true},
		PlanAwaitingApproval: {PlanApproved: true, PlanDraft: true, PlanRejected: true},
		PlanApproved:         {PlanSuperseded: true},
	}[s][next]
}

// IsValid reports whether s is a known Plan lifecycle state.
func (s PlanStatus) IsValid() bool {
	switch s {
	case PlanDraft, PlanAwaitingApproval, PlanApproved, PlanRejected, PlanSuperseded:
		return true
	default:
		return false
	}
}

// Scan implements sql.Scanner and rejects unknown persisted Plan states.
func (s *PlanStatus) Scan(value any) error { return scanStatus(value, s.set) }

func (s *PlanStatus) set(value string) error {
	status := PlanStatus(value)
	if !status.IsValid() {
		return fmt.Errorf("unknown plan status %q", value)
	}
	*s = status
	return nil
}

// Value implements driver.Valuer for PlanStatus.
func (s PlanStatus) Value() (driver.Value, error) { return string(s), nil }

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

// CanTransitionTo reports whether next is a permitted Task lifecycle state.
func (s TaskStatus) CanTransitionTo(next TaskStatus) bool {
	switch s {
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

// IsValid reports whether s is a known Task lifecycle state.
func (s TaskStatus) IsValid() bool {
	switch s {
	case TaskBlocked, TaskQueued, TaskLeased, TaskRunning, TaskVerifying, TaskSucceeded,
		TaskFailed, TaskAwaitingInput, TaskIndeterminate:
		return true
	default:
		return false
	}
}

// Scan implements sql.Scanner and rejects unknown persisted Task states.
func (s *TaskStatus) Scan(value any) error { return scanStatus(value, s.set) }

func (s *TaskStatus) set(value string) error {
	status := TaskStatus(value)
	if !status.IsValid() {
		return fmt.Errorf("unknown task status %q", value)
	}
	*s = status
	return nil
}

// Value implements driver.Valuer for TaskStatus.
func (s TaskStatus) Value() (driver.Value, error) { return string(s), nil }

// AttemptStatus describes the lifecycle of one Task execution attempt.
type AttemptStatus string

const (
	// AttemptRunning is currently executing on its Worker.
	AttemptRunning AttemptStatus = "running"
	// AttemptSucceeded produced a complete Worker result.
	AttemptSucceeded AttemptStatus = "succeeded"
	// AttemptFailed ended with a known execution failure.
	AttemptFailed AttemptStatus = "failed"
	// AttemptIndeterminate was interrupted while side effects might have happened.
	AttemptIndeterminate AttemptStatus = "indeterminate"
	// AttemptCancelled was explicitly stopped.
	AttemptCancelled AttemptStatus = "cancelled"
)

// CanTransitionTo reports whether next is a permitted Attempt lifecycle state.
func (s AttemptStatus) CanTransitionTo(next AttemptStatus) bool {
	return s == AttemptRunning && (next == AttemptSucceeded || next == AttemptFailed || next == AttemptIndeterminate || next == AttemptCancelled)
}

// IsValid reports whether s is a known Attempt lifecycle state.
func (s AttemptStatus) IsValid() bool {
	switch s {
	case AttemptRunning, AttemptSucceeded, AttemptFailed, AttemptIndeterminate, AttemptCancelled:
		return true
	default:
		return false
	}
}

// Scan implements sql.Scanner and rejects unknown persisted Attempt states.
func (s *AttemptStatus) Scan(value any) error { return scanStatus(value, s.set) }

func (s *AttemptStatus) set(value string) error {
	status := AttemptStatus(value)
	if !status.IsValid() {
		return fmt.Errorf("unknown attempt status %q", value)
	}
	*s = status
	return nil
}

// Value implements driver.Valuer for AttemptStatus.
func (s AttemptStatus) Value() (driver.Value, error) { return string(s), nil }

// ChangeRequestStatus describes the lifecycle of an execution-time plan change request.
type ChangeRequestStatus string

const (
	// ChangeRequestPending awaits an operator decision.
	ChangeRequestPending ChangeRequestStatus = "pending"
	// ChangeRequestApproved authorizes creation of a new plan version.
	ChangeRequestApproved ChangeRequestStatus = "approved"
	// ChangeRequestRejected was declined by an operator.
	ChangeRequestRejected ChangeRequestStatus = "rejected"
	// ChangeRequestCancelled was withdrawn before resolution.
	ChangeRequestCancelled ChangeRequestStatus = "cancelled"
)

// CanTransitionTo reports whether next is a permitted change-request lifecycle state.
func (s ChangeRequestStatus) CanTransitionTo(next ChangeRequestStatus) bool {
	return s == ChangeRequestPending && (next == ChangeRequestApproved || next == ChangeRequestRejected || next == ChangeRequestCancelled)
}

// IsValid reports whether s is a known change-request lifecycle state.
func (s ChangeRequestStatus) IsValid() bool {
	switch s {
	case ChangeRequestPending, ChangeRequestApproved, ChangeRequestRejected, ChangeRequestCancelled:
		return true
	default:
		return false
	}
}

// Scan implements sql.Scanner and rejects unknown persisted change-request states.
func (s *ChangeRequestStatus) Scan(value any) error { return scanStatus(value, s.set) }

func (s *ChangeRequestStatus) set(value string) error {
	status := ChangeRequestStatus(value)
	if !status.IsValid() {
		return fmt.Errorf("unknown plan change request status %q", value)
	}
	*s = status
	return nil
}

// Value implements driver.Valuer for ChangeRequestStatus.
func (s ChangeRequestStatus) Value() (driver.Value, error) { return string(s), nil }

// Mission is the durable unit of long-running user intent.
type Mission struct {
	ID                 string
	Goal               string
	AcceptanceContract string
	BudgetCents        int64
	PolicyJSON         string
	CurrentPlanVersion int
	Status             MissionStatus
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// ContractKind values classify what behavior a Task's Worker should perform.
// An empty ContractKind is treated as ContractImplementation everywhere it is
// read (ValidateTaskInputs accepts it; normalizePlanInput defaults it).
const (
	ContractImplementation = "implementation"
	ContractVerification   = "verification"
	ContractIntegration    = "integration"
)

// Task is a dependency-aware unit of work within one Mission.
type Task struct {
	ID           string
	MissionID    string
	Title        string
	ClientID     string
	Position     int
	Contract     string
	ContractKind string
	Status       TaskStatus
	DependsOn    []string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// TaskAttempt records one Worker execution of a Task.
// Callers should use named composite literals because optional execution
// metadata may be appended while existing exported fields remain available.
type TaskAttempt struct {
	ID        string
	TaskID    string
	LeaseID   string
	Worker    string
	Status    AttemptStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PlanVersion is an immutable snapshot of a Mission's proposed Task graph.
type PlanVersion struct {
	ID         string
	MissionID  string
	Version    int
	Status     PlanStatus
	Tasks      []TaskInput
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ApprovedAt *time.Time
}

// WorkspaceLease grants one Task exclusive ownership of an execution workspace.
type WorkspaceLease struct {
	ID         string
	TaskID     string
	Path       string
	Branch     string
	SandboxID  string
	Status     string
	ExpiresAt  time.Time
	CreatedAt  time.Time
	ReleasedAt *time.Time
}

// Event is an append-only record of one Mission domain occurrence.
type Event struct {
	ID        string
	MissionID string
	TaskID    string
	AttemptID string
	Type      string
	Payload   []byte
	CreatedAt time.Time
}

// PlanChangeRequest records a proposed execution-time change that needs approval.
type PlanChangeRequest struct {
	ID               string
	MissionID        string
	BaseVersion      int
	TriggerAttemptID string
	Reason           string
	ImpactedTaskIDs  []string
	PermissionChange string
	BudgetChange     string
	ProposedPlan     PlanVersion
	Status           ChangeRequestStatus
	ResolutionReason string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	ResolvedAt       *time.Time
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
// Callers should use named composite literals because optional verifier metadata
// may be appended while existing exported fields remain available.
type Evidence struct {
	ID                string
	MissionID         string
	TaskID            string
	AttemptID         string
	VerifierAttemptID string
	Kind              string
	Content           []byte
	SHA256            string
	Passed            bool
	CreatedAt         time.Time
}

// CreateMissionInput describes the minimum user intent needed to create a Mission.
type CreateMissionInput struct {
	Goal               string
	AcceptanceContract string
	BudgetCents        int64
	PolicyJSON         string
}

// CreateTaskInput describes a unit of work and its prerequisite Task IDs.
type CreateTaskInput struct {
	MissionID string
	Title     string
	DependsOn []string
}

// TaskInput describes a Task in a client-addressable proposed Plan graph.
type TaskInput struct {
	ClientID     string
	Position     int
	Title        string
	Contract     string
	ContractKind string
	Dependencies []string
}

// PlanInput describes a complete client-addressable Task graph.
type PlanInput struct {
	Tasks []TaskInput
}

// CreatePlanVersionInput describes a proposed immutable Plan snapshot.
type CreatePlanVersionInput struct {
	MissionID string
	Tasks     []TaskInput
}

// CreatePlanChangeRequestInput describes an execution-time proposal that needs approval.
type CreatePlanChangeRequestInput struct {
	MissionID        string
	BaseVersion      int
	TriggerAttemptID string
	Reason           string
	ImpactedTaskIDs  []string
	PermissionChange string
	BudgetChange     string
	ProposedPlan     CreatePlanVersionInput
}

// CreateWorkspaceLeaseInput describes one exclusive Task workspace allocation.
type CreateWorkspaceLeaseInput struct {
	TaskID    string
	Path      string
	Branch    string
	SandboxID string
	ExpiresAt time.Time
}

// CreateEventInput describes one append-only Mission domain event.
type CreateEventInput struct {
	MissionID string
	TaskID    string
	AttemptID string
	Type      string
	Payload   []byte
}

// CreateArtifactInput describes an immutable Worker artifact.
// Named composite literals are the supported source-compatibility contract.
type CreateArtifactInput struct {
	MissionID string
	TaskID    string
	AttemptID string
	Kind      string
	Content   []byte
}

// CreateEvidenceInput describes an immutable verifier result.
// Named composite literals remain compatible when optional metadata is added;
// positional composite literals are not part of the compatibility contract.
type CreateEvidenceInput struct {
	MissionID         string
	TaskID            string
	AttemptID         string
	VerifierAttemptID string
	Kind              string
	Content           []byte
	Passed            bool
}

func validTaskTransition(current, next TaskStatus) bool {
	return current.CanTransitionTo(next)
}

// ValidateTaskInputs verifies that a proposed client-addressable Task graph is complete and acyclic.
func ValidateTaskInputs(inputs []TaskInput) error {
	byClientID := make(map[string]TaskInput, len(inputs))
	for _, input := range inputs {
		clientID := strings.TrimSpace(input.ClientID)
		if clientID == "" {
			return fmt.Errorf("task client ID is required")
		}
		if _, exists := byClientID[clientID]; exists {
			return fmt.Errorf("duplicate task client ID %q", clientID)
		}
		if strings.TrimSpace(input.Title) == "" {
			return fmt.Errorf("task %q title is required", clientID)
		}
		if strings.TrimSpace(input.Contract) == "" {
			return fmt.Errorf("task %q contract is required", clientID)
		}
		switch input.ContractKind {
		case "", ContractImplementation, ContractVerification, ContractIntegration:
		default:
			return fmt.Errorf("task %q has unknown contract kind %q", clientID, input.ContractKind)
		}
		byClientID[clientID] = input
	}

	for clientID, input := range byClientID {
		for _, dependency := range input.Dependencies {
			dependency = strings.TrimSpace(dependency)
			if dependency == "" {
				return fmt.Errorf("task %q has a blank dependency", clientID)
			}
			if dependency == clientID {
				return fmt.Errorf("task %q cannot depend on itself", clientID)
			}
			if _, exists := byClientID[dependency]; !exists {
				return fmt.Errorf("task %q depends on unknown task %q", clientID, dependency)
			}
		}
	}

	const (
		unvisited = iota
		visiting
		visited
	)
	colors := make(map[string]int, len(byClientID))
	var visit func(string) error
	visit = func(clientID string) error {
		switch colors[clientID] {
		case visiting:
			return fmt.Errorf("task dependency cycle includes %q", clientID)
		case visited:
			return nil
		}
		colors[clientID] = visiting
		for _, dependency := range byClientID[clientID].Dependencies {
			if err := visit(strings.TrimSpace(dependency)); err != nil {
				return err
			}
		}
		colors[clientID] = visited
		return nil
	}
	for clientID := range byClientID {
		if err := visit(clientID); err != nil {
			return err
		}
	}
	return nil
}

func scanStatus(value any, set func(string) error) error {
	var raw string
	switch value := value.(type) {
	case string:
		raw = value
	case []byte:
		raw = string(value)
	case nil:
		return fmt.Errorf("status is null")
	default:
		return fmt.Errorf("status has unsupported type %T", value)
	}
	return set(raw)
}
