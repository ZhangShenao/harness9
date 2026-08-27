// state_test.go — LoopState 合法流转表的单元测试。
package engine

import "testing"

func TestIsLegalTransition(t *testing.T) {
	tests := []struct {
		name string
		from LoopState
		to   LoopState
		want bool
	}{
		{"idle 到 turn_start", StateIdle, StateTurnStart, true},
		{"turn_start 到 compacting", StateTurnStart, StateCompacting, true},
		{"compacting 到 generating", StateCompacting, StateGenerating, true},
		{"generating 到 tool_executing", StateGenerating, StateToolExecuting, true},
		{"generating 到 done", StateGenerating, StateDone, true},
		{"generating 到 terminated", StateGenerating, StateTerminated, true},
		{"tool_executing 回到 turn_start", StateToolExecuting, StateTurnStart, true},
		{"turn_start 直接到 terminated（边界熔断）", StateTurnStart, StateTerminated, true},
		{"compacting 到 terminated（预算熔断）", StateCompacting, StateTerminated, true},
		{"空闲到生成（跳阶段，非法）", StateIdle, StateGenerating, false},
		{"turn_start 到 done（未经过 LLM 调用，非法）", StateTurnStart, StateDone, false},
		{"done 到任意态", StateDone, StateGenerating, false},
		{"terminated 复活", StateTerminated, StateTurnStart, false},
		{"同状态自环", StateGenerating, StateGenerating, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLegalTransition(tt.from, tt.to); got != tt.want {
				t.Errorf("isLegalTransition(%s, %s) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func TestTransition_RejectsIllegalAndKeepsOriginal(t *testing.T) {
	from := StateGenerating
	to := transition(from, StateTerminated, 3)
	if to != StateTerminated {
		t.Fatalf("合法流转应生效，got %s", to)
	}
	blocked := transition(from, StateIdle, 3)
	if blocked != from {
		t.Fatalf("非法流转应保持原状态，got %s", blocked)
	}
}
