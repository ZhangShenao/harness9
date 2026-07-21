# TUI Interactive Interface: Implementation Principles

harness9 automatically launches a full-screen TUI mode in an interactive terminal (TTY), using the [Bubbletea](https://github.com/charmbracelet/bubbletea) framework to implement the Elm Architecture.

---

## File Structure

The TUI is split into four files by responsibility:

```
cmd/harness9/
├── tui.go          # tuiModel struct, package-level style variables, Init, RunTUI
├── tui_update.go   # Update logic: event handling, keyboard, scrolling, Tab completion, Markdown rendering, Thinking blocks
├── tui_view.go     # View rendering: 6 sub-renderers (Conversation/ToolProgress/StatusBar/Input/Footer)
├── tui_banner.go   # WelcomeBanner: HARNESS9 ASCII Art + bannerContent()
└── tui_test.go     # Unit tests: directly inject tea.Msg to verify model state (including thinking block tests)
```

---

## Phase State Machine

The TUI has two Phases; the first Enter triggers the switch from the welcome page to the conversation page:

```go
type tuiPhase int

const (
    phaseWelcome tuiPhase = iota  // Welcome page (HARNESS9 ASCII Art)
    phaseChat                      // Conversation page (Scrollback + streaming output)
)
```

### phaseWelcome — Welcome Page Layout

```
         ╦ ╦  ╔╦╗  ╔═╗  ╔╗╦  ╔══  ╔══  ╔══  ╔═╗
         ╠═╣  ╠╩╣  ╠╦╝  ║╚╗  ╠═   ╚═╗  ╚═╗  ╚═╣
         ╩ ╩  ╚ ╝  ╩╗   ╩ ╩  ╚══  ══╝  ══╝    ╝

  harness9  ·  An AI-powered coding agent
  /skill load skill  │  Tab completion  │  Ctrl+C to quit
  ──────────────────────────────────────────────
  model: gpt-4o-mini  │  mode: Default  │  ~/myproject
  › Enter a task...
  enter send  / skill commands  ↑↓ scroll  ctrl+c quit
```

### phaseChat — Conversation Page Layout

```
  ▶ You: Help me analyze the bug in main.go

  ◆ harness9:
    Sure, let me read the file first...
    ✓ read_file(main.go) — 234ms
    Found a null pointer dereference issue at line 42

  ⠼ Thinking...  bash(go test ./...)  [3.2s]    ← ToolProgress (visible only while running)
  model: gpt-4o-mini  │  mode: Default  │  ~/myproject  ← StatusBar
  › _                                                    ← Input
  enter send  / skill commands  ↑↓ scroll  ctrl+c quit  ← Footer
```

| Region | Height | Responsibility |
|------|------|------|
| Scrollback | Flexible (all remaining lines) | Appends historical message output; supports mouse/keyboard scrolling |
| ToolProgress | 1 line (only while running) | spinner verb + tool name summary + elapsed time |
| StatusBar | 1 line | Persistent model / mode / workdir info |
| Input | 1 line | Single-line text input box; disabled while the Agent is running |
| Footer | 1 line | Keyboard shortcut hints / scroll position percentage / Tab completion hints |

---

## WelcomeBanner: ASCII Art

`tui_banner.go` defines the HARNESS9 title made of three lines of box-drawing characters (character width 38):

```go
const asciiArt = `╦ ╦  ╔╦╗  ╔═╗  ╔╗╦  ╔══  ╔══  ╔══  ╔═╗
╠═╣  ╠╩╣  ╠╦╝  ║╚╗  ╠═   ╚═╗  ╚═╗  ╚═╣
╩ ╩  ╚ ╝  ╩╗   ╩ ╩  ╚══  ══╝  ══╝    ╝`
```

`bannerContent(width int)` renders the ASCII Art centered based on terminal width, and appends a subtitle, keyboard shortcut hints, and a separator line below it.

---

## Startup Condition: Automatic TTY Detection

`main.go` uses `github.com/charmbracelet/x/term` to detect whether standard input is an interactive terminal:

```go
if term.IsTerminal(os.Stdin.Fd()) {
    // Interactive terminal → launch TUI
    RunTUI(ctx, eng, skillsIndex, workDir, modelName)
} else {
    // Pipe / CI environment → fall back to CLI REPL
    RunCLI(ctx, eng, skillsIndex)
}
```

---

## Log Isolation

At the entry point of `RunTUI`, `log` output is redirected to `io.Discard` to prevent internal engine logs from polluting AltScreen output:

```go
func RunTUI(...) error {
    origWriter := log.Writer()
    log.SetOutput(io.Discard)
    defer log.SetOutput(origWriter)
    // ...
}
```

---

## Data Flow: engine.Event → Bubbletea Msg

engine.RunStream returns a `<-chan engine.Event`, bridged to the Bubbletea message loop via a **chained tea.Cmd**:

```
engine.RunStream(ctx, prompt)
  └─ <-chan Event
       └─ readNextEvent(ch)    ← blocking read of one Event, returns a tea.Cmd
            └─ eventMsg        ← wrapped as a Bubbletea Msg, triggers Update
                 └─ handleEvent() → updates model state based on event type
                      └─ readNextEvent(ch) ← schedules the next read (chain-driven)
```

```go
type eventMsg engine.Event

func readNextEvent(ch <-chan engine.Event) tea.Cmd {
    return func() tea.Msg {
        evt, ok := <-ch
        if !ok {
            return eventMsg{Type: engine.EventDone}
        }
        return eventMsg(evt)
    }
}
```

---

## Event Handling and Highlighting Rules

| engine.Event | TUI Behavior | Style |
|---|---|---|
| `EventThinkingDelta` | delta appended to `pendingThinking`, rendered as a dim reasoning block prefixed with `│` | Dark gray Color "238" |
| `EventActionDelta` | If a thinking block exists, flush it first; delta appended to `pendingReply`, raw text written to scrollback | Plain text |
| `EventToolStart` | Flush thinking block (if any); flush-render the current text block; record tool name, start time, tool arguments; start the spinner | Yellow tool progress line |
| `EventToolResult` | Append a completion line (tool name + elapsed time); clear `currentTool` | Green `✓` / red `✗` |
| `EventDone` | Flush thinking block (if any); flush-render the final text block; `running=false`; reactivate the input box | Bold green `✅ Task complete` |
| `EventError` | Discard unrendered raw text and thinking block; `running=false`; append a red error line | Red `❌` |

---

## Thinking Block Display (Reasoning Content Display)

When the LLM supports extended thinking (such as Anthropic Claude's `thinking_delta` or OpenRouter's `delta.reasoning`), the engine emits an `EventThinkingDelta` event, and the TUI renders the reasoning content as a dark gray block that is visually noticeably weaker than the main text, creating a visual hierarchy distinction from the LLM's reply body.

### Rendering Effect

```
◆ harness9:
« thinking »
  │ I need to analyze the user's requirements first, then decide which tool to use...
  │ read_file can explore the directory structure first, then bash can run tests to confirm
  │ whether the current go.sum is complete...
  └ ──────────────────────────────
Sure, let me help you complete this task...
```

### State Machine Design

The Thinking block maintains state using three fields:

```go
pendingThinking   string  // accumulates the reasoning text for the current turn
thinkingLineStart int     // index of the « thinking » header line in lines; -1 means inactive
```

State transitions:

```
dispatch() called
    → thinkingLineStart = -1, pendingThinking = ""
         ↓
EventThinkingDelta (first)
    → remove the blank-line placeholder at pendingReplyStart (avoid a blank line before the header)
    → append "« thinking »" to lines
    → thinkingLineStart = len(lines) - 1
         ↓
EventThinkingDelta (subsequent)
    → pendingThinking += delta
    → lines[thinkingLineStart+1:] fully overwritten (renderThinkingLines)
         ↓
EventActionDelta / EventToolStart / EventDone
    → flushPendingThinking(): append a "  └ ───" closing line
    → thinkingLineStart = -1, pendingThinking = ""
    → pendingReplyStart = len(lines)  ← subsequent body text is written from here
```

### flushPendingThinking — Key Constraint

`flushPendingThinking` only executes when `pendingThinking != ""` (returns immediately for an empty thinking block), ensuring idempotency. Call sites:

| Triggering event | flush timing |
|---|---|
| `EventActionDelta` | Before `pendingReply += delta`, ensuring `pendingReplyStart` has already been updated |
| `EventToolStart` | Before `flushPendingReply()`, to avoid line-index corruption |
| `EventDone` | Before `flushPendingReply()` |
| `EventError` | flush is not called; lines are truncated directly to `thinkingLineStart` |

### renderThinkingLines — Rendering Algorithm

```go
func renderThinkingLines(text string, width int) []string
```

- Splits paragraphs on `\n`, wrapping each paragraph via `thinkingWordWrap`
- Each line is prefixed with `"  │ "` (4 display columns); wrapping is disabled when terminal width < 24
- Returns an ANSI-colored line slice that directly overwrites `lines[thinkingLineStart+1:]`

### thinkingWordWrap — Word-Wrap Algorithm

- Wraps on word boundaries, ensuring each line is ≤ `width` runes
- Overly long words (URLs, etc.) are forcibly truncated via `hardBreak` to prevent overflowing the terminal width
- No special handling needed for the first word: a final check `if len([]rune(line)) > width` acts as a universal fallback

### Style Constants

```go
thinkingHeaderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("238")).Italic(true)  // « thinking »
thinkingLineStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))               // │ content line
thinkingEndStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("236"))               // └ closing line
```

The dark gray colors (Color "238"/"236") make the reasoning content visually noticeably weaker than the main text, letting users distinguish at a glance between the reasoning process and the final reply.

---

## Spinner Verb Rotation

While a tool is executing, the ToolProgress line displays a Chinese verb that rotates over time, enhancing the waiting feedback:

```go
var spinnerVerbs = []string{
    "思考中", "分析中", "处理中", "推理中", "计算中", "评估中",
}
```

Each spinner tick fires a `spinner.TickMsg`; once `tickCount` accumulates to 30 (about 3 seconds), `verbIdx` increments, cycling through 6 verbs:

```go
case spinner.TickMsg:
    if m.running && m.currentTool != "" {
        m.tickCount++
        if m.tickCount%30 == 0 {
            m.verbIdx = (m.verbIdx + 1) % len(spinnerVerbs)
        }
        // ...
    }
```

---

## summarizeTool: Tool Argument Summary

`renderToolProgress` calls `summarizeTool` to compress tool arguments into a single-line summary, displayed in parentheses after the tool name:

```
⠼ Thinking...  bash(go test ./... 2>&1 | head -20)  [1.2s]
⠼ Analyzing...  read_file(agent_loop.go)  [0.4s]
```

| Tool name | Summary logic |
|--------|---------|
| `bash` | Extracts the `command` field, truncated to 120 characters |
| `read_file` / `write_file` / `edit_file` | Extracts the `path` field, taking `filepath.Base` |
| Other tools | Raw JSON truncated to 80 characters |
| Parse failure | Returns an empty string (tool name shown without parentheses) |

---

## View() Call Chain

`View()` selects the rendering path based on `phase`:

```go
func (m tuiModel) View() string {
    if m.phase == phaseWelcome {
        // bannerContent + StatusBar + Input + Footer
    } else {
        scrollH := m.scrollHeight()
        // renderConversation(scrollH)
        // [renderToolProgress()]  ← only when running && currentTool != ""
        // renderStatusBar()
        // renderInput()
        // renderFooter()
    }
}
```

### Dynamic scrollHeight()

The number of available scrollback lines is dynamically adjusted based on running state:

```go
func (m tuiModel) scrollHeight() int {
    reserved := 3 // StatusBar + Input + Footer
    if m.running && m.currentTool != "" {
        reserved = 4 // add the ToolProgress line
    }
    h := m.height - reserved
    if h < 1 { h = 1 }
    return h
}
```

---

## Markdown Rendering

### Streaming Rendering Strategy

LLM text output (`EventActionDelta`) is appended and displayed as raw text during streaming; at **tool boundaries** (`EventToolStart`) and task completion (`EventDone`), the entire text block is uniformly rendered via [glamour](https://github.com/charmbracelet/glamour):

```
EventActionDelta × N  →  pendingReply accumulates raw text
                              ↓
EventToolStart / EventDone  →  glamour.Render(pendingReply)
                              ↓
                         replaces lines[pendingReplyStart:]
```

### Key Fields

```go
pendingReply      string // accumulates the raw Markdown of the current text block
pendingReplyStart int    // the starting line index in lines corresponding to pendingReply
```

### Avoiding Terminal Color Queries

`glamour.WithAutoStyle()` is deliberately not used — this option sends an OSC 11 terminal color query, and the terminal's response can be misinterpreted by Bubbletea's textinput as user input, causing garbled input in the text box. A fixed `"dark"` style is used instead:

```go
glamour.NewTermRenderer(
    glamour.WithStandardStyle("dark"),
    glamour.WithWordWrap(width-4),
)
```

---

## Keyboard Interaction and Scrolling

### All Keys

| Key | Idle state | Agent running |
|------|-----------|-------------|
| `Enter` | Sends the input, starts the Agent (first press triggers phaseWelcome→phaseChat); executes a Shell command for `!cmd`; exits the TUI when input is `/exit` | Ignored |
| `!` (first character) | Toggles Shell mode in real time (status bar/input area visual change, no Enter needed) | Ignored |
| `Esc` | In Shell mode: clears the input box, exits Shell mode | Ignored |
| `Tab` | Cycles through built-in command + Skills completion (built-in commands take priority) | Ignored |
| `Shift-Tab` | Cycles through Plan Mode (Default → Plan → AutoEdit → Default) | Ignored |
| `Ctrl-C` / `Ctrl-D` | Exits the TUI | Calls `cancelFn()` to interrupt the Agent; clears autoExecuting |
| Mouse wheel up / `PgUp` / `Ctrl-↑` | Scrolls up | Same as idle |
| Mouse wheel down / `PgDn` / `Ctrl-↓` | Scrolls down, returns to auto-scroll at the bottom | Same as idle |
| `End` | Forces a jump back to the bottom (auto-scroll) | — |

### Scroll Implementation

Scroll state is represented by `viewTop int`:

- `viewTop = -1`: **auto-scroll mode**, View() always shows the end of `lines`
- `viewTop ≥ 0`: **manual scroll mode**, View() displays starting from that line index

```go
func (m tuiModel) scrollBy(delta int) tuiModel {
    scrollH := m.scrollHeight()
    if m.viewTop < 0 {
        m.viewTop = len(m.lines) - scrollH // enter manual mode from the bottom
    }
    m.viewTop += delta
    if m.viewTop >= len(m.lines)-scrollH {
        m.viewTop = -1 // reached the bottom, return to auto-scroll
    }
    return m
}
```

The Footer shows the current position percentage while manually scrolling:

```
enter send  / skill commands  ↑↓ scroll  end back to bottom (42%)  ctrl+c quit
```

---

## Built-in Commands and Slash Commands

### Built-in Commands

The TUI has four built-in slash commands, processed before Skills:

| Command | Behavior |
|------|------|
| `/new` | Creates a new session, replaces the bound engine, refreshes the status bar |
| `/resume` | Lists historical sessions, enters index-selection mode |
| `/plan [task description]` | Enters Plan Mode; sends the planning request directly if a task description is given, otherwise prompts for input |
| `/exit` | Exits the TUI (equivalent to pressing Ctrl-C while idle) |

```go
var builtinCmds = []struct {
    name string
    desc string
}{
    {"new", "开启新会话"},
    {"resume", "恢复历史会话"},
    {"plan", "进入规划模式分析任务"},
    {"exit", "退出 TUI"},
}
```

### Skills Recognition Flow

When input starts with `/` and does not match a built-in command, `resolvePrompt` looks up the corresponding Skill:

```
/skill-name [optional additional text]
    ↓
skills.Index.GetFullContent("skill-name")
    ↓ success           ↓ failure
  ◎ Skill loaded      ✗ Skill not found: skill-name
  → Agent runs          → refocus the input box, wait for the next input
```

### Tab Completion

1. First Tab: matches both **built-in commands** and Skills against the current input prefix, with built-in commands listed first
2. Subsequent Tab: cycles through the merged list
3. Any non-Tab key: exits the completion cycle

The Footer shows matching hints in real time; built-in commands are shown with a parenthesized description, Skills show only the name; the currently selected item is highlighted in cyan:

```
  ↹  /new (开启新会话)   /resume (恢复历史会话)   /exit (退出 TUI)
```

```
  ↹  /new (开启新会话)   /go-coding-standards   /go-lint-guide
```

---

## Shell Execution Mode (`!` prefix)

When the input box starts with `!`, the TUI enters Shell mode: the status bar switches to a dark green background, the input area shows a `[SHELL] $` badge, and the footer displays dedicated keyboard shortcut hints. Pressing `Enter` asynchronously executes the command via `dispatchShellCommand`; command output is appended to the Scrollback and cached in `pendingShellOutput`, to be prepended into the LLM context before the next `dispatch()`.

The related types and functions are located in `tui_update.go`:

| Symbol | Purpose |
|------|------|
| `shellResultMsg` | A Bubbletea Msg carrying the command execution result (cmd / output / isErr / dur) |
| `dispatchShellCommand` | Intercepts interactive commands, appends a "$ cmd" line, returns the `runShellCmd` tea.Cmd |
| `runShellCmd` | Returns a tea.Cmd that asynchronously executes bash -c (30s timeout) |
| `truncateUTF8` | Byte-safe truncation, ensuring multi-byte UTF-8 character boundaries are not broken |
| `isInteractiveCmd` | Detects whether the first token is a known PTY-dependent program |
| `maxShellDisplayLen` | Truncation cap on the TUI display side (4096 bytes) |
| `maxShellContextLen` | Truncation cap on the LLM context storage side (2048 bytes) |

See [Shell Execution Feature Technical Design](shell-execution.md) for detailed implementation.

---

## Context Propagation

```
signal.NotifyContext(SIGINT/SIGTERM)  ← outerCtx (main.go)
  │
  ├─ tea.WithContext(outerCtx)        ← program-level Bubbletea context
  │    When SIGTERM arrives, Bubbletea automatically exits
  │
  └─ context.WithCancel(outerCtx)    ← a child context derived for each Agent run
       ├─ stored in m.cancelFn
       └─ Ctrl-C → cancelFn()        ← cancels the current Agent, does not exit the TUI
```

---

## Package-Level Style Variables

All `lipgloss.Style` values are defined in a package-level `var` block, avoiding repeated allocation on every `View()` call per frame:

| Variable | Color | Purpose |
|------|------|------|
| `userMsgStyle` | Color "12", Bold | User message label |
| `assistantStyle` | Color "10", Bold | Agent reply label |
| `dimStyle` | Color "240" | Gray auxiliary text |
| `errorStyle` | Color "9" | Error message |
| `statusBarStyle` | Bg "235" / Fg "11" | Default mode StatusBar background |
| `toolRunStyle` | Color "11" | Tool name (running, yellow) |
| `verbRunStyle` | Color "226" | Spinner + verb (bright yellow) |
| `toolOKStyle` | Color "10" | Tool succeeded (green) |
| `toolErrStyle` | Color "9" | Tool failed (red) |
| `doneStyle` | Color "10", Bold | Task complete (bold green) |
| `skillStyle` | Color "14" | Skill activated (cyan) |
| `cyanStyle` | Color "81" | Default mode accent text |
| `brandStyle` | Color "226", Bold | harness9 brand name |
| `sepStyle` | Color "237" | Separator line |
| `planAccentStyle` | Color "220" | Plan Mode accent text (amber) |
| `planStatusBarStyle` | Bg "94" / Fg "220" | Plan Mode StatusBar background |
| `planModeLabelStyle` | Color "208", Bold | Status bar `[PLAN]` label |
| `thinkingHeaderStyle` | Color "238", Italic | Thinking block header (« thinking ») |
| `thinkingLineStyle` | Color "238" | Thinking block content line (│ prefix) |
| `thinkingEndStyle` | Color "236" | Thinking block closing line (└ separator) |
| `shellCmdStyle` | Color "33", Bold | Shell mode: command line `$ cmd` |
| `shellOutputStyle` | Color "250" | Shell mode: output line (light gray) |
| `shellOKStyle` | Color "34" | Shell mode: `✓ complete` |
| `shellErrStyle` | Color "160" | Shell mode: `✗ non-zero exit` |
| `shellStatusBarStyle` | Bg "22" / Fg "120" | Shell mode StatusBar background (dark green) |
| `shellModeTagStyle` | Bg "58" / Fg "226", Bold | Input area `[SHELL]` badge |
| `shellModeAccentStyle` | Color "83" | Shell mode accent text (bright green) |
| `shellModePromptStyle` | Color "83", Bold | Input area `$ ` prompt |
| `shellModeLabelInBarStyle` | Color "83", Bold | Status bar `SHELL` label |

---

## Mode Color Priority

The three modes are clearly distinguished via the status bar background color. The color-switching logic is centralized in two methods in `tui_view.go`:

```go
func (m tuiModel) accentStyle() lipgloss.Style      // accent color (links, session ID, shortcuts)
func (m tuiModel) activeStatusBarStyle() lipgloss.Style  // status bar background
```

Priority (high → low): Shell mode > Plan mode > Default mode.

```
shellMode=true  →  dark green background #22 + bright green accent #83
Plan/AutoEdit   →  dark orange background #94 + amber accent #220
Default         →  dark gray background #235 + cyan accent #81
```

`renderStatusBar`, `renderFooter`, and `renderTodoLines` all uniformly call these two methods, so there is no scattered if-logic in the View layer.

---

## Technical Dependencies

| Library | Version | Purpose |
|------|------|------|
| `github.com/charmbracelet/bubbletea` | v1.3.10 | Elm Architecture TUI framework, AltScreen + mouse events |
| `github.com/charmbracelet/lipgloss` | v1.1.x | Terminal styles and colors |
| `github.com/charmbracelet/bubbles` | v1.0.0 | spinner (tool progress) + textinput (input box) |
| `github.com/charmbracelet/glamour` | v1.0.0 | Markdown rendering (code blocks, bold, lists, etc.) |
| `github.com/charmbracelet/x/term` | indirect dependency | TTY detection (`term.IsTerminal`) |
