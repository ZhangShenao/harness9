// TUI Update 层：处理所有 Bubbletea 消息，驱动 tuiModel 状态机。
//
// Update 层是 Bubbletea Elm 架构的逻辑侧，所有状态变更均在此层完成。
// 主要消息类型及处理分支：
//
//   - tea.KeyMsg         — 键盘输入（Ctrl+C/D 退出、Enter 提交、Tab 补全、ShiftTab 模式切换等）
//   - eventMsg           — 引擎 Event（handleEvent 分发各 EventType）
//   - shellResultMsg     — Shell 命令异步执行结果
//   - subAgentNotifyMsg  — 后台子代理完成通知
//   - subAgentDirectMsg  — @agent 前台直跑进度/完成
//   - spinner.TickMsg    — Spinner 动词轮换 tick
//   - tea.WindowSizeMsg  — 终端尺寸变化
//   - tea.MouseMsg       — 鼠标滚轮
//
// dispatch()：在此文件中实现的 RunStream 触发入口，管理 eventCh 生命周期。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/hooks"
	mcppkg "github.com/harness9/internal/mcp"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/permission"
	"github.com/harness9/internal/planning"
	"github.com/harness9/internal/sandbox"
	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/subagent"
)

// execPrompt 是用户批准 Plan Mode 计划后，首次触发执行阶段的指令文本。
//
// 设计要点：
//   - 规则 2 明确声明"仅更新状态而不调用其他工具，不算完成该项"，
//     这是 prompt 层对抗幻觉执行的约束，与工具层的批量完成检测（directCompletions > 1）形成双重防护。
//   - 只描述行为规范，不声明权限（权限由工具层 filterReadOnlyTools 硬性控制，prompt 声明是冗余的）。
const execPrompt = "按照 todo 清单逐项执行。规则：\n" +
	"1. 每开始一项前，用 todo_write 将其状态设为 in_progress\n" +
	"2. 用工具完成该项的实际工作——创建文件、写代码、运行命令等；" +
	"仅更新 todo_write 状态而不调用其他工具，不算完成该项\n" +
	"3. 确认实际产出后，用 todo_write 将其状态设为 completed\n" +
	"4. 不要输出进度摘要文字，立即处理下一项\n" +
	"全部完成后，用一句话汇报整体结果。"

// execContinuePrompt 是 autoExecuting 模式下每次 EventDone 后触发续跑的精简指令。
// 续跑场景下 LLM 已知晓基本规则（上下文中有 execPrompt 历史），此处只需提示继续处理下一项。
const execContinuePrompt = "继续处理 todo 清单中下一个 pending 或 in_progress 的任务项。" +
	"先用 todo_write 标记为 in_progress，然后用工具完成实际工作（写文件、执行命令等），" +
	"确认产出后标记为 completed，再处理下一项。" +
	"不要只更新状态而不做实际操作，不要输出进度摘要。"

const (
	// maxShellDisplayLen 是 Shell 命令输出在 TUI Scrollback 中展示的字节上限。
	// 超出部分添加截断提示行，建议用户缩小输出范围后重新执行。
	// 设为 4096 与 read_file 工具的 maxReadLen 保持一致，避免用户认知负担。
	maxShellDisplayLen = 4096

	// maxShellContextLen 是单条 Shell 命令输出存入 pendingShellOutput（并最终注入 LLM 上下文）的字节上限。
	// 截断在 shellResultMsg 处理时的存储侧执行，而非展示侧，
	// 确保大输出不会以完整形式长期驻留内存，也不会使 LLM 上下文过度膨胀。
	// 设为 2048 以留出足够上下文空间容纳多条命令记录。
	maxShellContextLen = 2048

	// maxSubAgentLines 是 subAgentLines 流式进度缓冲保留的最大行数。
	// 长时间运行的子代理会产生大量增量行，仅保留最近若干行用于实时进度展示。
	maxSubAgentLines = 12
)

// builtinCmds 是 TUI 内置的斜杠命令列表（不含 /），用于 Tab 补全和提示。
var builtinCmds = []struct {
	name string
	desc string
}{
	{"new", "开启新会话"},
	{"resume", "恢复历史会话"},
	{"plan", "进入规划模式分析任务"},
	{"compact", "手动压缩上下文"},
	{"tasks", "查看后台子代理任务"},
	{"mcp", "查看 MCP 工具列表并编辑配置"},
	{"exit", "退出 TUI"},
}

// compactDoneMsg 是 /compact 命令异步执行成功后投递的消息。
type compactDoneMsg struct {
	data memory.CompactionRecord
}

// compactErrMsg 是 /compact 命令异步执行失败后投递的消息。
type compactErrMsg struct {
	err error
}

// eventMsg 将 engine.Event 包装为 tea.Msg，供 Bubbletea 的 Update 分发。
type eventMsg engine.Event

// shellResultMsg 携带用户侧 "!" 命令异步执行的结果，由 runShellCmd 产生后经 Bubbletea 消息队列
// 分发给 Update() 的 case shellResultMsg 分支处理。
//
// 字段说明：
//   - cmd：原始命令字符串（不含前缀 "!"），用于在 dispatch() 构建 LLM 上下文记录
//   - output：stdout + stderr 合并输出（exec.CombinedOutput），未截断的原始内容
//   - isErr：命令退出码非零时为 true（包括超时被终止的情况）
//   - dur：从 runShellCmd 启动到 CombinedOutput 返回的实际耗时，精确到毫秒
type shellResultMsg struct {
	cmd    string
	output string
	isErr  bool
	dur    time.Duration
}

// subAgentNotifyMsg 由 TaskTracker 完成通知回调经 tea.Program.Send 投递，
// 触发一条即时的后台子代理完成提示（结果由 DrainCompleted 幂等取走并在下次 dispatch 注入 LLM）。
type subAgentNotifyMsg struct{}

// subAgentDirectMsg 是 @agent 前台直跑期间的进度/完成消息。
type subAgentDirectMsg struct {
	update *schema.SubAgentUpdate // 非 nil = 进度增量
	done   bool
	result string
	err    error
}

// mcpEditorDoneMsg 在用户关闭 MCP 配置编辑器后投递（tea.ExecProcess 回调）。
type mcpEditorDoneMsg struct{ err error }

// openMCPConfigCmd 暂停 TUI 并打开 .mcp.json 配置文件供用户手动编辑。
// 优先使用 $EDITOR（终端编辑器，TUI 挂起等待退出），否则使用系统关联程序打开（后台）。
func openMCPConfigCmd(path string) tea.Cmd {
	editor := os.Getenv("EDITOR")
	if editor != "" {
		return tea.ExecProcess(exec.Command(editor, path), func(err error) tea.Msg {
			return mcpEditorDoneMsg{err: err}
		})
	}
	openCmd := "open"
	if runtime.GOOS == "linux" {
		openCmd = "xdg-open"
	}
	return tea.ExecProcess(exec.Command(openCmd, path), func(err error) tea.Msg {
		return mcpEditorDoneMsg{err: err}
	})
}

// mcpUpdateMsg 在 MCP Manager 状态变更时由 waitMCPUpdate 发送。
type mcpUpdateMsg struct {
	statuses []mcppkg.ServerStatus
}

// waitMCPUpdate 返回一个 tea.Cmd，阻塞等待 mcpCh 发来更新后触发 mcpUpdateMsg。
func waitMCPUpdate(ch <-chan []mcppkg.ServerStatus) tea.Cmd {
	return func() tea.Msg {
		statuses, ok := <-ch
		if !ok {
			return nil
		}
		return mcpUpdateMsg{statuses: statuses}
	}
}

// sandboxUpdateMsg 在 Manager 状态变更时由 waitSandboxUpdate 发送。
type sandboxUpdateMsg struct {
	infos []sandbox.SandboxInfo
}

// waitSandboxUpdate 返回一个 tea.Cmd，阻塞等待 sandboxCh 发来更新后触发 sandboxUpdateMsg。
func waitSandboxUpdate(ch <-chan []sandbox.SandboxInfo) tea.Cmd {
	return func() tea.Msg {
		infos, ok := <-ch
		if !ok {
			return nil
		}
		return sandboxUpdateMsg{infos: infos}
	}
}

// readNextSubAgentDirect 读取一条直跑消息并投递给 Update；ch 关闭时投递终止 done。
func readNextSubAgentDirect(ch <-chan subAgentDirectMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return subAgentDirectMsg{done: true}
		}
		return msg
	}
}

// readNextEvent 返回一个 tea.Cmd，该 Cmd 阻塞直到 ch 中有一个 Event，
// 然后以 eventMsg 形式递交给 Update。ch 关闭时递交 EventDone。
func readNextEvent(ch <-chan engine.Event) tea.Cmd {
	return func() tea.Msg {
		evt, ok := <-ch
		if !ok {
			return eventMsg{Type: engine.EventDone}
		}
		return eventMsg(evt)
	}
}

// Update 实现 tea.Model——处理所有消息。
func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.MouseMsg:
		// 鼠标滚轮滚动（需 tea.WithMouseCellMotion() 启用）
		if msg.Action == tea.MouseActionPress {
			switch msg.Button {
			case tea.MouseButtonWheelUp:
				if m.mcpPanelMode {
					return m.handleMCPPanelKey(tea.KeyMsg{Type: tea.KeyUp})
				}
				m = m.scrollBy(-3)
			case tea.MouseButtonWheelDown:
				if m.mcpPanelMode {
					return m.handleMCPPanelKey(tea.KeyMsg{Type: tea.KeyDown})
				}
				m = m.scrollBy(3)
			}
		}
		return m, nil

	case tea.KeyMsg:
		// 审批对话框（approvalPending）激活时：优先由 handleApprovalKey 处理所有键盘输入。
		// approvalPending 在收到 EventApprovalRequired 后设为 true，此时工具 goroutine 阻塞等待响应。
		if m.approvalPending {
			return m.handleApprovalKey(msg)
		}
		// 审查对话框（planReviewing）激活时：↑↓ 移动光标，Enter 确认，Esc 取消。
		// planReviewing 在 Plan Mode 的 EventDone 中设为 true，View() 此时渲染审查对话框而非输入框。
		if m.planReviewing {
			switch msg.Type {
			case tea.KeyUp:
				if m.planReviewCursor > 0 {
					m.planReviewCursor--
				}
				return m, nil
			case tea.KeyDown:
				if m.planReviewCursor < 3 {
					m.planReviewCursor++
				}
				return m, nil
			case tea.KeyEnter:
				return m.confirmPlanReview(m.planReviewCursor)
			case tea.KeyEsc:
				// Esc 选择"取消"（第四个选项，cursor=3），放弃计划并恢复输入。
				return m.confirmPlanReview(3)
			}
			// 其他按键忽略，防止误触。
			return m, nil
		}
		// 任务面板（taskPanelMode）激活时：列表/详情态的全部按键交由 handleTaskPanelKey 处理，
		// 屏蔽普通输入。面板为模态视图，由 View() 替换输入区渲染。
		if m.taskPanelMode {
			return m.handleTaskPanelKey(msg)
		}
		// MCP 工具面板（mcpPanelMode）激活时：全部按键交由 handleMCPPanelKey 处理。
		if m.mcpPanelMode {
			return m.handleMCPPanelKey(msg)
		}
		switch msg.Type {
		case tea.KeyEsc:
			// Shell 模式下按 Esc：清空输入并退出 Shell 模式，恢复普通状态
			if m.shellMode && !m.running {
				m.shellMode = false
				m.input.SetValue("")
				m.input.Placeholder = "输入任务..."
				return m, textinput.Blink
			}
		case tea.KeyCtrlC, tea.KeyCtrlD:
			if m.running {
				m.autoExecuting = false
				m.cancelFn()
				return m, nil
			}
			return m, tea.Quit
		case tea.KeyCtrlT:
			// 切换后台任务面板。仅在空闲态可用，避免与运行中/审批/审查/恢复选择等模态冲突。
			if !m.running && !m.approvalPending && !m.planReviewing && !m.resumeSelecting {
				m.taskPanelMode = !m.taskPanelMode
				m.taskDetailID = ""
				m.taskPanelCursor = 0
			}
			return m, nil
		case tea.KeyPgUp, tea.KeyCtrlUp:
			scrollH := m.scrollHeight()
			m = m.scrollBy(-(scrollH / 2))
			return m, nil
		case tea.KeyPgDown, tea.KeyCtrlDown:
			scrollH := m.scrollHeight()
			m = m.scrollBy(scrollH / 2)
			return m, nil
		case tea.KeyEnd:
			m.viewTop = -1
			return m, nil
		case tea.KeyTab:
			if !m.running {
				m = m.cycleCompletion()
				m.completionHint = m.buildCompletionHint()
			}
			return m, nil
		case tea.KeyShiftTab:
			if !m.running {
				m.planMode = m.planMode.Next()
				if m.eng != nil {
					m.eng.SetPlanMode(m.planMode)
				}
			}
			return m, nil
		case tea.KeyEnter:
			if m.running {
				return m, nil
			}
			raw := strings.TrimSpace(m.input.Value())
			if m.resumeSelecting {
				return m.handleResumeSelection(raw)
			}
			if raw == "" {
				return m, nil
			}
			m.phase = phaseChat
			m.input.Reset()
			m.shellMode = false
			m.input.Placeholder = "输入任务..."
			// 清除补全状态
			m.typedPrefix = ""
			m.completions = nil
			m.completionHint = ""

			// /exit 静默退出，不追加用户消息行
			if raw == "/exit" {
				return m, tea.Quit
			}

			// /tasks 打开后台任务面板（模态），不作为用户消息回显。
			if raw == "/tasks" {
				m.taskPanelMode = true
				m.taskDetailID = ""
				m.taskPanelCursor = 0
				return m, nil
			}

			// /compact 手动压缩上下文，不走 LLM 推理循环。
			// 在 running 期间忽略（避免与正在进行的 runLoop 竞争 session 状态）。
			if raw == "/compact" {
				if m.running {
					m.lines = append(m.lines, dimStyle.Render("  ⚠ /compact 不可在推理期间执行，请等待本次任务完成"))
					m.input.Focus()
					return m, textinput.Blink
				}
				m.lines = append(m.lines, dimStyle.Render("  ⟳ 正在压缩上下文…"))
				m.compacting = true
				eng := m.eng
				return m, tea.Batch(
					tea.Cmd(m.spinner.Tick),
					func() tea.Msg {
						data, err := eng.Compact(context.Background())
						if err != nil {
							return compactErrMsg{err: err}
						}
						return compactDoneMsg{data: data}
					},
				)
			}

			// /mcp 打开 MCP 工具面板（模态），展示服务器状态 + 工具列表，支持编辑配置。
			if raw == "/mcp" {
				m.mcpPanelMode = true
				m.mcpPanelScroll = 0
				return m, nil
			}

			// Shell 模式: "!" 前缀直接执行 Bash 命令，绕过 LLM
			// 不显示 "▶ You:" 行，改为 "$ cmd" 风格
			if strings.HasPrefix(raw, "!") {
				shellCmd := strings.TrimSpace(strings.TrimPrefix(raw, "!"))
				return m.dispatchShellCommand(shellCmd)
			}

			// @<name> 前台直跑子代理：绕过主 LLM，dispatchMention 自行管理用户回显。
			if strings.HasPrefix(raw, "@") {
				return m.dispatchMention(raw)
			}

			// 显示用户消息
			m.lines = append(m.lines, userMsgStyle.Render("▶ You: ")+raw)
			// 新一轮用户消息开始：清空上一轮残留的子代理流式进度行，避免陈旧内容滞留。
			// 仅在 LLM 路径（含 /new、/resume、/plan、普通 prompt）重置；autoExecuting 续跑走 dispatch
			// 不经过此处，因此续跑期间子代理进度可跨 EventDone 保留。
			m.subAgentLines = nil
			m.subAgentStreaming = false

			// 处理其他内置命令
			if raw == "/new" {
				return m.handleNewSession()
			}
			if raw == "/resume" {
				return m.handleResumeList()
			}

			// /plan <task> — 进入 Plan Mode 并发送任务
			// 仅 "/plan"（无任务描述）时：激活 Plan Mode 并提示用户输入任务，不发送请求。
			if raw == "/plan" || strings.HasPrefix(raw, "/plan ") {
				task := strings.TrimSpace(strings.TrimPrefix(raw, "/plan"))
				m.planMode = planning.PlanModePlan
				if m.eng != nil {
					m.eng.SetPlanMode(planning.PlanModePlan)
				}
				if task == "" {
					m.lines = append(m.lines, dimStyle.Render("  [PLAN] 已进入规划模式 — 请输入要规划的任务"))
					m.input.Placeholder = "描述要规划的任务..."
					m.input.Reset()
					m.input.Focus()
					return m, textinput.Blink
				}
				raw = task
			}

			// 处理斜杠命令 / 普通输入
			prompt, ok := resolvePrompt(raw, m.skillsIndex)
			if !ok {
				name := strings.TrimPrefix(strings.SplitN(raw, " ", 2)[0], "/")
				m.lines = append(m.lines, errorStyle.Render("  ✗ 技能未找到: "+name))
				m.input.Focus()
				return m, textinput.Blink
			}
			if strings.HasPrefix(raw, "/") && m.skillsIndex != nil {
				name := strings.TrimPrefix(strings.SplitN(raw, " ", 2)[0], "/")
				m.lines = append(m.lines, skillStyle.Render("  ◎ 技能已加载: "+name))
			}

			return m.dispatch(prompt)
		}

	case eventMsg:
		return m.handleEvent(engine.Event(msg))

	case spinner.TickMsg:
		if (m.running && m.currentTool != "") || m.compacting {
			m.tickCount++
			if m.tickCount%30 == 0 {
				m.verbIdx = (m.verbIdx + 1) % len(spinnerVerbs)
			}
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
		return m, nil

	case compactDoneMsg:
		m.compacting = false
		data := msg.data
		switch {
		case data.MsgsBefore == 0:
			// 全零值：无 compactor 或无 session
			m.lines = append(m.lines, dimStyle.Render("  ✦ /compact: 无可压缩内容（无 session 或无 compactor）"))
		case data.MsgsBefore == data.MsgsAfter && data.TokensBefore == data.TokensAfter:
			// 压缩前后完全相同：消息数量不足（session 中至少需要 2 条消息才能产生可摘要的 head）
			m.lines = append(m.lines, dimStyle.Render("  ✦ /compact: 消息数量不足，无法压缩（至少需要 2 条消息）"))
		default:
			line := dimStyle.Render(fmt.Sprintf(
				"  ✦ Context compacted: %d → %d messages, %s → %s tokens",
				data.MsgsBefore, data.MsgsAfter,
				memory.FormatTokenCount(data.TokensBefore),
				memory.FormatTokenCount(data.TokensAfter),
			))
			m.lines = append(m.lines, line)
		}
		m.input.Focus()
		return m, textinput.Blink

	case compactErrMsg:
		m.compacting = false
		m.lines = append(m.lines, errorStyle.Render("  ✗ /compact 失败: "+msg.err.Error()))
		m.input.Focus()
		return m, textinput.Blink

	case shellResultMsg:
		// 展示侧（Scrollback）：UTF-8 安全截断至 maxShellDisplayLen，超出时追加截断提示。
		// 逐行渲染（splitLines），空输出不追加任何行。
		displayOutput := msg.output
		if len(displayOutput) > maxShellDisplayLen {
			displayOutput = truncateUTF8(displayOutput, maxShellDisplayLen) +
				"\n...[输出过长，已截断，建议用 head -n N 重新执行]..."
		}
		for _, line := range splitLines(displayOutput) {
			m.lines = append(m.lines, shellOutputStyle.Render(line))
		}
		// 结束状态行：展示退出码结果和实际耗时
		if msg.isErr {
			m.lines = append(m.lines, shellErrStyle.Render(fmt.Sprintf("  ✗ 非零退出 — %s", msg.dur)))
		} else {
			m.lines = append(m.lines, shellOKStyle.Render(fmt.Sprintf("  ✓ 完成 — %s", msg.dur)))
		}
		// 存储侧（LLM 上下文）：截断至 maxShellContextLen，防止大输出长期驻留内存。
		// dispatch() 会将所有 pendingShellOutput 记录拼接后前置注入下次 LLM 请求的 prompt。
		storedOutput := truncateUTF8(msg.output, maxShellContextLen)
		m.pendingShellOutput = append(m.pendingShellOutput,
			fmt.Sprintf("$ %s\n%s", msg.cmd, storedOutput))
		m.input.Focus()
		return m, textinput.Blink

	case subAgentNotifyMsg:
		// 后台子代理完成：即时将结果显示到对话区（用户立即可见），并缓存待下次注入 LLM。
		m = m.harvestSubAgentResults()
		return m, nil

	case sandboxUpdateMsg:
		m.sandboxes = msg.infos
		// 继续等待下次更新（持续监听 channel）；nil 守卫防止 nil channel 永久阻塞。
		if m.sandboxCh != nil {
			return m, waitSandboxUpdate(m.sandboxCh)
		}
		return m, nil

	case mcpUpdateMsg:
		m.mcpServers = msg.statuses
		if m.mcpCh != nil {
			return m, waitMCPUpdate(m.mcpCh)
		}
		return m, nil

	case mcpEditorDoneMsg:
		// 编辑器退出后恢复 TUI；配置修改需重启 harness9 生效。
		return m, nil

	case subAgentDirectMsg:
		if msg.update != nil {
			m = m.appendSubAgentUpdate(*msg.update)
			return m, readNextSubAgentDirect(m.directCh)
		}
		// 完成：前台直跑结束，将最终结果直接展示到对话区。
		m.running = false
		if m.cancelFn != nil {
			m.cancelFn()
		}
		if msg.err != nil {
			m.lines = append(m.lines, errorStyle.Render("  ✗ 子代理失败: "+msg.err.Error()))
		} else {
			for _, ln := range strings.Split(strings.TrimRight(msg.result, "\n"), "\n") {
				m.lines = append(m.lines, ln)
			}
		}
		m.subAgentLines = nil
		m.subAgentStreaming = false
		m.directCh = nil
		m.input.Focus()
		return m, textinput.Blink
	}

	if !m.running {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		// 非 Tab 的按键重置补全循环，重新计算提示
		if _, isKey := msg.(tea.KeyMsg); isKey {
			m.typedPrefix = ""
			m.completions = nil
		}
		// 检测 Shell 模式：输入以 "!" 开头时切换视觉状态
		val := m.input.Value()
		nowShell := strings.HasPrefix(val, "!")
		if nowShell != m.shellMode {
			m.shellMode = nowShell
			if m.shellMode {
				m.input.Placeholder = `Shell 命令... "git status"`
			} else {
				m.input.Placeholder = "输入任务..."
			}
		}
		m.completionHint = m.buildCompletionHint()
		return m, cmd
	}
	return m, nil
}

// handleEvent 处理单个 engine.Event，返回更新后的模型和下一个 tea.Cmd。
func (m tuiModel) handleEvent(evt engine.Event) (tea.Model, tea.Cmd) {
	switch evt.Type {
	case engine.EventThinkingDelta:
		delta, _ := evt.Data.(string)
		if m.thinkingLineStart == -1 {
			// 首个 thinking delta：追加标题行并记录起始位置。
			// 若 pendingReplyStart 指向末尾的空行占位符（由 dispatch 插入），
			// 先将其移除，避免 thinking 块头部出现无意义的空行。
			if m.pendingReplyStart < len(m.lines) && m.lines[m.pendingReplyStart] == "" {
				m.lines = m.lines[:m.pendingReplyStart]
			}
			m.lines = append(m.lines, thinkingHeaderStyle.Render("« thinking »"))
			m.thinkingLineStart = len(m.lines) - 1
		}
		m.pendingThinking += delta
		// 将 thinking 内容行覆写到 lines[thinkingLineStart+1:]
		thinkingLines := renderThinkingLines(m.pendingThinking, m.width)
		m.lines = append(m.lines[:m.thinkingLineStart+1], thinkingLines...)
		return m, readNextEvent(m.eventCh)

	case engine.EventApprovalRequired:
		req, ok := evt.Data.(engine.ApprovalRequest)
		if !ok {
			return m, readNextEvent(m.eventCh)
		}
		m.approvalPending = true
		m.approvalRequest = &req
		m.approvalCursor = 0
		m.approvalFeedback = ""
		m.approvalInputting = false
		// 不恢复 readNextEvent：工具 goroutine 阻塞等待 ResponseCh
		return m, nil

	case engine.EventActionDelta:
		delta, _ := evt.Data.(string)
		// 如果 thinking 块尚未结束，先 flush
		if m.pendingThinking != "" {
			m = m.flushPendingThinking()
		}
		m.pendingReply += delta
		// 原始文本回写到 lines，等工具边界时用 glamour 统一渲染
		rawLines := strings.Split(m.pendingReply, "\n")
		m.lines = append(m.lines[:m.pendingReplyStart], rawLines...)
		return m, readNextEvent(m.eventCh)

	case engine.EventToolStart:
		// thinking 块 flush 必须在 flushPendingReply 之前，避免行索引错乱
		if m.pendingThinking != "" {
			m = m.flushPendingThinking()
		}
		// 工具启动前先渲染当前累积的文本块
		m = m.flushPendingReply()
		tc, _ := evt.Data.(schema.ToolCall)
		// 按 ID 存入 pendingTools，防止并发工具互相覆盖
		m.pendingTools[tc.ID] = pendingToolInfo{name: tc.Name, args: tc.Arguments}
		// 同时更新 currentTool/toolArgs/toolStart 供 spinner 展示（始终展示最近启动的工具）
		m.currentTool = tc.Name
		m.toolStart = time.Now()
		m.toolArgs = tc.Arguments
		return m, tea.Batch(readNextEvent(m.eventCh), tea.Cmd(m.spinner.Tick))

	case engine.EventToolResult:
		data, _ := evt.Data.(engine.ToolResultData)
		result := data.Result
		// 引擎侧在 toolDone 回调中精确计算耗时，直接使用，不受 channel 传输延迟影响
		elapsed := data.Duration.Round(time.Millisecond)

		// 从 pendingTools 按 ToolCallID 精准取回名称和参数，避免并发覆盖导致名称丢失
		var toolName string
		var toolArgs json.RawMessage
		if info, ok := m.pendingTools[result.ToolCallID]; ok {
			toolName = info.name
			toolArgs = info.args
			delete(m.pendingTools, result.ToolCallID)
		} else {
			// 兜底：pendingTools 查不到时退回 currentTool（单工具场景）
			toolName = m.currentTool
			toolArgs = m.toolArgs
		}

		// 工具完成行：展示 tool_name(args摘要) — 耗时
		summary := summarizeTool(toolName, toolArgs)
		display := toolName
		if summary != "" {
			display = fmt.Sprintf("%s(%s)", toolName, summary)
		}
		var line string
		if result.IsError {
			line = toolErrStyle.Render(fmt.Sprintf("  ✗ %s", display)) + dimStyle.Render(fmt.Sprintf(" — %s", elapsed))
		} else {
			line = toolOKStyle.Render(fmt.Sprintf("  ✓ %s", display)) + dimStyle.Render(fmt.Sprintf(" — %s", elapsed))
		}
		m.lines = append(m.lines, line)

		// todo_write 完成后，在工具行下方追加最新 todo 快照
		if toolName == "todo_write" && !result.IsError && m.todoStore != nil {
			m = m.updateTodoBlock()
		}

		m.pendingReplyStart = len(m.lines)
		// 当所有并发工具均已完成时才清空 spinner 状态
		if len(m.pendingTools) == 0 {
			m.currentTool = ""
			m.toolArgs = nil
		}
		return m, readNextEvent(m.eventCh)

	case engine.EventDone:
		if m.pendingThinking != "" {
			m = m.flushPendingThinking()
		}
		// flushPendingReply 在这里调用，将流式累积的 Markdown 文本渲染到 lines。
		m = m.flushPendingReply()
		if m.cancelFn != nil {
			// 释放本次 runLoop 使用的 context，避免 goroutine 泄漏。
			m.cancelFn()
		}
		m.running = false
		m.currentTool = ""
		m.toolArgs = nil

		// Plan Mode 完成：展示审查对话框，暂停等待用户选择（1/2/3/4）。
		// planReviewing = true 后 View() 只渲染对话框，屏蔽普通输入。
		if m.planMode == planning.PlanModePlan {
			m.planReviewing = true
			return m, nil
		}

		// autoExecuting 续跑逻辑：若有未完成 todo，基于进度决定是否自动续跑。
		// 停滞检测：连续 3 次 EventDone 后已完成数（done）无增加，判定为空转，放弃自动执行。
		// 使用 done 而非 pending 计数判断进度：只有 completed 才代表真实工作产出，
		// pending→in_progress 只是状态标记，不代表任何实际产出。
		if m.autoExecuting && m.todoStore != nil {
			items := m.todoStore.Read()
			var pending, done int
			for _, item := range items {
				switch item.Status {
				case planning.TodoPending, planning.TodoInProgress:
					pending++
				case planning.TodoCompleted:
					done++
				}
			}
			if pending > 0 {
				if done > m.autoExecPrevDone {
					// 有进度（done 数增加）：重置停滞计数器，允许继续执行。
					m.autoExecStuck = 0
				} else {
					// 无进度：停滞计数 +1，连续 3 次后放弃。
					m.autoExecStuck++
				}
				if m.autoExecStuck < 3 {
					// 仍在容忍范围内：更新上次 done 计数，继续续跑。
					m.autoExecPrevDone = done
					return m.dispatch(execContinuePrompt)
				}
				// 连续 3 次无进度：放弃自动执行，提示用户手动干预。
				m.autoExecuting = false
				m.autoExecStuck = 0
				m.lines = append(m.lines, dimStyle.Render("  ⚠ 执行停滞，请手动描述下一步"))
			} else {
				// pending == 0：所有 todo 已完成，退出自动执行模式。
				m.autoExecuting = false
			}
		}
		// 纯工具执行（无 LLM 文字回复）时，lines 末尾是空行占位符。
		// 将其替换为完成标记，避免显示孤立的空行。
		if len(m.lines) > 0 && m.lines[len(m.lines)-1] == "" {
			m.lines[len(m.lines)-1] = doneStyle.Render("  ✅ 任务完成")
		}
		m.input.Focus()
		return m, textinput.Blink

	case engine.EventError:
		errMsg, _ := evt.Data.(string)
		// 丢弃未渲染的流式缓冲（含 thinking 块）
		if m.thinkingLineStart != -1 {
			m.lines = m.lines[:m.thinkingLineStart]
		} else {
			m.lines = m.lines[:m.pendingReplyStart]
		}
		m.pendingReply = ""
		m.pendingThinking = ""
		m.thinkingLineStart = -1
		if m.cancelFn != nil {
			m.cancelFn()
		}
		m.running = false
		m.currentTool = ""
		m.autoExecuting = false
		m.lines = append(m.lines, errorStyle.Render("❌ "+errMsg))
		m.input.Focus()
		return m, textinput.Blink

	case engine.EventTokenUpdate:
		data, _ := evt.Data.(engine.TokenUpdateData)
		m.contextTokens = data.EstimatedTokens
		if m.contextWindow == 0 && data.ContextWindow > 0 {
			m.contextWindow = data.ContextWindow
		}
		return m, readNextEvent(m.eventCh)

	case engine.EventSubAgent:
		if u, ok := evt.Data.(schema.SubAgentUpdate); ok {
			m = m.appendSubAgentUpdate(u)
		}
		return m, readNextEvent(m.eventCh)

	case engine.EventCompaction:
		data, _ := evt.Data.(memory.CompactionRecord)
		tierLabel := tierLabel(data.Tier)
		ratio := 0.0
		if data.TokensBefore > 0 {
			ratio = (1.0 - float64(data.TokensAfter)/float64(data.TokensBefore)) * 100
		}
		line1 := dimStyle.Render(fmt.Sprintf("  ⚡ 上下文压缩 [%s] %s→%s tokens（%.0f%% 压缩率）",
			tierLabel,
			memory.FormatTokenCount(data.TokensBefore),
			memory.FormatTokenCount(data.TokensAfter),
			ratio,
		))
		var line2 string
		if data.Tier == memory.TierEmergency {
			line2 = dimStyle.Render(fmt.Sprintf("     ⚠ 紧急截断回退 | 保留尾部 %d 条", data.PreservedTail))
		} else if data.Summarized > 0 {
			line2 = dimStyle.Render(fmt.Sprintf("     %d 锚点 | %d offload | %d 条摘要 | 保留尾部 %d 条",
				len(data.Anchors), len(data.Offloaded), data.Summarized, data.PreservedTail))
		} else {
			line2 = dimStyle.Render(fmt.Sprintf("     %d offload | 无摘要", len(data.Offloaded)))
		}
		m.lines = append(m.lines, line1, line2)
		return m, readNextEvent(m.eventCh)
	}

	return m, readNextEvent(m.eventCh)
}

// appendSubAgentUpdate 把一条子代理进度更新合并/追加到 subAgentLines（delta 合并、tool_start 带参数、错误提示）。
// 由 EventSubAgent（主引擎转发的后台/Task 工具进度）与 subAgentDirectMsg（@agent 前台直跑进度）共享，
// 保证两条路径的渲染语义完全一致。
func (m tuiModel) appendSubAgentUpdate(u schema.SubAgentUpdate) tuiModel {
	switch u.Kind {
	case schema.SubAgentDelta:
		// 文本增量：合并到当前正在累积的正文行，而非每个 token 新建一行（修复刷屏）。
		// 增量内的换行折叠为空格保持单行；末行过长（>160 runes）时另起一行软换行。
		s := strings.ReplaceAll(u.Text, "\n", " ")
		if s == "" {
			return m
		}
		last := len(m.subAgentLines) - 1
		if m.subAgentStreaming && last >= 0 && utf8.RuneCountInString(m.subAgentLines[last]) < 160 {
			m.subAgentLines[last] += s
		} else {
			m.subAgentLines = append(m.subAgentLines, fmt.Sprintf("[%s] %s", u.AgentName, s))
			m.subAgentStreaming = true
		}
	case schema.SubAgentStart:
		m.subAgentLines = append(m.subAgentLines, fmt.Sprintf("[%s] 子代理启动…", u.AgentName))
		m.subAgentStreaming = false
	case schema.SubAgentToolStart:
		line := fmt.Sprintf("[%s] ▸ %s", u.AgentName, u.ToolName)
		if args := strings.TrimSpace(u.Text); args != "" && args != "{}" {
			line += "(" + truncateUTF8(args, 80) + ")"
		}
		m.subAgentLines = append(m.subAgentLines, line)
		m.subAgentStreaming = false
	case schema.SubAgentToolResult:
		// 成功的工具结果不单独成行：上面的 `▸ 工具名(参数)` 已展示该调用，
		// 再加一行 ✓ 既无信息量，又会在 maxSubAgentLines 上限下挤掉有用的调用行（"空 tool-calling"）。
		// 仅在出错时提示，便于用户察觉子代理的工具失败。
		if u.IsError {
			m.subAgentLines = append(m.subAgentLines, fmt.Sprintf("[%s]   ✗ 工具执行失败", u.AgentName))
			m.subAgentStreaming = false
		}
	case schema.SubAgentDone:
		m.subAgentLines = append(m.subAgentLines, fmt.Sprintf("[%s] ✓ 完成", u.AgentName))
		m.subAgentStreaming = false
	case schema.SubAgentError:
		m.subAgentLines = append(m.subAgentLines, fmt.Sprintf("[%s] ✗ %s", u.AgentName, u.Text))
		m.subAgentStreaming = false
		// SubAgentThinking 故意不展示：子代理推理增量噪声大，仅 delta/工具/结果对用户有意义。
	}
	// 限制缓冲行数，避免长时间运行的子代理无界增长（仅保留最近 maxSubAgentLines 行）。
	if len(m.subAgentLines) > maxSubAgentLines {
		m.subAgentLines = m.subAgentLines[len(m.subAgentLines)-maxSubAgentLines:]
	}
	return m
}

// scrollHeight 返回对话区域可显示的行数。
// 运行中且有活跃工具时额外保留 1 行给 ToolProgress。
func (m tuiModel) scrollHeight() int {
	reserved := 3 // StatusBar + PromptInput + Footer
	if m.running && m.currentTool != "" {
		reserved++ // + ToolProgress
	}
	if m.compacting {
		reserved++ // + CompactProgress
	}
	if n := len(m.subAgentLines); n > 0 {
		reserved += n // + 子代理进度块（每行占一行）
	}
	if len(m.sandboxes) > 0 {
		reserved++ // + SandboxBar
	}
	h := m.height - reserved
	if h < 1 {
		h = 1
	}
	return h
}

// scrollBy 将视口向上（delta<0）或向下（delta>0）移动 delta 行。
// viewTop=-1 表示自动跟随底部；到达底部时自动切回 -1。
func (m tuiModel) scrollBy(delta int) tuiModel {
	scrollH := m.scrollHeight()
	total := len(m.lines)
	if total <= scrollH {
		return m // 内容不足一屏，无需滚动
	}
	if m.viewTop < 0 {
		// 从自动模式进入手动模式：以当前底部位置为起点
		m.viewTop = total - scrollH
	}
	m.viewTop += delta
	if m.viewTop <= 0 {
		m.viewTop = 0
	}
	if m.viewTop >= total-scrollH {
		m.viewTop = -1 // 回到底部自动模式
	}
	return m
}

// cycleCompletion 处理 Tab 键：首次进入补全模式，或在匹配列表中循环切换。
// 补全顺序：内置命令优先，其次 Skills。
func (m tuiModel) cycleCompletion() tuiModel {
	raw := m.input.Value()
	// @<name> 补全：匹配已注册的子代理名（供 @agent 前台直跑使用）。
	// 置于 "/" 守卫之前，复用同一套 typedPrefix/completions 循环状态。
	if strings.HasPrefix(raw, "@") && !m.resumeSelecting {
		prefix := strings.TrimPrefix(strings.SplitN(raw, " ", 2)[0], "@")
		if m.typedPrefix == "" {
			var matches []string
			if m.subAgentReg != nil {
				for _, d := range m.subAgentReg.List() {
					if strings.HasPrefix(d.Name, prefix) {
						matches = append(matches, d.Name)
					}
				}
			}
			if len(matches) == 0 {
				return m
			}
			m.typedPrefix = prefix
			m.completions = matches
			m.completionIdx = 0
		} else if len(m.completions) > 0 {
			m.completionIdx = (m.completionIdx + 1) % len(m.completions)
		}
		if len(m.completions) > 0 {
			m.input.SetValue("@" + m.completions[m.completionIdx] + " ")
			m.input.CursorEnd()
		}
		return m
	}
	if !strings.HasPrefix(raw, "/") || m.resumeSelecting {
		return m
	}
	prefix := strings.TrimPrefix(strings.SplitN(raw, " ", 2)[0], "/")

	if m.typedPrefix == "" {
		// 首次按 Tab：以当前输入作为前缀，初始化补全列表
		var matches []string
		for _, cmd := range builtinCmds {
			if strings.HasPrefix(cmd.name, prefix) {
				matches = append(matches, cmd.name)
			}
		}
		if m.skillsIndex != nil {
			for _, n := range m.skillsIndex.Names() {
				if strings.HasPrefix(n, prefix) {
					matches = append(matches, n)
				}
			}
		}
		if len(matches) == 0 {
			return m
		}
		m.typedPrefix = prefix
		m.completions = matches
		m.completionIdx = 0
	} else if len(m.completions) > 0 {
		// 已在补全模式：循环到下一个
		m.completionIdx = (m.completionIdx + 1) % len(m.completions)
	}

	if len(m.completions) > 0 {
		m.input.SetValue("/" + m.completions[m.completionIdx])
		m.input.CursorEnd()
	}
	return m
}

// buildCompletionHint 根据当前输入生成状态栏的补全提示文字。
// 空输入、非斜杠命令、或处于 resume 选择模式时返回 ""。
// 内置命令附带描述，Skills 仅显示名称。
func (m tuiModel) buildCompletionHint() string {
	raw := m.input.Value()
	if m.resumeSelecting {
		return ""
	}
	// @ 前缀：列出匹配的子代理建议（名称 + 截断描述），与 / 命令提示风格一致。
	if strings.HasPrefix(raw, "@") {
		return m.buildMentionHint(raw)
	}
	if !strings.HasPrefix(raw, "/") {
		return ""
	}
	prefix := strings.TrimPrefix(strings.SplitN(raw, " ", 2)[0], "/")

	type entry struct {
		name string
		desc string // 内置命令有描述，Skills 为空
	}

	// 正在补全循环中：从已缓存列表重建；否则实时匹配
	var entries []entry
	if m.typedPrefix != "" && len(m.completions) > 0 {
		// 用缓存列表还原 entry（desc 需重新查找）
		descOf := make(map[string]string, len(builtinCmds))
		for _, cmd := range builtinCmds {
			descOf[cmd.name] = cmd.desc
		}
		for _, n := range m.completions {
			entries = append(entries, entry{name: n, desc: descOf[n]})
		}
	} else {
		for _, cmd := range builtinCmds {
			if strings.HasPrefix(cmd.name, prefix) {
				entries = append(entries, entry{name: cmd.name, desc: cmd.desc})
			}
		}
		if m.skillsIndex != nil {
			for _, n := range m.skillsIndex.Names() {
				if strings.HasPrefix(n, prefix) {
					entries = append(entries, entry{name: n})
				}
			}
		}
	}
	if len(entries) == 0 {
		return ""
	}

	parts := make([]string, len(entries))
	for i, e := range entries {
		selected := m.typedPrefix != "" && i == m.completionIdx
		nameRendered := skillStyle.Render("/" + e.name)
		if !selected {
			nameRendered = dimStyle.Render("/" + e.name)
		}
		if e.desc != "" {
			parts[i] = nameRendered + " " + dimStyle.Render("("+e.desc+")")
		} else {
			parts[i] = nameRendered
		}
	}
	return "  ↹  " + strings.Join(parts, "   ")
}

// buildMentionHint 为 @ 前缀生成子代理建议提示：列出名称匹配的子代理及其截断描述。
// 与 buildCompletionHint 的 / 分支同构：Tab 循环中用缓存列表（高亮选中项），否则按前缀实时匹配。
func (m tuiModel) buildMentionHint(raw string) string {
	if m.subAgentReg == nil {
		return ""
	}
	prefix := strings.TrimPrefix(strings.SplitN(raw, " ", 2)[0], "@")

	type entry struct {
		name string
		desc string
	}
	var entries []entry
	if m.typedPrefix != "" && len(m.completions) > 0 {
		descOf := make(map[string]string)
		for _, d := range m.subAgentReg.List() {
			descOf[d.Name] = d.Description
		}
		for _, n := range m.completions {
			entries = append(entries, entry{name: n, desc: descOf[n]})
		}
	} else {
		for _, d := range m.subAgentReg.List() {
			if strings.HasPrefix(d.Name, prefix) {
				entries = append(entries, entry{name: d.Name, desc: d.Description})
			}
		}
	}
	if len(entries) == 0 {
		return ""
	}

	parts := make([]string, len(entries))
	for i, e := range entries {
		selected := m.typedPrefix != "" && i == m.completionIdx
		nameRendered := skillStyle.Render("@" + e.name)
		if !selected {
			nameRendered = dimStyle.Render("@" + e.name)
		}
		parts[i] = nameRendered + " " + dimStyle.Render("("+truncateUTF8(e.desc, 24)+")")
	}
	return "  ↹  " + strings.Join(parts, "   ")
}

// flushPendingReply 将 pendingReply 用 glamour 渲染并替换 lines 中的原始文本。
func (m tuiModel) flushPendingReply() tuiModel {
	if m.pendingReply == "" {
		return m
	}
	if m.pendingReplyStart > len(m.lines) {
		m.pendingReplyStart = len(m.lines)
	}
	rendered := renderMD(m.pendingReply, m.width)
	lines := splitLines(rendered)
	m.lines = append(m.lines[:m.pendingReplyStart], lines...)
	m.pendingReply = ""
	m.pendingReplyStart = len(m.lines)
	return m
}

// renderThinkingLines 将推理文本按段落分割并在 width 内 word-wrap，每行加 "  │ " 前缀并应用暗色样式。
// width 为终端列数（m.width）；0 时回退为不折行。
func renderThinkingLines(text string, width int) []string {
	const prefix = "  │ "
	const prefixCols = 4 // "  │ " 占用的显示列数

	wrapWidth := width - prefixCols - 1 // 右侧留 1 列边距
	if wrapWidth < 20 {
		wrapWidth = 0 // 终端过窄时不折行
	}

	var out []string
	for _, para := range strings.Split(text, "\n") {
		for _, line := range thinkingWordWrap(para, wrapWidth) {
			out = append(out, thinkingLineStyle.Render(prefix+line))
		}
	}
	if len(out) == 0 {
		out = []string{thinkingLineStyle.Render(prefix)}
	}
	return out
}

// thinkingWordWrap 将 text 按 width rune 数折行，保留词边界。
// 超过 width 的单个词（如 URL）会被强制截断，避免溢出终端宽度。
// width <= 0 时不折行，整段作为单行返回。
func thinkingWordWrap(text string, width int) []string {
	if width <= 0 || text == "" {
		return []string{text}
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}

	// hardBreak 将超长词按 width 强制截断为多段。
	hardBreak := func(word string) []string {
		runes := []rune(word)
		var chunks []string
		for len(runes) > width {
			chunks = append(chunks, string(runes[:width]))
			runes = runes[width:]
		}
		return append(chunks, string(runes))
	}

	var lines []string
	line := words[0]
	for _, word := range words[1:] {
		if len([]rune(line))+1+len([]rune(word)) <= width {
			line += " " + word
		} else {
			// 当前行满，先将超长单词硬截断再追加。
			if len([]rune(word)) > width {
				lines = append(lines, line)
				chunks := hardBreak(word)
				lines = append(lines, chunks[:len(chunks)-1]...)
				line = chunks[len(chunks)-1]
			} else {
				lines = append(lines, line)
				line = word
			}
		}
	}
	// 最后一行也需检查是否超长。
	if len([]rune(line)) > width {
		chunks := hardBreak(line)
		lines = append(lines, chunks[:len(chunks)-1]...)
		line = chunks[len(chunks)-1]
	}
	return append(lines, line)
}

// flushPendingThinking 追加 thinking 块结束行，并重置 thinking 缓冲和行索引。
// 调用后 pendingReplyStart 更新为当前 lines 长度，后续 action 文本从此处开始。
func (m tuiModel) flushPendingThinking() tuiModel {
	if m.pendingThinking == "" {
		return m
	}
	m.lines = append(m.lines, thinkingEndStyle.Render("  └ ──────────────────────────────"))
	m.pendingThinking = ""
	m.thinkingLineStart = -1
	m.pendingReplyStart = len(m.lines)
	return m
}

// renderMD 通过 glamour 将 Markdown 文本渲染为终端 ANSI 格式。
// 降级策略：任何错误均原样返回原文。
//
// 故意不使用 glamour.WithAutoStyle()：该选项会发送 OSC 11 终端颜色查询，
// 终端将响应写回 stdin，Bubbletea 的 textinput 会将其误判为用户输入，
// 导致输入框出现乱码（如 ]11;rgb:.../[35;1R）。改用固定 "dark" 样式规避此问题。
func renderMD(text string, width int) string {
	if width <= 4 {
		width = 80
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(width-4),
	)
	if err != nil {
		return text
	}
	out, err := r.Render(text)
	if err != nil {
		return text
	}
	return strings.TrimRight(out, "\n")
}

// splitLines 按换行符分割，去除末尾空行后返回切片。
func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// summarizeTool 根据工具名对参数进行智能截断摘要，用于 ToolProgress 展示。
func summarizeTool(name string, args json.RawMessage) string {
	switch name {
	case "bash":
		var v struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(args, &v); err != nil || v.Command == "" {
			return ""
		}
		cmd := strings.ReplaceAll(v.Command, "\n", " ↵ ")
		if len([]rune(cmd)) > 120 {
			return string([]rune(cmd)[:120]) + "…"
		}
		return cmd
	case "read_file", "write_file", "edit_file":
		var v struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(args, &v); err != nil || v.Path == "" {
			return ""
		}
		return filepath.Base(v.Path)
	default:
		if len(args) == 0 {
			return ""
		}
		s := string(args)
		runes := []rune(s)
		if len(runes) > 80 {
			return string(runes[:80]) + "…"
		}
		return s
	}
}

// dispatch 以指定 prompt 启动一次 agent 推理流（RunStream）。
//
// 调用时 running 必须为 false；若已有推理在进行（running == true）则静默返回，
// 防止多路 goroutine 并发驱动同一个 AgentEngine。
//
// autoExecuting 续跑时，dispatch 由 EventDone handler 在 Elm Update 循环内调用，
// 不存在并发问题（Bubbletea 保证 Update 是单线程的）。
// 但 running 检查保留作为额外安全网，防止其他代码路径意外调用。

// harvestSubAgentResults 排空后台子代理跟踪器：将每个已完成结果即时显示到对话区（scrollback，
// 用户立即可见），并写入 pendingSubAgentInject 以便下次 dispatch 注入 LLM 上下文。
// 从 subAgentNotifyMsg（即时显示）与 dispatch（兜底）两处调用——DrainCompleted 幂等，已注入结果
// 后续调用不再返回，从而实现"显示一次 + 注入一次"，二者不重复消费。
func (m tuiModel) harvestSubAgentResults() tuiModel {
	if m.subAgentTracker == nil {
		return m
	}
	for _, ct := range m.subAgentTracker.DrainCompleted() {
		status := "完成"
		if ct.IsError {
			status = "失败"
		}
		// 显示到对话区，用户即时可见。
		// 注意：仅在非流式时追加——流式回复进行中（running）时，EventActionDelta 会以
		// m.lines[:pendingReplyStart] 截断重写，追加到其后的行会被抹掉；此时只入注入缓冲，
		// 结果仍会在下次 dispatch 注入 LLM，由模型回复体现。
		if !m.running {
			m.lines = append(m.lines, subAgentLineStyle.Render(fmt.Sprintf("✓ 后台子代理 %s %s：", ct.AgentName, status)))
			for _, ln := range strings.Split(strings.TrimRight(ct.FinalText, "\n"), "\n") {
				m.lines = append(m.lines, subAgentLineStyle.Render("  "+ln))
			}
		}
		// 缓存以注入下次 LLM 请求。
		m.pendingSubAgentInject = append(m.pendingSubAgentInject,
			fmt.Sprintf("[后台子代理 %s %s]\n%s", ct.AgentName, status, ct.FinalText))
	}
	return m
}

// dispatchMention 解析 @<name> <task> 并前台直跑指定子代理（绕过主 LLM）。
func (m tuiModel) dispatchMention(raw string) (tuiModel, tea.Cmd) {
	m.lines = append(m.lines, userMsgStyle.Render("▶ You: ")+raw)
	body := strings.TrimSpace(strings.TrimPrefix(raw, "@"))
	name, task, _ := strings.Cut(body, " ")
	task = strings.TrimSpace(task)
	if m.subAgentReg == nil || m.subAgentRunner == nil {
		m.lines = append(m.lines, errorStyle.Render("  ✗ 子代理未启用"))
		return m, nil
	}
	def, ok := m.subAgentReg.Get(name)
	if !ok {
		var names []string
		for _, d := range m.subAgentReg.List() {
			names = append(names, d.Name)
		}
		m.lines = append(m.lines, errorStyle.Render("  ✗ 未知子代理: "+name+"（可用: "+strings.Join(names, ", ")+"）"))
		return m, nil
	}
	if task == "" {
		m.lines = append(m.lines, errorStyle.Render("  ✗ 请在 @"+name+" 后输入任务"))
		return m, nil
	}
	m.subAgentLines = nil
	m.subAgentStreaming = false
	m.running = true
	ctx, cancel := context.WithCancel(m.outerCtx)
	m.cancelFn = cancel
	ch := make(chan subAgentDirectMsg)
	m.directCh = ch
	def2 := def
	go func() {
		defer close(ch)
		sink := func(u schema.SubAgentUpdate) {
			// u 是 sink 的入参，每次回调独享一份；取地址后随消息发出是安全的。
			select {
			case ch <- subAgentDirectMsg{update: &u}:
			case <-ctx.Done():
			}
		}
		cctx := hooks.WithSubAgentProgress(ctx, sink)
		res, err := m.subAgentRunner.Run(cctx, def2, task, false)
		select {
		case ch <- subAgentDirectMsg{done: true, result: res.FinalText, err: err}:
		case <-ctx.Done():
		}
	}()
	m.lines = append(m.lines, assistantStyle.Render("◆ "+def.Name+":"), "")
	return m, tea.Batch(readNextSubAgentDirect(m.directCh), m.spinner.Tick)
}

func (m tuiModel) dispatch(prompt string) (tuiModel, tea.Cmd) {
	if m.running {
		return m, nil
	}
	// 将缓冲的 Shell 命令输出前置注入 LLM 上下文，格式为：
	//   [用户执行的 Shell 命令记录]
	//   $ git status
	//   On branch main...
	//   ---
	//   $ go build ./...
	//   ...
	//
	//   <用户的实际 prompt>
	//
	// entry 已在 shellResultMsg 处理时截断至 maxShellContextLen，
	// 此处保留 truncateUTF8 作防御性兜底，防止未来代码路径绕过存储侧截断。
	// 注入后立即清空，避免同一批命令被下一次 dispatch 重复注入。
	if len(m.pendingShellOutput) > 0 {
		var sb strings.Builder
		sb.WriteString("[用户执行的 Shell 命令记录]\n")
		for i, entry := range m.pendingShellOutput {
			if i > 0 {
				sb.WriteString("\n---\n")
			}
			sb.WriteString(truncateUTF8(entry, maxShellContextLen))
		}
		prompt = sb.String() + "\n\n" + prompt
		m.pendingShellOutput = nil
	}
	// 兜底收获后台子代理结果（若 notify 已处理则为空转），再将待注入结果前置拼接到 prompt。
	// 结果已在完成时显示到对话区，此处仅负责把它们注入 LLM 上下文，注入后清空缓冲。
	m = m.harvestSubAgentResults()
	if len(m.pendingSubAgentInject) > 0 {
		var b strings.Builder
		b.WriteString("[以下是先前后台子代理的执行结果，供你参考]\n")
		for _, blk := range m.pendingSubAgentInject {
			b.WriteString(blk)
			b.WriteString("\n")
		}
		prompt = b.String() + "\n" + prompt
		m.pendingSubAgentInject = nil
	}
	m.lines = append(m.lines, assistantStyle.Render("◆ harness9:"), "")
	m.pendingReplyStart = len(m.lines) - 1
	m.pendingReply = ""
	m.thinkingLineStart = -1
	m.pendingThinking = ""

	ctx, cancel := context.WithCancel(m.outerCtx)
	m.cancelFn = cancel
	m.running = true
	if m.eng == nil {
		m.input.Focus()
		return m, textinput.Blink
	}
	m.input.Blur()
	ch, err := m.eng.RunStream(ctx, prompt)
	if err != nil {
		m.lines = append(m.lines, errorStyle.Render("❌ "+err.Error()))
		m.running = false
		cancel()
		m.input.Focus()
		return m, textinput.Blink
	}
	m.eventCh = ch
	return m, readNextEvent(ch)
}

// handleNewSession 创建新会话，替换引擎绑定，刷新状态栏。
func (m tuiModel) handleNewSession() (tea.Model, tea.Cmd) {
	if m.manager == nil {
		m.lines = append(m.lines, errorStyle.Render("  ✗ Memory Manager 未初始化"))
		m.input.Focus()
		return m, textinput.Blink
	}
	sess, err := m.manager.NewSession(m.outerCtx)
	if err != nil {
		m.lines = append(m.lines, errorStyle.Render("  ✗ 创建会话失败: "+err.Error()))
		m.input.Focus()
		return m, textinput.Blink
	}
	m.session = sess
	m.sessionID = sess.SessionID()
	if m.eng != nil {
		m.eng.SetSession(sess)
	}
	m.lines = append(m.lines, dimStyle.Render("  ✓ 新会话已创建: "+m.sessionID))
	m.input.Reset()
	m.input.Focus()
	return m, textinput.Blink
}

// handleResumeList 列出历史会话供用户选择。
func (m tuiModel) handleResumeList() (tea.Model, tea.Cmd) {
	if m.manager == nil {
		m.lines = append(m.lines, errorStyle.Render("  ✗ Memory Manager 未初始化"))
		m.input.Focus()
		return m, textinput.Blink
	}
	sessions, err := m.manager.ListSessions(m.outerCtx)
	if err != nil {
		m.lines = append(m.lines, errorStyle.Render("  ✗ 获取会话列表失败: "+err.Error()))
		m.input.Focus()
		return m, textinput.Blink
	}
	if len(sessions) == 0 {
		m.lines = append(m.lines, dimStyle.Render("  暂无历史会话"))
		m.input.Focus()
		return m, textinput.Blink
	}
	if len(sessions) > 10 {
		sessions = sessions[:10]
	}
	m.resumeSessions = sessions
	m.resumeSelecting = true

	m.lines = append(m.lines, dimStyle.Render(fmt.Sprintf("  可用会话（%d 条）：", len(sessions))))
	for i, s := range sessions {
		line := fmt.Sprintf("  [%d] %s  %s  %d 条消息",
			i+1,
			s.ID,
			s.UpdatedAt.Format("2006-01-02 15:04"),
			s.MsgCount,
		)
		m.lines = append(m.lines, dimStyle.Render(line))
	}
	m.lines = append(m.lines, dimStyle.Render("  输入序号选择（非数字 Enter 取消）："))

	m.input.Reset()
	m.input.Placeholder = "序号..."
	m.input.Focus()
	return m, textinput.Blink
}

// handleResumeSelection 处理用户在 resume 选择模式下的输入。
func (m tuiModel) handleResumeSelection(raw string) (tea.Model, tea.Cmd) {
	savedSessions := m.resumeSessions // 先保存，下面清空 m.resumeSessions
	m.resumeSelecting = false
	m.resumeSessions = nil
	m.input.Placeholder = "输入任务..."
	m.input.Reset()

	num, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || num < 1 || num > len(savedSessions) {
		m.lines = append(m.lines, dimStyle.Render("  已取消"))
		m.input.Focus()
		return m, textinput.Blink
	}

	info := savedSessions[num-1]
	sess, err := m.manager.OpenSession(m.outerCtx, info.ID)
	if err != nil {
		m.lines = append(m.lines, errorStyle.Render("  ✗ 加载会话失败: "+err.Error()))
		m.input.Focus()
		return m, textinput.Blink
	}
	m.session = sess
	m.sessionID = info.ID
	if m.eng != nil {
		m.eng.SetSession(sess)
	}
	m.lines = append(m.lines, dimStyle.Render(
		fmt.Sprintf("  ✓ 已切换到会话 %s（%d 条消息）", info.ID, info.MsgCount),
	))
	m.input.Focus()
	return m, textinput.Blink
}

// confirmPlanReview 处理审查对话框的确认操作，cursor 0-3 对应四个选项。
// 由 Enter 键（光标位置）和 Esc 键（强制选项 3）调用。
func (m tuiModel) confirmPlanReview(cursor int) (tea.Model, tea.Cmd) {
	m.planReviewing = false
	m.planReviewCursor = 0
	switch cursor {
	case 0:
		// 批准并自动执行：切换到 Default 模式（完整工具权限），开启 autoExecuting 续跑循环。
		m.planMode = planning.PlanModeDefault
		m.input.Placeholder = "输入任务..."
		m.autoExecuting = true
		m.autoExecPrevDone = 0
		m.autoExecStuck = 0
		if m.eng != nil {
			m.eng.SetPlanMode(planning.PlanModeDefault)
		}
		m.lines = append(m.lines, dimStyle.Render("  ▶ 批准计划 — 自动执行中"))
		return m.dispatch(execPrompt)
	case 1:
		// 批准并逐步确认编辑（当前行为与选项 0 相同，planMode 设为 AutoEdit 预留扩展）。
		m.planMode = planning.PlanModeAutoEdit
		m.input.Placeholder = "输入任务..."
		m.autoExecuting = true
		m.autoExecPrevDone = 0
		m.autoExecStuck = 0
		if m.eng != nil {
			m.eng.SetPlanMode(planning.PlanModeAutoEdit)
		}
		m.lines = append(m.lines, dimStyle.Render("  ▶ 批准计划 — 逐步执行中"))
		return m.dispatch(execPrompt)
	case 2:
		// 继续修改计划：保持 Plan Mode，恢复输入框供用户继续描述修改意见。
		m.input.Placeholder = "继续描述修改意见..."
		m.input.Focus()
		return m, textinput.Blink
	default:
		// 取消（选项 3 / Esc）：切换回 Default 模式，恢复正常输入状态。
		m.planMode = planning.PlanModeDefault
		m.input.Placeholder = "输入任务..."
		if m.eng != nil {
			m.eng.SetPlanMode(planning.PlanModeDefault)
		}
		m.lines = append(m.lines, dimStyle.Render("  ✗ 已取消计划执行"))
		m.input.Focus()
		return m, textinput.Blink
	}
}

// handleTaskPanelKey 处理任务面板模态按键：列表态 ↑↓ 选择 / Enter 进详情 / Esc 关闭；
// 详情态 ↑↓ 滚动 / Esc 回列表 / Ctrl+T 关闭。
func (m tuiModel) handleTaskPanelKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	var list []subagent.TaskSnapshot
	if m.subAgentTracker != nil {
		list = m.subAgentTracker.List()
	}
	if m.taskDetailID == "" {
		switch msg.Type {
		case tea.KeyUp:
			if m.taskPanelCursor > 0 {
				m.taskPanelCursor--
			}
		case tea.KeyDown:
			if m.taskPanelCursor < len(list)-1 {
				m.taskPanelCursor++
			}
		case tea.KeyEnter:
			if m.taskPanelCursor >= 0 && m.taskPanelCursor < len(list) {
				m.taskDetailID = list[m.taskPanelCursor].ID
				m.taskDetailScroll = 0
			}
		case tea.KeyEsc, tea.KeyCtrlT:
			m.taskPanelMode = false
		}
		return m, nil
	}
	switch msg.Type {
	case tea.KeyUp:
		if m.taskDetailScroll > 0 {
			m.taskDetailScroll--
		}
	case tea.KeyDown:
		// 向下滚动，但夹住在最后一行——避免越滚越空（taskDetailScroll 无界增长后视图全空）。
		if m.subAgentTracker != nil {
			if d, ok := m.subAgentTracker.Get(m.taskDetailID); ok {
				if maxScroll := len(formatTaskLog(d)) - 1; m.taskDetailScroll < maxScroll {
					m.taskDetailScroll++
				}
			}
		}
	case tea.KeyEsc:
		m.taskDetailID = ""
	case tea.KeyCtrlT:
		m.taskPanelMode = false
		m.taskDetailID = ""
	}
	return m, nil
}

// truncateUTF8 按字节截断 s 到 maxBytes 以内，同时保证不在多字节 UTF-8 字符中间截断。
//
// 实现策略：先强制截断到 maxBytes 字节，再从末尾反向逐字节扫描：
//   - utf8.DecodeLastRuneInString 返回 (RuneError, 1) 表示末尾字节是不完整序列的一部分 → 剥离
//   - 返回有效 rune 或 (RuneError, size>1)（即合法的 U+FFFD）→ 停止剥离
//
// 这比 strings.ToValidUTF8 或 utf8.ValidString 的逐字节扫描更高效：
// 多字节 UTF-8 最多 4 字节，最多剥离 3 次即可找到完整末尾。
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

// interactiveCmds 是需要 PTY（伪终端）的交互式程序白名单。
//
// Bubbletea 以 AltScreen 模式独占终端输入输出，这类程序无法在其内部正常运行：
//   - 编辑器（vim/nano/emacs）：需要全屏 raw 模式操控终端
//   - 分页器（less/more/man）：需要逐页控制字符
//   - 监控工具（top/htop/watch）：需要持续重绘整个屏幕
//   - 远程/复用工具（ssh/tmux/screen）：需要建立自己的 PTY 会话
//
// 命中时输出错误提示，引导用户在独立终端窗口运行。
var interactiveCmds = map[string]bool{
	"vim": true, "vi": true, "nano": true, "emacs": true,
	"ssh": true, "top": true, "htop": true, "less": true,
	"man": true, "more": true, "watch": true, "tmux": true,
	"screen": true,
}

// isInteractiveCmd 检查命令行的首个 token 是否为已知交互式程序。
// 对绝对路径命令（如 /usr/bin/vim）使用 filepath.Base 提取文件名后再匹配，
// 防止因路径前缀导致的漏判。
func isInteractiveCmd(cmd string) bool {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	return interactiveCmds[filepath.Base(fields[0])]
}

// runShellCmd 返回异步执行 Shell 命令的 tea.Cmd。
//
// Bubbletea 运行时会在独立 goroutine 中调用此函数返回的闭包，不阻塞主消息循环。
// 执行结果以 shellResultMsg 形式发回主消息队列，由 Update() 处理。
//
// 实现细节：
//   - 通过 bash -c 执行，支持管道、重定向、&&/|| 等复杂 Shell 语法
//   - 固定超时 30s：与 BashTool.bashHardTimeout 保持一致，防止长时间阻塞 TUI
//   - CombinedOutput 合并 stdout 和 stderr，确保错误信息对用户可见
//   - err != nil 同时覆盖非零退出码和命令超时被终止两种情况（统一为 isErr=true）
func runShellCmd(workDir, cmd string) tea.Cmd {
	return func() tea.Msg {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, "bash", "-c", cmd)
		c.Dir = workDir
		out, err := c.CombinedOutput()
		return shellResultMsg{
			cmd:    cmd,
			output: string(out),
			isErr:  err != nil,
			dur:    time.Since(start).Round(time.Millisecond),
		}
	}
}

// dispatchShellCommand 处理用户侧 "!<cmd>" Shell 命令执行。
//
// 执行流程：
//  1. 空命令直接返回（重新聚焦输入框）
//  2. 追加 "$ cmd" 行到 Scrollback（先于执行结果显示，提供即时反馈）
//  3. isInteractiveCmd 检测：命中则追加错误提示，不执行
//  4. 非交互式命令：返回 runShellCmd tea.Cmd，由 Bubbletea 异步执行
//
// 注意：此函数不设置 m.running = true，Shell 命令与 LLM 推理是独立的并行路径，
// 二者都不会阻塞 TUI 主循环。
func (m tuiModel) dispatchShellCommand(cmd string) (tuiModel, tea.Cmd) {
	if cmd == "" {
		m.input.Focus()
		return m, textinput.Blink
	}
	// 先行追加命令行显示，使用户即时看到命令已提交（即使执行尚未完成）
	m.lines = append(m.lines, shellCmdStyle.Render("$ "+cmd))
	if isInteractiveCmd(cmd) {
		m.lines = append(m.lines, shellErrStyle.Render("  ✗ 该命令需要交互式终端，请在独立终端窗口中运行"))
		m.input.Focus()
		return m, textinput.Blink
	}
	return m, runShellCmd(m.workDir, cmd)
}

// updateTodoBlock 在对话流末尾追加最新 todo 快照。
// 每次 todo_write 完成后调用，快照追加在工具完成行之后，呈现实时进度。
func (m tuiModel) updateTodoBlock() tuiModel {
	if m.todoStore == nil {
		return m
	}
	todoLines := m.renderTodoLines(m.todoStore.Read())
	if len(todoLines) == 0 {
		return m
	}
	m.lines = append(m.lines, todoLines...)
	return m
}

// handleApprovalKey 处理审批对话框的键盘输入。
func (m tuiModel) handleApprovalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.approvalInputting {
		switch msg.Type {
		case tea.KeyEnter:
			return m.confirmApproval(4)
		case tea.KeyEsc:
			m.approvalInputting = false
			m.approvalFeedback = ""
			m.approvalCursor = 4
			return m, nil
		case tea.KeyBackspace, tea.KeyDelete:
			// 使用 rune 安全删除最后一个字符，避免在多字节 UTF-8 字符中间截断。
			if runes := []rune(m.approvalFeedback); len(runes) > 0 {
				m.approvalFeedback = string(runes[:len(runes)-1])
			}
		default:
			if msg.Runes != nil {
				m.approvalFeedback += string(msg.Runes)
			}
		}
		return m, nil
	}

	switch msg.Type {
	case tea.KeyUp:
		if m.approvalCursor > 0 {
			m.approvalCursor--
		}
	case tea.KeyDown:
		if m.approvalCursor < 4 {
			m.approvalCursor++
		}
	case tea.KeyEnter:
		if m.approvalCursor == 4 {
			m.approvalInputting = true
			m.approvalFeedback = ""
			return m, nil
		}
		return m.confirmApproval(m.approvalCursor)
	case tea.KeyEsc:
		return m.confirmApproval(3)
	case tea.KeyCtrlC, tea.KeyCtrlD:
		return m.confirmApproval(3) // treat as deny
	case tea.KeyRunes:
		if len(msg.Runes) == 1 {
			switch msg.Runes[0] {
			case '1':
				return m.confirmApproval(0)
			case '2':
				return m.confirmApproval(1)
			case '3':
				return m.confirmApproval(2)
			case '4':
				return m.confirmApproval(3)
			case '5':
				m.approvalInputting = true
				m.approvalFeedback = ""
				return m, nil
			}
		}
	}
	return m, nil
}

// confirmApproval 处理审批确认，cursor 0-4 对应五个选项：
//
//	0 = 允许（仅本次）
//	1 = 允许（本会话不再提示）
//	2 = 总是允许（写入白名单）
//	3 = 拒绝
//	4 = 拒绝并提供反馈
func (m tuiModel) confirmApproval(cursor int) (tea.Model, tea.Cmd) {
	if m.approvalRequest == nil {
		m.approvalPending = false
		return m, readNextEvent(m.eventCh)
	}
	req := m.approvalRequest
	m.approvalPending = false
	m.approvalRequest = nil
	m.approvalCursor = 0
	m.approvalInputting = false

	var resp hooks.ApprovalResponse
	switch cursor {
	case 0:
		resp = hooks.ApprovalResponse{Approved: true}
		m.lines = append(m.lines, dimStyle.Render(
			fmt.Sprintf("  ✓ 已允许（本次）: %s", req.ToolCall.Name),
		))
	case 1:
		resp = hooks.ApprovalResponse{Approved: true}
		m.lines = append(m.lines, dimStyle.Render(
			fmt.Sprintf("  ✓ 已允许（会话）: %s", req.ToolCall.Name),
		))
	case 2:
		resp = hooks.ApprovalResponse{Approved: true, Remember: true}
		m.writeApprovalToConfig(req)
		m.lines = append(m.lines, dimStyle.Render(
			fmt.Sprintf("  ✓ 已允许（写入白名单）: %s", req.ToolCall.Name),
		))
	case 3:
		resp = hooks.ApprovalResponse{Approved: false}
		m.lines = append(m.lines, errorStyle.Render(
			fmt.Sprintf("  ✗ 已拒绝: %s", req.ToolCall.Name),
		))
	case 4:
		resp = hooks.ApprovalResponse{Approved: false, Feedback: m.approvalFeedback}
		m.lines = append(m.lines, errorStyle.Render(
			fmt.Sprintf("  ✗ 已拒绝: %s — %s", req.ToolCall.Name, m.approvalFeedback),
		))
		m.approvalFeedback = ""
	}

	req.ResponseCh <- resp
	return m, readNextEvent(m.eventCh)
}

// writeApprovalToConfig 将审批的工具调用写入白名单配置文件（"总是允许"持久化）。
func (m tuiModel) writeApprovalToConfig(req *engine.ApprovalRequest) {
	if m.settingsPath == "" {
		return
	}
	rules, err := permission.LoadRules(m.settingsPath)
	if err != nil {
		return
	}
	var pattern string
	if req.ToolCall.Name == "bash" {
		var args struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(req.ToolCall.Arguments, &args); err == nil && args.Command != "" {
			// 取命令的第一个单词作为关键词
			fields := strings.Fields(args.Command)
			if len(fields) > 0 {
				pattern = fmt.Sprintf("bash(*%s*)", fields[0])
			}
		}
	}
	if pattern == "" {
		pattern = req.ToolCall.Name
	}
	rules.AddRule(permission.RuleAllow, []string{pattern})
	if err := os.MkdirAll(filepath.Dir(m.settingsPath), 0700); err != nil {
		return
	}
	_ = permission.SaveRules(m.settingsPath, rules)
}

// mcpPanelLineCount 计算 renderMCPPanel 的滚动内容区实际行数，用于滚动上界控制。
// 必须与 renderMCPPanel 的实际输出保持一致：仅当 t.Description != "" 时才计描述行。
func (m tuiModel) mcpPanelLineCount() int {
	count := 0
	for _, s := range m.mcpServers {
		count++ // 服务器 header 行
		if s.Status == mcppkg.StatusConnected {
			for _, t := range s.ToolDetails {
				count++ // 工具名行
				if t.Description != "" {
					count++ // 描述行（仅在有描述时）
				}
			}
		} else if s.ErrMsg != "" {
			count++ // 错误信息行
		}
		count++ // 空行分隔
	}
	return count
}

// handleMCPPanelKey 处理 MCP 工具面板（模态）的键盘输入。
//
//   - Esc    → 关闭面板，返回对话视图
//   - e / E  → 暂停 TUI，打开 .mcp.json 配置文件（$EDITOR 或系统关联程序）
//   - ↑ / k  → 向上滚动工具列表
//   - ↓ / j  → 向下滚动工具列表（夹住在内容末尾）
//   - Ctrl+C → 退出程序
func (m tuiModel) handleMCPPanelKey(msg tea.KeyMsg) (tuiModel, tea.Cmd) {
	// contentH 与 View() 中 mcpPanelOverhead 保持一致
	contentH := m.height - mcpPanelOverhead
	if contentH < 1 {
		contentH = 1
	}
	maxScroll := m.mcpPanelLineCount() - contentH
	if maxScroll < 0 {
		maxScroll = 0
	}
	switch msg.Type {
	case tea.KeyEsc:
		m.mcpPanelMode = false
		m.mcpPanelScroll = 0
	case tea.KeyUp:
		if m.mcpPanelScroll > 0 {
			m.mcpPanelScroll--
		}
	case tea.KeyDown:
		if m.mcpPanelScroll < maxScroll {
			m.mcpPanelScroll++
		}
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyRunes:
		switch string(msg.Runes) {
		case "k":
			if m.mcpPanelScroll > 0 {
				m.mcpPanelScroll--
			}
		case "j":
			if m.mcpPanelScroll < maxScroll {
				m.mcpPanelScroll++
			}
		case "e", "E":
			return m, openMCPConfigCmd(m.mcpConfigPath)
		}
	}
	return m, nil
}

func tierLabel(tier memory.CompactionTier) string {
	switch tier {
	case memory.TierWarn:
		return "Warn"
	case memory.TierSoft:
		return "Soft"
	case memory.TierFull:
		return "Full"
	case memory.TierEmergency:
		return "Emergency"
	default:
		return "Unknown"
	}
}
