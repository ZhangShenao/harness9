---
title: "harness9's Tool Calling System: From Interface Contracts to Concurrent Sandboxing"
date: 2026-06-01
tags: [harness9, harness, agent, golang, tool-calling, concurrency, sandbox]
summary: "A deep dive into every layer of harness9's tool calling system: the BaseTool interface abstraction, the concurrent execution model, path-level locking, the safePath sandbox, edit_file's four-tier fuzzy matching, and the self-healing mechanism that runs through the whole design — errors aren't a dead end, they're input for the next round of reasoning."
---

# harness9's Tool Calling System: From Interface Contracts to Concurrent Sandboxing

## About harness9

harness9 is a lightweight, feature-complete, production-ready Agent Harness framework written in Go.

- **Website**: [https://zhangshenao.github.io/harness9/](https://zhangshenao.github.io/harness9/)
- **GitHub**: [https://github.com/ZhangShenao/harness9](https://github.com/ZhangShenao/harness9)

A star is the most direct way to support this open-source project. Issues and PRs are welcome.

---

## TL;DR

harness9's tool calling system revolves around three core decisions: **interfaces are defined on the consumer side**, **concurrent execution with order-preserving writes**, and **errors are passed back verbatim to trigger self-healing**. The path sandbox and path-level read/write locks form the production-grade security baseline, and edit_file's four-tier fuzzy matching is a systematic countermeasure against the instability of LLM output.

---

## 1. The Data Type Layer: Three Types That Carry the Entire Protocol

The tool calling system's contract starts from three types in `internal/schema/message.go`.

```go
type ToolCall struct {
    ID        string
    Name      string
    Arguments json.RawMessage // Deferred deserialization — parsing is the tool's responsibility
}

type ToolResult struct {
    ToolCallID string
    Output     string
    IsError    bool  // when true, the engine passes the raw error text back to the LLM
}

type ToolDefinition struct {
    Name        string
    Description string
    InputSchema any   // each provider adapts this to its own SDK type
}
```

Using `json.RawMessage` for `Arguments` is a deliberate design choice. The engine layer doesn't know — and doesn't need to know — the argument structure of any given tool. The type-safety boundary is pushed down into each tool's implementation: the cost is that every tool must call `json.Unmarshal` itself, and the payoff is that the engine and the tools are fully decoupled — adding a new tool requires zero changes to the engine.

`InputSchema` uses `any` for the same reason. Built-in tools declare their JSON Schema as `map[string]interface{}`, and each provider adapter converts it into whatever type its own SDK expects (OpenAI's `shared.FunctionParameters`, Anthropic's `map[string]any`). The `schema` package itself stays oblivious to vendor differences.

![Diagram: core data type protocol layer](/blog/tool-calling/images/schema-types-01.jpg)


---

## 2. The Interface Layer: Defined on the Consumer Side

harness9's interface design follows Go convention: interfaces are declared by the dependent, not the implementer.

The `BaseTool` interface lives in `internal/tools/base.go`, and the `Registry` interface is in the same package:

```go
type BaseTool interface {
    Name() string
    Definition() schema.ToolDefinition
    Execute(ctx context.Context, args json.RawMessage) (string, error)
}

type Registry interface {
    Register(tool BaseTool) error
    GetAvailableTools() []schema.ToolDefinition
    Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult
}
```

The engine package (`internal/engine`) depends on the `tools.Registry` interface, not on the concrete `registryImpl` type. That means tests can inject any mock they like, with zero changes needed to production code.

The signature of `Registry.Execute` is worth pausing on — it takes a `schema.ToolCall` and returns a `schema.ToolResult`, rather than `(string, error)`. This wrapping performs a key semantic conversion at the registry layer: a tool execution failure is no longer a Go-level `error`, but a `ToolResult` with `IsError=true`. The engine can inject that failure record into the context as an ordinary Observation, and the LLM sees the error information on its next turn and decides for itself how to respond.

```go
func (r *registryImpl) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
    tool, exists := r.tools[call.Name]
    if !exists {
        return schema.ToolResult{
            ToolCallID: call.ID,
            Output:     fmt.Sprintf("Error: no tool named '%s' exists in the system.", call.Name),
            IsError:    true,
        }
    }
    output, err := tool.Execute(ctx, call.Arguments)
    if err != nil {
        return schema.ToolResult{
            ToolCallID: call.ID,
            Output:     fmt.Sprintf("Error executing %s: %v", call.Name, err),
            IsError:    true,
        }
    }
    return schema.ToolResult{ToolCallID: call.ID, Output: output}
}
```

Errors don't terminate the loop — errors are raw material for the next round of reasoning. This is the material foundation of harness9's self-healing capability.

![Diagram: Registry execution path and self-healing loop](/blog/tool-calling/images/registry-selfheal-02.jpg)


---

## 3. The Concurrent Execution Model: Pre-Allocated Slices + Indexed Writes

Mainstream LLMs (GPT, Claude) can emit multiple `ToolCall`s in a single response. harness9 executes them concurrently at the engine layer:

```go
func (e *AgentEngine) executeTools(ctx context.Context, turn int,
    toolCalls []schema.ToolCall, logPrefix string, em emitter) []schema.ToolResult {

    results := make([]schema.ToolResult, len(toolCalls)) // pre-allocated
    var wg sync.WaitGroup

    var sem chan struct{}
    if e.maxConcurrentTools > 0 {
        sem = make(chan struct{}, e.maxConcurrentTools) // concurrency limit
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
            // ...
            results[idx] = e.registry.Execute(toolCtx, tc) // indexed write
        }(i, toolCall)
    }
    wg.Wait()
    return results
}
```

Two concurrency-safety details are worth calling out on their own:

**Pre-allocation + indexed writes**: `results` is allocated to its final length before any goroutine is launched, and each goroutine writes to a fixed slot via the `idx` captured in its closure. Different goroutines write to different slots, so there's no race condition and no lock is needed. Go's memory model guarantees this pattern is safe.

**Independent per-tool timeouts**: each goroutine creates its own child context via `context.WithTimeout(ctx, e.toolTimeout)`. A timeout on one tool only cancels its own child context — it has no effect on other tools running in the same turn. That's why `toolCtx`, rather than the shared `ctx`, is what gets passed to `registry.Execute`.

Concurrency level is controlled via the `maxConcurrentTools` option — a channel acts as a semaphore, `sem <- struct{}{}` blocking to acquire a slot and `<-sem` releasing it. Setting it to 0 means no limit.

The engine's default configuration is `maxTurns=50, toolTimeout=60s`, giving production scenarios a reasonable upper bound as a safety net.

![Diagram: concurrent tool execution timeline](/blog/tool-calling/images/concurrent-tools-timing-03.jpg)


---

## 4. The Path Sandbox: safePath's Two Lines of Defense

The `bash` tool imposes no command restrictions, while `read_file` and `write_file` are protected by `safePath`. The two take opposite security philosophies, and both are deliberate choices.

The core logic of `safePath` lives in `internal/tools/safe_path.go`:

```go
func safePath(workDir, inputPath string) (string, error) {
    // Check absolute-path inputs first: intercept sensitive paths before Join
    if filepath.IsAbs(inputPath) {
        cleanInput := filepath.Clean(inputPath)
        if isSensitivePath(cleanInput) {
            return "", fmt.Errorf("path '%s' is a protected sensitive path, access denied", inputPath)
        }
    }

    cleanWorkDir := filepath.Clean(workDir)
    joined := filepath.Join(cleanWorkDir, inputPath)
    absPath, err := filepath.Abs(joined)
    // ...

    // The prefix must be cleanWorkDir + PathSeparator, not just cleanWorkDir —
    // otherwise "/project-evil" would be wrongly accepted as a valid subpath of "/project"
    if !strings.HasPrefix(absPath, cleanWorkDir+string(os.PathSeparator)) && absPath != cleanWorkDir {
        return "", fmt.Errorf("path '%s' is outside the workspace", inputPath)
    }

    if isSensitivePath(absPath) { // second check: re-verify after Join
        return "", fmt.Errorf("path '%s' is a protected sensitive path, access denied", inputPath)
    }
    return absPath, nil
}
```

Each of the two lines of defense targets a distinct attack vector:

The first line (absolute-path pre-check) targets cases where an attacker supplies an absolute path directly, intercepting it before `filepath.Join` runs — this prevents bypassing the relative-path sandbox via something like `/home/user/.ssh/id_rsa`.

The second line (post-Join prefix check) targets relative-path traversal like `../../etc/passwd`. `filepath.Abs` resolves `Join("/project", "../../etc/passwd")` down to `/etc/passwd`, and the prefix check then finds it doesn't start with `/project/` and rejects it outright.

The `/project-evil` detail in the comment guards against a real bug — a naive string-prefix match would let `/project-evil` slip past a check for `/project`, but adding `PathSeparator` closes that hole.

The hardcoded list of sensitive paths includes `~/.ssh`, `~/.aws`, `~/.kube`, `~/.gnupg`, `~/.netrc`, and `~/.config/gcloud` — the directories with the highest risk of credential leakage — and these are rejected regardless of what `workDir` is set to.

![Diagram: safePath's two-layer defense](/blog/tool-calling/images/safepath-defense-04.jpg)


---

## 5. Path-Level Locking: One Order of Magnitude Finer Than a Global Lock

`safePath` guards against escaping the sandbox; path-level read/write locks guard against concurrent contention on the same file.

`internal/tools/path_locker.go` implements a reference-counted, path-granularity locking scheme:

```go
type pathLock struct {
    rw  *sync.RWMutex
    ref int
}

var (
    pathLocksMu sync.Mutex
    pathLocks   = make(map[string]*pathLock)
)

func RLockPath(path string) func() {
    l := getOrCreatePathLock(path) // ref++
    l.rw.RLock()
    return func() {
        l.rw.RUnlock()
        releasePathLock(path, l) // ref--, removed from the map once it hits zero
    }
}
```

The call site is extremely simple:

```go
// read_file.go
unlock := RLockPath(fullPath)
defer unlock()

// write_file.go / edit_file.go
unlock := LockPath(fullPath)
defer unlock()
```

The key properties of this design:

Different paths never contend with each other. Two goroutines reading `a.go` and `b.go` at the same time get distinct `RWMutex` instances and never block one another. Lock contention only arises between concurrent calls operating on the exact same path.

Reference counting solves the problem of unbounded map growth. Path entries with no active users are removed from `pathLocks`, so memory doesn't leak as the number of historically touched paths grows.

Both `getOrCreatePathLock` and `releasePathLock` guard map access with `pathLocksMu`, ensuring `ref++` and `ref--` stay atomic.

Compared to a global `sync.RWMutex`, path-level locking's advantage shows up in throughput: when the LLM concurrently calls `read_file("a.go")` and `write_file("b.go")`, the two operations can run fully in parallel.

![Diagram: path-level locking vs. global locking](/blog/tool-calling/images/path-locker-comparison-05.jpg)



---

## 6. edit_file's Four-Tier Fuzzy Matching

`edit_file` is the most intricately designed of the built-in tools, and it exists to solve one problem: the `source_text` an LLM generates often doesn't exactly match what's actually in the file.

The four-level fallback pipeline unfolds inside the `fuzzyReplace` function:

```go
// L1: exact match
count := strings.Count(originalContent, sourceText)
if count == 1 {
    return strings.Replace(originalContent, sourceText, targetText, 1), nil
}
if count > 1 {
    return "", fmt.Errorf("source_text matched %d locations, please provide more surrounding context to ensure uniqueness", count)
}

// Entering L2-L4, normalize line endings first
normalizedContent := strings.ReplaceAll(originalContent, "\r\n", "\n")
normalizedSource := strings.ReplaceAll(sourceText, "\r\n", "\n")

// L2: line-ending-normalized match
count = strings.Count(normalizedContent, normalizedSource)
if count == 1 { /* replace, restore \r\n if needed */ }

// L3: whole-block leading/trailing whitespace trim
trimmedSource := strings.TrimSpace(normalizedSource)
if trimmedSource != "" {
    count = strings.Count(normalizedContent, trimmedSource)
    if count == 1 { /* replace */ }
}

// L4: line-by-line, indentation-stripped sliding-window match
return lineByLineReplace(normalizedContent, normalizedSource, normalizedTarget, hasCRLF)
```

Every level has a uniqueness guard: if `count > 1`, it returns an error immediately, asking the LLM to supply more context rather than guessing which occurrence to match. This avoids the class of silent, destructive bug where correct code gets edited by mistake.

L4's line-by-line, indentation-stripped comparison is the last line of defense, purpose-built to handle the LLM's unstable indentation output. It slides a window line by line, comparing content after `strings.TrimSpace`, tolerating differences in spaces and tabs.

Preserving the original line-ending style is a subtle but important detail: the replacements at L2 and below operate on normalized content (`\n`), and before writing back, the code checks whether the original file used `\r\n` and restores it if so — ensuring cross-platform compatibility.

The engineering significance of this mechanism: the LLM doesn't need to reproduce code formatting perfectly — the framework will find the closest match for it. At the same time, the uniqueness guard ensures that this leniency never turns into a hazard.

![Diagram: edit_file's four-tier matching pipeline](/blog/tool-calling/images/edit-file-pipeline-06.jpg)



---

## 7. The bash Tool's YOLO Philosophy and Dual Timeouts

The `bash` tool takes a completely different security philosophy from the other file tools. It applies no path sandboxing and no command allowlist.

```go
const bashHardTimeout = 30 * time.Second

func (t *BashTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
    // ...
    timeoutCtx, cancel := context.WithTimeout(ctx, bashHardTimeout)
    defer cancel()

    cmd := exec.CommandContext(timeoutCtx, "bash", "-c", input.Command)
    cmd.Dir = t.workDir
    out, err := cmd.CombinedOutput()

    if timeoutCtx.Err() == context.DeadlineExceeded {
        return outputStr + "\n[Warning: command execution timed out (30s) and was forcibly terminated.]", nil
    }
    if err != nil {
        // Note: returns (string, nil), not (string, error)
        return fmt.Sprintf("Execution error: %v\nOutput:\n%s", err, outputStr), nil
    }
    // ...
}
```

Three design points:

The `bash -c` wrapper supports full shell syntax — pipes, logical AND, environment variables, redirection — so the LLM never needs to split commands apart.

Dual-timeout backstop: the engine layer's `toolTimeout` (default 60s) and the tool's own `bashHardTimeout` (30s) — whichever is shorter actually takes effect. This is the safety net for blocking commands like `tail -f`, `top`, or a running web server.

When a command fails, the tool returns `(string, nil)` rather than `(string, error)` — this is the implementation detail behind the YOLO philosophy. Returning `nil` means the Registry produces a `ToolResult` with `IsError=false`, and the error content (including exit code and stderr) enters the context as plain text for the LLM to read and act on. There's no half-measure sandboxing here: the bash tool inherently grants full shell access, and a simple `cd /` would escape `workDir` anyway, so a command allowlist would only create a false sense of security. If path-level security is what you need, use `read_file` and `write_file` instead.

![Diagram: the bash tool's dual-timeout guard](/blog/tool-calling/images/bash-timeout-guard-07.jpg)



---

## 8. A Bird's-Eye View of the Whole Tool System

Putting all the layers above together:

```
LLM Provider
    │ ToolCall[] (Arguments as json.RawMessage)
    ▼
AgentEngine.executeTools
    ├── goroutine[0] → toolCtx (independent timeout) → Registry.Execute → BashTool
    ├── goroutine[1] → toolCtx (independent timeout) → Registry.Execute → ReadFileTool
    │                                             └── safePath → RLockPath → os.Open
    └── goroutine[2] → toolCtx (independent timeout) → Registry.Execute → EditFileTool
                                                  └── safePath → LockPath → fuzzyReplace
    ↓ sync.WaitGroup.Wait()
results[0..n] (pre-allocated, race-free)
    │ passed back verbatim when IsError=true
    ▼
ContextHistory (RoleUser + ToolCallID correlation)
    │
    ▼
Next round of LLM reasoning
```

![Diagram: bird's-eye view of the tool calling system architecture](/blog/tool-calling/images/toolcalling-overview-08.jpg)

---

## Conclusion

harness9's tool calling system is a microcosm of the framework's "simple but complete" principle: 3 core types, 2 interfaces, and 1 concurrent execution function, backed by the security foundation formed by safePath and path-level locking, with edit_file's four-tier fuzzy matching handling the uncertainty of LLM output.

A question worth pondering: as the number of tools grows into the dozens and an LLM might issue as many as 10 concurrent calls at once, what's the optimal configuration for `maxConcurrentTools` and `toolTimeout`? The answer depends heavily on the I/O characteristics of the specific tools involved and the calling habits of the model in question.
