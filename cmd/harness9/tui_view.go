// TUI View 层：负责根据 tuiModel 状态生成每帧终端输出字符串。
//
// View 层是 Bubbletea Elm 架构的渲染侧，仅做只读数据映射，不产生副作用。
// 包含以下渲染函数（均为 tuiModel 方法）：
//
//   - View()                 — 顶层渲染入口，按 phase / 模态状态分发
//   - renderConversation()   — 对话历史 Scrollback 区
//   - renderToolProgress()   — 工具执行 Spinner 进度行
//   - renderSubAgentProgress() — 子代理流式进度块
//   - renderStatusBar()      — 常驻状态栏（model/mode/workdir/token）
//   - renderPlanReviewDialog() — Plan Mode 审查对话框（模态）
//   - renderApprovalDialog() — 工具审批对话框（模态）
//   - renderTaskPanel()      — 后台任务面板（模态）
//   - renderInput()          — 底部输入框（含 Shell/Plan 模式适配）
//   - renderFooter()         — 底部快捷键提示行
//   - renderPlanLines()      — todo 任务列表快照行
//   - accentStyle()          — 当前模式对应的强调色
//   - activeStatusBarStyle() — 当前模式对应的状态栏背景样式
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	mcppkg "github.com/harness9/internal/mcp"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/planning"
	"github.com/harness9/internal/sandbox"
	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/subagent"
)

// accentStyle 返回当前执行模式对应的强调色样式（accent style）。
//
// 优先级（高→低）：Shell 模式 > Plan Mode > Default。
// Shell 模式优先级高于 Plan Mode 的原因：用户激活 Shell 模式时需要即时视觉反馈，
// 即使当前是 Plan Mode 也应当清晰显示 Shell 模式的绿色主题。
//
//   - shellMode=true              → 亮绿色（shellModeAccentStyle, #83）
//   - PlanModePlan / PlanModeAutoEdit → 琥珀黄（planAccentStyle, #220）
//   - PlanModeDefault              → 青色（cyanStyle, #81）
//
// 将颜色切换逻辑集中于此一处，renderStatusBar / renderFooter / renderPlanLines
// 统一调用，View 层无散落的 if 判断，模式颜色映射易于维护和扩展。
func (m tuiModel) accentStyle() lipgloss.Style {
	if m.shellMode {
		return shellModeAccentStyle
	}
	if m.planMode != planning.PlanModeDefault {
		return planAccentStyle
	}
	return cyanStyle
}

// activeStatusBarStyle 返回当前模式下的状态栏容器背景样式。
//
// 三种背景色通过不同色调给用户强烈的视觉区分信号：
//   - shellMode=true      → 深绿底（shellStatusBarStyle, bg #22 / fg #120）
//   - Plan/AutoEdit 模式  → 深橙底（planStatusBarStyle,  bg #94 / fg #220）
//   - Default 模式        → 深灰底（statusBarStyle,       bg #235 / fg #11）
//
// 优先级规则与 accentStyle() 保持一致。
func (m tuiModel) activeStatusBarStyle() lipgloss.Style {
	if m.shellMode {
		return shellStatusBarStyle
	}
	if m.planMode != planning.PlanModeDefault {
		return planStatusBarStyle
	}
	return statusBarStyle
}

// shortPath 将绝对路径中的 $HOME 替换为 "~"。
func shortPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return strings.Replace(p, home, "~", 1)
}

// renderPlanLines 将 TodoItem 列表渲染为结构化多行文本，追加到 Scrollback（m.lines）。
//
// 输出格式：标题行（图标 + "Tasks" + 进度统计 + 活跃任务数）+ 分隔线 + 各任务行。
// 每个任务行包含：序号、状态图标（✔/▶/○/⊘）和内容文本。
//
// 状态图标映射：
//   - in_progress → ▶（黄色，工具运行色）
//   - completed   → ✔（绿色）
//   - cancelled   → ⊘（灰色）
//   - pending     → ○（灰色）
//
// 颜色跟随当前 planMode 的 accentStyle（Plan Mode 下为琥珀色，其他为青色）。
func (m tuiModel) renderPlanLines(items []planning.PlanItem) []string {
	if len(items) == 0 {
		return nil
	}

	accent := m.accentStyle()

	// 统计完成数与进行中数
	var done, active int
	for _, item := range items {
		switch item.Status {
		case planning.PlanCompleted:
			done++
		case planning.PlanInProgress:
			active++
		}
	}

	lines := make([]string, 0, len(items)+3)

	// 标题行：图标 + "Tasks" + 进度 + 活跃数
	progress := accent.Render(fmt.Sprintf("%d/%d", done, len(items)))
	title := "  " + accent.Render("☰") + "  " +
		lipgloss.NewStyle().Bold(true).Render("Tasks") +
		dimStyle.Render("  ·  ") + progress
	if active > 0 {
		title += dimStyle.Render("  ·  ") + toolRunStyle.Render(fmt.Sprintf("%d active", active))
	}
	lines = append(lines, title)
	lines = append(lines, dimStyle.Render("  ──────────────────────────────────────"))

	// 各任务项
	for i, item := range items {
		num := dimStyle.Render(fmt.Sprintf("%2d.", i+1))
		var icon, content string
		switch item.Status {
		case planning.PlanInProgress:
			icon = toolRunStyle.Render("▶")
			content = toolRunStyle.Render(item.Content)
		case planning.PlanCompleted:
			icon = toolOKStyle.Render("✔")
			content = dimStyle.Render(item.Content)
		case planning.PlanCancelled:
			icon = dimStyle.Render("⊘")
			content = dimStyle.Render(item.Content)
		default: // pending
			icon = dimStyle.Render("○")
			content = item.Content
		}
		lines = append(lines, "  "+num+"  "+icon+"  "+content)
	}
	return lines
}

// renderConversation 渲染对话历史区（Scrollback）。
// scrollH 为可显示行数（由 scrollHeight() 计算）。
func (m tuiModel) renderConversation(scrollH int) string {
	var scrollLines []string
	if m.viewTop < 0 || len(m.lines) <= scrollH {
		if len(m.lines) >= scrollH {
			scrollLines = m.lines[len(m.lines)-scrollH:]
		} else {
			pad := make([]string, scrollH-len(m.lines))
			scrollLines = append(pad, m.lines...)
		}
	} else {
		start := m.viewTop
		end := start + scrollH
		if end > len(m.lines) {
			end = len(m.lines)
		}
		scrollLines = m.lines[start:end]
		if len(scrollLines) < scrollH {
			pad := make([]string, scrollH-len(scrollLines))
			scrollLines = append(pad, scrollLines...)
		}
	}
	return strings.Join(scrollLines, "\n")
}

// renderToolProgress 渲染工具执行进度行。
// 仅在 phaseChat && running && currentTool != "" 时调用。
func (m tuiModel) renderToolProgress() string {
	verb := spinnerVerbs[m.verbIdx]
	elapsed := time.Since(m.toolStart).Round(time.Millisecond)
	summary := summarizeTool(m.currentTool, m.toolArgs)

	var toolDisplay string
	if summary != "" {
		toolDisplay = fmt.Sprintf("%s(%s)", m.currentTool, summary)
	} else {
		toolDisplay = m.currentTool
	}

	return "  " +
		verbRunStyle.Render(m.spinner.View()+" "+verb+"...") +
		toolRunStyle.Render("  "+toolDisplay) +
		dimStyle.Render(fmt.Sprintf("  [%s]", elapsed))
}

// renderCompactProgress 渲染 /compact 命令执行期间的 spinner 进度行。
// 与 renderToolProgress 风格一致：spinner + 动词 + 操作名称。
func (m tuiModel) renderCompactProgress() string {
	return "  " +
		verbRunStyle.Render(m.spinner.View()+" 压缩中...") +
		toolRunStyle.Render("  /compact")
}

// renderSubAgentProgress 渲染当前活跃子代理的流式进度块（暗青色缩进行）。
// 仅在 len(m.subAgentLines) > 0 时由 View() 调用；每行已在 EventSubAgent 处理时带上 [agent] 前缀。
func (m tuiModel) renderSubAgentProgress() string {
	var sb strings.Builder
	for i, line := range m.subAgentLines {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString("  " + subAgentLineStyle.Render(line))
	}
	return sb.String()
}

// renderStatusBar 渲染常驻状态栏（model 名 + mode + workdir + session 信息）。
// 宽度充足时展示完整 session ID；窄终端（< 120 列）时截断为前 8 位加 "…"。
func (m tuiModel) renderStatusBar() string {
	accent := m.accentStyle()

	sessionInfo := ""
	if m.sessionID != "" {
		sid := m.sessionID
		if m.width < 120 && len(sid) > 8 {
			sid = sid[:8] + "…"
		}
		sessionInfo = dimStyle.Render("  │  session: ") + accent.Render(sid)

		if m.contextTokens > 0 {
			var tokenStr string
			if m.contextWindow > 0 {
				pct := m.contextTokens * 100 / m.contextWindow
				var tokenStyle lipgloss.Style
				switch {
				case pct >= 80:
					tokenStyle = tokenHighStyle
				case pct >= 50:
					tokenStyle = tokenWarnStyle
				default:
					tokenStyle = tokenOKStyle
				}
				tokenStr = tokenStyle.Render(
					memory.FormatTokenCount(m.contextTokens)+"/"+memory.FormatTokenCount(m.contextWindow),
				) + dimStyle.Render(fmt.Sprintf(" (%d%%)", pct))
			} else {
				tokenStr = accent.Render(memory.FormatTokenCount(m.contextTokens))
			}
			sessionInfo += dimStyle.Render("  ctx: ") + tokenStr
		}
	}
	// modePart 在状态栏中显示当前模式标签：
	//   Shell 模式  → "│ SHELL"（亮绿加粗），优先于 Plan 模式标签展示
	//   Plan/Auto   → "│ [PLAN]"（琥珀 Color "208"）
	//   Default     → 空字符串，不占用状态栏空间
	modeLabel := m.planMode.Label()
	var modePart string
	if m.shellMode {
		modePart = dimStyle.Render("  │  ") + shellModeLabelInBarStyle.Render("SHELL")
	} else if modeLabel != "" {
		modePart = dimStyle.Render("  │  ") + planModeLabelStyle.Render(modeLabel)
	}
	if m.permMode != engine.PermissionModeDefault {
		modePart += dimStyle.Render("  │  ") + approvalTitleMedStyle.Render(m.permMode.String())
	}

	var tasksPart string
	if m.planStore != nil {
		items := m.planStore.Read()
		if len(items) > 0 {
			var completed int
			for _, item := range items {
				if item.Status == planning.PlanCompleted {
					completed++
				}
			}
			tasksPart = dimStyle.Render("  │  ") + accent.Render(fmt.Sprintf("%d/%d tasks", completed, len(items)))
		}
	}

	// bgTasksPart 在存在后台子代理任务时展示 "⚙ {运行} 运行/{完成} 完成" 指示段，
	// 提示用户可用 Ctrl+T / /tasks 打开任务面板查看详情。
	var bgTasksPart string
	if m.subAgentTracker != nil {
		if r, d := m.subAgentTracker.RunningCount(), m.subAgentTracker.DoneCount(); r > 0 || d > 0 {
			bgTasksPart = dimStyle.Render("  │  ") +
				accent.Render(fmt.Sprintf("⚙ %d 运行/%d 完成", r, d))
		}
	}

	content := dimStyle.Render("  model: ") +
		accent.Render(m.modelName) +
		modePart +
		tasksPart +
		bgTasksPart +
		dimStyle.Render("  │  ") +
		accent.Render(shortPath(m.workDir)) +
		sessionInfo
	return m.activeStatusBarStyle().Width(m.width).Render(content)
}

// renderPlanReviewDialog 渲染 Plan Mode 完成后的审查选择对话框（带圆角边框）。
// 对话框在 planReviewing == true 时由 View() 插入到 StatusBar 之前，
// ↑↓ 移动光标，Enter 确认，Esc 取消。
func (m tuiModel) renderPlanReviewDialog() string {
	options := []string{
		"批准并自动执行",
		"批准并逐步确认编辑",
		"继续修改计划（保持 Plan Mode）",
		"取消",
	}

	var sb strings.Builder
	sb.WriteString(planModeLabelStyle.Render("Plan Mode 完成 — 选择下一步操作"))
	sb.WriteString("\n\n")
	for i, opt := range options {
		if i == m.planReviewCursor {
			sb.WriteString(planAccentStyle.Render("▶ ") + planReviewSelectedStyle.Render(opt))
		} else {
			sb.WriteString("  " + dimStyle.Render(opt))
		}
		if i < len(options)-1 {
			sb.WriteByte('\n')
		}
	}
	sb.WriteString("\n\n")
	sb.WriteString(dimStyle.Render("↑↓ 移动  Enter 确认  Esc 取消"))

	return planReviewBoxStyle.Render(sb.String())
}

// renderTaskPanel 渲染后台任务面板：taskDetailID 为空渲染列表，否则渲染该任务详情日志。
func (m tuiModel) renderTaskPanel() string {
	var sb strings.Builder
	if m.subAgentTracker == nil {
		return "（无任务）"
	}
	if m.taskDetailID == "" {
		sb.WriteString(m.accentStyle().Render("后台子代理任务（↑↓ 选择，Enter 查看，Esc 关闭）") + "\n\n")
		list := m.subAgentTracker.List()
		if len(list) == 0 {
			sb.WriteString(dimStyle.Render("  暂无后台任务") + "\n")
		}
		for i, s := range list {
			icon := "●"
			switch s.State {
			case subagent.TaskDone:
				icon = "✓"
			case subagent.TaskFailed:
				icon = "✗"
			}
			row := fmt.Sprintf("%s %s  %s  \"%s\"", icon, s.AgentName, s.State.String(), truncateUTF8(s.Prompt, 48))
			if i == m.taskPanelCursor {
				sb.WriteString(userMsgStyle.Render("▶ "+row) + "\n")
			} else {
				sb.WriteString("  " + dimStyle.Render(row) + "\n")
			}
		}
		return sb.String()
	}
	d, ok := m.subAgentTracker.Get(m.taskDetailID)
	if !ok {
		return "（任务已不存在，Esc 返回）"
	}
	sb.WriteString(m.accentStyle().Render(fmt.Sprintf("%s — %s  （↑↓ 滚动，Esc 返回）", d.AgentName, d.State.String())) + "\n\n")
	lines := formatTaskLog(d)
	start := m.taskDetailScroll
	if start > len(lines) {
		start = len(lines)
	}
	for _, ln := range lines[start:] {
		sb.WriteString(ln + "\n")
	}
	return sb.String()
}

// formatTaskLog 把任务全过程日志格式化为展示行（与 subAgentLines 风格一致）。
func formatTaskLog(d subagent.TaskDetail) []string {
	var out []string
	for _, u := range d.Log {
		switch u.Kind {
		case schema.SubAgentStart:
			out = append(out, subAgentLineStyle.Render("启动…"))
		case schema.SubAgentToolStart:
			line := "▸ " + u.ToolName
			if a := strings.TrimSpace(u.Text); a != "" && a != "{}" {
				line += "(" + truncateUTF8(a, 80) + ")"
			}
			out = append(out, subAgentLineStyle.Render(line))
		case schema.SubAgentDelta:
			if s := strings.TrimSpace(u.Text); s != "" {
				out = append(out, subAgentLineStyle.Render(s))
			}
		case schema.SubAgentToolResult:
			if u.IsError {
				out = append(out, subAgentLineStyle.Render("  ✗ 工具执行失败"))
			}
		case schema.SubAgentError:
			out = append(out, subAgentLineStyle.Render("✗ "+u.Text))
		}
	}
	if d.State != subagent.TaskRunning && d.FinalText != "" {
		out = append(out, "", subAgentLineStyle.Render("— 最终结果 —"))
		for _, ln := range strings.Split(strings.TrimRight(d.FinalText, "\n"), "\n") {
			out = append(out, subAgentLineStyle.Render(ln))
		}
	}
	return out
}

// renderInput 渲染底部输入行，根据当前模式切换提示符样式。
//
//   - Shell 模式：" [SHELL]  $ <textinput>"（深橄榄徽章 + 绿色 $ 提示符）
//   - Plan Mode：  "  › <textinput>"（琥珀黄 › 提示符）
//   - Default 模式："  › <textinput>"（普通灰色 › 提示符）
//
// Shell 模式下的徽章和 $ 提示符使用预计算的包级样式变量，避免每帧创建新样式对象。
func (m tuiModel) renderInput() string {
	if m.shellMode {
		badge := shellModeTagStyle.Render("SHELL")
		prompt := shellModePromptStyle.Render(" $ ")
		return " " + badge + prompt + m.input.View()
	}
	if m.planMode != planning.PlanModeDefault {
		return "  " + planAccentStyle.Render("›") + " " + m.input.View()
	}
	return "  › " + m.input.View()
}

// renderFooter 渲染底部快捷键提示行，提示内容按以下优先级选取：
//
//  1. Shell 模式：固定展示 enter/esc/ctrl+c 及"输出自动注入"说明
//  2. 补全提示（completionHint != ""）：Tab 补全候选列表
//  3. 手动滚动（viewTop ≥ 0）：含百分比的滚动位置提示
//  4. 默认：常规快捷键提示（enter / / / ↑↓ / ctrl+c）
//
// Shell 模式 footer 特意提示"输出自动注入 LLM 上下文"，
// 使用户意识到命令结果会被纳入下次 LLM 请求的上下文中。
func (m tuiModel) renderFooter() string {
	if m.shellMode {
		a := shellModeAccentStyle
		return "  " +
			a.Render("enter") + dimStyle.Render(" 执行  ") +
			a.Render("esc") + dimStyle.Render(" 取消  ") +
			dimStyle.Render("输出自动注入 LLM 上下文  ") +
			a.Render("ctrl+c") + dimStyle.Render(" 退出")
	}

	if m.completionHint != "" {
		return m.completionHint
	}

	accent := m.accentStyle()

	if m.viewTop >= 0 {
		scrollH := m.scrollHeight()
		maxTop := len(m.lines) - scrollH
		if maxTop < 1 {
			maxTop = 1
		}
		pct := m.viewTop * 100 / maxTop
		return "  " +
			accent.Render("enter") + dimStyle.Render(" 发送  ") +
			accent.Render("/") + dimStyle.Render(" 技能命令  ") +
			accent.Render("↑↓") + dimStyle.Render(" 滚动  ") +
			accent.Render("end") + dimStyle.Render(fmt.Sprintf(" 回底部 (%d%%)  ", pct)) +
			accent.Render("ctrl+c") + dimStyle.Render(" 退出")
	}

	return "  " +
		accent.Render("enter") + dimStyle.Render(" 发送  ") +
		accent.Render("/") + dimStyle.Render(" 技能命令  ") +
		accent.Render("↑↓") + dimStyle.Render(" 滚动  ") +
		accent.Render("ctrl+c") + dimStyle.Render(" 退出")
}

// renderApprovalDialog 渲染工具执行审批对话框。
// 在 approvalPending == true 时由 View() 插入 StatusBar 之前。
func (m tuiModel) renderApprovalDialog() string {
	if m.approvalRequest == nil {
		return ""
	}
	req := m.approvalRequest

	var titleStyle lipgloss.Style
	switch req.RiskLevel {
	case "high":
		titleStyle = approvalTitleHighStyle
	case "medium":
		titleStyle = approvalTitleMedStyle
	default:
		titleStyle = approvalTitleLowStyle
	}

	riskLabel := map[string]string{
		"high":   "高风险",
		"medium": "中等风险",
		"low":    "低风险",
	}[req.RiskLevel]
	if riskLabel == "" {
		riskLabel = "需确认"
	}

	var sb strings.Builder
	sb.WriteString(titleStyle.Render(fmt.Sprintf("⚠  工具审批请求 [%s]", riskLabel)))
	sb.WriteString("\n\n")
	sb.WriteString(dimStyle.Render("工具：") + lipgloss.NewStyle().Bold(true).Render(req.ToolCall.Name))
	if req.Reason != "" {
		sb.WriteString("\n")
		sb.WriteString(dimStyle.Render("原因：") + req.Reason)
	}
	sb.WriteString("\n\n")

	if m.approvalInputting {
		sb.WriteString(dimStyle.Render("请输入拒绝原因（Enter 提交，Esc 取消）：\n"))
		sb.WriteString("> " + m.approvalFeedback + "█")
	} else {
		options := []string{
			"允许（仅本次）",
			"允许（本会话不再提示）",
			"总是允许（写入白名单）",
			"拒绝",
			"拒绝并提供反馈...",
		}
		for i, opt := range options {
			if i == m.approvalCursor {
				sb.WriteString(approvalSelectedStyle.Render("▶ ") +
					approvalSelectedStyle.Render(fmt.Sprintf("[%d] %s", i+1, opt)))
			} else {
				sb.WriteString(dimStyle.Render(fmt.Sprintf("  [%d] %s", i+1, opt)))
			}
			if i < len(options)-1 {
				sb.WriteByte('\n')
			}
		}
		sb.WriteString("\n\n")
		sb.WriteString(dimStyle.Render("↑↓ 移动  Enter/1-5 确认  Esc 拒绝"))
	}

	return approvalBoxStyle.Render(sb.String())
}

// View 实现 tea.Model——根据当前 phase 渲染完整 TUI 帧。
func (m tuiModel) View() string {
	if m.width == 0 {
		return ""
	}

	var sb strings.Builder

	if m.phase == phaseWelcome {
		sb.WriteString(bannerContent(m.width))
		sb.WriteByte('\n')
		sb.WriteString(m.renderStatusBar())
		sb.WriteByte('\n')
		sb.WriteString(m.renderInput())
		sb.WriteByte('\n')
		sb.WriteString(m.renderFooter())
	} else {
		// MCP 工具面板独占全屏，必须在 renderConversation 之前处理，
		// 否则对话区会占满屏幕，MCP 面板内容被压到可视区域之外。
		if m.mcpPanelMode {
			contentH := m.height - mcpPanelOverhead
			if contentH < 1 {
				contentH = 1
			}
			sb.WriteString(m.renderMCPPanel(contentH))
			sb.WriteByte('\n')
			sb.WriteString(m.renderStatusBar())
			return sb.String()
		}
		scrollH := m.scrollHeight()
		sb.WriteString(m.renderConversation(scrollH))
		sb.WriteByte('\n')
		if len(m.subAgentLines) > 0 {
			sb.WriteString(m.renderSubAgentProgress())
			sb.WriteByte('\n')
		}
		if m.running && m.currentTool != "" {
			sb.WriteString(m.renderToolProgress())
			sb.WriteByte('\n')
		}
		if m.compacting {
			sb.WriteString(m.renderCompactProgress())
			sb.WriteByte('\n')
		}
		if m.approvalPending {
			sb.WriteString(m.renderApprovalDialog())
			sb.WriteByte('\n')
			sb.WriteString(m.renderStatusBar())
			return sb.String()
		}
		if m.planReviewing {
			sb.WriteString(m.renderPlanReviewDialog())
			sb.WriteByte('\n')
			sb.WriteString(m.renderStatusBar())
			return sb.String()
		}
		if m.taskPanelMode {
			sb.WriteString(m.renderTaskPanel())
			sb.WriteByte('\n')
			sb.WriteString(m.renderStatusBar())
			return sb.String()
		}
		sb.WriteString(m.renderStatusBar())
		sb.WriteByte('\n')
		if bar := m.renderSandboxBar(); bar != "" {
			sb.WriteString(bar)
			sb.WriteByte('\n')
		}
		if bar := m.renderMCPBar(); bar != "" {
			sb.WriteString(bar)
			sb.WriteByte('\n')
		}
		sb.WriteString(m.renderInput())
		sb.WriteByte('\n')
		sb.WriteString(m.renderFooter())
	}

	return sb.String()
}

// renderSandboxBar 渲染 Sandbox 状态栏，仅在有活跃 Sandbox 时显示。
// 格式：[Sandbox] 3a2f (main) Running │ 7b1c (sub-1) Running
func (m tuiModel) renderSandboxBar() string {
	if len(m.sandboxes) == 0 {
		return ""
	}

	parts := make([]string, 0, len(m.sandboxes)+1)
	parts = append(parts, sandboxBarBgStyle.Render("[Sandbox]"))

	for i, info := range m.sandboxes {
		label := "main"
		if i > 0 {
			label = fmt.Sprintf("sub-%d", i)
		}

		shortID := info.DockerID
		if len(shortID) > 4 {
			shortID = shortID[:4]
		}

		stateStr := info.State.String()
		var stateStyled string
		switch info.State {
		case sandbox.StateRunning:
			stateStyled = sandboxRunningStyle.Render(stateStr)
		case sandbox.StatePending:
			stateStyled = sandboxPendingStyle.Render(stateStr)
		case sandbox.StateStopping, sandbox.StateTerminated:
			stateStyled = sandboxStoppingStyle.Render(stateStr)
		case sandbox.StateFailed:
			stateStyled = sandboxFailedStyle.Render(stateStr)
		default:
			stateStyled = stateStr
		}

		parts = append(parts, fmt.Sprintf("%s (%s) %s", shortID, label, stateStyled))
	}

	bar := strings.Join(parts, " │ ")
	// 超过终端宽度时降级为不显示，确保布局稳定
	if m.width > 0 && lipgloss.Width(bar) > m.width-2 {
		return ""
	}
	return bar
}

// renderMCPBar 渲染 MCP Server 状态栏，仅在有 MCP Server 时显示。
// 格式：[MCP] context7 ● 2 tools │ other-server ✗ failed
func (m tuiModel) renderMCPBar() string {
	if len(m.mcpServers) == 0 {
		return ""
	}

	parts := make([]string, 0, len(m.mcpServers)+1)
	parts = append(parts, mcpBarBgStyle.Render("[MCP]"))

	for i, s := range m.mcpServers {
		var stateStyled string
		switch s.Status {
		case mcppkg.StatusConnected:
			stateStyled = mcpConnectedStyle.Render(fmt.Sprintf("● %d tools", s.ToolsLen))
		case mcppkg.StatusFailed:
			stateStyled = mcpFailedStyle.Render("✗ failed")
		default:
			stateStyled = mcpPendingStyle.Render("○ …")
		}
		parts = append(parts, dimStyle.Render(s.Name)+" "+stateStyled)
		if i < len(m.mcpServers)-1 {
			parts = append(parts, sepStyle.Render("│"))
		}
	}

	return strings.Join(parts, " ")
}

// mcpPanelOverhead 是 MCP 面板固定占用的行数（不含滚动内容区）：
// 2（"MCP 工具管理" + 空行）+ 1（底栏）+ 1（View() 中的状态栏行）= 4。
// View() 和 handleMCPPanelKey() 必须使用相同常量计算 contentH，保持一致。
const mcpPanelOverhead = 4

// renderMCPPanel 渲染 MCP 工具管理面板（模态）。
// contentH 是滚动内容区可用的行数，由调用方根据终端高度计算，以确保面板撑满屏幕。
func (m tuiModel) renderMCPPanel(contentH int) string {
	var sb strings.Builder
	sb.WriteString(cyanStyle.Render("MCP 工具管理") + "\n\n")

	if len(m.mcpServers) == 0 {
		sb.WriteString(dimStyle.Render("  未配置 MCP Server。") + "\n")
		sb.WriteString(dimStyle.Render("  创建 .mcp.json 后重启 harness9。") + "\n\n")
		sb.WriteString(dimStyle.Render("  e 创建配置  Esc 关闭"))
		return sb.String()
	}

	// 收集全部行，以支持滚动
	var lines []string
	for _, s := range m.mcpServers {
		switch s.Status {
		case mcppkg.StatusConnected:
			header := mcpConnectedStyle.Render("●") + " " +
				lipgloss.NewStyle().Bold(true).Render(s.Name) +
				dimStyle.Render(fmt.Sprintf("  %d 工具", s.ToolsLen))
			lines = append(lines, header)
			for _, t := range s.ToolDetails {
				lines = append(lines, "  "+cyanStyle.Render("·")+" "+dimStyle.Render(t.AdapterName))
				if t.Description != "" {
					lines = append(lines, "    "+dimStyle.Render(truncateUTF8(t.Description, 68)))
				}
			}
		case mcppkg.StatusFailed:
			header := mcpFailedStyle.Render("✗") + " " +
				lipgloss.NewStyle().Bold(true).Render(s.Name) +
				dimStyle.Render("  连接失败")
			lines = append(lines, header)
			if s.ErrMsg != "" {
				lines = append(lines, "    "+toolErrStyle.Render(truncateUTF8(s.ErrMsg, 68)))
			}
		default:
			lines = append(lines, mcpPendingStyle.Render("○")+" "+
				lipgloss.NewStyle().Bold(true).Render(s.Name)+
				dimStyle.Render("  连接中…"))
		}
		lines = append(lines, "")
	}

	// 应用滚动偏移并裁剪至可用高度
	canScroll := len(lines) > contentH
	start := m.mcpPanelScroll
	if start > len(lines) {
		start = len(lines)
	}
	end := start + contentH
	if end > len(lines) {
		end = len(lines)
	}
	for _, ln := range lines[start:end] {
		sb.WriteString(ln + "\n")
	}
	// 用空行补齐剩余内容区，使底栏始终固定在面板底部
	for i := end - start; i < contentH; i++ {
		sb.WriteByte('\n')
	}

	// 仅当内容超出可视区域时才显示滚动提示，避免内容可完整显示时误导用户
	if canScroll {
		sb.WriteString(dimStyle.Render("e 编辑配置  ↑↓/jk 滚动  Esc 关闭"))
	} else {
		sb.WriteString(dimStyle.Render("e 编辑配置  Esc 关闭"))
	}
	return sb.String()
}
