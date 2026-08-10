package memory_test

import (
	"strings"
	"testing"

	"github.com/harness9/internal/memory"
)

func TestParseAnchorsAndSummary_WellFormed(t *testing.T) {
	input := `## Anchors

### User Intent
Build a REST API server

### Execution Progress
- Set up project structure
- Implemented routing

### Key Decisions
- Using chi router for performance

### Tried Solutions
- Tried gin: too heavy

### Next Steps
- Add authentication
- Write tests

## Summary
The project uses Go 1.25 with chi router. Main file is cmd/server/main.go.

## Offloaded References
- .harness9/tool_results/sess1/call_1.txt (bash, 150行) - ls output`

	anchors, summary := memory.ParseAnchorsAndSummary(input)
	if len(anchors) != 5 {
		t.Fatalf("want 5 anchors, got %d", len(anchors))
	}
	types := map[string]string{}
	for _, a := range anchors {
		types[string(a.Type)] = a.Content
	}
	if !strings.Contains(types["user_intent"], "Build a REST API") {
		t.Errorf("user_intent mismatch: %q", types["user_intent"])
	}
	if !strings.Contains(types["key_decision"], "chi router") {
		t.Errorf("key_decision mismatch: %q", types["key_decision"])
	}
	if !strings.Contains(summary, "Go 1.25") {
		t.Errorf("summary should contain 'Go 1.25', got: %q", summary)
	}
}

func TestParseAnchorsAndSummary_MissingSectionsFillNA(t *testing.T) {
	input := `## Anchors

### User Intent
Do something

## Summary
Brief context.`

	anchors, summary := memory.ParseAnchorsAndSummary(input)
	if len(anchors) != 5 {
		t.Fatalf("want 5 anchors, got %d", len(anchors))
	}
	for _, a := range anchors {
		if a.Type == memory.AnchorUserIntent {
			if a.Content != "Do something" {
				t.Errorf("user_intent should be 'Do something', got %q", a.Content)
			}
		} else if a.Content != "N/A" {
			t.Errorf("missing anchor %s should be 'N/A', got %q", a.Type, a.Content)
		}
	}
	if !strings.Contains(summary, "Brief context") {
		t.Errorf("summary mismatch: %q", summary)
	}
}

func TestParseAnchorsAndSummary_Malformed(t *testing.T) {
	anchors, summary := memory.ParseAnchorsAndSummary("random text no structure")
	if len(anchors) != 5 {
		t.Fatalf("want 5 anchors (all N/A), got %d", len(anchors))
	}
	for _, a := range anchors {
		if a.Content != "N/A" {
			t.Errorf("malformed should give N/A, %s got %q", a.Type, a.Content)
		}
	}
	if summary != "" {
		t.Errorf("summary should be empty, got: %q", summary)
	}
}

func TestMergeAnchors_PreservesOldNotOverwritten(t *testing.T) {
	old := []memory.Anchor{
		{Type: memory.AnchorUserIntent, Content: "old intent"},
		{Type: memory.AnchorKeyDecision, Content: "old decision"},
	}
	newAnchors := []memory.Anchor{
		{Type: memory.AnchorUserIntent, Content: "updated intent"},
		{Type: memory.AnchorNextStep, Content: "new step"},
	}
	merged := memory.MergeAnchors(old, newAnchors)
	intentSeen := false
	decisionSeen := false
	for _, a := range merged {
		if a.Type == memory.AnchorUserIntent {
			intentSeen = true
			if a.Content != "updated intent" {
				t.Errorf("intent should be updated, got %q", a.Content)
			}
		}
		if a.Type == memory.AnchorKeyDecision {
			decisionSeen = true
			if a.Content != "old decision" {
				t.Errorf("decision should preserve old, got %q", a.Content)
			}
		}
	}
	if !intentSeen || !decisionSeen {
		t.Error("both intent and decision should be present")
	}
}
