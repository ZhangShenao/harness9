package dataset

import (
	"testing"

	"github.com/harness9/internal/evals"
)

// TestMissionFoundationPlanVersioning verifies that a Plan goes through
// draft -> approved -> superseded lifecycle correctly, and that only
// the active PlanVersion is schedulable.
func TestMissionFoundationPlanVersioning(t *testing.T) {
	evals.SetupHermeticEnv(t)
	// This is a state-machine eval, not an Agent eval -- it directly
	// exercises the Store + CommandService without an LLM.
	// It validates the core invariant: approved plans are immutable,
	// new versions supersede old ones.
	// Implementation will use mission.NewStore + mission.NewCommandService
	// to verify the lifecycle. Since this is hermetic (no API keys),
	// it runs in CI without real LLM calls.
	t.Skip("S1 eval: will be activated when mission package is importable from evals/dataset")
}

// TestMissionFoundationCommandIdempotency verifies that duplicate
// commands with the same IdempotencyKey do not double-apply.
func TestMissionFoundationCommandIdempotency(t *testing.T) {
	evals.SetupHermeticEnv(t)
	t.Skip("S1 eval: will be activated when mission package is importable from evals/dataset")
}

// TestMissionFoundationChangeRequestGating verifies that unapproved
// PlanChangeRequests do not affect schedulable state.
func TestMissionFoundationChangeRequestGating(t *testing.T) {
	evals.SetupHermeticEnv(t)
	t.Skip("S1 eval: will be activated when mission package is importable from evals/dataset")
}
