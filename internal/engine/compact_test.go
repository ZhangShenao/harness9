package engine

import (
	"context"
	"testing"

	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/provider/providertest"
	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/tools"
)

// noopRegistry 是 tools.Registry 的空实现，仅用于测试。
type noopRegistry struct{}

func (noopRegistry) Register(_ tools.BaseTool) error            { return nil }
func (noopRegistry) GetAvailableTools() []schema.ToolDefinition { return nil }
func (noopRegistry) Execute(_ context.Context, _ schema.ToolCall) schema.ToolResult {
	return schema.ToolResult{}
}

// fixedCompactor 是 memory.Compactor 的简单实现：始终只保留最后 N 条消息（含 system）。
// 同时实现 RecordedCompactor 接口，返回携带 token/msg 计数的 CompactionRecord。
type fixedCompactor struct {
	keep int // 保留最近 keep 条非 system 消息
}

func (c *fixedCompactor) Compact(msgs []schema.Message) []schema.Message {
	if len(msgs) == 0 {
		return msgs
	}
	if c.keep <= 0 || len(msgs) <= c.keep+1 {
		return msgs
	}
	// 保留 system 消息 + 最近 keep 条
	result := make([]schema.Message, 0, c.keep+1)
	result = append(result, msgs[0])
	result = append(result, msgs[len(msgs)-c.keep:]...)
	return result
}

// CompactWithRecord 实现 memory.RecordedCompactor 接口，返回压缩审计记录。
func (c *fixedCompactor) CompactWithRecord(msgs []schema.Message) ([]schema.Message, memory.CompactionRecord) {
	tokensBefore := memory.EstimateTokens(msgs)
	msgsBefore := len(msgs)
	result := c.Compact(msgs)
	record := memory.CompactionRecord{
		TokensBefore: tokensBefore,
		TokensAfter:  memory.EstimateTokens(result),
		MsgsBefore:   msgsBefore,
		MsgsAfter:    len(result),
	}
	record.FillDefaults()
	return result, record
}

func newTestEngine(opts ...Option) *AgentEngine {
	return NewAgentEngine(providertest.NewMock(), noopRegistry{}, "/tmp", opts...)
}

// TestCompact_NilCompactor 验证 compactor 为 nil 时，Compact 返回零值 CompactionRecord 且不报错。
func TestCompact_NilCompactor(t *testing.T) {
	sess := memory.NewMemorySession("test-nil-compactor")
	ctx := context.Background()

	// 预填若干消息（不含 system，与真实 session 不变式一致）
	msgs := []schema.Message{
		{Role: schema.RoleUser, Content: "hello"},
		{Role: schema.RoleAssistant, Content: "world"},
	}
	if err := sess.AddMessages(ctx, msgs); err != nil {
		t.Fatalf("AddMessages: %v", err)
	}

	// compactor 为 nil（不调用 WithCompactor）
	eng := newTestEngine(WithSession(sess))

	data, err := eng.Compact(ctx)
	if err != nil {
		t.Fatalf("Compact returned unexpected error: %v", err)
	}
	// 零值 CompactionRecord：Tier=TierNone 且无 token/msg 计数
	if data.Tier != memory.TierNone || data.TokensBefore != 0 || data.MsgsBefore != 0 {
		t.Errorf("expected zero CompactionRecord, got %+v", data)
	}
	// session 历史不变
	got, err := sess.GetMessages(ctx, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(got) != len(msgs) {
		t.Errorf("session messages changed: want %d, got %d", len(msgs), len(got))
	}
}

// TestCompact_NilSession 验证 session 为 nil 时，Compact 返回零值 CompactionRecord 且不报错。
func TestCompact_NilSession(t *testing.T) {
	eng := newTestEngine(WithCompactor(&fixedCompactor{keep: 2}))

	data, err := eng.Compact(context.Background())
	if err != nil {
		t.Fatalf("Compact returned unexpected error: %v", err)
	}
	if data.Tier != memory.TierNone || data.TokensBefore != 0 || data.MsgsBefore != 0 {
		t.Errorf("expected zero CompactionRecord, got %+v", data)
	}
}

// TestCompact_Normal 验证正常执行：session 历史被替换为压缩后版本，CompactionRecord 字段正确。
//
// 与真实使用场景一致：system prompt 不持久化到 session DB，
// Compact 内部动态注入 system prompt 后交给 compactor 处理，写回时剥离 system。
// CompactionRecord 的 MsgsBefore/MsgsAfter 包含 system prompt（与 CompactWithRecord 入参一致）。
func TestCompact_Normal(t *testing.T) {
	sess := memory.NewMemorySession("test-normal-compact")
	ctx := context.Background()

	// 预填 4 条非 system 消息（真实场景中 session 不存储 system prompt）
	msgs := []schema.Message{
		{Role: schema.RoleUser, Content: "msg1"},
		{Role: schema.RoleAssistant, Content: "msg2"},
		{Role: schema.RoleUser, Content: "msg3"},
		{Role: schema.RoleAssistant, Content: "msg4"},
	}
	if err := sess.AddMessages(ctx, msgs); err != nil {
		t.Fatalf("AddMessages: %v", err)
	}

	comp := &fixedCompactor{keep: 2} // 保留 system + 最近 2 条；写回时 system 被剥离 → session 有 2 条
	eng := newTestEngine(WithSession(sess), WithCompactor(comp))

	data, err := eng.Compact(ctx)
	if err != nil {
		t.Fatalf("Compact error: %v", err)
	}

	// MsgsBefore/After 含 system prompt（CompactWithRecord 入参包含 system）
	// 4 条 session 消息 + 1 system = 5 条
	if data.MsgsBefore != 5 {
		t.Errorf("MsgsBefore: want 5 (system + 4), got %d", data.MsgsBefore)
	}
	// system + 2 条保留 = 3 条
	if data.MsgsAfter != 3 {
		t.Errorf("MsgsAfter: want 3 (system + 2), got %d", data.MsgsAfter)
	}
	// token 数应减少
	if data.TokensAfter >= data.TokensBefore {
		t.Errorf("expected TokensAfter < TokensBefore, got before=%d after=%d",
			data.TokensBefore, data.TokensAfter)
	}

	// session 写回的消息不含 system（system 由 engine 动态注入，不持久化）
	got, err := sess.GetMessages(ctx, 0)
	if err != nil {
		t.Fatalf("GetMessages after compact: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("session messages after compact: want 2, got %d", len(got))
	}
	// 第一条应为 user（msg3），而非 system
	if len(got) > 0 && got[0].Role == schema.RoleSystem {
		t.Errorf("session should not contain system message after compact")
	}
}

// TestCompact_EmptySession 验证 session 无消息时，Compact 返回零值 CompactionRecord 且不报错。
func TestCompact_EmptySession(t *testing.T) {
	sess := memory.NewMemorySession("test-empty")
	comp := &fixedCompactor{keep: 2}
	eng := newTestEngine(WithSession(sess), WithCompactor(comp))

	data, err := eng.Compact(context.Background())
	if err != nil {
		t.Fatalf("Compact error: %v", err)
	}
	if data.Tier != memory.TierNone || data.TokensBefore != 0 || data.MsgsBefore != 0 {
		t.Errorf("expected zero CompactionRecord for empty session, got %+v", data)
	}
}
