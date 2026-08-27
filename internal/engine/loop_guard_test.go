// loop_guard_test.go — loopGuard 单元测试（表驱动，脱离引擎独立验证）。
package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/harness9/internal/schema"
)

func TestGuard_CheckTurn_MaxTurns(t *testing.T) {
	g := newLoopGuard(GuardConfig{MaxTurns: 3}, time.Now())
	if err := g.CheckTurn(3); err != nil {
		t.Fatalf("turn=3 不应触发，got: %v", err)
	}
	err := g.CheckTurn(4)
	if err == nil || !strings.Contains(err.Error(), "已达最大 Turn 数 (3)") {
		t.Fatalf("期望逐字保留的措辞，got: %v", err)
	}
	reason, ok := g.Terminated()
	if !ok || reason != ReasonMaxTurns {
		t.Fatalf("期望 ReasonMaxTurns，got %q ok=%v", reason, ok)
	}
}

func TestGuard_CheckTurn_DisabledWhenZero(t *testing.T) {
	g := newLoopGuard(GuardConfig{}, time.Now())
	for i := 1; i <= 2000; i += 500 {
		if err := g.CheckTurn(i); err != nil {
			t.Fatalf("MaxTurns<=0 应不限制，turn=%d got: %v", i, err)
		}
	}
}

func TestGuard_CheckTurn_RunTimeout(t *testing.T) {
	// deadline 设在过去 → 第一次检查即超时。
	g := newLoopGuard(GuardConfig{RunTimeout: time.Hour}, time.Now().Add(-2*time.Hour))
	err := g.CheckTurn(1)
	if err == nil || !strings.Contains(err.Error(), "运行超时") {
		t.Fatalf("期望超时终止，got: %v", err)
	}
	if _, ok := g.Remaining(); !ok {
		t.Fatal("deadline 已激活时 Remaining 应返回 ok=true")
	}
	if reason, _ := g.Terminated(); reason != ReasonRunTimeout {
		t.Fatalf("期望 ReasonRunTimeout，got %q", reason)
	}
}

func TestGuard_Remaining_NotActiveWithoutTimeout(t *testing.T) {
	g := newLoopGuard(GuardConfig{}, time.Now())
	if _, ok := g.Remaining(); ok {
		t.Fatal("未配置 RunTimeout 时 Remaining 应返回 ok=false")
	}
}

func TestGuard_CheckBudget_TripAndAccumulate(t *testing.T) {
	g := newLoopGuard(GuardConfig{TokenBudget: 250}, time.Now())

	// 第一轮：实际用量 100。
	g.AddUsage(&schema.Usage{InputTokens: 100}, 90)
	if err := g.CheckBudget(); err != nil {
		t.Fatalf("100 < 250 不应触发，got: %v", err)
	}

	// 第二轮：usage 缺失 → 走估算兜底 90。
	g.AddUsage(nil, 90)
	if err := g.CheckBudget(); err != nil {
		t.Fatalf("190 < 250 不应触发，got: %v", err)
	}

	// 第三轮：实际用量 100，累计 290 >= 250 触发。
	g.AddUsage(&schema.Usage{InputTokens: 100}, 999999)
	if err := g.CheckBudget(); err == nil || !strings.Contains(err.Error(), "Token 预算 (250)") {
		t.Fatalf("期望预算触发，got: %v", err)
	}
	if reason, _ := g.Terminated(); reason != ReasonTokenBudget {
		t.Fatalf("期望 ReasonTokenBudget，got %q", reason)
	}
}

func TestGuard_CheckBudget_DisabledWhenZero(t *testing.T) {
	g := newLoopGuard(GuardConfig{}, time.Now())
	for i := 0; i < 10; i++ {
		g.AddUsage(&schema.Usage{InputTokens: 1_000_000}, 0)
	}
	if err := g.CheckBudget(); err != nil {
		t.Fatalf("TokenBudget<=0 应不限制，got: %v", err)
	}
}

// ---- Task 3：重复检测与三源仲裁 ----

func TestGuard_ComputeSignature_CanonicalizesJSON(t *testing.T) {
	a := computeSignature(schema.ToolCall{Name: "bash", Arguments: []byte(`{"command":"ls","dir":"/tmp"}`)})
	b := computeSignature(schema.ToolCall{Name: "bash", Arguments: []byte(`{ "dir": "/tmp",  "command": "ls" }`)})
	c := computeSignature(schema.ToolCall{Name: "bash", Arguments: []byte(`{"command":"ls","dir":"/etc"}`)})
	if a != b {
		t.Error("键序与空白不同的相同语义参数应有相同签名")
	}
	if a == c {
		t.Error("参数值不同必须有不同签名")
	}
	d := computeSignature(schema.ToolCall{Name: "cat", Arguments: []byte(`{"command":"ls"}`)})
	if a == d {
		t.Error("工具名不同必须有不同签名")
	}
}

func TestGuard_ComputeSignature_NonJSONFallback(t *testing.T) {
	a := computeSignature(schema.ToolCall{Name: "bash", Arguments: []byte(`not json`)})
	b := computeSignature(schema.ToolCall{Name: "bash", Arguments: []byte(`not json`)})
	if a != b {
		t.Error("非法 JSON 应退化为原始字节哈希，输入相同则签名相同")
	}
}

// sameReadCall 构造完全一致的 read_file 调用。
func sameReadCall(id string) schema.ToolCall {
	return schema.ToolCall{ID: id, Name: "read_file", Arguments: []byte(`{"path":"a.go"}`)}
}

func TestGuard_Repetition_FiresRemindThenEscalates(t *testing.T) {
	g := newLoopGuard(GuardConfig{RepetitionWindow: 10, RepetitionThreshold: 3}, time.Now())

	turn := 0
	fire := func(wantText bool, wantEscalate bool) {
		turn++
		g.RecordToolCalls(turn, []schema.ToolCall{sameReadCall(fmt.Sprintf("c%d", turn))})
		txt, err := g.EvaluateReminders(turn)
		t.Helper()
		if wantEscalate {
			if err == nil {
				t.Fatalf("turn %d: 期望升级终止", turn)
			}
			if reason, _ := g.Terminated(); reason != ReasonRepetitionLoop {
				t.Fatalf("turn %d: 期望 ReasonRepetitionLoop，got %q", turn, reason)
			}
			return
		}
		if err != nil {
			t.Fatalf("turn %d: 不期望终止，got %v", turn, err)
		}
		if wantText && txt == "" {
			t.Fatalf("turn %d: 期望注入提醒", turn)
		}
		if wantText && !strings.Contains(txt, `read_file({"path":"a.go"})`) {
			t.Fatalf("turn %d: 提醒应包含定位签名，got: %s", turn, txt)
		}
		if !wantText && txt != "" {
			t.Fatalf("turn %d: 不期望注入，got: %s", turn, txt)
		}
	}

	fire(false, false) // 第 1 次
	fire(false, false) // 第 2 次
	fire(true, false)  // 第 3 次达阈值 → 注入提醒（reminded=true）
	// 控制器裁决 #5：升级发生在"再命中且已 reminded"，即第 4 轮直接升级，
	// 一旦 escalate 就不再注入文本。
	fire(false, true) // 提醒后再命中 → 升级终止（ReasonRepetitionLoop）
}

func TestGuard_Repetition_ProgressToolBreaksCycle(t *testing.T) {
	g := newLoopGuard(GuardConfig{RepetitionWindow: 10, RepetitionThreshold: 3}, time.Now())
	turn := 0
	step := func(calls []schema.ToolCall) {
		turn++
		g.RecordToolCalls(turn, calls)
		if _, err := g.EvaluateReminders(turn); err != nil {
			t.Fatalf("进展打破后不应升级终止，turn %d: %v", turn, err)
		}
	}
	read := func() []schema.ToolCall { return []schema.ToolCall{sameReadCall(fmt.Sprintf("c%d", turn))} }
	edit := func() []schema.ToolCall {
		return []schema.ToolCall{{ID: "e", Name: "edit_file", Arguments: []byte(`{"path":"b.go"}`)}}
	}

	step(read()) // 1
	step(read()) // 2
	step(edit()) // 3 进展：清空计数
	step(read()) // 1'
	step(read()) // 2'
	turn++
	if txt, _ := g.EvaluateReminders(turn); txt != "" {
		t.Fatalf("清空后只有 2 次，不应触发提醒，got: %s", txt)
	}
}

func TestGuard_Arbitration_PriorityRepetitionOverStallOverMemory(t *testing.T) {
	g := newLoopGuard(GuardConfig{
		RepetitionWindow: 10, RepetitionThreshold: 1,
		StallWindow: 1, StallText: "[STALL]",
		MemoryInterval: 1, MemoryText: "[MEM]",
	}, time.Now())
	g.RecordToolCalls(1, []schema.ToolCall{sameReadCall("c1")})
	g.RecordToolCalls(2, []schema.ToolCall{sameReadCall("c2")})
	txt, err := g.EvaluateReminders(2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(txt, "read_file") {
		t.Fatalf("重复优先级最高，got: %s", txt)
	}
}

func TestGuard_StallReminder_FiresOncePerWindowAndResets(t *testing.T) {
	g := newLoopGuard(GuardConfig{StallWindow: 3, StallText: "[STALL]"}, time.Now())
	for i := 1; i <= 2; i++ {
		g.RecordToolCalls(i, []schema.ToolCall{sameReadCall(fmt.Sprintf("c%d", i))})
		if txt, _ := g.EvaluateReminders(i); txt != "" {
			t.Fatalf("不足窗口不应停滞提醒，turn %d got %s", i, txt)
		}
	}
	// turnsSinceProgress 由 RecordToolCalls 驱动：第 3 轮需先记录再仲裁。
	g.RecordToolCalls(3, []schema.ToolCall{sameReadCall("c3")})
	txt, _ := g.EvaluateReminders(3)
	if txt != "[STALL]" {
		t.Fatalf("第三轮应停滞提醒，got %q", txt)
	}
	g.RecordToolCalls(4, []schema.ToolCall{sameReadCall("c4")})
	if txt, _ := g.EvaluateReminders(4); txt != "" {
		t.Fatalf("重置后不应立即再提醒，got %s", txt)
	}
}

func TestGuard_MemoryReminder_PeriodicInject(t *testing.T) {
	g := newLoopGuard(GuardConfig{MemoryInterval: 2, MemoryText: "[MEM]"}, time.Now())
	for i := 1; i <= 3; i++ {
		g.RecordToolCalls(i, []schema.ToolCall{{ID: "e", Name: "edit_file", Arguments: []byte(`{}`)}})
		txt, _ := g.EvaluateReminders(i)
		want := i%2 == 0
		if want && txt != "[MEM]" {
			t.Fatalf("turn %d 应注入记忆提醒，got %q", i, txt)
		}
		if !want && txt != "" {
			t.Fatalf("turn %d 不应注入，got %q", i, txt)
		}
	}
}

func TestGuard_ReminderDisabledWhenZero(t *testing.T) {
	g := newLoopGuard(GuardConfig{}, time.Now())
	g.RecordToolCalls(1, []schema.ToolCall{sameReadCall("c1")})
	g.RecordToolCalls(2, []schema.ToolCall{sameReadCall("c2")})
	g.RecordToolCalls(3, []schema.ToolCall{sameReadCall("c3")})
	g.RecordToolCalls(4, []schema.ToolCall{sameReadCall("c4")})
	for i := 1; i <= 4; i++ {
		if txt, err := g.EvaluateReminders(i); txt != "" || err != nil {
			t.Fatalf("零值配置应全关，turn %d got (%q,%v)", i, txt, err)
		}
	}
}
