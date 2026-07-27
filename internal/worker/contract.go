package worker

import (
	"fmt"
	"strings"

	"github.com/harness9/internal/subagent"
)

// implementationSystemPrompt is the system prompt for Mission's default
// implementation Task Contract. It replaces AutoDev's dev.md: unlike AutoDev
// (which keeps a worktree as a subdirectory of a shared, fixed-workDir
// Runner and has the sub-agent `cd` into it), each Attempt here gets its own
// Runner whose WorkDir is the worktree root itself, so paths and bash
// commands are used directly with no cd/relative-path indirection.
const implementationSystemPrompt = `你是 Mission 的 implementation Worker，在一个专属 git worktree 中独立完成被分配的 Task。

你的工作目录就是这个 Task 专属的 git worktree 根目录——所有文件路径和 bash 命令都直接使用，不需要 cd 到任何子目录。

工作流程：
1. 如果存在 AGENTS.md，先阅读它了解项目规范。
2. 探索代码库，理解需要改动的范围。
3. 实现 Task 描述里的目标。
4. 运行 go build ./...，编译通过前不要进入下一步。
5. 运行 go test ./... -timeout 5m，最多重试 3 次修复失败；3 次仍失败则如实报告失败，不得伪造通过。
6. gofmt -w . 格式化。
7. git add -A && git commit -m "<简明 commit message>" 提交。

完成后，在最后一条消息末尾附上（不要用代码块包裹）：
TASK_RESULT: SUCCESS
COMMIT: <git rev-parse HEAD 的输出>

如果第 5 步 3 次仍失败，附上：
TASK_RESULT: FAILED
REASON: <一句话原因>`

// ImplementationContract is Mission's default Task Contract for code
// implementation work, used by the Worker Adapter for every Task unless a
// future Contract type (e.g. integration, verification) overrides it.
var ImplementationContract = subagent.SubAgentDefinition{
	Name:         "mission-implementation",
	Description:  "Mission 默认的实现类 Task Contract：在专属 worktree 中实现、测试并提交一个 Task",
	SystemPrompt: implementationSystemPrompt,
	Tools:        []string{"bash", "read_file", "write_file", "edit_file", "web_search", "web_fetch"},
	Source:       "builtin",
}

// Result is the outcome the Worker Adapter extracts from a sub-agent's final
// text, per the TASK_RESULT/COMMIT/REASON convention in
// implementationSystemPrompt.
type Result struct {
	Success bool
	Commit  string
	Reason  string
}

// ParseResult extracts a Result from a sub-agent's raw final text. It returns
// an error if no TASK_RESULT marker is present, or if a reported success is
// missing its COMMIT sha.
func ParseResult(finalText string) (Result, error) {
	var result Result
	found := false
	for _, line := range strings.Split(finalText, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "TASK_RESULT: SUCCESS"):
			result.Success = true
			found = true
		case strings.HasPrefix(line, "TASK_RESULT: FAILED"):
			result.Success = false
			found = true
		case strings.HasPrefix(line, "COMMIT:"):
			result.Commit = strings.TrimSpace(strings.TrimPrefix(line, "COMMIT:"))
		case strings.HasPrefix(line, "REASON:"):
			result.Reason = strings.TrimSpace(strings.TrimPrefix(line, "REASON:"))
		}
	}
	if !found {
		return Result{}, fmt.Errorf("worker output is missing a TASK_RESULT marker")
	}
	if result.Success && result.Commit == "" {
		return Result{}, fmt.Errorf("worker reported success without a COMMIT sha")
	}
	return result, nil
}
