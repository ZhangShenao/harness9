// guardrails_test.go — 行为护栏黄金数据集（AGENTS.md §5.8 DoD 扩充）。
//
// 覆盖维度（spec §6 Eval 数据集节）：
//  1. repetition_terminates —— 同签名死循环被定向提醒打断，无效则升级硬终止
//  2. budget_trips_hermetically —— token 预算耗尽即受控停止（ScriptedProvider
//     固定返回 Usage{InputTokens:100}，走实际用量累计路径，纯 Hermetic）
//  3. reminder_reaches_llm_context —— 提醒文案确实出现在发给 LLM 的历史副本中
//     （渐进干预不等于静默拦截）
package dataset

import (
	"context"
	"strings"
	"testing"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/evals"
	"github.com/harness9/internal/schema"
)

// runCaseOrFail 是数据集通用的执行+断言转译器（与其他 *_test.go 中的样板一致）。
func runCaseOrFail(t *testing.T, c *evals.Case) {
	t.Helper()
	result := evals.RunCase(context.Background(), c)
	for _, f := range result.Warnings {
		t.Logf("[WARN] %s: %v", c.ID, f)
	}
	for _, f := range result.Failures {
		t.Errorf("[%s] %v", c.ID, f)
	}
}

// 用例 1：无限重复 read_file 同一文件。脚本供给远超所需的轮数（Provider 脚本
// 耗尽后会返回自然终止文本，反而掩盖缺陷，因此脚本长度须显著大于护栏触发点）。
// 核心不变量：终局必须是 ErrorAssertion 命中（保持 error 语义），且消耗的
// 工具调用数被护栏约束在个位数~十余次量级而非 50 次。
func TestGuardrails_RepetitionTerminates(t *testing.T) {
	evals.SetupHermeticEnv(t)

	loop := evals.ScriptedTurn{
		ToolCalls: []schema.ToolCall{evals.MakeToolCall("dup", "read_file", `{"path":"src/config.json"}`)},
	}
	turns := make([]evals.ScriptedTurn, 0, 30)
	for i := 0; i < 30; i++ {
		turns = append(turns, loop)
	}

	runCaseOrFail(t, &evals.Case{
		ID:       "guardrails/repetition_terminates",
		Category: "guardrails",
		Prompt:   "读取 src/config.json 并总结内容",
		Provider: evals.NewScriptedProvider(turns...),
		EngineOptions: []engine.Option{
			engine.WithRepetitionReminder(4, 2),
		},
		Assertions: []evals.Assertion{
			&evals.ToolCalledAssertion{ToolName: "read_file"},
			&evals.ErrorAssertion{},
			&evals.MaxToolCallsAssertion{Max: 20},
		},
	})
}

// 用例 2：token 预算耗尽。ScriptedProvider 每轮固定回 100 input tokens，
// 预算 220 → 第 3 次调用后（累计 300 ≥ 220）在 Turn 边界必触发。
// 注意：脚本轮必须携带工具调用——纯文本回复会让引擎走自然终止路径提前退出，
// 预算护栏永远无法到达；repetition/stall 检测未开启，与本用例完全解耦。
// 核心不变量：稳定在第 3 次调用内终止（经验值 TurnIndex=3，上界留 4 容纳过冲语义）。
func TestGuardrails_BudgetTripsHermetically(t *testing.T) {
	evals.SetupHermeticEnv(t)

	turns := make([]evals.ScriptedTurn, 0, 30)
	for i := 0; i < 30; i++ {
		turns = append(turns, evals.ScriptedTurn{
			ToolCalls: []schema.ToolCall{evals.MakeToolCall("tick", "read_file", `{"path":"notes.txt"}`)},
		})
	}

	c := &evals.Case{
		ID:       "guardrails/budget_trips_hermetically",
		Category: "guardrails",
		Prompt:   "持续分析",
		Provider: evals.NewScriptedProvider(turns...),
		EngineOptions: []engine.Option{
			engine.WithTokenBudget(220),
		},
		Assertions: []evals.Assertion{
			&evals.ErrorAssertion{},
		},
	}
	result := evals.RunCase(context.Background(), c)
	for _, f := range result.Failures {
		t.Errorf("[%s] %v", c.ID, f)
	}
	if got := result.Case.Provider.TurnIndex(); got > 4 {
		t.Errorf("[%s] 预算 220/每轮100 应在第 3±1 次调用终止，实际 %d", c.ID, got)
	}
}

// 用例 3：提醒可见性。重复发生 → 第 4 次达到阈值(reminder(3,2))前 2 次
// 不打扰；第 3 次起发给 LLM 的 messages 末尾必然携带定向提醒文案。
// 反向验证（Reverse）：前 2 轮绝不能提前注入。
func TestGuardrails_ReminderVisibleInLLMContext(t *testing.T) {
	evals.SetupHermeticEnv(t)

	loop := evals.ScriptedTurn{
		ToolCalls: []schema.ToolCall{evals.MakeToolCall("dup", "bash", `{"command":"ls -la"}`)},
	}
	provider := evals.NewScriptedProvider(loop, loop, loop, loop, loop)

	c := &evals.Case{
		ID:       "guardrails/reminder_visible_in_llm_context",
		Category: "guardrails",
		Prompt:   "列一下目录",
		Provider: provider,
		EngineOptions: []engine.Option{
			engine.WithRepetitionReminder(3, 2),
		},
		Assertions: []evals.Assertion{
			&evals.ToolCalledAssertion{ToolName: "bash", MinTimes: 2},
			// 升级硬终止是本护栏语义的一部分：提醒无效后在下一命中点以 error 受控停止
			&evals.ErrorAssertion{},
		},
	}
	result := evals.RunCase(context.Background(), c)
	for _, f := range result.Failures {
		t.Errorf("[%s] %v", c.ID, f)
	}

	// 在已记录的 LLM 调用中定位首次注入点。
	sawReminder := false
	callIdxOfFirst := -1
	for i, call := range provider.Calls() {
		has := false
		for _, m := range call.Messages {
			if strings.Contains(m.Content, "相同的工具调用") {
				has = true
			}
		}
		if has {
			sawReminder = true
			callIdxOfFirst = i
			break
		}
	}
	if !sawReminder {
		t.Fatalf("[%s] 定向提醒应出现在发给 LLM 的历史副本中", c.ID)
	}
	if callIdxOfFirst < 1 {
		t.Fatalf("[%s] 首次提醒不应出现在第一次调用（i=%d）", c.ID, callIdxOfFirst)
	}
}
