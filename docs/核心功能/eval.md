# 测试 · 评估 · 可观测体系

harness9 的质量保障体系由三个相互独立但协同工作的子系统构成，共同回答一个核心问题：**这个 Agent 是否真的在正确地工作？**

```
开发阶段 ──→ Test（确定性测试）      ScriptedProvider + Assertion
CI 阶段  ──→ Eval（黄金数据集评估）  26 个用例 + Quality Gate
生产阶段 ──→ Observability（追踪）  OTEL Traces + Metrics → Langfuse
```

---

## 一、设计哲学

### 1.1 为什么 Agent 需要专门的测试体系？

传统软件的单元测试假设：给定相同输入，总得到相同输出。Agent 系统打破了这一假设——LLM 的输出天然非确定性，同一 prompt 在不同会话下可能产生截然不同的行为路径。

| 挑战 | 传统测试的失效原因 | harness9 的解法 |
|------|-----------------|----------------|
| **非确定性** | Mock 不了真实 LLM 行为 | `ScriptedProvider` 将行为脚本化，使测试确定可重复 |
| **行为验证** | 断言返回值不够，要验证「做了什么」 | `recordingHook` + `Assertion` 框架验证工具调用轨迹 |
| **性能退化** | 没有 baseline 就发现不了退化 | 黄金数据集 + CI Quality Gate，每次 PR 自动对比 |
| **生产可见性** | 没有工具看清 Agent 在做什么 | OTEL 链路追踪，每次 LLM 调用和工具执行都可视化 |

### 1.2 三层金字塔模型

```
          ┌─────────────────────────────┐
          │   Observability（可观测）    │  ← 生产环境：OTEL Traces + Metrics
          │   看清 Agent 在做什么        │    接入 Langfuse / Grafana / Jaeger
          └──────────────┬──────────────┘
                         │
          ┌──────────────▼──────────────┐
          │   Eval（评估）               │  ← CI/CD：黄金数据集 Quality Gate
           │   量化 Agent 能力边界        │    26 个用例，PR 触发，失败阻断合并
          └──────────────┬──────────────┘
                         │
          ┌──────────────▼──────────────┐
          │   Test（测试）               │  ← 开发阶段：ScriptedProvider + Assertion
          │   验证 Agent 行为的正确性    │    确定性、Hermetic 隔离、无 API Key 依赖
          └─────────────────────────────┘
```

### 1.3 非侵入设计原则

harness9 的核心引擎（engine / provider / hooks）不感知任何测试或可观测逻辑。所有能力通过三个已有扩展点无缝接入：

```
┌─────────────────────────────────────────────────────────────┐
│                     AgentEngine（核心引擎）                   │
│  EngineObserver ← 唯一新增接口（4 处生命周期回调）  [接入点 1] │
└──────────────────────────────────────────────────────────────┘
         ↑ WithEngineObserver 注入
         │
┌────────┴──────────┐   ┌──────────────────────────────────────┐
│ OTELEngineObserver│   │ TracingProvider [接入点 2]             │
│ Interaction Span  │   │ 包装 LLMProvider，LLM Request Span     │
│ Turn Span         │   │ + Token Metrics + Input/Output 上报   │
└───────────────────┘   └──────────────────────────────────────┘
                                      ↑ 替换原始 provider
┌──────────────────────────────────────────────────────────────┐
│ ObservabilityHook [接入点 3]                                   │
│ 实现 ToolHook，Tool Execution Span + Tool Metrics              │
│ 注册到 HookRegistry 末端（纯观测，不干预工具决策）              │
└──────────────────────────────────────────────────────────────┘
```

---

## 二、Test 子系统

### 2.1 架构总览

```
internal/evals/
├── provider.go       ScriptedProvider — 确定性 LLM mock
├── assertions.go     Assertion 接口 + Case/Result 类型 + 8 种断言
├── harness.go        RunCase / Suite / recordingHook
├── testenv.go        SetupHermeticEnv — 标准 Hermetic 隔离
├── report.go         SuiteReport / BuildReport / WriteJSON / WriteMarkdown
└── dataset/
    ├── tool_calling_test.go    工具调用准确性（4 用例）
    ├── planning_test.go        Planning 完成率（4 用例）
    ├── context_test.go         Context Engineering 连贯性（3 用例）
    ├── error_handling_test.go  Error Handling / Self-Healing（3 用例）
    └── memory_test.go          Memory 持久化（2 用例）
```

### 2.2 ScriptedProvider：把 LLM 行为脚本化

`ScriptedProvider` 是 eval 框架的基石，实现 `provider.LLMProvider` 接口，按预设的 `ScriptedTurn` 序列返回确定性回复，不发起任何网络请求。

```go
// 脚本：第一轮发起 bash 工具调用，第二轮返回文本结论
p := evals.NewScriptedProvider(
    evals.ScriptedTurn{
        ToolCalls: []schema.ToolCall{
            evals.MakeToolCall("tc1", "bash", `{"command":"ls -la"}`),
        },
    },
    evals.ScriptedTurn{Text: "目录中有 3 个文件。"},
)
```

| 机制 | 说明 |
|------|------|
| **Turn 序列** | 每次 `Generate` 消费一个 `ScriptedTurn`，耗尽后返回默认终止回复 |
| **录制调用** | 所有 LLM 调用都被记录到 `calls []RecordedCall`，供 Assertion 验证 |
| **Err 注入** | `ScriptedTurn{Err: err}` 模拟 LLM API 失败，测试引擎自愈能力 |
| **线程安全** | 内部互斥锁，goroutine 并发调用无竞争 |

### 2.3 Assertion 框架

断言分为 **Hard**（失败则 Case 不通过）和 **Soft**（仅记警告）两类：

```
Assertion
├── Hard Assertions（失败 → Passed=false）
│   ├── ToolCalledAssertion{ToolName, MinTimes}   工具被调用 >= N 次
│   ├── ToolNotCalledAssertion{ToolName}           工具一次都没被调用
│   ├── OutputContainsAssertion{Expected}          最终输出包含期望字符串
│   ├── OutputExcludesAssertion{Forbidden}         最终输出不含禁止字符串
│   ├── NoErrorAssertion{}                         RunError == nil
│   └── ErrorAssertion{}                           RunError != nil（测试错误路径）
└── Soft Assertions（失败 → Warnings，不影响 Passed）
    ├── MaxTurnsAssertion{Max}                     Turn 数 <= Max（效率告警）
    └── MaxToolCallsAssertion{Max}                 工具调用次数 <= Max（效率告警）
```

`recordingHook` 在 `HookRegistry.BeforeExecute` 阶段记录工具名——在 registry 查找**之前**触发，无论工具是否注册都能正确捕获 LLM 的调用意图。

### 2.4 EvalHarness：最小化引擎环境

`RunCase` 为每个 Case 构建完全隔离的最小化 `AgentEngine`：

```
RunCase(c *Case) Result
    │
    ├── 确定工作目录（c.WorkDir 或自动创建临时目录，defer 清理）
    ├── 注册四个基础工具（read_file / write_file / bash / edit_file）
    ├── 挂载 recordingHook（记录工具名，位于 HookRegistry 最前端）
    ├── engine.NewAgentEngine(c.Provider, hookReg, workDir, WithMaxTurns(c.MaxTurns))
    │       ← ScriptedProvider + 无 Session + 无 Compactor（保证确定性）
    ├── eng.Run(ctx, c.Prompt)
    └── 逐一执行 c.Assertions → 聚合 Failures / Warnings → Result.Passed
```

**不使用 Session 和 Compactor** 是关键决策——排除持久化和压缩带来的非确定性，保证相同脚本总产生相同结果。

### 2.5 Hermetic 测试隔离

```go
func TestMyFeature(t *testing.T) {
    evals.SetupHermeticEnv(t)  // 必须首行调用

    c := &evals.Case{
        ID:       "feature/basic",
        Category: "feature",
        Prompt:   "运行 ls 命令",
        Provider: evals.NewScriptedProvider(
            evals.ScriptedTurn{
                ToolCalls: []schema.ToolCall{
                    evals.MakeToolCall("tc1", "bash", `{"command":"ls"}`),
                },
            },
            evals.ScriptedTurn{Text: "命令已执行。"},
        ),
        Assertions: []evals.Assertion{
            &evals.ToolCalledAssertion{ToolName: "bash"},
            &evals.NoErrorAssertion{},
            &evals.MaxTurnsAssertion{Max: 3}, // soft：效率告警
        },
    }

    result := evals.RunCase(context.Background(), c)
    if !result.Passed {
        for _, f := range result.Failures {
            t.Errorf("❌ %s", f.Error())
        }
    }
    for _, w := range result.Warnings {
        t.Logf("⚠️ %s", w.Error())
    }
}
```

`SetupHermeticEnv` 清除所有 `_API_KEY`、`_TOKEN`、`_SECRET` 后缀的环境变量，防止 eval 测试因环境中存在真实 API Key 而意外调用付费服务，保证本地与 CI 环境行为完全一致。

---

## 三、Eval 子系统：黄金数据集

### 3.1 当前黄金数据集（26 个用例）

| 类别 | 用例 | 验证目标 |
|------|------|---------|
| `tool_calling` | `bash_basic` | bash 工具被正确调用 |
| `tool_calling` | `read_file` | read_file 工具被正确调用 |
| `tool_calling` | `write_then_read` | 多工具顺序调用（write → read） |
| `tool_calling` | `edit_file_fuzzy_indent` | 缩进不一致时 edit_file L4 模糊匹配端到端不破坏循环 |
| `tool_calling` | `parallel_tools` | 同一 Turn 并行调用多工具，结果均被记录（`-race` 下兼为并发竞争回归） |
| `tool_calling` | `no_tool_conversation` | 纯对话不触发工具调用 |
| `planning` | `plan_generated` | todo_write 写入计划 |
| `planning` | `no_write_in_plan_mode` | 规划阶段不调用 write_file/edit_file |
| `planning` | `plan_then_execute` | 先生成计划再执行（完整 Planning 链路） |
| `planning` | `exploration_only` | 纯探索模式只用只读工具 |
| `context` | `sequential_tool_chain` | 多步工具调用依赖上一步 Observation |
| `context` | `multi_turn_conversation` | 多轮纯对话连贯性 |
| `context` | `tool_error_observation` | 工具失败 Observation 驱动 LLM 改变策略 |
| `context` | `stall_nudge_preserves_loop` | WithStallReminder 注入停滞提示不破坏 ReAct 循环正确性 |
| `error_handling` | `bash_fallback_on_error` | 工具失败后 LLM 切换替代方案（Self-Healing） |
| `error_handling` | `write_failure_graceful_stop` | 写入失败后优雅降级不重试 |
| `error_handling` | `max_turns_protection` | MaxTurns 触发引擎受控终止（不 panic） |
| `memory` | `write_memory` | memory_write 工具被调用 |
| `memory` | `search_memory` | memory_search 工具被调用 |
| `compaction` | `anchor_preservation` | 压缩后 LLM 仍能回答关于用户意图的问题（锚点保留） |
| `compaction` | `offload_retrieval` | 压缩后 LLM 可通过工具检索 offloaded 内容 |
| `compaction` | `progressive_tiers` | 大量对话压缩后 LLM 行为仍连贯（渐进式分层） |
| `guardrails` | `repetition_terminates` | 同签名重复调用死循环：先注入定向提醒、无效后升级硬终止 |
| `guardrails` | `budget_trips_hermetically` | Token 预算耗尽在 Turn 边界受控终止（Hermetic，无真实 API） |
| `guardrails` | `reminder_visible_in_llm_context` | 定向提醒出现在发给 LLM 的历史副本中，且首轮不打扰 |
| `guardrails` | `history_survives_breaker` | 受控熔断后完整轨迹仍持久化到 Session（丢失轨迹缺陷回归护栏） |

### 3.2 运行 Eval

```bash
# 运行全量黄金数据集（26 个用例，无需 API Key）
go test ./internal/evals/... ./internal/evals/dataset/... -v

# 只运行特定类别
go test ./internal/evals/dataset/... -v -run TestToolCalling
go test ./internal/evals/dataset/... -v -run TestPlanning
go test ./internal/evals/dataset/... -v -run TestContextEngineering
go test ./internal/evals/dataset/... -v -run TestErrorHandling
go test ./internal/evals/dataset/... -v -run TestMemory
go test ./internal/evals/dataset/... -v -run TestCompaction
go test ./internal/evals/dataset/... -v -run TestGuardrails

# 生成 JSON + Markdown 报告
results := suite.Run(ctx)
report  := evals.BuildReport(results)
evals.WriteJSON(report, "eval-report.json")
evals.WriteMarkdown(report, "eval-report.md")
```

### 3.3 新增 Eval 用例的规范

Feature 开发完成后，**必须**在 `internal/evals/dataset/` 下新增对应的黄金用例（参见 [AGENTS.md §5.8 测试与评估规范](https://github.com/ZhangShenao/harness9/blob/master/AGENTS.md)）：

- 每个功能至少覆盖**正向用例**（功能正常工作）和**反向用例**（约束被正确执行）
- `SetupHermeticEnv` 首行调用，`NoErrorAssertion` 或 `ErrorAssertion` 必选
- 扩展数据集只需新增 `_test.go` 文件，无需修改框架代码
- 当前 26 个用例是 baseline，只能增加，不能删除或降低覆盖率

---

## 四、CI/CD 质量门控

### 4.1 流水线设计

```
PR 触发（push to master / pull_request）
       │
       ▼
  unit-tests job
  └── go test ./...  ← 全量单元测试（含 observability + evals）
       │
       ▼ needs: unit-tests
  eval job（Quality Gate）
  ├── 环境：OPENAI_API_KEY=""  ANTHROPIC_API_KEY=""  HARNESS9_EVAL_HERMETIC=1
  │          OTEL_ENABLED=false（CI 中关闭 OTEL 上报）
  ├── go test ./internal/evals/... ./internal/evals/dataset/... -v
  ├── 结果上传为 Artifact（保留 30 天）
  └── 摘要写入 GitHub Step Summary
```

**Quality Gate**：`continue-on-error: false`——eval 失败则 CI 失败，PR 无法合并。

**Hermetic 保障**：
1. 不产生任何真实 LLM API 费用
2. 测试结果完全确定，无随机波动
3. 任何行为退化都来自代码变更，而非 LLM 版本更新

---

## 五、Observability 子系统

### 5.1 Span 层次结构

harness9 的每一次 Agent 运行产生一棵完整的 Span 树：

```
harness9.interaction   [session.id="abc123",  langfuse.trace.input="用户 prompt"]
│   duration: 12.4s
│
├── harness9.turn   [agent.turn=1]
│   │   duration: 3.2s
│   │
│   ├── harness9.llm_request   [gen_ai.request.model="anthropic/claude-sonnet-4.6"]
│   │       langfuse.observation.input  = [{"role":"system",...},{"role":"user",...}]
│   │       langfuse.observation.output = "LLM 回复文本或工具调用 JSON"
│   │       gen_ai.usage.input_tokens=4821, gen_ai.usage.output_tokens=312
│   │       duration: 2.1s
│   │
│   ├── harness9.tool   [tool.name="bash", tool.success=true]
│   │       langfuse.observation.input  = {"command":"ls -la"}
│   │       langfuse.observation.output = "total 24\n..."
│   │       duration: 0.8s
│   │
│   └── harness9.tool   [tool.name="read_file", tool.success=true]
│           duration: 0.1s
│
└── harness9.turn   [agent.turn=2, turn.has_tool_calls=false]
    └── harness9.llm_request   [...]
            duration: 2.6s
```

### 5.2 三组件实现原理

#### OTELEngineObserver — Interaction + Turn Span

`runLoop` 在 4 个生命周期点回调 `EngineObserver`：

```
runLoop 入口 → OnInteractionStart(ctx, sessionID, prompt)
               返回携带 interaction Span 的增强 ctx

for 每个 Turn:
  → OnTurnStart(ctx, turn)    返回携带 turn Span 的 turnCtx
    em.generate(turnCtx, ...)   ← LLM 调用继承 turn Span
    e.executeTools(turnCtx, ...) ← 工具执行继承 turn Span
  → OnTurnEnd(turnCtx, turn, hasToolCalls)

runLoop 退出（defer 保证）
  → OnInteractionEnd(ctx, turns, err)
    → span.End() + ForceFlush()   ← 立即推送到后端
```

**关键设计**：`OnInteractionStart` 和 `OnTurnStart` 双写 Span（OTEL 标准 slot + 自定义 key），确保在中间层代码（compaction、session 加载）可能替换 ctx 时，父子关系链路不会断开。

#### TracingProvider — LLM Request Span + Token Metrics

```go
func (p *TracingProvider) GenerateStream(ctx context.Context, ...) {
    ctx, span := p.tracer.Start(ctx, SpanLLMRequest)  // ctx 含 turn Span，自动嵌套
    span.SetAttributes(attribute.String(AttrLangfuseObsInput, serializeMessages(messages)))

    ch, _ := p.inner.GenerateStream(ctx, ...)
    go func() {
        defer span.End()
        // 等待 StreamChunkDone，提取 Usage 和最终回复
        span.SetAttributes(attribute.String(AttrLangfuseObsOutput, serializeOutput(lastMsg)))
        p.recordMetrics(ctx, span, lastUsage, elapsed, nil)
    }()
}
```

#### ObservabilityHook — Tool Execution Span

```go
func (h *ObservabilityHook) BeforeExecute(ctx context.Context, tc schema.ToolCall) (...) {
    var span trace.Span
    ctx, span = h.tracer.Start(ctx, SpanToolExecution, ...)
    span.SetAttributes(attribute.String(AttrLangfuseObsInput, truncateAttr(string(tc.Arguments))))
    return ctx, hooks.Allow(), nil  // 始终放行，不干预工具决策
}

func (h *ObservabilityHook) AfterExecute(ctx context.Context, tc schema.ToolCall, result schema.ToolResult) schema.ToolResult {
    span := trace.SpanFromContext(ctx)
    span.SetAttributes(attribute.String(AttrLangfuseObsOutput, truncateAttr(result.Output)))
    span.End()
    h.toolDuration.Record(...)   // Histogram
    h.toolCallsTotal.Add(...)    // Counter，by name + status
    return result                // 透传，不修改
}
```

### 5.3 Metrics 体系

| 指标名 | 类型 | 说明 |
|--------|------|------|
| `harness9.llm.request.duration` | Histogram | LLM API 请求延迟（秒）|
| `harness9.llm.tokens.input` | Counter | 累计输入 Token |
| `harness9.llm.tokens.output` | Counter | 累计输出 Token |
| `harness9.tool.calls.total` | Counter | 工具调用次数（by name + status）|
| `harness9.tool.execution.duration` | Histogram | 工具执行耗时 |
| `harness9.agent.turns.total` | Counter | Agent Turn 总数 |

### 5.4 OTEL SDK 初始化与配置

```
Setup(ctx, cfg)
├── cfg.Enabled=false 或 ExporterNoop  → 零开销 noopProviders()
├── ExporterStdout                     → stdouttrace（本地调试，写 stderr）
└── ExporterOTLP
        ├── 显式拼接 /v1/traces（不依赖 SDK 自动追加，版本间行为差异）
        ├── 显式传 WithHeaders（不依赖 SDK 读 env var，保证可靠性）
        ├── https:// → TLS，http:// → 不加密
        └── 全局 OTEL error handler 写 stderr（绕过 TUI io.Discard）
```

| 环境变量 | 默认值 | 说明 |
|---------|--------|------|
| `OTEL_ENABLED` | `false` | `true` 启用 |
| `OTEL_SERVICE_NAME` | `harness9` | 服务名 |
| `OTEL_EXPORTER_TYPE` | `noop` | `noop` / `stdout` / `otlp` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | base URL，如 `https://us.cloud.langfuse.com/api/public/otel` |
| `OTEL_EXPORTER_OTLP_HEADERS` | — | `key=val,key2=val2`，用于 Langfuse Authorization header |

---

## 六、接入观测平台

### 6.1 接入 Langfuse（推荐）

Langfuse 是专为 LLM 应用设计的可观测平台，原生支持 OpenTelemetry，提供 Trace 可视化、Token 费用分析、会话回放。

#### 步骤一：注册账号并获取 API Key

1. 前往 [cloud.langfuse.com](https://cloud.langfuse.com/auth/sign-up) 注册
2. 进入项目 → **Settings → API Keys** → 点击 **Create new API key**
3. 复制 **Public Key**（`pk-lf-...`）和 **Secret Key**（`sk-lf-...`，仅显示一次）

#### 步骤二：生成 Base64 认证字符串

```bash
# macOS / Linux
AUTH=$(echo -n "pk-lf-YOUR_PUBLIC_KEY:sk-lf-YOUR_SECRET_KEY" | base64)

# GNU/Linux（较长 key 防折行）
AUTH=$(echo -n "pk-lf-YOUR_PUBLIC_KEY:sk-lf-YOUR_SECRET_KEY" | base64 -w 0)
```

#### 步骤三：配置并启动

写入 `.env`（已在 `.gitignore`，不会进入 Git 仓库）：

```bash
OTEL_ENABLED=true
OTEL_EXPORTER_TYPE=otlp

# 选择所在区域
OTEL_EXPORTER_OTLP_ENDPOINT=https://us.cloud.langfuse.com/api/public/otel   # US 区域
# OTEL_EXPORTER_OTLP_ENDPOINT=https://cloud.langfuse.com/api/public/otel    # EU 区域
# OTEL_EXPORTER_OTLP_ENDPOINT=https://jp.cloud.langfuse.com/api/public/otel # JP 区域

OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <YOUR_BASE64_AUTH>,x-langfuse-ingestion-version=4
```

然后运行 `./harness9`，对话产生工具调用后，Langfuse **Traces** 标签即可看到如下结构：

```
Trace: harness9.interaction
│   Input  = "用户 prompt"
│
└── Generation: harness9.llm_request
│       Input  = [{"role":"system",...},{"role":"user",...}]
│       Output = "LLM 回复"
│       25,269 prompt → 1,027 completion  ← 自动费用估算
│       anthropic/claude-sonnet-4.6
└── Span: harness9.tool  (bash)
        Input  = {"command":"ls -la"}
        Output = "total 24\n..."
```

#### 常见问题排查

| 症状 | 原因 | 解决方案 |
|------|------|---------|
| 控制台无数据 | `AUTH` 编码含换行符 | 用 `echo $AUTH` 确认输出，或加 `-w 0` |
| `401 Unauthorized` | Key 顺序错误 | 必须是 `pk-lf-...:sk-lf-...`（Public Key 在前） |
| Trace 出现但有延迟 | 缺少 ingestion-version header | 在 headers 中加 `x-langfuse-ingestion-version=4` |
| 区域连不上 | endpoint 选错 | 确认账号注册区域（EU/US/JP），切换对应 endpoint |
| Input/Output 显示 null | 旧版 `langfuse.input/output` 属性名 | 已修复（v4 使用 `langfuse.trace.*` / `langfuse.observation.*`）|
| trace 导出失败（no data） | 工具输出含非法 UTF-8 字节 | 已修复（`truncateAttr` 自动净化）|

### 6.2 接入 Jaeger（本地开发）

```bash
# 启动 Jaeger（all-in-one，含 OTLP HTTP 接收端）
docker run --rm -p 16686:16686 -p 4318:4318 jaegertracing/all-in-one

export OTEL_ENABLED=true
export OTEL_EXPORTER_TYPE=otlp
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
./harness9

# 打开 http://localhost:16686 → 搜索 Service: harness9
```

### 6.3 接入 Grafana + Tempo

```bash
export OTEL_ENABLED=true
export OTEL_EXPORTER_TYPE=otlp
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318  # Tempo OTLP HTTP 端口
./harness9
```

### 6.4 本地调试（stdout 导出器）

```bash
export OTEL_ENABLED=true
export OTEL_EXPORTER_TYPE=stdout
./harness9
# Span 数据以 JSON 格式打印到 stderr
```

---

## 七、模块文件索引

| 文件 | 包 | 职责 |
|------|----|------|
| `internal/engine/observer.go` | `engine` | `EngineObserver` 接口 + `noopObserver`（零开销空实现） |
| `internal/observability/config.go` | `observability` | `Config` 结构体 + `ConfigFromEnv()`（含 parseOTLPHeaders） |
| `internal/observability/attributes.go` | `observability` | Span 名称、Metric 名称、Langfuse v4 属性键常量 |
| `internal/observability/setup.go` | `observability` | OTEL SDK 初始化（`Setup`、`NewNoopProviders`、ForceFlush 绑定） |
| `internal/observability/observer.go` | `observability` | `OTELEngineObserver`（Interaction + Turn Span，双写保证链路） |
| `internal/observability/provider.go` | `observability` | `TracingProvider`（LLM Request Span + Token Metrics） |
| `internal/observability/hook.go` | `observability` | `ObservabilityHook`（Tool Execution Span + Tool Metrics） |
| `internal/observability/helpers.go` | `observability` | `serializeMessages` / `serializeOutput` / `truncateAttr`（UTF-8 净化）|
| `internal/evals/provider.go` | `evals` | `ScriptedProvider`（确定性 mock，线程安全）|
| `internal/evals/assertions.go` | `evals` | `Assertion` 接口 + `Case` / `Result` 类型 + 8 种断言（Hard/Soft）|
| `internal/evals/harness.go` | `evals` | `RunCase` / `Suite` / `recordingHook`（临时目录自动清理）|
| `internal/evals/testenv.go` | `evals` | `SetupHermeticEnv()`（标准 Hermetic 隔离，`t.Setenv` 自动恢复）|
| `internal/evals/report.go` | `evals` | `BuildReport` / `WriteJSON` / `WriteMarkdown`（分类统计 + 详细结果）|
| `internal/evals/dataset/tool_calling_test.go` | `dataset` | 工具调用准确性（4 用例）|
| `internal/evals/dataset/planning_test.go` | `dataset` | Planning 完成率（4 用例）|
| `internal/evals/dataset/context_test.go` | `dataset` | Context Engineering（3 用例）|
| `internal/evals/dataset/error_handling_test.go` | `dataset` | Error Handling / Self-Healing（3 用例）|
| `internal/evals/dataset/memory_test.go` | `dataset` | Memory 持久化（2 用例）|
| `.github/workflows/eval.yml` | CI | GitHub Actions Quality Gate |
