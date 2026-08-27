# Agent Loop 核心实现原理

## 1. 架构总览

harness9 的核心是一个**标准 ReAct 循环**引擎，每个 Turn 执行一次 LLM 调用，根据响应决定执行工具或结束任务。引擎编排三个核心抽象协同工作：

```
┌──────────────────────────────────────────────────────────────────────┐
│                         AgentEngine                                   │
│                    (核心编排器 / ReAct Loop)                           │
│                                                                      │
│  ┌────────────────────────────────────────────────────────────────┐  │
│  │                    每个 Turn 的单阶段流程                         │  │
│  │                                                                │  │
│  │  LLM 调用                                                       │  │
│  │  ┌───────────────┐  Generate(tools=all)  ┌───────────────┐    │  │
│  │  │  Context       │ ─────────────────── ► │  LLMProvider   │    │  │
│  │  │  History       │ ◄── 文本 + ToolCalls ─ │  (推理与行动)   │    │  │
│  │  └───────┬───────┘                       └───────────────┘    │  │
│  │          │                                                      │  │
│  │          │ 注入到 contextHistory                                │  │
│  │          │                                                      │  │
│  │          │ ToolCalls                                            │  │
│  │          ▼                                                      │  │
│  │  ┌───────────────┐  Execute()  ┌───────────────┐              │  │
│  │  │  Observation   │ ◄────────── │  Registry      │              │  │
│  │  │  (工具结果)     │             │  (工具执行层)   │              │  │
│  │  └───────────────┘             └───────────────┘              │  │
│  └────────────────────────────────────────────────────────────────┘  │
│                                                                      │
└──────────────────────────────────────────────────────────────────────┘
```

| 组件 | 代码位置 | 职责 |
|------|---------|------|
| `schema` | `internal/schema/message.go` | 定义跨组件共享的核心数据类型 |
| `schema.StreamChunk` | `internal/schema/stream.go` | Provider 层流式增量数据类型 |
| `LLMProvider` | `internal/provider/interface.go` | 抽象 LLM 通信层，封装 API 差异（含阻塞 + 流式） |
| `OpenAIProvider` | `internal/provider/openai.go` | OpenAI 兼容 API 适配器（OpenAI / OpenRouter / Azure） |
| `AnthropicProvider` | `internal/provider/anthropic.go` | Anthropic 兼容 API 适配器（Anthropic / OpenRouter） |
| `Registry` | `internal/tools/registry.go` | 解耦工具发现与执行 |
| `AgentEngine.Run` | `internal/engine/agent_loop.go` | 阻塞式 ReAct 主循环 |
| `AgentEngine.RunStream` | `internal/engine/stream.go` | 流式 ReAct 主循环，逐 token 输出 |
| `engine.Event` | `internal/engine/stream.go` | 引擎面向客户端的流式事件类型 |
| `env` | `internal/env/env.go` | 基于 .env 文件的环境变量配置加载 |

## 2. ReAct 设计理念

ReAct（Reasoning + Acting）是 harness9 采用的标准 Agent 循环模式。每个 Turn 中，LLM 接收当前对话上下文（包含历史工具结果），同时输出推理文本和工具调用请求（或最终回复）。

```
Turn N:
  LLM(contextHistory, tools) → 推理文本 + ToolCalls（或纯文本最终回复）
  → 若有 ToolCalls：并发执行 → 将结果作为 Observation 注入上下文 → Turn N+1
  → 若无 ToolCalls：任务完成，退出循环
```

**emitter 抽象**将循环内核（`runLoop`）与输出侧行为解耦，使阻塞模式（`Run`）和流式模式（`RunStream`）共享同一套循环逻辑：

| emitter 方法 | 阻塞模式行为 | 流式模式行为 |
|-------------|------------|------------|
| `generate` | 调用 `Generate`，文本打印到 stdout | 调用 `GenerateStream`，文本增量发送为 `EventActionDelta` |
| `toolStart` | 写结构化日志 | 写日志 + 发送 `EventToolStart` |
| `toolDone` | 写结构化日志 | 写日志 + 发送 `EventToolResult` |

## 3. 数据模型 (`internal/schema`)

### 3.1 消息角色体系

```
Role (string)
├── "system"     → 系统提示词：定义 Agent 身份、约束与行为边界
├── "user"       → 用户输入 & 工具执行结果 (Observation)
└── "assistant"  → 模型输出：推理文本 + 工具调用请求
```

每个 Turn 产生一条 assistant 消息（含推理文本和/或 ToolCalls），以及若干 user 消息（每个工具结果一条）。

### 3.2 核心类型关系

```
┌──────────────────────────────────────────────────┐
│  Message                                         │
│  ├── Role        Role        消息作者角色          │
│  ├── Content     string      纯文本内容            │
│  ├── ToolCalls   []ToolCall  模型发出的工具调用请求  │
│  └── ToolCallID  string      关联原始 ToolCall 的 ID│
│                                                  │
│  ToolCall                 ToolResult              │
│  ├── ID         string     ├── ToolCallID  string │
│  ├── Name       string     ├── Output      string │
│  └── Arguments  RawMessage └── IsError      bool  │
│                                                  │
│  ToolDefinition                                  │
│  ├── Name        string   工具唯一标识             │
│  ├── Description string   用途描述                │
│  └── InputSchema interface{} 参数 JSON Schema      │
└──────────────────────────────────────────────────┘
```

**关键设计决策：**

- **`ToolCall.Arguments` 使用 `json.RawMessage`**：延迟反序列化，将参数解析责任交给具体工具实现。
- **`ToolDefinition.InputSchema` 使用 `interface{}`**：不同 LLM SDK 对工具参数格式要求不同（OpenAI 需要 `shared.FunctionParameters`，Anthropic 需要 `map[string]any`），各 Provider 内部负责类型转换，避免额外的 JSON 往返序列化开销。
- **`ToolCallID` 关联机制**：工具执行结果（Observation）通过 `ToolCallID` 与原始 `ToolCall` 关联。
- **`ToolResult.IsError` 自愈标记**：当工具执行失败时，引擎将错误暴露给 LLM，使其能尝试修正参数并重试（Self-Healing）。

### 3.3 流式数据类型

#### Provider 层 — `schema.StreamChunk`（`internal/schema/stream.go`）

Provider 通过 `GenerateStream` 方法返回 `<-chan StreamChunk`，每个 chunk 代表 LLM 的一次增量产出。工具调用参数的流式累积在 Provider 内部由 `toolCallAccumulator` 完成，不通过 `StreamChunk` 暴露中间状态——`StreamChunkDone` 中的完整 `Message.ToolCalls` 已是累积后的最终结果：

```
StreamChunk
├── Type     StreamChunkType  chunk 类型标识
├── Delta    string           文本增量（text_delta / thinking_delta 时有效）
├── Message  *Message         完整响应（done 时有效，含 ToolCalls）
├── Usage    *Usage           token 用量（done 时由 Provider 填充）
└── Error    string           错误信息（error 时有效）
```

**chunk 类型生命周期：**

```
text_delta ──────────────────────────────────────┐   (多次，逐 token)
                                                  │
thinking_delta ──────────────────────────────────┤   (多次，推理内容，可选)
                                                  │
                                                  ▼
                                               done  (流结束，携带完整 Message + Usage)
```

| StreamChunkType | 含义 | 携带数据 |
|----------------|------|---------|
| `text_delta` | 文本增量，逐 token | `Delta` |
| `thinking_delta` | 推理增量（extended thinking / reasoning_content） | `Delta` |
| `done` | 流结束 | `Message`（完整响应，含 ToolCalls）、`Usage` |
| `error` | 出错 | `Error` |

**工具调用累积说明：** Provider 内部通过 `toolCallAccumulators`（`internal/provider/tool_call_accumulator.go`）将 SDK 流式返回的 JSON 片段拼接为完整的工具参数，最终统一放入 `StreamChunkDone.Message.ToolCalls`，上层无需感知中间状态。

#### Engine 层 — `engine.Event`（`internal/engine/stream.go`）

引擎通过 `RunStream` 方法返回 `<-chan Event`，将 Provider 的底层 StreamChunk 转化为面向客户端的语义事件：

```
Event
├── Type EventType  事件类型
├── Turn int        当前 Turn 编号
└── Data any        事件载荷（类型随 Type 变化）
```

| EventType | 含义 | Data 类型 |
|-----------|------|----------|
| `action_delta` | LLM 输出的文本增量（逐 token） | `string` |
| `thinking_delta` | 推理内容增量（extended thinking / reasoning） | `string` |
| `tool_start` | 工具开始执行 | `schema.ToolCall` |
| `tool_result` | 工具执行完成 | `ToolResultData`（含 `Result schema.ToolResult` 和 `Duration time.Duration`） |
| `token_update` | 每轮 LLM 调用前发出，报告 token 估算 | `TokenUpdateData` |
| `compaction` | 上下文发生有效压缩（token 减少 > 5%） | `CompactionData` |
| `approval_required` | 工具执行需要人类审批 | `ApprovalRequest` |
| `terminated` | 护栏受控熔断终止 | `TerminationData{Reason, Message}` |
| `state_change` | 状态机流转 | `StateChangeData{From, To, Turn}` |
| `done` | 循环正常结束 | `nil` |
| `error` | 出错 | `string` |

**事件流转示例：**

```
Turn 1:
  token_update        ← LLM 调用前估算值
  thinking_delta × N  ← 推理内容（若模型支持 extended thinking）
  action_delta × N    ← LLM 逐 token 输出
  token_update        ← 实际 token 用量（LLM 返回后更新）
  approval_required   ← 危险工具等待人类审批（可选）
  tool_start          ← 工具开始执行
  tool_result         ← 工具执行完成
Turn 2:
  token_update        ← 下轮估算值
  action_delta × N    ← 最终回复（无工具调用）
  done                ← 循环结束
```

## 4. Agent Loop 循环流程

```
                     ┌─────────────────────┐
                     │   初始化对话上下文     │
                     │   System(含WorkDir)  │
                     │   + User             │
                     └──────────┬──────────┘
                                │
                ┌───────────────▼───────────────┐
                │   Turn 计数 ++                  │
                │   检查 MaxTurns / ctx.Done()   │
                └───────────────┬───────────────┘
                                │
                   ┌────────────▼────────────┐
                   │  LLM 调用                │
                   │  Generate(availableTools)│
                   │  → 注入 contextHistory   │
                   └────────────┬────────────┘
                                │
                       ┌────────▼────────┐    有 ToolCalls
                       │  终止条件检测     │──────────────────┐
                       │  ToolCalls == 0? │                   │
                       └────────┬────────┘                   │
                                │ 无 ToolCalls               │
                       ┌────────▼────────┐    ┌──────────────┴───────────┐
                       │  任务完成         │    │  ToolCall 阶段 (并发)     │
                       │  退出循环         │    │  信号量限制并发数          │
                       └─────────────────┘    │  每工具独立超时            │
                                              └────────────┬─────────────┘
                                                           │
                                             ┌─────────────▼────────────┐
                                             │  Observation 阶段         │
                                             │  追加工具结果到上下文      │
                                             └────────────┬─────────────┘
                                                           │
                                             ┌─────────────▼────────────┐
                                             │  回到 Turn 计数 ++        │
                                             └──────────────────────────┘
```

### 4.1 初始化阶段

引擎启动时，通过 `loadHistoryWith` 构造初始对话上下文。若注入了 `Session`，历史消息从持久化存储中恢复；否则仅含 system 提示和当前用户输入：

```go
// loadHistoryWith 从 Session 恢复历史消息，注入 system prompt，追加用户输入。
// startLen 标记新消息起始位置（已有历史 + system 不持久化），
// 用于 saveHistoryWith 时仅保存 msgs[startLen:]。
func (e *AgentEngine) loadHistoryWith(ctx context.Context, userPrompt string, sess memory.Session) ([]schema.Message, int) {
    var history []schema.Message
    if sess != nil {
        msgs, err := sess.GetMessages(ctx, 0) // 0 = 返回全部历史
        if err == nil {
            history = msgs
        }
    }
    // system prompt 注入在历史消息开头（若尚不存在），每次调用重建，不持久化到 DB。
    if len(history) == 0 || history[0].Role != schema.RoleSystem {
        history = append([]schema.Message{{Role: schema.RoleSystem, Content: e.buildSystemPrompt()}}, history...)
    }
    startLen := len(history) // 新消息从此处开始；system prompt 不计入持久化范围
    history = append(history, schema.Message{Role: schema.RoleUser, Content: userPrompt})
    return history, startLen
}
```

**WorkDir 会被注入到 system prompt** 中，使 LLM 了解其工作目录。system prompt 本身不持久化（每次启动时重建并前插到历史消息开头，避免重复插入），`startLen` 标记新消息的起始位置，用于 `saveHistoryWith` 时仅保存 `msgs[startLen:]`。

### 4.2 LLM 调用阶段

每个 Turn 执行一次 LLM 调用，携带完整工具列表：

```go
availableTools := e.registry.GetAvailableTools()
responseMsg, err := em.generate(ctx, turnCount, contextHistory, availableTools)
contextHistory = append(contextHistory, *responseMsg)
```

### 4.3 终止条件检测 —— 四维熔断保障

熔断裁决集中在 `loopGuard`（`internal/engine/loop_guard.go`），只在 Turn 边界触发，
接受最多一轮的过冲，绝不在流式中途撕断：

| 维度 | 配置 | 默认值 | 裁决时机 |
|------|------|--------|---------|
| 自然终止 | — | — | `ToolCalls == 0` |
| MaxTurns | `WithMaxTurns` | 500 | Turn 开始 |
| 墙钟超时 | `WithRunTimeout(d)` | 不限 | Turn 开始；工具子 context 取 min(toolTimeout, remaining) |
| Token 预算 | `WithTokenBudget(n)` | 不限 | Turn 开始（按 API 实际 usage 累计，缺失时估算兜底）|
| 重复死循环 | `WithRepetitionReminder(window, threshold)` | 关闭 | Turn 开始（详见 Reminder 系统）|

**统一受控出口**：所有受控熔断收敛到 `terminate` 闭包——置 `Terminated` 状态、
发送 `EventTerminated{Reason, Message}`、**执行历史持久化**（修复旧实现熔断路径
丢失轨迹的缺陷）、记录带原因的结构化日志、返回 error（返回语义保持不变）。
意外故障（LLM 重试耗尽 / context 取消）仍走 `EventError`，TUI 可据此区分二者。

### 4.4 ToolCall 阶段 — 并发执行（带独立超时）

当模型请求调用多个工具时，引擎使用 **goroutine + `sync.WaitGroup`** 并发执行。可选信号量（`maxConcurrentTools`）控制最大并发度，**每个工具有独立的超时控制**：

```go
go func(idx int, tc schema.ToolCall) {
    defer wg.Done()

    if sem != nil {
        sem <- struct{}{}
        defer func() { <-sem }()
    }

    // 独立超时：单个工具超时不影响其他工具
    toolCtx := ctx
    if e.toolTimeout > 0 {
        toolCtx, cancel = context.WithTimeout(ctx, e.toolTimeout)
        defer cancel()
    }

    results[idx] = e.registry.Execute(toolCtx, tc)
}(i, toolCall)
```

**并发安全设计要点：**

| 问题 | 解决方案 |
|------|---------|
| 多个 goroutine 写入同一结果集 | 预分配切片，每个 goroutine 按索引 `idx` 写入独立位置 |
| 结果顺序一致性 | 索引与原始 `ToolCalls` 顺序一一对应 |
| 单工具超时 | `context.WithTimeout` 为每个工具创建独立子 context |
| 闭包变量捕获 | `idx`、`tc` 显式传参，避免数据竞争 |
| 并发度控制 | 有缓冲 channel 信号量，0 = 不限制 |

### 4.5 Observation 阶段

工具执行完毕后，结果按原始顺序追加到上下文：

```go
for i, toolCall := range responseMsg.ToolCalls {
    contextHistory = append(contextHistory, schema.Message{
        Role:       schema.RoleUser,        // Observation 以 user 角色回传
        Content:    results[i].Output,
        ToolCallID: toolCall.ID,             // 关联原始请求
    })
}
```

### 4.6 流式架构（`RunStream`）

`RunStream` 是 `Run` 的流式对应方法，共享相同的 `runLoop` 主循环逻辑，通过 Go channel 逐事件输出。核心数据流：

```
┌─────────────┐  GenerateStream()  ┌──────────────────┐
│  LLMProvider │ ───────────────── │  chan StreamChunk  │
│  (OpenAI /   │                   │  (逐 token delta)  │
│   Anthropic) │                   └────────┬─────────┘
└─────────────┘                             │
                                            ▼
                                   ┌──────────────────┐
                                   │  streamGenerate() │
                                   │  读 StreamChunk   │
                                   │  转发为 Event     │
                                   └────────┬─────────┘
                                            │
                                            ▼
┌─────────────┐  Execute()         ┌──────────────────┐
│  Registry    │ ─────────────────  │    chan Event     │
│  (工具执行)   │                    │  (面向客户端)      │
└─────────────┘                    └────────┬─────────┘
                                            │
                                            ▼
                                   ┌──────────────────┐
                                   │   客户端消费者     │
                                   │   (TUI / CLI /    │
                                   │    SSE handler)   │
                                   └──────────────────┘
```

**`streamGenerate` 方法**替代阻塞模式中直接调用 `Generate` 的位置。它调用 `GenerateStream`，从 `StreamChunk` channel 中读取并转发为语义化的 `Event`：

```go
func (e *AgentEngine) streamGenerate(ctx context.Context, ch chan<- Event,
    turn int, history []schema.Message, tools []schema.ToolDefinition) (*schema.Message, error) {

    stream, err := e.provider.GenerateStream(ctx, history, tools)
    for chunk := range stream {
        switch chunk.Type {
        case schema.StreamChunkTextDelta:
            sendEvent(ctx, ch, Event{Type: EventActionDelta, Turn: turn, Data: chunk.Delta})
        case schema.StreamChunkDone:
            msg = chunk.Message
        }
    }
    return msg, nil
}
```

**context 取消感知**：所有 channel 发送都通过 `select` 监听 `ctx.Done()`，确保取消时不会阻塞：

```go
func sendEvent(ctx context.Context, ch chan<- Event, evt Event) bool {
    select {
    case <-ctx.Done():
        return false
    case ch <- evt:
        return true
    }
}
```

### 4.7 Reminder 干预系统

Reminder 干预系统取代了原先平铺的 Nudge 机制（`WithMemoryNudge` / `WithStallNudge`
已删除，更名为 `WithMemoryReminder` / `WithStallReminder`，语义不变），并由
`loopGuard.EvaluateReminders` 统一承担软干预与重复检测升级两条路径。

**三源仲裁**：每轮至多注入一条干预消息，优先级为 **重复 > 停滞 > 记忆提示**：

```
EvaluateReminders(turnCount)
│
├─ ① 重复检测命中？
│     ├─ 首次达标 → 注入定向提醒，置 reminded，继续运行
│     └─ 提醒后再次达标 → 升级为硬终止（ReasonRepetitionLoop）
│
├─ ② 停滞检测命中？（连续 StallWindow 轮无进展工具）→ 注入停滞提醒，计数归零
│
└─ ③ 记忆提醒命中？（turnCount % MemoryInterval == 0）→ 注入记忆提示
```

**定向提醒模板**（重复检测专属，携带事实性定位，由签名标签动态生成）：

> 系统检测：你在最近 {window} 轮内已第 {total} 次发起相同的工具调用（{name(args...)}），
> 且每次都得到相同结果。继续同一调用不会产生新信息。请择一执行：
> ① 改用其他手段获取所需信息；② 基于已有结果推进任务；
> ③ 若任务已完成，直接输出最终回复停止。

**重复签名与升级策略**：签名为 `sha256(工具名 + canonical JSON 参数)`（canonical 化消除
键序与空白差异）；同一签名在无进展的工作周期内累计出现达到 `threshold` 次：
首次达标注入上述定向提醒并继续运行；提醒已被证明无效后再次达标，升级为硬终止——
走统一受控出口持久化轨迹、返回 error。

**进展打破规则**：一旦某轮包含进展工具（`edit_file` / `write_file`），清空全部签名
重复计数与停滞计数，开启新工作周期。理由：SWE-bench 式"改代码 → 重跑同一命令"
是合法修复节奏，参数不变的 `go build` 夹在编辑动作之间并非死循环。

**注入机制与语义**：提醒以 `{Role: user}` 消息追加到**本轮派发副本**（压缩后的 history）
末尾，位于最后一个工具结果之后；防御性副本不写入 `contextHistory`、不持久化、不跨轮
累积，每轮重新评估重新注入。Anthropic 连续 user 消息由 Provider 的 `convertMessages`
兼容处理。注入前的仲裁若裁决出重复升级终止，则本轮直接受控退出，不再发送 LLM 调用。

### 4.8 显式状态机

`LoopState` 将 `runLoop` 的执行阶段显式化为单一事实源：熔断检查点可按状态精准生效，
可观测层（OTEL Span 属性）与客户端事件流共享同一份阶段事实源。

**七态图与单向流转表：**

```
idle ──► turn_start ⇄ (compacting ──► generating ──► tool_executing) ──► done | terminated
```

| 当前状态 | 可流转至 |
|---------|---------|
| `idle` | `turn_start` |
| `turn_start` | `compacting`, `terminated` |
| `compacting` | `generating`, `terminated` |
| `generating` | `tool_executing`, `done`, `terminated` |
| `tool_executing` | `turn_start` |
| `done` | —（终态）|
| `terminated` | —（终态）|

**局部值无锁决策**：状态变量是 `runLoop` 的局部值而非引擎字段，多引擎实例并发天然
无竞态，延续 session/planMode 快照式读取的隔离哲学。

**单一出口汇聚**：所有流转经 `setState` 汇聚到两路观察者——`EngineObserver.OnStateChange(ctx, from, to, turn)` 回调与流式事件 `EventStateChange{Data: StateChangeData{From, To, Turn}}`。
非法流转被拒绝并记录告警日志（防御性，编码失误不会击穿主循环）。

## 5. 接口抽象与解耦设计

### 5.1 LLMProvider 接口

```go
type LLMProvider interface {
    // 阻塞式调用：返回完整响应 Message 和实际 token 用量（Usage 可能为 nil）
    Generate(ctx context.Context, messages []schema.Message,
             availableTools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error)

    // 流式调用：通过 channel 逐 chunk 返回增量；最后一个有效 chunk 类型为 StreamChunkDone
    GenerateStream(ctx context.Context, messages []schema.Message,
                   availableTools []schema.ToolDefinition) (<-chan schema.StreamChunk, error)
}
```

**设计理念：**
- 引擎只依赖接口，切换模型只需替换 Provider 实现
- 双模式共存：`Generate` 用于阻塞场景，`GenerateStream` 用于流式场景
- `GenerateStream` 返回的 channel 在流结束后自动关闭，最后一个有效 chunk 的 Type 为 `StreamChunkDone`

### 5.2 具体实现

两个 Provider 均采用**统一的消息转换层**架构，`Generate` 和 `GenerateStream` 共享同一套转换逻辑：

```
                    ┌──────────────────┐
                    │  convertMessages  │ ← schema.Message → SDK 原生消息
                    │  convertTools     │ ← schema.ToolDefinition → SDK 原生工具
                    └───────┬──────────┘
                            │
               ┌────────────┼─────────────┐
               ▼                           ▼
        Generate()                 GenerateStream()
        SDK.New()                  SDK.NewStreaming()
        → *Message                 → chan StreamChunk
```

#### OpenAIProvider（`internal/provider/openai.go`）

OpenAI 兼容实现，支持所有遵循 OpenAI Chat Completion API 规范的后端：

| 环境变量 | 说明 |
|---------|------|
| `OPENAI_API_KEY` | API 认证密钥（必需） |
| `OPENAI_BASE_URL` | API 端点基址，如 `https://api.openai.com/v1`（必需） |

```go
p, err := provider.NewOpenAIProvider("gpt-4o")
```

**消息转换规则：**

| schema 类型 | OpenAI SDK 类型 |
|-------------|----------------|
| `RoleSystem` | `openai.SystemMessage` |
| `RoleUser`（含 ToolCallID） | `openai.ToolMessage(content, toolCallID)` |
| `RoleUser`（无 ToolCallID） | `openai.UserMessage(content)` |
| `RoleAssistant` | `ChatCompletionAssistantMessageParam`（含 ToolCalls） |
| `ToolDefinition` | `openai.ChatCompletionFunctionTool` |

`InputSchema` 的 `interface{}` → `shared.FunctionParameters` 转换由 `convertToFunctionParameters` 函数完成：优先尝试直接类型断言，失败时通过 JSON 往返转换。

**流式实现：** `GenerateStream` 使用 `client.Chat.Completions.NewStreaming()` 返回 `*ssestream.Stream[ChatCompletionChunk]`。内部使用 `openaiToolCallAccumulator` 累积工具调用参数。

#### AnthropicProvider（`internal/provider/anthropic.go`）

Anthropic 兼容实现，支持 Anthropic 官方和 OpenRouter 等兼容端点：

| 环境变量 | 说明 |
|---------|------|
| `ANTHROPIC_API_KEY` | API 认证密钥（必需） |
| `ANTHROPIC_BASE_URL` | API 端点基址，如 `https://api.anthropic.com`（必需） |

```go
p, err := provider.NewAnthropicProvider("claude-sonnet-4-20250514", 4096)
//                                                        model     maxTokens
```

**Anthropic API 特殊处理：**

| 差异点 | 处理方式 |
|--------|---------|
| System prompt 不在 messages 数组中 | 从 `RoleSystem` 消息中提取，设置为 `params.System` |
| ToolUseBlock 的 Input 类型 | `json.Unmarshal` 将 `Arguments` 解析为 `map[string]interface{}` |
| `required` 字段类型 | `extractSchemaFields` 安全处理 `[]interface{}` → `[]string` 转换 |
| `MaxTokens` 必须显式指定 | 通过构造函数参数传入，默认 4096 |

**流式实现：** `GenerateStream` 使用 `client.Messages.NewStreaming()` 返回 `*ssestream.Stream[MessageStreamEventUnion]`。事件类型映射：

| Anthropic 事件 | 处理 |
|----------------|------|
| `content_block_start` (type=tool_use) | → `StreamChunkToolCallStart`，记录 ID/Name |
| `content_block_delta` (type=text_delta) | → `StreamChunkTextDelta` |
| `content_block_delta` (type=input_json_delta) | → `StreamChunkToolCallDelta`，累积 partial JSON |

### 5.3 环境配置（`internal/env`）

`env` 包提供零依赖的 `.env` 文件加载器，在程序启动时调用：

```go
env.Load(filepath.Join(workDir, ".env"))
```

| 特性 | 说明 |
|------|------|
| 系统环境变量优先 | 已存在的环境变量不会被 `.env` 文件覆盖 |
| 静默跳过缺失文件 | 无 `.env` 文件时返回 nil，不阻断启动 |
| 支持引号值 | 自动去除成对匹配的双引号或单引号 |
| 注释和空行 | `#` 开头的行和空行被跳过 |

### 5.4 Registry 接口

```go
type Registry interface {
    Register(tool BaseTool) error
    GetAvailableTools() []schema.ToolDefinition
    Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult
}
```

### 5.5 依赖注入 + 函数选项

```go
eng := engine.NewAgentEngine(p, r, workDir,
    engine.WithMaxTurns(100),
    engine.WithToolTimeout(30 * time.Second),
    engine.WithMaxConcurrentTools(4),
    engine.WithSession(sess),
    engine.WithCompactor(&memory.SlidingWindowCompactor{MaxMessages: 100}),
)
```

| 选项 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `WithMaxTurns(n)` | `int` | 500 | 单次 Run 最大 Turn 数，0 = 不限制 |
| `WithToolTimeout(d)` | `time.Duration` | 60s | 单个工具执行超时，0 = 使用原始 context |
| `WithRunTimeout(d)` | `time.Duration` | 不限（0 = 关闭） | 墙钟超时 deadline；工具子 context 取 min(toolTimeout, remaining) |
| `WithTokenBudget(n)` | `int` | 不限（0 = 关闭） | 累计 input token 预算，按 API 实际 usage 统计、缺失时估算兜底 |
| `WithRepetitionReminder(window, threshold)` | `int, int` | 关闭 | 重复签名检测：同一签名累计出现 ≥ threshold 次先注入定向提醒，再次命中升级硬终止 |
| `WithStallReminder(window, text)` | `int, string` | 0, "" | 连续 window 轮无进展工具时注入一次停滞提醒，0 关闭（原 `WithStallNudge` 更名） |
| `WithMemoryReminder(interval, text)` | `int, string` | 0, "" | 每隔 interval 轮向防御性副本注入长期记忆提示，0 关闭（原 `WithMemoryNudge` 更名） |
| `WithMaxConcurrentTools(n)` | `int` | 0 | 同一 Turn 内最大并发工具数，0 = 不限制 |
| `WithSession(s)` | `memory.Session` | nil | 注入会话存储，启用历史消息持久化 |
| `WithCompactor(c)` | `memory.Compactor` | nil | 注入上下文压缩器，控制上下文窗口大小 |
| `WithContextWindow(n)` | `int` | 0 | 模型 context window（tokens），用于 TUI token 使用率展示 |
| `WithPromptBuilder(pb)` | `PromptBuilder` | nil | 自定义 system prompt 构建器，nil 时使用内置默认文案 |
| `WithPlanMode(mode)` | `planning.PlanMode` | Default | 初始执行模式；可运行时通过 `SetPlanMode` 更新 |
| `WithTodoStore(s)` | `*planning.TodoStore` | nil | 绑定任务列表，启用跨会话 todo 持久化 |
| `WithEngineObserver(o)` | `EngineObserver` | noopObserver | 注入生命周期观察者（OTEL Tracing 等），nil 时退化为 noop |

运行时可通过 `eng.SetSession(sess)` 切换会话，`eng.SetPlanMode(mode)` 切换执行模式（均并发安全，内部使用 `sync.RWMutex`，但对当前正在运行的 `runLoop` 无影响）。

**双模式调用：**

```go
// 阻塞式：同步等待完整结果
err := eng.Run(ctx, prompt)

// 流式：通过 channel 逐事件返回
stream, err := eng.RunStream(ctx, prompt)
for evt := range stream {
    switch evt.Type {
    case engine.EventActionDelta:
        fmt.Print(evt.Data.(string))  // 逐 token 输出
    case engine.EventDone:
        // 循环结束
    }
}
```

两种模式共享同一个 `AgentEngine` 实例和配置，运行时可自由选择。

## 6. 日志与可观测性

### 6.1 EngineObserver 接口

`EngineObserver` 是引擎为可观测层提供的唯一扩展接口。`runLoop` 在 4 个生命周期节点回调它：

```go
type EngineObserver interface {
    OnInteractionStart(ctx, sessionID, prompt) context.Context  // runLoop 入口
    OnInteractionEnd(ctx, turns, err)                           // runLoop 退出（defer 保证）
    OnTurnStart(ctx, turn) context.Context                      // 每个 Turn 开始
    OnTurnEnd(ctx, turn, hasToolCalls)                          // 每个 Turn 结束
}
```

所有 `OnXxxStart` 方法返回增强 ctx（可携带 OTEL Span），供 LLM 调用和工具执行继承父链路。
未注入时自动退化为零开销的 `noopObserver`。

**自定义 Observer 注意事项**：实现 `OnInteractionStart` / `OnTurnStart` 时，除了用 `context.WithValue` 将 Span 存入自定义 key（供 `OnInteractionEnd` / `OnTurnEnd` 取用），还**必须**通过 `trace.ContextWithSpan(ctx, span)` 将 Span 写入 OTEL 标准 slot，否则下游的 `tracer.Start(ctx, ...)` 无法找到父节点，导致每个 Span 独立成为根节点：

```go
func (o *MyObserver) OnInteractionStart(ctx context.Context, sessionID, prompt string) context.Context {
    ctx, span := o.tracer.Start(ctx, "my.interaction")
    // ① 写入 OTEL 标准 slot——下游 tracer.Start 自动嵌套
    ctx = trace.ContextWithSpan(ctx, span)
    // ② 写入自定义 key——供 OnInteractionEnd 取用
    return context.WithValue(ctx, mySpanKey{}, span)
}
```

### 6.2 结构化日志

引擎采用结构化日志格式，阻塞模式使用 `[engine]` 前缀，流式模式使用 `[engine-stream]` 前缀：

**阻塞模式日志示例：**

```
[engine] 启动 | workdir=/Users/zsa/project maxTurns=50 toolTimeout=1m0s maxConcurrent=0
[engine] ======== Turn 1 ======== | history=2  tools=3
[engine] 工具启动 | name=bash id=call_123
[engine] 工具完成 | name=bash bytes=45
[engine] Turn 1 | Observation 注入完成 | history=4 | llm=1.2s tools=0.3s turn=1.5s
[engine] ======== Turn 2 ======== | history=4  tools=3
[engine] Turn 2 | 任务完成 | llm=0.8s total=2.3s
[engine] 循环结束 | 总Turns=2 | total_time=2.3s
```

**日志分层：**

| 层级 | 前缀 | 内容 | 输出方式 |
|------|------|------|---------|
| 引擎内部（阻塞） | `[engine]` | Turn 计数、工具状态 | `log.Printf`（stderr） |
| 引擎内部（流式） | `[engine-stream]` | 同上 | `log.Printf`（stderr） |
| 模型输出（阻塞） | `[assistant]` | LLM 产出的文本内容 | `fmt.Printf`（stdout） |
| 模型输出（流式） | 无前缀 | 通过 Event channel 交给客户端处理 | 由消费者控制 |

## 7. 完整数据流图

以一个两轮对话为例：

```
Turn 1:
  [Context]
    system:    "You are harness9... working directory is: /test"
    user:      "我今天想去北京旅游，帮我看看天气合适吗？"

  LLM 调用: → Generate(ctx, history, [get_weather])
    assistant: "让我查询一下北京的天气。"
               + ToolCall{id:"call_abc", name:"get_weather", args:{"city":"北京"}}
    → 注入到 contextHistory

  ToolCall: → Registry.Execute(get_weather, {"city":"北京"})
    ToolResult{id:"call_abc", output:"今天天气晴，最低温度 14 度..."}

  Observation: user: "今天天气晴，最低温度 14 度..." (toolCallID:"call_abc")

Turn 2:
  [Context = 4 messages: system, user, assistant(+ToolCalls), user(obs)]

  LLM 调用: → Generate(ctx, history, [get_weather])
    assistant: "北京今天天气不错，适合出游！" (无 ToolCall)
    → 注入到 contextHistory

  → 终止条件满足，循环退出
```

### 7.1 流式模式数据流

以相同任务在流式模式（`RunStream`）下为例，客户端通过 Event channel 接收增量：

```
Turn 1:
  streamGenerate() → GenerateStream(ctx, history, [get_weather])
    Event{action_delta, "让"}           ← 逐 token
    Event{action_delta, "我"}
    Event{action_delta, "查询一下北京的天气。"}
    Event{tool_start, ToolCall{name:"get_weather", id:"call_abc"}}

  executeTools() → 并发执行工具
    Event{tool_result, ToolResult{output:"今天天气晴，最低温度 14 度..."}}

Turn 2:
  streamGenerate() → GenerateStream(ctx, history, [get_weather])
    Event{action_delta, "北京今天天气不错"}   ← 逐 token
    Event{action_delta, "，适合出游！"}
    Event{done}                              ← 循环结束
```

## 8. Provider 实现对比

| 维度 | OpenAIProvider | AnthropicProvider |
|------|---------------|------------------|
| API 协议 | Chat Completion | Messages |
| System prompt | 作为 messages 数组中的 system 消息 | 作为独立 `params.System` 参数 |
| 工具调用响应 | `ToolCalls[].Function.Arguments`（JSON 字符串） | `Content[]` 中 `tool_use` block 的 `Input`（结构化对象） |
| 历史工具调用 | `ChatCompletionMessageFunctionToolCallParam` | `ToolUseBlockParam` |
| 工具结果回传 | `openai.ToolMessage(content, toolCallID)` | `anthropic.NewToolResultBlock(toolCallID, content, isError)` |
| InputSchema 转换 | `convertToFunctionParameters` → `shared.FunctionParameters` | `extractSchemaFields` → `properties` + `required` |
| MaxTokens | 不需要显式指定 | 必须显式传入 |
| 构造函数 | `NewOpenAIProvider(model) (*OpenAIProvider, error)` | `NewAnthropicProvider(model, maxTokens) (*AnthropicProvider, error)` |
| 流式 SDK 方法 | `client.Chat.Completions.NewStreaming()` | `client.Messages.NewStreaming()` |
| 流式 chunk 类型 | `ChatCompletionChunk` | `MessageStreamEventUnion` |
| 流式文本增量 | `Choices[0].Delta.Content` | `content_block_delta` + `text_delta` |
| 流式工具增量 | `Choices[0].Delta.ToolCalls[]` | `content_block_start(tool_use)` + `input_json_delta` |

两个 Provider 的消息转换逻辑均提取为 `convertMessages` / `convertTools` 方法，`Generate` 和 `GenerateStream` 共享同一套转换逻辑。`schema.Message` → SDK 原生参数的映射封装在 Provider 内部，引擎层无需感知 API 差异。

## 9. 已知限制与未来演进

| 限制 | 当前状态 | 演进方向 |
|------|---------|---------|
| **上下文窗口控制** | 已实现 `SummarizationCompactor`（默认，LLM 摘要 + 增量更新）、`TokenBudgetCompactor`（回退）、`SlidingWindowCompactor`（消息数窗口） | 进一步优化摘要质量；支持自定义摘要模板 |
| **会话历史持久化** | 已实现 SQLiteSession（WAL 模式，`~/.harness9/sessions.db`）+ TodoStore 跨会话持久化 | 多工作目录隔离；会话标签与搜索（FTS5） |
| **流式输出** | 已实现 `RunStream` + `GenerateStream`，支持逐 token delta + EventTokenUpdate/EventCompaction | 扩展 SSE HTTP 端点，对接外部实时推送渠道 |
| **Planning** | 已实现 Plan Mode + TodoStore + 自动续跑 + 停滞检测 | PlanModeAutoEdit 逐步确认编辑模式 |
| **权限控制** | Plan Mode 提供工具层只读约束 | 工具执行前统一 PermissionChecker，支持交互式确认 |
| **Hook 系统** | 无 | PreToolUse / PostToolUse / Stop / TurnComplete 事件钩子 |
| **多 Agent 编排** | 单 Agent 模式 | 子 Agent 调度、并行 Agent、专用角色 Agent |
| **循环序列检测** | A/B/A/B 交替循环暂不检测（精确签名已覆盖高频病理形态） | 序列模式挖掘 / 输出复读机检测（YAGNI 后置，见设计文档 §7 非目标） |

## 10. 设计原则总结

| 原则 | 体现 |
|------|------|
| **标准 ReAct** | Reasoning + Acting + Observation，每 Turn 一次 LLM 调用 |
| **emitter 解耦** | 循环内核与输出侧行为分离，阻塞 / 流式共享同一 `runLoop` |
| **接口隔离** | `LLMProvider` 和 `Registry` 各司其职，引擎只依赖抽象 |
| **双模式共存** | `Run`（阻塞）和 `RunStream`（流式）共享引擎配置，运行时按需选择 |
| **channel 驱动流式** | Provider → `chan StreamChunk` → Engine → `chan Event`，Go 原生 CSP 模型 |
| **函数选项** | `WithMaxTurns` / `WithToolTimeout` / `WithMaxConcurrentTools` 可选配置 |
| **并发安全** | 索引隔离写入 + WaitGroup + 信号量限流 + 显式参数传递，无数据竞争 |
| **四维熔断保障** | 自然终止 + `loopGuard` 四维硬熔断（MaxTurns / 墙钟超时 / Token 预算 / 重复死循环），Turn 边界裁决 + 统一受控出口 |
| **可观测性** | 结构化日志 `[engine]` / `[engine-stream]` 前缀 + key=value 格式 |
| **延迟解析** | `json.RawMessage` 用于 Arguments 延迟反序列化；`interface{}` 用于 InputSchema 兼容多 SDK |
| **自愈能力** | `ToolResult.IsError` 支持模型感知错误并自动重试 |

## PromptBuilder 与 Skills 集成

自 `context-engineering` 分支起，`runLoop` 中的 system prompt 不再硬编码，
而是通过 `PromptBuilder` 接口动态构建：

```go
type PromptBuilder interface {
    Build() string
}
```

`WithPromptBuilder(pb PromptBuilder)` Option 将 builder 注入引擎。
未设置时回退到内置默认文案（向后兼容）。

`internal/context.DefaultPromptBuilder` 的实现按以下顺序组装 prompt：

1. harness9 基础 prompt（角色定义 + workDir）
2. `workdir/AGENTS.md`（不存在时跳过）
3. Skills 索引摘要（来自 `internal/skills.Index.Summary()`）

Skills 的全文内容通过 `use_skill` 工具按需加载（Progressive Disclosure），
不影响基础 ReAct 循环的执行逻辑。
