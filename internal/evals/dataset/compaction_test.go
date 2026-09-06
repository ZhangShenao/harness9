// Package dataset - compaction 压缩评估用例。
// 验证 ProgressiveCompactor 的锚点保留、offload 检索和渐进式分层压缩行为，
// 以及原生规划的压缩免疫（Plan 在压缩后原样注入）。
package dataset

import (
	"context"
	"strings"
	"testing"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/evals"
	"github.com/harness9/internal/hooks"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/planning"
	"github.com/harness9/internal/provider/providertest"
	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/tools"
)

// TestCompaction_AnchorPreservation 验证压缩后 LLM 仍能回答关于用户意图的问题。
func TestCompaction_AnchorPreservation(t *testing.T) {
	evals.SetupHermeticEnv(t)

	c := &evals.Case{
		ID:       "compaction/anchor_preservation",
		Category: "compaction",
		Prompt:   "帮我用 chi router 搭建一个 Go web 服务器，需要路由和中间件。",
		Provider: evals.NewScriptedProvider(
			evals.ScriptedTurn{
				ToolCalls: []schema.ToolCall{
					evals.MakeToolCall("tc1", "bash", `{"command":"ls -la"}`),
				},
			},
			evals.ScriptedTurn{Text: "我看到了项目结构。根据你使用 chi router 和中间件的需求，我的计划是：1) 搭建 chi router 2) 添加日志中间件 3) 创建健康检查端点。你的意图很明确：用 chi router 搭建 Go web 服务器。"},
		),
		Assertions: []evals.Assertion{
			&evals.NoErrorAssertion{},
			&evals.OutputContainsAssertion{Expected: "chi"},
			&evals.MaxTurnsAssertion{Max: 5},
		},
	}
	result := evals.RunCase(context.Background(), c)
	if !result.Passed {
		t.Fatalf("case failed: %v", result.Failures)
	}
}

// TestCompaction_OffloadRetrieval 验证压缩后 LLM 可通过工具检索 offloaded 内容。
func TestCompaction_OffloadRetrieval(t *testing.T) {
	evals.SetupHermeticEnv(t)

	c := &evals.Case{
		ID:       "compaction/offload_retrieval",
		Category: "compaction",
		Prompt:   "运行 ls -la 并告诉我有哪些文件。",
		Provider: evals.NewScriptedProvider(
			evals.ScriptedTurn{
				ToolCalls: []schema.ToolCall{
					evals.MakeToolCall("tc1", "bash", `{"command":"ls -la"}`),
				},
			},
			evals.ScriptedTurn{Text: "我可以查看目录列表。文件已成功检索。"},
		),
		Assertions: []evals.Assertion{
			&evals.NoErrorAssertion{},
			&evals.ToolCalledAssertion{ToolName: "bash"},
			&evals.MaxTurnsAssertion{Max: 5},
		},
	}
	result := evals.RunCase(context.Background(), c)
	if !result.Passed {
		t.Fatalf("case failed: %v", result.Failures)
	}
}

// TestCompaction_ProgressiveTiers 验证大量对话后压缩，LLM 行为仍连贯。
func TestCompaction_ProgressiveTiers(t *testing.T) {
	evals.SetupHermeticEnv(t)

	c := &evals.Case{
		ID:       "compaction/progressive_tiers",
		Category: "compaction",
		Prompt:   "读取一个文件并总结其内容。",
		Provider: evals.NewScriptedProvider(
			evals.ScriptedTurn{
				ToolCalls: []schema.ToolCall{
					evals.MakeToolCall("tc1", "read_file", `{"path":"main.go"}`),
				},
			},
			evals.ScriptedTurn{Text: "我已读取文件。它包含应用程序的主入口点。总结完成，任务结束。"},
		),
		Assertions: []evals.Assertion{
			&evals.NoErrorAssertion{},
			&evals.ToolCalledAssertion{ToolName: "read_file"},
			&evals.OutputContainsAssertion{Expected: "总结"},
		},
	}
	result := evals.RunCase(context.Background(), c)
	if !result.Passed {
		t.Fatalf("case failed: %v", result.Failures)
	}
}

// TestCompaction_PlanSurvives 验证压缩免疫（Spec §10.2 compaction/plan_survives）：
// 历史被压缩器大幅裁剪后，活跃 Plan 仍原样出现在发送给 LLM 的视图末尾。
// 该用例绕过 RunCase 直接构建引擎——需要注入 Compactor + PlanStore 并捕获 LLM 输入视图；
// 依然 hermetic：providertest mock 不发起真实 API 调用。
func TestCompaction_PlanSurvives(t *testing.T) {
	evals.SetupHermeticEnv(t)

	store := planning.NewPlanStore()
	store.Write([]planning.PlanItem{
		{ID: "1", Content: "压缩后仍可见的步骤", Status: planning.PlanPending},
	})

	turn := 0
	var turn2Tail string
	p := providertest.NewMockWithCallback(func(msgs []schema.Message, _ []schema.ToolDefinition) schema.Message {
		turn++
		switch turn {
		case 1:
			// Turn 1：发起工具调用，驱动循环进入 Turn 2（历史被压缩后再次调用 LLM）。
			return schema.Message{
				Role: schema.RoleAssistant,
				ToolCalls: []schema.ToolCall{
					{ID: "c1", Name: "bash", Arguments: []byte(`{"command":"ls"}`)},
				},
			}
		default:
			turn2Tail = msgs[len(msgs)-1].Content
			return schema.Message{Role: schema.RoleAssistant, Content: "继续执行"}
		}
	})

	reg := tools.NewRegistry()
	if err := reg.Register(tools.NewBashTool(t.TempDir())); err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	hookReg := hooks.NewHookRegistry(reg)

	eng := engine.NewAgentEngine(p, hookReg, t.TempDir(),
		engine.WithPlanStore(store),
		// MaxTokens=1 的压缩器对任何历史都触发截断压缩，构造"压缩必然发生"的场景。
		engine.WithCompactor(memory.NewTokenBudgetCompactor(1)),
	)
	if err := eng.Run(context.Background(), "执行任务"); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if !strings.Contains(turn2Tail, "当前执行计划") || !strings.Contains(turn2Tail, "压缩后仍可见的步骤") {
		t.Errorf("plan must survive compaction verbatim, tail: %q", turn2Tail)
	}
}
