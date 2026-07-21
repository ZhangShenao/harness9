# Shell Execution Feature Technical Design

## Overview

harness9's Shell execution feature lets users run Bash commands directly inside the TUI conversation box, without switching to a separate terminal. Command output is appended to the conversation stream in real time, and is automatically injected as context the next time a message is sent to the LLM, allowing the Agent to reason using the command results.

Trigger: type `!` as the first character of the input box to enter Shell mode, press `Enter` to execute, `Esc` to cancel.

---

## Design Principles

| Principle | Implementation |
|------|---------|
| **Does not interrupt the conversation flow** | Command output is appended inline to the Scrollback, no new page is opened |
| **LLM-aware results** | Output is buffered in `pendingShellOutput` and prepended on the next dispatch |
| **Non-blocking TUI** | Executed asynchronously via `tea.Cmd`; the main goroutine does not wait |
| **Security interception** | Known interactive commands (vim/ssh, etc.) are rejected outright, with a prompt to run them in a separate terminal |
| **Bounded memory** | Storage-side truncation by byte count, preventing large output from occupying memory long-term |

---

## Interaction Flow

```
User input "!git status"
    │
    ▼
tea.KeyEnter triggers Update()
    │
    ▼
strings.HasPrefix(raw, "!") → true
    │
    ▼
dispatchShellCommand("git status")
    ├── empty command → return directly, no-op
    ├── isInteractiveCmd → reject, show error message
    └── normal command
            │
            ▼
        lines append "$ git status" (shellCmdStyle)
            │
            ▼
        return runShellCmd(workDir, cmd) as a tea.Cmd
            │
            ▼  [async goroutine, 30s timeout]
        exec.CommandContext("bash", "-c", "git status")
            │
            ▼
        shellResultMsg{cmd, output, isErr, dur}
            │
            ▼
    case shellResultMsg: handled in Update()
        ├── Display side: truncateUTF8(output, 4096) appended line by line (shellOutputStyle)
        ├── Status line: ✓ done / ✗ non-zero exit + duration
        └── Storage side: truncateUTF8(output, 2048) appended to pendingShellOutput
```

---

## Visual State Switching

The input box checks in real time whether it starts with `!`, switching the Shell mode visual indicator:

```
Normal mode                    Shell mode
┌──────────────────────┐        ┌──────────────────────────────────┐
│  ›  Enter a task...   │        │  [SHELL]  $  !git status█        │
└──────────────────────┘        └──────────────────────────────────┘

Status bar: dark gray #235       Status bar: dark green #22 (shellStatusBarStyle)
Footer: normal shortcuts         Footer: enter to execute / esc to cancel / ctrl+c to quit
```

### Style Variables Involved (`tui.go`)

| Variable | Purpose |
|------|------|
| `shellStatusBarStyle` | Dark green background (#22) + light green text (#120) status bar, clearly distinguished from the default gray background and Plan Mode orange background |
| `shellModeTagStyle` | `[SHELL]` badge in the input area: dark olive background (#58) + bright yellow text (#226) |
| `shellModeAccentStyle` | Bright green (#83) accent, replacing the default cyan |
| `shellModePromptStyle` | `$` prompt style, precomputed to avoid `.Bold(true)` allocation on every frame |
| `shellModeLabelInBarStyle` | `SHELL` label inside the status bar, bright green bold |
| `shellCmdStyle` | Command line `$ cmd`, yellow bold (#33) |
| `shellOutputStyle` | Output line, light gray (#250) |
| `shellOKStyle` | `✓ done` line, green (#34) |
| `shellErrStyle` | `✗ non-zero exit` line, red (#160) |

Color-switching logic is centralized in two methods in `tui_view.go`, so the View layer has no scattered if-statements:

```go
func (m tuiModel) accentStyle() lipgloss.Style         // returns the accent color for the current mode
func (m tuiModel) activeStatusBarStyle() lipgloss.Style // returns the status bar container style for the current mode
```

Priority (high → low):

```
shellMode=true  →  dark green background #22 + bright green accent #83
Plan/AutoEdit   →  dark orange background #94 + amber accent #220
Default         →  dark gray background #235 + cyan accent #81
```

---

## Core Data Flow

### `tuiModel` Fields

```go
pendingShellOutput []string  // Shell command records accumulated in this round, cleared on next dispatch
shellMode          bool      // true when the input box starts with "!", drives View-layer style switching
```

### `shellResultMsg` Type

```go
type shellResultMsg struct {
    cmd    string        // the original command string
    output string        // combined stdout + stderr output (CombinedOutput)
    isErr  bool          // exit code != 0
    dur    time.Duration // actual execution duration
}
```

### LLM Context Injection

The next time the user presses `Enter` to send a message, `dispatch()` prepends the buffered command records to the prompt:

```
[Shell command records executed by the user]
$ git status
On branch main...

---
$ go build ./...
# github.com/harness9/cmd/harness9
...

[The user's actual question]
```

After injection, `pendingShellOutput` is cleared to avoid duplicate injection. Each record is independently truncated to `maxShellContextLen` (2048 bytes), with multiple records separated by `---`.

---

## Truncation Strategy

The Shell feature involves two truncation boundaries, both using `truncateUTF8` to guarantee byte-level truncation does not corrupt multi-byte characters:

| Scenario | Constant | Truncation Timing | Truncation Marker |
|------|------|---------|---------|
| TUI display | `maxShellDisplayLen = 4096` | before display in `case shellResultMsg:` | `...[output too long, truncated; consider re-running with head -n N]...` |
| LLM context | `maxShellContextLen = 2048` | at storage time in `case shellResultMsg:` | none (truncated directly; the LLM can perceive the content is incomplete) |

### `truncateUTF8` Implementation

```go
func truncateUTF8(s string, maxBytes int) string {
    if len(s) <= maxBytes {
        return s
    }
    s = s[:maxBytes]
    for len(s) > 0 {
        r, size := utf8.DecodeLastRuneInString(s)
        if r != utf8.RuneError || size > 1 {
            break
        }
        s = s[:len(s)-1]
    }
    return s
}
```

`utf8.DecodeLastRuneInString` returns `(RuneError, 1)` for an incomplete trailing sequence, backing off one byte at a time until the tail is a valid rune. It does not back off for a legitimately valid `RuneError` (U+FFFD, `size > 1`).

---

## Asynchronous Execution Mechanism

Shell commands are executed asynchronously via Bubbletea's `tea.Cmd` mode, so the TUI main loop does not block:

```
Update() returns (m, runShellCmd(workDir, cmd))
    │
    ▼  the Bubbletea runtime executes this Cmd in a separate goroutine
exec.CommandContext(ctx, "bash", "-c", cmd)  // 30s timeout
    │
    ▼  the Cmd returns a tea.Msg
shellResultMsg → sent to the main message queue
    │
    ▼
Update() case shellResultMsg: handles the result
```

The working directory is fixed to `tuiModel.workDir` (the program's launch directory), injected via `c.Dir = workDir`. `stdout` and `stderr` are merged via `CombinedOutput()`, ensuring error messages are visible to the user.

---

## Interactive Command Interception

Bubbletea runs in AltScreen mode, taking exclusive control of terminal input/output, so PTY-dependent programs cannot function correctly:

```go
var interactiveCmds = map[string]bool{
    "vim": true, "vi": true, "nano": true, "emacs": true,
    "ssh": true, "top": true, "htop": true, "less": true,
    "man": true, "more": true, "watch": true, "tmux": true,
    "screen": true,
}
```

`isInteractiveCmd` extracts the `filepath.Base` of the first token on the command line (handling absolute paths such as `/usr/bin/vim`), and matches it against the interception list. On a match, it prints `✗ This command requires an interactive terminal; please run it in a separate terminal window` and does not execute the command.

---

## Keyboard Behavior

| Key | Behavior |
|------|------|
| `!` (first character) | Triggers the Shell mode visual switch (in real time, no need to press Enter) |
| `Enter` | Executes the command after `!`; the input box is unavailable until the command finishes executing |
| `Esc` | Clears the input box, exits Shell mode (only while not executing) |
| `Ctrl-C` | If a command is running: cancels it (note: in the current implementation, cancellation only occurs when the 30s timeout expires); otherwise exits the program |
| `Backspace` (deleting `!`) | Exits Shell mode in real time, restoring the normal input prompt |

---

## Code Location Index

| Content | File | Location |
|------|------|------|
| Shell style variables (`shellCmdStyle`, etc.) | `cmd/harness9/tui.go` | `var (...)` block, Shell mode style group |
| `shellMode` / `pendingShellOutput` fields | `cmd/harness9/tui.go` | `tuiModel` struct |
| Constants `maxShellDisplayLen` / `maxShellContextLen` | `cmd/harness9/tui_update.go` | `const (...)` block |
| `shellResultMsg` type | `cmd/harness9/tui_update.go` | where the `shellResultMsg` struct is defined |
| Esc exits Shell mode | `cmd/harness9/tui_update.go` | `case tea.KeyEsc:` |
| Enter dispatches Shell command | `cmd/harness9/tui_update.go` | `case tea.KeyEnter:`, `strings.HasPrefix(raw, "!")` branch |
| `case shellResultMsg:` result handling | `cmd/harness9/tui_update.go` | `case shellResultMsg:` inside `Update()` |
| Shell mode real-time detection | `cmd/harness9/tui_update.go` | textinput fallthrough block at the end of `Update()` |
| `dispatch()` context injection | `cmd/harness9/tui_update.go` | `pendingShellOutput` handling block in the `dispatch()` function |
| `truncateUTF8` | `cmd/harness9/tui_update.go` | `truncateUTF8` function |
| `interactiveCmds` / `isInteractiveCmd` | `cmd/harness9/tui_update.go` | `interactiveCmds` var + `isInteractiveCmd` function |
| `runShellCmd` | `cmd/harness9/tui_update.go` | `runShellCmd` function |
| `dispatchShellCommand` | `cmd/harness9/tui_update.go` | `dispatchShellCommand` function |
| `accentStyle()` / `activeStatusBarStyle()` | `cmd/harness9/tui_view.go` | the two methods at the top of the file |
| `renderStatusBar()` SHELL label | `cmd/harness9/tui_view.go` | `renderStatusBar`, `modePart` assignment branch |
| `renderInput()` Shell mode | `cmd/harness9/tui_view.go` | first `if m.shellMode` branch in `renderInput` |
| `renderFooter()` Shell mode | `cmd/harness9/tui_view.go` | first `if m.shellMode` branch in `renderFooter` |
| Unit tests | `cmd/harness9/tui_test.go` | `TestShell*`, `TestTruncateUTF8*` |
