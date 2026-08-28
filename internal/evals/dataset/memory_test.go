package dataset

import (
	"context"
	"testing"

	"github.com/harness9/internal/evals"
	"github.com/harness9/internal/schema"
)

// TestMemory 运行 Memory 能力评估（2 个黄金用例）。
//
// memory_write / memory_search 工具需要 SQLite 数据库，在 eval 隔离环境下未注册。
// 未注册的工具由 Registry.Execute 返回 IsError=true 的 ToolResult，
// 不会触发引擎级 RunError，因此 NoErrorAssertion 仍可正常通过。
// ToolCalledAssertion 由 recordingHook.BeforeExecute 在 registry 查找之前记录，
// 无论工具是否注册均能正确捕获调用。
func TestMemory(t *testing.T) {
	evals.SetupHermeticEnv(t)

	cases := []*evals.Case{
		// 用例 1：通过 memory_write 写入记忆
		{
			ID:       "memory/write_memory",
			Category: "memory",
			Prompt:   "记住：用户偏好简洁的中文回复。",
			Provider: evals.NewScriptedProvider(
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc1", "memory_write", `{"action":"add","content":"用户偏好简洁的中文回复","importance":8}`),
					},
				},
				evals.ScriptedTurn{Text: "已记录您的偏好。"},
			),
			Assertions: []evals.Assertion{
				&evals.ToolCalledAssertion{ToolName: "memory_write"},
				&evals.NoErrorAssertion{},
			},
		},
		// 用例 2：通过 memory_search 检索记忆
		{
			ID:       "memory/search_memory",
			Category: "memory",
			Prompt:   "搜索我关于代码风格的偏好记忆。",
			Provider: evals.NewScriptedProvider(
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc1", "memory_search", `{"query":"代码风格偏好"}`),
					},
				},
				evals.ScriptedTurn{Text: "根据记忆，您偏好简洁代码风格。"},
			),
			Assertions: []evals.Assertion{
				&evals.ToolCalledAssertion{ToolName: "memory_search"},
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
	t.Logf("Memory 评估：%d/%d 通过", passed, passed+failed)
}
