# Tool Calling System

## Overview

Tool Calling is one of the core capabilities of the harness9 Agent framework, enabling LLMs to interact with the external environment through structured function calls. This document describes in detail the architecture, data flow, key interfaces, and implementation details of the tool calling system.

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────┐
│                        Agent Engine                         │
│                                                             │
│  ┌──────────┐    ToolCall[]    ┌─────────────────────────┐  │
│  │          │ ──────────────► │                          │  │
│  │  LLM     │                  │   Tool Registry          │  │
│  │ Provider │ ◄────────────── │                          │  │
│  │          │    ToolResult[]  │  ┌──────┐ ┌──────┐      │  │
│  └──────────┘                  │  │bash  │ │read  │ ...  │  │
│       │                        │  │Tool  │ │file  │      │  │
│       │ Message                │  └──────┘ └──────┘      │  │
│       │ (ToolCalls)            └─────────────────────────┘  │
│       │                                                   │
│       ▼                                                   │
│  ┌──────────────────────────────────────────┐             │
│  │          Context History                  │             │
│  │  [system] → [user] → [assistant+TC] →   │             │
│  │  [observation] → [assistant] → ...        │             │
│  └──────────────────────────────────────────┘             │
└─────────────────────────────────────────────────────────────┘
```

## Core Data Flow

A complete Tool Calling cycle consists of the following steps:

```
1. LLM generates a response containing ToolCalls
2. Engine detects ToolCalls and executes each tool concurrently
3. Each tool call returns a ToolResult
4. ToolResult is converted into an Observation message (Role=user, ToolCallID=xxx)
5. Observation is injected into Context History
6. Proceed to the next round of LLM calls
```

Sequence diagram:

```
Engine                LLMProvider           Registry            BaseTool
  │                       │                     │                   │
  │  Generate(msgs,tools) │                     │                   │
  │──────────────────────►│                     │                   │
  │  Message{ToolCalls}   │                     │                   │
  │◄──────────────────────│                     │                   │
  │                       │                     │                   │
  │  Execute(call)        │                     │                   │
  │─────────────────────────────────────────────►│                   │
  │                       │                     │  Execute(ctx,args)│
  │                       │                     │──────────────────►│
  │                       │                     │  (string, error)  │
  │                       │                     │◄──────────────────│
  │  ToolResult           │                     │                   │
  │◄─────────────────────────────────────────────│                   │
  │                       │                     │                   │
  │  [Observation → ctx]  │                     │                   │
  │  Generate(msgs,tools) │                     │                   │
  │──────────────────────►│                     │                   │
```

## Core Type Definitions

### Tool Call Request — ToolCall

```go
type ToolCall struct {
    ID        string          // Unique identifier assigned by the LLM, used to correlate the request with its result
    Name      string          // Target tool name (e.g. "bash", "read_file")
    Arguments json.RawMessage // Raw JSON parameters, deserialized lazily
}
```

**Design decision**: `Arguments` uses `json.RawMessage` instead of `map[string]interface{}`, deferring parsing responsibility to the specific tool implementation. This avoids premature type assertions at the engine layer, and also allows tools to accept arbitrary JSON structures as input.

### Tool Execution Result — ToolResult

```go
type ToolResult struct {
    ToolCallID string // Correlates with the original ToolCall.ID
    Output     string // stdout or error information from the tool execution
    IsError    bool   // Marks whether the execution failed
}
```

**Key design**: The `IsError` field allows the engine to relay failure information back to the LLM, triggering self-healing behavior — for example, the LLM can fix command syntax and retry.

### Tool Definition — ToolDefinition

```go
type ToolDefinition struct {
    Name        string      // Unique tool identifier
    Description string      // Natural language description for the LLM to understand the tool's purpose
    InputSchema interface{} // JSON Schema describing the parameter format
}
```

**Design decision**: `InputSchema` uses `interface{}` rather than a concrete type, because different LLM SDKs require different parameter formats:
- The OpenAI SDK requires `shared.FunctionParameters` (i.e. `map[string]interface{}`)
- The Anthropic SDK requires separate `Properties` + `Required` fields

Each Provider implementation is responsible for converting `interface{}` into the format required by its SDK.

## Key Interfaces

### BaseTool — Tool Implementation Contract

```go
type BaseTool interface {
    Name() string
    Definition() schema.ToolDefinition
    Execute(ctx context.Context, args json.RawMessage) (string, error)
}
```

| Method | Responsibility |
|------|------|
| `Name()` | Returns the tool's unique identifier in the Registry |
| `Definition()` | Returns the tool's metadata (description, parameter schema) for the LLM to understand |
| `Execute()` | Executes the tool logic, receives raw JSON parameters, returns text output |

### Registry — Tool Registration Center

```go
type Registry interface {
    Register(tool BaseTool)
    GetAvailableTools() []schema.ToolDefinition
    Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult
}
```

| Method | Responsibility |
|------|------|
| `Register()` | Registers a tool with the registry; duplicate names are overwritten and a warning is printed |
| `GetAvailableTools()` | Returns the list of ToolDefinition for all registered tools, passed to the LLM |
| `Execute()` | Looks up the tool by ToolCall.Name and executes it, returning a ToolResult |

### LLMProvider — Model Provider Interface

```go
type LLMProvider interface {
    Generate(ctx context.Context, messages []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error)
    GenerateStream(ctx context.Context, messages []schema.Message, availableTools []schema.ToolDefinition) (<-chan schema.StreamChunk, error)
}
```

When `availableTools` is `nil`, it means no tools are provided (Thinking phase); the semantics of an empty slice `[]` and `nil` differ:
- `nil`: does not pass the tools parameter to the API (the model cannot call tools)
- `[]`: passes an empty tools array (theoretically uncommon, but the engine uses `nil` to represent Thinking)

`Generate` returns `*schema.Usage` (containing `InputTokens` and `OutputTokens`), which the engine uses to update the TUI's token usage display. `GenerateStream` returns actual usage via the `Usage` field in `StreamChunkDone`.

## Concurrent Execution Model

The engine executes all ToolCalls within a single Turn concurrently via `executeToolsConcurrently`:

```go
func (e *AgentEngine) executeToolsConcurrently(ctx context.Context, turn int, toolCalls []schema.ToolCall) []schema.ToolResult {
    results := make([]schema.ToolResult, len(toolCalls))
    var wg sync.WaitGroup

    for i, toolCall := range toolCalls {
        wg.Add(1)
        go func(idx int, tc schema.ToolCall, currentTurn int) {
            defer wg.Done()

            toolCtx := ctx
            var cancel context.CancelFunc
            if e.ToolTimeout > 0 {
                toolCtx, cancel = context.WithTimeout(ctx, e.ToolTimeout)
                defer cancel()
            }

            results[idx] = e.registry.Execute(toolCtx, tc)
        }(i, toolCall, turn)
    }

    wg.Wait()
    return results
}
```

**Key design points**:

1. **Preallocation + indexed writes**: The `results` slice is preallocated before goroutines start; each goroutine writes to its corresponding position via index `idx`, avoiding race conditions.

2. **Independent timeout control**: Each tool gets its own `context.WithTimeout` child context. A timeout in one tool does not affect the execution of other tools — it only marks the current tool as failed.

3. **WaitGroup synchronization**: Results are injected into Observation uniformly only after all tools have finished executing, ensuring message order matches the ToolCalls order.

## Logging System

The tool calling process is output through **Block-Style Structured Logs**. Design goals:

1. **Readability**: Line breaks are preserved as-is; multi-line command output no longer appears as literal `\n` characters.
2. **Structure**: Argument JSON has HTML escaping disabled and is pretty-printed as needed; output is prefixed with `│ ` to form an "info block".
3. **Scannability**: The first line retains a single-line header, so keywords like `grep "Tool Started"` can still locate entries.

### Tool Started — Short Arguments (≤ 80 bytes, inline on a single line)

```
2026/04/29 15:11:30 [engine] Turn 1 │ Tool Started │ tool=bash id=call_xyz
        arguments: {"command":"go version && pwd && ls -la"}
```

### Tool Started — Long Arguments (pretty-printed)

```
2026/04/29 15:11:30 [engine] Turn 1 │ Tool Started │ tool=write_file id=call_abc
        arguments:
          {
            "path": "src/main.go",
            "content": "package main\n\nimport \"fmt\"\n..."
          }
```

### Tool Completed — Multi-line Output (with `│ ` prefix)

```
2026/04/29 15:11:30 [engine] Turn 1 │ Tool Completed │ tool=bash id=call_xyz status=ok bytes=1363 (truncated to 512)
        output:
        │ go version go1.25.3 darwin/arm64
        │ /Users/zsa/Desktop/harness/harness9
        │ total 9456
        │ drwxr-xr-x@ 22 zsa  staff      704  Apr 29 15:09 .
```

### Tool Failed

```
2026/04/29 15:11:30 [engine] Turn 1 │ Tool Failed │ tool=bash id=call_xyz status=error bytes=42
        output:
        │ command not found: foo
```

### Key Implementation Details

- **JSON HTML-escape disabled**: Uses `json.NewEncoder.SetEscapeHTML(false)` to avoid `&&` being escaped into `\u0026\u0026`.
- **Truncation threshold `MaxOutputLen = 512`**: Any single output in the log exceeding this length is truncated (UTF-8 safe — never cuts through a multi-byte character), with the header carrying a `(truncated to N)` hint.
- **Continuation line indent `Indent = 8 spaces`**: All continuation lines use the same indentation for visual alignment.
- **Inline threshold `argInlineThreshold = 80`**: JSON shorter than this length after compaction is displayed inline on a single line.

### Logging Coverage Points

| Stage | Log Content |
|------|---------|
| Engine startup | workdir, thinking mode, maxTurns, toolTimeout |
| Turn start | Current turn number, context message count |
| Phase 1 (Thinking) | LLM call with tools disabled |
| Phase 2 (Action) | LLM call with tools restored |
| Tool started | Tool name, ID, **structured JSON arguments** (short = inline / long = multi-line) |
| Tool completed/failed | Tool name, ID, status, byte count, **multi-line block output** |
| Observation injection | Message count change |
| Loop end | Total turn count, final message count |

## Provider Adaptation Layer

### OpenAI-Compatible Adapter

**File**: `internal/provider/openai.go`

**Type conversion rules**:

| schema type | OpenAI SDK type |
|-------------|----------------|
| `RoleSystem` | `openai.SystemMessage` |
| `RoleUser` (no ToolCallID) | `openai.UserMessage` |
| `RoleUser` (with ToolCallID) | `openai.ToolMessage` |
| `RoleAssistant` | `ChatCompletionAssistantMessageParam` |
| `ToolDefinition` | `ChatCompletionFunctionTool` |

**Environment variables**:
- `OPENAI_API_KEY`: API authentication key (required)
- `OPENAI_BASE_URL`: API endpoint base URL (required, supports OpenRouter and other compatible services)

### Anthropic-Compatible Adapter

**File**: `internal/provider/anthropic.go`

**Key differences from the OpenAI adapter**:

| Difference | OpenAI | Anthropic |
|--------|--------|-----------|
| System Prompt | In the messages array | Separate parameter `params.System` |
| Tool result | `ToolMessage(content, toolCallID)` | `ToolResultBlock(toolCallID, content, isError)` |
| Tool call arguments | Raw JSON string | Deserialized into `map[string]interface{}` |
| MaxTokens | Optional | **Required** parameter |
| Tool definition schema | Complete JSON Schema | Separate `Properties` + `Required` |

**Environment variables**:
- `ANTHROPIC_API_KEY`: API authentication key (required)
- `ANTHROPIC_BASE_URL`: API endpoint base URL (required)

## Implemented Tools

harness9 currently ships with four built-in tools, covering a Minimum Viable Toolset for file I/O and shell command execution:

| Tool | File | Main capability | Sandbox protection |
|------|------|---------|---------|
| `read_file`  | `internal/tools/read_file.go`  | Read the content of a file in the workspace | Yes — safePath validation |
| `write_file` | `internal/tools/write_file.go` | Create/overwrite a file in the workspace | Yes — safePath validation |
| `edit_file`  | `internal/tools/edit_file.go`  | Precise text replacement (multi-level fuzzy matching) | Yes — safePath validation |
| `bash`       | `internal/tools/bash.go`       | Execute arbitrary bash commands  | No — YOLO philosophy, no command whitelist |

### Shared Security Module: safePath (Path Sandbox)

**File**: `internal/tools/safe_path.go`

`read_file` and `write_file` share a single path validation logic:

```go
func safePath(workDir, inputPath string) (string, error)
```

Implementation highlights:
- Joins with `filepath.Join(workDir, inputPath)` then normalizes via `filepath.Abs`
- Validates that the absolute path must have `workDir + PathSeparator` as a prefix (preventing `/project-evil` from being misidentified as a subpath of `/project`)
- Any path containing a `../` escape returns an error, which the Registry wraps as an `IsError=true` `ToolResult` relayed back to the LLM

**Why it was split into its own file**: To avoid duplicating the same security code in `read_file` and `write_file`, so that any future policy adjustment (e.g. blacklists, ACLs, audit logging) only needs to be modified in one place.

### read_file — File Reading Tool

**File**: `internal/tools/read_file.go`

| Attribute | Value |
|------|-----|
| Name | `read_file` |
| Parameters | `path` (string, required) — file path relative to the workspace |
| Output | File content text |
| Truncation policy | Truncated with an appended notice when exceeding `maxReadLen = 8192` bytes |

**Security measures**:
- The path is validated through the shared `safePath` (Sandbox Boundary)
- Uses `io.LimitReader(file, maxReadLen+1)` to limit the read size per call, preventing oversized files from consuming the Context Window
- The extra `+1` byte is used to detect whether truncation actually occurred

### write_file — File Writing Tool

**File**: `internal/tools/write_file.go`

| Attribute | Value |
|------|-----|
| Name | `write_file` |
| Parameters | `path` (string, required) — file path relative to the workspace<br>`content` (string, required) — the complete file content to write |
| Output | `Successfully wrote N bytes to file: <path>` |
| Write semantics | Overwrite — replaces the target directly if it already exists |
| File permissions | 0644 |

**Security / robustness**:
- The path is validated through the shared `safePath`, consistent with `read_file`'s security policy
- If the parent directory does not exist, it is auto-created via `os.MkdirAll(filepath.Dir(fullPath), 0755)` (Auto-Mkdir), avoiding repeated LLM trial-and-error due to ENOENT
- The LLM must decide on its own whether to `read_file` first before overwriting with `write_file`; the framework does not perform versioning / backups

### edit_file — File Editing Tool (Multi-Level Fuzzy Matching)

**File**: `internal/tools/edit_file.go`

| Attribute | Value |
|------|-----|
| Name | `edit_file` |
| Parameters | `path` (string, required) — file path relative to the workspace<br>`source_text` (string, required) — the original text fragment to match<br>`target_text` (string, required) — the replacement text |
| Output | `Successfully edited file: <path>` |
| Write semantics | Overwrite — only the matched text region is replaced |

**Multi-Level Fuzzy Matching algorithm**:

The core competitive advantage of edit_file lies in its Four-Level Fallback Pipeline, progressively degrading tolerance for formatting deviations in LLM output:

```
L1 — Exact Match
    sourceText occurs exactly once in the original content; replace directly.
    This is the most efficient and safest matching method.

L2 — Line Ending Normalization
    Normalizes \r\n to \n before matching, ensuring cross-platform file format compatibility.
    After replacement, the original file's line-ending style (\r\n / \n) is automatically preserved.

L3 — Trimmed Match
    Matches after trimming whitespace from both ends of sourceText, tolerating extra whitespace produced by the LLM.

L4 — Line-by-Line Indent-Agnostic Matching
    Sliding-window matching after trimming leading/trailing whitespace on each line, tolerating indentation differences (spaces vs. tabs).
    This is the last line of defense; once matched, the entire matched block is replaced with targetText.
```

**Uniqueness Guard**: The matching result at all four levels must be unique (count == 1). When multiple matches are found, a clear error is returned, asking the LLM to provide more surrounding code to pinpoint the location precisely, avoiding incorrect edits or deletions.

**Line-ending style preservation**: The L2-L4 replacement operations are performed on `normalizedContent` (\r\n → \n); before writing, the line-ending style is automatically restored based on whether the original content contained `\r\n`, ensuring cross-platform compatibility.

**Security / robustness**:
- The path is validated through the shared `safePath`, consistent with the `read_file` / `write_file` security policy
- Returns a clear error when the file does not exist or JSON argument parsing fails, guiding the LLM toward self-healing retries
- Does not auto-create parent directories (unlike write_file); the target file is required to already exist

### bash — Shell Command Execution Tool

**File**: `internal/tools/bash.go`

| Attribute | Value |
|------|-----|
| Name | `bash` |
| Parameters | `command` (string, required) — the bash command; `timeout_secs` (int, optional) — per-call timeout in seconds |
| Output | The combined content of `stdout` and `stderr` |
| Default timeout | `defaultBashTimeout = 120s`; per-call relaxation via `timeout_secs` allowed (capped at `maxBashTimeout = 600s`), whichever comes first against the parent context |
| Truncation threshold | Truncated when exceeding `maxOutputLen = 16000` bytes (head + tail preserved: ~1/3 head + ~2/3 tail, UTF-8 safe) |

**Key design philosophy**:
- **YOLO philosophy (Trust-the-LLM)**: Does not restrict the kinds of commands that can be executed, handing all judgment and decision-making entirely over to the LLM, with no whitelist/blacklist.
- **Execution method**: Wrapped via `bash -c <command>`, supporting pipes `|`, logical and/or `&& ||`, environment variables, redirection, and other complex shell syntax.
- **Errors relayed as-is (Self-Correction Loopback)**: When a command exits with a non-zero exit code, it **still returns `(string, nil)`**, relaying the error content (including `exit status N`) back to the LLM as readable text to trigger a self-healing retry, rather than interrupting the agent loop.
- **Time Budgeting**: A double safeguard of the engine-layer `ToolTimeout` plus the tool-internal `defaultBashTimeout` (120s, relaxable to 600s via `timeout_secs`), preventing blocking commands such as `top` / `tail -f` / web servers from hanging the engine.
- **Empty command protection**: When `command == ""`, directly returns `Error: command is an empty string` without invoking `exec`.

**Why no sandboxing is applied**: The bash tool inherently provides full shell access; adding `cd /` alone would escape `workDir`, so a "semi-sandbox" would only create a false sense of security. For path-level security, use `read_file` / `write_file` instead.

### Registration Example

```go
registry := tools.NewRegistry()
registry.Register(tools.NewReadFileTool(workDir))
registry.Register(tools.NewWriteFileTool(workDir))
registry.Register(tools.NewEditFileTool(workDir))
registry.Register(tools.NewBashTool(workDir))
```

## Extension Guide

### Adding a New Tool

1. Create a new file under `internal/tools/` (e.g. `write_file.go`)
2. Implement the three methods of the `BaseTool` interface
3. Register it in `cmd/harness9/main.go`:

```go
writeTool := tools.NewWriteFileTool(workDir)
registry.Register(writeTool)
```

### Adding a New Provider

1. Create a new file under `internal/provider/` (e.g. `google.go`)
2. Implement the `Generate` method of the `LLMProvider` interface
3. Handle the conversion from schema types to SDK types
4. Replace the Provider initialization in `main.go`

### Adding Tool Middleware

Currently, the Registry's `Execute` method calls the tool directly. If middleware capabilities (logging, permission checks, rate limiting) need to be added, the call chain can be wrapped in `registryImpl.Execute`:

```go
func (r *registryImpl) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
    // Pre-middleware: permission checks, logging, rate limiting
    if !r.isAllowed(call.Name) {
        return schema.ToolResult{...}
    }

    output, err := tool.Execute(ctx, call.Arguments)

    // Post-middleware: result transformation, audit logging
    return schema.ToolResult{...}
}
```

## Design Decision Records

### 1. Why does ToolCall.Arguments use json.RawMessage?

Deferred deserialization hands the responsibility for type safety to the specific tool implementation. The engine does not need to know each tool's parameter structure, reducing coupling. It also avoids the complexity of type assertions on `map[string]interface{}` in nested structures.

### 2. Why does ToolResult use string instead of interface{}?

The LLM's tool results are passed through a text channel. Whether the tool output is command-line output, file content, or an error message, it is ultimately injected into the context as text. Using `string` simplifies the implementation of the Provider adaptation layer.

### 3. Why does Observation use RoleUser?

This follows the OpenAI and Anthropic API conventions: tool execution results are relayed back as a message with the user role, correlated with the original request via the `ToolCallID` field.

### 4. Why support parallel ToolCall?

Mainstream LLMs (GPT-4, Claude) support issuing multiple tool call requests in a single response. Parallel execution significantly reduces total latency, especially when there is no dependency between multiple tools (e.g. reading multiple files simultaneously).

### 5. Why is the IsError field important?

Error information is valuable context for the LLM. When a tool execution fails, the LLM can see the reason for the failure and attempt to self-heal — correcting the command, adjusting parameters, or choosing an alternative approach. This is more robust than failing silently or terminating the loop outright.

## File Index

| File | Responsibility |
|------|------|
| `internal/schema/message.go` | Core type definitions such as ToolCall, ToolResult, ToolDefinition |
| `internal/tools/base.go` | BaseTool interface definition |
| `internal/tools/registry.go` | Tool registry interface and implementation |
| `internal/tools/safe_path.go` | Shared path sandbox validation (prevents Path Traversal) |
| `internal/tools/safe_path_test.go` | Path sandbox unit tests |
| `internal/tools/read_file.go` | `read_file` tool implementation |
| `internal/tools/write_file.go` | `write_file` tool implementation |
| `internal/tools/edit_file.go` | `edit_file` tool implementation (multi-level fuzzy matching replacement) |
| `internal/tools/bash.go` | `bash` tool implementation |
| `internal/provider/interface.go` | LLMProvider interface definition |
| `internal/provider/openai.go` | OpenAI-compatible API adapter |
| `internal/provider/anthropic.go` | Anthropic-compatible API adapter |
| `internal/provider/tool_call_accumulator.go` | Streaming tool call argument accumulator shared by OpenAI/Anthropic |
| `internal/provider/providertest/mock.go` | Test Mock Provider (`_test` compilation unit, does not ship in the production binary) |
| `internal/engine/agent_loop.go` | Agent main loop, orchestrates the full Tool Calling flow + block-style log formatting |
| `internal/engine/stream.go` | Tool Calling orchestration for streaming mode |
| `internal/engine/agent_loop_test.go` | Main loop unit tests |
