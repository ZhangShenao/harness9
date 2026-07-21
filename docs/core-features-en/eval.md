# Test · Eval · Observability System

harness9's quality assurance system consists of three mutually independent but cooperative subsystems, together answering one core question: **is this Agent really working correctly?**

```
Development stage ──→ Test (deterministic testing)      ScriptedProvider + Assertion
CI stage           ──→ Eval (golden dataset evaluation)  16 cases + Quality Gate
Production stage   ──→ Observability (tracing)           OTEL Traces + Metrics → Langfuse
```

---

## I. Design Philosophy

### 1.1 Why does an Agent need a dedicated testing system?

Traditional software unit testing assumes: given the same input, you always get the same output. Agent systems break this assumption — LLM output is inherently non-deterministic, and the same prompt can produce completely different behavior paths across different sessions.

| Challenge | Why traditional testing fails | harness9's solution |
|------|-----------------|----------------|
| **Non-determinism** | Real LLM behavior cannot be mocked | `ScriptedProvider` scripts the behavior, making tests deterministic and repeatable |
| **Behavior verification** | Asserting return values isn't enough; you need to verify "what was done" | `recordingHook` + `Assertion` framework verifies the tool call trace |
| **Performance regression** | Without a baseline, regressions go undetected | Golden dataset + CI Quality Gate, automatically compared on every PR |
| **Production visibility** | No tooling to see what the Agent is actually doing | OTEL tracing visualizes every LLM call and tool execution |

### 1.2 Three-layer pyramid model

```
          ┌──────────────────────────────────────┐
          │ Observability                        │  ← Production: OTEL Traces + Metrics
          │ See what the Agent is doing          │    Integrates with Langfuse / Grafana / Jaeger
          └───────────────────┬──────────────────┘
                             │
          ┌───────────────────▼──────────────────┐
          │ Eval                                 │  ← CI/CD: golden dataset Quality Gate
          │ Quantify Agent capability boundaries │    16 cases, PR-triggered, failure blocks merge
          └───────────────────┬──────────────────┘
                             │
          ┌───────────────────▼──────────────────┐
          │ Test                                 │  ← Development stage: ScriptedProvider + Assertion
          │ Verify correctness of Agent behavior │    Deterministic, hermetic isolation, no API Key dependency
          └──────────────────────────────────────┘
```

### 1.3 Non-invasive design principle

harness9's core engine (engine / provider / hooks) has no awareness of any test or observability logic. All capabilities are seamlessly plugged in through three existing extension points:

```
┌─────────────────────────────────────────────────────────────┐
│                     AgentEngine (core engine)                 │
│  EngineObserver ← the only new interface (4 lifecycle callbacks) [Access point 1] │
└──────────────────────────────────────────────────────────────┘
         ↑ Injected via WithEngineObserver
         │
┌────────┴──────────┐   ┌──────────────────────────────────────┐
│ OTELEngineObserver│   │ TracingProvider [Access point 2]       │
│ Interaction Span  │   │ Wraps LLMProvider, LLM Request Span    │
│ Turn Span         │   │ + Token Metrics + Input/Output reporting│
└───────────────────┘   └──────────────────────────────────────┘
                                      ↑ Replaces the original provider
┌──────────────────────────────────────────────────────────────┐
│ ObservabilityHook [Access point 3]                             │
│ Implements ToolHook, Tool Execution Span + Tool Metrics         │
│ Registered at the end of HookRegistry (pure observation, does not │
│ interfere with tool decisions)                                  │
└──────────────────────────────────────────────────────────────┘
```

---

## II. Test Subsystem

### 2.1 Architecture Overview

```
internal/evals/
├── provider.go       ScriptedProvider — deterministic LLM mock
├── assertions.go     Assertion interface + Case/Result types + 8 assertions
├── harness.go        RunCase / Suite / recordingHook
├── testenv.go        SetupHermeticEnv — standard hermetic isolation
├── report.go         SuiteReport / BuildReport / WriteJSON / WriteMarkdown
└── dataset/
    ├── tool_calling_test.go    Tool calling accuracy (4 cases)
    ├── planning_test.go        Planning completion rate (4 cases)
    ├── context_test.go         Context Engineering coherence (3 cases)
    ├── error_handling_test.go  Error Handling / Self-Healing (3 cases)
    └── memory_test.go          Memory persistence (2 cases)
```

### 2.2 ScriptedProvider: Scripting LLM Behavior

`ScriptedProvider` is the cornerstone of the eval framework. It implements the `provider.LLMProvider` interface and returns deterministic replies according to a preset sequence of `ScriptedTurn`s, without making any network requests.

```go
// Script: first turn issues a bash tool call, second turn returns a text conclusion
p := evals.NewScriptedProvider(
    evals.ScriptedTurn{
        ToolCalls: []schema.ToolCall{
            evals.MakeToolCall("tc1", "bash", `{"command":"ls -la"}`),
        },
    },
    evals.ScriptedTurn{Text: "The directory contains 3 files."},
)
```

| Mechanism | Description |
|------|------|
| **Turn sequence** | Each `Generate` call consumes one `ScriptedTurn`; once exhausted it returns a default termination reply |
| **Call recording** | Every LLM call is recorded to `calls []RecordedCall`, for Assertion verification |
| **Err injection** | `ScriptedTurn{Err: err}` simulates an LLM API failure, testing the engine's self-healing capability |
| **Thread safety** | Internal mutex, no race conditions under concurrent goroutine calls |

### 2.3 Assertion Framework

Assertions fall into two categories: **Hard** (failure means the Case does not pass) and **Soft** (only logs a warning):

```
Assertion
├── Hard Assertions (failure → Passed=false)
│   ├── ToolCalledAssertion{ToolName, MinTimes}   Tool was called >= N times
│   ├── ToolNotCalledAssertion{ToolName}           Tool was never called
│   ├── OutputContainsAssertion{Expected}          Final output contains the expected string
│   ├── OutputExcludesAssertion{Forbidden}         Final output does not contain the forbidden string
│   ├── NoErrorAssertion{}                         RunError == nil
│   └── ErrorAssertion{}                           RunError != nil (tests the error path)
└── Soft Assertions (failure → Warnings, does not affect Passed)
    ├── MaxTurnsAssertion{Max}                     Turn count <= Max (efficiency warning)
    └── MaxToolCallsAssertion{Max}                 Tool call count <= Max (efficiency warning)
```

`recordingHook` records the tool name during the `HookRegistry.BeforeExecute` phase — triggered **before** the registry lookup, so it correctly captures the LLM's calling intent regardless of whether the tool is registered.

### 2.4 EvalHarness: Minimal Engine Environment

`RunCase` builds a fully isolated, minimal `AgentEngine` for each Case:

```
RunCase(c *Case) Result
    │
    ├── Determine the working directory (c.WorkDir, or auto-create a temp dir, deferred cleanup)
    ├── Register four base tools (read_file / write_file / bash / edit_file)
    ├── Mount recordingHook (records tool names, at the front of HookRegistry)
    ├── engine.NewAgentEngine(c.Provider, hookReg, workDir, WithMaxTurns(c.MaxTurns))
    │       ← ScriptedProvider + no Session + no Compactor (ensures determinism)
    ├── eng.Run(ctx, c.Prompt)
    └── Run c.Assertions one by one → aggregate Failures / Warnings → Result.Passed
```

**Not using Session and Compactor** is a key decision — it eliminates the non-determinism introduced by persistence and compaction, guaranteeing that the same script always produces the same result.

### 2.5 Hermetic Test Isolation

```go
func TestMyFeature(t *testing.T) {
    evals.SetupHermeticEnv(t)  // Must be called first

    c := &evals.Case{
        ID:       "feature/basic",
        Category: "feature",
        Prompt:   "Run the ls command",
        Provider: evals.NewScriptedProvider(
            evals.ScriptedTurn{
                ToolCalls: []schema.ToolCall{
                    evals.MakeToolCall("tc1", "bash", `{"command":"ls"}`),
                },
            },
            evals.ScriptedTurn{Text: "Command executed."},
        ),
        Assertions: []evals.Assertion{
            &evals.ToolCalledAssertion{ToolName: "bash"},
            &evals.NoErrorAssertion{},
            &evals.MaxTurnsAssertion{Max: 3}, // soft: efficiency warning
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

`SetupHermeticEnv` clears all environment variables ending in `_API_KEY`, `_TOKEN`, and `_SECRET`, preventing eval tests from accidentally invoking a paid service due to a real API Key being present in the environment, guaranteeing identical behavior locally and in CI.

---

## III. Eval Subsystem: Golden Dataset

### 3.1 Current Golden Dataset (16 cases)

| Category | Case | Verification target |
|------|------|---------|
| `tool_calling` | `bash_basic` | bash tool is correctly called |
| `tool_calling` | `read_file` | read_file tool is correctly called |
| `tool_calling` | `write_then_read` | Multiple tools called in sequence (write → read) |
| `tool_calling` | `no_tool_conversation` | Pure conversation does not trigger a tool call |
| `planning` | `plan_generated` | todo_write writes a plan |
| `planning` | `no_write_in_plan_mode` | write_file/edit_file are not called during the planning stage |
| `planning` | `plan_then_execute` | Generate a plan first, then execute it (full Planning pipeline) |
| `planning` | `exploration_only` | Pure exploration mode uses only read-only tools |
| `context` | `sequential_tool_chain` | Multi-step tool calls depend on the previous step's Observation |
| `context` | `multi_turn_conversation` | Multi-turn pure-conversation coherence |
| `context` | `tool_error_observation` | Tool failure Observation drives the LLM to change strategy |
| `error_handling` | `bash_fallback_on_error` | LLM switches to an alternative approach after a tool failure (Self-Healing) |
| `error_handling` | `write_failure_graceful_stop` | Graceful degradation without retry after a write failure |
| `error_handling` | `max_turns_protection` | MaxTurns triggers controlled engine termination (no panic) |
| `memory` | `write_memory` | memory_write tool is called |
| `memory` | `search_memory` | memory_search tool is called |

### 3.2 Running Eval

```bash
# Run the full golden dataset (16 cases, no API Key required)
go test ./internal/evals/... ./internal/evals/dataset/... -v

# Run only a specific category
go test ./internal/evals/dataset/... -v -run TestToolCalling
go test ./internal/evals/dataset/... -v -run TestPlanning
go test ./internal/evals/dataset/... -v -run TestContextEngineering
go test ./internal/evals/dataset/... -v -run TestErrorHandling
go test ./internal/evals/dataset/... -v -run TestMemory

# Generate JSON + Markdown reports
results := suite.Run(ctx)
report  := evals.BuildReport(results)
evals.WriteJSON(report, "eval-report.json")
evals.WriteMarkdown(report, "eval-report.md")
```

### 3.3 Standard for Adding New Eval Cases

Once feature development is complete, a corresponding golden case **must** be added under `internal/evals/dataset/` (see [AGENTS.md §5.8 Test & Eval Standard](https://github.com/ZhangShenao/harness9/blob/master/AGENTS.md)):

- Every feature must cover at least a **positive case** (the feature works correctly) and a **negative case** (the constraint is correctly enforced)
- `SetupHermeticEnv` must be called first; `NoErrorAssertion` or `ErrorAssertion` is mandatory
- Extending the dataset only requires adding a new `_test.go` file — no framework code changes needed
- The current 16 cases are the baseline; they may only be added to, never removed or have coverage reduced

---

## IV. CI/CD Quality Gate

### 4.1 Pipeline Design

```
PR triggered (push to master / pull_request)
       │
       ▼
  unit-tests job
  └── go test ./...  ← Full unit test suite (including observability + evals)
       │
       ▼ needs: unit-tests
  eval job (Quality Gate)
  ├── Environment: OPENAI_API_KEY=""  ANTHROPIC_API_KEY=""  HARNESS9_EVAL_HERMETIC=1
  │          OTEL_ENABLED=false (OTEL reporting disabled in CI)
  ├── go test ./internal/evals/... ./internal/evals/dataset/... -v
  ├── Results uploaded as an Artifact (retained for 30 days)
  └── Summary written to GitHub Step Summary
```

**Quality Gate**: `continue-on-error: false` — if eval fails, CI fails and the PR cannot be merged.

**Hermetic guarantees**:
1. Incurs no real LLM API costs
2. Test results are fully deterministic, with no random fluctuation
3. Any behavioral regression comes from the code change, not from an LLM version update

---

## V. Observability Subsystem

### 5.1 Span Hierarchy

Every Agent run in harness9 produces a complete Span tree:

```
harness9.interaction   [session.id="abc123",  langfuse.trace.input="user prompt"]
│   duration: 12.4s
│
├── harness9.turn   [agent.turn=1]
│   │   duration: 3.2s
│   │
│   ├── harness9.llm_request   [gen_ai.request.model="anthropic/claude-sonnet-4.6"]
│   │       langfuse.observation.input  = [{"role":"system",...},{"role":"user",...}]
│   │       langfuse.observation.output = "LLM reply text or tool call JSON"
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

### 5.2 Implementation Principles of the Three Components

#### OTELEngineObserver — Interaction + Turn Span

`runLoop` calls back into `EngineObserver` at 4 lifecycle points:

```
runLoop entry → OnInteractionStart(ctx, sessionID, prompt)
               Returns an enhanced ctx carrying the interaction Span

for each Turn:
  → OnTurnStart(ctx, turn)    Returns turnCtx carrying the turn Span
    em.generate(turnCtx, ...)   ← LLM call inherits the turn Span
    e.executeTools(turnCtx, ...) ← Tool execution inherits the turn Span
  → OnTurnEnd(turnCtx, turn, hasToolCalls)

runLoop exit (guaranteed via defer)
  → OnInteractionEnd(ctx, turns, err)
    → span.End() + ForceFlush()   ← Pushed to the backend immediately
```

**Key design**: `OnInteractionStart` and `OnTurnStart` write the Span in two places (the OTEL standard slot + a custom key), ensuring that the parent-child relationship chain does not break even when intermediate-layer code (compaction, session loading) may replace the ctx.

#### TracingProvider — LLM Request Span + Token Metrics

```go
func (p *TracingProvider) GenerateStream(ctx context.Context, ...) {
    ctx, span := p.tracer.Start(ctx, SpanLLMRequest)  // ctx contains the turn Span, nested automatically
    span.SetAttributes(attribute.String(AttrLangfuseObsInput, serializeMessages(messages)))

    ch, _ := p.inner.GenerateStream(ctx, ...)
    go func() {
        defer span.End()
        // Wait for StreamChunkDone, extract Usage and the final reply
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
    return ctx, hooks.Allow(), nil  // Always allows; does not interfere with tool decisions
}

func (h *ObservabilityHook) AfterExecute(ctx context.Context, tc schema.ToolCall, result schema.ToolResult) schema.ToolResult {
    span := trace.SpanFromContext(ctx)
    span.SetAttributes(attribute.String(AttrLangfuseObsOutput, truncateAttr(result.Output)))
    span.End()
    h.toolDuration.Record(...)   // Histogram
    h.toolCallsTotal.Add(...)    // Counter, by name + status
    return result                // Passed through unmodified
}
```

### 5.3 Metrics System

| Metric name | Type | Description |
|--------|------|------|
| `harness9.llm.request.duration` | Histogram | LLM API request latency (seconds) |
| `harness9.llm.tokens.input` | Counter | Cumulative input tokens |
| `harness9.llm.tokens.output` | Counter | Cumulative output tokens |
| `harness9.tool.calls.total` | Counter | Tool call count (by name + status) |
| `harness9.tool.execution.duration` | Histogram | Tool execution duration |
| `harness9.agent.turns.total` | Counter | Total Agent Turn count |

### 5.4 OTEL SDK Initialization and Configuration

```
Setup(ctx, cfg)
├── cfg.Enabled=false or ExporterNoop  → zero-overhead noopProviders()
├── ExporterStdout                     → stdouttrace (local debugging, writes to stderr)
└── ExporterOTLP
        ├── Explicitly appends /v1/traces (does not rely on the SDK auto-appending it, since behavior differs across versions)
        ├── Explicitly passes WithHeaders (does not rely on the SDK reading env vars, to guarantee reliability)
        ├── https:// → TLS, http:// → unencrypted
        └── Global OTEL error handler writes to stderr (bypasses the TUI's io.Discard)
```

| Environment variable | Default | Description |
|---------|--------|------|
| `OTEL_ENABLED` | `false` | `true` enables it |
| `OTEL_SERVICE_NAME` | `harness9` | Service name |
| `OTEL_EXPORTER_TYPE` | `noop` | `noop` / `stdout` / `otlp` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | Base URL, e.g. `https://us.cloud.langfuse.com/api/public/otel` |
| `OTEL_EXPORTER_OTLP_HEADERS` | — | `key=val,key2=val2`, used for the Langfuse Authorization header |

---

## VI. Integrating with Observability Platforms

### 6.1 Integrating with Langfuse (recommended)

Langfuse is an observability platform purpose-built for LLM applications, with native OpenTelemetry support, providing trace visualization, token cost analysis, and session replay.

#### Step 1: Register an account and obtain an API Key

1. Go to [cloud.langfuse.com](https://cloud.langfuse.com/auth/sign-up) to register
2. In your project, go to **Settings → API Keys** → click **Create new API key**
3. Copy the **Public Key** (`pk-lf-...`) and **Secret Key** (`sk-lf-...`, shown only once)

#### Step 2: Generate a Base64 authentication string

```bash
# macOS / Linux
AUTH=$(echo -n "pk-lf-YOUR_PUBLIC_KEY:sk-lf-YOUR_SECRET_KEY" | base64)

# GNU/Linux (longer keys, avoid line wrapping)
AUTH=$(echo -n "pk-lf-YOUR_PUBLIC_KEY:sk-lf-YOUR_SECRET_KEY" | base64 -w 0)
```

#### Step 3: Configure and start

Write to `.env` (already in `.gitignore`, won't be committed to Git):

```bash
OTEL_ENABLED=true
OTEL_EXPORTER_TYPE=otlp

# Select your region
OTEL_EXPORTER_OTLP_ENDPOINT=https://us.cloud.langfuse.com/api/public/otel   # US region
# OTEL_EXPORTER_OTLP_ENDPOINT=https://cloud.langfuse.com/api/public/otel    # EU region
# OTEL_EXPORTER_OTLP_ENDPOINT=https://jp.cloud.langfuse.com/api/public/otel # JP region

OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic <YOUR_BASE64_AUTH>,x-langfuse-ingestion-version=4
```

Then run `./harness9`. After a conversation produces a tool call, the Langfuse **Traces** tab will show the following structure:

```
Trace: harness9.interaction
│   Input  = "user prompt"
│
└── Generation: harness9.llm_request
│       Input  = [{"role":"system",...},{"role":"user",...}]
│       Output = "LLM reply"
│       25,269 prompt → 1,027 completion  ← automatic cost estimation
│       anthropic/claude-sonnet-4.6
└── Span: harness9.tool  (bash)
        Input  = {"command":"ls -la"}
        Output = "total 24\n..."
```

#### Common troubleshooting

| Symptom | Cause | Solution |
|------|------|---------|
| No data in the console | `AUTH` encoding contains a newline character | Confirm the output with `echo $AUTH`, or add `-w 0` |
| `401 Unauthorized` | Keys are in the wrong order | Must be `pk-lf-...:sk-lf-...` (Public Key first) |
| Trace appears but is delayed | Missing the ingestion-version header | Add `x-langfuse-ingestion-version=4` to the headers |
| Can't connect to the region | Wrong endpoint chosen | Confirm your account's registration region (EU/US/JP) and switch to the matching endpoint |
| Input/Output shows null | Old `langfuse.input/output` attribute names | Already fixed (v4 uses `langfuse.trace.*` / `langfuse.observation.*`) |
| Trace export fails (no data) | Tool output contains invalid UTF-8 bytes | Already fixed (`truncateAttr` auto-sanitizes) |

### 6.2 Integrating with Jaeger (local development)

```bash
# Start Jaeger (all-in-one, including the OTLP HTTP receiver)
docker run --rm -p 16686:16686 -p 4318:4318 jaegertracing/all-in-one

export OTEL_ENABLED=true
export OTEL_EXPORTER_TYPE=otlp
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
./harness9

# Open http://localhost:16686 → search Service: harness9
```

### 6.3 Integrating with Grafana + Tempo

```bash
export OTEL_ENABLED=true
export OTEL_EXPORTER_TYPE=otlp
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318  # Tempo OTLP HTTP port
./harness9
```

### 6.4 Local Debugging (stdout exporter)

```bash
export OTEL_ENABLED=true
export OTEL_EXPORTER_TYPE=stdout
./harness9
# Span data is printed to stderr in JSON format
```

---

## VII. Module File Index

| File | Package | Responsibility |
|------|----|------|
| `internal/engine/observer.go` | `engine` | `EngineObserver` interface + `noopObserver` (zero-overhead empty implementation) |
| `internal/observability/config.go` | `observability` | `Config` struct + `ConfigFromEnv()` (including parseOTLPHeaders) |
| `internal/observability/attributes.go` | `observability` | Span names, Metric names, Langfuse v4 attribute key constants |
| `internal/observability/setup.go` | `observability` | OTEL SDK initialization (`Setup`, `NewNoopProviders`, ForceFlush binding) |
| `internal/observability/observer.go` | `observability` | `OTELEngineObserver` (Interaction + Turn Span, dual-write guarantees the chain) |
| `internal/observability/provider.go` | `observability` | `TracingProvider` (LLM Request Span + Token Metrics) |
| `internal/observability/hook.go` | `observability` | `ObservabilityHook` (Tool Execution Span + Tool Metrics) |
| `internal/observability/helpers.go` | `observability` | `serializeMessages` / `serializeOutput` / `truncateAttr` (UTF-8 sanitization) |
| `internal/evals/provider.go` | `evals` | `ScriptedProvider` (deterministic mock, thread-safe) |
| `internal/evals/assertions.go` | `evals` | `Assertion` interface + `Case` / `Result` types + 8 assertions (Hard/Soft) |
| `internal/evals/harness.go` | `evals` | `RunCase` / `Suite` / `recordingHook` (temp dir auto-cleanup) |
| `internal/evals/testenv.go` | `evals` | `SetupHermeticEnv()` (standard hermetic isolation, auto-restored via `t.Setenv`) |
| `internal/evals/report.go` | `evals` | `BuildReport` / `WriteJSON` / `WriteMarkdown` (categorized statistics + detailed results) |
| `internal/evals/dataset/tool_calling_test.go` | `dataset` | Tool calling accuracy (4 cases) |
| `internal/evals/dataset/planning_test.go` | `dataset` | Planning completion rate (4 cases) |
| `internal/evals/dataset/context_test.go` | `dataset` | Context Engineering (3 cases) |
| `internal/evals/dataset/error_handling_test.go` | `dataset` | Error Handling / Self-Healing (3 cases) |
| `internal/evals/dataset/memory_test.go` | `dataset` | Memory persistence (2 cases) |
| `.github/workflows/eval.yml` | CI | GitHub Actions Quality Gate |
