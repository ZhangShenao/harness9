// Package dataset — Error Handling 黄金数据集
//
// 验证 Agent 的自愈能力（Self-Healing）：工具执行失败时，错误信息被回传给 LLM，
// LLM 应能根据 Observation 调整行为，而非让引擎崩溃。
package dataset

import (
	"context"
	"testing"

	"github.com/harness9/internal/evals"
	"github.com/harness9/internal/schema"
)

// TestErrorHandling 运行错误处理能力评估（3 个黄金用例）。
func TestErrorHandling(t *testing.T) {
	evals.SetupHermeticEnv(t)

	cases := []*evals.Case{
		// 用例 1：工具失败后改用替代方案。
		// 第一次 bash 调用会因命令不存在返回 IsError=true，
		// 引擎将错误作为 Observation 回传；脚本化的 LLM 随后改用 echo 命令。
		// 验证：引擎不会因工具 IsError 终止，RunError 仍为 nil。
		{
			ID:       "error_handling/bash_fallback_on_error",
			Category: "error_handling",
			Prompt:   "运行 nonexistent_cmd，如果失败则改用 echo 命令。",
			Provider: evals.NewScriptedProvider(
				// Turn 1：尝试不存在的命令（工具返回 IsError=true，引擎回传错误 Observation）
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc1", "bash", `{"command":"nonexistent_cmd"}`),
					},
				},
				// Turn 2：LLM 观察到失败，改用 echo
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc2", "bash", `{"command":"echo fallback_success"}`),
					},
				},
				evals.ScriptedTurn{Text: "第一个命令失败，已改用 echo 命令成功执行。"},
			),
			Assertions: []evals.Assertion{
				// 两次 bash 都应被调用
				&evals.ToolCalledAssertion{ToolName: "bash", MinTimes: 2},
				// 引擎本身不应报错（工具 IsError 不等于引擎 RunError）
				&evals.NoErrorAssertion{},
				&evals.MaxTurnsAssertion{Max: 4},
			},
		},

		// 用例 2：文件写入失败后优雅降级（不再重试写操作）。
		// 验证 LLM 能接受工具错误并给出说明，不陷入无限重试循环。
		{
			ID:       "error_handling/write_failure_graceful_stop",
			Category: "error_handling",
			Prompt:   "将内容写入 /root/restricted.txt（权限受限目录）。",
			Provider: evals.NewScriptedProvider(
				// Turn 1：尝试写入（实际路径在沙箱外，工具会返回错误）
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc1", "write_file", `{"path":"/root/restricted.txt","content":"test"}`),
					},
				},
				// Turn 2：LLM 观察到写入失败，给出说明，不再重试
				evals.ScriptedTurn{Text: "写入失败，目标路径权限受限，已停止操作。"},
			),
			Assertions: []evals.Assertion{
				&evals.ToolCalledAssertion{ToolName: "write_file"},
				// LLM 不应继续重试（MaxToolCallsAssertion 是 soft，只记录警告）
				&evals.MaxToolCallsAssertion{Max: 2},
				&evals.NoErrorAssertion{},
				&evals.OutputContainsAssertion{Expected: "失败"},
			},
		},

		// 用例 3：MaxTurns 保护——引擎在达到最大轮数时正常终止（不 panic）。
		// 使用 MaxTurns=3 的 Case，脚本化的 LLM 连续发起工具调用，
		// 超过限制后引擎返回 error，RunError 应非 nil（符合预期的受控终止）。
		{
			ID:       "error_handling/max_turns_protection",
			Category: "error_handling",
			Prompt:   "持续运行 ls 命令直到被停止。",
			Provider: evals.NewScriptedProvider(
				evals.ScriptedTurn{ToolCalls: []schema.ToolCall{evals.MakeToolCall("tc1", "bash", `{"command":"ls"}`)}},
				evals.ScriptedTurn{ToolCalls: []schema.ToolCall{evals.MakeToolCall("tc2", "bash", `{"command":"ls"}`)}},
				evals.ScriptedTurn{ToolCalls: []schema.ToolCall{evals.MakeToolCall("tc3", "bash", `{"command":"ls"}`)}},
				// 第 4、5 轮如果被调用说明 MaxTurns 未生效
				evals.ScriptedTurn{ToolCalls: []schema.ToolCall{evals.MakeToolCall("tc4", "bash", `{"command":"ls"}`)}},
				evals.ScriptedTurn{Text: "完成。"},
			),
			MaxTurns: 3, // 故意设置较小的 MaxTurns，验证引擎受控终止
			Assertions: []evals.Assertion{
				&evals.ToolCalledAssertion{ToolName: "bash"},
				// MaxTurns 触发时 RunError 非 nil（"已达最大 Turn 数"），这是预期行为
				&evals.ErrorAssertion{},
				// bash 至多被调用 3 次（MaxTurns=3，第 4 次不应触发）
				&evals.MaxToolCallsAssertion{Max: 3},
			},
		},
	}

	suite := &evals.Suite{Cases: cases}
	results := suite.Run(context.Background())

	passed, failed := 0, 0
	for _, r := range results {
		if r.Passed {
			passed++
			t.Logf("✅ %s (%d turns, %dms)", r.Case.ID, r.TurnCount, r.Duration.Milliseconds())
		} else {
			failed++
			t.Errorf("❌ %s", r.Case.ID)
			for _, f := range r.Failures {
				t.Errorf("   %s", f.Error())
			}
		}
		for _, w := range r.Warnings {
			t.Logf("   ⚠️ %s: %s", r.Case.ID, w.Error())
		}
	}
	t.Logf("Error Handling 评估：%d/%d 通过", passed, passed+failed)
}
