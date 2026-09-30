package subagent

import (
	"context"
	"encoding/json"
	"regexp"
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

// parseRunningTaskID 从 task 工具后台返回句柄 `<task id="..." state="running"/>` 中解析任务 id。
func parseRunningTaskID(t *testing.T, out string) string {
	t.Helper()
	m := regexp.MustCompile(`id="([^"]+)"`).FindStringSubmatch(out)
	if len(m) != 2 {
		t.Fatalf("无法从返回文本解析 task id: %q", out)
	}
	return m[1]
}

// TestTaskToolBackgroundCreatesController 验证后台启动后 tracker 中的任务
// 可被 Control（controller 已 Attach），且任务完成后进入 Done 而非 Failed。
//
// 时序设计（确定性，无竞态）：第 1 轮 LLM 调用阻塞在 proceed 上——测试侧先完成
// Control(pause) 再放行，保证 Control 必然落在非终态 Running 任务上；单轮任务
// 自然终止后不再经过第 2 轮门控，故 pause 不阻碍 Done。
func TestTaskToolBackgroundCreatesController(t *testing.T) {
	entered := make(chan struct{}) // 后台任务已进入第 1 轮 LLM 调用
	proceed := make(chan struct{}) // 测试侧完成 Control 后放行
	mock := providertest.NewMockWithCallback(func(_ []schema.Message, _ []schema.ToolDefinition) schema.Message {
		close(entered)
		<-proceed
		return schema.Message{Role: schema.RoleAssistant, Content: "bg-done"}
	})
	tt := newTaskToolForTest(t, mock)
	args, _ := json.Marshal(map[string]any{"subagent_type": "reviewer", "prompt": "x", "background": true})
	out, err := tt.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "running") {
		t.Fatalf("后台应立即返回 running 状态: %s", out)
	}
	id := parseRunningTaskID(t, out)

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("后台任务未进入第 1 轮 LLM 调用")
	}

	// controller 已在后台分支 Attach：Control 应路由成功且无错
	if err := tt.tracker.Control(id, "pause", ""); err != nil {
		t.Fatalf("Control(pause) 应无错（controller 未 Attach 或路由失败）: %v", err)
	}
	close(proceed)

	// 任务完成后终态必须是 Done（而非 Failed）
	deadline := time.After(5 * time.Second)
	for {
		d, ok := tt.tracker.Get(id)
		if ok && d.State == TaskDone {
			break
		}
		select {
		case <-deadline:
			d, _ := tt.tracker.Get(id)
			t.Fatalf("后台任务未在期限内完成，State=%v", d.State)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	d, _ := tt.tracker.Get(id)
	if d.FinalText != "bg-done" {
		t.Fatalf("FinalText=%q, want bg-done", d.FinalText)
	}
}
