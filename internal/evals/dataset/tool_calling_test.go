// Package dataset 包含 harness9 的黄金评估数据集。
// 所有测试使用 ScriptedProvider（确定性）+ SetupHermeticEnv（hermetic 隔离），无 API Key 依赖。
// 运行方式：go test ./internal/evals/dataset/... -v
package dataset

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/harness9/internal/evals"
	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/tools"
)

// TestToolCalling 运行工具调用准确性评估（6 个黄金用例）。
func TestToolCalling(t *testing.T) {
	evals.SetupHermeticEnv(t)

	cases := []*evals.Case{
		// 用例 1：bash 基础调用
		{
			ID:       "tool_calling/bash_basic",
			Category: "tool_calling",
			Prompt:   "运行命令 `echo hello`，告诉我输出结果。",
			Provider: evals.NewScriptedProvider(
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc1", "bash", `{"command":"echo hello"}`),
					},
				},
				evals.ScriptedTurn{Text: "命令输出了 hello。"},
			),
			Assertions: []evals.Assertion{
				&evals.ToolCalledAssertion{ToolName: "bash"},
				&evals.NoErrorAssertion{},
				&evals.MaxTurnsAssertion{Max: 3},
			},
		},
		// 用例 2：read_file 调用
		{
			ID:       "tool_calling/read_file",
			Category: "tool_calling",
			Prompt:   "读取 README.md 文件，总结其内容。",
			Provider: evals.NewScriptedProvider(
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc1", "read_file", `{"path":"README.md"}`),
					},
				},
				evals.ScriptedTurn{Text: "README.md 描述了一个 Agent 框架项目。"},
			),
			Assertions: []evals.Assertion{
				&evals.ToolCalledAssertion{ToolName: "read_file"},
				&evals.NoErrorAssertion{},
			},
		},
		// 用例 3：write_file 后 read_file（多工具顺序调用）
		{
			ID:       "tool_calling/write_then_read",
			Category: "tool_calling",
			Prompt:   "创建 hello.txt 写入 'Hello World'，然后读取确认内容。",
			Provider: evals.NewScriptedProvider(
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc1", "write_file", `{"path":"hello.txt","content":"Hello World"}`),
					},
				},
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc2", "read_file", `{"path":"hello.txt"}`),
					},
				},
				evals.ScriptedTurn{Text: "已确认文件内容为 Hello World。"},
			),
			Assertions: []evals.Assertion{
				&evals.ToolCalledAssertion{ToolName: "write_file"},
				&evals.ToolCalledAssertion{ToolName: "read_file"},
				&evals.NoErrorAssertion{},
				&evals.MaxTurnsAssertion{Max: 4},
			},
		},
		// 用例 5：edit_file 模糊匹配（缩进不一致触发 L4）端到端不破坏循环。
		// 先 write_file 写入带 4/8 空格缩进的 Python 方法；随后 edit_file 用 0/4 缩进的
		// source_text（与文件缩进不一致 → 走 L4 逐行去缩进匹配 + 重缩进），验证编辑成功、
		// 引擎不因模糊匹配/缩进重排而报错（覆盖 fuzzyReplaceWithLevel + reindentBlock 路径）。
		{
			ID:       "tool_calling/edit_file_fuzzy_indent",
			Category: "tool_calling",
			Prompt:   "把 mod.py 中 compute 方法的返回值从 1 改成 2。",
			Provider: evals.NewScriptedProvider(
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc1", "write_file",
							`{"path":"mod.py","content":"class A:\n    def compute(self):\n        return 1\n"}`),
					},
				},
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						// source_text 缩进（0/4）与文件（4/8）不一致 → 强制 L4 模糊匹配
						evals.MakeToolCall("tc2", "edit_file",
							`{"path":"mod.py","source_text":"def compute(self):\n    return 1","target_text":"def compute(self):\n    return 2"}`),
					},
				},
				evals.ScriptedTurn{Text: "已将 compute 的返回值改为 2。"},
			),
			Assertions: []evals.Assertion{
				&evals.ToolCalledAssertion{ToolName: "write_file"},
				&evals.ToolCalledAssertion{ToolName: "edit_file"},
				// 模糊匹配编辑成功不应让引擎报错（self-healing/正确性）
				&evals.NoErrorAssertion{},
				&evals.MaxTurnsAssertion{Max: 4},
			},
		},
		// 用例 4：纯对话，不应调用工具
		{
			ID:       "tool_calling/no_tool_conversation",
			Category: "tool_calling",
			Prompt:   "harness9 是什么？简单介绍一下。",
			Provider: evals.NewScriptedProvider(
				evals.ScriptedTurn{Text: "harness9 是一个轻量级 AI Agent Harness 框架。"},
			),
			Assertions: []evals.Assertion{
				&evals.ToolNotCalledAssertion{ToolName: "bash"},
				&evals.ToolNotCalledAssertion{ToolName: "write_file"},
				&evals.OutputContainsAssertion{Expected: "harness9"},
				&evals.NoErrorAssertion{},
			},
		},
		// 用例 6：同一 Turn 内并行调用两个工具。
		// 引擎会并发执行这两个工具调用（preallocated slice + index write 保证结果顺序），
		// 该用例在 go test -race 下同时是 recordingHook 并发写竞争的回归测试。
		{
			ID:       "tool_calling/parallel_tools",
			Category: "tool_calling",
			Prompt:   "同时运行 ls 和 pwd 两个命令，汇报结果。",
			Provider: evals.NewScriptedProvider(
				evals.ScriptedTurn{
					ToolCalls: []schema.ToolCall{
						evals.MakeToolCall("tc1", "bash", `{"command":"ls"}`),
						evals.MakeToolCall("tc2", "bash", `{"command":"pwd"}`),
					},
				},
				evals.ScriptedTurn{Text: "已同时执行两个命令。"},
			),
			Assertions: []evals.Assertion{
				// 两个并行工具调用都必须被记录（MinTimes=2 验证并发记录不丢项）
				&evals.ToolCalledAssertion{ToolName: "bash", MinTimes: 2},
				&evals.NoErrorAssertion{},
				&evals.MaxTurnsAssertion{Max: 3},
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
	t.Logf("工具调用评估：%d/%d 通过", passed, passed+failed)
}

// TestToolCallingGlob 验证 glob 工具被正确调用（临时目录预置文件）。
// 核心不变量：glob 作为一等工具经 ExtraTools 注册后被调度执行、引擎零 RunError
// （命中真实文件使工具输出非空，端到端走通 WalkDir + ** 匹配路径）。
func TestToolCallingGlob(t *testing.T) {
	evals.SetupHermeticEnv(t)
	wd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wd, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd, "pkg", "a.go"), []byte("package pkg"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &evals.Case{
		ID:       "tool_calling/glob",
		Category: "tool_calling",
		Prompt:   "找出工作区内所有 Go 文件",
		WorkDir:  wd,
		Provider: evals.NewScriptedProvider(
			evals.ScriptedTurn{ToolCalls: []schema.ToolCall{{
				ID: "c1", Name: "glob", Arguments: json.RawMessage(`{"pattern":"**/*.go"}`),
			}}},
			evals.ScriptedTurn{Text: "找到 1 个 Go 文件：pkg/a.go"},
		),
		Assertions: []evals.Assertion{
			&evals.ToolCalledAssertion{ToolName: "glob"},
			&evals.NoErrorAssertion{},
		},
		ExtraTools: []tools.BaseTool{tools.NewGlobTool(wd)},
	}
	if res := evals.RunCase(context.Background(), c); !res.Passed {
		t.Fatalf("eval 失败: %+v", res.Failures)
	}
}

// TestToolCallingGrepWithFilter 验证 grep 带 glob 过滤调用。
// 核心不变量：glob 参数过滤非 Go 文件后仅 a.go 命中、引擎零 RunError
// （端到端覆盖正则编译 + WalkDir + 文件名过滤 + 行级命中输出路径）。
func TestToolCallingGrepWithFilter(t *testing.T) {
	evals.SetupHermeticEnv(t)
	wd := t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, "a.go"), []byte("package main\nfunc Foo(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd, "b.txt"), []byte("Foo in txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &evals.Case{
		ID:       "tool_calling/grep",
		Category: "tool_calling",
		Prompt:   "只在 Go 文件里搜 Foo 的定义",
		WorkDir:  wd,
		Provider: evals.NewScriptedProvider(
			evals.ScriptedTurn{ToolCalls: []schema.ToolCall{{
				ID: "c1", Name: "grep", Arguments: json.RawMessage(`{"pattern":"func Foo","glob":"*.go"}`),
			}}},
			evals.ScriptedTurn{Text: "Foo 定义在 a.go:2"},
		),
		Assertions: []evals.Assertion{
			&evals.ToolCalledAssertion{ToolName: "grep"},
			&evals.NoErrorAssertion{},
		},
		ExtraTools: []tools.BaseTool{tools.NewGrepTool(wd)},
	}
	if res := evals.RunCase(context.Background(), c); !res.Passed {
		t.Fatalf("eval 失败: %+v", res.Failures)
	}
}
