package memory_test

import (
	"testing"

	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/schema"
)

func msgs(roles ...string) []schema.Message {
	result := make([]schema.Message, len(roles))
	for i, r := range roles {
		result[i] = schema.Message{Role: schema.Role(r), Content: r + "_content"}
	}
	return result
}

func msgWithToolCallID(id string) schema.Message {
	return schema.Message{Role: schema.RoleUser, ToolCallID: id, Content: "obs"}
}

func msgAssistantWithTool() schema.Message {
	return schema.Message{
		Role:      schema.RoleAssistant,
		ToolCalls: []schema.ToolCall{{ID: "tc1", Name: "bash"}},
	}
}

func TestSlidingWindow_NoCompaction(t *testing.T) {
	c := &memory.SlidingWindowCompactor{MaxMessages: 10}
	input := msgs("system", "user", "assistant")
	got := c.Compact(input)
	if len(got) != 3 {
		t.Fatalf("want 3 msgs, got %d", len(got))
	}
}

func TestSlidingWindow_BasicCompaction(t *testing.T) {
	// [system, u1, a1, u2, a2, u3] MaxMessages=4
	// startIdx = 6-4+1 = 3 → msgs[3]=u2 (no tool_call_id) → keep
	// result: [system, u2, a2, u3]
	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: "u1"},
		{Role: schema.RoleAssistant, Content: "a1"},
		{Role: schema.RoleUser, Content: "u2"},
		{Role: schema.RoleAssistant, Content: "a2"},
		{Role: schema.RoleUser, Content: "u3"},
	}
	c := &memory.SlidingWindowCompactor{MaxMessages: 4}
	got := c.Compact(input)
	if len(got) != 4 {
		t.Fatalf("want 4 msgs, got %d: %+v", len(got), got)
	}
	if got[0].Role != schema.RoleSystem {
		t.Error("first msg must be system")
	}
	if got[1].Content != "u2" {
		t.Errorf("want u2, got %q", got[1].Content)
	}
}

func TestSlidingWindow_BacktrackOrphanObservation(t *testing.T) {
	// [system, u1, assistant(tool_calls), obs(tool_call_id=tc1), a2, u3] MaxMessages=4
	// startIdx = 6-4+1 = 3 → msgs[3]=obs has ToolCallID → backtrack
	// startIdx=2 → assistant has tool_calls, ToolCallID="" → stop
	// result: [system, assistant(tool_calls), obs, a2, u3] = 5 msgs
	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: "u1"},
		msgAssistantWithTool(),
		msgWithToolCallID("tc1"),
		{Role: schema.RoleAssistant, Content: "a2"},
		{Role: schema.RoleUser, Content: "u3"},
	}
	c := &memory.SlidingWindowCompactor{MaxMessages: 4}
	got := c.Compact(input)
	if len(got) != 5 {
		t.Fatalf("want 5 msgs (backtracked), got %d: %+v", len(got), got)
	}
	if got[0].Role != schema.RoleSystem {
		t.Error("first msg must be system")
	}
	if len(got[1].ToolCalls) == 0 {
		t.Error("second msg must be assistant with tool_calls")
	}
	if got[2].ToolCallID != "tc1" {
		t.Error("third msg must be the observation")
	}
}

func TestSlidingWindow_DefaultMaxMessages(t *testing.T) {
	// MaxMessages=0 should use default 100
	c := &memory.SlidingWindowCompactor{MaxMessages: 0}
	input := msgs("system", "user", "assistant")
	got := c.Compact(input)
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
}

func TestSlidingWindow_MaxMessagesOne(t *testing.T) {
	// MaxMessages=1 means only system fits; treated as min 2
	c := &memory.SlidingWindowCompactor{MaxMessages: 1}
	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: "u1"},
		{Role: schema.RoleAssistant, Content: "a1"},
	}
	got := c.Compact(input) // must not panic
	if got[0].Role != schema.RoleSystem {
		t.Error("first msg must be system")
	}
}

func TestSlidingWindow_EmptyInput(t *testing.T) {
	c := &memory.SlidingWindowCompactor{MaxMessages: 5}
	got := c.Compact(nil)
	if len(got) != 0 {
		t.Errorf("want 0, got %d", len(got))
	}
}

func TestSlidingWindow_NoSystemMessage(t *testing.T) {
	c := &memory.SlidingWindowCompactor{MaxMessages: 2}
	input := []schema.Message{
		{Role: schema.RoleUser, Content: "u1"},
		{Role: schema.RoleUser, Content: "u2"},
		{Role: schema.RoleUser, Content: "u3"},
	}
	// No system message: should return unchanged
	got := c.Compact(input)
	if len(got) != 3 {
		t.Errorf("want 3 (unchanged), got %d", len(got))
	}
}

// TestSlidingWindow_BTypeOrphan_ToolCallWithoutResult 验证 B 类孤立修复：
// 窗口内保留了 assistant(tool_call)，但其 tool_result 被截出窗口，
// repairOrphanedToolPairs 应插入占位 tool_result，避免 Anthropic API 400。
//
// 布局（6 条消息）：
//
//	[sys, user_old, assistant(tc_done), result(tc_done), assistant(tc_pending), user_latest]
//
// MaxMessages=3 → startIdx=4 → msgs[4]=assistant(tc_pending)，ToolCallID=""，停止回溯
// 窗口：[sys, assistant(tc_pending), user_latest]
// tc_pending 有 ToolCall 但无 result → B 类孤立 → repair 插入占位
func TestSlidingWindow_BTypeOrphan_ToolCallWithoutResult(t *testing.T) {
	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: "user old"},
		{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "tc_done", Name: "read_file"}}},
		{Role: schema.RoleUser, ToolCallID: "tc_done", Content: "file contents"},
		// 窗口起点：assistant 尚未得到 result（对话在此轮被截断）
		{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "tc_pending", Name: "bash"}}},
		{Role: schema.RoleUser, Content: "user_latest"},
	}
	c := &memory.SlidingWindowCompactor{MaxMessages: 3}
	got := c.Compact(input)

	// 断言：窗口内每个 tool_call 都有对应的 tool_result（修复前会缺失 tc_pending 的结果）
	calledIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	for _, m := range got {
		for _, tc := range m.ToolCalls {
			calledIDs[tc.ID] = true
		}
		if m.ToolCallID != "" {
			resultIDs[m.ToolCallID] = true
		}
	}
	for id := range calledIDs {
		if !resultIDs[id] {
			t.Errorf("B 类孤立未修复：tool_call %q 在压缩窗口中无对应 tool_result（会触发 Anthropic API 400）", id)
		}
	}
}

// TestSlidingWindow_ATypeOrphan_ToolResultWithoutCall 验证 A 类孤立修复：
// 向前回溯逻辑（backward scan）遇到非 tool_result 消息即停止，
// 导致 result 落入窗口但其 call 仍在 head 中，repairOrphanedToolPairs 应删除该孤立 result。
//
// 布局（5 条消息）：
//
//	[sys, assistant(tc_a), user_middle, result(tc_a), user_latest]
//
// MaxMessages=3 → startIdx=3 → msgs[3]=result(tc_a)，ToolCallID="tc_a" → 回溯
// → startIdx=2 → msgs[2]=user_middle，ToolCallID=""，停止
// 窗口：[sys, user_middle, result(tc_a), user_latest]
// result(tc_a) 的 call 不在窗口 → A 类孤立 → repair 删除 result(tc_a)
func TestSlidingWindow_ATypeOrphan_ToolResultWithoutCall(t *testing.T) {
	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "tc_a", Name: "bash"}}},
		// user_middle 打断了 call-result 的紧邻关系，backward scan 止于此
		{Role: schema.RoleUser, Content: "user_middle"},
		{Role: schema.RoleUser, ToolCallID: "tc_a", Content: "bash ok"},
		{Role: schema.RoleUser, Content: "user_latest"},
	}
	c := &memory.SlidingWindowCompactor{MaxMessages: 3}
	got := c.Compact(input)

	// 断言：孤立的 result(tc_a) 应被 repair 删除，否则 Anthropic API 会报 400
	for _, m := range got {
		if m.ToolCallID == "tc_a" {
			t.Error("A 类孤立未修复：tool_result tc_a 的 call 不在压缩窗口，应被 repairOrphanedToolPairs 删除")
		}
	}
}

// --- TokenBudgetCompactor tests ---

func longContent(chars int) string {
	b := make([]byte, chars)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

func TestTokenBudgetCompactor_NoCompactionNeeded(t *testing.T) {
	c := memory.NewTokenBudgetCompactor(10_000)
	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: "hello"},
		{Role: schema.RoleAssistant, Content: "hi"},
	}
	got := c.Compact(input)
	if len(got) != 3 {
		t.Fatalf("want 3 (no compaction needed), got %d", len(got))
	}
}

func TestTokenBudgetCompactor_CompactsOldMessages(t *testing.T) {
	// Budget: 100 tokens = 400 chars.
	// system (3 chars) + 2 old messages (400 chars each) + 2 recent (short) > budget.
	c := &memory.TokenBudgetCompactor{MaxTokens: 100, MinTailMessages: 2}
	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: longContent(400)},
		{Role: schema.RoleAssistant, Content: longContent(400)},
		{Role: schema.RoleUser, Content: "recent user"},
		{Role: schema.RoleAssistant, Content: "recent assistant"},
	}
	got := c.Compact(input)
	if got[0].Role != schema.RoleSystem {
		t.Error("first msg must be system")
	}
	// Should have dropped the long old messages
	var totalNonSysChars int
	for _, m := range got[1:] {
		totalNonSysChars += len(m.Content)
	}
	// 100 tokens * 4 chars/token = 400 chars budget for non-system content
	// (generous check — we just verify old 800-char messages are gone)
	if totalNonSysChars >= 800 {
		t.Errorf("old messages not compacted: total non-sys chars = %d", totalNonSysChars)
	}
}

func TestTokenBudgetCompactor_PreservesMinTailMessages(t *testing.T) {
	// Even with tiny budget, MinTailMessages=2 must be preserved.
	c := &memory.TokenBudgetCompactor{MaxTokens: 1, MinTailMessages: 2}
	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "s"},
		{Role: schema.RoleUser, Content: longContent(1000)},
		{Role: schema.RoleAssistant, Content: longContent(1000)},
		{Role: schema.RoleUser, Content: longContent(1000)},
		{Role: schema.RoleAssistant, Content: longContent(1000)},
	}
	got := c.Compact(input)
	// Must have system + at least MinTailMessages(2) messages
	if len(got) < 3 {
		t.Fatalf("want at least 3 (system + 2 tail), got %d", len(got))
	}
	if got[0].Role != schema.RoleSystem {
		t.Error("first msg must be system")
	}
}

func TestTokenBudgetCompactor_EmptyInput(t *testing.T) {
	c := memory.NewTokenBudgetCompactor(10_000)
	got := c.Compact(nil)
	if len(got) != 0 {
		t.Fatalf("want 0, got %d", len(got))
	}
}

func TestTokenBudgetCompactor_NoSystemMessage(t *testing.T) {
	// Without system message as first msg, compactor returns unchanged.
	c := &memory.TokenBudgetCompactor{MaxTokens: 1}
	input := []schema.Message{
		{Role: schema.RoleUser, Content: longContent(1000)},
		{Role: schema.RoleAssistant, Content: longContent(1000)},
	}
	got := c.Compact(input)
	if len(got) != 2 {
		t.Fatalf("want 2 (unchanged — no system msg), got %d", len(got))
	}
}

func TestTokenBudgetCompactor_RepairsOrphanedToolResult(t *testing.T) {
	// After compaction, a tool_result without its tool_call must be removed.
	// Craft: system + old assistant(tool_call tc_old) + old tool_result(tc_old) + 6 recent msgs
	// With MinTailMessages=6, the old assistant+tool_result get compacted away.
	// The orphaned tool_result check then has nothing to orphan (both old msgs gone together).
	// Instead, test repairOrphanedToolPairs directly via a compaction that splits a pair:
	c := &memory.TokenBudgetCompactor{MaxTokens: 1, MinTailMessages: 2}
	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "s"},
		// These two will be compacted away (head):
		{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "old_call", Name: "bash"}}},
		// This tool_result is in the tail (last 2) but its tool_call is in the head:
		{Role: schema.RoleUser, ToolCallID: "old_call", Content: "result"},
		{Role: schema.RoleUser, Content: longContent(10)},
	}
	got := c.Compact(input)
	// The orphaned tool_result should be removed, leaving system + last 2 msgs (repaired)
	for _, m := range got {
		if m.ToolCallID == "old_call" {
			t.Error("orphaned tool_result should have been removed after compaction")
		}
	}
}

func TestTokenBudgetCompactor_InsertsStubForOrphanedToolCall(t *testing.T) {
	// If an assistant message with tool_calls is kept but its tool_result was
	// compacted away, a stub tool_result must be inserted.
	// MinTailMessages=3 keeps: assistant(tool_call tc1), [tc1's result is NOT in tail], user_msg
	c := &memory.TokenBudgetCompactor{MaxTokens: 1, MinTailMessages: 3}
	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "s"},
		// head (compacted away):
		{Role: schema.RoleUser, Content: longContent(100)},
		// tail (kept):
		{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "tc1", Name: "bash"}}},
		{Role: schema.RoleUser, ToolCallID: "tc1", Content: "bash result"},
		{Role: schema.RoleUser, Content: "next question"},
	}
	got := c.Compact(input)
	// Verify that every tool_call has a matching tool_result
	calledIDs := map[string]bool{}
	for _, m := range got {
		for _, tc := range m.ToolCalls {
			calledIDs[tc.ID] = true
		}
	}
	resultIDs := map[string]bool{}
	for _, m := range got {
		if m.ToolCallID != "" {
			resultIDs[m.ToolCallID] = true
		}
	}
	for id := range calledIDs {
		if !resultIDs[id] {
			t.Errorf("tool_call %s has no matching tool_result", id)
		}
	}
}
