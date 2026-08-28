package engine_test

import (
	"context"
	"testing"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/provider/providertest"
	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/tools"
)

func TestRunLoopPersistsMessages(t *testing.T) {
	ctx := context.Background()
	sess := memory.NewMemorySession("test")
	mock := providertest.NewMockWithCallback(func(_ []schema.Message, _ []schema.ToolDefinition) schema.Message {
		return schema.Message{Role: schema.RoleAssistant, Content: "done"}
	})
	reg := tools.NewRegistry()
	eng := engine.NewAgentEngine(mock, reg, t.TempDir(),
		engine.WithSession(sess),
	)

	if err := eng.Run(ctx, "hello"); err != nil {
		t.Fatal(err)
	}

	msgs, err := sess.GetMessages(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 期望：[user:hello, assistant:done]
	if len(msgs) != 2 {
		t.Fatalf("want 2 msgs saved, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != schema.RoleUser || msgs[0].Content != "hello" {
		t.Errorf("unexpected first msg: %+v", msgs[0])
	}
	if msgs[1].Role != schema.RoleAssistant || msgs[1].Content != "done" {
		t.Errorf("unexpected second msg: %+v", msgs[1])
	}
}

func TestRunLoopLoadsHistory(t *testing.T) {
	ctx := context.Background()
	sess := memory.NewMemorySession("history")

	// 预填一条历史
	_ = sess.AddMessages(ctx, []schema.Message{
		{Role: schema.RoleUser, Content: "prev question"},
		{Role: schema.RoleAssistant, Content: "prev answer"},
	})

	var capturedHistory []schema.Message
	mock := providertest.NewMockWithCallback(func(msgs []schema.Message, _ []schema.ToolDefinition) schema.Message {
		capturedHistory = msgs
		return schema.Message{Role: schema.RoleAssistant, Content: "new answer"}
	})
	reg := tools.NewRegistry()
	eng := engine.NewAgentEngine(mock, reg, t.TempDir(),
		engine.WithSession(sess),
	)

	if err := eng.Run(ctx, "new question"); err != nil {
		t.Fatal(err)
	}

	// LLM 收到的上下文应包含：system + prev question + prev answer + new question
	if len(capturedHistory) < 4 {
		t.Fatalf("want ≥4 msgs in context, got %d: %+v", len(capturedHistory), capturedHistory)
	}
	if capturedHistory[0].Role != schema.RoleSystem {
		t.Error("first msg must be system")
	}
}

func TestRunLoopWithCompactor(t *testing.T) {
	ctx := context.Background()
	sess := memory.NewMemorySession("compact")

	// 预填 10 条历史
	var historical []schema.Message
	for i := 0; i < 10; i++ {
		historical = append(historical, schema.Message{Role: schema.RoleUser, Content: "q"})
		historical = append(historical, schema.Message{Role: schema.RoleAssistant, Content: "a"})
	}
	_ = sess.AddMessages(ctx, historical)

	var capturedLen int
	mock := providertest.NewMockWithCallback(func(msgs []schema.Message, _ []schema.ToolDefinition) schema.Message {
		capturedLen = len(msgs)
		return schema.Message{Role: schema.RoleAssistant, Content: "ok"}
	})
	reg := tools.NewRegistry()
	eng := engine.NewAgentEngine(mock, reg, t.TempDir(),
		engine.WithSession(sess),
		engine.WithCompactor(&memory.SlidingWindowCompactor{MaxMessages: 5}),
	)

	if err := eng.Run(ctx, "new"); err != nil {
		t.Fatal(err)
	}

	// MaxMessages=5: system(1) + 最多 4 条历史 + new_user → ≤6
	if capturedLen > 6 {
		t.Errorf("compactor did not apply: got %d msgs in context, want ≤6", capturedLen)
	}
}
