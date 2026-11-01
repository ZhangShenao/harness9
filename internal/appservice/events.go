package appservice

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/hooks"
	"github.com/harness9/internal/logfmt"
	"github.com/harness9/internal/permission"
	"github.com/harness9/internal/schema"
)

// Event 是 appservice 对外派发的运行时事件。
//
// Type 沿用引擎的 EventType 常量（EventActionDelta/EventThinkingDelta/...），
// 仅 EventApprovalRequired 的 Data 被替换为 ApprovalData——引擎原生的
// ApprovalRequest 携带不可序列化的 ResponseCh channel，且不应泄漏到 UI 层；
// UI 通过 RespondApproval(id) 应答，由 Service 负责送达引擎。
type Event struct {
	Type string `json:"type"`
	Turn int    `json:"turn"`
	Data any    `json:"data,omitempty"`
}

// ApprovalData 是审批请求的 UI 视图。
type ApprovalData struct {
	// ID 是本次审批的唯一标识（RespondApproval 的应答凭据）。
	ID string `json:"id"`
	// ToolName 是被审批的工具名。
	ToolName string `json:"tool_name"`
	// Args 是工具参数 JSON 文本（超长截断）。
	Args string `json:"args"`
	// Reason 是 hook 给出的审批原因。
	Reason string `json:"reason"`
	// RiskLevel 是风险等级（low/medium/high）。
	RiskLevel string `json:"risk_level"`
}

// approvalArgsMaxLen 限制审批弹窗中展示的参数文本长度。
const approvalArgsMaxLen = 2000

// truncateUTF8Safe 按 rune 边界安全截断字符串（截断点不切断多字节字符）。
func truncateUTF8Safe(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "…"
}

// pump 持续消费引擎事件通道并转发给监听者，直到通道关闭。
//
// 关键并发设计（区别于 TUI）：事件通道是无缓冲的，TUI 在审批弹窗打开期间
// 暂停读取通道，依赖"同一时刻只有一个工具会请求审批"的约定；appservice 的泵
// 与 UI 解耦——审批请求被登记到 pendingApprovals 后立即继续泵送后续事件，
// 多工具并发审批天然不死锁，UI 侧的多个审批弹窗排队展示即可。
//
// 生命周期：SendMessage 的 ctx 取消（Interrupt/Close）会让引擎结束并关闭通道，
// 泵随之退出并复位 running 状态。Cancelled run 的错误事件同样会被转发。
func (s *Service) pump(stack *SessionStack, ch <-chan engine.Event, cancel context.CancelFunc) {
	defer func() {
		cancel()
		s.mu.Lock()
		s.running = false
		s.runCancel = nil
		// 清掉本次 run 遗留的未应答审批：run 已结束，引擎侧接收者不复存在，
		// 残留条目会让 RespondApproval 误以为仍在等待（应答只会超时丢弃），
		// 也会让 PendingApprovals 恢复逻辑展示已失效的弹窗。
		s.pendingApprovals = make(map[string]*engine.ApprovalRequest)
		s.mu.Unlock()
		log.Print(logfmt.FormatMsg("appservice", "事件泵退出"))
	}()
	for evt := range ch {
		if evt.Type == engine.EventApprovalRequired {
			req, ok := evt.Data.(engine.ApprovalRequest)
			if !ok {
				continue
			}
			s.emit(Event{Type: string(evt.Type), Turn: evt.Turn, Data: s.registerApproval(req)})
			continue
		}
		s.emit(Event{Type: string(evt.Type), Turn: evt.Turn, Data: evt.Data})
	}
}

// registerApproval 登记审批请求并返回 UI 视图。
func (s *Service) registerApproval(req engine.ApprovalRequest) ApprovalData {
	s.mu.Lock()
	s.approvalSeq++
	id := fmt.Sprintf("appr_%d", s.approvalSeq)
	s.pendingApprovals[id] = &req
	s.mu.Unlock()
	log.Print(logfmt.FormatMsg("appservice", fmt.Sprintf("审批请求 %s │ tool=%s risk=%s", id, req.ToolCall.Name, req.RiskLevel)))
	return ApprovalData{
		ID:        id,
		ToolName:  req.ToolCall.Name,
		Args:      truncateUTF8Safe(string(req.ToolCall.Arguments), approvalArgsMaxLen),
		Reason:    req.Reason,
		RiskLevel: req.RiskLevel,
	}
}

// RespondApproval 把 UI 的审批决策送达引擎（每个请求恰好应答一次）。
//
// remember && approved 时，把该工具写入白名单规则文件（对标 TUI"总是允许"，
// 移植自 cmd/harness9/tui_update.go 的 writeApprovalToConfig）：bash 取命令
// 首个单词构造 glob 模式，其余工具按工具名整体放行。
func (s *Service) RespondApproval(id string, approved bool, feedback string, remember bool) error {
	s.mu.Lock()
	req, ok := s.pendingApprovals[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("审批请求不存在或已应答: %s", id)
	}
	delete(s.pendingApprovals, id)
	s.mu.Unlock()

	resp := hooks.ApprovalResponse{Approved: approved, Feedback: feedback}
	if approved && remember {
		resp.Remember = true
		s.writeApprovalPattern(req)
	}
	// 应答必须恰好一次且不阻塞泵：若 run 已被取消，引擎侧的接收者可能已退出，
	// 此时丢弃应答（goroutine 随 ctx 结束）。
	go func() {
		select {
		case req.ResponseCh <- resp:
		case <-time.After(30 * time.Second):
			log.Print(logfmt.FormatMsg("appservice", fmt.Sprintf("审批应答 %s 超时丢弃（run 可能已取消）", id)))
		case <-s.ctx.Done():
		}
	}()
	return nil
}

// PendingApprovals 返回当前待审批列表（重连/恢复 UI 时同步用）。
func (s *Service) PendingApprovals() []ApprovalData {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ApprovalData, 0, len(s.pendingApprovals))
	for id, req := range s.pendingApprovals {
		out = append(out, ApprovalData{
			ID:        id,
			ToolName:  req.ToolCall.Name,
			Args:      truncateUTF8Safe(string(req.ToolCall.Arguments), approvalArgsMaxLen),
			Reason:    req.Reason,
			RiskLevel: req.RiskLevel,
		})
	}
	return out
}

// writeApprovalPattern 把已批准的工具模式持久化到白名单规则文件。
// 移植自 TUI writeApprovalToConfig（cmd/harness9/tui_update.go:1770）。
func (s *Service) writeApprovalPattern(req *engine.ApprovalRequest) {
	settingsPath := s.shared.settingsPath
	if settingsPath == "" {
		return
	}
	rules, err := permission.LoadRules(settingsPath)
	if err != nil {
		return
	}
	var pattern string
	if req.ToolCall.Name == "bash" {
		var args struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(req.ToolCall.Arguments, &args); err == nil && args.Command != "" {
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
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		return
	}
	_ = permission.SaveRules(settingsPath, rules)
}

// HistoryItem 是会话历史渲染的统一时间线条目。
//
// 持久化的 schema.Message 是面向 LLM 的紧凑表示（工具结果独立成 user 消息），
// UI 需要"工具调用与结果合并在同一张卡片"的视图；historyToItems 负责该归并。
type HistoryItem struct {
	// Kind 是条目类型：user | assistant | tool。
	Kind string `json:"kind"`
	// Text 是消息文本（user 原文 / assistant 回复正文）。
	Text string `json:"text,omitempty"`
	// ToolCallID / ToolName / ToolArgs / ToolOutput 仅 Kind=tool 时有效。
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolArgs   string `json:"tool_args,omitempty"`
	ToolOutput string `json:"tool_output,omitempty"`
	// IsError 标记工具结果是否失败。
	IsError bool `json:"is_error,omitempty"`
	// Pending 表示该工具调用没有对应结果（上次运行被中断的残留）。
	Pending bool `json:"pending,omitempty"`
}

// historyToItems 把持久化消息转换为 UI 时间线：跳过 system，
// 工具结果（user + ToolCallID）归并进对应工具卡片。
func historyToItems(msgs []schema.Message) []HistoryItem {
	items := make([]HistoryItem, 0, len(msgs))
	toolIndex := make(map[string]int) // toolCallID -> items 下标
	for _, msg := range msgs {
		switch msg.Role {
		case schema.RoleSystem:
			continue
		case schema.RoleUser:
			if msg.ToolCallID != "" {
				if idx, ok := toolIndex[msg.ToolCallID]; ok {
					items[idx].ToolOutput = msg.Content
					items[idx].IsError = msg.IsError
					items[idx].Pending = false
				} else {
					items = append(items, HistoryItem{
						Kind: "tool", ToolCallID: msg.ToolCallID,
						ToolOutput: msg.Content, IsError: msg.IsError,
					})
				}
				continue
			}
			items = append(items, HistoryItem{Kind: "user", Text: msg.Content})
		case schema.RoleAssistant:
			if msg.Content != "" {
				items = append(items, HistoryItem{Kind: "assistant", Text: msg.Content})
			}
			for _, tc := range msg.ToolCalls {
				items = append(items, HistoryItem{
					Kind: "tool", ToolCallID: tc.ID,
					ToolName: tc.Name, ToolArgs: string(tc.Arguments),
					Pending: true,
				})
				toolIndex[tc.ID] = len(items) - 1
			}
		}
	}
	return items
}
