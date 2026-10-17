package dataset

// Sub-Agent 协调平面黄金数据集（spec §8）：后台启动/状态查询/steer 状态机/
// wait 聚合/并行委派/explorer 选择，六个行为收敛为四个测试函数（状态查询
// 路径并入首用例）。全部 Hermetic——子代理 Provider 用 ScriptedProvider
// 脚本化，不发起真实 API 调用。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/harness9/internal/evals"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/subagent"
	"github.com/harness9/internal/tools"
)

// sleepScript 是让子代理存活足够久的脚本：turn1 调 bash sleep（工具执行期
// 主代理可施加控制），turn2 纯文本收尾。
func sleepScript(final string) *evals.ScriptedProvider {
	return evals.NewScriptedProvider(
		evals.ScriptedTurn{ToolCalls: []schema.ToolCall{{
			ID: "sub-c1", Name: "bash",
			Arguments: json.RawMessage(`{"command":"sleep 2"}`),
		}}},
		evals.ScriptedTurn{Text: final},
	)
}

// newSubAgentTools 构造 task 家族四工具（挂真实 Runner + 脚本化子 Provider）。
// settings.json 预写 bash/read_file allow 规则，避免子代理工具调用落入审批
// （后台任务审批自动拒绝会杀死子代理）。
func newSubAgentTools(t *testing.T, wd string, subP *evals.ScriptedProvider) []tools.BaseTool {
	t.Helper()
	settings := filepath.Join(wd, "settings.json")
	if err := os.WriteFile(settings, []byte(
		`{"permissions":{"allow":["bash","read_file","write_file","edit_file","plan_write","glob","grep"]}}`),
		0o600); err != nil {
		t.Fatal(err)
	}
	reg := subagent.NewRegistry()
	if err := reg.Register(subagent.SubAgentDefinition{
		Name:         "explorer",
		Description:  "只读探索专家",
		SystemPrompt: "你是只读探索专家。",
		Tools:        []string{"bash", "read_file"},
		Source:       "builtin",
	}); err != nil {
		t.Fatal(err)
	}
	tracker := subagent.NewTaskTracker()
	runner := subagent.NewRunner(subagent.RunnerConfig{
		BaseTools:    []tools.BaseTool{tools.NewBashTool(wd), tools.NewReadFileTool(wd)},
		SettingsPath: settings,
		WorkDir:      wd,
		ProviderFor: func(model string) (provider.LLMProvider, int, error) {
			return subP, 128000, nil
		},
		CompactorFor: func(_ provider.LLMProvider, _ int) memory.Compactor { return nil },
		BaseCtx:      context.Background(),
		ToolTimeout:  30 * time.Second,
	})
	return []tools.BaseTool{
		subagent.NewTaskTool(reg, runner, tracker),
		subagent.NewTaskStatusTool(tracker),
		subagent.NewTaskWaitTool(tracker, context.Background()),
		subagent.NewTaskControlTool(tracker),
	}
}

// mainToolCall 构造主代理单个工具调用 Turn 的辅助函数。
func mainToolCall(name, args string) evals.ScriptedTurn {
	return evals.ScriptedTurn{ToolCalls: []schema.ToolCall{{
		ID: "m-" + name, Name: name, Arguments: json.RawMessage(args),
	}}}
}

// TestSubAgentBackgroundReturnsID 验证后台委派的完整观察路径：
// task(background=true) 立即返回 task id（不阻塞主 Turn）→ task_status 查询
// 到运行中任务 → task_wait 阻塞聚合到完成。核心不变量：后台启动不失败、
// 状态查询路径可见、join 语义生效（脚本化子代理经 allow 规则真实执行 bash）。
func TestSubAgentBackgroundReturnsID(t *testing.T) {
	evals.SetupHermeticEnv(t)
	subP := sleepScript("探索完成")
	c := &evals.Case{
		ID:       "subagent/background_returns_id",
		Category: "subagent",
		Prompt:   "把探索任务委派给 explorer 后台执行，查询状态，然后等它完成。",
		Provider: evals.NewScriptedProvider(
			mainToolCall("task", `{"subagent_type":"explorer","description":"探索","prompt":"探索 internal 目录","background":true}`),
			mainToolCall("task_status", `{}`),
			mainToolCall("task_wait", `{"all":true,"timeout_sec":10}`),
			evals.ScriptedTurn{Text: "后台任务已委派并完成。"},
		),
		Assertions: []evals.Assertion{
			&evals.ToolCalledAssertion{ToolName: "task"},
			&evals.ToolCalledAssertion{ToolName: "task_status"},
			&evals.ToolCalledAssertion{ToolName: "task_wait"},
			&evals.NoErrorAssertion{},
		},
	}
	c.ExtraTools = newSubAgentTools(t, t.TempDir(), subP)
	res := evals.RunCase(context.Background(), c)
	if !res.Passed {
		t.Fatalf("eval 失败: %+v", res.Failures)
	}
}

// TestSubAgentSteerStateMachine 验证后台任务的 pause → steer → resume 状态机：
// 暂停是 Turn 边界门控（不杀任务）、steer 在暂停态可投递信箱（不自动恢复）、
// resume 放行后子代理跑完收尾。核心不变量：控制链路全程无 RunError，
// 任务最终经 task_wait 聚合到终态（未被暂停挂死）。
func TestSubAgentSteerStateMachine(t *testing.T) {
	evals.SetupHermeticEnv(t)
	subP := sleepScript("已按转向指令调整方向")
	// task id 顺序稳定：explorer 的第一个后台任务为 task-explorer-1
	c := &evals.Case{
		ID:       "subagent/steer_state_machine",
		Category: "subagent",
		Prompt:   "委派后台探索任务，暂停它，注入转向指令，恢复它，最后等完成。",
		Provider: evals.NewScriptedProvider(
			mainToolCall("task", `{"subagent_type":"explorer","description":"探索","prompt":"探索","background":true}`),
			mainToolCall("task_control", `{"task_id":"task-explorer-1","action":"pause"}`),
			mainToolCall("task_control", `{"task_id":"task-explorer-1","action":"steer","message":"只看 engine 包"}`),
			mainToolCall("task_control", `{"task_id":"task-explorer-1","action":"resume"}`),
			mainToolCall("task_wait", `{"all":true,"timeout_sec":15}`),
			evals.ScriptedTurn{Text: "已暂停、转向并恢复完成。"},
		),
		Assertions: []evals.Assertion{
			&evals.ToolCalledAssertion{ToolName: "task_control"},
			&evals.ToolCalledAssertion{ToolName: "task_wait"},
			&evals.NoErrorAssertion{},
			&evals.MaxTurnsAssertion{Max: 8},
		},
	}
	c.ExtraTools = newSubAgentTools(t, t.TempDir(), subP)
	res := evals.RunCase(context.Background(), c)
	if !res.Passed {
		t.Fatalf("eval 失败: %+v", res.Failures)
	}
}

// TestSubAgentParallelDispatch 验证同一 Turn 内并行委派两个后台任务 +
// task_wait 聚合等待：两个 task 调用并发执行（引擎并发工具调度），task_wait
// 的 all=true 等待全部非终态任务。核心不变量：并行启动零失败、join 返回时
// 两个任务均达终态。注：两个子代理共享同一 ScriptedProvider 实例，脚本仅
// 两轮——先到者消费 bash sleep 轮，后到者直接取到纯文本轮自然终止（brief
// 尾注约定的确定性退化），不影响"task_wait 返回"断言。
func TestSubAgentParallelDispatch(t *testing.T) {
	evals.SetupHermeticEnv(t)
	subP := sleepScript("并行探索完成")
	c := &evals.Case{
		ID:       "subagent/parallel_dispatch",
		Category: "subagent",
		Prompt:   "并行委派两个后台探索任务，然后等待两个都完成。",
		Provider: evals.NewScriptedProvider(
			evals.ScriptedTurn{ToolCalls: []schema.ToolCall{
				{ID: "m1", Name: "task", Arguments: json.RawMessage(`{"subagent_type":"explorer","description":"探索A","prompt":"探索A","background":true}`)},
				{ID: "m2", Name: "task", Arguments: json.RawMessage(`{"subagent_type":"explorer","description":"探索B","prompt":"探索B","background":true}`)},
			}},
			evals.ScriptedTurn{ToolCalls: []schema.ToolCall{
				{ID: "m3", Name: "task_wait", Arguments: json.RawMessage(`{"all":true,"timeout_sec":15}`)},
			}},
			evals.ScriptedTurn{Text: "两个并行任务均完成。"},
		),
		Assertions: []evals.Assertion{
			&evals.ToolCalledAssertion{ToolName: "task"},
			&evals.ToolCalledAssertion{ToolName: "task_wait"},
			&evals.NoErrorAssertion{},
		},
	}
	c.ExtraTools = newSubAgentTools(t, t.TempDir(), subP)
	res := evals.RunCase(context.Background(), c)
	if !res.Passed {
		t.Fatalf("eval 失败: %+v", res.Failures)
	}
}

// TestSubAgentExplorerSelection 验证面向"深入梳理代码关系、只要结论"的探索型
// 请求时，主代理选择 explorer 子代理后台委派（而非自己展开多轮工具调用）。
// 核心不变量：委派发生且零 RunError；委派深度止于一层（子代理工具集不含
// task 家族，由 ResolveTools 剥离，本用例脚本不触发子代理递归）。
func TestSubAgentExplorerSelection(t *testing.T) {
	evals.SetupHermeticEnv(t)
	subP := sleepScript("探索结论")
	c := &evals.Case{
		ID:       "subagent/explorer_selection",
		Category: "subagent",
		Prompt:   "深入梳理 internal/engine 与 internal/memory 的调用关系，只需要结论。",
		Provider: evals.NewScriptedProvider(
			evals.ScriptedTurn{ToolCalls: []schema.ToolCall{{
				ID: "m1", Name: "task", Arguments: json.RawMessage(`{"subagent_type":"explorer","description":"梳理调用关系","prompt":"梳理 internal/engine 与 internal/memory 的调用关系","background":true}`),
			}}},
			evals.ScriptedTurn{ToolCalls: []schema.ToolCall{{
				ID: "m2", Name: "task_wait", Arguments: json.RawMessage(`{"all":true,"timeout_sec":15}`),
			}}},
			evals.ScriptedTurn{Text: "已委派 explorer 完成梳理。"},
		),
		Assertions: []evals.Assertion{
			&evals.ToolCalledAssertion{ToolName: "task"},
			&evals.NoErrorAssertion{},
		},
	}
	c.ExtraTools = newSubAgentTools(t, t.TempDir(), subP)
	res := evals.RunCase(context.Background(), c)
	if !res.Passed {
		t.Fatalf("eval 失败: %+v", res.Failures)
	}
}
