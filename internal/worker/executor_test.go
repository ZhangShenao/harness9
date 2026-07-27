package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harness9/internal/hooks"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/provider/providertest"
	"github.com/harness9/internal/schema"
)

func TestRunnerExecutorReturnsSubAgentFinalText(t *testing.T) {
	repoRoot := newTestRepo(t)
	const wantText = "TASK_RESULT: SUCCESS\nCOMMIT: deadbeef"
	mockLLM := providertest.NewMockWithCallback(func(_ []schema.Message, _ []schema.ToolDefinition) schema.Message {
		return schema.Message{Role: schema.RoleAssistant, Content: wantText}
	})

	executor := NewRunnerExecutor(RunnerExecutorConfig{
		ProviderFor: func(model string) (provider.LLMProvider, int, error) {
			return mockLLM, 100000, nil
		},
		CompactorFor:       func(provider.LLMProvider, int) memory.Compactor { return nil },
		SandboxMgr:         nil, // no Docker required for this test
		DefaultMaxTurns:    5,
		ToolTimeout:        10 * time.Second,
		MaxConcurrentTools: 1,
		BaseCtx:            context.Background(),
	})

	result, err := executor.Execute(context.Background(), repoRoot, "implement the thing")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.FinalText != wantText {
		t.Fatalf("FinalText = %q, want %q", result.FinalText, wantText)
	}
}

// alwaysDenyHook denies every tool call it sees. Used to prove that
// RunnerExecutorConfig.SharedHooks is actually threaded through to the
// subagent.Runner that executes tool calls, rather than being silently
// dropped on the floor.
type alwaysDenyHook struct{}

func (alwaysDenyHook) BeforeExecute(ctx context.Context, tc schema.ToolCall) (context.Context, hooks.HookDecision, error) {
	return ctx, hooks.Deny("denied-by-test-hook"), nil
}

func (alwaysDenyHook) AfterExecute(_ context.Context, _ schema.ToolCall, result schema.ToolResult) schema.ToolResult {
	return result
}

// TestRunnerExecutorThreadsSharedHooks confirms RunnerExecutorConfig.SharedHooks
// reaches the real subagent.Runner built inside Execute: a hook that denies
// every tool call must cause the sub-agent's bash call to come back as an
// error Observation carrying the hook's deny reason, proving the hook chain
// was actually installed (as opposed to being accepted but ignored).
func TestRunnerExecutorThreadsSharedHooks(t *testing.T) {
	repoRoot := newTestRepo(t)
	// The permission hook always runs first in the chain (ahead of
	// SharedHooks) and defaults every unmatched tool call to HookActionAsk,
	// which a background sub-agent auto-denies before any SharedHooks entry
	// gets a turn. Explicitly allow "bash" here so the permission hook lets it
	// through and the chain actually reaches alwaysDenyHook, isolating what
	// this test wants to prove.
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"permissions":{"allow":["bash"]}}`), 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	var observedObservation string
	turn := 0
	mockLLM := providertest.NewMockWithCallback(func(msgs []schema.Message, _ []schema.ToolDefinition) schema.Message {
		turn++
		if turn == 1 {
			return schema.Message{
				Role:    schema.RoleAssistant,
				Content: "invoking bash",
				ToolCalls: []schema.ToolCall{
					{ID: "call_1", Name: "bash", Arguments: []byte(`{"command": "echo hi"}`)},
				},
			}
		}
		// Second turn: the previous message in history is the Observation the
		// engine appended for call_1's result — record it so the test can
		// assert on it, then terminate the loop with a final answer.
		observedObservation = msgs[len(msgs)-1].Content
		return schema.Message{Role: schema.RoleAssistant, Content: "TASK_RESULT: FAILED\nREASON: tool denied"}
	})

	executor := NewRunnerExecutor(RunnerExecutorConfig{
		ProviderFor: func(model string) (provider.LLMProvider, int, error) {
			return mockLLM, 100000, nil
		},
		CompactorFor:       func(provider.LLMProvider, int) memory.Compactor { return nil },
		SandboxMgr:         nil,
		SharedHooks:        []hooks.ToolHook{alwaysDenyHook{}},
		SettingsPath:       settingsPath,
		DefaultMaxTurns:    5,
		ToolTimeout:        10 * time.Second,
		MaxConcurrentTools: 1,
		BaseCtx:            context.Background(),
	})

	if _, err := executor.Execute(context.Background(), repoRoot, "implement the thing"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(observedObservation, "denied-by-test-hook") {
		t.Fatalf("observed tool Observation = %q, want it to contain the SharedHooks deny reason (denied-by-test-hook)", observedObservation)
	}
}
