package subagent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harness9/internal/hooks"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/provider/providertest"
	"github.com/harness9/internal/schema"
)

func newTaskToolForTest(t *testing.T, p provider.LLMProvider) *TaskTool {
	t.Helper()
	reg := NewRegistry()
	_ = reg.Register(SubAgentDefinition{Name: "reviewer", Description: "审查代码", SystemPrompt: "p"})
	runner := &Runner{
		workDir:         t.TempDir(),
		defaultMaxTurns: 5,
		providerFor: func(string) (provider.LLMProvider, int, error) {
			return p, 128_000, nil
		},
		compactorFor: func(provider.LLMProvider, int) memory.Compactor { return nil },
		baseCtx:      context.Background(),
	}
	return NewTaskTool(reg, runner, NewTaskTracker())
}

func TestTaskToolDefinitionEnumeratesAgents(t *testing.T) {
	tt := newTaskToolForTest(t, providertest.NewMock())
	def := tt.Definition()
	if def.Name != "task" {
		t.Fatalf("Name=%q", def.Name)
	}
	blob, _ := json.Marshal(def.InputSchema)
	if !strings.Contains(string(blob), "reviewer") {
		t.Errorf("schema 应枚举 reviewer: %s", blob)
	}
	if !strings.Contains(def.Description, "审查代码") {
		t.Errorf("description 应含子代理用途: %s", def.Description)
	}
}

func TestTaskToolForegroundReturnsResult(t *testing.T) {
	mock := providertest.NewMockWithCallback(func(_ []schema.Message, _ []schema.ToolDefinition) schema.Message {
		return schema.Message{Role: schema.RoleAssistant, Content: "REVIEW-DONE"}
	})
	tt := newTaskToolForTest(t, mock)
	args, _ := json.Marshal(map[string]any{
		"subagent_type": "reviewer", "description": "审查", "prompt": "看看 main.go",
	})
	out, err := tt.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "REVIEW-DONE") || !strings.Contains(out, "completed") {
		t.Fatalf("前台返回应含结果与 completed 状态: %s", out)
	}
}

func TestTaskToolUnknownAgent(t *testing.T) {
	tt := newTaskToolForTest(t, providertest.NewMock())
	args, _ := json.Marshal(map[string]any{
		"subagent_type": "ghost", "prompt": "x",
	})
	if _, err := tt.Execute(context.Background(), args); err == nil {
		t.Fatal("未知子代理类型应返回 error")
	}
}

func TestTaskToolBackgroundReturnsRunning(t *testing.T) {
	mock := providertest.NewMockWithCallback(func(_ []schema.Message, _ []schema.ToolDefinition) schema.Message {
		return schema.Message{Role: schema.RoleAssistant, Content: "bg"}
	})
	tt := newTaskToolForTest(t, mock)
	args, _ := json.Marshal(map[string]any{
		"subagent_type": "reviewer", "prompt": "x", "background": true,
	})
	out, err := tt.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "running") {
		t.Fatalf("后台应立即返回 running 状态: %s", out)
	}
}

func TestTaskToolConcurrentBackgroundUniqueIDs(t *testing.T) {
	mock := providertest.NewMockWithCallback(func(_ []schema.Message, _ []schema.ToolDefinition) schema.Message {
		return schema.Message{Role: schema.RoleAssistant, Content: "ok"}
	})
	tt := newTaskToolForTest(t, mock)
	args, _ := json.Marshal(map[string]any{"subagent_type": "reviewer", "prompt": "x", "background": true})
	const n = 8
	var wg sync.WaitGroup
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			out, err := tt.Execute(context.Background(), args)
			if err != nil {
				t.Errorf("Execute err: %v", err)
				return
			}
			ids[idx] = out
		}(i)
	}
	wg.Wait()
	deadline := time.After(5 * time.Second)
	for tt.tracker.DoneCount() < n {
		select {
		case <-deadline:
			t.Fatalf("后台任务未在期限内全部完成，Done=%d", tt.tracker.DoneCount())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	seen := map[string]bool{}
	for _, out := range ids {
		if out == "" || seen[out] {
			t.Fatalf("后台返回了空或重复的 running 句柄: %q (all=%v)", out, ids)
		}
		seen[out] = true
	}
}

// 后台安全：即使调用方 ctx 携带"父 sink"，后台也绝不调用它（避免向已关闭 channel 发送）；进度只写入 tracker。
func TestTaskToolBackgroundDoesNotUseParentSink(t *testing.T) {
	mock := providertest.NewMockWithCallback(func(_ []schema.Message, _ []schema.ToolDefinition) schema.Message {
		return schema.Message{Role: schema.RoleAssistant, Content: "bg-final"}
	})
	tt := newTaskToolForTest(t, mock)
	var parentCalls int
	parentCtx := hooks.WithSubAgentProgress(context.Background(), func(schema.SubAgentUpdate) { parentCalls++ })
	args, _ := json.Marshal(map[string]any{"subagent_type": "reviewer", "prompt": "x", "background": true})
	if _, err := tt.Execute(parentCtx, args); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for tt.tracker.DoneCount() < 1 {
		select {
		case <-deadline:
			t.Fatal("后台任务未完成")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if parentCalls != 0 {
		t.Fatalf("后台绝不应调用父 sink，实际 %d 次", parentCalls)
	}
	list := tt.tracker.List()
	if len(list) != 1 || list[0].LogLines == 0 {
		t.Fatalf("tracker 应捕获后台日志：%+v", list)
	}
}
