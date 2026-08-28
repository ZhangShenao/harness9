package memory_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/schema"
)

// mockSummarizer 是 memory.Summarizer 接口的可编程测试桩，
// 按调用次序返回预设响应和错误，并记录每次调用的完整 messages 参数。
// responses 和 errs 按序消费：第 N 次调用取 responses[N-1] 和 errs[N-1]（越界则取零值）。
type mockSummarizer struct {
	responses []string
	errs      []error
	calls     [][]schema.Message
}

func (m *mockSummarizer) Generate(_ context.Context, msgs []schema.Message, _ []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
	idx := len(m.calls)
	m.calls = append(m.calls, msgs)

	var callErr error
	if idx < len(m.errs) {
		callErr = m.errs[idx]
	}
	if callErr != nil {
		return nil, nil, callErr
	}

	var resp string
	if idx < len(m.responses) {
		resp = m.responses[idx]
	}
	return &schema.Message{Role: schema.RoleAssistant, Content: resp}, nil, nil
}

func newSummarizationCompactor(p memory.Summarizer, maxTokens, minTail int) *memory.SummarizationCompactor {
	return &memory.SummarizationCompactor{
		Provider:        p,
		MaxTokens:       maxTokens,
		MinTailMessages: minTail,
	}
}

// TestSummarizationCompactor_UnderBudget 验证 token 未超限时原样返回，不调用 Provider。
func TestSummarizationCompactor_UnderBudget(t *testing.T) {
	p := &mockSummarizer{responses: []string{"should not be called"}}
	c := newSummarizationCompactor(p, 100_000, 2)

	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: "hello"},
		{Role: schema.RoleAssistant, Content: "hi"},
	}
	got := c.Compact(input)

	if len(got) != 3 {
		t.Fatalf("want 3 msgs (no compaction), got %d", len(got))
	}
	if len(p.calls) != 0 {
		t.Errorf("provider should not be called when under budget, got %d calls", len(p.calls))
	}
}

// TestSummarizationCompactor_NoSystemMessage 验证首条消息不是 system 时直接返回。
func TestSummarizationCompactor_NoSystemMessage(t *testing.T) {
	p := &mockSummarizer{}
	c := newSummarizationCompactor(p, 1, 2)

	input := []schema.Message{
		{Role: schema.RoleUser, Content: longContent(1000)},
		{Role: schema.RoleAssistant, Content: longContent(1000)},
	}
	got := c.Compact(input)

	if len(got) != 2 {
		t.Fatalf("want 2 (unchanged — no system msg), got %d", len(got))
	}
	if len(p.calls) != 0 {
		t.Error("provider should not be called without system message")
	}
}

// TestSummarizationCompactor_EmptyHeadNoOp 验证非 system 消息数量 ≤ MinTailMessages 时不压缩。
func TestSummarizationCompactor_EmptyHeadNoOp(t *testing.T) {
	p := &mockSummarizer{}
	c := newSummarizationCompactor(p, 1, 4)

	// 4 non-system messages, minTail=4 → head is empty → no-op
	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: longContent(400)},
		{Role: schema.RoleAssistant, Content: longContent(400)},
		{Role: schema.RoleUser, Content: longContent(400)},
		{Role: schema.RoleAssistant, Content: longContent(400)},
	}
	got := c.Compact(input)

	if len(got) != 5 {
		t.Fatalf("want 5 (unchanged — head empty), got %d", len(got))
	}
	if len(p.calls) != 0 {
		t.Error("provider should not be called when head is empty")
	}
}

// TestSummarizationCompactor_CallsProviderAndInjectsSummary 验证超限时调用 Provider，
// 将摘要注入为单条 user 消息，并保留 tail 不变。
func TestSummarizationCompactor_CallsProviderAndInjectsSummary(t *testing.T) {
	const summaryText = "**Goal:** test"
	p := &mockSummarizer{responses: []string{summaryText}}
	c := newSummarizationCompactor(p, 1, 2)

	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: longContent(400)},      // head
		{Role: schema.RoleAssistant, Content: longContent(400)}, // head
		{Role: schema.RoleUser, Content: "recent user"},         // tail
		{Role: schema.RoleAssistant, Content: "recent asst"},    // tail
	}
	got := c.Compact(input)

	if len(p.calls) != 1 {
		t.Fatalf("expected 1 provider call, got %d", len(p.calls))
	}
	// 结果：[system, summary, tail0, tail1]
	if len(got) != 4 {
		t.Fatalf("want 4 msgs (system+summary+2tail), got %d", len(got))
	}
	if got[0].Role != schema.RoleSystem {
		t.Error("first msg must be system")
	}
	if !strings.Contains(got[1].Content, summaryText) {
		t.Errorf("summary msg should contain provider output, got: %q", got[1].Content)
	}
	if got[2].Content != "recent user" {
		t.Errorf("tail[0] should be preserved as 'recent user', got %q", got[2].Content)
	}
	if got[3].Content != "recent asst" {
		t.Errorf("tail[1] should be preserved as 'recent asst', got %q", got[3].Content)
	}
}

// TestSummarizationCompactor_FallbackOnProviderError 验证 Provider 返回错误时
// 回退到 TokenBudgetCompactor 而非崩溃或返回未压缩数据。
func TestSummarizationCompactor_FallbackOnProviderError(t *testing.T) {
	p := &mockSummarizer{errs: []error{errors.New("llm unavailable")}}
	fallback := memory.NewTokenBudgetCompactor(100)
	c := &memory.SummarizationCompactor{
		Provider:        p,
		MaxTokens:       1, // 极低预算，必触发压缩
		MinTailMessages: 2,
		Fallback:        fallback,
	}

	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "s"},
		{Role: schema.RoleUser, Content: longContent(400)},
		{Role: schema.RoleAssistant, Content: longContent(400)},
		{Role: schema.RoleUser, Content: "recent a"},
		{Role: schema.RoleAssistant, Content: "recent b"},
	}
	got := c.Compact(input)

	// 回退到 TokenBudgetCompactor 后应仍有 system + at least 2 tail
	if len(got) < 3 {
		t.Fatalf("fallback result should have at least 3 msgs, got %d", len(got))
	}
	if got[0].Role != schema.RoleSystem {
		t.Error("first msg must be system after fallback")
	}
}

// TestSummarizationCompactor_IncrementalUpdate 验证 head 中已含摘要消息时，
// 向 Provider 发送增量更新 prompt（含 <previous-summary> 标签）。
func TestSummarizationCompactor_IncrementalUpdate(t *testing.T) {
	const prevSummaryContent = "**Goal:** previous summary content"
	const newSummary = "**Goal:** updated summary"

	p := &mockSummarizer{responses: []string{newSummary}}
	c := newSummarizationCompactor(p, 1, 2)

	// head 中包含上次摘要消息
	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: "[Conversation Summary]\n" + prevSummaryContent}, // 上次摘要
		{Role: schema.RoleUser, Content: longContent(400)},                                // new head msg
		{Role: schema.RoleUser, Content: "tail1"},                                         // tail
		{Role: schema.RoleAssistant, Content: "tail2"},                                    // tail
	}
	_ = c.Compact(input)

	if len(p.calls) != 1 {
		t.Fatalf("expected 1 provider call, got %d", len(p.calls))
	}
	// 验证发送给 LLM 的 user 消息包含 <previous-summary> 标签（增量更新路径）
	promptMsgs := p.calls[0]
	var userPrompt string
	for _, m := range promptMsgs {
		if m.Role == schema.RoleUser {
			userPrompt = m.Content
		}
	}
	if !strings.Contains(userPrompt, "<previous-summary>") {
		t.Errorf("incremental update prompt should contain <previous-summary>, got: %q", userPrompt)
	}
	if !strings.Contains(userPrompt, prevSummaryContent) {
		t.Errorf("incremental update prompt should contain previous summary text")
	}
}

// TestSummarizationCompactor_OrphanedToolPairRepaired 验证压缩后孤立的工具对被修复。
func TestSummarizationCompactor_OrphanedToolPairRepaired(t *testing.T) {
	p := &mockSummarizer{responses: []string{"**Goal:** repair test"}}
	c := newSummarizationCompactor(p, 1, 2)

	// head 中包含 assistant tool_call + 对应 tool_result
	// tail 中包含一个没有 tool_result 的 tool_call → 应插入 stub
	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		// head:
		{Role: schema.RoleUser, Content: longContent(400)},
		// tail:
		{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "tc1", Name: "bash"}}},
		{Role: schema.RoleUser, Content: "tail plain"},
	}
	got := c.Compact(input)

	// tc1 的 tool_result 缺失，repairOrphanedToolPairs 应插入 stub
	resultIDs := map[string]bool{}
	for _, m := range got {
		if m.ToolCallID != "" {
			resultIDs[m.ToolCallID] = true
		}
	}
	if !resultIDs["tc1"] {
		t.Error("orphaned tool_call tc1 should have a stub tool_result inserted after compaction")
	}
}

// TestSummarizationCompactor_NilProviderFallback 验证 Provider 为 nil 时走 fallback 而非 panic。
func TestSummarizationCompactor_NilProviderFallback(t *testing.T) {
	c := &memory.SummarizationCompactor{
		Provider:        nil,
		MaxTokens:       1,
		MinTailMessages: 2,
		Fallback:        memory.NewTokenBudgetCompactor(10_000),
	}

	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: longContent(400)},
		{Role: schema.RoleAssistant, Content: longContent(400)},
		{Role: schema.RoleUser, Content: "tail a"},
		{Role: schema.RoleAssistant, Content: "tail b"},
	}
	got := c.Compact(input) // must not panic

	if got[0].Role != schema.RoleSystem {
		t.Error("first msg must be system even after nil-provider fallback")
	}
}

// TestSummarizationCompactor_NilResponseFallback 验证 Provider 返回 nil Message 时走 fallback。
func TestSummarizationCompactor_NilResponseFallback(t *testing.T) {
	// mockNilResponse 故意返回 (nil, nil, nil)
	p := &mockNilResponse{}
	c := &memory.SummarizationCompactor{
		Provider:        p,
		MaxTokens:       1,
		MinTailMessages: 2,
		Fallback:        memory.NewTokenBudgetCompactor(10_000),
	}

	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: longContent(400)},
		{Role: schema.RoleAssistant, Content: longContent(400)},
		{Role: schema.RoleUser, Content: "tail a"},
		{Role: schema.RoleAssistant, Content: "tail b"},
	}
	got := c.Compact(input) // must not panic

	if got[0].Role != schema.RoleSystem {
		t.Error("first msg must be system even after nil-response fallback")
	}
}

// mockNilResponse 模拟返回 (nil, nil, nil) 的异常 provider。
type mockNilResponse struct{}

func (m *mockNilResponse) Generate(_ context.Context, _ []schema.Message, _ []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
	return nil, nil, nil
}

// TestSummarizationCompactor_NewConstructorDefaults 验证 NewSummarizationCompactor 的默认参数。
func TestSummarizationCompactor_NewConstructorDefaults(t *testing.T) {
	p := &mockSummarizer{}
	c := memory.NewSummarizationCompactor(p, 200_000)

	if c.MaxTokens != 160_000 {
		t.Errorf("MaxTokens: want 160000 (80%% of 200000), got %d", c.MaxTokens)
	}
	if c.MinTailMessages != 6 {
		t.Errorf("MinTailMessages: want 6, got %d", c.MinTailMessages)
	}
	if c.Fallback == nil {
		t.Error("Fallback should not be nil")
	}
}

// TestSummarizationCompactor_SummaryMarkerInOutput 验证摘要消息以 summaryMarker 开头，
// 以便下次压缩时能识别并执行增量更新。
func TestSummarizationCompactor_SummaryMarkerInOutput(t *testing.T) {
	const summaryResponse = "**Goal:** do something"
	p := &mockSummarizer{responses: []string{summaryResponse}}
	c := newSummarizationCompactor(p, 1, 2)

	input := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: longContent(400)},
		{Role: schema.RoleAssistant, Content: longContent(400)},
		{Role: schema.RoleUser, Content: "t1"},
		{Role: schema.RoleAssistant, Content: "t2"},
	}
	got := c.Compact(input)

	// 找到摘要消息（system 之后的第一条）
	if len(got) < 2 {
		t.Fatal("expected at least 2 messages")
	}
	summaryMsg := got[1]
	if !strings.HasPrefix(summaryMsg.Content, "[Conversation Summary]") {
		t.Errorf("summary msg should start with marker, got: %q", summaryMsg.Content)
	}
	if !strings.Contains(summaryMsg.Content, summaryResponse) {
		t.Errorf("summary msg should contain provider response, got: %q", summaryMsg.Content)
	}
}

// mockTodoInjector 实现 memory.TodoInjector 接口。
type mockTodoInjector struct {
	text string
}

func (m *mockTodoInjector) FormatForInjection() string { return m.text }

func TestSummarizationCompactor_InjectsTodos(t *testing.T) {
	p := &mockSummarizer{responses: []string{"summary content"}}
	injector := &mockTodoInjector{text: "[>] active task\n[ ] pending task"}
	c := memory.NewSummarizationCompactor(p, 0, memory.WithTodoInjector(injector))
	c.MaxTokens = 1
	c.MinTailMessages = 1

	msgs := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: "hello hello hello hello hello hello hello"},
		{Role: schema.RoleAssistant, Content: "world world world world world world world"},
		{Role: schema.RoleUser, Content: "tail message"},
	}

	got := c.Compact(msgs)

	var summaryContent string
	for _, m := range got {
		if strings.Contains(m.Content, "[Conversation Summary]") {
			summaryContent = m.Content
			break
		}
	}
	if summaryContent == "" {
		t.Fatal("no summary message found in compacted output")
	}
	if !strings.Contains(summaryContent, "## Active Tasks") {
		t.Error("summary should contain ## Active Tasks header")
	}
	if !strings.Contains(summaryContent, "active task") {
		t.Error("summary should contain the injected todo content")
	}
}

func TestSummarizationCompactor_NilInjector_NoChange(t *testing.T) {
	p := &mockSummarizer{responses: []string{"clean summary"}}
	c := &memory.SummarizationCompactor{
		Provider:        p,
		MaxTokens:       1,
		MinTailMessages: 1,
	}
	// No injector set

	msgs := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: "hello hello hello hello hello hello hello"},
		{Role: schema.RoleAssistant, Content: "world world world world world world world"},
		{Role: schema.RoleUser, Content: "tail"},
	}

	got := c.Compact(msgs)
	for _, m := range got {
		if strings.Contains(m.Content, "## Active Tasks") {
			t.Error("no injector set, should not have Active Tasks section")
		}
	}
}

// stubSummarizer 是 memory.Summarizer 接口的简单固定响应桩，供 Task 8 测试使用。
type stubSummarizer struct{ text string }

func (s stubSummarizer) Generate(_ context.Context, _ []schema.Message, _ []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
	return &schema.Message{Role: schema.RoleAssistant, Content: s.text}, nil, nil
}

func newStubSummarizer(text string) stubSummarizer { return stubSummarizer{text: text} }

// recordingExtractor 记录 Extract 是否被调用及收到的消息数。
type recordingExtractor struct {
	called bool
	count  int
}

func (r *recordingExtractor) Extract(msgs []schema.Message) {
	r.called = true
	r.count = len(msgs)
}

func TestCompactInvokesExtractorBeforeSummarize(t *testing.T) {
	// 构造一个超出预算、需要压缩的历史。
	msgs := []schema.Message{{Role: schema.RoleSystem, Content: "sys"}}
	for i := 0; i < 20; i++ {
		msgs = append(msgs, schema.Message{Role: schema.RoleUser, Content: strings.Repeat("x", 2000)})
	}
	rec := &recordingExtractor{}
	c := memory.NewSummarizationCompactor(
		newStubSummarizer("摘要内容"),
		1000,
		memory.WithMemoryExtractor(rec),
	)
	c.Compact(msgs)
	if !rec.called {
		t.Fatal("压缩时应调用 extractor.Extract")
	}
	if rec.count == 0 {
		t.Error("extractor 应收到 head 消息")
	}
}
