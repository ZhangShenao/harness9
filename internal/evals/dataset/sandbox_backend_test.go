package dataset

// Seatbelt（macOS 原生沙箱）行为用例：通过 Case.ToolEnvironment 把真实
// sandbox-exec 环境注入最小化引擎，验证沙箱后端对 Agent 工具执行的
// 放行（workspace-write）与拒绝（read-only）两个方向——AGENTS.md §5.8
// 要求新后端必须补行为 eval，仅测 profile 构造不够。
//
// 断言设计说明：harness 的 OutputContains 断言检查的是【脚本化的最终文本】
// 而非工具输出；沙箱约束是文件系统级事实，因此这里用副作用断言
// （文件是否被真实写入）代替字符串匹配，杜绝"只改 prompt 话术"的假通过。
// 非 darwin 或缺少 sandbox-exec 时整体跳过（与 sandbox 包冒烟测试同一守卫）。

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/harness9/internal/evals"
	"github.com/harness9/internal/sandbox"
	"github.com/harness9/internal/schema"
)

func skipNoSeatbelt(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt 仅在 macOS 上可用")
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec 不可用")
	}
}

// TestSeatbeltWorkspaceWriteAllowsWorkspace 正向用例：workspace-write 档下，
// 工作区内的写入被沙箱放行，bash 工具链路端到端真实落盘。
// 核心不变量：沙箱不能误伤合法操作。
func TestSeatbeltWorkspaceWriteAllowsWorkspace(t *testing.T) {
	evals.SetupHermeticEnv(t)
	skipNoSeatbelt(t)

	workDir := t.TempDir()
	env := sandbox.NewSeatbeltEnvironment(workDir, sandbox.SeatbeltWorkspaceWrite)

	c := &evals.Case{
		ID:              "sandbox_backend/seatbelt_workspace_write",
		Category:        "sandbox_backend",
		Prompt:          "在工作目录创建 inside.txt，内容为 seatbelt-ok。",
		ToolEnvironment: env,
		WorkDir:         workDir,
		Provider: evals.NewScriptedProvider(
			evals.ScriptedTurn{
				ToolCalls: []schema.ToolCall{
					evals.MakeToolCall("sb1", "bash", `{"command":"printf seatbelt-ok > inside.txt"}`),
				},
			},
			evals.ScriptedTurn{Text: "已创建 inside.txt。"},
		),
		Assertions: []evals.Assertion{
			&evals.ToolCalledAssertion{ToolName: "bash"},
			&evals.NoErrorAssertion{},
			&evals.MaxTurnsAssertion{Max: 3},
		},
	}
	if r := evals.RunCase(t.Context(), c); !r.Passed {
		t.Fatalf("workspace-write 应完整执行：%+v", r)
	}
	// 行为断言：文件必须真实写入且内容正确（证明沙箱放行了合法写入）
	data, err := os.ReadFile(filepath.Join(workDir, "inside.txt"))
	if err != nil {
		t.Fatalf("工作区文件应被真实写入: %v", err)
	}
	if string(data) != "seatbelt-ok" {
		t.Fatalf("文件内容不符: %q", string(data))
	}
}

// TestSeatbeltReadOnlyDeniesWrite 反向用例：read-only 档下，工作区写入被
// 内核级拒绝，文件不得真实落盘。
// 核心不变量：沙箱约束真实生效（拒绝是文件系统事实，不是话术）。
func TestSeatbeltReadOnlyDeniesWrite(t *testing.T) {
	evals.SetupHermeticEnv(t)
	skipNoSeatbelt(t)

	workDir := t.TempDir()
	env := sandbox.NewSeatbeltEnvironment(workDir, sandbox.SeatbeltReadOnly)

	c := &evals.Case{
		ID:              "sandbox_backend/seatbelt_read_only_denies_write",
		Category:        "sandbox_backend",
		Prompt:          "尝试在工作目录创建 blocked.txt。",
		ToolEnvironment: env,
		WorkDir:         workDir,
		Provider: evals.NewScriptedProvider(
			evals.ScriptedTurn{
				ToolCalls: []schema.ToolCall{
					evals.MakeToolCall("sb2", "bash", `{"command":"touch blocked.txt"}`),
				},
			},
			evals.ScriptedTurn{Text: "写入被沙箱拒绝。"},
		),
		Assertions: []evals.Assertion{
			&evals.ToolCalledAssertion{ToolName: "bash"},
			// 工具失败以错误文本回传（Self-Correction Loopback），不终止 run
			&evals.NoErrorAssertion{},
			&evals.MaxTurnsAssertion{Max: 3},
		},
	}
	if r := evals.RunCase(t.Context(), c); !r.Passed {
		t.Fatalf("read-only 下 run 应正常结束（错误经观察回传）: %+v", r)
	}
	// 行为断言：被拒绝的写入不得真实落盘
	if _, err := os.Stat(filepath.Join(workDir, "blocked.txt")); !os.IsNotExist(err) {
		t.Fatalf("read-only 沙箱未阻止工作区写入: %v", err)
	}
}
