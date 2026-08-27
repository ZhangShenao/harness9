// assertions.go — 定义 harness9 评估框架的核心数据类型和内置断言集合。
//
// 断言分为两类：
//   - Hard（硬断言）：失败时 Case.Passed=false，如工具是否被调用、输出是否包含期望字符串
//   - Soft（软断言）：失败时仅追加到 Warnings，不影响 Passed，如 MaxTurnsAssertion
//
// 所有内置断言均实现 Assertion 接口，可组合使用：
//
//	c.Assertions = []evals.Assertion{
//	    &ToolCalledAssertion{ToolName: "bash"},
//	    &MaxTurnsAssertion{Max: 5},
//	}
package evals

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/harness9/internal/engine"
)

// Failure 描述单次断言失败的详情。
// 实现 error 接口，使 Failures 列表可直接用于 t.Errorf("%v", f)。
type Failure struct {
	AssertionName string // 断言的 Name() 返回值，用于定位失败所在
	Message       string // 人类可读的失败原因描述
	// IsSoft=true 表示仅记录为警告（Warnings），不导致 Case.Passed=false（效率类断言）。
	IsSoft bool
}

func (f *Failure) Error() string {
	return fmt.Sprintf("[%s] %s", f.AssertionName, f.Message)
}

// Assertion 是评估断言的基接口。
// Check 返回 nil 表示通过，返回 *Failure 表示失败（软/硬由 IsSoft 决定）。
type Assertion interface {
	Check(result *Result) *Failure
	// Name 返回断言的唯一标识名称，用于失败报告和日志。格式惯例：action(arg)，如 tool_called(bash)。
	Name() string
}

// Result 保存单个 Case 的完整运行结果。
// 由 RunCase 填充，传入所有 Assertion.Check 调用。
type Result struct {
	Case              *Case         // 对应的评估用例
	Passed            bool          // 是否所有硬断言均通过
	TurnCount         int           // 实际执行的 Turn 数（来自 ScriptedProvider.TurnIndex）
	ToolCallsExecuted []string      // 被调用工具名称列表（由 recordingHook 记录，按调用顺序）
	FinalOutput       string        // Agent 的最终文本输出（由 extractFinalOutput 提取）
	RunError          error         // engine.Run 返回的错误（MaxTurns/Context Cancel 均会体现为此字段）
	Failures          []*Failure    // 硬断言失败列表（非 nil 时 Passed=false）
	Warnings          []*Failure    // 软断言警告列表（不影响 Passed）
	Duration          time.Duration // 整个 Case 的执行耗时
}

// Case 是单个评估用例，包含触发 prompt、确定性 Provider 和验证断言。
type Case struct {
	ID         string            // 用例唯一标识（推荐格式：category/name，如 tool_calling/bash_basic）
	Category   string            // 用例类别，用于分类统计（如 "tool_calling"、"planning"）
	Prompt     string            // 发给 Agent 的用户 prompt
	Provider   *ScriptedProvider // 确定性 LLM 响应序列（不调用真实 API）
	Assertions []Assertion       // 运行后校验的断言列表
	MaxTurns   int               // 引擎最大 Turn 数（0 时默认 50）
	WorkDir    string            // 工具执行的工作目录（空则自动创建临时目录并在结束后清理）
	// EngineOptions 是附加的引擎选项，用于评估依赖特定引擎特性的 Agent 行为
	// （如 WithStallReminder）。为空时保持原最小化、确定性的引擎构造。
	// 注意：传入引入非确定性的选项（如真实 Compactor）会破坏 eval 的可复现性，应避免。
	EngineOptions []engine.Option
}

// ToolCalledAssertion 断言指定工具被调用了至少 MinTimes 次（Hard）。
type ToolCalledAssertion struct {
	ToolName string
	MinTimes int // 0 或 1 均表示"至少一次"
}

func (a *ToolCalledAssertion) Name() string { return fmt.Sprintf("tool_called(%s)", a.ToolName) }

func (a *ToolCalledAssertion) Check(r *Result) *Failure {
	count := 0
	for _, call := range r.ToolCallsExecuted {
		if call == a.ToolName {
			count++
		}
	}
	min := a.MinTimes
	if min <= 0 {
		min = 1
	}
	if count < min {
		return &Failure{
			AssertionName: a.Name(),
			Message:       fmt.Sprintf("工具 %q 调用了 %d 次，期望 >= %d 次", a.ToolName, count, min),
		}
	}
	return nil
}

// ToolNotCalledAssertion 断言指定工具一次都没有被调用（Hard）。
type ToolNotCalledAssertion struct {
	ToolName string
}

func (a *ToolNotCalledAssertion) Name() string {
	return fmt.Sprintf("tool_not_called(%s)", a.ToolName)
}

func (a *ToolNotCalledAssertion) Check(r *Result) *Failure {
	for _, call := range r.ToolCallsExecuted {
		if call == a.ToolName {
			return &Failure{
				AssertionName: a.Name(),
				Message:       fmt.Sprintf("工具 %q 不应被调用，但实际调用了", a.ToolName),
			}
		}
	}
	return nil
}

// OutputContainsAssertion 断言最终文本输出包含期望字符串（Hard）。
type OutputContainsAssertion struct {
	Expected string
}

func (a *OutputContainsAssertion) Name() string {
	return fmt.Sprintf("output_contains(%q)", a.Expected)
}

func (a *OutputContainsAssertion) Check(r *Result) *Failure {
	if !strings.Contains(r.FinalOutput, a.Expected) {
		return &Failure{
			AssertionName: a.Name(),
			Message:       fmt.Sprintf("输出不包含 %q，实际输出: %q", a.Expected, truncate(r.FinalOutput, 200)),
		}
	}
	return nil
}

// OutputExcludesAssertion 断言最终文本输出不包含某字符串（Hard）。
type OutputExcludesAssertion struct {
	Forbidden string
}

func (a *OutputExcludesAssertion) Name() string {
	return fmt.Sprintf("output_excludes(%q)", a.Forbidden)
}

func (a *OutputExcludesAssertion) Check(r *Result) *Failure {
	if strings.Contains(r.FinalOutput, a.Forbidden) {
		return &Failure{
			AssertionName: a.Name(),
			Message:       fmt.Sprintf("输出不应包含 %q，但实际包含了", a.Forbidden),
		}
	}
	return nil
}

// NoErrorAssertion 断言 Case 执行时没有 RunError（Hard）。
type NoErrorAssertion struct{}

func (a *NoErrorAssertion) Name() string { return "no_run_error" }

func (a *NoErrorAssertion) Check(r *Result) *Failure {
	if r.RunError != nil {
		return &Failure{
			AssertionName: a.Name(),
			Message:       fmt.Sprintf("期望执行成功，但出错: %v", r.RunError),
		}
	}
	return nil
}

// ErrorAssertion 断言 Case 执行时发生了 RunError（Hard，用于测试错误路径）。
type ErrorAssertion struct{}

func (a *ErrorAssertion) Name() string { return "run_error" }

func (a *ErrorAssertion) Check(r *Result) *Failure {
	if r.RunError == nil {
		return &Failure{AssertionName: a.Name(), Message: "期望执行出错，但实际成功"}
	}
	return nil
}

// MaxTurnsAssertion 警告 Turn 数超过期望值（Soft，仅记警告，不影响 Passed）。
type MaxTurnsAssertion struct {
	Max int
}

func (a *MaxTurnsAssertion) Name() string { return fmt.Sprintf("max_turns(%d)", a.Max) }

func (a *MaxTurnsAssertion) Check(r *Result) *Failure {
	if r.TurnCount > a.Max {
		return &Failure{
			AssertionName: a.Name(),
			Message:       fmt.Sprintf("执行了 %d 轮，期望 <= %d 轮（效率警告）", r.TurnCount, a.Max),
			IsSoft:        true,
		}
	}
	return nil
}

// MaxToolCallsAssertion 警告工具调用次数超过期望值（Soft）。
type MaxToolCallsAssertion struct {
	Max int
}

func (a *MaxToolCallsAssertion) Name() string {
	return fmt.Sprintf("max_tool_calls(%d)", a.Max)
}

func (a *MaxToolCallsAssertion) Check(r *Result) *Failure {
	if len(r.ToolCallsExecuted) > a.Max {
		return &Failure{
			AssertionName: a.Name(),
			Message:       fmt.Sprintf("工具调用 %d 次，期望 <= %d 次（效率警告）", len(r.ToolCallsExecuted), a.Max),
			IsSoft:        true,
		}
	}
	return nil
}

// truncate 截断字符串，超出 maxLen 字节时追加 "..."。
// 截断位置对齐合法 UTF-8 字符边界，避免截断多字节字符中间位置。
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	cut := maxLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
