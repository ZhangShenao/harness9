// Package scheduler (existing package, new file).
package scheduler

import (
	"context"
	"fmt"

	"github.com/harness9/internal/mission"
)

// RoutingDispatcher dispatches each Task to a different Dispatcher based on
// its ContractKind, so the Scheduler itself stays agnostic to how many kinds
// of Task Contract exist. A Task with an empty ContractKind routes to the
// mission.ContractImplementation entry, matching mission.ValidateTaskInputs'
// treatment of an unset ContractKind as an implicit default.
type RoutingDispatcher struct {
	byKind map[string]Dispatcher
}

// NewRoutingDispatcher creates a RoutingDispatcher. byKind's keys are
// mission.ContractKind values (e.g. mission.ContractImplementation); a Task
// whose resolved ContractKind has no matching entry causes Dispatch to
// return an error rather than silently dropping the Task.
func NewRoutingDispatcher(byKind map[string]Dispatcher) *RoutingDispatcher {
	return &RoutingDispatcher{byKind: byKind}
}

// Dispatch implements Dispatcher by delegating to the sub-Dispatcher
// registered for task's ContractKind.
func (r *RoutingDispatcher) Dispatch(ctx context.Context, task mission.Task) error {
	kind := task.ContractKind
	if kind == "" {
		kind = mission.ContractImplementation
	}
	d, ok := r.byKind[kind]
	if !ok {
		return fmt.Errorf("no dispatcher registered for contract kind %q", kind)
	}
	return d.Dispatch(ctx, task)
}
