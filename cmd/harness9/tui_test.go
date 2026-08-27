package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/harness9/internal/engine"
	mcppkg "github.com/harness9/internal/mcp"
	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/subagent"
)

// newTestModel 返回适合单元测试的 tuiModel（nil engine/skills，固定尺寸）。
func newTestModel() tuiModel {
	m := newTUIModel(nil, nil, nil, nil, nil, nil, nil, nil, context.Background(), "/tmp/test", "test-model", nil, nil)
	m.width = 80
	m.height = 24
	return m
}

// applyUpdate 调用 m.Update(msg)，返回更新后的 tuiModel。
// 丢弃返回的 tea.Cmd（单元测试中不执行 Cmd）。
func applyUpdate(m tuiModel, msg tea.Msg) tuiModel {
	updated, _ := m.Update(msg)
	return updated.(tuiModel)
}

func TestEventActionDelta_AppendsToLastLine(t *testing.T) {
	m := newTestModel()
	m.lines = []string{""}
	m.running = true

	m = applyUpdate(m, eventMsg{Type: engine.EventActionDelta, Data: "hello"})
	if got := m.lines[len(m.lines)-1]; got != "hello" {
		t.Errorf("first delta: got %q, want %q", got, "hello")
	}

	m = applyUpdate(m, eventMsg{Type: engine.EventActionDelta, Data: " world"})
	if got := m.lines[len(m.lines)-1]; got != "hello world" {
		t.Errorf("second delta: got %q, want %q", got, "hello world")
	}
}

func TestEventActionDelta_InitializesEmptyLines(t *testing.T) {
	m := newTestModel()
	m.lines = nil
	m.running = true

	m = applyUpdate(m, eventMsg{Type: engine.EventActionDelta, Data: "hi"})
	if len(m.lines) == 0 {
		t.Fatal("lines should not be empty after delta on nil slice")
	}
	if got := m.lines[len(m.lines)-1]; got != "hi" {
		t.Errorf("got %q, want %q", got, "hi")
	}
}

func TestEventToolStart_SetsCurrentTool(t *testing.T) {
	m := newTestModel()
	m.running = true

	tc := schema.ToolCall{Name: "bash", ID: "1", Arguments: json.RawMessage(`{"command":"ls"}`)}
	m = applyUpdate(m, eventMsg{Type: engine.EventToolStart, Data: tc})

	if m.currentTool != "bash" {
		t.Errorf("currentTool = %q, want %q", m.currentTool, "bash")
	}
	if m.toolStart.IsZero() {
		t.Error("toolStart should be set when tool starts")
	}
	if m.toolArgs == nil {
		t.Error("toolArgs should be set on EventToolStart")
	}
}

func TestEventToolResult_ClearsCurrentToolAndAppendsLine(t *testing.T) {
	m := newTestModel()
	m.running = true
	m.currentTool = "bash"
	m.lines = []string{}

	data := engine.ToolResultData{Result: schema.ToolResult{Output: "ok", IsError: false}, Duration: 100 * time.Millisecond}
	m = applyUpdate(m, eventMsg{Type: engine.EventToolResult, Data: data})

	if m.currentTool != "" {
		t.Errorf("currentTool should be cleared, got %q", m.currentTool)
	}
	if len(m.lines) == 0 {
		t.Error("completion line should be appended to scrollback")
	}
	if !strings.Contains(m.lines[len(m.lines)-1], "bash") {
		t.Errorf("completion line should mention tool name, got %q", m.lines[len(m.lines)-1])
	}
}

func TestEventToolResult_ErrorMark(t *testing.T) {
	m := newTestModel()
	m.running = true
	m.currentTool = "bash"
	m.lines = []string{}

	data := engine.ToolResultData{Result: schema.ToolResult{Output: "failed", IsError: true}, Duration: 0}
	m = applyUpdate(m, eventMsg{Type: engine.EventToolResult, Data: data})

	if len(m.lines) == 0 {
		t.Fatal("completion line should be appended")
	}
	if !strings.Contains(m.lines[len(m.lines)-1], "✗") {
		t.Errorf("error result should use ✗, got %q", m.lines[len(m.lines)-1])
	}
}

func TestEventDone_ResetsRunningState(t *testing.T) {
	m := newTestModel()
	m.running = true
	m.currentTool = "bash"
	m.lines = []string{""}
	var cancelled bool
	m.cancelFn = func() { cancelled = true }

	m = applyUpdate(m, eventMsg{Type: engine.EventDone})

	if m.running {
		t.Error("running should be false after EventDone")
	}
	if m.currentTool != "" {
		t.Errorf("currentTool should be cleared, got %q", m.currentTool)
	}
	if !cancelled {
		t.Error("EventDone should call cancelFn to release context")
	}
}

func TestEventError_AppendsToScrollbackAndResetsRunning(t *testing.T) {
	m := newTestModel()
	m.running = true
	m.currentTool = "bash"
	m.lines = []string{"partial text"}
	m.pendingReplyStart = 0

	m = applyUpdate(m, eventMsg{Type: engine.EventError, Data: "context cancelled"})

	if m.running {
		t.Error("running should be false after EventError")
	}
	if m.currentTool != "" {
		t.Errorf("currentTool should be cleared, got %q", m.currentTool)
	}
	if len(m.lines) == 0 {
		t.Fatal("error line should be appended to scrollback")
	}
	if !strings.Contains(m.lines[len(m.lines)-1], "context cancelled") {
		t.Errorf("last line should contain error message, got %q", m.lines[len(m.lines)-1])
	}
}

func TestWindowSizeMsg_UpdatesDimensions(t *testing.T) {
	m := newTestModel()

	m = applyUpdate(m, tea.WindowSizeMsg{Width: 120, Height: 40})

	if m.width != 120 || m.height != 40 {
		t.Errorf("got %dx%d, want 120x40", m.width, m.height)
	}
}

func TestKeyCtrlC_WhenIdle_ReturnsQuitCmd(t *testing.T) {
	m := newTestModel()
	m.running = false

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("expected a non-nil quit command")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}

func TestKeyCtrlC_WhenRunning_CallsCancelFn(t *testing.T) {
	m := newTestModel()
	m.running = true
	var cancelled bool
	m.cancelFn = func() { cancelled = true }

	m = applyUpdate(m, tea.KeyMsg{Type: tea.KeyCtrlC})

	if !cancelled {
		t.Error("cancelFn should be called when Ctrl-C during agent run")
	}
	if !m.running {
		// running stays true until EventDone/EventError arrives from engine
		t.Error("running should remain true until engine confirms cancellation")
	}
}

func TestKeyEnter_EmptyInput_Ignored(t *testing.T) {
	m := newTestModel()
	m.running = false
	initialLines := len(m.lines)

	m = applyUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.lines) != initialLines {
		t.Error("empty Enter should not append to scrollback")
	}
}

func TestKeyEnter_WhenRunning_Ignored(t *testing.T) {
	m := newTestModel()
	m.running = true
	m.input.SetValue("do something")
	initialLines := len(m.lines)

	m = applyUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.lines) != initialLines {
		t.Error("Enter while agent is running should be ignored")
	}
}

func TestKeyPgUp_EntersManualScroll(t *testing.T) {
	m := newTestModel()
	// 填充足够多的行触发滚动
	for i := 0; i < 30; i++ {
		m.lines = append(m.lines, "line")
	}

	m = applyUpdate(m, tea.KeyMsg{Type: tea.KeyPgUp})

	if m.viewTop < 0 {
		t.Error("PgUp should enter manual scroll mode (viewTop >= 0)")
	}
}

func TestMouseWheelUp_ScrollsUp(t *testing.T) {
	m := newTestModel()
	for i := 0; i < 30; i++ {
		m.lines = append(m.lines, "line")
	}

	m = applyUpdate(m, tea.MouseMsg{
		Button: tea.MouseButtonWheelUp,
		Action: tea.MouseActionPress,
	})

	if m.viewTop < 0 {
		t.Error("MouseWheelUp should enter manual scroll mode (viewTop >= 0)")
	}
}

func TestMouseWheelDown_AtBottom_NoChange(t *testing.T) {
	m := newTestModel()
	// viewTop=-1（底部），向下滚动不应改变状态
	m = applyUpdate(m, tea.MouseMsg{
		Button: tea.MouseButtonWheelDown,
		Action: tea.MouseActionPress,
	})

	if m.viewTop != -1 {
		t.Errorf("WheelDown at bottom should keep viewTop=-1, got %d", m.viewTop)
	}
}

func TestKeyEnd_ReturnsToAutoScroll(t *testing.T) {
	m := newTestModel()
	m.viewTop = 5 // 已在手动滚动

	m = applyUpdate(m, tea.KeyMsg{Type: tea.KeyEnd})

	if m.viewTop != -1 {
		t.Errorf("End should reset to auto-scroll (viewTop=-1), got %d", m.viewTop)
	}
}

func TestKeyPgDown_AtBottom_StaysAutoScroll(t *testing.T) {
	m := newTestModel()
	// viewTop=-1 时按 PgDn 不应改变状态
	m = applyUpdate(m, tea.KeyMsg{Type: tea.KeyPgDown})

	if m.viewTop != -1 {
		t.Errorf("PgDn at auto-scroll bottom should keep viewTop=-1, got %d", m.viewTop)
	}
}

func TestSummarizeTool_Bash(t *testing.T) {
	args := json.RawMessage(`{"command":"go test ./... 2>&1 | head -20"}`)
	got := summarizeTool("bash", args)
	if got != "go test ./... 2>&1 | head -20" {
		t.Errorf("got %q", got)
	}
}

func TestSummarizeTool_Bash_Truncates(t *testing.T) {
	long := strings.Repeat("x", 130)
	args := json.RawMessage(`{"command":"` + long + `"}`)
	got := summarizeTool("bash", args)
	if len([]rune(got)) != 121 { // 120 chars + "…"
		t.Errorf("expected 121 runes (120 + ellipsis), got %d: %q", len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix, got %q", got)
	}
}

func TestSummarizeTool_ReadFile(t *testing.T) {
	args := json.RawMessage(`{"path":"/home/user/project/main.go"}`)
	got := summarizeTool("read_file", args)
	if got != "main.go" {
		t.Errorf("got %q, want %q", got, "main.go")
	}
}

func TestSummarizeTool_Other(t *testing.T) {
	args := json.RawMessage(`{"key":"value"}`)
	got := summarizeTool("custom_tool", args)
	if got != `{"key":"value"}` {
		t.Errorf("got %q", got)
	}
}

func TestSummarizeTool_WriteFile(t *testing.T) {
	args := json.RawMessage(`{"path":"/home/user/project/utils.go","content":"package main"}`)
	got := summarizeTool("write_file", args)
	if got != "utils.go" {
		t.Errorf("got %q, want %q", got, "utils.go")
	}
}

func TestSummarizeTool_InvalidArgs(t *testing.T) {
	args := json.RawMessage(`not-json`)
	got := summarizeTool("bash", args)
	if got != "" {
		t.Errorf("invalid args should return empty string, got %q", got)
	}
}

func TestPhaseTransition_WelcomeToChat(t *testing.T) {
	m := newTestModel()
	if m.phase != phaseWelcome {
		t.Fatal("new model should start in phaseWelcome")
	}
	m.input.SetValue("hello world")

	m = applyUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.phase != phaseChat {
		t.Errorf("phase should be phaseChat after first Enter, got %v", m.phase)
	}
}

func TestPhaseStaysChat_AfterFirstEnter(t *testing.T) {
	m := newTestModel()
	m.phase = phaseChat
	m.input.SetValue("second message")

	m = applyUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.phase != phaseChat {
		t.Errorf("phase should remain phaseChat, got %v", m.phase)
	}
}

func TestSpinnerVerbRotation(t *testing.T) {
	m := newTestModel()
	m.running = true
	m.currentTool = "bash"
	m.verbIdx = 0
	m.tickCount = 29

	m = applyUpdate(m, spinner.TickMsg{})

	if m.tickCount != 30 {
		t.Errorf("tickCount should be 30, got %d", m.tickCount)
	}
	if m.verbIdx != 1 {
		t.Errorf("verbIdx should advance to 1 after 30 ticks, got %d", m.verbIdx)
	}

	// verify wraparound
	m.verbIdx = 5
	m.tickCount = 59
	m = applyUpdate(m, spinner.TickMsg{})
	if m.verbIdx != 0 {
		t.Errorf("verbIdx should wrap to 0, got %d", m.verbIdx)
	}
}

func TestScrollHeight_DynamicReservedLines(t *testing.T) {
	m := newTestModel() // height = 24
	if got := m.scrollHeight(); got != 21 {
		t.Errorf("idle: want 21 (24-3), got %d", got)
	}
	m.running = true
	m.currentTool = "bash"
	if got := m.scrollHeight(); got != 20 {
		t.Errorf("running with tool: want 20 (24-4), got %d", got)
	}
}

func TestEventThinkingDelta_CreatesThinkingBlock(t *testing.T) {
	m := newTestModel()
	m.running = true

	m = applyUpdate(m, eventMsg{Type: engine.EventThinkingDelta, Data: "step one"})

	// 应该有 « thinking » 标题行
	var hasHeader bool
	for _, line := range m.lines {
		if strings.Contains(line, "thinking") {
			hasHeader = true
			break
		}
	}
	if !hasHeader {
		t.Error("lines should contain thinking header after first EventThinkingDelta")
	}
	if m.thinkingLineStart == -1 {
		t.Error("thinkingLineStart should be set after first EventThinkingDelta")
	}
	if m.pendingThinking != "step one" {
		t.Errorf("pendingThinking = %q, want %q", m.pendingThinking, "step one")
	}
}

func TestEventThinkingDelta_AccumulatesContent(t *testing.T) {
	m := newTestModel()
	m.running = true

	m = applyUpdate(m, eventMsg{Type: engine.EventThinkingDelta, Data: "part one"})
	m = applyUpdate(m, eventMsg{Type: engine.EventThinkingDelta, Data: " part two"})

	if m.pendingThinking != "part one part two" {
		t.Errorf("pendingThinking = %q, want %q", m.pendingThinking, "part one part two")
	}
}

func TestEventActionDelta_FlushesThinkingBlock(t *testing.T) {
	m := newTestModel()
	m.running = true

	// 先发 thinking delta
	m = applyUpdate(m, eventMsg{Type: engine.EventThinkingDelta, Data: "reason"})
	if m.thinkingLineStart == -1 {
		t.Fatal("setup: thinkingLineStart should be set")
	}

	// 再发 action delta，应 flush thinking 块
	m = applyUpdate(m, eventMsg{Type: engine.EventActionDelta, Data: "answer"})

	if m.thinkingLineStart != -1 {
		t.Error("thinkingLineStart should be reset to -1 after action delta flushes thinking")
	}
	if m.pendingThinking != "" {
		t.Errorf("pendingThinking should be empty after flush, got %q", m.pendingThinking)
	}
	// action 文本应该在 lines 中
	if m.pendingReply != "answer" {
		t.Errorf("pendingReply = %q, want %q", m.pendingReply, "answer")
	}
}

func TestEventToolStart_FlushesThinkingBlock(t *testing.T) {
	m := newTestModel()
	m.running = true

	m = applyUpdate(m, eventMsg{Type: engine.EventThinkingDelta, Data: "reason"})
	if m.thinkingLineStart == -1 {
		t.Fatal("setup: thinkingLineStart should be set")
	}

	tc := schema.ToolCall{Name: "bash", ID: "1", Arguments: json.RawMessage(`{}`)}
	m = applyUpdate(m, eventMsg{Type: engine.EventToolStart, Data: tc})

	if m.thinkingLineStart != -1 {
		t.Error("thinkingLineStart should be reset after EventToolStart flushes thinking")
	}
	if m.pendingThinking != "" {
		t.Errorf("pendingThinking should be empty after flush, got %q", m.pendingThinking)
	}
}

func TestEventDone_FlushesActiveThinkingBlock(t *testing.T) {
	m := newTestModel()
	m.running = true
	m.cancelFn = func() {}

	m = applyUpdate(m, eventMsg{Type: engine.EventThinkingDelta, Data: "reason"})
	if m.thinkingLineStart == -1 {
		t.Fatal("setup: thinkingLineStart should be set")
	}

	m = applyUpdate(m, eventMsg{Type: engine.EventDone})

	if m.thinkingLineStart != -1 {
		t.Error("EventDone should flush thinking block (thinkingLineStart reset to -1)")
	}
	if m.pendingThinking != "" {
		t.Errorf("pendingThinking should be empty after EventDone flush, got %q", m.pendingThinking)
	}
}

func TestThinkingWordWrap_ShortText(t *testing.T) {
	lines := thinkingWordWrap("hello world", 80)
	if len(lines) != 1 || lines[0] != "hello world" {
		t.Errorf("short text should stay on one line, got %v", lines)
	}
}

func TestThinkingWordWrap_LongText(t *testing.T) {
	// 40 chars wide: "one two three four" each word ~5 chars
	text := "alpha beta gamma delta epsilon zeta eta theta"
	lines := thinkingWordWrap(text, 20)
	if len(lines) < 2 {
		t.Errorf("long text should wrap into multiple lines, got %v", lines)
	}
	for _, line := range lines {
		if len([]rune(line)) > 20 {
			t.Errorf("line exceeds width: %q (%d runes)", line, len([]rune(line)))
		}
	}
}

func TestThinkingWordWrap_ZeroWidth(t *testing.T) {
	text := "no wrapping when width is zero"
	lines := thinkingWordWrap(text, 0)
	if len(lines) != 1 || lines[0] != text {
		t.Errorf("zero width should not wrap, got %v", lines)
	}
}

func TestRenderThinkingLines_WrapsAtWidth(t *testing.T) {
	// width=40: prefix "  │ " = 4 cols, so wrap at 35 runes
	longText := "I need to create a temporary directory for a new Go web project with no external dependencies"
	lines := renderThinkingLines(longText, 40)
	if len(lines) < 2 {
		t.Errorf("expected wrapping into multiple lines at width=40, got %d line(s): %v", len(lines), lines)
	}
}

// TestEventError_ResetsThinkingBlock 验证 EventError 在 thinking 块活跃时，
// 正确截断 lines（移除从 thinkingLineStart 起的所有行）并重置 thinkingLineStart。
func TestEventError_ResetsThinkingBlock(t *testing.T) {
	m := newTestModel()
	m.running = true
	m.cancelFn = func() {}

	// 先触发 thinking 块
	m = applyUpdate(m, eventMsg{Type: engine.EventThinkingDelta, Data: "deep reason"})
	if m.thinkingLineStart == -1 {
		t.Fatal("setup: thinkingLineStart should be set after thinking delta")
	}
	linesWithThinking := len(m.lines)

	// 再触发错误
	m = applyUpdate(m, eventMsg{Type: engine.EventError, Data: "network error"})

	if m.thinkingLineStart != -1 {
		t.Error("EventError should reset thinkingLineStart to -1")
	}
	if m.pendingThinking != "" {
		t.Errorf("pendingThinking should be empty after EventError, got %q", m.pendingThinking)
	}
	// lines 应该已截断（移除了 thinking 块的所有行，并追加了错误行）
	if len(m.lines) >= linesWithThinking {
		t.Errorf("EventError should truncate thinking lines; linesWithThinking=%d, after=%d",
			linesWithThinking, len(m.lines))
	}
	if !strings.Contains(m.lines[len(m.lines)-1], "network error") {
		t.Errorf("last line should contain error message, got %q", m.lines[len(m.lines)-1])
	}
}

// TestEventThinkingDelta_NoBlankLineBeforeHeader 验证 thinking 块头部（« thinking »）
// 直接追加在 "◆ harness9:" 之后，不出现孤立的空行占位符。
func TestEventThinkingDelta_NoBlankLineBeforeHeader(t *testing.T) {
	m := newTestModel()
	// 模拟 dispatch 的初始状态：尾部有一个空行占位符，pendingReplyStart 指向它
	m.lines = []string{"◆ harness9:", ""}
	m.pendingReplyStart = 1 // 指向空行占位符
	m.thinkingLineStart = -1
	m.running = true

	m = applyUpdate(m, eventMsg{Type: engine.EventThinkingDelta, Data: "first thought"})

	// 检查 lines 中不存在连续的空行（""）紧跟 thinking 内容
	for i, line := range m.lines {
		if line == "" && i+1 < len(m.lines) && strings.Contains(m.lines[i+1], "thinking") {
			t.Errorf("blank line at index %d precedes thinking header at index %d", i, i+1)
		}
	}
	// thinking 标题行必须存在
	var hasHeader bool
	for _, line := range m.lines {
		if strings.Contains(line, "thinking") {
			hasHeader = true
			break
		}
	}
	if !hasHeader {
		t.Error("thinking header should be present after first EventThinkingDelta")
	}
}

// TestThinkingWordWrap_FirstWordExceedsWidth 验证首词超长（如 URL）时被正确 hard-break。
func TestThinkingWordWrap_FirstWordExceedsWidth(t *testing.T) {
	url := "https://pkg.go.dev/github.com/charmbracelet/bubbletea#Model"
	lines := thinkingWordWrap(url, 20)
	for _, line := range lines {
		if len([]rune(line)) > 20 {
			t.Errorf("line exceeds width 20: %q (%d runes)", line, len([]rune(line)))
		}
	}
	if len(lines) < 2 {
		t.Errorf("long URL should be split into multiple lines, got %v", lines)
	}
}

// --- Shell 模式测试 ---

func TestShellExec_EmptyCmd_Noop(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("!")
	initialLines := len(m.lines)

	m = applyUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})

	// 空命令：不追加任何输出行
	if len(m.lines) != initialLines {
		t.Errorf("empty '!' should not append lines, got %d extra", len(m.lines)-initialLines)
	}
}

func TestShellExec_InteractiveCmd_ShowsError(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("!vim file.go")

	m = applyUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})

	// 应该有 "$ vim file.go" 行和拒绝错误行
	if len(m.lines) < 2 {
		t.Fatalf("expected at least 2 lines, got %d", len(m.lines))
	}
	var hasCmdLine, hasErrLine bool
	for _, line := range m.lines {
		if strings.Contains(line, "vim file.go") {
			hasCmdLine = true
		}
		if strings.Contains(line, "交互式终端") {
			hasErrLine = true
		}
	}
	if !hasCmdLine {
		t.Error("expected '$ vim file.go' in output")
	}
	if !hasErrLine {
		t.Error("expected interactive terminal error message")
	}
}

func TestShellExec_NormalCmd_AppendsCmdLine(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("!echo hello")
	initialLines := len(m.lines)

	m = applyUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})

	// 应该追加 "$ echo hello" 行
	if len(m.lines) <= initialLines {
		t.Fatal("expected at least one line appended for shell command")
	}
	var hasCmdLine bool
	for _, line := range m.lines {
		if strings.Contains(line, "echo hello") {
			hasCmdLine = true
			break
		}
	}
	if !hasCmdLine {
		t.Error("expected '$ echo hello' line in scrollback")
	}
	// phase 应切换为 phaseChat
	if m.phase != phaseChat {
		t.Errorf("phase should be phaseChat after '!' command, got %v", m.phase)
	}
}

func TestShellExec_DoesNotShowUserPrefix(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("!git status")

	m = applyUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})

	for _, line := range m.lines {
		if strings.Contains(line, "▶ You:") {
			t.Error("shell command should not show '▶ You:' prefix")
		}
	}
}

func TestShellResultMsg_SuccessRendersOutput(t *testing.T) {
	m := newTestModel()

	m = applyUpdate(m, shellResultMsg{
		cmd:    "echo hi",
		output: "hi\n",
		isErr:  false,
		dur:    5 * time.Millisecond,
	})

	var hasOutput, hasOK bool
	for _, line := range m.lines {
		if strings.Contains(line, "hi") {
			hasOutput = true
		}
		if strings.Contains(line, "✓") {
			hasOK = true
		}
	}
	if !hasOutput {
		t.Error("expected output 'hi' in scrollback")
	}
	if !hasOK {
		t.Error("expected ✓ success line for zero-exit command")
	}
}

func TestShellResultMsg_ErrorRendersOutput(t *testing.T) {
	m := newTestModel()

	m = applyUpdate(m, shellResultMsg{
		cmd:    "false",
		output: "",
		isErr:  true,
		dur:    2 * time.Millisecond,
	})

	var hasErr bool
	for _, line := range m.lines {
		if strings.Contains(line, "✗") {
			hasErr = true
			break
		}
	}
	if !hasErr {
		t.Error("expected ✗ error line for non-zero-exit command")
	}
}

func TestShellResultMsg_LongOutput_Truncated(t *testing.T) {
	m := newTestModel()
	longOutput := strings.Repeat("x", 5000)

	m = applyUpdate(m, shellResultMsg{
		cmd:    "cat bigfile",
		output: longOutput,
		isErr:  false,
		dur:    10 * time.Millisecond,
	})

	var hasTruncation bool
	for _, line := range m.lines {
		if strings.Contains(line, "已截断") {
			hasTruncation = true
			break
		}
	}
	if !hasTruncation {
		t.Error("output > 4096 bytes should be truncated with a notice")
	}
}

func TestShellResultMsg_BuffersPendingOutput(t *testing.T) {
	m := newTestModel()

	m = applyUpdate(m, shellResultMsg{
		cmd:    "git log --oneline",
		output: "abc1234 feat: something\n",
		isErr:  false,
		dur:    3 * time.Millisecond,
	})

	if len(m.pendingShellOutput) != 1 {
		t.Fatalf("expected 1 pending shell output entry, got %d", len(m.pendingShellOutput))
	}
	if !strings.Contains(m.pendingShellOutput[0], "git log --oneline") {
		t.Errorf("pending entry should contain command, got %q", m.pendingShellOutput[0])
	}
	if !strings.Contains(m.pendingShellOutput[0], "abc1234") {
		t.Errorf("pending entry should contain output, got %q", m.pendingShellOutput[0])
	}
}

func TestShellResultMsg_LargeOutput_TruncatedAtStorage(t *testing.T) {
	// 验证 W2：超过 maxShellContextLen 的输出在存入 pendingShellOutput 时已截断
	m := newTestModel()
	largeOutput := strings.Repeat("a", maxShellContextLen+100)

	m = applyUpdate(m, shellResultMsg{
		cmd:    "cat bigfile",
		output: largeOutput,
		isErr:  false,
		dur:    5 * time.Millisecond,
	})

	if len(m.pendingShellOutput) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(m.pendingShellOutput))
	}
	// 存储的 entry 格式为 "$ cmd\noutput"，长度应 ≤ maxShellContextLen + 头部开销
	entry := m.pendingShellOutput[0]
	// 输出部分不超过 maxShellContextLen
	cmdHeader := "$ cat bigfile\n"
	outputPart := strings.TrimPrefix(entry, cmdHeader)
	if len(outputPart) > maxShellContextLen {
		t.Errorf("stored output part len=%d, should be ≤ maxShellContextLen=%d", len(outputPart), maxShellContextLen)
	}
}

func TestTruncateUTF8_ASCII(t *testing.T) {
	s := "hello world"
	got := truncateUTF8(s, 5)
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestTruncateUTF8_NoTruncation(t *testing.T) {
	s := "hi"
	got := truncateUTF8(s, 100)
	if got != s {
		t.Errorf("got %q, want %q", got, s)
	}
}

func TestTruncateUTF8_MultibyteBoundary(t *testing.T) {
	// "你好" = 6 bytes in UTF-8 (3 bytes each)
	s := "你好"
	// 截断到 4 bytes：落在 "好" 的中间（第 1 个续字节），应退回到 "你" 结束处（3 bytes）
	got := truncateUTF8(s, 4)
	if got != "你" {
		t.Errorf("should back off to valid UTF-8 boundary, got %q (len=%d)", got, len(got))
	}
	// 确认结果是合法 UTF-8
	for i, r := range got {
		if r == '�' {
			t.Errorf("invalid UTF-8 at position %d", i)
		}
	}
}

func TestTruncateUTF8_ExactBoundary(t *testing.T) {
	// "你" = 3 bytes，截断到 3 bytes，应原样返回
	got := truncateUTF8("你好", 3)
	if got != "你" {
		t.Errorf("got %q, want %q", got, "你")
	}
}

func TestDispatch_InjectsPendingShellOutput(t *testing.T) {
	m := newTestModel()
	m.pendingShellOutput = []string{"$ git status\nOn branch main\n"}

	// dispatch 应将 pendingShellOutput 前置到 prompt 并清空缓冲
	m, _ = m.dispatch("请分析上面的输出")

	if len(m.pendingShellOutput) != 0 {
		t.Error("pendingShellOutput should be cleared after dispatch")
	}
}

func TestDispatch_ClearsPendingShellOutputAfterInject(t *testing.T) {
	m := newTestModel()
	m.pendingShellOutput = []string{"$ ls\nfoo bar\n", "$ pwd\n/home/user\n"}

	m, _ = m.dispatch("继续")

	if len(m.pendingShellOutput) != 0 {
		t.Errorf("pendingShellOutput should be empty after dispatch, got %d entries", len(m.pendingShellOutput))
	}
}

// --- @agent 前台直跑测试 ---

func TestDispatchMentionUnknownAgent(t *testing.T) {
	m := newTestModel()
	reg := subagent.NewRegistry()
	_ = reg.Register(subagent.SubAgentDefinition{Name: "explorer", Description: "d", SystemPrompt: "p"})
	m.subAgentReg = reg
	m.subAgentRunner = subagent.NewRunner(subagent.RunnerConfig{})
	m2, _ := m.dispatchMention("@ghost 干活")
	if !strings.Contains(strings.Join(m2.lines, "\n"), "未知子代理") {
		t.Fatalf("应提示未知子代理: %q", strings.Join(m2.lines, "\n"))
	}
}

func TestDispatchMentionEmptyTask(t *testing.T) {
	m := newTestModel()
	reg := subagent.NewRegistry()
	_ = reg.Register(subagent.SubAgentDefinition{Name: "explorer", Description: "d", SystemPrompt: "p"})
	m.subAgentReg = reg
	m.subAgentRunner = subagent.NewRunner(subagent.RunnerConfig{})
	m2, _ := m.dispatchMention("@explorer")
	if !strings.Contains(strings.Join(m2.lines, "\n"), "请在 @explorer 后输入任务") {
		t.Fatal("空任务应提示")
	}
}

func TestIsInteractiveCmd(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"vim file.go", true},
		{"vi /etc/hosts", true},
		{"ssh user@host", true},
		{"top", true},
		{"echo hello", false},
		{"git status", false},
		{"go test ./...", false},
		{"", false},
		{"/usr/bin/vim", true}, // 绝对路径也能检测
	}
	for _, tc := range cases {
		got := isInteractiveCmd(tc.cmd)
		if got != tc.want {
			t.Errorf("isInteractiveCmd(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

// --- Shell 模式 UI 视觉状态测试 ---

// triggerShellModeDetection 通过一次 KeyRight 消息（不修改输入内容）触发
// Update 末尾的 textinput 更新路径，从而激活 shellMode 检测逻辑。
// KeyRight 未在 switch msg.Type 中处理，会自动 fall-through 到 m.input.Update。
func triggerShellModeDetection(m tuiModel) tuiModel {
	return applyUpdate(m, tea.KeyMsg{Type: tea.KeyRight})
}

func TestShellMode_ActivatedByTextInputValue(t *testing.T) {
	m := newTestModel()
	if m.shellMode {
		t.Error("initial shellMode should be false")
	}

	m.input.SetValue("!echo hi")
	m = triggerShellModeDetection(m)

	if !m.shellMode {
		t.Error("shellMode should be true when input starts with '!'")
	}
	if m.input.Placeholder != `Shell 命令... "git status"` {
		t.Errorf("placeholder should change in shell mode, got %q", m.input.Placeholder)
	}
}

func TestShellMode_DeactivatesWhenExclamationRemoved(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("!ls")
	m = triggerShellModeDetection(m)
	if !m.shellMode {
		t.Fatal("setup: shellMode should be true")
	}

	m.input.SetValue("ls") // 移除 "!" 前缀
	m = triggerShellModeDetection(m)

	if m.shellMode {
		t.Error("shellMode should be false when input no longer starts with '!'")
	}
	if m.input.Placeholder != "输入任务..." {
		t.Errorf("placeholder should reset, got %q", m.input.Placeholder)
	}
}

func TestShellMode_EscKeyExitsShellMode(t *testing.T) {
	m := newTestModel()
	m.shellMode = true
	m.input.SetValue("!vim")

	m = applyUpdate(m, tea.KeyMsg{Type: tea.KeyEsc})

	if m.shellMode {
		t.Error("Esc should exit shell mode")
	}
	if m.input.Value() != "" {
		t.Errorf("Esc should clear input, got %q", m.input.Value())
	}
	if m.input.Placeholder != "输入任务..." {
		t.Errorf("Esc should reset placeholder, got %q", m.input.Placeholder)
	}
}

func TestShellMode_EnterResetsShellMode(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("!echo test")
	m = triggerShellModeDetection(m)
	if !m.shellMode {
		t.Fatal("setup: shellMode should be true")
	}

	m = applyUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.shellMode {
		t.Error("shellMode should be reset to false after Enter")
	}
}

func TestRenderInput_ShowsShellBadgeInShellMode(t *testing.T) {
	m := newTestModel()
	m.phase = phaseChat
	m.width = 80
	m.height = 24

	// 普通模式 — 无 SHELL 徽章
	normal := m.renderInput()
	if strings.Contains(normal, "SHELL") {
		t.Error("normal mode should not show SHELL badge")
	}

	// Shell 模式 — 有 SHELL 徽章
	m.shellMode = true
	shell := m.renderInput()
	if !strings.Contains(shell, "SHELL") {
		t.Error("shell mode should show SHELL badge in renderInput")
	}
	if strings.Contains(shell, "›") {
		t.Error("shell mode prompt should use $ not ›")
	}
}

func TestRenderFooter_ShowsShellHintsInShellMode(t *testing.T) {
	m := newTestModel()
	m.width = 80
	m.shellMode = false

	normal := m.renderFooter()
	if strings.Contains(normal, "取消") && strings.Contains(normal, "执行") {
		// shell-specific hints should not be in normal footer
		t.Error("normal footer should not show shell-specific hints")
	}

	m.shellMode = true
	shellFooter := m.renderFooter()
	if !strings.Contains(shellFooter, "执行") {
		t.Error("shell mode footer should contain '执行'")
	}
	if !strings.Contains(shellFooter, "取消") {
		t.Error("shell mode footer should contain '取消'")
	}
	if !strings.Contains(shellFooter, "LLM") {
		t.Error("shell mode footer should mention LLM context injection")
	}
}

// TestFlushPendingThinking_UpdatesPendingReplyStart 验证 flushPendingThinking 后
// pendingReplyStart 被更新到 thinking 结束行之后，后续 action 文本从正确位置写入。
func TestFlushPendingThinking_UpdatesPendingReplyStart(t *testing.T) {
	m := newTestModel()
	m.running = true

	// 模拟有 thinking 块的状态
	m = applyUpdate(m, eventMsg{Type: engine.EventThinkingDelta, Data: "reason"})
	linesBeforeFlush := len(m.lines)
	if linesBeforeFlush == 0 {
		t.Fatal("setup: lines should not be empty after thinking delta")
	}

	// 发 action delta，触发 flushPendingThinking + 追加文本
	m = applyUpdate(m, eventMsg{Type: engine.EventActionDelta, Data: "answer"})

	// pendingReplyStart 应在结束行之后（至少 linesBeforeFlush+1，含结束分隔线）
	if m.pendingReplyStart <= linesBeforeFlush {
		t.Errorf("pendingReplyStart=%d should be > %d (lines before flush) after thinking flush",
			m.pendingReplyStart, linesBeforeFlush)
	}
	if m.pendingReply != "answer" {
		t.Errorf("pendingReply = %q, want %q", m.pendingReply, "answer")
	}
}

// TestHandleTaskPanelKeyNavigation 验证任务面板的列表/详情导航：
// ↑↓ 光标夹紧、Enter 进详情、Esc 列表态关闭/详情态返回。
func TestHandleTaskPanelKeyNavigation(t *testing.T) {
	tr := subagent.NewTaskTracker()
	id1 := tr.Start("a", "p1")
	_ = tr.Start("b", "p2")

	m := newTestModel()
	m.subAgentTracker = tr
	m.taskPanelMode = true

	// 列表态：↓ 到 1，再 ↓ 夹紧在 1（共 2 项）。
	mm, _ := m.handleTaskPanelKey(tea.KeyMsg{Type: tea.KeyDown})
	m = mm.(tuiModel)
	if m.taskPanelCursor != 1 {
		t.Fatalf("KeyDown cursor=%d, want 1", m.taskPanelCursor)
	}
	mm, _ = m.handleTaskPanelKey(tea.KeyMsg{Type: tea.KeyDown})
	m = mm.(tuiModel)
	if m.taskPanelCursor != 1 {
		t.Fatalf("KeyDown 应夹紧在 1, 得 %d", m.taskPanelCursor)
	}
	// ↑ 回到 0。
	mm, _ = m.handleTaskPanelKey(tea.KeyMsg{Type: tea.KeyUp})
	m = mm.(tuiModel)
	if m.taskPanelCursor != 0 {
		t.Fatalf("KeyUp cursor=%d, want 0", m.taskPanelCursor)
	}
	// Enter 进详情：cursor=0 → id1。
	mm, _ = m.handleTaskPanelKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(tuiModel)
	if m.taskDetailID != id1 {
		t.Fatalf("Enter 应进入 id1 详情, 得 %q", m.taskDetailID)
	}
	// 详情态 Esc 返回列表。
	mm, _ = m.handleTaskPanelKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(tuiModel)
	if m.taskDetailID != "" {
		t.Fatal("详情态 Esc 应返回列表（taskDetailID 清空）")
	}
	if !m.taskPanelMode {
		t.Fatal("详情态 Esc 不应关闭面板")
	}
	// 列表态 Esc 关闭面板。
	mm, _ = m.handleTaskPanelKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(tuiModel)
	if m.taskPanelMode {
		t.Fatal("列表态 Esc 应关闭面板")
	}
}

// TestBuildMentionHint 验证 @ 前缀的子代理建议提示：前缀匹配、全列出、无匹配。
func TestBuildMentionHint(t *testing.T) {
	reg := subagent.NewRegistry()
	_ = reg.Register(subagent.SubAgentDefinition{Name: "explorer", Description: "只读代码探索", SystemPrompt: "p"})
	_ = reg.Register(subagent.SubAgentDefinition{Name: "code-reviewer", Description: "代码审查", SystemPrompt: "p"})

	m := newTestModel()
	m.subAgentReg = reg

	// 前缀 @expl → 仅 explorer
	m.input.SetValue("@expl")
	hint := m.buildCompletionHint()
	if !strings.Contains(hint, "explorer") {
		t.Fatalf("@expl 应建议 explorer: %q", hint)
	}
	if strings.Contains(hint, "code-reviewer") {
		t.Fatalf("@expl 不应包含 code-reviewer: %q", hint)
	}
	// 裸 @ → 列出全部
	m.input.SetValue("@")
	hint = m.buildCompletionHint()
	if !strings.Contains(hint, "explorer") || !strings.Contains(hint, "code-reviewer") {
		t.Fatalf("@ 应列出全部子代理: %q", hint)
	}
	// 无匹配 → 空
	m.input.SetValue("@zzz")
	if h := m.buildCompletionHint(); h != "" {
		t.Fatalf("@zzz 无匹配应返回空, 得 %q", h)
	}
}

// panelRowCount 统计字符串在终端中实际占用的行数。
// 末尾无 \n 时最后一行也计入（与终端渲染一致）。
func panelRowCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// TestRenderMCPPanel_EmptyServers 验证无 MCP Server 时显示未配置提示。
func TestRenderMCPPanel_EmptyServers(t *testing.T) {
	m := newTestModel()
	m.mcpServers = nil

	out := m.renderMCPPanel(10)
	if !strings.Contains(out, "未配置 MCP Server") {
		t.Errorf("空 Server 时应含未配置提示，got: %q", out)
	}
}

// TestRenderMCPPanel_RowCount 验证面板产生恰好 contentH+3 行，
// 使 View() 追加 "\n" + renderStatusBar 后总高度 = contentH+4 = m.height。
func TestRenderMCPPanel_RowCount(t *testing.T) {
	tools := []mcppkg.ToolDetail{
		{AdapterName: "mcp__srv__tool1", Description: "desc one"},
		{AdapterName: "mcp__srv__tool2", Description: "desc two"},
	}
	servers := []mcppkg.ServerStatus{
		{Name: "srv", Status: mcppkg.StatusConnected, ToolsLen: 2, ToolDetails: tools},
	}

	cases := []struct {
		contentH int
	}{
		{5},
		{10},
		{20},
		{30},
	}

	for _, tc := range cases {
		m := newTestModel()
		m.mcpServers = servers
		m.mcpPanelScroll = 0

		out := m.renderMCPPanel(tc.contentH)
		got := panelRowCount(out)
		want := tc.contentH + 3
		if got != want {
			t.Errorf("contentH=%d: 面板行数 = %d，期望 %d\n输出: %q",
				tc.contentH, got, want, out)
		}
	}
}

// TestRenderMCPPanel_ScrollHint 验证滚动提示仅在内容超出高度时出现。
func TestRenderMCPPanel_ScrollHint(t *testing.T) {
	// 构造 8 行内容（1 header + 2*2 tool + 1 blank + 2 extra tools * 2）= 1+4+1=6 for 2 tools
	tools := func(n int) []mcppkg.ToolDetail {
		out := make([]mcppkg.ToolDetail, n)
		for i := range out {
			out[i] = mcppkg.ToolDetail{AdapterName: "mcp__s__t", Description: "d"}
		}
		return out
	}

	m := newTestModel()
	m.mcpServers = []mcppkg.ServerStatus{
		{Name: "s", Status: mcppkg.StatusConnected, ToolsLen: 2, ToolDetails: tools(2)},
	}
	// totalLines = 1(header) + 2*2(tools) + 1(blank) = 6

	// contentH > totalLines → 不显示滚动提示
	outFit := m.renderMCPPanel(20)
	if strings.Contains(outFit, "滚动") {
		t.Errorf("内容可完整展示时不应显示滚动提示，got: %q", outFit)
	}
	if !strings.Contains(outFit, "Esc") {
		t.Errorf("内容可完整展示时应有 Esc 提示，got: %q", outFit)
	}

	// contentH < totalLines → 显示滚动提示
	outScroll := m.renderMCPPanel(4)
	if !strings.Contains(outScroll, "滚动") {
		t.Errorf("内容溢出时应显示滚动提示，got: %q", outScroll)
	}
}

// TestRenderMCPPanel_ScrollOffset 验证 mcpPanelScroll 正确跳过头部行。
func TestRenderMCPPanel_ScrollOffset(t *testing.T) {
	m := newTestModel()
	m.mcpServers = []mcppkg.ServerStatus{
		{
			Name:     "ctx",
			Status:   mcppkg.StatusConnected,
			ToolsLen: 1,
			ToolDetails: []mcppkg.ToolDetail{
				{AdapterName: "mcp__ctx__tool", Description: "some desc"},
			},
		},
	}

	// scroll=0：第一行应含 server 名称
	m.mcpPanelScroll = 0
	out0 := m.renderMCPPanel(5)
	if !strings.Contains(out0, "ctx") {
		t.Errorf("scroll=0 时应显示 server 名称，got: %q", out0)
	}

	// scroll=1：跳过 server header，server 名称不再出现（工具行出现）
	m.mcpPanelScroll = 1
	out1 := m.renderMCPPanel(5)
	if strings.Contains(out1, "● ctx") {
		t.Errorf("scroll=1 时 server header 应被跳过，got: %q", out1)
	}
}

// TestMCPPanelOverhead_LayoutConsistency 验证 mcpPanelOverhead 常量与实际布局吻合。
// 模拟 View() 拼接：panel + "\n" + statusBar，总行数应等于 contentH + mcpPanelOverhead。
func TestMCPPanelOverhead_LayoutConsistency(t *testing.T) {
	m := newTestModel()
	m.mcpServers = []mcppkg.ServerStatus{
		{Name: "s", Status: mcppkg.StatusConnected, ToolsLen: 1,
			ToolDetails: []mcppkg.ToolDetail{{AdapterName: "mcp__s__t", Description: "d"}}},
	}
	m.height = 20
	contentH := m.height - mcpPanelOverhead

	panel := m.renderMCPPanel(contentH)
	// 模拟 View() 追加 "\n" + renderStatusBar()（单行，无尾随 \n）
	full := panel + "\n" + m.renderStatusBar()
	totalRows := panelRowCount(full)
	if totalRows != m.height {
		t.Errorf("面板总行数 = %d，期望 m.height = %d（mcpPanelOverhead=%d）",
			totalRows, m.height, mcpPanelOverhead)
	}
}

// TestEventTerminated_ShowsReasonAndResetsRunning 验证受控终止事件的现场清理与文案。
func TestEventTerminated_ShowsReasonAndResetsRunning(t *testing.T) {
	m := newTestModel()
	m.running = true
	m = applyUpdate(m, eventMsg{Type: engine.EventTerminated, Data: engine.TerminationData{
		Reason:  engine.ReasonTokenBudget,
		Message: "已达到 Token 预算 (350 tokens)",
	}})
	if m.running {
		t.Error("running should be false after EventTerminated")
	}
	if len(m.lines) == 0 || !strings.Contains(m.lines[len(m.lines)-1], "token_budget") {
		t.Errorf("应渲染携带原因码的终止文案，got %+v", m.lines)
	}
}

// TestEventStateChange_ConsumedWithoutError 验证状态事件被消费（不阻塞）且不动 running 态。
func TestEventStateChange_ConsumedWithoutError(t *testing.T) {
	m := newTestModel()
	m.running = true
	before := m.verbIdx
	m = applyUpdate(m, eventMsg{Type: engine.EventStateChange, Data: engine.StateChangeData{
		From: engine.StateTurnStart, To: engine.StateCompacting, Turn: 1,
	}})
	if !m.running {
		t.Error("EventStateChange 不应改变 running 态")
	}
	if m.verbIdx == before && len(spinnerVerbs) > 1 {
		t.Error("spinner 动词应前进")
	}
}
