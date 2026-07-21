# Human-in-the-Loop Permission Control

The Human-in-the-Loop (HITL) module in harness9 addresses one core problem: **how can humans retain substantive control over high-risk operations without disrupting the smooth operation of the Agent?**

Most frameworks either fully trust the Agent (YOLO mode) or pop up a confirmation dialog before every single tool call (which exhausts the human). harness9 takes a middle path: a built-in rule engine automatically decides based on risk level, and only operations that truly require human judgment pause and trigger an approval dialog.

---

## System Architecture

```
internal/hooks/
├── decision.go      # HookDecision (allow/deny/ask), ApprovalResponse, ApprovalFunc, context injection/extraction
├── hook.go          # ToolHook interface (returns HookDecision) + HookRegistry (onion-model Execute)
└── danger_hook.go   # Built-in high-risk command interception (bash pattern matching)

internal/permission/
├── rules.go         # Rules (glob matching) + LoadRules / SaveRules (JSON config)
└── hook.go          # PermissionHook (reloads config file on demand)

internal/engine/
├── stream.go        # EventApprovalRequired + ApprovalRequest payload
├── agent_loop.go    # emitter.approval field + executeTools injects ApprovalFunc
└── permission.go    # PermissionMode enum

internal/tools/
└── safe_path.go     # Hardcoded sensitive path interception (~/.ssh, ~/.aws, etc.)

cmd/harness9/
├── tui.go           # Approval dialog state fields + style variables
├── tui_update.go    # handleApprovalKey / confirmApproval / writeApprovalToConfig
└── tui_view.go      # renderApprovalDialog()
```

---

## Workflow Overview

```
Tool call request
      │
      ▼
┌─────────────────────────────────────────────────────┐
│  HookRegistry.Execute (onion model)                  │
│                                                     │
│  1. PermissionHook ─── reads settings.json           │
│     ├── allow rule matched  → Allow (pass through directly) │
│     ├── deny  rule matched  → Deny (reject immediately)     │
│     └── no match            → Ask (enter approval flow)     │
│                                                     │
│  2. DangerHook ──────── pattern-matches built-in high-risk commands │
│     ├── already approved by an earlier hook  → skip (avoid double dialog) │
│     ├── high-risk pattern matched        → Ask (high/medium risk)   │
│     └── no match                         → Allow                  │
│                                                     │
│  3. OffloadHook ─────── large-output file dump (AfterExecute)│
└─────────────────────────────────────────────────────┘
      │
      │ HookActionAsk + ApprovalFunc present
      ▼
┌─────────────────────────────────────────────────────┐
│  engine.executeTools (tool goroutine)                │
│                                                     │
│  Sends EventApprovalRequired → ch (event channel)   │
│  ⟳ Blocks on ResponseCh awaiting user decision       │
└─────────────────────────────────────────────────────┘
      │
      │ TUI receives EventApprovalRequired
      ▼
┌─────────────────────────────────────────────────────┐
│  TUI approval dialog (does not resume readNextEvent) │
│                                                     │
│  [1] Allow (this time only)                          │
│  [2] Allow (don't ask again this session)            │
│  [3] Always allow (write to allowlist)               │
│  [4] Deny                                           │
│  [5] Deny and provide feedback...                    │
└─────────────────────────────────────────────────────┘
      │
      │ confirmApproval → ResponseCh <- resp
      ▼
  Tool goroutine unblocks, continues execution or returns IsError=true
```

---

## HookDecision Decision Type

```go
type HookAction string

const (
    HookActionAllow HookAction = "allow" // continue execution, pass to next hook
    HookActionDeny  HookAction = "deny"  // reject immediately, AfterExecute not called
    HookActionAsk   HookAction = "ask"   // request human approval
)

type HookDecision struct {
    Action       HookAction
    Reason       string          // reason shown to the user
    RiskLevel    string          // "high" | "medium" | "low" (affects dialog color)
    ModifiedArgs json.RawMessage // optional: tool arguments modified by the hook
}
```

Constructor functions:

```go
hooks.Allow()                      // pass through
hooks.Deny("path is locked")       // reject, with reason
hooks.Ask("recursive delete operation", "high")   // request approval, specify risk level
```

---

## ToolHook Interface

```go
type ToolHook interface {
    // BeforeExecute fires before the tool executes and returns a structured decision.
    BeforeExecute(ctx context.Context, tc schema.ToolCall) (context.Context, HookDecision, error)
    // AfterExecute fires after the tool executes and can modify the returned result.
    AfterExecute(ctx context.Context, tc schema.ToolCall, result schema.ToolResult) schema.ToolResult
}
```

`HookRegistry.Execute` calls the hook chain in sequence following the onion model:

- `error` → short-circuits immediately, returns `IsError=true`
- `HookActionDeny` → rejects immediately, skips remaining hooks and all `AfterExecute` calls
- `HookActionAsk` → looks up `ApprovalFunc` in the context; if already approved by a human (`withApproved`) **or** already explicitly allowed by a rule (`withExplicitlyAllowed`), skips the duplicate dialog; if no `ApprovalFunc` is present (non-interactive mode), treated as Allow
- `HookActionAllow` → sets the `withExplicitlyAllowed` marker in the context (so that a subsequent hook's Ask skips approval), and applies `ModifiedArgs` (if the hook carries a rewritten set of arguments)

**Semantic distinction between the two "already allowed" markers:**
- `withApproved` (`approvedContextKey`): set when the user clicks "Allow" in real time in the approval dialog, indicating human intervention and approval
- `withExplicitlyAllowed` (`explicitlyAllowedContextKey`): set when an earlier hook silently allows based on a rule (e.g., an allowlist match), with no human involved

The two markers are equivalent in the `HookActionAsk` detection logic (both skip approval), but are kept as separate keys so their origin remains traceable.

`AfterExecute` is called only for hooks that have already completed `BeforeExecute`, in reverse order (guaranteed by the `executed` counter).

---

## Built-in High-Risk Command Interception (DangerHook)

`DangerHook` applies only to the `bash` tool, and detects dangerous patterns via case-insensitive substring matching:

| Risk Level | Pattern | Reason |
|---------|------|------|
| high | `rm -rf` | forced recursive delete |
| high | `rm -r /` | forced recursive delete of root directory |
| high | `\| bash`, `\|bash` | pipe execution of a remote script (including the no-space variant) |
| high | `\| sh`, `\|sh` | pipe execution of a remote script (including the no-space variant) |
| high | `:(){ :|:` | fork bomb |
| high | `dd if=` | direct write to a block device (may overwrite disk) |
| high | `> /dev/` | write to a device file |
| high | `chmod -r 777` | recursively grant all permissions to everyone |
| high | `chown -r` | recursively change file owner |
| medium | `sudo ` | execute command with root privileges |
| medium | `chmod 777 ` | grant all permissions to everyone |
| medium | `chmod +x ` | add executable permission |
| medium | `pkill ` | kill processes by name |
| medium | `kill -9 ` | force kill a process |
| medium | `killall ` | kill all processes with the same name |
| medium | `iptables ` | modify firewall rules |
| medium | `systemctl ` | manage system services |

Non-bash tools return Allow directly; when there is no bash argument or parsing fails, it fails open (Allow).

---

## Permission Rule Configuration (settings.json)

The config file is located at `{workDir}/.harness9/settings.json`, in JSON format:

```json
{
  "permissions": {
    "allow": ["bash(git *)", "read_file", "bash(*go test*)"],
    "deny":  ["bash(rm -rf *)"],
    "ask":   ["bash(sudo *)"]
  }
}
```

**Rule syntax:**

| Syntax | Semantics |
|------|------|
| `"read_file"` | matches any invocation of that tool |
| `"bash(git *)"` | bash tool, any invocation whose command starts with `git ` |
| `"bash(*docker*)"` | bash tool, command containing `docker` |

**Matching priority:** rules are matched in declaration order, and the first matching rule takes effect; the file load order is deny → allow → ask; when there is no match, the default is Ask.

**Dynamic updates:** `PermissionHook` re-reads the config file from disk on every tool call (`NewFileHook`); once the user selects "Always allow" in the approval dialog, the same type of call takes effect immediately on the next invocation, with no restart required.

**`SaveRules` caveat:** after serialization and reload, rule order is reset to deny→allow→ask, independent of the original insertion order.

---

## Sensitive Path Hard Protection (safe_path)

`safePath()` runs in every file tool (`read_file`, `write_file`, `edit_file`), and rejects access to the following paths regardless of any configuration:

```
~/.ssh        ~/.aws         ~/.kube
~/.gnupg      ~/.netrc       ~/.config/gcloud
```

Note: the `bash` tool does not go through `safePath`; protection against bash accessing sensitive files is handled by `DangerHook` pattern matching.

---

## Engine Approval Events

The `emitter.approval` closure in `RunStream` converts a `HookActionAsk` from the Hook layer into an event-driven stream:

```go
// The tool goroutine sends an event and blocks waiting for a response
approval: func(ctx context.Context, tc schema.ToolCall, reason, riskLevel string) hooks.ApprovalResponse {
    if e.permissionMode == PermissionModeBypassAll {
        return hooks.ApprovalResponse{Approved: true}
    }
    respCh := make(chan hooks.ApprovalResponse, 1)
    req := ApprovalRequest{ToolCall: tc, Reason: reason, RiskLevel: riskLevel, ResponseCh: respCh}
    select {
    case <-ctx.Done():
        return hooks.ApprovalResponse{Approved: false}
    case ch <- Event{Type: EventApprovalRequired, Data: req}:
    }
    select {
    case <-ctx.Done():
        return hooks.ApprovalResponse{Approved: false}
    case resp := <-respCh:
        return resp
    }
},
```

**Concurrency safety:** `ch` (the event channel) is unbuffered; when multiple tool goroutines request approval simultaneously, the second goroutine blocks on `ch <- Event` until the TUI has finished processing the first approval and resumes consuming the channel. In the TUI implementation, `handleEvent` returns `nil` upon receiving `EventApprovalRequired` (without calling `readNextEvent`), and is driven only by keyboard events, so it does not block channel consumption while the dialog is open.

**Non-interactive mode (`Run`):** `emitter.approval` is `nil`; when `HookActionAsk` finds no `ApprovalFunc` to call in `HookRegistry.Execute`, it is automatically treated as Allow, preserving backward compatibility.

---

## TUI Approval Dialog

The dialog replaces the normal input line rendering when `approvalPending == true`, and is displayed above the status bar:

```
╭─────────────────────────────────────────────────────╮
│  ⚠  Tool Approval Request [High Risk]                │
│                                                     │
│  Tool: bash                                         │
│  Reason: forced recursive delete of files/directories │
│                                                     │
│  ▶ [1] Allow (this time only)                       │
│    [2] Allow (don't ask again this session)          │
│    [3] Always allow (write to allowlist)             │
│    [4] Deny                                         │
│    [5] Deny and provide feedback...                  │
│                                                     │
│  ↑↓ move  Enter/1-5 confirm  Esc deny               │
╰─────────────────────────────────────────────────────╯
```

**Keyboard interaction:**

| Key | Action |
|------|------|
| `↑` / `↓` | move cursor |
| `1`-`5` | directly select the corresponding option |
| `Enter` | confirm the currently highlighted option (option 5 enters feedback input mode) |
| `Esc` | deny directly (equivalent to option 4) |
| `Ctrl+C` / `Ctrl+D` | deny and interrupt |

**Risk level color coding:**
- `high` → red (`#160`)
- `medium` → orange (`#208`)
- `low` / unknown → yellow (`#220`)

**Option 5 feedback input mode:** after entering this mode, ordinary character input is appended to the feedback text; `Enter` submits, `Esc` cancels and returns to option-selection mode; the feedback text is passed back to the LLM via `ApprovalResponse.Feedback`.

---

## PermissionMode

`engine.PermissionMode` is a global permission strategy orthogonal to `planning.PlanMode`:

| Mode | Description |
|------|------|
| `PermissionModeDefault` | dangerous operations not on the allowlist trigger the approval dialog (default) |
| `PermissionModeAutoApprove` | operations on the allowlist pass automatically (not yet implemented) |
| `PermissionModeReadOnly` | rejects all write operations (not yet implemented) |
| `PermissionModeBypassAll` | skips all permission checks; the approval closure returns Approved=true directly |

```go
eng := engine.NewAgentEngine(llm, hookReg, workDir,
    engine.WithPermissionMode(engine.PermissionModeBypassAll),
)
```

Modes other than Default (except BypassAll) are currently shown in the TUI status bar.

---

## Extension: Custom Hooks

Implement the `hooks.ToolHook` interface to plug into the HookRegistry:

```go
type MyAuditHook struct{}

func (h *MyAuditHook) BeforeExecute(ctx context.Context, tc schema.ToolCall) (context.Context, hooks.HookDecision, error) {
    log.Printf("audit: %s %s", tc.Name, tc.Arguments)
    return ctx, hooks.Allow(), nil
}

func (h *MyAuditHook) AfterExecute(ctx context.Context, tc schema.ToolCall, result schema.ToolResult) schema.ToolResult {
    return result
}

// Register — order determines execution priority
hookReg := hooks.NewHookRegistry(registry, permHook, dangerHook, &MyAuditHook{}, offloadHook)
```

**Design considerations:**
- When `BeforeExecute` returns `Deny`, `AfterExecute` will not be called (already guaranteed by the `executed` counter)
- For `Ask`, if the context is already marked as "approved this time" (`withApproved`), it is treated as Allow directly, avoiding duplicate dialogs across multiple hooks
- **`fail-open` principle:** safety guardrails should have sensible defaults, avoiding false positives that would block normal operations and stall the Agent

---

## Known Limitations

- **"Don't ask again this session"** currently behaves identically to "this time only"; a session-level in-memory allowlist is not yet implemented
- The `PermissionModeAutoApprove` and `PermissionModeReadOnly` enums are defined, but their concrete behavior is not yet implemented
- When writing to the allowlist, the pattern is generated from the command's first word (e.g., `bash(*mkdir*)`), which may be looser than expected
- The `bash` tool does not go through `safePath`; complex shell scripts may bypass `DangerHook`'s string matching
- For approval dialogs triggered by the `ask` list in `settings.json`, the risk level is fixed at "medium risk" (orange) and cannot be set to `high`/`low` per rule in the configuration; `DangerHook` itself can distinguish between high and medium risk levels

## Bug Fix Log

**2026-06-08 Approval dialog still popped up repeatedly after writing to the allowlist** (`self-dev` branch)

**Root cause:** when `PermissionHook` returned `HookActionAllow` (allowlist match), the original implementation did not record an "already allowed" marker in the context. A subsequent `DangerHook` detecting a dangerous pattern still returned `HookActionAsk`, and since the context carried no approval marker at that point, the approval dialog was triggered a second time.

**Fix:** added a separate `explicitlyAllowedContextKey` (distinct from the human-approval `approvedContextKey`); `HookActionAllow` writes to this key, and the `HookActionAsk` detection logic checks both keys, skipping approval if either is set.
