package hooks

import (
	"context"
	"testing"

	"github.com/harness9/internal/schema"
)

func TestSubAgentProgressContextRoundTrip(t *testing.T) {
	var got schema.SubAgentUpdate
	fn := func(u schema.SubAgentUpdate) { got = u }

	ctx := WithSubAgentProgress(context.Background(), fn)
	extracted := SubAgentProgressFromContext(ctx)
	if extracted == nil {
		t.Fatal("期望从 context 提取到 SubAgentProgressFunc，得到 nil")
	}
	extracted(schema.SubAgentUpdate{AgentName: "reviewer", Kind: schema.SubAgentDelta, Text: "hi"})
	if got.AgentName != "reviewer" || got.Text != "hi" {
		t.Fatalf("回调收到的更新不符：%+v", got)
	}
}

func TestSubAgentProgressFromContextEmpty(t *testing.T) {
	if SubAgentProgressFromContext(context.Background()) != nil {
		t.Fatal("空 context 应返回 nil")
	}
}
