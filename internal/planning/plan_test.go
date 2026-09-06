package planning_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/harness9/internal/planning"
)

func TestPlanStore_WriteAndRead(t *testing.T) {
	s := planning.NewPlanStore()
	items := []planning.PlanItem{
		{ID: "1", Content: "task one", Status: planning.PlanPending},
		{ID: "2", Content: "task two", Status: planning.PlanInProgress},
	}
	got := s.Write(items)
	if len(got) != 2 {
		t.Fatalf("Write returned %d items, want 2", len(got))
	}
	read := s.Read()
	if len(read) != 2 {
		t.Fatalf("Read returned %d items, want 2", len(read))
	}
	if read[0].ID != "1" || read[1].ID != "2" {
		t.Errorf("unexpected items: %+v", read)
	}
}

func TestPlanStore_Read_IsCopy(t *testing.T) {
	s := planning.NewPlanStore()
	s.Write([]planning.PlanItem{{ID: "1", Content: "x", Status: planning.PlanPending}})
	got := s.Read()
	got[0].Content = "mutated"
	second := s.Read()
	if second[0].Content == "mutated" {
		t.Error("Read returned a reference, not a copy")
	}
}

func TestPlanStore_WriteReplaces(t *testing.T) {
	s := planning.NewPlanStore()
	s.Write([]planning.PlanItem{{ID: "1", Content: "old", Status: planning.PlanPending}})
	s.Write([]planning.PlanItem{{ID: "2", Content: "new", Status: planning.PlanPending}})
	got := s.Read()
	if len(got) != 1 || got[0].ID != "2" {
		t.Errorf("Write did not replace: %+v", got)
	}
}

func TestPlanStore_FormatPlan_ActiveOnly(t *testing.T) {
	s := planning.NewPlanStore()
	s.Write([]planning.PlanItem{
		{ID: "1", Content: "done task", Status: planning.PlanCompleted},
		{ID: "2", Content: "active task", Status: planning.PlanInProgress},
		{ID: "3", Content: "pending task", Status: planning.PlanPending},
		{ID: "4", Content: "cancelled task", Status: planning.PlanCancelled},
	})
	out := s.FormatPlan()
	if out == "" {
		t.Fatal("expected non-empty output when active items exist")
	}
	// 注入块以权威状态标题行开头（原样注入的契约）
	if !strings.HasPrefix(out, "## 当前执行计划") {
		t.Errorf("output should start with plan header, got %q", out)
	}
	if strings.Contains(out, "done task") {
		t.Error("completed item should not appear in injection")
	}
	if strings.Contains(out, "cancelled task") {
		t.Error("cancelled item should not appear in injection")
	}
	if !strings.Contains(out, "active task") {
		t.Error("in_progress item should appear in injection")
	}
	if !strings.Contains(out, "pending task") {
		t.Error("pending item should appear in injection")
	}
}

func TestPlanStore_FormatPlan_Empty(t *testing.T) {
	s := planning.NewPlanStore()
	if got := s.FormatPlan(); got != "" {
		t.Errorf("empty store should return empty string, got %q", got)
	}
	// All completed → also empty
	s.Write([]planning.PlanItem{{ID: "1", Content: "done", Status: planning.PlanCompleted}})
	if got := s.FormatPlan(); got != "" {
		t.Errorf("all-completed store should return empty string, got %q", got)
	}
}

func TestPlanStore_ActiveCount(t *testing.T) {
	s := planning.NewPlanStore()
	s.Write([]planning.PlanItem{
		{ID: "1", Content: "a", Status: planning.PlanCompleted},
		{ID: "2", Content: "b", Status: planning.PlanInProgress},
		{ID: "3", Content: "c", Status: planning.PlanPending},
	})
	active, total := s.ActiveCount()
	if active != 2 {
		t.Errorf("active want 2, got %d", active)
	}
	if total != 3 {
		t.Errorf("total want 3, got %d", total)
	}
}

func TestPlanStore_ConcurrentWrite(t *testing.T) {
	s := planning.NewPlanStore()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			s.Write([]planning.PlanItem{{ID: "x", Content: "c", Status: planning.PlanPending}})
			s.Read()
		}(i)
	}
	wg.Wait()
}
