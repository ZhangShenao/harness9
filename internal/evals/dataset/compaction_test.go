// Package dataset - compaction 压缩评估用例。
// 验证 ProgressiveCompactor 的锚点保留、offload 检索和渐进式分层压缩行为，
// 以及原生规划的压缩免疫（Plan 在压缩后原样注入）。
package dataset

import (
	"context"
	"fmt"
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

// budgetRecorder 是带 token 预算判定的 Recorded 压缩器桩：超过预算时收缩为
// [system + 最近 2 条]，并记录实际发生收缩的次数。用于验证写回式压缩的核心
// 不变量——一次压缩写回后持续生效，后续轮次不重复压缩（issue #117）。
type budgetRecorder struct {
	budget      int
	shrinkCalls int
}

func (c *budgetRecorder) Compact(msgs []schema.Message) []schema.Message {
	result, _ := c.CompactWithRecord(msgs)
	return result
}

func (c *budgetRecorder) CompactWithRecord(msgs []schema.Message) ([]schema.Message, memory.CompactionRecord) {
	if memory.EstimateTokens(msgs) <= c.budget {
		return msgs, memory.CompactionRecord{}
	}
	c.shrinkCalls++
	result := append([]schema.Message{msgs[0]}, msgs[len(msgs)-2:]...)
	return result, memory.CompactionRecord{
		MsgsBefore:   len(msgs),
		MsgsAfter:    len(result),
		TokensBefore: memory.EstimateTokens(msgs),
		TokensAfter:  memory.EstimateTokens(result),
	}
}

// TestCompaction_WriteBackOnce 验证写回式压缩（issue #117）：历史过阈触发一次
// 压缩后，压缩产物写回历史与 Session——下一轮基于已收缩的历史判定，不再重复
// 压缩；LLM 看到的是写回后的压缩形态，Session 持久化无重复。
// 反向不变量（对照）：修复前"逐轮视图"模式下同一历史每轮都会重新触发压缩。
func TestCompaction_WriteBackOnce(t *testing.T) {
	evals.SetupHermeticEnv(t)

	turn := 0
	var turn2Input string
	p := providertest.NewMockWithCallback(func(msgs []schema.Message, _ []schema.ToolDefinition) schema.Message {
		turn++
		if turn == 1 {
			return schema.Message{
				Role: schema.RoleAssistant,
				ToolCalls: []schema.ToolCall{
					{ID: "c1", Name: "bash", Arguments: []byte(`{"command":"echo hi"}`)},
				},
			}
		}
		for _, m := range msgs {
			turn2Input += m.Content + "\n"
		}
		return schema.Message{Role: schema.RoleAssistant, Content: "任务完成"}
	})

	reg := tools.NewRegistry()
	if err := reg.Register(tools.NewBashTool(t.TempDir())); err != nil {
		t.Fatalf("注册工具失败: %v", err)
	}
	hookReg := hooks.NewHookRegistry(reg)

	// 预填 10 条大消息（每条 ≈600 token，总计 ≈6000 远超 3000 预算），Turn 1 必然触发压缩；
	// 收缩后仅剩 system + 最近 2 条（≈1300 token），追加本轮新消息也不会再次越阈。
	sess := memory.NewMemorySession("eval-writeback")
	seed := make([]schema.Message, 0, 10)
	for i := 0; i < 10; i++ {
		role := schema.RoleUser
		if i%2 == 1 {
			role = schema.RoleAssistant
		}
		seed = append(seed, schema.Message{Role: role,
			Content: fmt.Sprintf("OLDMSG-%02d %s", i, strings.Repeat("filler", 400))})
	}
	if err := sess.AddMessages(context.Background(), seed); err != nil {
		t.Fatalf("AddMessages: %v", err)
	}

	rec := &budgetRecorder{budget: 3000}
	eng := engine.NewAgentEngine(p, hookReg, t.TempDir(),
		engine.WithSession(sess),
		engine.WithCompactor(rec),
	)
	if err := eng.Run(context.Background(), "执行任务"); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	// 核心断言：整个 Run 只发生一次实际收缩（Turn 1 触发写回后，Turn 2 不再重压）。
	if rec.shrinkCalls != 1 {
		t.Errorf("压缩应恰好发生 1 次（写回后持续生效），实际 %d 次", rec.shrinkCalls)
	}
	// LLM 第二轮看到写回后的压缩形态：保留尾部原文，早期大消息已被收缩掉。
	if !strings.Contains(turn2Input, "OLDMSG-09") {
		t.Error("压缩视图应保留尾部消息原文")
	}
	if strings.Contains(turn2Input, "OLDMSG-00") {
		t.Error("早期旧消息应已被压缩，不应出现在 Turn 2 输入中")
	}
	// Session 持久化：压缩产物 + 本次 Run 新增消息，无重复。
	got, err := sess.GetMessages(context.Background(), 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	// [OLDMSG-09, task] + turn1 tool-call 响应 + observation + turn2 最终回复。
	if len(got) != 5 {
		t.Errorf("session 应为压缩产物 (2) + 新增消息 (3) 共 5 条，实际 %d 条", len(got))
	}
}
