package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/hooks"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/sandbox"
	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/tools"
)

// benchmarkBashTimeout 是 SWE-bench 场景下单条 bash 命令的超时。
// 远大于默认 120s，使 Agent 能运行较慢的测试套件 / 依赖安装（验证修复的关键路径）。
const benchmarkBashTimeout = 300 * time.Second

// benchmarkMaxTurns 是 benchmark 默认 Turn 上限：足够 explore+fix+verify，又能截断失控循环。
const benchmarkMaxTurns = 80

// stallNudgeWindow 是停滞提示窗口：连续该轮数未做任何改动（无 edit/write）即注入一次提示，
// 打断"反复静态重读却不收敛"的空转（轨迹分析 R6：xarray-3364、pylint-7080 烧满 80 轮即此形态）。
const stallNudgeWindow = 10

// stallNudgeText 是停滞时注入的提示（仅作用于发送给 LLM 的临时副本，不持久化）。
const stallNudgeText = "你已连续多轮只在静态读代码 / grep，却没有做任何改动或运行测试。请立刻二选一：" +
	"(1) 运行与改动相关的真实测试以获取反馈；(2) 若已定位问题，做出最小修改后用真实测试验证。不要继续空转重读。"

// verifyGateText 是验证关卡续跑提示：当整条轨迹从未运行过任何测试便自然结束时注入一次，
// 要求 Agent 真正验证后再收尾（轨迹分析 R2：8/8 失败实例均零验证即交卷）。
const verifyGateText = "你似乎尚未运行过任何测试就准备结束。请先在沙箱里复现该 Issue，并运行与你改动相关的现有测试来验证修复是否真的生效" +
	"（必要时用 `python -m ensurepip --upgrade && python -m pip install -e . pytest` 安装依赖、用 timeout_secs 放宽超时）。" +
	"验证通过、或确认环境确实无法运行测试并说明原因后，再结束。"

// testRunnerTokens 是判定一条 bash 命令"是否在运行测试"的子串特征（小写匹配）。
var testRunnerTokens = []string{
	"pytest", "py.test", "-m unittest", "unittest discover",
	"nosetests", "runtests.py", "setup.py test", "manage.py test",
	"django-admin test", "tox",
}

// looksLikeTestRun 用启发式判断 cmd 是否在运行测试套件，用于验证关卡。
// 这是宽松启发式：先排除明显的安装语境（pip install，其中也含 pytest）与只读探索命令
// （grep/cat/ls/find/head/tail 开头），再匹配测试运行特征子串。偏向"宁可少判"——
// 漏判仅多一次温和提示，误判才会跳过本应有的验证提示。
func looksLikeTestRun(cmd string) bool {
	c := strings.ToLower(cmd)
	if strings.Contains(c, "pip install") || strings.Contains(c, "pip3 install") {
		return false
	}
	trimmed := strings.TrimSpace(c)
	for _, ro := range []string{"grep ", "cat ", "ls ", "find ", "head ", "tail "} {
		if strings.HasPrefix(trimmed, ro) {
			return false
		}
	}
	for _, tok := range testRunnerTokens {
		if strings.Contains(c, tok) {
			return true
		}
	}
	return false
}

// defaultBootstrapCmd 返回"可配置·默认自举"路线下的默认依赖安装命令（接通 sandbox.BootstrapCmd 接缝）：
// 恢复 pip（精简镜像可能无 pip）→ 以 editable 模式安装当前仓库及其依赖 → 确保 pytest 存在。
// 全程 best-effort（manager.runBootstrap 内 fail-open）：装不全时 Agent 仍可继续，但绝大多数
// 纯 Python 仓库（flask/requests/pylint/sphinx/pytest 等）与带 wheel 的依赖（numpy/pandas 等）
// 都能就此可运行真实测试。需要本机编译器的仓库可改用官方每实例镜像（设 SANDBOX_IMAGE）。
//
// inst 暂未使用，保留以便将来按 EnvironmentSetupCommit / Version 做每实例定制。
func defaultBootstrapCmd(inst Instance) string {
	return strings.Join([]string{
		"python -m ensurepip --upgrade >/dev/null 2>&1 || true",
		"python -m pip install -e . -q 2>&1 | tail -n 20 || true",
		"python -m pip install -q pytest 2>&1 | tail -n 5 || true",
	}, " ; ")
}

// Config 存储从 CLI flags 解析的运行配置。
type Config struct {
	DatasetPath string
	OutputDir   string
	SampleN     int
	// MaxTurns 为 0 时沿用引擎默认值（500），大于 0 时显式限制。
	MaxTurns    int
	Parallel    int
	Resume      bool
	TimeoutMins int
	Model       string
	// Seed 是按 repo 采样的随机种子。固定默认值保证基准可复现（同 seed → 同实例集），
	// 修复了此前用 time.Now().UnixNano() 导致每次运行抽样不同、无法对比迭代效果的问题。
	Seed int64
	// RunID 标识本次运行，用于将 trajectory 日志写入 logs/<RunID>/，避免多次运行互相覆盖/污染。
	RunID string
}

// resolveModelName 解析最终使用的模型名：cfg.Model > LLM_MODEL 环境变量 > 默认值。
// 与 newProvider 保持相同的优先级逻辑，用于填写 predictions.jsonl 的 model_name_or_path。
func resolveModelName(model string) string {
	if model == "" {
		model = os.Getenv("LLM_MODEL")
	}
	if model == "" {
		model = "openai/gpt-4o-mini"
	}
	return model
}

// newProvider 根据模型名创建 LLM provider。
// 优先级：cfg.Model > LLM_MODEL 环境变量 > 默认值 openai/gpt-4o-mini。
func newProvider(model string) (provider.LLMProvider, error) {
	return provider.NewOpenAIProvider(resolveModelName(model))
}

// runInstance 对单个 SWE-bench instance 执行完整的 clone → sandbox → engine → patch 流程。
// 任何环境错误都返回 RunResult.Error，不 panic。
func runInstance(ctx context.Context, inst Instance, cfg Config) RunResult {
	start := time.Now()

	// 1. 创建隔离临时目录
	tmpDir, err := os.MkdirTemp("", "swebench-"+inst.InstanceID+"-*")
	if err != nil {
		return RunResult{Instance: inst, Error: fmt.Errorf("创建临时目录失败: %w", err), Duration: time.Since(start)}
	}
	defer os.RemoveAll(tmpDir)

	// 2. git clone + checkout base_commit（宿主机执行）
	// 使用 --filter=blob:none（blobless clone）：只拉取 commits 和 tree 元数据，
	// 不下载文件内容；checkout 时按需拉取，对大仓库（Django/Sympy 等）速度快 10x+。
	repoURL := "https://github.com/" + inst.Repo
	cloneCtx, cloneCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cloneCancel()

	cloneOut, err := exec.CommandContext(cloneCtx, "git", "clone", "--filter=blob:none", repoURL, tmpDir).CombinedOutput()
	if err != nil {
		return RunResult{Instance: inst, Error: fmt.Errorf("git clone 失败: %w\n%s", err, cloneOut), Duration: time.Since(start)}
	}
	checkoutCtx, checkoutCancel := context.WithTimeout(ctx, 3*time.Minute)
	defer checkoutCancel()
	checkoutOut, err := exec.CommandContext(checkoutCtx, "git", "-C", tmpDir, "checkout", inst.BaseCommit).CombinedOutput()
	if err != nil {
		return RunResult{Instance: inst, Error: fmt.Errorf("git checkout 失败: %w\n%s", err, checkoutOut), Duration: time.Since(start)}
	}

	// 3. 创建 Docker Sandbox 环境
	// SWE-bench 仓库需要 Python 环境；若用户未通过 SANDBOX_IMAGE 显式覆盖，
	// 默认使用 python:3.11（非 slim：自带 pip 与更全的运行库，便于 editable 安装与
	// 从 wheel 拉取 numpy/pandas 等依赖），替代默认的 ubuntu:22.04。
	// 需要本机编译器的仓库（如 astropy C 扩展）可设 SANDBOX_IMAGE 指向官方每实例镜像。
	sandboxCfg := sandbox.DefaultConfig()
	if os.Getenv("SANDBOX_IMAGE") == "" {
		sandboxCfg.Image = "python:3.11"
	}
	// 依赖自举（接通已存在的 BootstrapCmd 接缝）：用户未显式提供 SANDBOX_BOOTSTRAP_CMD 时，
	// 注入默认的 editable 安装命令，让真实测试在 Agent 启动前即变得可运行——这是恢复
	// 验证闭环、把"纯静态分析蒙答案"变为"真实测试驱动收敛"的决定性一步（轨迹分析 R1）。
	if strings.TrimSpace(sandboxCfg.BootstrapCmd) == "" {
		sandboxCfg.BootstrapCmd = defaultBootstrapCmd(inst)
	}
	// macOS Docker Desktop 用 VirtioFS 处理 bind mount，大型 git repo 的 volume
	// 注册比 Linux 慢，30s（默认值）容易触发超时；扩大到 90s 留足缓冲。
	sandboxCfg.StartTimeout = 90 * time.Second
	mgr := sandbox.NewManager(sandboxCfg)
	// sandboxCtx 必须 > StartTimeout（90s），否则外层超时先触发，
	// 使内部 StartTimeout 的 90s 缓冲完全无效。设为 120s 留有余量。
	sandboxCtx, sandboxCancel := context.WithTimeout(ctx, 120*time.Second)
	defer sandboxCancel()
	env, err := mgr.Create(sandboxCtx, tmpDir)
	if err != nil {
		return RunResult{Instance: inst, Error: fmt.Errorf("sandbox 创建失败: %w", err), Duration: time.Since(start)}
	}
	defer func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		mgr.DestroyAll(cleanCtx)
	}()

	// 4. 注册工具
	registry := tools.NewRegistry()
	toolList := []tools.BaseTool{
		// 放宽 bash 超时（默认 120s → 300s），让真实测试套件 / 依赖安装得以完成。
		tools.NewBashTool(tmpDir, tools.WithEnvironment(env), tools.WithBashTimeout(benchmarkBashTimeout)),
		tools.NewReadFileTool(tmpDir, tools.ReadFileWithEnvironment(env)),
		tools.NewWriteFileTool(tmpDir, tools.WriteFileWithEnvironment(env)),
		tools.NewEditFileTool(tmpDir, tools.EditFileWithEnvironment(env)),
	}
	for _, t := range toolList {
		if err := registry.Register(t); err != nil {
			return RunResult{Instance: inst, Error: fmt.Errorf("注册工具失败: %w", err), Duration: time.Since(start)}
		}
	}
	hookReg := hooks.NewHookRegistry(registry)

	// 5. 构造 provider 和 engine
	// MaxTurns=0 时不传 WithMaxTurns，沿用引擎默认值（500）。
	llm, err := newProvider(cfg.Model)
	if err != nil {
		return RunResult{Instance: inst, Error: fmt.Errorf("创建 LLM provider 失败: %w", err), Duration: time.Since(start)}
	}
	// 上下文窗口与压缩器：此前 benchmark 未配置 compactor，长轨迹上下文无界增长，
	// 触及模型窗口后 API 返回 400（prompt too long），在无重试时直接杀实例。
	// 这里用无需 LLM、无需 session 的 TokenBudgetCompactor 做字符级裁剪；
	// 预算取上下文窗口的 55%，为工具定义(~25K) + 输出预留 + chars/4 估算误差留足余量。
	lim := provider.GetModelLimits(resolveModelName(cfg.Model))
	compactor := &memory.TokenBudgetCompactor{
		MaxTokens:       lim.ContextTokens * 55 / 100,
		MinTailMessages: 8,
	}
	// 内存会话：承载验证关卡的多轮续跑（第二次 RunStream 复用全部历史继续对话）。
	// 纯内存、随实例生命周期销毁，不落盘。
	sess := memory.NewMemorySession("swebench-" + inst.InstanceID)
	engOpts := []engine.Option{
		engine.WithPromptBuilder(&swebenchPromptBuilder{instance: inst, workDir: tmpDir}),
		engine.WithContextWindow(lim.ContextTokens),
		engine.WithCompactor(compactor),
		engine.WithSession(sess),
		// 无人值守：显式短路审批，零延迟（不依赖是否注册了 hook）。
		engine.WithPermissionMode(engine.PermissionModeBypassAll),
		// 瞬时 LLM/流式错误的应用层重试：把"一次抖动杀实例"变为可恢复事件。
		engine.WithGenerateRetry(4, 2*time.Second),
		// 停滞提示：连续多轮无改动/无测试运行时注入一次提示，打断盲目空转（轨迹分析 R6）。
		engine.WithStallReminder(stallNudgeWindow, stallNudgeText),
		// 无人值守场景的重复死循环护栏：同一调用 10 轮内出现 4 次即注入定向提醒，
		// 再犯硬终止（烧满 benchmarkMaxTurns 才停的失控轨迹由此止损）。
		engine.WithRepetitionReminder(10, 4),
	}
	if cfg.MaxTurns > 0 {
		engOpts = append(engOpts, engine.WithMaxTurns(cfg.MaxTurns))
	} else {
		// benchmark 默认上限：足够 explore+fix+verify，又能截断失控循环
		// （此前沿用引擎默认 500，配合每实例超时会在卡死实例上烧掉大量 token）。
		engOpts = append(engOpts, engine.WithMaxTurns(benchmarkMaxTurns))
	}
	eng := engine.NewAgentEngine(llm, hookReg, tmpDir, engOpts...)

	// 6. 执行 agent loop（带 per-instance 超时），同时将完整 trajectory 写入日志
	instanceCtx, instanceCancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutMins)*time.Minute)
	defer instanceCancel()

	// logs/<RunID>/ 目录由 main.go 在进入并发循环前统一创建（os.MkdirAll），此处无需再建。
	// 按 RunID 命名空间隔离，避免不同运行的同名日志互相覆盖、污染分析。
	logPath := filepath.Join(cfg.OutputDir, "logs", cfg.RunID, inst.InstanceID+".log")
	runErr := runWithVerificationGate(instanceCtx, eng, "请修复上述 Issue。", logPath, inst)

	// 7. 收集 patch（无论 runErr 如何，MaxTurns 触发时也可能有部分 patch）
	// 先 `git add -A -N`（intent-to-add）登记新建文件：纯 `git diff` 只输出已跟踪文件的改动，
	// Agent 用 write_file 新建的修复文件不会出现在 diff 中而被静默丢弃。-N 使新文件以
	// 新增 hunk 形式进入 `git diff`，从而被完整捕获进 model_patch。
	diffCtx, diffCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer diffCancel()
	_ = exec.CommandContext(diffCtx, "git", "-C", tmpDir, "add", "-A", "-N").Run()
	patchOut, _ := exec.CommandContext(diffCtx, "git", "-C", tmpDir, "diff").CombinedOutput()
	patch := strings.TrimSpace(string(patchOut))

	if runErr != nil && patch == "" {
		return RunResult{Instance: inst, Error: runErr, Duration: time.Since(start)}
	}
	return RunResult{Instance: inst, Patch: patch, Duration: time.Since(start)}
}

// runWithVerificationGate 执行 agent loop 并将完整 trajectory 写入 logPath，
// 在 Agent 自然结束却"全程未运行过任何测试"时，注入一次续跑提示要求真实验证（验证关卡，
// 轨迹分析 R2）。续跑复用同一引擎 + 内存会话（历史完整延续），且至多一次，由 per-instance
// 超时与 turn 上限共同兜底，避免在不可运行环境里 livelock。日志文件创建失败时 fail-open。
func runWithVerificationGate(ctx context.Context, eng *engine.AgentEngine, userPrompt, logPath string, inst Instance) error {
	// 创建日志文件（fail-open：失败时写入 Discard，agent 继续运行）
	var w io.Writer = io.Discard
	if lf, err := os.Create(logPath); err == nil {
		bw := bufio.NewWriter(lf)
		defer func() { bw.Flush(); lf.Close() }()
		w = bw
	}

	// 写文件头
	fmt.Fprintf(w, "=== SWE-bench Instance: %s ===\n", inst.InstanceID)
	fmt.Fprintf(w, "Repo:        %s\n", inst.Repo)
	fmt.Fprintf(w, "BaseCommit:  %s\n", inst.BaseCommit)
	fmt.Fprintf(w, "StartTime:   %s\n\n", time.Now().Format("2006-01-02 15:04:05"))

	ranTest, runErr := streamOnce(ctx, eng, w, userPrompt)

	// 验证关卡：Agent 已自然结束（ctx 未取消）但全程未运行任何测试 → 注入一次续跑提示。
	if !ranTest && ctx.Err() == nil {
		fmt.Fprint(w, "\n\n=== 验证关卡：未检测到任何测试运行，注入续跑提示要求真实验证 ===\n")
		ranTest2, err2 := streamOnce(ctx, eng, w, verifyGateText)
		runErr = errors.Join(runErr, err2)
		if !ranTest2 {
			fmt.Fprint(w, "\n\n=== 验证关卡：续跑后仍未运行测试（可能环境无法运行或 Agent 坚持静态分析）===\n")
		}
	}

	return runErr
}

// streamOnce 驱动一次 RunStream，把所有事件以可读格式写入 w，并返回本次是否运行过测试。
// ranTest 通过对 bash 工具调用的 command 应用 looksLikeTestRun 启发式判定。
func streamOnce(ctx context.Context, eng *engine.AgentEngine, w io.Writer, userPrompt string) (ranTest bool, runErr error) {
	stream, err := eng.RunStream(ctx, userPrompt)
	if err != nil {
		return false, err
	}

	currentTurn := 0
	for evt := range stream {
		// 新 Turn 时打印分隔符
		if evt.Turn > 0 && evt.Turn != currentTurn {
			currentTurn = evt.Turn
			fmt.Fprintf(w, "\n\n--- Turn %d ---\n", currentTurn)
		}

		switch evt.Type {
		case engine.EventActionDelta:
			fmt.Fprint(w, evt.Data.(string))

		case engine.EventThinkingDelta:
			fmt.Fprint(w, evt.Data.(string))

		case engine.EventToolStart:
			tc := evt.Data.(schema.ToolCall)
			fmt.Fprintf(w, "\n\n[Tool Call: %s]\n%s\n", tc.Name, string(tc.Arguments))
			// 验证关卡信号：检测 bash 是否在运行测试（解析 command 字段后判定，避免误把
			// `grep pytest` 当成测试运行）。
			if tc.Name == "bash" {
				var a struct {
					Command string `json:"command"`
				}
				if json.Unmarshal(tc.Arguments, &a) == nil && looksLikeTestRun(a.Command) {
					ranTest = true
				}
			}

		case engine.EventToolResult:
			trd := evt.Data.(engine.ToolResultData)
			status := "ok"
			if trd.Result.IsError {
				status = "error"
			}
			fmt.Fprintf(w, "\n[Tool Result: %s | %s | %s]\n%s\n",
				trd.Result.ToolCallID, trd.Duration.Round(time.Millisecond), status,
				trd.Result.Output)

		case engine.EventTokenUpdate:
			tud := evt.Data.(engine.TokenUpdateData)
			fmt.Fprintf(w, "\n[Tokens: %d]\n", tud.EstimatedTokens)

		case engine.EventCompaction:
			cd := evt.Data.(memory.CompactionRecord)
			fmt.Fprintf(w, "\n[Compaction: %d→%d tokens, %d→%d msgs]\n",
				cd.TokensBefore, cd.TokensAfter, cd.MsgsBefore, cd.MsgsAfter)

		case engine.EventError:
			// errors.Join 累积多次 EventError，保留完整错误链而非只保留最后一条。
			runErr = errors.Join(runErr, fmt.Errorf("%s", evt.Data.(string)))

		case engine.EventApprovalRequired:
			// Benchmark 无人值守模式：自动批准所有工具调用
			req := evt.Data.(engine.ApprovalRequest)
			fmt.Fprintf(w, "\n[Auto-Approved: %s]\n", req.ToolCall.Name)
			req.ResponseCh <- hooks.ApprovalResponse{Approved: true}
		}
	}

	return ranTest, runErr
}
