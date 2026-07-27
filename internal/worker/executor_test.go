package worker

import (
	"context"
	"testing"
	"time"

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
