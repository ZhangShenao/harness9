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
| `AgentEngine.Run` | `internal/engine/agent_loop.go` | 阻塞式 ReAct 主循环（编排器 + emitter 定义） |
| `AgentEngine.RunStream` | `internal/engine/stream.go` | 流式 ReAct 主循环，逐 token 输出 |
| `engine.Event` | `internal/engine/stream.go` | 引擎面向客户端的流式事件类型 |
| `runLoop 阶段化实现` | `internal/engine/loop_phases.go` | 主循环各阶段私有方法：初始化 / Turn 前置 / 预处理 / 生成 / 停滞检测 / Observation 注入 / 收尾 |
| `With* 配置选项` | `internal/engine/options.go` | 函数式选项与运行期 `SetSession` / `SetPlanMode` |
| `generateWithRetry` | `internal/engine/retry.go` | LLM 调用双档重试（默认预算 / 网络传输预算）+ 网络错误分类 |
| `历史加载与持久化` | `internal/engine/history.go` | Session 历史加载 / 保存、system prompt 构建、压缩适配 |
| `Plan Mode 过滤` | `internal/engine/planmode.go` | 只读工具白名单、规划前缀、进展工具与 nudge 判定 |
| `executeTools` | `internal/engine/tools_exec.go` | 同 Turn 多工具并发调度（信号量 + 每工具独立超时） |
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

循环由 `runLoop` 编排器（`agent_loop.go`）按阶段调度，各阶段实现为 `loop_phases.go` 中的独立私有方法，共享一个聚合交互状态的 `loopContext`：

```
beginInteraction      初始化：observer 接入、状态快照、Todo 恢复、Plan 前缀、历史加载
for {
    beginTurn          Turn 计数 + 双重终止判定（MaxTurns / ctx 取消）
    prepareTurnInput   工具过滤 + 压缩检查 + token 估算上报 + nudge 注入
    generateTurn       带重试 LLM 调用 + 实际用量上报
    （自然终止判定）     模型无工具调用则收敛退出
    executeTools       并发工具调度（tools_exec.go）
    injectObservations Observation 注入
}
saveHistory / saveTodos   收尾：历史仅自然终止路径持久化；todos 所有退出路径持久化
```

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
func (e *AgentEngine) loadHistoryWith(ctx context.Context, userPrompt string, sess memory.Session, logPrefix string) ([]schema.Message, int) {
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

### 4.3 终止条件检测

引擎实现三重安全保障：

```go
// 1. MaxTurns 限制：防止无限循环
if e.maxTurns > 0 && turnCount > e.maxTurns {
    return fmt.Errorf("已达最大 Turn 数 (%d)，循环终止", e.maxTurns)
}

// 2. Context 取消：支持超时和手动中断
select {
case <-ctx.Done():
    return fmt.Errorf("context 已取消：%w", ctx.Err())
default:
}

// 3. 自然终止：模型不再请求工具调用
if len(responseMsg.ToolCalls) == 0 {
    break
}
```

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

工具执行完毕后，`injectObservations` 将结果按原始顺序追加到上下文。空输出兜底为占位文案（部分 backend 拒绝空 content 的 tool_result，返回 400 且无信息可供模型推理），`IsError` 透传给 Provider 设置 `tool_result.is_error`（强化自愈信号）：

```go
for i, toolCall := range toolCalls {
    content := results[i].Output
    if content == "" {
        content = "[工具执行完成，无输出]"   // 空输出兜底，避免 400 与无效轮次
    }
    history = append(history, schema.Message{
        Role:       schema.RoleUser,        // Observation 以 user 角色回传
        Content:    content,
        ToolCallID: toolCall.ID,             // 关联原始请求
        IsError:    results[i].IsError,      // 透传错误信号，供 Provider 标记 is_error
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
| `WithMaxTurns(n)` | `int` | 50 | 单次 Run 最大 Turn 数，0 = 不限制 |
| `WithToolTimeout(d)` | `time.Duration` | 60s | 单个工具执行超时，0 = 使用原始 context |
| `WithMaxConcurrentTools(n)` | `int` | 0 | 同一 Turn 内最大并发工具数，0 = 不限制 |
| `WithSession(s)` | `memory.Session` | nil | 注入会话存储，启用历史消息持久化 |
| `WithCompactor(c)` | `memory.Compactor` | nil | 注入上下文压缩器，控制上下文窗口大小 |
| `WithContextWindow(n)` | `int` | 0 | 模型 context window（tokens），用于 TUI token 使用率展示 |
| `WithPromptBuilder(pb)` | `PromptBuilder` | nil | 自定义 system prompt 构建器，nil 时使用内置默认文案 |
| `WithPlanMode(mode)` | `planning.PlanMode` | Default | 初始执行模式；可运行时通过 `SetPlanMode` 更新 |
| `WithTodoStore(s)` | `*planning.TodoStore` | nil | 绑定任务列表，启用跨会话 todo 持久化 |
| `WithEngineObserver(o)` | `EngineObserver` | noopObserver | 注入生命周期观察者（OTEL Tracing 等），nil 时退化为 noop |
| `WithMemoryNudge(n, text)` | `int, string` | 0, "" | 每隔 n 轮向防御性副本注入长期记忆提示，0 关闭 |

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
| **三重保障终止** | 自然终止 + MaxTurns 限制 + Context 取消 |
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
