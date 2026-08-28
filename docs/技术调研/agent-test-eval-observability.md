# Agent 框架自动化测试、评估与可观测体系调研报告

> 调研日期：2026-06-09
> 调研范围：DeepAgents、OpenHarness、OpenCode、OpenClaw、HermesAgent、Claude Agent SDK

---

## 1. Executive Summary

### 核心发现

经过对 6 个主流 Agent 框架的深度调研，得出以下核心发现：

**测试与评估层面**

- **DeepAgents** 建立了目前最完备的 Agent 评估体系，包含 118 个评估用例、LLM-as-Judge 机制、雷达图可视化、CI 多轮 Trial 聚合，是最值得参考的工业级范本
- **HermesAgent** 采用"hermetic 隔离测试"模式：严格隔断 API 密钥、使用独立临时目录、确定性 locale/timezone，保证本地与 CI 环境一致；另有独立的 `batch_runner.py` 用于 SWE Benchmark 批量运行
- **OpenHarness** 以"真实 API 调用验证真实大任务"为方向，用 6 个复合功能任务（安全审计、多 Agent 协作、内存持久化等）衡量 Agent 综合能力
- **OpenClaw** 采用矩阵化 Provider 扫描（覆盖 Anthropic/OpenAI/Google 等 9 个供应商），有独立的 `qa/` 场景目录支持人工/半自动评估
- **Claude Agent SDK** 依托 Hooks 系统提供完整可观测性接入点，同时原生支持 OpenTelemetry 三信号导出（Traces/Metrics/Log Events）

**可观测性层面**

- **Claude Agent SDK** 是 6 个框架中唯一有文档记录的原生 OpenTelemetry 集成方案，提供 `claude_code.interaction`、`claude_code.llm_request`、`claude_code.tool` 等标准化 Span，支持 W3C Trace Context 传播，支持 Sub-Agent 调用链嵌套
- **OpenClaw** 的 `CoreAgentHarness` 实现了细粒度事件总线（20+ 事件类型），是"自研可观测层"路线的代表
- **HermesAgent** 采用文件日志路线：4 个专项日志文件（agent/errors/gateway/gui）+ session_id 关联 + 组件级过滤

**CI/CD 层面**

- **DeepAgents** 的 CI 设计最为成熟：Provider 串行/跨 Provider 并行的矩阵执行；baseline/hillclimb 双层评估 tier；`evals_trials.yml` 多轮 Trial 统计（均值/标准差/per-category 细分）；Radar Chart 自动发布至 GitHub Pages
- **HermesAgent** 的 CI 采用 LPT 算法负载均衡分片，per-file 隔离执行，历史时长数据驱动的动态分片

### 关键建议（优先级排序）

| 优先级 | 方向 | 建议 |
|--------|------|------|
| P0 | Tracing | 接入 OpenTelemetry，借鉴 Claude Agent SDK 方案，在 engine/provider 层埋点 |
| P0 | 评估数据集 | 构建 Golden Dataset：覆盖工具调用准确率、多轮对话连贯性、Planning 完成率等核心能力 |
| P1 | LLM-as-Judge | 引入 openevals 风格的 LLM 评判，对非确定性输出（回答质量、规划合理性）给出 0/1 评分 |
| P1 | CI 评估 Pipeline | 基于 `baseline` 层评估用例建立 Quality Gate，PR 触发自动评估 |
| P2 | Hermetic 测试隔离 | 仿 HermesAgent 模式，在 CI 中严格隔断 API Key，使用 mock provider 保证确定性 |
| P2 | 多轮 Trial 统计 | 对非确定性评估项跑 N 次取均值/标准差，规避单次偶然性 |

---

## 2. 各框架调研

### 2.1 DeepAgents (LangChain)

**基础信息**

- GitHub: https://github.com/langchain-ai/deepagents
- 语言：Python + TypeScript
- Stars：24,218（截至调研日期）
- 定位："batteries-included agent harness"，构建在 LangGraph 之上

#### 2.1.1 评估架构

DeepAgents 在 `libs/evals/` 目录维护了一套完整的 Agent 评估框架，是所有调研框架中最系统化的评估设计。

**目录结构**

```
libs/evals/
├── deepagents_evals/
│   ├── cli.py           # 评估 CLI 入口
│   ├── radar.py         # 雷达图可视化
│   ├── trial_summary.py # 多轮 Trial 聚合
│   └── categories.json  # 评估类别配置
├── tests/
│   └── evals/
│       ├── conftest.py       # LangSmith 集成 + 模型初始化
│       ├── llm_judge.py      # LLM-as-Judge 实现
│       ├── utils.py          # AgentTrajectory + Assertion 框架
│       ├── test_tool_selection.py
│       ├── test_memory.py
│       ├── test_subagents.py
│       └── ...（共 15 个评估文件）
```

**评估类别（8 个维度）**

| 类别 | 说明 |
|------|------|
| file_operations | 文件读写、编辑、目录操作 |
| retrieval | 搜索与信息提取 |
| tool_use | 工具选择与执行 |
| memory | 跨轮次信息持久化与召回 |
| conversation | 对话质量与约束满足 |
| summarization | 内容摘要与任务延续 |
| unit_test | 单个组件功能验证 |
| upstream_middleware | LangChain 集成层行为 |

**外部 Benchmark 集成**

```
FRAMES     — 信息检索评估
NEXUS      — 工具编排测试
BFCL v3   — Function Calling 能力
TAU2 Airline — 对话式 Agent 场景
Memory Agent Bench — 多轮记忆性能
```

#### 2.1.2 LLM-as-Judge 机制

`llm_judge.py` 实现了一个基于 `openevals.llm.create_llm_as_judge` 的评判器：

```python
# 评判模式 1：仅评判文本输出
_RESPONSES_PROMPT = """
You are a strict grading assistant.
Evaluate the agent's final text response against this criterion:
{criterion}
"""

# 评判模式 2：评判完整 Trajectory（含工具调用）
_TRAJECTORY_PROMPT = """
...
Tool calls are real actions the agent executed.
Treat them as evidence that the action was performed.
"""
```

**评判流程**：
1. 每个 criterion 独立评判，返回 `{"score": 0/1, "comment": "..."}`
2. 所有 criterion 均通过才算整体通过
3. 结果通过 `LogSmith feedback` API 上报，带 binary score + summary comment

#### 2.1.3 Assertion 框架

`utils.py` 中的 `AgentTrajectory` + Assertion 体系将评估分为两类：

```python
# 硬失败断言（SuccessAssertion）—— 违反则 test fail
FinalTextContains(text="expected keyword")
FinalTextExcludes(text="forbidden keyword")
FinalTextMinLength(min_len=200)
FileEquals(path="output.py", content=...)
FileContains(path="report.md", text="security")
FileExcludes(path="output.py", text="TODO")

# 效率断言（EfficiencyAssertion）—— 仅记录，不 fail
AgentSteps(expected=2)        # 期望的推理步数
ToolCallRequests(expected=3)  # 期望的工具调用次数
MaxToolCallRequests(max=5)    # 最多工具调用次数
ToolCall(name="slack_send_dm", args_contains={"user_id": "U12345"})
```

#### 2.1.4 评估指标体系

从 `evals_report.json` 中聚合的指标：

| 指标 | 说明 |
|------|------|
| correctness | 任务是否成功完成（0-1） |
| solve_rate | 通过率 |
| step_ratio | 实际步数 / 期望步数 |
| tool_call_ratio | 实际工具调用数 / 期望数 |
| median_duration_s | 中位数执行时长（秒） |

每个 eval_category 单独统计 correctness，radar.py 将其渲染为雷达图（极坐标图），支持多模型对比叠加。

#### 2.1.5 CI/CD 集成

**三层工作流设计**

```yaml
# 1. _eval.yml —— 可复用单次评估（被矩阵调用）
jobs:
  run-evals:
    steps:
      - run: make evals MODEL="$MODEL"
      - run: python scripts/generate_summary.py  # 生成 Markdown 摘要
      - upload-artifact: evals_report.json

# 2. evals.yml —— Provider 矩阵并行评估
strategy:
  matrix: [anthropic, openai, openrouter, google, ...]
  max-parallel: 1  # 同一 Provider 内串行（避免限速）
# 跨 Provider 并行，所有完成后聚合 Radar Chart 并发布到 eval-assets 分支

# 3. evals_trials.yml —— 多轮 Trial 统计
jobs:
  aggregate-trials:
    steps:
      - run: scripts/run_trials.py --aggregate-only
      # 计算 mean/median/std_dev/min/max
      # 检测 surviving trials 数量，不足时 exit non-zero
```

**Tier 系统（防回归核心）**

```python
# conftest.py 中注册的评估分层
@pytest.mark.eval_tier("baseline")   # 回归门控：每个 PR 都跑
@pytest.mark.eval_tier("hillclimb")  # 进步追踪：定期跑（更难的任务）
```

**LangSmith 强制集成**

```python
# conftest.py：LangSmith 不可用时 abort
assert os.environ.get("LANGSMITH_TRACING"), "LangSmith tracing required"

# 每次 eval 运行记录元数据
{
    "model": "claude-sonnet-4-6",
    "date": "2026-06-09",
    "deepagents_version": "0.x.y"
}
```

---

### 2.2 OpenHarness (HKUDS)

**基础信息**

- GitHub: https://github.com/HKUDS/OpenHarness
- 语言：Python
- Stars：13,655
- 定位：开放式 Agent Harness，内置个人助手 Ohmo

#### 2.2.1 测试架构

OpenHarness 的测试结构分层清晰，共 30 个子目录覆盖所有核心模块：

```
tests/
├── conftest.py
├── test_real_large_tasks.py    # 6 个复合真实任务评估
├── test_hooks_skills_plugins_real.py  # 真实 API 验证
├── test_merged_prs_on_autoagent.py    # PR 级别 Agent 评估
├── test_platforms.py
├── test_untested_features.py
└── test_{engine,memory,tools,sandbox,...}/  # 单元测试
```

#### 2.2.2 真实大任务评估模式

`test_real_large_tasks.py` 展示了 OpenHarness 最有特色的评估方式：**用真实 API 调用验证复合功能**：

**6 个核心任务**

| 任务 | 涉及模块 | 核心指标 |
|------|----------|----------|
| 安全审计 | Hooks + 权限控制 + web_fetch + grep | 工具执行计数、Hook 日志条目、高危模式检测率 |
| Coordinator 代码审查 | 多 Agent 协调 + 邮箱通信 | Worker 完成时间、团队成员数、综合质量关键词 |
| Migration 计划（含记忆） | Skill 加载 + 记忆持久化 + Session 导出 | 记忆文件创建数、Session 快照格式、MD 导出大小 |
| Bug 修复（隔离工作区） | 文件编辑 + 测试执行 | Worktree 创建成功、原始仓库不变、测试通过 |
| 全流程 Pipeline | Coordinator + 3 并发 Worker + 权限同步 | Worker 完成状态、权限解决流程、综合合成质量 |
| Session 重构 | Session 保存/恢复 + 多轮 + 成本追踪 | Session 文件存在、消息数、Token 计数、代码可编译 |

**数据采集函数 `collect()`**

```python
def collect():
    return {
        "text_chars": len(assistant_text),      # 文本生成量
        "tools_used": [t.name for t in calls],  # 工具调用列表
        "tool_results": {                        # 工具输出摘要
            t.name: {"status": "ok"|"error", "preview": output[:200]}
        },
        "turns": turn_count,                    # 轮次数
        "tokens": {"input": n_in, "output": n_out}  # Token 用量
    }
```

#### 2.2.3 Hook + Skill 行为验证

`test_hooks_skills_plugins_real.py` 验证的不是输出内容，而是**模型对限制的适应行为**：

```python
# 核心断言逻辑
bash_blocked = "bash" in blocked_tools
used_alternative = "glob" in tools_used or "grep" in tools_used
has_answer = len(response_text) > 50

assert bash_blocked and used_alternative and has_answer
# 含义：bash 被 hook 阻断后，模型换用 glob/grep 完成任务
```

#### 2.2.4 Autopilot 服务可观测性

`src/openharness/autopilot/service.py` 中实现了内部可观测机制：

```python
# 基于 Journal 的事件流（append-only）
def append_journal(kind, summary, task_id=None, metadata=None):
    entry = RepoJournalEntry(
        timestamp=time.time(),
        kind=kind,           # intake_added / run_finished / verification_failed / ci_failed_retry
        summary=summary,
        task_id=task_id,
        metadata=metadata or {}
    )

# 任务状态统计
def stats() -> dict[str, int]:
    counts = {}
    for card in self._load_registry().cards:
        counts[card.status] = counts.get(card.status, 0) + 1
    return counts

# 失败阶段追踪（根因分析）
failure_stages = [
    "git_prepare_failed",
    "local_verification_failed",
    "remote_ci_failed",
    "agent_runtime_error"
]
```

---

### 2.3 OpenCode (Anomaly)

**基础信息**

- GitHub: https://github.com/anomalyco/opencode
- 语言：TypeScript
- Stars：171,725
- 定位：开源编程 Agent

#### 2.3.1 测试体系

OpenCode 的测试使用 Vitest 框架，覆盖范围广（54 个顶层测试文件）但 Agent 行为评估相对薄弱，主要集中在**工程质量保障**层面：

**测试层次**

```
test/
├── architecture-smells.test.ts    # 架构边界检查（循环依赖）
├── extension-import-boundaries.test.ts  # 扩展包导入边界
├── cli-json-stdout.e2e.test.ts    # E2E CLI 输出格式验证
├── gateway.multi.e2e.test.ts      # 多实例 Gateway E2E
├── openclaw-launcher.e2e.test.ts  # 启动器 E2E
├── vitest-*.test.ts               # 测试配置自检
└── global-setup.ts / setup.ts    # 测试环境隔离
```

**E2E 测试设计**

`global-setup.ts` 采用"home isolation"：每个测试进程使用独立的临时目录作为 HOME，避免测试间状态污染。这与 HermesAgent 的 hermetic 隔离思路相同。

#### 2.3.2 CI/CD 设计

OpenCode 的 CI（`ci.yml`）采用**preflight 路由 + 多 lane 并行**模式，值得 harness9 学习：

```
preflight (路由层)
  → 分析 git diff，计算变更范围
  → 输出 job matrix 配置

并行执行：
├── security-fast      # 私钥检测 + 依赖审计（最快，阻塞其他）
├── node-tests         # TS 测试（多 shard）
├── platform-tests     # Windows/macOS/Android/Python
└── type-check + lint  # 类型安全 + 代码质量
```

**shard 负载均衡**：测试文件按历史执行时长分片，dist build 缓存共享，pnpm store 预热。

---

### 2.4 OpenClaw（OpenClaw）

**基础信息**

- GitHub: https://github.com/openclaw/openclaw
- 语言：TypeScript
- Stars：377,669
- 定位：全平台个人 AI 助手

#### 2.4.1 CoreAgentHarness —— 自研可观测层

OpenClaw 的 `packages/agent-core/src/harness/agent-harness.ts`（39KB）是所有框架中最完整的"自研 Agent 可观测层"实现：

**事件总线架构**

```typescript
// 两种订阅模式
subscribe(listener)              // 订阅所有事件（全局监听）
on(type, handler)                // 订阅特定类型事件（类型安全）
```

**完整事件类型（20+）**

| 事件类别 | 具体事件 |
|----------|----------|
| 执行事件 | `message_start`, `message_end`, `turn_end`, `agent_end` |
| 队列事件 | `queue_update`（steering/follow-up/next-turn 三队列状态） |
| 生命周期 | `settled`（空闲就绪信号） |
| 模型变更 | `model_select`, `thinking_level_select` |
| 资源更新 | `resources_update` |
| Hook 拦截 | `before_agent_start`, `context`, `tool_call`, `tool_result`, `before_provider_request`, `before_provider_payload`, `after_provider_response` |
| Session | `session_before_compact`, `session_compact` |

**可观测指标（从类型定义提取）**

```typescript
interface AgentHarnessMetrics {
    // Token 用量（从 assistant messages 提取）
    token_usage: { input: number; output: number; cacheRead: number; cacheWrite: number }
    // 执行时间
    timestamp: Date
    // 错误状态
    stop_reason: "aborted" | "error" | "natural"
    error_message?: string
    // Session 状态
    had_pending_mutations: boolean
}
```

**Hook 拦截机制**

```typescript
// 测试/观测层通过 hook 拦截工具调用
type ToolCallResult = {
    block?: boolean        // 阻断工具执行
    block_reason?: string  // 阻断原因（传给 LLM）
}

// 支持在测试中注入 mock 工具结果
type ToolResultPatch = {
    updated_output?: string  // 替换真实工具输出
}
```

#### 2.4.2 QA 场景目录

`qa/scenarios/` 按功能域组织 15 个场景类别（agents/channels/memory/runtime/security/scheduling 等），每个场景是独立的 markdown 文件，包含手动或半自动执行步骤。

`qa/frontier-harness-plan.md` 揭示了 OpenClaw 的前沿测试策略：
- 三模型基线：GPT（主）→ Claude（次）→ Gemini（验证）
- 优先 frontier 子集稳定化，再全量测试
- 人工评估维度：是否先读上下文、是否给出具体洞察（非泛泛回答）、是否保持正常行为（不扩大范围）

#### 2.4.3 CI 中的 Live 模型测试

`openclaw-live-and-e2e-checks-reusable.yml` 实现了**多 Provider 矩阵化扫描**：

```yaml
providers: [anthropic, google, openai, minimax, opencode, openrouter, xai, zai, fireworks]
max_models_per_provider: 6
timeout_per_model: 45s
advisory_flag: true  # non-critical provider 测试不阻塞发布
```

25+ 个独立 Live 测试套件，覆盖：gateway profiles、backend 系统、基础设施韧性、extension 兼容性、Prompt Caching 验证等。

---

### 2.5 HermesAgent (NousResearch)

**基础信息**

- GitHub: https://github.com/NousResearch/hermes-agent
- 语言：Python
- Stars：187,474
- 定位："The agent that grows with you"

#### 2.5.1 Hermetic 测试隔离模式

HermesAgent 的 `tests/conftest.py`（14KB）实现了业界最严格的测试隔离机制：

```python
@pytest.fixture(autouse=True)
def _hermetic_environment(tmp_path, monkeypatch):
    # 1. 清除所有凭证形态的环境变量
    for key in os.environ:
        if any(key.endswith(suffix) for suffix in ("_API_KEY", "_TOKEN", "_SECRET")):
            monkeypatch.delenv(key, raising=False)

    # 2. 隔离文件系统（HERMES_HOME → 临时目录）
    monkeypatch.setenv("HERMES_HOME", str(tmp_path / "hermes-home"))

    # 3. 确定性运行时
    monkeypatch.setenv("TZ", "UTC")
    monkeypatch.setenv("LANG", "C.UTF-8")
    monkeypatch.setenv("PYTHONHASHSEED", "0")

    # 4. 清除约 80 个 HERMES_* 行为变量
    for var in HERMES_BEHAVIOR_VARS:
        monkeypatch.delenv(var, raising=False)
```

**Live System Guard**（防止测试误操作生产环境）

```python
@pytest.fixture(autouse=True)
def _live_system_guard(monkeypatch):
    # 阻断危险 subprocess 操作
    original_popen = subprocess.Popen.__init__
    def safe_popen(self, cmd, *args, **kwargs):
        if "systemctl" in str(cmd) or "killall hermes" in str(cmd):
            raise RuntimeError("Blocked live system operation in test")
        return original_popen(self, cmd, *args, **kwargs)
    monkeypatch.setattr(subprocess.Popen, "__init__", safe_popen)
```

#### 2.5.2 批量评估 Pipeline

`batch_runner.py` 是 HermesAgent 的核心评估工具，用于 SWE Benchmark 等批量评估：

**指标体系**

```python
# 工具使用统计
tool_stats = {
    "tool_name": {
        "count": int,       # 调用次数
        "success": int,     # 成功次数
        "failure": int,     # 失败次数
        "success_rate": float
    }
}

# 推理覆盖率统计
reasoning_stats = {
    "total_assistant_turns": int,
    "turns_with_reasoning": int,    # 含 <REASONING_SCRATCHPAD> 的轮次
    "turns_without_reasoning": int,
    "has_any_reasoning": bool
}

# 运行元数据
run_metrics = {
    "duration": float,
    "total_prompts": int,
    "batch_count": int,
    "model": str,
    "timestamp": str
}
```

**SWE Mini Runner（`mini_swe_runner.py`）**

```python
class MiniSWERunner:
    """轻量级 SWE 任务执行器，输出 Hermes trajectory 格式"""

    def run_task(self, task):
        # 指标：api_calls, completed(bool), conversations(trajectory), metadata
        return {
            "api_calls": self.api_call_count,
            "completed": self.task_completed,
            "conversations": self.trajectory,
            "metadata": {"timestamp": ..., "model": ..., "environment": ...}
        }
```

**Trajectory 压缩分析（`trajectory_compressor.py`）**

```python
# 压缩质量指标
compression_metrics = {
    "compression_ratio": float,      # 压缩比
    "tokens_saved": int,             # 节省 Token 数
    "turns_removed": int,            # 移除轮次数
    "still_over_limit": bool,        # 是否仍超限
    "summarization_api_calls": int   # 摘要 API 调用数
}
```

#### 2.5.3 CI 分片策略

`tests.yml` 的测试执行设计：

```yaml
# 6 个并行 shard，基于历史时长的 LPT 算法分片
strategy:
  matrix:
    shard: [0, 1, 2, 3, 4, 5]

steps:
  - name: Restore test durations cache
    uses: actions/cache@v4
    with:
      key: test-durations-${{ github.sha }}
      restore-keys: test-durations-

  - name: Run tests
    run: python scripts/run_tests_parallel.py --shard ${{ matrix.shard }} --total 6

  - name: Merge duration data (main only)
    if: github.ref == 'refs/heads/main'
    run: python scripts/merge_durations.py
```

**安全隔离**：CI 中所有 API Key 显式置空：
```yaml
env:
  OPENROUTER_API_KEY: ""
  OPENAI_API_KEY: ""
  NOUS_API_KEY: ""
```

---

### 2.6 Claude Agent SDK (Anthropic)

**基础信息**

- 文档：https://code.claude.com/docs/en/agent-sdk/overview
- 语言：Python + TypeScript
- 定位：将 Claude Code 的完整工具执行能力封装为 SDK

#### 2.6.1 Hooks 系统 —— 可观测性接入点

Claude Agent SDK 的 Hooks 系统是其可观测性的核心机制。共支持 18 种 Hook 事件：

```typescript
// 完整 Hook 事件列表
type HookEvent =
  | "PreToolUse"          // 工具调用前（可 block/modify）
  | "PostToolUse"         // 工具执行后（可注入上下文）
  | "PostToolUseFailure"  // 工具失败（可捕获错误）
  | "PostToolBatch"       // 整批工具调用完成（TypeScript only）
  | "UserPromptSubmit"    // 用户提示词提交前
  | "MessageDisplay"      // 助手消息显示前（TypeScript only）
  | "Stop"                // Agent 执行停止
  | "SubagentStart"       // Sub-Agent 初始化
  | "SubagentStop"        // Sub-Agent 完成
  | "PreCompact"          // 上下文压缩前
  | "PermissionRequest"   // 权限请求
  | "SessionStart"        // 会话初始化（TypeScript only）
  | "SessionEnd"          // 会话结束（TypeScript only）
  | "Notification"        // Agent 状态消息
  | "Setup"               // 初始化（TypeScript only）
  | "TeammateIdle"        // 队员空闲（TypeScript only）
  | "TaskCompleted"       // 后台任务完成（TypeScript only）
  | "ConfigChange"        // 配置变更（TypeScript only）
```

**用于可观测性的典型 Hook 模式**

```python
# 1. 全链路审计 —— 每个工具调用写入审计日志
async def audit_all_tools(input_data, tool_use_id, context):
    await audit_db.insert({
        "session_id": input_data["session_id"],
        "tool": input_data["tool_name"],
        "input": input_data["tool_input"],  # 需开启 OTEL_LOG_TOOL_DETAILS=1
        "timestamp": datetime.now().isoformat()
    })
    return {"async_": True}  # 异步不阻塞 agent

# 2. Sub-Agent 追踪
async def track_subagents(input_data, tool_use_id, context):
    print(f"Subagent {input_data['agent_id']} completed")
    print(f"Transcript: {input_data['agent_transcript_path']}")
    return {}

# 3. 错误监控
async def monitor_tool_failures(input_data, tool_use_id, context):
    await alert_service.send({
        "tool": input_data["tool_name"],
        "error": input_data.get("error_message"),
        "session_id": input_data["session_id"]
    })
    return {}
```

#### 2.6.2 OpenTelemetry 原生集成

Claude Agent SDK 是唯一有官方文档的 OTEL 原生集成方案：

**三信号导出配置**

```bash
# 必须设置
CLAUDE_CODE_ENABLE_TELEMETRY=1
CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1  # Traces 需要

# 标准 OTEL 配置
OTEL_TRACES_EXPORTER=otlp
OTEL_METRICS_EXPORTER=otlp
OTEL_LOGS_EXPORTER=otlp
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318
OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer your-token

# 减少短生命周期进程的数据丢失
OTEL_METRIC_EXPORT_INTERVAL=1000
OTEL_TRACES_EXPORT_INTERVAL=1000
```

**Span 层次结构**

```
claude_code.interaction          # 单个 Agent turn
├── claude_code.llm_request      # LLM API 调用
├── claude_code.tool             # 工具执行
│   ├── claude_code.tool.blocked_on_user    # 等待用户审批
│   └── claude_code.tool.execution         # 实际执行
├── claude_code.hook             # Hook 执行（需 ENABLE_BETA_TRACING_DETAILED）
└── [subagent]                   # Sub-Agent 调用（嵌套）
    ├── claude_code.llm_request
    └── claude_code.tool
```

**W3C Trace Context 传播**：主进程调用 `query()` 时，如有活跃 OTel Span，自动注入 `TRACEPARENT`/`TRACESTATE` 到子进程环境，Agent 的 interaction span 成为主进程 span 的子 span。

**关键 Metrics**

从 `claude_code.*` metric 系列中提取：

| 指标类型 | 内容 |
|----------|------|
| Counters | tokens（input/output）、cost、sessions、lines of code、tool decisions |
| Histograms | llm_request latency（P50/P95/P99）、tool execution duration |
| Gauges | active sessions、context window utilization |

**接入平台**：官方文档明确支持 Honeycomb、Datadog、Grafana、**Langfuse**、自托管 collector。

**隐私控制**（默认不记录内容，按需开启）

```bash
OTEL_LOG_USER_PROMPTS=1     # 记录用户 prompt 文本
OTEL_LOG_TOOL_DETAILS=1     # 记录工具参数（文件路径、命令等）
OTEL_LOG_TOOL_CONTENT=1     # 记录工具完整输入输出（60KB 上限）
OTEL_LOG_RAW_API_BODIES=1   # 记录完整 API 请求/响应 JSON
```

#### 2.6.3 CI/CD 中使用 Agent SDK

官方 Hosting 文档推荐在 CI/CD 中将 SDK 作为无头（headless）执行方式：

```typescript
// CI 中一次性任务模式
const prompt = process.env.TASK_PROMPT!;
for await (const message of query({
    prompt,
    options: {
        maxTurns: 20,
        permissionMode: "bypassPermissions",  // CI 中全自动
        env: { ...process.env, ...otelEnv }
    }
})) {
    if (message.type === "result") {
        process.exit(message.subtype === "success" ? 0 : 1);
    }
}
```

---

## 3. 横向对比

### 3.1 评估体系对比

| 维度 | DeepAgents | OpenHarness | OpenCode | OpenClaw | HermesAgent | Claude SDK |
|------|:----------:|:-----------:|:--------:|:--------:|:-----------:|:---------:|
| Golden Dataset | 118 用例 | 6 复合任务 | 无 | QA 场景目录 | SWE Bench | 无内置 |
| LLM-as-Judge | ✅ openevals | 关键词匹配 | 无 | 手动评审 | 无 | Hooks 可实现 |
| 效率指标 | ✅ step/tool ratio | 工具调用计数 | 无 | 无 | tool 成功率 | 无 |
| 外部 Benchmark | ✅ FRAMES/BFCL/TAU2 | 无 | 无 | 无 | SWE Benchmark | 无 |
| 多轮 Trial 统计 | ✅ mean/std | 无 | 无 | 无 | 无 | 无 |
| 雷达图可视化 | ✅ matplotlib | 无 | 无 | 无 | 无 | 无 |

### 3.2 可观测性对比

| 维度 | DeepAgents | OpenHarness | OpenCode | OpenClaw | HermesAgent | Claude SDK |
|------|:----------:|:-----------:|:--------:|:--------:|:-----------:|:---------:|
| 链路 Tracing | LangSmith | Journal 事件流 | 无 | 事件总线 | 文件日志 | ✅ OTEL 原生 |
| Metrics 体系 | LangSmith | 状态计数 | 无 | token 用量 | tool 成功率 | ✅ OTEL Counter/Hist |
| Sub-Agent 追踪 | LangSmith | 团队状态 | 无 | agent_id 事件 | 无 | ✅ Span 嵌套 |
| W3C Trace Context | 无 | 无 | 无 | 无 | 无 | ✅ 自动传播 |
| 第三方平台接入 | LangSmith | 无 | 无 | 无 | 无 | Langfuse/DD/Grafana |
| 开箱即用程度 | 中（需 LangSmith 账号） | 低 | 低 | 中（需集成事件） | 低 | 高（环境变量启用） |

### 3.3 CI/CD 对比

| 维度 | DeepAgents | OpenHarness | OpenCode | OpenClaw | HermesAgent | Claude SDK |
|------|:----------:|:-----------:|:--------:|:--------:|:-----------:|:---------:|
| PR 触发评估 | ✅ baseline tier | 无 | 无 | 无 | 无 | 无内置 |
| Quality Gate | ✅ tier 分层 | 无 | 无 | advisory flag | pass/fail | exit code |
| Provider 矩阵 | ✅ 9 个 Provider | 无 | 无 | ✅ 9 个 Provider | 无 | 无 |
| 多轮 Trial 回归 | ✅ std_dev 告警 | 无 | 无 | 无 | 无 | 无 |
| 测试隔离 | LangSmith 强制 | 有（真实 API） | home isolation | provider 隔离 | ✅ hermetic | 无 |
| 分片负载均衡 | 无 | 无 | ✅ shard | ✅ shard | ✅ LPT 算法 | 无 |

### 3.4 设计路线对比

**"接入成熟生态" vs "自研"**

| 路线 | 代表框架 | 优势 | 劣势 |
|------|----------|------|------|
| 接入成熟生态（LangSmith） | DeepAgents | 开箱即用 UI；历史数据对比；多人协作 | 外部依赖；数据出境；月费 |
| 接入成熟生态（OpenTelemetry） | Claude Agent SDK | 标准化；接入任意 OTEL 兼容后端 | 需自建收集器；Traces 仍是 Beta |
| 自研可观测层 | OpenClaw（CoreAgentHarness） | 完全掌控；无外部依赖；深度定制 | 开发成本高；功能难追主流工具 |
| 混合（日志 + 基础指标） | HermesAgent | 成本低；简单 | 可视化差；跨 session 分析困难 |

---

## 4. 最佳实践总结

### 4.1 测试数据集设计 Best Practice

**1. 双层用例结构（来自 DeepAgents）**

```
baseline tier（回归门控，PR 必跑）
  ├── 工具选择准确性（直接/间接/多步）
  ├── 文件操作正确性（CRUD + 边界）
  ├── 基础记忆召回（当前 Session）
  └── 简单对话约束满足

hillclimb tier（进步追踪，定期跑）
  ├── 外部 Benchmark（FRAMES/BFCL/SWEBench）
  ├── 复合功能任务（多 Agent + 记忆 + Planning）
  └── 长尾场景（并发工具、上下文压缩边界）
```

**2. 用例污染防护（Test Leakage Prevention）**

- 每次 eval run 使用独立临时目录（隔离文件系统）
- CI 中清除所有 API Key 相关环境变量（hermetic 模式）
- 评估集与训练集严格隔离（勿在 README/文档中给出评估用例答案）
- Synthetic Trace 生成时使用不同的随机种子

**3. 评估数据聚合**

```python
# 单次 eval 结果（不可靠）→ N 轮 Trial 统计（可靠）
trials_summary = {
    "correctness": {"mean": 0.82, "std": 0.04, "min": 0.78, "max": 0.88},
    "per_category": {
        "tool_use": {"mean": 0.91, "std": 0.02},
        "memory":   {"mean": 0.73, "std": 0.08}  # 高方差 → 不稳定
    }
}
# 标准差 > 0.05 的维度视为"不稳定"，需专项关注
```

### 4.2 LLM-as-Judge 设计 Best Practice

**分离"确定性断言"与"非确定性评判"**

```python
# 确定性断言（硬失败，无需 LLM）：
assert "error" not in output_file
assert tool_calls.count("write_file") <= 3
assert final_text.startswith("# Report")

# 非确定性评判（LLM Judge）：
judge_criteria = [
    "The agent correctly identified the root cause of the bug",
    "The proposed solution is idiomatic Go code",
    "The explanation is clear and actionable"
]
# 每条独立评判，全部通过才算成功
```

**Judge Prompt 设计要点（来自 DeepAgents llm_judge.py）**

```python
JUDGE_PROMPT = """
You are a strict grading assistant. Score the agent response against ONE criterion only.

Criterion: {criterion}

Agent Response: {response}

Score 1 if the criterion is met, 0 if not.
Respond with ONLY a JSON object: {"score": 0 or 1, "comment": "brief explanation"}
"""
# 关键点：一次只评判一条 criterion；要求 JSON 输出（便于解析）；指明 strict
```

### 4.3 CI/CD 集成 Best Practice

**PR 评估 Pipeline 模板**

```yaml
# .github/workflows/eval.yml
on:
  pull_request:
    branches: [master]
    paths-ignore: ["docs/**", "*.md"]

jobs:
  baseline-eval:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Run baseline evals
        env:
          ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
          OPENAI_API_KEY: ""  # hermetic: 不使用外部 API
        run: go test -v -run TestBaseline ./evals/...

      - name: Check quality gate
        run: |
          # 读取评估报告，检查是否达到阈值
          SCORE=$(cat eval_report.json | jq '.correctness.mean')
          if (( $(echo "$SCORE < 0.80" | bc -l) )); then
            echo "Quality gate failed: correctness $SCORE < 0.80"
            exit 1
          fi
```

**回归检测逻辑**

```python
# 对比当前 PR 与 master 的评估结果
def detect_regression(current: dict, baseline: dict, threshold=0.05):
    regressions = []
    for category, score in current["per_category"].items():
        baseline_score = baseline["per_category"].get(category, {}).get("mean", 0)
        if baseline_score - score["mean"] > threshold:
            regressions.append({
                "category": category,
                "regression": baseline_score - score["mean"],
                "current": score["mean"],
                "baseline": baseline_score
            })
    return regressions
```

### 4.4 Tracing & Observability Best Practice

**最小可行 Tracing 方案（来自 Claude Agent SDK 设计）**

```
Layer 1: 结构化日志（立即可实施）
  - 每个 LLM 调用：model, latency_ms, input_tokens, output_tokens, tool_calls
  - 每个工具调用：name, args_hash(非完整参数), duration_ms, success/error
  - Session 级别：session_id, total_turns, total_tokens, compaction_count

Layer 2: OTEL Spans（中期目标）
  - claude_code.interaction → claude_code.llm_request → claude_code.tool
  - 资源属性：service.name, session.id, agent.type
  - 关联 Sub-Agent 调用（parent_span_id）

Layer 3: 第三方平台接入（长期目标）
  - OTLP Exporter → Langfuse / Grafana / Datadog
  - 或 LangSmith SDK 集成（Python/TS 友好）
```

**关键 Metrics（优先实施）**

```
1. p50/p95/p99 LLM 延迟（每个 Provider 分别统计）
2. 工具调用成功率（按工具名分组）
3. Token 消耗速率（每 Turn、每 Session）
4. Context 压缩触发频率
5. Agent 循环异常退出率（超时 / MaxTurns / 工具连续失败）
```

---

## 5. harness9 适配建议

基于调研结论，针对 harness9 的 Go 语言特性，给出分阶段的可行落地方案：

### 5.1 Phase 1：基础评估能力（1-2 周）

#### 5.1.1 构建 Golden Dataset

在 `internal/evals/` 目录下组织评估用例：

```
internal/evals/
├── dataset/
│   ├── tool_calling/      # 工具调用准确性用例
│   ├── planning/          # Planning 模块用例（TodoStore 状态机）
│   ├── memory/            # 短期记忆用例
│   ├── sub_agent/         # Sub-Agent 委派用例
│   └── context_mgmt/      # 上下文压缩用例
├── harness.go             # 评估框架核心（mock provider + assertion）
├── metrics.go             # 指标聚合
└── report.go              # 评估报告生成
```

**Mock Provider 设计**

```go
// internal/evals/harness.go
type EvalProvider struct {
    // 预设的确定性回复序列
    Responses []EvalResponse
    idx       int
    mu        sync.Mutex

    // 录制的实际调用
    Calls []ProviderCall
}

type EvalResponse struct {
    Text      string
    ToolCalls []schema.ToolCall
    // 可设置错误，测试 agent 的 self-healing
    Error     error
}

func (p *EvalProvider) Generate(ctx context.Context, msgs []schema.Message, tools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
    p.mu.Lock()
    defer p.mu.Unlock()
    // 记录调用
    p.Calls = append(p.Calls, ProviderCall{Messages: msgs, Tools: tools})
    // 返回预设回复
    if p.idx >= len(p.Responses) {
        return &schema.Message{Role: schema.RoleAssistant, Content: "Done."}, nil, nil
    }
    resp := p.Responses[p.idx]
    p.idx++
    return &schema.Message{
        Role:      schema.RoleAssistant,
        Content:   resp.Text,
        ToolCalls: resp.ToolCalls,
    }, nil, resp.Error
}
```

**Assertion 框架**

```go
// internal/evals/assertions.go
type Assertion interface {
    Check(result *EvalResult) error
    Name() string
}

// 工具调用断言
type ToolCalledAssertion struct {
    ToolName string
    MinTimes int
    MaxTimes int
}

// 最终输出断言
type OutputContainsAssertion struct {
    Expected string
}

// 效率断言（记录但不 fail）
type MaxTurnsAssertion struct {
    MaxTurns int
    Warn     bool // true=warn only
}
```

#### 5.1.2 Hermetic CI 测试

仿 HermesAgent 模式，在 `go test` 中强制隔离：

```go
// internal/evals/testenv.go
func SetupHermeticEnv(t *testing.T) {
    t.Helper()
    // 清除 API Key 相关环境变量
    for _, key := range os.Environ() {
        if strings.HasSuffix(key, "_API_KEY") || strings.HasSuffix(key, "_TOKEN") {
            name := strings.SplitN(key, "=", 2)[0]
            t.Setenv(name, "")
        }
    }
    // 隔离工作目录
    tmpDir := t.TempDir()
    t.Setenv("HARNESS9_WORK_DIR", tmpDir)
}
```

### 5.2 Phase 2：结构化 Tracing（2-4 周）

#### 5.2.1 OpenTelemetry 接入方案

在 harness9 中接入 OTEL，参考 Claude Agent SDK 的 Span 层次设计：

```go
// internal/observability/tracer.go
package observability

import (
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/trace"
)

const (
    SpanNameInteraction   = "harness9.interaction"   // 一个完整 Agent 运行
    SpanNameLLMRequest    = "harness9.llm_request"   // 单次 LLM 调用
    SpanNameToolExecution = "harness9.tool"           // 工具执行
    SpanNameSubAgent      = "harness9.subagent"       // Sub-Agent 调用
    SpanNameCompaction    = "harness9.compaction"     // 上下文压缩
)

// Span 属性键
const (
    AttrSessionID     = "session.id"
    AttrModel         = "llm.model"
    AttrInputTokens   = "llm.tokens.input"
    AttrOutputTokens  = "llm.tokens.output"
    AttrToolName      = "tool.name"
    AttrToolSuccess   = "tool.success"
    AttrTurnNumber    = "agent.turn"
    AttrAgentType     = "agent.type"     // "main" | "sub"
)
```

**在 engine/agent_loop.go 中埋点**

```go
// 在 runLoop 函数中添加 Span
func (e *AgentEngine) runLoop(ctx context.Context, initialPrompt string, ...) error {
    ctx, span := otel.Tracer("harness9").Start(ctx, SpanNameInteraction,
        trace.WithAttributes(
            attribute.String(AttrSessionID, e.session.ID()),
            attribute.String(AttrModel, e.provider.ModelName()),
        ))
    defer span.End()

    for turn := 0; ; turn++ {
        // LLM 调用 Span
        llmCtx, llmSpan := otel.Tracer("harness9").Start(ctx, SpanNameLLMRequest)
        msg, usage, err := e.provider.Generate(llmCtx, messages, tools)
        if err != nil {
            llmSpan.RecordError(err)
        }
        if usage != nil {
            llmSpan.SetAttributes(
                attribute.Int64(AttrInputTokens, int64(usage.InputTokens)),
                attribute.Int64(AttrOutputTokens, int64(usage.OutputTokens)),
            )
        }
        llmSpan.End()
        // ...
    }
}
```

#### 5.2.2 关键 Metrics 定义

```go
// internal/observability/metrics.go
var (
    // LLM 请求延迟直方图
    LLMRequestDuration = otel.Meter("harness9").Float64Histogram(
        "harness9.llm.request.duration",
        metric.WithUnit("s"),
        metric.WithDescription("LLM API request duration"),
    )

    // Token 消耗计数器
    TokensUsed = otel.Meter("harness9").Int64Counter(
        "harness9.llm.tokens.total",
        metric.WithDescription("Total tokens consumed"),
    )

    // 工具调用成功/失败计数
    ToolCallOutcome = otel.Meter("harness9").Int64Counter(
        "harness9.tool.calls",
        metric.WithDescription("Tool call outcomes by name and status"),
    )

    // Agent 循环轮次
    AgentTurns = otel.Meter("harness9").Int64Histogram(
        "harness9.agent.turns",
        metric.WithDescription("Number of turns per agent run"),
    )
)
```

### 5.3 Phase 3：LLM-as-Judge + CI 质量门控（4-6 周）

#### 5.3.1 LLM Judge 模块

```go
// internal/evals/judge.go
type LLMJudge struct {
    provider provider.LLMProvider
    model    string
}

type JudgeCriterion struct {
    Name        string
    Description string
}

type JudgeResult struct {
    Criterion string
    Score     int     // 0 or 1
    Comment   string
    Passed    bool
}

func (j *LLMJudge) EvaluateTrajectory(
    ctx context.Context,
    trajectory *AgentTrajectory,
    criteria []JudgeCriterion,
) ([]JudgeResult, error) {
    results := make([]JudgeResult, len(criteria))
    // 并发评判每个 criterion
    var wg sync.WaitGroup
    for i, c := range criteria {
        wg.Add(1)
        go func(idx int, criterion JudgeCriterion) {
            defer wg.Done()
            results[idx] = j.judgeOne(ctx, trajectory, criterion)
        }(i, c)
    }
    wg.Wait()
    return results, nil
}
```

#### 5.3.2 GitHub Actions Quality Gate

```yaml
# .github/workflows/eval.yml
name: Agent Eval
on:
  pull_request:
    branches: [master]

jobs:
  baseline-eval:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Setup Go
        uses: actions/setup-go@v5
        with:
          go-version-file: go.mod

      - name: Run baseline evaluations
        env:
          ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
          HARNESS9_EVAL_MODE: "baseline"
        run: |
          go test -v -timeout 10m -run TestEval ./internal/evals/... \
            -eval-report eval_report.json

      - name: Quality gate check
        run: |
          go run ./internal/evals/cmd/gate/main.go \
            --report eval_report.json \
            --min-correctness 0.80 \
            --max-regression 0.05 \
            --baseline-file .eval_baseline.json

      - name: Upload eval report
        if: always()
        uses: actions/upload-artifact@v4
        with:
          name: eval-report-${{ github.sha }}
          path: eval_report.json
```

### 5.4 完整架构蓝图

```
harness9 Test & Eval & Observability 架构

┌─────────────────────────────────────────────────────────────────┐
│                     开发时（本地）                                │
│  go test ./internal/evals/ -run TestBaseline                     │
│  ├── MockProvider（确定性回复序列）                               │
│  ├── HermeticEnv（隔离 API Key + 工作目录）                       │
│  ├── Assertion Framework（工具调用/输出内容/效率指标）             │
│  └── LLM Judge（对非确定性输出评分，可选）                        │
└─────────────────────────────────────────────────────────────────┘
                         │ PR
┌─────────────────────────────────────────────────────────────────┐
│                    CI/CD（GitHub Actions）                        │
│  eval.yml                                                        │
│  ├── baseline tier（必跑，<5min，全 MockProvider）                │
│  ├── Quality Gate（correctness >= 0.80，regression < 0.05）      │
│  └── hillclimb tier（定期跑，真实 API，带 LLM Judge）             │
└─────────────────────────────────────────────────────────────────┘
                         │ 生产运行
┌─────────────────────────────────────────────────────────────────┐
│                   Observability（运行时）                         │
│  Phase 1: 结构化日志（logfmt 已有基础）                           │
│  ├── 每次 LLM 调用：model/latency/tokens/tool_calls              │
│  └── 每次工具调用：name/duration/success                          │
│                                                                   │
│  Phase 2: OpenTelemetry                                           │
│  ├── harness9.interaction（单次 Agent 运行根 Span）               │
│  ├── harness9.llm_request（嵌套 Span）                            │
│  ├── harness9.tool（嵌套 Span）                                   │
│  └── harness9.subagent（嵌套 Span，Sub-Agent 调用链）             │
│                                                                   │
│  Phase 3: 平台接入                                                │
│  ├── OTLP → Langfuse（LLM 专用，成本低）                          │
│  └── OTLP → Grafana / Datadog（通用 APM）                         │
└─────────────────────────────────────────────────────────────────┘
```

### 5.5 优先级总结

| 优先级 | 任务 | 预期工时 | 关键价值 |
|--------|------|----------|----------|
| **P0** | 构建 MockProvider + 基础 Assertion 框架 | 3 天 | 确定性测试基础 |
| **P0** | 为 Planning/Tool Calling 核心能力建立 Golden Dataset | 2 天 | 回归检测基础 |
| **P0** | GitHub Actions baseline eval + Quality Gate | 1 天 | CI 门控 |
| **P1** | OpenTelemetry 接入（engine/provider 层埋点） | 1 周 | 生产可观测性 |
| **P1** | 结构化 Metrics 输出（Token/Latency/Tool Success Rate） | 3 天 | 性能监控 |
| **P2** | LLM-as-Judge（对 Planning/Sub-Agent 质量评分） | 1 周 | 非确定性评估 |
| **P2** | 多轮 Trial CI Pipeline（均值/标准差统计） | 2 天 | 评估可靠性 |
| **P3** | Langfuse / Grafana 平台接入 | 1 周 | 可视化 |

---

## 6. 参考资料

以下资料经过 WebFetch 确认可正常访问且与调研主题相关：

| 标题 | 来源 | URL | 摘要 |
|------|------|-----|------|
| DeepAgents Eval Catalog | GitHub | https://github.com/langchain-ai/deepagents/blob/main/libs/evals/EVAL_CATALOG.md | 118 个评估用例目录，覆盖 8 个能力维度 |
| Claude Agent SDK Observability | Anthropic 官方文档 | https://code.claude.com/docs/en/agent-sdk/observability | 原生 OTEL 三信号导出完整配置指南 |
| Claude Agent SDK Hooks | Anthropic 官方文档 | https://code.claude.com/docs/en/agent-sdk/hooks | 18 种 Hook 事件详细说明，含可观测性最佳实践 |
| Claude Agent SDK Hosting | Anthropic 官方文档 | https://code.claude.com/docs/en/agent-sdk/hosting | CI/CD 集成模式，Ephemeral/Long-running/Hybrid session 说明 |
| OpenHarness tests/test_real_large_tasks.py | GitHub | https://raw.githubusercontent.com/HKUDS/OpenHarness/main/tests/test_real_large_tasks.py | 6 个复合任务真实评估实现 |
| HermesAgent tests/conftest.py | GitHub | https://raw.githubusercontent.com/NousResearch/hermes-agent/main/tests/conftest.py | Hermetic 测试隔离模式的权威实现 |
| OpenClaw agent-harness.ts | GitHub | https://github.com/openclaw/openclaw/blob/main/packages/agent-core/src/harness/agent-harness.ts | 自研 Agent 可观测事件总线 39KB 完整实现 |
| DeepAgents evals CI workflow | GitHub | https://raw.githubusercontent.com/langchain-ai/deepagents/main/.github/workflows/evals.yml | Provider 矩阵并行评估 + Radar Chart 自动发布 |
| DeepAgents evals_trials CI workflow | GitHub | https://raw.githubusercontent.com/langchain-ai/deepagents/main/.github/workflows/evals_trials.yml | 多轮 Trial 统计 + 回归检测 CI 设计 |
