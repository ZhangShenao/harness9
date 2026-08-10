package memory_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/schema"
)

func newProgressiveCompactor(p memory.Summarizer, contextWindow int) *memory.ProgressiveCompactor {
	return memory.NewProgressiveCompactor(p, contextWindow)
}

func TestProgressiveCompactor_TierNone(t *testing.T) {
	p := &mockSummarizer{}
	c := newProgressiveCompactor(p, 100_000)
	msgs := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: "hello"},
	}
	result, record := c.CompactWithRecord(msgs)
	if len(result) != len(msgs) {
		t.Errorf("TierNone should return msgs unchanged")
	}
	if record.Tier != memory.TierNone {
		t.Errorf("want TierNone, got %d", record.Tier)
	}
	if len(p.calls) != 0 {
		t.Error("provider should not be called for TierNone")
	}
}

func TestProgressiveCompactor_TierFull_AnchorExtraction(t *testing.T) {
	anchorOutput := `## Anchors

### User Intent
Build a web server

### Execution Progress
- Set up project

### Key Decisions
- Using net/http

### Tried Solutions
- Tried gin: too heavy

### Next Steps
- Add tests

## Summary
Project uses Go 1.25 stdlib.`
	p := &mockSummarizer{responses: []string{anchorOutput}}
	c := newProgressiveCompactor(p, 2500)
	c.MinTailMessages = 2

	msgs := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: longContent(2000)},
		{Role: schema.RoleAssistant, Content: longContent(2000)},
		{Role: schema.RoleUser, Content: longContent(2000)},
		{Role: schema.RoleAssistant, Content: longContent(2000)},
		{Role: schema.RoleUser, Content: "tail1"},
		{Role: schema.RoleAssistant, Content: "tail2"},
	}

	result, record := c.CompactWithRecord(msgs)

	if record.Tier != memory.TierFull {
		t.Errorf("want TierFull, got %d", record.Tier)
	}
	if len(record.Anchors) != 5 {
		t.Errorf("want 5 anchors, got %d", len(record.Anchors))
	}
	if record.TokensAfter >= record.TokensBefore {
		t.Error("tokens should decrease after compaction")
	}
	if !strings.HasPrefix(result[1].Content, "[Context Compaction]") {
		t.Errorf("result[1] should be compaction msg")
	}
}

func TestProgressiveCompactor_TierWarn_OffloadOnly(t *testing.T) {
	p := &mockSummarizer{}
	c := newProgressiveCompactor(p, 2000)
	msgs := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: longContent(5000)},
		{Role: schema.RoleAssistant, Content: "resp"},
		{Role: schema.RoleUser, Content: "t1"},
		{Role: schema.RoleAssistant, Content: "t2"},
		{Role: schema.RoleUser, Content: "t3"},
		{Role: schema.RoleAssistant, Content: "t4"},
		{Role: schema.RoleUser, Content: "t5"},
		{Role: schema.RoleAssistant, Content: "t6"},
	}

	result, record := c.CompactWithRecord(msgs)

	if record.Tier != memory.TierWarn {
		t.Errorf("want TierWarn, got %d", record.Tier)
	}
	if len(p.calls) != 0 {
		t.Error("provider should not be called for TierWarn")
	}
	if result[0].Role != schema.RoleSystem {
		t.Error("first msg must be system")
	}
}

func TestProgressiveCompactor_LLMFailureFallsBack(t *testing.T) {
	p := &mockSummarizer{errs: []error{errors.New("llm unavailable")}}
	c := &memory.ProgressiveCompactor{
		Provider:        p,
		ContextWindow:   1000,
		MinTailMessages: 2,
		Fallback:        memory.NewTokenBudgetCompactor(1000),
	}

	msgs := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: longContent(2000)},
		{Role: schema.RoleAssistant, Content: longContent(2000)},
		{Role: schema.RoleUser, Content: "t1"},
		{Role: schema.RoleAssistant, Content: "t2"},
	}

	result, record := c.CompactWithRecord(msgs)

	if record.Error == "" {
		t.Error("record should have error set when LLM fails")
	}
	if result[0].Role != schema.RoleSystem {
		t.Error("first msg must be system after fallback")
	}
}

func TestProgressiveCompactor_IncrementalUpdate(t *testing.T) {
	anchorOutput := `## Anchors

### User Intent
Updated intent

### Execution Progress
- More progress

### Key Decisions
- N/A

### Tried Solutions
- N/A

### Next Steps
- Updated step

## Summary
Updated summary.`
	p := &mockSummarizer{responses: []string{anchorOutput}}
	c := newProgressiveCompactor(p, 2500)
	c.MinTailMessages = 2
	c.SetLastSummary("previous summary text")
	c.SetLastAnchors([]memory.Anchor{
		{Type: memory.AnchorUserIntent, Content: "old intent"},
		{Type: memory.AnchorKeyDecision, Content: "old decision"},
	})

	msgs := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: longContent(2000)},
		{Role: schema.RoleAssistant, Content: longContent(2000)},
		{Role: schema.RoleUser, Content: longContent(2000)},
		{Role: schema.RoleAssistant, Content: longContent(2000)},
		{Role: schema.RoleUser, Content: "t1"},
		{Role: schema.RoleAssistant, Content: "t2"},
	}

	c.CompactWithRecord(msgs)

	if len(p.calls) != 1 {
		t.Fatalf("expected 1 provider call, got %d", len(p.calls))
	}
	var userPrompt string
	for _, m := range p.calls[0] {
		if m.Role == schema.RoleUser {
			userPrompt = m.Content
		}
	}
	if !strings.Contains(userPrompt, "previous summary text") {
		t.Error("incremental prompt should contain previous summary")
	}
}

func TestProgressiveCompactor_TierEmergency(t *testing.T) {
	p := &mockSummarizer{}
	c := &memory.ProgressiveCompactor{
		Provider:           p,
		ContextWindow:      1000,
		MinTailMessages:    1,
		Fallback:           memory.NewTokenBudgetCompactor(100),
		EmergencyThreshold: 0.95,
	}

	msgs := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: longContent(4000)},
		{Role: schema.RoleAssistant, Content: longContent(4000)},
		{Role: schema.RoleUser, Content: "tail"},
	}

	result, record := c.CompactWithRecord(msgs)

	if record.Tier != memory.TierEmergency {
		t.Errorf("want TierEmergency, got %d", record.Tier)
	}
	if len(p.calls) != 0 {
		t.Error("provider should not be called for Emergency")
	}
	if record.Error == "" {
		t.Error("emergency should set error message")
	}
	if result[0].Role != schema.RoleSystem {
		t.Error("first msg must be system")
	}
}

func TestProgressiveCompactor_CompactBackwardCompat(t *testing.T) {
	anchorOutput := `## Anchors

### User Intent
Test

### Execution Progress
- N/A

### Key Decisions
- N/A

### Tried Solutions
- N/A

### Next Steps
- N/A

## Summary
Summary.`
	p := &mockSummarizer{responses: []string{anchorOutput}}
	c := newProgressiveCompactor(p, 2500)
	c.MinTailMessages = 2

	msgs := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: longContent(2000)},
		{Role: schema.RoleAssistant, Content: longContent(2000)},
		{Role: schema.RoleUser, Content: "t1"},
		{Role: schema.RoleAssistant, Content: "t2"},
	}

	result := c.Compact(msgs)
	if result[0].Role != schema.RoleSystem {
		t.Error("first msg must be system")
	}
}

func TestProgressiveCompactor_CompactForce(t *testing.T) {
	p := &mockSummarizer{}
	c := &memory.ProgressiveCompactor{
		Provider:        p,
		ContextWindow:   100000,
		MinTailMessages: 1,
		Fallback:        memory.NewTokenBudgetCompactor(100),
	}

	msgs := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: longContent(2000)},
		{Role: schema.RoleAssistant, Content: longContent(2000)},
		{Role: schema.RoleUser, Content: "tail"},
	}

	result := c.CompactForce(msgs)
	if result[0].Role != schema.RoleSystem {
		t.Error("first msg must be system")
	}
}

func TestProgressiveCompactor_ContextWindowZero(t *testing.T) {
	p := &mockSummarizer{}
	c := newProgressiveCompactor(p, 0)
	msgs := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: longContent(10000)},
	}
	result, record := c.CompactWithRecord(msgs)
	if record.Tier != memory.TierNone {
		t.Errorf("ContextWindow=0 should give TierNone, got %d", record.Tier)
	}
	if len(result) != len(msgs) {
		t.Error("should return unchanged")
	}
}
