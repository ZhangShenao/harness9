---
title: "Agent Loop: A Production-Grade ReAct Loop in 500 Lines of Go"
date: 2026-05-28
tags: [harness9, agent, golang, react, concurrency]
summary: "harness9's AgentLoop implements a production-ready ReAct main loop in under 500 lines of Go: an explicit for loop, goroutine-based concurrent tool execution, a triple-layer termination guarantee, and an emitter abstraction that decouples dual run modes. This post breaks down every key architectural decision."
---

# Agent Loop: A Production-Grade ReAct Loop in 500 Lines of Go

## About harness9

harness9 is a lightweight, feature-complete, production-ready Agent Harness framework written in Go.

- **Website**: [https://zhangshenao.github.io/harness9/](https://zhangshenao.github.io/harness9/)
- **GitHub**: [https://github.com/ZhangShenao/harness9](https://github.com/ZhangShenao/harness9)

⭐ Stars are the most direct way to support open-source work — issues and PRs are welcome.

---

## TL;DR

harness9's AgentLoop lives in the `internal/engine/` directory and comes in at under 500 lines of Go. It implements the standard Reasoning-and-Acting loop (ReAct Loop) with a single explicit `for` loop, executes tools concurrently via goroutines, decouples blocking and streaming modes through an `emitter` abstraction, and guarantees loop termination through three independent conditions. No graph engine, no third-party loop framework — control of the loop stays entirely inside the framework.

---

## Why Not a Graph Engine

Before diving into the implementation, it's worth answering a more fundamental question: why did harness9 choose an explicit loop over the more "modern" graph-orchestration approach?

Looking across the seven frameworks surveyed, the choices fall into three camps:

- **Delegated**: OpenCode and OpenClaw hand loop control to Vercel AI SDK's `streamText`, bounding turns via a `maxSteps` parameter. This means less code, but the loop logic becomes a black box — there's no way to inject custom logic between turns.
- **Graph-orchestrated**: DeepAgents delegates to LangGraph's `StateGraph`, controlling depth via `recursion_limit: 9999`. The graph model brings powerful branching and concurrency capabilities, but also pulls in a graph-engine dependency and a steeper mental model.
- **Explicit loop**: OpenHarness, HermesAgent, and the OpenAI Agent SDK all use an explicit `while True` loop, giving full control over every iteration's execution path.

harness9 chose the explicit loop for a straightforward reason: **an Agent Loop is fundamentally a stateful iterative process, and a loop expresses that more naturally than a graph does**. Each round's logic is linear — call the LLM, check for termination, execute tools, inject the observation — this isn't a problem that needs a graph to model. Introducing a graph engine would just turn 200 lines of clear code into yet another framework you have to learn.

Go's `for` loop plus `goroutine` is already enough.

---

## The Structure of the ReAct Main Loop

![Diagram: full ReAct main loop control flow](/blog/agent-loop/images/react-loop-control-flow-01.png)



`runLoop` is the shared loop kernel used by both `Run` and `RunStream`, located in `internal/engine/agent_loop.go`. Its overall structure:

```go
func (e *AgentEngine) runLoop(ctx context.Context, userPrompt string, logPrefix string, em emitter) error {
    // 1. Snapshot configuration (avoids data races with the TUI goroutine)
    e.mu.RLock()
    sess, comp, planMode, todoStore := e.session, e.compactor, e.planMode, e.todoStore
    e.mu.RUnlock()

    contextHistory, startLen := e.loadHistoryWith(ctx, userPrompt, sess)
    defer func() { /* persist the TodoStore */ }()

    turnCount := 0
    for {
        turnCount++

        // Triple-layer termination guarantee
        if e.maxTurns > 0 && turnCount > e.maxTurns { return ... }
        select { case <-ctx.Done(): return ...; default: }

        // Compaction + token estimation + LLM call
        compactedHistory := e.applyCompactionWith(comp, contextHistory)
        responseMsg, usage, err := em.generate(ctx, turnCount, compactedHistory, availableTools)
        contextHistory = append(contextHistory, *responseMsg)

        // Natural termination
        if len(responseMsg.ToolCalls) == 0 { break }

        // Concurrent tool execution
        results := e.executeTools(ctx, turnCount, responseMsg.ToolCalls, logPrefix, em)

        // Inject observations
        for i, toolCall := range responseMsg.ToolCalls {
            contextHistory = append(contextHistory, schema.Message{
                Role: schema.RoleUser, Content: results[i].Output, ToolCallID: toolCall.ID,
            })
        }
    }

    e.saveHistoryWith(ctx, sess, contextHistory, startLen)
    return nil
}
```

The structure of the loop body directly mirrors ReAct semantics: reasoning (Think) is done by the LLM, action (Act) is carried out by tools, and observation (Observe) is injected into the context — after which the next round of reasoning begins.

---

## Context Initialization: The System Prompt Is Never Persisted

Before entering the loop, `loadHistoryWith` does two things: restores history from the `Session`, and injects the system prompt.

```go
func (e *AgentEngine) loadHistoryWith(ctx context.Context, userPrompt string, sess memory.Session) ([]schema.Message, int) {
    var history []schema.Message
    if sess != nil {
        msgs, _ := sess.GetMessages(ctx, 0)
        history = msgs
    }
    // System prompt is never persisted to the DB — it's re-injected on every call
    if len(history) == 0 || history[0].Role != schema.RoleSystem {
        history = append([]schema.Message{{Role: schema.RoleSystem, Content: e.buildSystemPrompt()}}, history...)
    }
    startLen := len(history)
    history = append(history, schema.Message{Role: schema.RoleUser, Content: userPrompt})
    return history, startLen
}
```

`startLen` marks "the position where messages newly added by this Run begin." When `saveHistoryWith` is called, it only persists `msgs[startLen:]` — the system prompt is never written to the database. This decision avoids the problem of repeatedly persisting a stale system prompt: across sessions the system prompt can change (working directory changes, Skills updates, etc.), so rebuilding it from scratch every time is the only way to guarantee correctness.

---

## Triple-Layer Termination Guarantee

An infinite loop is one of the most common failure modes in agent systems. harness9 prevents it with three layers of protection:

![Diagram: triple-layer termination guarantee mechanism](/blog/agent-loop/images/triple-termination-guard-02.png)



**Layer 1: Natural termination.** When the model stops issuing tool calls, the task is done, and the loop `break`s on its own:

```go
if len(responseMsg.ToolCalls) == 0 {
    break
}
```

**Layer 2: The MaxTurns safety valve.** Defaults to 50 turns, guarding against the model getting stuck in a tool-calling loop:

```go
if e.maxTurns > 0 && turnCount > e.maxTurns {
    return fmt.Errorf("reached max turn count (%d), loop terminated", e.maxTurns)
}
```

**Layer 3: Context cancellation.** Checked at the start of every round, supporting both timeout control and manual interruption:

```go
select {
case <-ctx.Done():
    return fmt.Errorf("context cancelled: %w", ctx.Err())
default:
}
```

Note the order of the checks: context cancellation is checked *after* the MaxTurns check. When `turnCount` exceeds the limit, the function returns immediately without paying the cost of checking `ctx.Done()`. This is a small but deliberate ordering choice — MaxTurns triggers more often than external cancellation in practice.

The test cases `TestMaxTurnsLimit` and `TestContextCancellation` directly exercise these two paths:

```go
// MaxTurns safety valve: when the LLM keeps issuing tool calls, force exit once the limit is hit
eng := NewAgentEngine(p, r, "/test", WithMaxTurns(2))
err := eng.Run(context.Background(), "loop forever")
// err contains "max turn count"

// Context cancellation: cancel immediately before calling
ctx, cancel := context.WithCancel(context.Background())
cancel()
err = eng.Run(ctx, "cancelled task")
// err contains "context cancelled"
```

---

## Concurrent Tool Execution: Goroutines + Pre-Allocated Slices

When the model issues multiple tool calls in a single round, running them sequentially wastes time. harness9 executes all tools for that round concurrently using goroutines.

![Diagram: concurrent tool execution architecture](/blog/agent-loop/images/concurrent-tool-execution-03.png)



The implementation of `executeTools` is tight and race-free:

```go
func (e *AgentEngine) executeTools(ctx context.Context, turn int, toolCalls []schema.ToolCall, logPrefix string, em emitter) []schema.ToolResult {
    results := make([]schema.ToolResult, len(toolCalls)) // pre-allocated, written by index
    var wg sync.WaitGroup

    var sem chan struct{}
    if e.maxConcurrentTools > 0 {
        sem = make(chan struct{}, e.maxConcurrentTools) // semaphore limiting concurrency
    }

    for i, toolCall := range toolCalls {
        wg.Add(1)
        go func(idx int, tc schema.ToolCall) {
            defer wg.Done()

            if sem != nil {
                sem <- struct{}{}
                defer func() { <-sem }()
            }

            toolCtx := ctx
            var cancel context.CancelFunc
            if e.toolTimeout > 0 {
                toolCtx, cancel = context.WithTimeout(ctx, e.toolTimeout)
                defer cancel()
            }

            em.toolStart(turn, tc)
            start := time.Now()
            results[idx] = e.registry.Execute(toolCtx, tc) // write to its own index
            em.toolDone(turn, tc, results[idx], time.Since(start))
        }(i, toolCall)
    }

    wg.Wait()
    return results
}
```

A few key design decisions:

**Pre-allocated slice with index-based writes, instead of collecting results over a channel.** The size of the result set is known before the loop starts (`len(toolCalls)`), so no dynamic growth is needed. Each goroutine writes to its own `results[idx]`, which is inherently lock-free — there's no shared-memory conflict between different indices. This is simpler than collecting results over a `channel` and sorting them afterward, and it avoids the sorting overhead entirely.

**`idx` and `tc` are passed explicitly, not captured by closure.** Go's range variables are reused across loop iterations; if a closure captured `i` and `toolCall` directly, all goroutines could end up reading the same final value. Passing them as `go func(idx int, tc schema.ToolCall)` arguments guarantees each goroutine gets its own copy at compile time.

**The semaphore is a buffered channel.** `make(chan struct{}, maxConcurrentTools)` is Go's idiomatic semaphore implementation. A value of 0 means no limit (`sem` is nil), skipping the acquire/release logic entirely with zero extra overhead.

**Each tool gets its own `context.WithTimeout`.** `toolCtx` is derived from the parent `ctx`, so a single tool timing out doesn't affect the execution of other tools within the same turn. A timed-out tool returns a result with `IsError: true`, and the LLM can retry once it receives the observation.

---

## Self-Healing: Errors Are Observations, Not Exceptions

The traditional error-handling philosophy is: a tool failure means execution stops and the error propagates upward. harness9 takes a completely different approach: **the result of a failed tool execution is passed back to the LLM as-is, as an observation**.

`ToolResult.IsError` is the key field in this mechanism:

```go
type ToolResult struct {
    ToolCallID string `json:"tool_call_id"`
    Output     string `json:"output"`   // the error message, on failure
    IsError    bool   `json:"is_error"` // marks execution failure
}
```

When `Registry.Execute` returns a result with `IsError: true`, `runLoop` doesn't return an error — it injects the result into the context as an ordinary observation:

```go
for i, toolCall := range responseMsg.ToolCalls {
    contextHistory = append(contextHistory, schema.Message{
        Role:       schema.RoleUser,
        Content:    results[i].Output, // "command not found" or some other error message
        ToolCallID: toolCall.ID,
    })
}
```

On the next round, the LLM receives the context containing the error message and can diagnose the cause and retry with corrected arguments. The `TestToolErrorResult` test verifies this chain:

```go
// errorRegistry returns a result with IsError: true for any tool call
r := &errorRegistry{} // output: "command not found", IsError: true
eng := NewAgentEngine(p, r, "/test")
err := eng.Run(context.Background(), "test error")
// no error is returned — the error is passed to the next round's LLM as an observation

// Verification: the last message the second-round LLM receives contains the error output
lastMsg := p.calls[1].messages[len(p.calls[1].messages)-1]
// lastMsg.Content contains "command not found"
```

The cost of this design: if the LLM can't fix the problem, the loop keeps going until MaxTurns triggers. The benefit: many tool-call failures can recover automatically, without human intervention.

---

## The emitter Abstraction: One Loop, Two Outputs

One of harness9's core design decisions is that **blocking mode (`Run`) and streaming mode (`RunStream`) share the exact same `runLoop` implementation**. The difference between them is entirely encapsulated in the `emitter` struct.

![Diagram: emitter architecture decoupling the dual run modes](/blog/agent-loop/images/emitter-dual-mode-04.png)



The definition of `emitter` makes the two modes' differences explicit:

```go
type emitter struct {
    generate    func(ctx context.Context, turn int, history []schema.Message, tools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error)
    toolStart   func(turn int, tc schema.ToolCall)
    toolDone    func(turn int, tc schema.ToolCall, result schema.ToolResult, d time.Duration)
    tokenUpdate func(tokens, window int)
    compaction  func(data CompactionData)
    approval    hooks.ApprovalFunc // Human-in-the-Loop approval callback
}
```

The `emitter` built by `Run` calls the blocking `provider.Generate` in its `generate` field, printing text to stdout:

```go
em := emitter{
    generate: func(ctx context.Context, _ int, history []schema.Message, tools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
        msg, usage, err := e.provider.Generate(ctx, history, tools)
        if msg.Content != "" {
            fmt.Printf("[assistant] %s\n", msg.Content)
        }
        return msg, usage, nil
    },
    toolStart: func(turn int, tc schema.ToolCall) {
        log.Print(logfmt.FormatToolStart("engine", turn, tc))
    },
    // ...
}
```

The `emitter` built by `RunStream` calls `streamGenerate` in its `generate` field, forwarding text deltas as `EventActionDelta` events over a channel:

```go
em := emitter{
    generate: func(ctx context.Context, turn int, history []schema.Message, tools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
        return e.streamGenerate(ctx, ch, turn, history, tools)
    },
    toolStart: func(turn int, tc schema.ToolCall) {
        log.Print(logfmt.FormatToolStart("engine-stream", turn, tc))
        sendEvent(ctx, ch, Event{Type: EventToolStart, Turn: turn, Data: tc})
    },
    // ...
}
```

`runLoop` itself has no idea how output works — it only ever calls `em.generate`, `em.toolStart`, and `em.toolDone`. This is a direct application of the Strategy Pattern, but lighter weight than defining an interface — using function fields instead of an interface eliminates the type-assertion and method-set overhead.

---

## Streaming Architecture: Two Layers of Channels

`RunStream`'s data flow goes through two layers of channel conversion:

![Diagram: two-layer channel streaming data flow](/blog/agent-loop/images/stream-two-layer-channel-05.png)



**Layer one**: `provider.GenerateStream` returns a `<-chan StreamChunk` — the Provider layer's delta protocol, carrying token-level text deltas and tool-call argument deltas.

**Layer two**: `streamGenerate` consumes the `StreamChunk` channel and converts it into client-facing semantic `Event`s, sent over a `chan Event`:

```go
func (e *AgentEngine) streamGenerate(ctx context.Context, ch chan<- Event, turn int,
    history []schema.Message, tools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {

    stream, err := e.provider.GenerateStream(ctx, history, tools)
    // ...
    for chunk := range stream {
        switch chunk.Type {
        case schema.StreamChunkTextDelta:
            if !sendEvent(ctx, ch, Event{Type: EventActionDelta, Turn: turn, Data: chunk.Delta}) {
                return nil, nil, ctx.Err()
            }
        case schema.StreamChunkThinkingDelta:
            if !sendEvent(ctx, ch, Event{Type: EventThinkingDelta, Turn: turn, Data: chunk.Delta}) {
                return nil, nil, ctx.Err()
            }
        case schema.StreamChunkDone:
            msg = chunk.Message
            usage = chunk.Usage
        }
    }
    return msg, usage, nil
}
```

`sendEvent` is a key helper function that uses `select` to simultaneously watch the channel send and `ctx.Done()`:

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

Without this `select`, if the consumer (the TUI) stops reading from the channel, the producer goroutine would block forever, leaking a goroutine.

`RunStream` itself runs `runLoop` in a dedicated goroutine, and the channel closes automatically when that goroutine exits:

```go
func (e *AgentEngine) RunStream(ctx context.Context, userPrompt string) (<-chan Event, error) {
    ch := make(chan Event)
    go func() {
        defer close(ch)
        // ...
        if err := e.runLoop(ctx, userPrompt, "engine-stream", em); err != nil {
            ch <- Event{Type: EventError, Data: err.Error()}
            return
        }
        ch <- Event{Type: EventDone}
    }()
    return ch, nil
}
```

Note that the termination events (`EventDone` / `EventError`) use a direct `ch <-` rather than `sendEvent`. This is intentional — termination events must reach the consumer and must not be dropped due to context cancellation.

---

## Engine State Management: RWMutex Snapshots

`AgentEngine` is stateful — it holds fields like `Session`, `Compactor`, and `PlanMode` that can be updated at runtime. In the TUI, users can switch sessions via `/new` and `/resume` commands, and toggle Plan Mode with `Shift+Tab`.

This creates a concurrency problem: `SetSession` and `SetPlanMode` may be called by the TUI goroutine while `runLoop` is in the middle of executing.

The solution is to take a **snapshot** at the entry point of `runLoop`:

```go
e.mu.RLock()
sess := e.session
comp := e.compactor
planMode := e.planMode
todoStore := e.todoStore
e.mu.RUnlock()
```

From that point on, the entire loop uses the local variables `sess`, `comp`, and `planMode`, and never touches `e`'s fields again. Changes made by `SetSession` have no effect on a `runLoop` that's already running — they only take effect on the next call.

What makes this design clean: **the lock is held for an extremely short time** (just a few microseconds to read the fields), so it never contends with the LLM calls inside the loop, which can take seconds or even tens of seconds.

---

## Plan Mode: A Hard Constraint at the Tool Layer

harness9's Plan Mode enforces a read-only constraint at the tool layer, not as a soft constraint at the prompt layer.

```go
var planModeWhitelist = map[string]bool{
    "read_file":  true,
    "bash":       true,
    "use_skill":  true,
    "todo_write": true,
}

func filterReadOnlyTools(tools []schema.ToolDefinition) []schema.ToolDefinition {
    var result []schema.ToolDefinition
    for _, t := range tools {
        if planModeWhitelist[t.Name] {
            result = append(result, t)
        }
    }
    return result
}
```

In Plan Mode, `write_file` and `edit_file` are removed from the tool list passed to the LLM. The LLM has no way to call either tool under any circumstances — not because the prompt tells it not to, but because it can't even see their definitions.

This distinction matters. A soft constraint at the prompt layer relies on the model following instructions, and reliability degrades as the conversation grows longer and the context more complex. A hard constraint at the tool layer is structural and independent of model behavior.

The test `TestRunLoop_PlanMode_FiltersWriteTools` verifies this constraint: `write_file` and `edit_file` disappear from the tool list the LLM receives, while `read_file`, `bash`, and `todo_write` remain.

---


## Data Model: Lazily Parsed `RawMessage`

The `schema` package defines harness9's message contract. One design worth calling out: `ToolCall.Arguments` uses `json.RawMessage`:

```go
type ToolCall struct {
    ID        string          `json:"id"`
    Name      string          `json:"name"`
    Arguments json.RawMessage `json:"arguments"` // deferred deserialization
}
```

The tool-call arguments returned by the LLM are JSON, but each tool has its own argument structure. Using `json.RawMessage` for deferred deserialization means the engine layer never needs to know any tool's argument structure — that's the responsibility of the specific tool implementation. The engine only passes the data along; the tool parses it itself.

`ToolDefinition.InputSchema` similarly uses the `any` type:

```go
type ToolDefinition struct {
    Name        string `json:"name"`
    Description string `json:"description"`
    InputSchema any    `json:"input_schema"` // compatible with different SDKs' parameter formats
}
```

The OpenAI SDK needs `shared.FunctionParameters`, while the Anthropic SDK needs `map[string]any`. Using `any` lets each Provider handle its own type conversion in its adapter layer, with the engine layer none the wiser.

The shared principle behind both designs: **push parsing responsibility to the layer closest to the consumer**.

---

## Observability: Structured Logging and Token Awareness

Before each round of the loop executes, the engine estimates the token usage of the current context, then updates it with the actual value after the LLM call:

```go
// Estimated value (before the call)
msgTokensAfter := memory.EstimateTokens(compactedHistory)
toolTokens := memory.EstimateToolTokens(availableTools)
em.tokenUpdate(msgTokensAfter + toolTokens, e.contextWindow)

// Actual value (after the call, extracted from the API response's usage field)
if usage != nil && usage.InputTokens > 0 {
    em.tokenUpdate(usage.InputTokens, e.contextWindow)
}
```

The TUI status bar computes utilization from `contextWindow` and changes color as a warning when approaching the limit. This two-step update (estimate first, then correct) ensures the TUI can show progress while the LLM call is still in flight, rather than waiting for the call to finish before updating.

Blocking mode and streaming mode use different log prefixes, `[engine]` and `[engine-stream]`, making it easy to tell log sources apart when both are used together.

---

## Summary of Design Trade-offs

harness9's Agent Loop design deliberately gives up a few things:

**Giving up graph orchestration in favor of clarity.** LangGraph offers powerful graph-orchestration capabilities, but an agent's single-threaded reasoning path is expressed more intuitively with a `for` loop. The graph model's strength is in multi-agent orchestration; in a single-agent scenario, it brings complexity rather than capability.

**Giving up an external loop framework in favor of retaining control.** Passing `maxSteps` to the Vercel AI SDK saves a few dozen lines of code, but loses the ability to inject custom logic between rounds (compaction checks, Plan Mode filtering, token accounting).

**Giving up path-conflict detection in favor of unconditional concurrency.** HermesAgent performs path-conflict detection before executing tools concurrently (reads and writes to the same file can't run in parallel). harness9 doesn't do this check — all tools run concurrently, unconditionally. The reason is that harness9 implements a per-path read-write lock in `tools/path_locker.go`, resolving concurrency conflicts at the execution layer instead of detecting them ahead of time at the scheduling layer.

The common logic behind these trade-offs is: **solve the problem with less code, at the right layer**.

---

## Closing Thoughts

harness9's Agent Loop has no tricks — just a clear separation of responsibilities: `emitter` encapsulates output differences, `executeTools` encapsulates concurrent execution, `loadHistoryWith` encapsulates context restoration, and `runLoop` does nothing but scheduling.

One question to leave you with: when an LLM calls `write_file` to write to file A and `read_file` to read that same file A within a single round, could concurrent execution produce a race condition? How does harness9's `path_locker.go` handle this?

The answer is in `internal/tools/path_locker.go`.
