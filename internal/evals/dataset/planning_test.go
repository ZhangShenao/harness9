package dataset

import (
	"context"
	"testing"

	"github.com/harness9/internal/evals"
	"github.com/harness9/internal/schema"
)

// TestPlanning 运行 Planning 能力评估（2 个黄金用例）。
func TestPlanning(t *testing.T) {
	evals.SetupHermeticEnv(t)

	cases := []*evals.Case{
		// 用例 1：通过 plan_write 生成计划
		{
			ID:       "planning/plan_generated",
			Category: "planning",
			Prompt:   "用 plan_write 创建一个包含 3 个步骤的实现计划。",
			Provider: evals.NewScriptedProvider(
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc1", "plan_write", `{"steps":[
							{"id":"1","content":"步骤一：读取需求","status":"pending"},
							{"id":"2","content":"步骤二：实现功能","status":"pending"},
							{"id":"3","content":"步骤三：编写测试","status":"pending"}
						]}`),
					},
				},
				evals.ScriptedTurn{Text: "已生成包含 3 个步骤的实现计划。"},
			),
			Assertions: []evals.Assertion{
				&evals.ToolCalledAssertion{ToolName: "plan_write"},
				&evals.NoErrorAssertion{},
			},
		},
		// 用例 2：探索后规划——只读收集信息并输出计划，不直接修改文件。
		// 验证"先探索后规划、不越权写文件"的行为约束（Plan Mode 已移除，
		// 该约束由 prompt 准则 + 防作弊校验承担）。
		{
			ID:       "planning/exploration_before_plan",
			Category: "planning",
			Prompt:   "分析代码并制定修改计划，不要直接修改文件。",
			Provider: evals.NewScriptedProvider(
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc1", "read_file", `{"path":"go.mod"}`),
					},
				},
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc2", "plan_write", `{"steps":[
							{"id":"1","content":"修改 go.mod 添加依赖","status":"pending"}
						]}`),
					},
				},
				evals.ScriptedTurn{Text: "分析完成，计划已制定。"},
			),
			Assertions: []evals.Assertion{
				&evals.ToolCalledAssertion{ToolName: "plan_write"},
				&evals.ToolNotCalledAssertion{ToolName: "write_file"},
				&evals.ToolNotCalledAssertion{ToolName: "edit_file"},
				&evals.NoErrorAssertion{},
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
	t.Logf("Planning 评估：%d/%d 通过", passed, passed+failed)
}

// TestPlanningExecution 验证从计划生成到执行的完整流程（2 个黄金用例）。
func TestPlanningExecution(t *testing.T) {
	evals.SetupHermeticEnv(t)

	cases := []*evals.Case{
		// 用例 3：先用 plan_write 生成计划，再写入文件执行第一个条目。
		// 验证 Planning + 执行的完整链路。
		{
			ID:       "planning/plan_then_execute",
			Category: "planning",
			Prompt:   "制定一个创建 hello.txt 的计划，然后执行第一步。",
			Provider: evals.NewScriptedProvider(
				// Turn 1：生成计划
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc1", "plan_write", `{"steps":[
							{"id":"1","content":"创建 hello.txt","status":"pending"},
							{"id":"2","content":"验证文件存在","status":"pending"}
						]}`),
					},
				},
				// Turn 2：执行第一步（写入文件）
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc2", "write_file", `{"path":"hello.txt","content":"Hello World"}`),
					},
				},
				evals.ScriptedTurn{Text: "已创建 hello.txt，第一步完成。"},
			),
			Assertions: []evals.Assertion{
				// 计划生成和执行均需触发
				&evals.ToolCalledAssertion{ToolName: "plan_write"},
				&evals.ToolCalledAssertion{ToolName: "write_file"},
				&evals.NoErrorAssertion{},
				&evals.MaxTurnsAssertion{Max: 4},
			},
		},

		// 用例 4：pure 探索模式——只用只读工具收集信息，不做任何写操作。
		// 验证 LLM 在被明确要求"只分析不修改"时遵守约束。
		{
			ID:       "planning/exploration_only",
			Category: "planning",
			Prompt:   "分析当前目录结构，只读取信息，不要修改任何文件。",
			Provider: evals.NewScriptedProvider(
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc1", "bash", `{"command":"ls -la"}`),
					},
				},
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc2", "bash", `{"command":"find . -name '*.go' | head -5"}`),
					},
				},
				evals.ScriptedTurn{Text: "目录分析完成，未做任何修改。"},
			),
			Assertions: []evals.Assertion{
				&evals.ToolCalledAssertion{ToolName: "bash", MinTimes: 2},
				// 明确不应触发写操作
				&evals.ToolNotCalledAssertion{ToolName: "write_file"},
				&evals.ToolNotCalledAssertion{ToolName: "edit_file"},
				&evals.NoErrorAssertion{},
			},
		},

		// 用例 5（新增反向用例）：简单任务不强制规划（Spec §10.2 planning/simple_task_no_plan）。
		// 验证规划是按需的原生能力：单步任务直接执行，不产生 plan_write 调用。
		{
			ID:       "planning/simple_task_no_plan",
			Category: "planning",
			Prompt:   "创建 hello.txt，内容为 Hi。",
			Provider: evals.NewScriptedProvider(
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc1", "write_file", `{"path":"hello.txt","content":"Hi"}`),
					},
				},
				evals.ScriptedTurn{Text: "已创建 hello.txt。"},
			),
			Assertions: []evals.Assertion{
				&evals.ToolNotCalledAssertion{ToolName: "plan_write"},
				&evals.ToolCalledAssertion{ToolName: "write_file"},
				&evals.NoErrorAssertion{},
			},
		},
	}

	suite := &evals.Suite{Cases: cases}
	results := suite.Run(context.Background())

	for _, r := range results {
		if r.Passed {
			t.Logf("✅ %s (%d turns, %dms)", r.Case.ID, r.TurnCount, r.Duration.Milliseconds())
		} else {
			t.Errorf("❌ %s", r.Case.ID)
			for _, f := range r.Failures {
				t.Errorf("   %s", f.Error())
			}
		}
	}
}
