package worker

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/harness9/internal/hooks"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/permission"
	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/sandbox"
	"github.com/harness9/internal/subagent"
	"github.com/harness9/internal/tools"
)

// RunnerExecutorConfig configures a RunnerExecutor.
type RunnerExecutorConfig struct {
	ProviderFor  func(model string) (provider.LLMProvider, int, error)
	CompactorFor func(p provider.LLMProvider, ctxWin int) memory.Compactor
	SandboxMgr   *sandbox.Manager // optional; nil = no Sandbox for any Attempt
	// SharedHooks is threaded straight through to subagent.RunnerConfig.SharedHooks
	// for every Attempt this Executor runs. Worker Attempts run unattended
	// (Execute always passes background=true to Runner.Run, which auto-denies
	// any HookActionAsk decision since there is no human present to answer an
	// approval prompt), so this is the only seam through which a caller can
	// give the Attempt's bash tool the same high-risk-pattern interception
	// (e.g. hooks.NewDangerHook()) the rest of the codebase relies on. This
	// package deliberately does not construct any default hooks itself —
	// choosing which hooks (if any) to install is left to whoever builds a
	// RunnerExecutorConfig.
	SharedHooks []hooks.ToolHook
	// SettingsPath points at a permission.Hook settings file. Left empty
	// (the common case), Execute generates a throwaway one that pre-approves
	// exactly this Attempt's own baseTools, since permission.Rules.Evaluate
	// asks for anything unmatched by default and an unattended Attempt can
	// never answer that ask itself — see writeUnattendedPermissionSettings.
	// Set explicitly only when a caller needs finer-grained control (e.g.
	// denying a specific tool outright) than the all-tools-allowed default.
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

	settingsPath := e.cfg.SettingsPath
	if settingsPath == "" {
		generatedPath, err := writeUnattendedPermissionSettings(baseTools)
		if err != nil {
			return subagent.SubAgentResult{}, fmt.Errorf("write default permission settings: %w", err)
		}
		defer os.Remove(generatedPath)
		settingsPath = generatedPath
	}

	runner := subagent.NewRunner(subagent.RunnerConfig{
		BaseTools:          baseTools,
		SharedHooks:        e.cfg.SharedHooks,
		SettingsPath:       settingsPath,
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

// writeUnattendedPermissionSettings writes a permission settings file that
// pre-approves exactly the tool categories baseTools grants, then returns
// its path. Worker Attempts run unattended (Execute always passes
// background=true to Runner.Run), and permission.Rules.Evaluate's default
// for anything not explicitly allowed is HookActionAsk — sensible for an
// interactive human session where the TUI can show an approval dialog, but
// fatal for a background Attempt: Runner.Run auto-denies every
// EventApprovalRequired when there is no human present to answer it, so
// without an explicit allowlist an unattended Worker Attempt could never
// execute a single bash or read_file call. DangerHook (installed via
// SharedHooks) still runs after this pre-approval and still gets a real
// Ask-then-auto-deny for genuinely dangerous command patterns — this only
// removes the redundant baseline approval step for tool categories the Task
// Contract already grants; it does not bypass danger-pattern screening.
func writeUnattendedPermissionSettings(baseTools []tools.BaseTool) (string, error) {
	names := make([]string, 0, len(baseTools))
	for _, t := range baseTools {
		names = append(names, t.Name())
	}
	rules := permission.NewRules()
	rules.AddRule(permission.RuleAllow, names)

	f, err := os.CreateTemp("", "harness9-worker-settings-*.json")
	if err != nil {
		return "", fmt.Errorf("create temp settings file: %w", err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close temp settings file: %w", err)
	}
	if err := permission.SaveRules(path, rules); err != nil {
		return "", fmt.Errorf("save settings: %w", err)
	}
	return path, nil
}
