// Package scheduler provides the deterministic, LLM-free dispatch loop that
// schedules Mission Tasks to Workers. The Scheduler itself never uses an LLM
// to decide safety-critical state -- it only enforces concurrency, budget,
// and policy constraints.
package scheduler

import (
	"context"
	"fmt"

	"github.com/harness9/internal/mission"
)

// Result describes the outcome of a Dispatch call.
type Result struct {
	Status     string
	Artifact   *mission.CreateArtifactInput
	ExitReason string
}

// Dispatcher executes one Task Attempt and returns a structured result.
// Implementations are responsible for acquiring leases, running workers,
// recording artifacts, and cleaning up.
type Dispatcher interface {
	Dispatch(ctx context.Context, task mission.Task, attempt mission.TaskAttempt) (Result, error)
}

// ErrNoDispatcher is returned when no Dispatcher is registered for a ContractKind.
var ErrNoDispatcher = fmt.Errorf("no dispatcher registered for contract kind")

// RoutingDispatcher routes Dispatch calls to per-ContractKind Dispatchers.
// The Scheduler uses this and remains agnostic to how many Contract kinds exist.
type RoutingDispatcher struct {
	impl map[mission.ContractKind]Dispatcher
}

// NewRoutingDispatcher creates an empty RoutingDispatcher.
func NewRoutingDispatcher() *RoutingDispatcher {
	return &RoutingDispatcher{impl: make(map[mission.ContractKind]Dispatcher)}
}

// Register associates a Dispatcher with a ContractKind.
func (r *RoutingDispatcher) Register(kind mission.ContractKind, d Dispatcher) {
	r.impl[kind] = d
}

// Dispatch routes to the registered Dispatcher for the task's ContractKind.
func (r *RoutingDispatcher) Dispatch(ctx context.Context, task mission.Task, attempt mission.TaskAttempt) (Result, error) {
	d, ok := r.impl[task.ContractKind]
	if !ok {
		return Result{}, fmt.Errorf("%w: %s", ErrNoDispatcher, task.ContractKind)
	}
	return d.Dispatch(ctx, task, attempt)
}
