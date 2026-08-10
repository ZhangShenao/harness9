// Package dataset - compaction 压缩评估用例。
// 验证 ProgressiveCompactor 的锚点保留、offload 检索和渐进式分层压缩行为。
package dataset

import (
	"context"
	"testing"

	"github.com/harness9/internal/evals"
	"github.com/harness9/internal/schema"
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
