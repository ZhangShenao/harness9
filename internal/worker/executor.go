package worker

import (
	"context"
	"time"

	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/sandbox"
	"github.com/harness9/internal/subagent"
	"github.com/harness9/internal/tools"
)

// RunnerExecutorConfig configures a RunnerExecutor.
type RunnerExecutorConfig struct {
	ProviderFor        func(model string) (provider.LLMProvider, int, error)
	CompactorFor       func(p provider.LLMProvider, ctxWin int) memory.Compactor
	SandboxMgr         *sandbox.Manager // optional; nil = no Sandbox for any Attempt
	SettingsPath       string
	DefaultMaxTurns    int
	ToolTimeout        time.Duration
	MaxConcurrentTools int
	BaseCtx            context.Context
}

// RunnerExecutor is the real Executor: for every call it builds a fresh,
// throwaway subagent.Runner whose WorkDir is the Attempt's own worktree, then
// runs the implementation Task Contract in it. A fresh Runner per call is
// required because subagent.Runner's WorkDir is fixed at construction and
// cannot be changed per call.
type RunnerExecutor struct {
	cfg RunnerExecutorConfig
}

// NewRunnerExecutor creates a RunnerExecutor from cfg.
func NewRunnerExecutor(cfg RunnerExecutorConfig) *RunnerExecutor {
	return &RunnerExecutor{cfg: cfg}
}

// Execute implements Executor by constructing a dedicated Runner rooted at
// workDir and running ImplementationContract in it. background=true is
// passed to Runner.Run: Worker Attempts are unattended (no human present to
// answer approval prompts), matching exactly the semantics Runner.Run already
// applies for background sub-agent calls (auto-deny approvals, execution
// bound to the Runner's own base context rather than the caller's).
func (e *RunnerExecutor) Execute(ctx context.Context, workDir, prompt string) (subagent.SubAgentResult, error) {
	baseTools := []tools.BaseTool{
		tools.NewBashTool(workDir),
		tools.NewReadFileTool(workDir),
		tools.NewWriteFileTool(workDir),
		tools.NewEditFileTool(workDir),
		tools.NewWebFetchTool(),
		tools.NewWebSearchTool(),
	}
	runner := subagent.NewRunner(subagent.RunnerConfig{
		BaseTools:          baseTools,
		SettingsPath:       e.cfg.SettingsPath,
		WorkDir:            workDir,
		DefaultMaxTurns:    e.cfg.DefaultMaxTurns,
		ToolTimeout:        e.cfg.ToolTimeout,
		MaxConcurrentTools: e.cfg.MaxConcurrentTools,
		ProviderFor:        e.cfg.ProviderFor,
		CompactorFor:       e.cfg.CompactorFor,
		BaseCtx:            e.cfg.BaseCtx,
		SandboxMgr:         e.cfg.SandboxMgr,
	})
	return runner.Run(ctx, ImplementationContract, prompt, true)
}
