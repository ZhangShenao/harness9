---
title: "Hooks and Human-in-the-Loop: harness9's Tool Permission Interception System"
date: 2026-06-29
tags: [harness9, agent, golang, hooks, permission, human-in-the-loop]
summary: "harness9's permission system is built from five layers: the onion-model HookRegistry, the three-tier HookDecision, DangerHook's 19 substring rules, PermissionHook's dynamic allowlist, and hard-coded tool-layer protection. This post breaks down the architectural decisions and trade-offs behind each layer."
---

# Hooks and Human-in-the-Loop: harness9's Tool Permission Interception System

## About harness9

harness9 is a Local-First, lightweight, feature-complete, production-ready general-purpose Go Agent framework.

- **Website**: [https://zhangshenao.github.io/harness9/](https://zhangshenao.github.io/harness9/)
- **GitHub**: [https://github.com/ZhangShenao/harness9](https://github.com/ZhangShenao/harness9)

⭐ Stars are the most direct way to support this open-source project. Issues and PRs are welcome.

---

## TL;DR

- HookRegistry wraps the tool registry (Registry) using an onion model: each layer of hooks is checked in order before tool execution, then unwound in reverse order afterward — the hook chain is completely transparent to the engine
- The three-tier HookDecision (Allow / Deny / Ask) is passed via context key-values; two independent flags, `withApproved` and `withExplicitlyAllowed`, prevent the same tool call from being intercepted repeatedly
- DangerHook uses 19 substring-matching rules to cover the most frequent high-risk bash operations, deliberately avoiding regex — speed first, and false positives are preferred over false negatives
- PermissionHook (`internal/permission`) reloads the JSON rules file from disk on every tool call, so an "always allow" entry written to the allowlist takes effect immediately without restarting the process
- Hard protection for sensitive paths (`~/.ssh`, `~/.aws`, etc.) is implemented at the tool layer (`safe_path.go`) rather than the hook layer — it's the last line of defense and is unaffected by rule configuration
- A Sub-Agent's tool set can only be a subset of its parent agent's available tools; permissions only tighten monotonically down the delegation chain and can never be escalated

## What you'll learn

- How the control flow of the HookRegistry onion model works, and how context flags prevent multiple hook layers from popping up duplicate approval dialogs for the same tool call
- Why DangerHook chose substring matching over regular expressions
- PermissionHook's dynamic reload mechanism: how a single "always allow" click flows from the TUI to disk and back into the next tool call
- The orthogonal relationship between the four PermissionMode modes and PlanMode
- How harness9 enforces one-way permission tightening down the Sub-Agent delegation chain

---

## The onion model: HookRegistry's architectural decision

The most intuitive way to intercept tool calls is to add an `if` check inside `Execute`. harness9 doesn't do that.

HookRegistry wraps the original `tools.Registry` using the decorator pattern, and itself implements the `tools.Registry` interface, making it completely transparent to the engine. The engine calls `registry.Execute(ctx, call)` without knowing — or needing to know — how many hook layers it passes through.

```go
// HookRegistry wraps the original Registry with a hook chain, implementing the tools.Registry interface.
// With zero hooks, its behavior is identical to the original Registry.
type HookRegistry struct {
    inner tools.Registry
    hooks []ToolHook
}

func (r *HookRegistry) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
    executed := 0
    for _, h := range r.hooks {
        newCtx, dec, err := h.BeforeExecute(ctx, call)
        // ... decision handling
        executed++
    }
    result := r.inner.Execute(ctx, call)
    for i := executed - 1; i >= 0; i-- {
        result = r.hooks[i].AfterExecute(ctx, call, result)
    }
    return result
}
```

Before execution, each hook layer is walked inward in order; after execution, the layers are unwound from the inside out — a standard onion model. `executed` tracks how many hooks completed their pre-check, ensuring only hooks that actually ran their pre-check receive a post-execution callback. A Deny short-circuit skips all post-execution callbacks, keeping the semantics consistent.

![Onion model: HookRegistry control flow](/blog/hooks-human-in-the-loop/images/hook-registry-onion-01.png)


---

## The three-tier decision: HookDecision's design philosophy

A hook's BeforeExecute returns a `HookDecision`, which has only three possible values: Allow, Deny, or Ask.

```go
type HookDecision struct {
    Action       HookAction
    Reason       string
    RiskLevel    string          // "low" | "medium" | "high"
    ModifiedArgs json.RawMessage // tool arguments modified by the hook
}
```

Allow and Deny are semantically straightforward. Ask is the most interesting design.

Ask doesn't pop up a dialog directly — it delegates the decision to an `ApprovalFunc` injected into the context. A hook doesn't depend on the TUI and has no idea what the approval UI looks like; it only knows "this operation needs a human decision." The TUI injects the approval callback into the context at startup via `hooks.WithApprovalFn(ctx, fn)`, and hooks pull it back out of the context to invoke it.

This design fully decouples hooks from the rendering layer. In a hermetic eval environment, `ApprovalFunc` can be swapped for an auto-approving mock without changing a single line of hook code.

### Context flags prevent duplicate interception

When multiple hooks are stacked, each one could potentially trigger an Ask for the same tool call. harness9 handles this with two independent context keys:

```go
// written after a human approves in real time
func withApproved(ctx context.Context) context.Context { ... }

// written after a rule explicitly allows (allowlist hit)
func withExplicitlyAllowed(ctx context.Context) context.Context { ... }
```

When an Ask decision is triggered, these two flags are checked first:

```go
case HookActionAsk:
    if isApproved(newCtx) || isExplicitlyAllowed(newCtx) {
        break // already approved or already allowed, skip approval
    }
    if fn := ApprovalFnFromContext(newCtx); fn != nil {
        resp := fn(newCtx, call, dec.Reason, dec.RiskLevel)
        // ...
        newCtx = withApproved(newCtx)
    }
```

The two flags carry different semantics: `withApproved` is the result of a user clicking "allow" in real time; `withExplicitlyAllowed` is the result of an allowlist rule silently permitting the call. Both prevent subsequent hooks from popping up another dialog, but their origin is traceable — the behavior log never conflates the two.

![HookDecision three-tier decision state machine](/blog/hooks-human-in-the-loop/images/hook-decision-state-02.png)


---

## DangerHook: why not regex

DangerHook ships with 19 built-in patterns that detect the most common high-risk bash operations. The core data structure is deliberately plain:

```go
type dangerPattern struct {
    substr    string
    riskLevel string
    reason    string
}

var defaultDangerPatterns = []dangerPattern{
    {substr: "rm -rf",   riskLevel: "high",   reason: "forceful recursive file/directory deletion"},
    {substr: "| bash",   riskLevel: "high",   reason: "piping to execute a remote script (curl|bash attack)"},
    {substr: ":(){ :|:", riskLevel: "high",   reason: "fork bomb"},
    {substr: "sudo ",    riskLevel: "medium", reason: "executing a command with root privileges"},
    {substr: "systemctl ", riskLevel: "medium", reason: "managing system services"},
    // ...19 rules total
}
```

The detection logic uses `strings.Contains` for substring matching, with the command first lowercased:

```go
cmd := strings.ToLower(args.Command)
for _, p := range h.patterns {
    if strings.Contains(cmd, strings.ToLower(p.substr)) {
        return ctx, Ask(p.reason, p.riskLevel), nil
    }
}
```

Avoiding regex is a deliberate engineering trade-off. Regex is more precise, but in the context of tool interception, false positives are preferable to false negatives — asking the user one extra time is low cost, while missing a single `rm -rf` is high cost. Substring matching produces false positives (for example, a command containing the literal string `"sudo"` in a comment), but it never misses a match. It's also faster, which matters since it runs on every single tool call.

The two variants that cover whitespace differences (`"| bash"` and `"|bash"`) reflect the same philosophy: better to write one extra rule than leave room for evasion.

![DangerHook's 19 tiered patterns](/blog/hooks-human-in-the-loop/images/danger-hook-patterns-03.png)


---

## PermissionHook: JSON allowlist and dynamic reload

DangerHook is static — its rules are hard-coded. PermissionHook is dynamic — its rules are loaded from a JSON configuration file, re-read from disk on every tool call.

The configuration format is straightforward:

```json
{
  "permissions": {
    "allow": ["bash(git *)", "read_file"],
    "deny":  ["bash(rm -rf *)"],
    "ask":   ["bash(sudo *)"]
  }
}
```

The rule syntax has two levels:
- `"toolName"` — matches any call to that tool
- `"toolName(pattern)"` — tool name match AND arguments match a glob pattern

Evaluation is ordered matching, with the first matching rule winning:

```go
func (r *Rules) Evaluate(toolName, argStr string) string {
    for _, ru := range r.rules {
        for _, p := range ru.patterns {
            if matchPattern(toolName, argStr, p) {
                return ru.action
            }
        }
    }
    return RuleAsk // default to Ask when nothing matches
}
```

`LoadRules` loads rules in the order deny → allow → ask, ensuring deny rules are always evaluated first.

### Dynamic reload: reading from disk on every call

The PermissionHook created by `NewFileHook` calls `LoadRules` to reload the file on every `BeforeExecute`:

```go
func (h *Hook) BeforeExecute(ctx context.Context, tc schema.ToolCall) (...) {
    rules := h.rules
    if h.settingsPath != "" {
        if loaded, err := LoadRules(h.settingsPath); err == nil {
            rules = loaded
        }
    }
    // ...
}
```

This looks a bit "brute force," but it's the right trade-off. When a user clicks "always allow" in the TUI, `writeApprovalToConfig` immediately writes that tool's pattern into the JSON file. The next time a similar tool call comes in, PermissionHook reads the new rule, hits the allowlist, and silently permits it without popping up a dialog. The entire flow requires no restart, no semaphores, and no state synchronization.

The cost of disk I/O is entirely acceptable at the granularity of a tool call — tool execution itself typically takes milliseconds to seconds, so one extra file read is not a bottleneck.

![PermissionHook's dynamic reload flow](/blog/hooks-human-in-the-loop/images/permission-hook-reload-04.png)


---

## Hard protection for sensitive paths: not done at the hook layer

Protection for six paths — `~/.ssh`, `~/.aws`, `~/.kube`, `~/.gnupg`, `~/.netrc`, and `~/.config/gcloud` — is not implemented at the hook layer, but in the tool layer's `safe_path.go`:

```go
var hardSensitivePaths = buildSensitivePaths()

func buildSensitivePaths() []string {
    home, _ := os.UserHomeDir()
    return []string{
        filepath.Join(home, ".ssh"),
        filepath.Join(home, ".aws"),
        filepath.Join(home, ".kube"),
        filepath.Join(home, ".gnupg"),
        filepath.Join(home, ".netrc"),
        filepath.Join(home, ".config", "gcloud"),
    }
}

func safePath(workDir, inputPath string) (string, error) {
    // ... path normalization ...
    if isSensitivePath(absPath) {
        return "", fmt.Errorf("path '%s' is a protected sensitive path, access denied", inputPath)
    }
    return absPath, nil
}
```

This is a deliberate architectural decision. Hook-layer rules can be configured, and can be bypassed by BypassAll mode; but the `safePath` check runs inside the tool itself, returning an error directly, without going through the hook chain, and unaffected by PermissionMode. Protection of credential files is unconditional.

Path traversal protection lives in the same function: after `filepath.Abs` normalization, it checks whether the result still has `workDir` as a prefix, handling absolute and relative paths separately to prevent `../` escapes and the classic absolute-path-doubling trap (`workDir + "/workDir/subpath"`).

![Dual defense for sensitive paths](/blog/hooks-human-in-the-loop/images/sensitive-path-guard-05.png)

---

## The TUI's five-option approval dialog

When a user sees the approval dialog in the TUI, there are five options:

![Tool call approval](/blog/hooks-human-in-the-loop/images/tool_call_approval.png)


Option 5 opens an inline input box where the user can explain the reason for rejection in natural language. This feedback is passed back to the engine via `ApprovalResponse.Feedback`, injected into the context as the tool call's error result in the form `"operation rejected by user: <feedback>"`. The LLM can read this text and adjust its strategy on the next turn.

The dialog title's color changes based on `RiskLevel`: high risk is red, medium risk is amber, low risk is cyan. This signal originates from DangerHook's `riskLevel` field and flows straight through from the detection layer to the rendering layer without any extra transformation.

```go
func (m tuiModel) renderApprovalDialog() string {
    switch req.RiskLevel {
    case "high":
        titleStyle = approvalTitleHighStyle
    case "medium":
        titleStyle = approvalTitleMedStyle
    default:
        titleStyle = approvalTitleLowStyle
    }
    // ...
}
```

Option 3's implementation is worth noting — `confirmApproval(2)` calls `writeApprovalToConfig`, which writes this tool call's pattern into the JSON configuration:

```go
case 2:
    resp = hooks.ApprovalResponse{Approved: true, Remember: true}
    m.writeApprovalToConfig(req)
```

`writeApprovalToConfig` takes the first word of the command for bash tools as a keyword and generates a glob pattern like `bash(*git*)`, appending it to the allow list. The granularity isn't fine, but it's good enough in practice — the user says "always allow git operations," and from then on all `git *` commands pass silently.

![TUI five-option approval dialog interaction flow](/blog/hooks-human-in-the-loop/images/tui-approval-dialog-06.png)


---

## The four PermissionMode modes

`PermissionMode` is defined in `internal/engine/permission.go` and is orthogonal to `planning.PlanMode` — the two dimensions independently control different behaviors:

```go
const (
    PermissionModeDefault     PermissionMode = iota // dangerous operations trigger an approval dialog
    PermissionModeAutoApprove                       // allowlisted operations pass automatically, others still require approval
    PermissionModeReadOnly                          // deny all write operations
    PermissionModeBypassAll                          // bypass all permission checks
)
```

Use cases for the four modes:

- **Default**: everyday development. DangerHook and PermissionHook work normally, and dangerous operations trigger a dialog.
- **AutoApprove**: the current workspace is already fully trusted, so allowlisted operations don't need confirmation every time. Toggled via the TUI `/approve` command.
- **ReadOnly**: code review or read-only analysis tasks. Explicitly blocks any file modification to prevent the agent from writing unintentionally.
- **BypassAll**: Docker Sandbox or controlled CI environments. The container itself already provides isolation guarantees, so software-layer permission checks aren't needed, trading them away for the lowest possible tool-call latency.

The existence of BypassAll reveals an important design stance: harness9's permission system is designed for human-agent collaboration scenarios, not fully automated ones. When the runtime environment already provides isolation, software-layer permission checks are pure overhead and can be turned off.

![Orthogonal relationship between PermissionMode and PlanMode](/blog/hooks-human-in-the-loop/images/permission-mode-matrix-07.png)


---

## One-way permission tightening for Sub-Agents

A Sub-Agent's tool set is computed in `SubAgentDefinition.ResolveTools`:

```go
// formula: available tool set = (parent's tool set ∩ allowlist) - denylist - {task}
```

This rule guarantees that permissions can only shrink down the delegation chain, never grow. A tool the parent agent doesn't have can never be granted to a sub-agent, either. Even if `.harness9/agents/dev.md` lists a tool in its allowlist, if the parent agent never registered that tool at startup, the sub-agent still won't get it.

The `task` tool is forcibly removed from every sub-agent's tool set, preventing recursive delegation (a Sub-Agent spawning further Sub-Agents). This isn't a technical impossibility — it's a deliberate design constraint. Unbounded recursive delegation would make permission auditing extremely complex, and in practice there's almost no real need for delegation chains more than two levels deep.

From the hook's perspective, each Sub-Agent runs on its own isolated engine instance with its own HookRegistry. The main agent's approval decisions (`withApproved`, `withExplicitlyAllowed`) never leak into a sub-agent's context — every delegation is, in permission terms, a fresh session.

![Sub-Agent permission inheritance chain](/blog/hooks-human-in-the-loop/images/subagent-permission-chain-08.png)


---

## Overall architecture overview

From the moment a tool call is triggered to its final execution, the complete path is:

```
LLM issues a ToolCall
    ↓
engine.Execute(ctx, call)
    ↓
HookRegistry (onion entry point)
    ↓ BeforeExecute
PermissionHook → LoadRules (disk) → Evaluate → Allow/Deny/Ask
    ↓ continues on Allow
DangerHook → substring match against 19 patterns → Allow/Ask
    ↓ on Ask
ApprovalFunc (TUI) → user picks from five options → ApprovalResponse
    ↓ on Approved
tools.Registry.Execute (actual tool)
    ↓
safePath (path traversal + sensitive paths) ← last line of defense, cannot be bypassed
    ↓
each hook's post-execution cleanup (from inside out)
    ↓
ToolResult returned to the LLM
```

Every layer can be independently replaced. PermissionHook can be swapped for any struct implementing the `ToolHook` interface, DangerHook can be overridden with custom rules, and `ApprovalFunc` maps to different UIs or automatic decision logic depending on the runtime environment. `safePath` is the only layer that cannot be replaced — it lives inside the tool itself, outside of any interface.

![Complete tool-call permission interception path](/blog/hooks-human-in-the-loop/images/full-permission-pipeline-09.png)


---

## Conclusion

harness9's permission system has no central policy engine, no DSL, no rule language. It's an orthogonal combination of a few simple mechanisms: the onion model + context-carried decision state + a disk-persisted allowlist + unconditional hard protection inside the tools themselves.

The question really worth thinking about is: which security constraints should be configurable, and which must be hard-coded? harness9's answer is that credential files are non-configurable, and everything else is.

---
