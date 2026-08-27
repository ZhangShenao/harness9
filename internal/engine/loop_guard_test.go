// loop_guard_test.go — loopGuard 单元测试（表驱动，脱离引擎独立验证）。
package engine

import (
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
