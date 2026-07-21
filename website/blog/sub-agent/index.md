---
title: "Sub-Agent: How harness9 Lets the Main Agent Delegate Work"
date: 2026-07-06
tags: [harness9, agent, golang, SubAgent, task-delegation]
summary: "harness9's Sub-Agent system reduces every delegation to an ordinary AgentEngine instance running on an isolated Session, and the delegation entry point itself is an ordinary tool, isomorphic to bash or read_file. This post first lays out the overall mental model along the two main threads of 'creation' and 'delegation', then works through the task tool design, ResolveTools permission narrowing, context isolation, the Runner's dual Context derivation, and TaskTracker's channel-free concurrency model."
---

# Sub-Agent: How harness9 Lets the Main Agent Delegate Work

## About harness9

harness9 is a Local-First, lightweight, feature-complete, production-ready general-purpose Go Agent framework.

- **Website**: [https://zhangshenao.github.io/harness9/](https://zhangshenao.github.io/harness9/)
- **GitHub**: [https://github.com/ZhangShenao/harness9](https://github.com/ZhangShenao/harness9)

⭐ Stars are the most direct way to support this open-source project. Issues and PRs are welcome.

---

## TL;DR

- A Sub-Agent is not a new component — it's an ordinary `engine.AgentEngine` instance running on an isolated Session, reusing the same `RunStream` pipeline, without changing a single line of `runLoop`
- There are only two ways to create a Sub-Agent: built-in programmatic registration (`general-purpose`) and file-based definitions under `.harness9/agents/*.md`; both converge into the same `Registry`
- There are only two ways to delegate to a Sub-Agent: the main LLM decides semantically via the `task` tool, or the user bypasses the LLM entirely and runs it directly in the foreground with `@agent`
- The delegation entry point `task` is just an ordinary `tools.BaseTool`, completely transparent to the Engine, dispatched through the same path as bash or read_file — there is no dedicated "delegation protocol"
- `ResolveTools` narrows permissions in three steps — "allowlist ∩ full set − denylist − task" — with the `task` tool hard-coded to be permanently removed, banning recursive delegation at the root, i.e. a Sub-Agent can never spawn another Sub-Agent
- A Sub-Agent cannot see the main agent's conversation history or system prompt; the `prompt` parameter is the sole information channel, and an independent `MemorySession` guarantees zero context leakage
- Foreground execution reuses the parent caller's ctx approval channel; background execution always fails closed on approval requests; both paths share the same `Runner.Run` implementation
- `TaskTracker` replaces channels with `sync.Mutex` to manage background task state, eliminating send-on-closed-channel risk at the root

## What You'll Learn

- By the end of the opening section, you'll have a complete mental model of "how a Sub-Agent is created" and "how a Sub-Agent is delegated to" — no need to read to the end to piece it together
- How the `task` tool's parameter schema is designed, how `Execute` switches between blocking in the foreground and returning immediately in the background, and why delegation is built as an ordinary tool rather than a new protocol
- How `ResolveTools` guarantees that permissions can only narrow, never expand, along the delegation chain
- The Runner's two `execCtx` derivation strategies for foreground/background, and why it bypasses the parent tool's 60-second timeout
- Why `TaskTracker` abandons channels in favor of a mutex-protected in-memory buffer

---

## Why Have a Sub-Agent at All?

An agent's context window is an extremely scarce resource. A single multi-step exploration — reading ten files, running five bash commands, trial and error along the way — if all of that intermediate process gets stuffed into the main conversation history, it both pollutes the quality of subsequent reasoning and eats up the token budget.

Mainstream harness frameworks all converge on the same solution: outsource a well-bounded subtask wholesale, and only bring back a concise conclusion. harness9 fully inherits this core idea, but makes one key engineering decision — **a Sub-Agent is not a newly built executor; it is the very same `engine.AgentEngine` reused from the main agent**.

```go
sub := engine.NewAgentEngine(p, childReg, r.workDir, opts...)
stream, err := sub.RunStream(execCtx, prompt)
```

These two lines in `Runner.Run` are the entire execution kernel of a Sub-Agent. There is no separate Sub-Agent loop, no dedicated scheduler. The Sub-Agent and the main agent run the exact same standard ReAct loop; the only differences are a narrower tool set, a cleaner Session, and a different Context derivation strategy.

![Main agent and Sub-Agent share the same AgentEngine execution kernel](/blog/sub-agent/images/shared-engine-kernel-01.png)


---

## Creation and Delegation: Two Mental Models

First, let's separate two things that are easy to conflate: **creating a Sub-Agent** and **delegating a task** are two independent paths — one happens at startup, the other at runtime.

### Creation: Where Does a Sub-Agent Come From

harness9 recognizes only two "origins", both of which ultimately flow into the same `Registry`:

| Creation method | Vehicle | When registered | Use case |
|---------|------|---------|---------|
| Programmatic built-in | A `SubAgentDefinition{}` literal in `main.go` | At startup, via `subAgentReg.Register(...)` | Built-in `general-purpose`, a general-purpose task Sub-Agent |
| File-based definition | `.harness9/agents/*.md` (YAML frontmatter + body) | At startup, scanned and loaded by `Registry.LoadFromDir` | Project-side custom specialized roles (security auditing, documentation writing, etc.) |

Both methods lead to the same place: constructing a `SubAgentDefinition` and dropping it into the `Registry.defs` map. The `Registry` itself is simple:

```go
type Registry struct {
    defs map[string]SubAgentDefinition
}

func (r *Registry) Register(def SubAgentDefinition) error { /* Validate + dedup */ }
func (r *Registry) Get(name string) (SubAgentDefinition, bool)
func (r *Registry) List() []SubAgentDefinition
```

The convention is: register once at startup, read-only at runtime — consistent with the convention for `tools.Registry` — no locking is needed because there's no concurrent writing at runtime. File-based definitions also have an override rule: a file definition with the same name overrides the programmatic definition (logged, not an error), so to change built-in behavior you just write a `.harness9/agents/general-purpose.md` — no need to touch Go code.

The creation stage answers exactly one question: what Sub-Agent types exist in the system, and what are their permission boundaries and role definitions. It has nothing to do with executing a specific task.

### Delegation: How a Task Gets Dispatched

Once creation is done, the only question left at runtime is: should this task be outsourced, to whom, and how. harness9 offers two paths:

| Delegation method | Triggered by | Goes through main LLM decision? | Execution mode |
|---------|--------|---------------------|---------|
| `task` tool | The main LLM's ToolCall | Yes — the LLM autonomously chooses `subagent_type` and `prompt` | Foreground blocking (default) or background async (`background=true`) |
| `@agent` direct run | User input `@agent-name task description` | No — completely bypasses the LLM's tool decision | Foreground blocking only |

The `task` tool is the sole interface the delegation system exposes to the main LLM — it is both the entry point for initiating delegation and the switch between foreground and background. `@agent` is a bypass left for humans, used for "I want to see this Sub-Agent's output live right now" scenarios, at the cost of only supporting foreground execution.

Both paths ultimately lead to the same `Runner.Run` — delegation's "decision" can be initiated in two ways, but its "execution" has only one implementation.

![Creation and delegation: two independent paths converge into one Runner](/blog/sub-agent/images/creation-vs-delegation-02.png)


---

## The task Tool: Designing the Delegation Entry Point Itself

The only interface the main LLM ever touches along the delegation path is the `task` tool. `TaskTool` is defined in `internal/subagent/task_tool.go`, and represents the entire capability the Sub-Agent delegation system exposes externally — worth understanding clearly before diving into the Runner's internal mechanics.

### Parameter Schema: Four Fields Draw the Boundaries of Delegation

`Definition()` is **dynamically generated** on every call, folding the `Name` of every currently registered Sub-Agent into the enum values of `subagent_type`:

```go
func (t *TaskTool) Definition() schema.ToolDefinition {
    defs := t.reg.List()
    names := make([]string, 0, len(defs))
    var sb strings.Builder
    sb.WriteString("Delegate a well-bounded task to a specialized Sub-Agent. The Sub-Agent has its own independent context and a restricted tool set.\nAvailable Sub-Agents:\n")
    for _, d := range defs {
        names = append(names, d.Name)
        fmt.Fprintf(&sb, "- %s: %s\n", d.Name, d.Description)
    }
    return schema.ToolDefinition{
        Name:        "task",
        Description: sb.String(),
        InputSchema: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "subagent_type": map[string]any{
                    "type": "string", "enum": names,
                    "description": "The Sub-Agent type name to invoke",
                },
                "description": map[string]any{
                    "type": "string", "description": "A short title for the task (3-5 words, for UI display)",
                },
                "prompt": map[string]any{
                    "type":        "string",
                    "description": "The full task description passed to the Sub-Agent. The Sub-Agent cannot see the main conversation history, so all necessary information (file paths, background, requirements) must be written here.",
                },
                "background": map[string]any{
                    "type":        "boolean",
                    "description": "Whether to run asynchronously in the background. true returns immediately, with the result injected later; false (default) blocks until completion.",
                },
            },
            "required": []string{"subagent_type", "prompt"},
        },
    }
}
```

Each of the four parameters has its own distinct job, and none is redundant:

- `subagent_type` is the only required enum, and its `enum` array is computed live from `Registry.List()`, so it always stays in sync with what's actually registered — adding a new `.harness9/agents/*.md` file requires no schema code changes at all. This is also the direct interface between the "creation" and "delegation" stages: whatever's in the `Registry`, `task` can delegate to.
- `prompt` is the only required free-text parameter, the sole information channel between parent and child; its description explicitly states "the Sub-Agent cannot see the main conversation history" — translating an architectural constraint into a prompt the LLM can understand.
- `description` is purely a UI decoration field, display-only, not involved in execution logic — cleanly separated from the mechanism fields.
- `background` defaults to `false` (blocking), controlling the Sub-Agent's execution mode (foreground/background).

The `Description` itself is also carefully designed: it's not a static string, but concatenates the `Name` and `Description` of every registered Sub-Agent into the tool description. Whether the LLM decides "should I delegate, and to whom" relies entirely on this dynamically-assembled text — there's no additional retrieval or recommendation mechanism.

### Execute: Two Execution Semantics in One Function

The `Execute` skeleton is short, but one branch determines two completely different execution paths:

```go
func (t *TaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
    var a taskArgs
    if err := json.Unmarshal(args, &a); err != nil {
        return "", fmt.Errorf("failed to parse arguments: %w", err)
    }
    if a.SubAgentType == "" {
        return "", fmt.Errorf("subagent_type must not be empty")
    }
    if a.Prompt == "" {
        return "", fmt.Errorf("prompt must not be empty")
    }
    def, ok := t.reg.Get(a.SubAgentType)
    if !ok {
        return "", fmt.Errorf("unknown Sub-Agent type %q, available: %s", a.SubAgentType, t.agentNames())
    }

    if a.Background {
        taskID := t.tracker.Start(def.Name, a.Prompt)
        go func() {
            defer func() {
                if rec := recover(); rec != nil {
                    t.tracker.Finish(taskID, fmt.Sprintf("Sub-Agent background execution panicked: %v", rec), true)
                }
            }()
            sink := func(u schema.SubAgentUpdate) { t.tracker.AppendLog(taskID, u) }
            bgCtx := hooks.WithSubAgentProgress(context.Background(), sink)
            res, err := t.runner.Run(bgCtx, def, a.Prompt, true)
            if err != nil {
                t.tracker.Finish(taskID, err.Error(), true)
            } else {
                t.tracker.Finish(taskID, res.FinalText, false)
            }
        }()
        return fmt.Sprintf(`<task id=%q state="running"/>`, taskID), nil
    }

    res, err := t.runner.Run(ctx, def, a.Prompt, false)
    if err != nil {
        return fmt.Sprintf(`<task state="error">%s</task>`, err.Error()), nil
    }
    return fmt.Sprintf(`<task state="completed"><task_result>%s</task_result></task>`, res.FinalText), nil
}
```

The first half is standard argument validation and lookup. The important part is the `if a.Background` branch:

- **Foreground execution**: directly calls `t.runner.Run(ctx, def, a.Prompt, false)`, synchronously waits for the result, and wraps `FinalText` into an XML-style string that's returned right away. `Execute` is an ordinary function invoked synchronously by the Engine, so taking this branch means the current Turn is blocked here until the Sub-Agent finishes running.
- **Background execution**: first calls `t.tracker.Start` to get a `taskID`, spawns a goroutine to run asynchronously, and `Execute` itself **returns immediately** with a placeholder result of `state="running"`. The Sub-Agent's actual execution is completely decoupled from this call's lifecycle.

The background branch also has a safeguard: `recover()` catches panics inside the goroutine and converts them into `Finish(taskID, ..., true)` written back to the tracker, rather than letting them crash the process directly — this is a goroutine running independently, detached from the main call stack, with nobody waiting to catch its panic.

### Why an Ordinary Tool, Not a New Protocol

The only special thing about `TaskTool` is that it implements the `tools.BaseTool` interface — `Name()` / `Definition()` / `Execute()` — which looks exactly like `bash`, `read_file`, or `write_file`. This seemingly plain decision is actually the key to letting the entire delegation system integrate with the Engine with zero intrusion:

- The Engine's main loop, hook chain, timeout control, concurrent scheduling, approval dialogs, and TUI rendering are all built around a single abstraction: "tool call". The cheapest way to reuse this infrastructure for delegation is to make delegation look like a tool call.
- `HookRegistry` doesn't need any special branch for `task`. However `danger_hook` and `offload_hook` wrap `bash`, they wrap `task` the same way — the only place `task` is treated specially is inside the Sub-Agent (the `denyTaskHook` refuses to let it be called again); from the parent agent's perspective, it's just an ordinary tool.
- The LLM side doesn't need to learn a new protocol either. The model already knows how to emit ToolCalls, so making delegation a tool call avoids extra prompt engineering to teach it a new interaction mode — calling `task` and calling `bash` follow the exact same cognitive path.

In short: **making delegation a tool means using an already-proven abstraction (tool calling) to carry a new capability (Sub-Agent delegation), rather than inventing a new abstraction from scratch**. This is entirely consistent with harness9's philosophy of "minimizing the abstraction layer".

### Return Value Design: Two Terminal States, One Protocol

The `task` tool's return value uses a lightweight XML-style markup, with three terminal states:

```
<task state="completed"><task_result>...</task_result></task>   // foreground success
<task state="error">...</task_result></task>                     // foreground failure
<task id="task-general-purpose-1" state="running"/>               // background, returns immediately
```

Both foreground terminal states are **synchronously** stuffed into this tool call's output, readable by the parent agent immediately on the next turn; the background `running` state is just a handle — the actual result doesn't come back through this `Execute` call, but rather after `TaskTracker.Finish`, gets drained via `DrainCompleted()` before the next `dispatch()` call and prepended to the LLM's prompt (concurrency details covered later in the TaskTracker section).

This design decouples the hard constraint "a tool call must synchronously return a string" from the semantic requirement "a background task is inherently asynchronous" — `task` always returns synchronously, but the returned content is either the final result or a pointer to a future result (`taskID`); how that pointer gets redeemed is entirely handled by `TaskTracker`. This is also why `TaskTool` holds three collaborators simultaneously — `Registry`, `Runner`, and `TaskTracker` — `Execute` itself is purely orchestration, holding no state of its own.

![The task tool as delegation entry point: parameter schema and two return protocols](/blog/sub-agent/images/task-tool-entrypoint-03.png)


---

## general-purpose: The Built-in General Sub-Agent

Having covered the delegation entry point, let's return to the first creation method — programmatic built-in registration. harness9 ships a built-in `general-purpose` Sub-Agent, on par with the equivalent capability in Claude Code and DeepAgents — when there's no more specialized Sub-Agent available, it's the default delegation target.

Its definition is almost entirely empty values:

```go
subAgentReg.Register(subagent.SubAgentDefinition{
    Name:         "general-purpose",
    Description:  "A general-purpose Sub-Agent for tasks that require balancing exploration and modification, complex reasoning, or multi-step dependencies...",
    SystemPrompt: generalPurposeSystemPrompt,
    Source:       "builtin", // Tools/Model/MaxTurns are all left empty
})
```

Leaving `Tools` empty means inheriting **all** of the parent agent's available tools; leaving `Model` empty means inheriting the parent agent's model; leaving `MaxTurns` empty means inheriting the engine's default turn count. It doesn't shrink the boundary of what it can do — it only shrinks the **scope of context**. The value of delegating to it isn't about restricting what it can do, but about isolating the lengthy intermediate process inside a sub-session and returning only a single conclusion.

When a domain-specific Sub-Agent is needed (security auditing, documentation writing), harness9's recommendation is to go with a file-based definition rather than piling more programmatic built-ins into the kernel — keep the core lean, and leave specialization to the project side.

---

## File-Based Definitions: One Markdown File Is One New Agent

The second creation method. Create a `.harness9/agents/security-auditor.md` in the working directory, and harness9 automatically scans and loads it at startup:

```markdown
---
name: security-auditor
description: Security auditing expert. Use after code changes involving authentication, authorization, or input validation.
tools: read_file, bash
disallowed_tools: write_file, edit_file
model: openai/gpt-4o
max_turns: 30
skills: security-review
---

You are an application security engineer focused on identifying security vulnerabilities in code.
When reviewing, output findings by priority: Critical > High > Medium > Low, each with a CWE number and remediation advice.
Do not modify files — only produce the audit report.
```

The parsing logic lives in `parseAgentFile`, a deliberately plain hand-written parser — it cuts out the frontmatter using `---\n` delimiters, splits each line on `:` into key/value pairs, and splits list fields on commas:

```go
func parseAgentFile(content string) (SubAgentDefinition, error) {
    const delim = "---\n"
    if !strings.HasPrefix(content, delim) {
        return SubAgentDefinition{}, fmt.Errorf("missing frontmatter opening delimiter")
    }
    // ... locate the closing delimiter, the body becomes SystemPrompt
    for _, line := range strings.Split(fm, "\n") {
        k, v, ok := strings.Cut(line, ":")
        // ...
    }
}
```

No YAML library was introduced, because the frontmatter field set is fixed and flat — a hand-written parser is more in line with the "minimize the abstraction layer" principle than pulling in `gopkg.in/yaml.v3`. When `LoadFromDir` scans, a missing directory silently returns nil — it works with zero configuration; a single file failing to parse only logs a warning without aborting; a file definition overrides a programmatic definition of the same name (logged), so a project can directly override the built-in `general-purpose` with a file definition.

![The loading path for file-based Sub-Agent definitions, from Markdown to the registry](/blog/sub-agent/images/file-based-agent-loading-04.png)


---

## ResolveTools: Permissions Can Only Narrow, Never Expand

Now let's move into the deeper implementation — what the Runner actually does once delegation happens. First, permission computation, which is the single most critical piece of code in the whole system. A Sub-Agent's tool set isn't declared arbitrarily; it's a deterministic three-step narrowing algorithm:

```go
func (d SubAgentDefinition) ResolveTools(all []string) []string {
    allowed := make(map[string]bool, len(all))
    for _, t := range all {
        allowed[t] = true
    }

    var base []string
    if len(d.Tools) > 0 {
        for _, t := range d.Tools {
            if allowed[t] {
                base = append(base, t)
            }
        }
    } else {
        base = append(base, all...)
    }

    denied := map[string]bool{"task": true}
    for _, t := range d.DisallowedTools {
        denied[t] = true
    }

    result := make([]string, 0, len(base))
    for _, t := range base {
        if !denied[t] {
            result = append(result, t)
        }
    }
    return result
}
```

Three steps: intersect the `Tools` allowlist with the parent's full set (or take the full set if left empty); subtract the `DisallowedTools` denylist; and `task` is always forcibly stuffed into the `denied` map, regardless of whether the definition file mentions it.

This function's input is `all []string` — the parent agent's set of available tools — so **a Sub-Agent's permissions can never exceed those of the main agent, and a Sub-Agent is not allowed to recursively delegate again**. There's no way for a Sub-Agent to obtain a tool the parent agent doesn't have. Intersection is inherently non-expanding, and that's the mathematical foundation of "the delegation chain only narrows in one direction".

The handling of `task` is a double defense. Hard-coded removal in `ResolveTools` is the first layer; `Runner.buildChildRegistry` additionally wraps a `denyTaskHook`:

```go
type denyTaskHook struct{}

func (denyTaskHook) BeforeExecute(ctx context.Context, tc schema.ToolCall) (context.Context, hooks.HookDecision, error) {
    if tc.Name == "task" {
        return ctx, hooks.Deny("Sub-Agents are not allowed to spawn further Sub-Agents"), nil
    }
    return ctx, hooks.Allow(), nil
}
```

Here's an idea worth remembering: even if some future refactor accidentally lets `task` slip into a Sub-Agent's tool registry, this hook can still intercept it before execution. Banning recursion doesn't rely on a single checkpoint as a backstop — it relies on two independent mechanisms stacked on top of each other.

![The three-step ResolveTools narrowing algorithm](/blog/sub-agent/images/resolve-tools-narrowing-05.png)


---

## Complete Context Isolation: prompt Is the Only Information Channel

A Sub-Agent is not a branch extension of the main conversation — it's an entirely new session:

```go
childID := fmt.Sprintf("subagent-%s", def.Name)
childSession := memory.NewMemorySession(childID)
```

`NewMemorySession` is a pure in-memory Session, carrying none of the parent agent's conversation history and none of the parent agent's system prompt. The Sub-Agent's system prompt is assembled separately by `promptBuilder`:

```go
func (b *promptBuilder) Build() string {
    var sb strings.Builder
    sb.WriteString(b.systemPrompt)
    fmt.Fprintf(&sb, "\n\nWorking directory: %s", b.workDir)
    if b.loader != nil {
        for _, name := range b.skills {
            body, err := b.loader(name)
            if err != nil || strings.TrimSpace(body) == "" {
                continue
            }
            fmt.Fprintf(&sb, "\n\n## Preloaded skill: %s\n\n%s", name, body)
        }
    }
    // ... Sandbox environment description (optional)
    return sb.String()
}
```

`promptBuilder` implicitly satisfies the `engine.PromptBuilder` interface via structural typing, without `import engine` — this is a technique harness9 uses consistently to avoid circular dependencies (subagent depends on engine, engine never depends back on subagent).

Put plainly, **a Sub-Agent knows nothing about the parent conversation when it starts. The `prompt` parameter of the `task` tool is the sole information channel between parent and child** — file paths, background information, task requirements, all of it has to be explicitly written into the prompt string by the LLM. This isn't an oversight — it's a deliberate constraint: **the value of context isolation lies precisely in forcing the main LLM to describe the task clearly, rather than counting on "the Sub-Agent can see the whole history anyway"**.

---


## Foreground Execution vs Background Execution

The `task` tool supports a `background` parameter — foreground blocking or background async — but both paths run through the exact same `Runner.Run(ctx, def, prompt, background)` underneath. `background` is merely a switch that determines the approval strategy and result delivery method, not two separate implementations.

During foreground execution, approval requests pass through directly to the parent agent's approval channel:

```go
case engine.EventApprovalRequired:
    req, ok := evt.Data.(engine.ApprovalRequest)
    if !ok {
        continue
    }
    if background || parentApproval == nil {
        req.ResponseCh <- hooks.ApprovalResponse{Approved: false,
            Feedback: "Sub-Agent has no available approval channel, automatically denied"}
    } else {
        req.ResponseCh <- parentApproval(execCtx, req.ToolCall, req.Reason, req.RiskLevel)
    }
```

Background execution has no TUI channel to pop an approval dialog through, and harness9's strategy is **fail-closed** — always automatically deny, never auto-approve. This is a deliberate security trade-off: **it's better to have a background task fail due to insufficient permission than to allow a high-risk operation to proceed unsupervised**.

The result delivery method also differs. In the foreground, `FinalText` is directly returned synchronously as the tool result, immediately readable by the parent agent:

```go
return fmt.Sprintf(`<task state="completed"><task_result>%s</task_result></task>`, res.FinalText), nil
```

In the background, a `running` status marker is returned immediately, and the actual result travels through `TaskTracker`:

```go
return fmt.Sprintf(`<task id=%q state="running"/>`, taskID), nil
```

![The two delegation paths: foreground blocking and background async](/blog/sub-agent/images/foreground-background-paths-06.png)


---


## @agent: A Direct-Run Channel That Bypasses LLM Decision-Making

Back to the second delegation path mentioned in the opening. Besides letting the main agent decide via the `task` tool, harness9 also leaves a path for direct human command: typing `@general-purpose investigate the implementation of xxx` in the input box directly invokes the specified Sub-Agent in the foreground, completely bypassing the main LLM's tool-selection reasoning.

This path reuses exactly the same rendering logic (`subAgentLines`) as the `task` tool's foreground execution; the only difference is who triggers it — one is the LLM deciding autonomously, the other is the user specifying explicitly. The `@` syntax only supports the foreground; to run in the background you still have to fall back to natural language and let the main LLM call `task(background=true)`. The reasoning behind this limitation: the value of a background task is "I don't want to wait, let the system handle it in the background", while the value of an `@` direct run is "I want to see this Sub-Agent's live output right now" — these are two different use cases.

---

## The Full Data Flow

Let's trace through the complete delegation chain by putting all the pieces above together:

```
Main agent LLM decides to call task
    ↓
TaskTool.Execute parses subagent_type / prompt / background
    ↓
Runner.Run
    ├─ buildChildRegistry: ResolveTools(allowlist ∩ full set − denylist − task) → permission hook chain
    ├─ providerFor(def.Model): model override or inherit parent model
    ├─ newPromptBuilder: system prompt + workDir + skills body
    ├─ memory.NewMemorySession: brand-new pure in-memory Session
    └─ engine.NewAgentEngine → sub.RunStream(execCtx, prompt)
            ↓ event stream consumption
        EventActionDelta/ToolStart/ToolResult → emit(SubAgentUpdate) → TUI progress line
        EventApprovalRequired → passed through in foreground / auto-denied in background
        EventDone → channel closes, loop ends
            ↓
    Foreground: FinalText → tool result → parent agent context (immediately readable)
    Background: TaskTracker.Finish → DrainCompleted before the next dispatch → injected into the main LLM prompt
```

![The complete Sub-Agent delegation data flow](/blog/sub-agent/images/subagent-full-dataflow-07.png)


---

## Closing Thoughts

**A Sub-Agent isn't magic — its core value can be summed up in a single word: isolation**. At the creation stage, the `Registry` defines the boundary of "what Sub-Agents exist"; at the delegation stage, an ordinary `task` tool defines the boundary of "how delegation is initiated". `ResolveTools` isolates permissions, and `MemorySession` isolates context.

---
