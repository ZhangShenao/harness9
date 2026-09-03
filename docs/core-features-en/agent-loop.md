# Agent Loop Core Implementation Principles

## 1. Architecture Overview

The core of harness9 is a **standard ReAct loop** engine: each Turn executes one LLM call and decides whether to execute a tool or end the task based on the response. The engine orchestrates three core abstractions working together:

```
┌──────────────────────────────────────────────────────────────────────┐
│                         AgentEngine                                   │
│                    (Core Orchestrator / ReAct Loop)                   │
│                                                                      │
│  ┌────────────────────────────────────────────────────────────────┐  │
│  │                Single-phase flow per Turn                       │  │
│  │                                                                │  │
│  │  LLM Call                                                       │  │
│  │  ┌───────────────┐  Generate(tools=all)  ┌───────────────┐    │  │
│  │  │  Context       │ ─────────────────── ► │  LLMProvider   │    │  │
│  │  │  History       │ ◄── Text + ToolCalls ─│  (Reasoning &  │    │  │
│  │  └───────┬───────┘                       │   Acting)      │    │  │
│  │          │                                └───────────────┘    │  │
│  │          │ Injected into contextHistory                        │  │
│  │          │                                                      │  │
│  │          │ ToolCalls                                            │  │
│  │          ▼                                                      │  │
│  │  ┌───────────────┐  Execute()  ┌───────────────┐              │  │
│  │  │  Observation   │ ◄────────── │  Registry      │              │  │
│  │  │  (Tool Result) │             │  (Tool Exec    │              │  │
│  │  └───────────────┘             │   Layer)       │              │  │
│  │                                └───────────────┘              │  │
│  └────────────────────────────────────────────────────────────────┘  │
│                                                                      │
└──────────────────────────────────────────────────────────────────────┘
```

| Component | Code Location | Responsibility |
|------|---------|------|
| `schema` | `internal/schema/message.go` | Defines core data types shared across components |
| `schema.StreamChunk` | `internal/schema/stream.go` | Provider-layer streaming delta data type |
| `LLMProvider` | `internal/provider/interface.go` | Abstracts the LLM communication layer, encapsulating API differences (blocking + streaming) |
| `OpenAIProvider` | `internal/provider/openai.go` | OpenAI-compatible API adapter (OpenAI / OpenRouter / Azure) |
| `AnthropicProvider` | `internal/provider/anthropic.go` | Anthropic-compatible API adapter (Anthropic / OpenRouter) |
| `Registry` | `internal/tools/registry.go` | Decouples tool discovery from execution |
| `AgentEngine.Run` | `internal/engine/agent_loop.go` | Blocking ReAct main loop (orchestrator + emitter definition) |
| `AgentEngine.RunStream` | `internal/engine/stream.go` | Streaming ReAct main loop, token-by-token output |
| `engine.Event` | `internal/engine/stream.go` | Client-facing streaming event type from the engine |
| `runLoop phased implementation` | `internal/engine/loop_phases.go` | Private methods for each loop phase: initialization / turn preamble / preprocessing / generation / stall detection / Observation injection / teardown |
| `With* options` | `internal/engine/options.go` | Functional options and runtime `SetSession` / `SetPlanMode` |
| `generateWithRetry` | `internal/engine/retry.go` | Dual-tier LLM retry (default budget / network-transport budget) + network error classification |
| `History load & persist` | `internal/engine/history.go` | Session history load/save, system prompt building, compaction adapter |
| `Plan Mode filtering` | `internal/engine/planmode.go` | Read-only tool whitelist, planning prefix, progress-tool & nudge detection |
| `executeTools` | `internal/engine/tools_exec.go` | Concurrent tool scheduling within a turn (semaphore + per-tool timeout) |
| `env` | `internal/env/env.go` | Environment variable configuration loading based on .env files |

## 2. ReAct Design Philosophy

ReAct (Reasoning + Acting) is the standard Agent loop pattern adopted by harness9. In each Turn, the LLM receives the current conversation context (including historical tool results) and outputs both reasoning text and a tool call request (or a final reply) at the same time.

```
Turn N:
  LLM(contextHistory, tools) → Reasoning text + ToolCalls (or plain-text final reply)
  → If ToolCalls present: execute concurrently → inject result as Observation into context → Turn N+1
  → If no ToolCalls: task complete, exit loop
```

The **emitter abstraction** decouples the loop kernel (`runLoop`) from output-side behavior, allowing the blocking mode (`Run`) and the streaming mode (`RunStream`) to share the same loop logic:

| emitter Method | Blocking Mode Behavior | Streaming Mode Behavior |
|-------------|------------|------------|
| `generate` | Calls `Generate`, prints text to stdout | Calls `GenerateStream`, sends text deltas as `EventActionDelta` |
| `toolStart` | Writes structured log | Writes log + sends `EventToolStart` |
| `toolDone` | Writes structured log | Writes log + sends `EventToolResult` |

## 3. Data Model (`internal/schema`)

### 3.1 Message Role System

```
Role (string)
├── "system"     → System prompt: defines the Agent's identity, constraints, and behavioral boundaries
├── "user"       → User input & tool execution results (Observation)
└── "assistant"  → Model output: reasoning text + tool call requests
```

Each Turn produces one assistant message (containing reasoning text and/or ToolCalls), along with zero or more user messages (one per tool result).

### 3.2 Core Type Relationships

```
┌──────────────────────────────────────────────────┐
│  Message                                         │
│  ├── Role        Role        Message author role  │
│  ├── Content     string      Plain-text content   │
│  ├── ToolCalls   []ToolCall  Tool call requests from the model │
│  └── ToolCallID  string      Links to original ToolCall's ID  │
│                                                  │
│  ToolCall                 ToolResult              │
│  ├── ID         string     ├── ToolCallID  string │
│  ├── Name       string     ├── Output      string │
│  └── Arguments  RawMessage └── IsError      bool  │
│                                                  │
│  ToolDefinition                                  │
│  ├── Name        string   Unique tool identifier  │
│  ├── Description string   Purpose description      │
│  └── InputSchema interface{} Parameter JSON Schema │
└──────────────────────────────────────────────────┘
```

**Key design decisions:**

- **`ToolCall.Arguments` uses `json.RawMessage`**: deferred deserialization, delegating argument parsing responsibility to the concrete tool implementation.
- **`ToolDefinition.InputSchema` uses `interface{}`**: different LLM SDKs require different tool parameter formats (OpenAI needs `shared.FunctionParameters`, Anthropic needs `map[string]any`); each Provider handles the type conversion internally, avoiding extra JSON round-trip serialization overhead.
- **`ToolCallID` correlation mechanism**: tool execution results (Observation) are linked to the original `ToolCall` via `ToolCallID`.
- **`ToolResult.IsError` self-healing marker**: when a tool execution fails, the engine exposes the error to the LLM, allowing it to attempt to correct the arguments and retry (Self-Healing).

### 3.3 Streaming Data Types

#### Provider Layer — `schema.StreamChunk` (`internal/schema/stream.go`)

The Provider returns a `<-chan StreamChunk` via the `GenerateStream` method; each chunk represents one incremental output from the LLM. Streaming accumulation of tool call arguments is done internally within the Provider by `toolCallAccumulator` and is not exposed as intermediate state via `StreamChunk` — the complete `Message.ToolCalls` in `StreamChunkDone` is already the final accumulated result:

```
StreamChunk
├── Type     StreamChunkType  Chunk type identifier
├── Delta    string           Text delta (valid for text_delta / thinking_delta)
├── Message  *Message         Complete response (valid when done, contains ToolCalls)
├── Usage    *Usage           Token usage (filled by Provider when done)
└── Error    string           Error message (valid when error)
```

**Chunk type lifecycle:**

```
text_delta ──────────────────────────────────────┐   (multiple, token by token)
                                                  │
thinking_delta ──────────────────────────────────┤   (multiple, reasoning content, optional)
                                                  │
                                                  ▼
                                               done  (stream ended, carries complete Message + Usage)
```

| StreamChunkType | Meaning | Carried Data |
|----------------|------|---------|
| `text_delta` | Text delta, token by token | `Delta` |
| `thinking_delta` | Reasoning delta (extended thinking / reasoning_content) | `Delta` |
| `done` | Stream ended | `Message` (complete response, contains ToolCalls), `Usage` |
| `error` | Error occurred | `Error` |

**Tool call accumulation note:** Internally, the Provider uses `toolCallAccumulators` (`internal/provider/tool_call_accumulator.go`) to concatenate the JSON fragments returned by the SDK's streaming interface into complete tool arguments, ultimately placing them all into `StreamChunkDone.Message.ToolCalls`, so upstream layers do not need to be aware of the intermediate state.

#### Engine Layer — `engine.Event` (`internal/engine/stream.go`)

The engine returns a `<-chan Event` via the `RunStream` method, converting the Provider's low-level StreamChunk into client-facing semantic events:

```
Event
├── Type EventType  Event type
├── Turn int        Current Turn number
└── Data any        Event payload (type varies with Type)
```

| EventType | Meaning | Data Type |
|-----------|------|----------|
| `action_delta` | Text delta output by the LLM (token by token) | `string` |
| `thinking_delta` | Reasoning content delta (extended thinking / reasoning) | `string` |
| `tool_start` | Tool execution starts | `schema.ToolCall` |
| `tool_result` | Tool execution completed | `ToolResultData` (contains `Result schema.ToolResult` and `Duration time.Duration`) |
| `token_update` | Emitted before each LLM call, reporting token estimate | `TokenUpdateData` |
| `compaction` | Context underwent effective compaction (token reduction > 5%) | `CompactionData` |
| `approval_required` | Tool execution requires human approval | `ApprovalRequest` |
| `done` | Loop ended normally | `nil` |
| `error` | Error occurred | `string` |

**Example event flow:**

```
Turn 1:
  token_update        ← Estimate before LLM call
  thinking_delta × N  ← Reasoning content (if the model supports extended thinking)
  action_delta × N    ← LLM token-by-token output
  token_update        ← Actual token usage (updated after LLM returns)
  approval_required   ← Waiting for human approval on a dangerous tool (optional)
  tool_start          ← Tool execution starts
  tool_result         ← Tool execution completed
Turn 2:
  token_update        ← Next round's estimate
  action_delta × N    ← Final reply (no tool call)
  done                ← Loop ended
```

## 4. Agent Loop Cycle Flow

The loop is orchestrated by the `runLoop` orchestrator (`agent_loop.go`), which dispatches to independent private methods defined in `loop_phases.go`, one per phase, all sharing a `loopContext` that aggregates the interaction state:

```
beginInteraction      Initialization: observer hookup, state snapshot, todo restore,
                      Plan Mode prefix, history loading
for {
    beginTurn          Turn counter + dual termination checks (MaxTurns / ctx cancel)
    prepareTurnInput   Tool filtering + compaction check + token estimate report + nudge injection
    generateTurn       LLM call with retry + actual usage report
    (termination check) Converge and exit when the model issues no tool calls
    executeTools       Concurrent tool scheduling (tools_exec.go)
    injectObservations Observation injection
}
saveHistory / saveTodos   Teardown: history persists only on the natural-termination path;
                          todos persist on every exit path
```

```
                     ┌─────────────────────┐
                     │   Initialize context  │
                     │   System(with WorkDir)│
                     │   + User              │
                     └──────────┬──────────┘
                                │
                ┌───────────────▼───────────────┐
                │   Turn count ++                 │
                │   Check MaxTurns / ctx.Done()  │
                └───────────────┬───────────────┘
                                │
                   ┌────────────▼────────────┐
                   │  LLM call                │
                   │  Generate(availableTools)│
                   │  → Inject into contextHistory│
                   └────────────┬────────────┘
                                │
                       ┌────────▼────────┐    Has ToolCalls
                       │  Termination check│──────────────────┐
                       │  ToolCalls == 0? │                   │
                       └────────┬────────┘                   │
                                │ No ToolCalls               │
                       ┌────────▼────────┐    ┌──────────────┴───────────┐
                       │  Task complete    │    │  ToolCall phase (concurrent)│
                       │  Exit loop        │    │  Semaphore limits concurrency│
                       └─────────────────┘    │  Each tool has independent timeout│
                                              └────────────┬─────────────┘
                                                           │
                                             ┌─────────────▼────────────┐
                                             │  Observation phase       │
                                             │  Append tool results to context│
                                             └────────────┬─────────────┘
                                                           │
                                             ┌─────────────▼────────────┐
                                             │  Back to Turn count ++   │
                                             └──────────────────────────┘
```

### 4.1 Initialization Phase

When the engine starts, it constructs the initial conversation context via `loadHistoryWith`. If a `Session` is injected, historical messages are restored from persistent storage; otherwise it contains only the system prompt and the current user input:

```go
// loadHistoryWith restores historical messages from the Session, injects the system prompt,
// and appends the user input.
// startLen marks the starting position of new messages (existing history + system prompt are not persisted),
// used by saveHistoryWith to save only msgs[startLen:].
func (e *AgentEngine) loadHistoryWith(ctx context.Context, userPrompt string, sess memory.Session, logPrefix string) ([]schema.Message, int) {
    var history []schema.Message
    if sess != nil {
        msgs, err := sess.GetMessages(ctx, 0) // 0 = return all history
        if err == nil {
            history = msgs
        }
    }
    // The system prompt is injected at the beginning of the history messages (if not already present),
    // rebuilt on every call, not persisted to the DB.
    if len(history) == 0 || history[0].Role != schema.RoleSystem {
        history = append([]schema.Message{{Role: schema.RoleSystem, Content: e.buildSystemPrompt()}}, history...)
    }
    startLen := len(history) // New messages start here; the system prompt is not counted in the persistence range
    history = append(history, schema.Message{Role: schema.RoleUser, Content: userPrompt})
    return history, startLen
}
```

**WorkDir is injected into the system prompt** so the LLM knows its working directory. The system prompt itself is not persisted (it is rebuilt and prepended to the front of the history messages on every startup, avoiding duplicate insertion); `startLen` marks the starting position of new messages, used by `saveHistoryWith` to save only `msgs[startLen:]`.

### 4.2 LLM Call Phase

Each Turn executes one LLM call, carrying the complete tool list:

```go
availableTools := e.registry.GetAvailableTools()
responseMsg, err := em.generate(ctx, turnCount, contextHistory, availableTools)
contextHistory = append(contextHistory, *responseMsg)
```

### 4.3 Termination Condition Detection

The engine implements a triple safety guarantee:

```go
// 1. MaxTurns limit: prevents infinite loops
if e.maxTurns > 0 && turnCount > e.maxTurns {
    return fmt.Errorf("maximum turn count reached (%d), loop terminated", e.maxTurns)
}

// 2. Context cancellation: supports timeout and manual interruption
select {
case <-ctx.Done():
    return fmt.Errorf("context cancelled: %w", ctx.Err())
default:
}

// 3. Natural termination: the model no longer requests tool calls
if len(responseMsg.ToolCalls) == 0 {
    break
}
```

### 4.4 ToolCall Phase — Concurrent Execution (with Independent Timeouts)

When the model requests multiple tool calls, the engine uses **goroutine + `sync.WaitGroup`** for concurrent execution. An optional semaphore (`maxConcurrentTools`) controls maximum concurrency, and **each tool has an independent timeout control**:

```go
go func(idx int, tc schema.ToolCall) {
    defer wg.Done()

    if sem != nil {
        sem <- struct{}{}
        defer func() { <-sem }()
    }

    // Independent timeout: a single tool's timeout does not affect other tools
    toolCtx := ctx
    if e.toolTimeout > 0 {
        toolCtx, cancel = context.WithTimeout(ctx, e.toolTimeout)
        defer cancel()
    }

    results[idx] = e.registry.Execute(toolCtx, tc)
}(i, toolCall)
```

**Key concurrency safety design points:**

| Issue | Solution |
|------|---------|
| Multiple goroutines writing to the same result set | Pre-allocated slice, each goroutine writes to its own position by index `idx` |
| Result order consistency | Index corresponds one-to-one with the original `ToolCalls` order |
| Single tool timeout | `context.WithTimeout` creates an independent child context for each tool |
| Closure variable capture | `idx`, `tc` passed explicitly, avoiding data races |
| Concurrency control | Buffered channel semaphore, 0 = unlimited |

### 4.5 Observation Phase

After tool execution completes, `injectObservations` appends the results to the context in their original order. Empty outputs fall back to a placeholder message (some backends reject tool_result with empty content, returning 400, and an empty Observation carries no information for the model to reason about); `IsError` is passed through so the Provider can set `tool_result.is_error` (strengthening the self-healing signal):

```go
for i, toolCall := range toolCalls {
    content := results[i].Output
    if content == "" {
        content = "[tool completed with no output]" // empty-output fallback, avoids 400s and wasted turns
    }
    history = append(history, schema.Message{
        Role:       schema.RoleUser,        // Observation is passed back with the user role
        Content:    content,
        ToolCallID: toolCall.ID,             // Links to the original request
        IsError:    results[i].IsError,      // Passes the error signal through for Provider is_error marking
    })
}
```

### 4.6 Streaming Architecture (`RunStream`)

`RunStream` is the streaming counterpart of `Run`, sharing the same `runLoop` main loop logic, outputting event by event via a Go channel. Core data flow:

```
┌─────────────┐  GenerateStream()  ┌──────────────────┐
│  LLMProvider │ ───────────────── │  chan StreamChunk  │
│  (OpenAI /   │                   │  (token-by-token   │
│   Anthropic) │                   │   delta)           │
└─────────────┘                   └────────┬─────────┘
                                            │
                                            ▼
                                   ┌──────────────────┐
                                   │  streamGenerate() │
                                   │  Reads StreamChunk│
                                   │  Forwards as Event│
                                   └────────┬─────────┘
                                            │
                                            ▼
┌─────────────┐  Execute()         ┌──────────────────┐
│  Registry    │ ─────────────────  │    chan Event     │
│  (Tool Exec) │                    │  (client-facing)  │
└─────────────┘                    └────────┬─────────┘
                                            │
                                            ▼
                                   ┌──────────────────┐
                                   │  Client consumer  │
                                   │   (TUI / CLI /    │
                                   │    SSE handler)   │
                                   └──────────────────┘
```

**The `streamGenerate` method** replaces the direct call to `Generate` used in blocking mode. It calls `GenerateStream`, reads from the `StreamChunk` channel, and forwards it as semantic `Event`s:

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

**Context cancellation awareness**: all channel sends go through `select` listening on `ctx.Done()`, ensuring that sends never block on cancellation:

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

## 5. Interface Abstraction and Decoupling Design

### 5.1 LLMProvider Interface

```go
type LLMProvider interface {
    // Blocking call: returns the complete response Message and actual token usage (Usage may be nil)
    Generate(ctx context.Context, messages []schema.Message,
             availableTools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error)

    // Streaming call: returns incremental chunks via channel; the last valid chunk type is StreamChunkDone
    GenerateStream(ctx context.Context, messages []schema.Message,
                   availableTools []schema.ToolDefinition) (<-chan schema.StreamChunk, error)
}
```

**Design philosophy:**
- The engine only depends on the interface; switching models only requires replacing the Provider implementation
- Dual-mode coexistence: `Generate` is used for blocking scenarios, `GenerateStream` for streaming scenarios
- The channel returned by `GenerateStream` closes automatically once the stream ends; the last valid chunk's Type is `StreamChunkDone`

### 5.2 Concrete Implementations

Both Providers adopt a **unified message conversion layer** architecture, where `Generate` and `GenerateStream` share the same conversion logic:

```
                    ┌──────────────────┐
                    │  convertMessages  │ ← schema.Message → native SDK message
                    │  convertTools     │ ← schema.ToolDefinition → native SDK tool
                    └───────┬──────────┘
                            │
               ┌────────────┼─────────────┐
               ▼                           ▼
        Generate()                 GenerateStream()
        SDK.New()                  SDK.NewStreaming()
        → *Message                 → chan StreamChunk
```

#### OpenAIProvider (`internal/provider/openai.go`)

An OpenAI-compatible implementation, supporting any backend that follows the OpenAI Chat Completion API spec:

| Environment Variable | Description |
|---------|------|
| `OPENAI_API_KEY` | API authentication key (required) |
| `OPENAI_BASE_URL` | API endpoint base URL, e.g. `https://api.openai.com/v1` (required) |

```go
p, err := provider.NewOpenAIProvider("gpt-4o")
```

**Message conversion rules:**

| schema Type | OpenAI SDK Type |
|-------------|----------------|
| `RoleSystem` | `openai.SystemMessage` |
| `RoleUser` (with ToolCallID) | `openai.ToolMessage(content, toolCallID)` |
| `RoleUser` (without ToolCallID) | `openai.UserMessage(content)` |
| `RoleAssistant` | `ChatCompletionAssistantMessageParam` (contains ToolCalls) |
| `ToolDefinition` | `openai.ChatCompletionFunctionTool` |

The conversion of `InputSchema`'s `interface{}` → `shared.FunctionParameters` is handled by the `convertToFunctionParameters` function: it first attempts a direct type assertion, falling back to a JSON round trip on failure.

**Streaming implementation:** `GenerateStream` uses `client.Chat.Completions.NewStreaming()`, returning `*ssestream.Stream[ChatCompletionChunk]`. Internally, `openaiToolCallAccumulator` accumulates tool call arguments.

#### AnthropicProvider (`internal/provider/anthropic.go`)

An Anthropic-compatible implementation, supporting both the Anthropic official endpoint and compatible endpoints like OpenRouter:

| Environment Variable | Description |
|---------|------|
| `ANTHROPIC_API_KEY` | API authentication key (required) |
| `ANTHROPIC_BASE_URL` | API endpoint base URL, e.g. `https://api.anthropic.com` (required) |

```go
p, err := provider.NewAnthropicProvider("claude-sonnet-4-20250514", 4096)
//                                                        model     maxTokens
```

**Anthropic API special handling:**

| Difference | Handling |
|--------|---------|
| System prompt is not in the messages array | Extracted from the `RoleSystem` message, set as `params.System` |
| ToolUseBlock's Input type | `json.Unmarshal` parses `Arguments` into `map[string]interface{}` |
| `required` field type | `extractSchemaFields` safely handles `[]interface{}` → `[]string` conversion |
| `MaxTokens` must be explicitly specified | Passed via constructor argument, defaults to 4096 |

**Streaming implementation:** `GenerateStream` uses `client.Messages.NewStreaming()`, returning `*ssestream.Stream[MessageStreamEventUnion]`. Event type mapping:

| Anthropic Event | Handling |
|----------------|------|
| `content_block_start` (type=tool_use) | → `StreamChunkToolCallStart`, records ID/Name |
| `content_block_delta` (type=text_delta) | → `StreamChunkTextDelta` |
| `content_block_delta` (type=input_json_delta) | → `StreamChunkToolCallDelta`, accumulates partial JSON |

### 5.3 Environment Configuration (`internal/env`)

The `env` package provides a zero-dependency `.env` file loader, called at program startup:

```go
env.Load(filepath.Join(workDir, ".env"))
```

| Feature | Description |
|------|------|
| System environment variables take priority | Existing environment variables are not overridden by the `.env` file |
| Silently skips missing file | Returns nil when no `.env` file exists, does not block startup |
| Supports quoted values | Automatically strips matched pairs of double or single quotes |
| Comments and blank lines | Lines starting with `#` and blank lines are skipped |

### 5.4 Registry Interface

```go
type Registry interface {
    Register(tool BaseTool) error
    GetAvailableTools() []schema.ToolDefinition
    Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult
}
```

### 5.5 Dependency Injection + Functional Options

```go
eng := engine.NewAgentEngine(p, r, workDir,
    engine.WithMaxTurns(100),
    engine.WithToolTimeout(30 * time.Second),
    engine.WithMaxConcurrentTools(4),
    engine.WithSession(sess),
    engine.WithCompactor(&memory.SlidingWindowCompactor{MaxMessages: 100}),
)
```

| Option | Type | Default | Description |
|------|------|--------|------|
| `WithMaxTurns(n)` | `int` | 50 | Maximum number of Turns per Run, 0 = unlimited |
| `WithToolTimeout(d)` | `time.Duration` | 60s | Timeout for a single tool execution, 0 = use the original context |
| `WithMaxConcurrentTools(n)` | `int` | 0 | Maximum concurrent tools within the same Turn, 0 = unlimited |
| `WithSession(s)` | `memory.Session` | nil | Injects session storage, enabling persistence of historical messages |
| `WithCompactor(c)` | `memory.Compactor` | nil | Injects a context compactor, controlling context window size |
| `WithContextWindow(n)` | `int` | 0 | Model's context window (tokens), used for TUI token usage display |
| `WithPromptBuilder(pb)` | `PromptBuilder` | nil | Custom system prompt builder; when nil, the built-in default text is used |
| `WithPlanMode(mode)` | `planning.PlanMode` | Default | Initial execution mode; can be updated at runtime via `SetPlanMode` |
| `WithTodoStore(s)` | `*planning.TodoStore` | nil | Binds a todo list, enabling cross-session todo persistence |
| `WithEngineObserver(o)` | `EngineObserver` | noopObserver | Injects a lifecycle observer (OTEL Tracing, etc.); degrades to noop if nil |
| `WithMemoryNudge(n, text)` | `int, string` | 0, "" | Injects a Long-Term Memory hint into the defensive copy every n turns, 0 disables it |

At runtime, `eng.SetSession(sess)` can switch sessions and `eng.SetPlanMode(mode)` can switch execution mode (both are concurrency-safe, using `sync.RWMutex` internally, but have no effect on a `runLoop` currently in progress).

**Dual-mode invocation:**

```go
// Blocking: synchronously waits for the complete result
err := eng.Run(ctx, prompt)

// Streaming: returns event by event via channel
stream, err := eng.RunStream(ctx, prompt)
for evt := range stream {
    switch evt.Type {
    case engine.EventActionDelta:
        fmt.Print(evt.Data.(string))  // Token-by-token output
    case engine.EventDone:
        // Loop ended
    }
}
```

Both modes share the same `AgentEngine` instance and configuration, and can be freely chosen at runtime.

## 6. Logging and Observability

### 6.1 EngineObserver Interface

`EngineObserver` is the sole extension interface the engine provides for the observability layer. `runLoop` calls it back at 4 lifecycle points:

```go
type EngineObserver interface {
    OnInteractionStart(ctx, sessionID, prompt) context.Context  // runLoop entry point
    OnInteractionEnd(ctx, turns, err)                           // runLoop exit (guaranteed via defer)
    OnTurnStart(ctx, turn) context.Context                      // Start of each Turn
    OnTurnEnd(ctx, turn, hasToolCalls)                          // End of each Turn
}
```

All `OnXxxStart` methods return an enhanced ctx (which may carry an OTEL Span), for the LLM call and tool execution to inherit the parent chain from.
When not injected, it automatically degrades to the zero-overhead `noopObserver`.

**Notes for custom Observers**: when implementing `OnInteractionStart` / `OnTurnStart`, in addition to storing the Span into a custom key via `context.WithValue` (for `OnInteractionEnd` / `OnTurnEnd` to retrieve), you **must also** write the Span into the OTEL standard slot via `trace.ContextWithSpan(ctx, span)` — otherwise the downstream `tracer.Start(ctx, ...)` cannot find the parent node, causing every Span to independently become a root node:

```go
func (o *MyObserver) OnInteractionStart(ctx context.Context, sessionID, prompt string) context.Context {
    ctx, span := o.tracer.Start(ctx, "my.interaction")
    // ① Write into the OTEL standard slot — downstream tracer.Start auto-nests
    ctx = trace.ContextWithSpan(ctx, span)
    // ② Write into a custom key — for OnInteractionEnd to retrieve
    return context.WithValue(ctx, mySpanKey{}, span)
}
```

### 6.2 Structured Logging

The engine uses a structured logging format, with the `[engine]` prefix for blocking mode and the `[engine-stream]` prefix for streaming mode:

**Blocking mode log example:**

```
[engine] started | workdir=/Users/zsa/project maxTurns=50 toolTimeout=1m0s maxConcurrent=0
[engine] ======== Turn 1 ======== | history=2  tools=3
[engine] tool started | name=bash id=call_123
[engine] tool completed | name=bash bytes=45
[engine] Turn 1 | Observation injection complete | history=4 | llm=1.2s tools=0.3s turn=1.5s
[engine] ======== Turn 2 ======== | history=4  tools=3
[engine] Turn 2 | task complete | llm=0.8s total=2.3s
[engine] loop ended | totalTurns=2 | total_time=2.3s
```

**Log layering:**

| Layer | Prefix | Content | Output Method |
|------|------|------|---------|
| Engine internal (blocking) | `[engine]` | Turn counting, tool status | `log.Printf` (stderr) |
| Engine internal (streaming) | `[engine-stream]` | Same as above | `log.Printf` (stderr) |
| Model output (blocking) | `[assistant]` | Text content produced by the LLM | `fmt.Printf` (stdout) |
| Model output (streaming) | No prefix | Handed to the client via Event channel | Controlled by the consumer |

## 7. Complete Data Flow Diagram

Taking a two-turn conversation as an example:

```
Turn 1:
  [Context]
    system:    "You are harness9... working directory is: /test"
    user:      "I want to travel to Beijing today, can you check if the weather is suitable?"

  LLM call: → Generate(ctx, history, [get_weather])
    assistant: "Let me check the weather in Beijing."
               + ToolCall{id:"call_abc", name:"get_weather", args:{"city":"Beijing"}}
    → Injected into contextHistory

  ToolCall: → Registry.Execute(get_weather, {"city":"Beijing"})
    ToolResult{id:"call_abc", output:"Sunny today, low of 14 degrees..."}

  Observation: user: "Sunny today, low of 14 degrees..." (toolCallID:"call_abc")

Turn 2:
  [Context = 4 messages: system, user, assistant(+ToolCalls), user(obs)]

  LLM call: → Generate(ctx, history, [get_weather])
    assistant: "The weather in Beijing looks great today, perfect for a trip!" (no ToolCall)
    → Injected into contextHistory

  → Termination condition met, loop exits
```

### 7.1 Streaming Mode Data Flow

Taking the same task under streaming mode (`RunStream`) as an example, the client receives increments via the Event channel:

```
Turn 1:
  streamGenerate() → GenerateStream(ctx, history, [get_weather])
    Event{action_delta, "Let"}           ← token by token
    Event{action_delta, "me"}
    Event{action_delta, "check the weather in Beijing."}
    Event{tool_start, ToolCall{name:"get_weather", id:"call_abc"}}

  executeTools() → Concurrent tool execution
    Event{tool_result, ToolResult{output:"Sunny today, low of 14 degrees..."}}

Turn 2:
  streamGenerate() → GenerateStream(ctx, history, [get_weather])
    Event{action_delta, "The weather in Beijing looks great today"}   ← token by token
    Event{action_delta, ", perfect for a trip!"}
    Event{done}                              ← loop ended
```

## 8. Provider Implementation Comparison

| Dimension | OpenAIProvider | AnthropicProvider |
|------|---------------|------------------|
| API Protocol | Chat Completion | Messages |
| System prompt | As a system message in the messages array | As an independent `params.System` parameter |
| Tool call response | `ToolCalls[].Function.Arguments` (JSON string) | `Input` of the `tool_use` block within `Content[]` (structured object) |
| Historical tool call | `ChatCompletionMessageFunctionToolCallParam` | `ToolUseBlockParam` |
| Tool result passback | `openai.ToolMessage(content, toolCallID)` | `anthropic.NewToolResultBlock(toolCallID, content, isError)` |
| InputSchema conversion | `convertToFunctionParameters` → `shared.FunctionParameters` | `extractSchemaFields` → `properties` + `required` |
| MaxTokens | Not required explicitly | Must be passed explicitly |
| Constructor | `NewOpenAIProvider(model) (*OpenAIProvider, error)` | `NewAnthropicProvider(model, maxTokens) (*AnthropicProvider, error)` |
| Streaming SDK method | `client.Chat.Completions.NewStreaming()` | `client.Messages.NewStreaming()` |
| Streaming chunk type | `ChatCompletionChunk` | `MessageStreamEventUnion` |
| Streaming text delta | `Choices[0].Delta.Content` | `content_block_delta` + `text_delta` |
| Streaming tool delta | `Choices[0].Delta.ToolCalls[]` | `content_block_start(tool_use)` + `input_json_delta` |

Both Providers' message conversion logic is factored out into `convertMessages` / `convertTools` methods, with `Generate` and `GenerateStream` sharing the same conversion logic. The mapping from `schema.Message` to native SDK parameters is encapsulated inside the Provider; the engine layer does not need to be aware of the API differences.

## 9. Known Limitations and Future Evolution

| Limitation | Current Status | Direction of Evolution |
|------|---------|---------|
| **Context window control** | Implemented: `SummarizationCompactor` (default, LLM summarization + incremental update), `TokenBudgetCompactor` (fallback), `SlidingWindowCompactor` (message-count window) | Further optimize summary quality; support custom summary templates |
| **Session history persistence** | Implemented: SQLiteSession (WAL mode, `~/.harness9/sessions.db`) + TodoStore cross-session persistence | Multi-working-directory isolation; session tagging and search (FTS5) |
| **Streaming output** | Implemented: `RunStream` + `GenerateStream`, supporting token-by-token deltas + EventTokenUpdate/EventCompaction | Extend to an SSE HTTP endpoint, connecting to external real-time push channels |
| **Planning** | Implemented: Plan Mode + TodoStore + auto-continuation + stagnation detection | PlanModeAutoEdit for step-by-step confirmed edit mode |
| **Permission control** | Plan Mode provides tool-layer read-only constraints | Unified PermissionChecker before tool execution, supporting interactive confirmation |
| **Hook system** | None | PreToolUse / PostToolUse / Stop / TurnComplete event hooks |
| **Multi-Agent orchestration** | Single-Agent mode | Sub-Agent scheduling, parallel Agents, dedicated role Agents |

## 10. Summary of Design Principles

| Principle | Manifestation |
|------|------|
| **Standard ReAct** | Reasoning + Acting + Observation, one LLM call per Turn |
| **emitter decoupling** | Loop kernel decoupled from output-side behavior; blocking / streaming share the same `runLoop` |
| **Interface isolation** | `LLMProvider` and `Registry` each handle their own responsibilities; the engine depends only on abstractions |
| **Dual-mode coexistence** | `Run` (blocking) and `RunStream` (streaming) share the engine configuration, chosen freely at runtime |
| **Channel-driven streaming** | Provider → `chan StreamChunk` → Engine → `chan Event`, native Go CSP model |
| **Functional options** | `WithMaxTurns` / `WithToolTimeout` / `WithMaxConcurrentTools` optional configuration |
| **Concurrency safety** | Index-isolated writes + WaitGroup + semaphore throttling + explicit parameter passing, no data races |
| **Triple-guaranteed termination** | Natural termination + MaxTurns limit + Context cancellation |
| **Observability** | Structured logging with `[engine]` / `[engine-stream]` prefixes + key=value format |
| **Deferred parsing** | `json.RawMessage` used for deferred Arguments deserialization; `interface{}` used for InputSchema compatibility across multiple SDKs |
| **Self-healing capability** | `ToolResult.IsError` allows the model to perceive errors and automatically retry |

## PromptBuilder and Skills Integration

Since the `context-engineering` branch, the system prompt in `runLoop` is no longer hardcoded,
but is dynamically constructed via the `PromptBuilder` interface:

```go
type PromptBuilder interface {
    Build() string
}
```

The `WithPromptBuilder(pb PromptBuilder)` Option injects the builder into the engine.
When not set, it falls back to the built-in default text (backward compatible).

The `internal/context.DefaultPromptBuilder` implementation assembles the prompt in the following order:

1. harness9 base prompt (role definition + workDir)
2. `workdir/AGENTS.md` (skipped if it does not exist)
3. Skills index summary (from `internal/skills.Index.Summary()`)

The full content of Skills is loaded on demand via the `use_skill` tool (Progressive Disclosure),
which does not affect the execution logic of the base ReAct loop.
