// Package dataset — Context Engineering 黄金数据集
//
// 验证 Agent 在多轮对话中的上下文连贯性：工具结果是否被观察并用于后续推理、
// 多步任务中每一步是否依赖前一步的 Observation 驱动。
package dataset

import (
	"context"
	"testing"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/evals"
	"github.com/harness9/internal/schema"
)

// TestContextEngineering 运行 Context Engineering 能力评估（3 个黄金用例）。
func TestContextEngineering(t *testing.T) {
	evals.SetupHermeticEnv(t)

	cases := []*evals.Case{
		// 用例1：多步工具调用——每步依赖上一步的 Observation。
		// 验证引擎把工具结果正确注入上下文，LLM 能够连续推理。
		{
			ID:       "context/sequential_tool_chain",
			Category: "context",
			Prompt:   "先读取 README.md，再根据读取结果用 bash 运行一条相关命令，最后总结结果。",
			Provider: evals.NewScriptedProvider(
				// Turn 1：先读文件
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc1", "read_file", `{"path":"README.md"}`),
					},
				},
				// Turn 2：根据读取结果执行 bash（模拟 LLM 观察了工具结果后继续行动）
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc2", "bash", `{"command":"echo harness9"}`),
					},
				},
				// Turn 3：输出总结
				evals.ScriptedTurn{Text: "已完成：读取了 README.md 并执行了相关命令。"},
			),
			Assertions: []evals.Assertion{
				&evals.ToolCalledAssertion{ToolName: "read_file"},
				&evals.ToolCalledAssertion{ToolName: "bash"},
				&evals.NoErrorAssertion{},
				&evals.MaxTurnsAssertion{Max: 4},
			},
		},

		// 用例2：多轮纯对话连贯性——不调用工具，但多轮回复必须保持对话语境。
		// 验证 ScriptedProvider 多轮序列在无工具场景下的正常退出。
		{
			ID:       "context/multi_turn_conversation",
			Category: "context",
			Prompt:   "介绍一下 harness9 的核心特性，然后解释它与其他框架的区别。",
			Provider: evals.NewScriptedProvider(
				evals.ScriptedTurn{Text: "harness9 的核心特性包括：标准 ReAct 循环、并发工具执行、Planning 模块和长期记忆。"},
				evals.ScriptedTurn{Text: "与其他框架相比，harness9 更简洁，代码直白，依赖极少，适合生产环境。"},
			),
			Assertions: []evals.Assertion{
				// 纯对话不应触发任何工具
				&evals.ToolNotCalledAssertion{ToolName: "bash"},
				&evals.ToolNotCalledAssertion{ToolName: "read_file"},
				&evals.ToolNotCalledAssertion{ToolName: "write_file"},
				&evals.OutputContainsAssertion{Expected: "harness9"},
				&evals.NoErrorAssertion{},
			},
		},

		// 用例3：工具输出驱动后续行为——LLM 在读取文件失败后改变策略。
		// 工具返回 IsError=true 时，引擎将错误作为 Observation 回传，
		// LLM 应能根据错误信息调整行为（此处脚本化为不再重试，转为纯文本回复）。
		{
			ID:       "context/tool_error_observation",
			Category: "context",
			Prompt:   "读取 nonexistent.txt 文件的内容。",
			Provider: evals.NewScriptedProvider(
				// Turn 1：尝试读取（实际工具会返回 IsError=true，脚本化 LLM 继续下一步）
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc1", "read_file", `{"path":"nonexistent.txt"}`),
					},
				},
				// Turn 2：LLM 观察到错误 Observation，给出说明（不再重试）
				evals.ScriptedTurn{Text: "文件 nonexistent.txt 不存在，无法读取。"},
			),
			Assertions: []evals.Assertion{
				&evals.ToolCalledAssertion{ToolName: "read_file"},
				// 文件不存在时，引擎不会崩溃，RunError 应为 nil
				&evals.NoErrorAssertion{},
				// 最终输出应提示文件不存在
				&evals.OutputContainsAssertion{Expected: "不存在"},
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
	t.Logf("Context Engineering 评估: %d/%d 通过", passed, passed+failed)
}

// TestStallNudgeKeepsReActLoopCorrect 验证：启用 WithStallReminder（连续多轮无进展工具即注入
// 一次停滞提示）后，ReAct 主循环仍正确运行——长时只读探索触发 nudge 注入，循环不被破坏，
// 工具调用序列与终止行为保持正确。这是停滞提示注入路径（ApplyReminders）的回归护栏，
// 防止 nudge 注入破坏消息结构 / 工具调用流（轨迹分析 R6 的工程化落地）。
func TestStallNudgeKeepsReActLoopCorrect(t *testing.T) {
	evals.SetupHermeticEnv(t)

	c := &evals.Case{
		ID:       "context/stall_nudge_preserves_loop",
		Category: "context",
		Prompt:   "反复探索代码库后再行动。",
		Provider: evals.NewScriptedProvider(
			// 连续 3 轮只读探索（read_file）——window=2 时第 3 轮前会注入停滞提示。
			evals.ScriptedTurn{ToolCalls: []schema.ToolCall{evals.MakeToolCall("t1", "read_file", `{"path":"a.txt"}`)}},
			evals.ScriptedTurn{ToolCalls: []schema.ToolCall{evals.MakeToolCall("t2", "read_file", `{"path":"a.txt"}`)}},
			evals.ScriptedTurn{ToolCalls: []schema.ToolCall{evals.MakeToolCall("t3", "read_file", `{"path":"a.txt"}`)}},
			// 收到提示语义后执行一条命令并收尾（ScriptedProvider 按序返回，验证循环未被破坏）。
			evals.ScriptedTurn{ToolCalls: []schema.ToolCall{evals.MakeToolCall("t4", "bash", `{"command":"echo done"}`)}},
			evals.ScriptedTurn{Text: "探索完成并已行动。"},
		),
		// 启用停滞提示，窗口 2：第 3 轮只读前注入一次提示。
		EngineOptions: []engine.Option{engine.WithStallReminder(2, "【停滞提示】请运行测试或定稿，不要继续空转。")},
		Assertions: []evals.Assertion{
			&evals.ToolCalledAssertion{ToolName: "read_file"},
			&evals.ToolCalledAssertion{ToolName: "bash"},
			&evals.NoErrorAssertion{},
		},
	}

	res := evals.RunCase(context.Background(), c)
	if !res.Passed {
		for _, f := range res.Failures {
			t.Errorf("❌ %s: %s", c.ID, f.Error())
		}
	} else {
		t.Logf("✅ %s (%d turns)", c.ID, res.TurnCount)
	}
}
