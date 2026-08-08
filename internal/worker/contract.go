package worker

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/harness9/internal/mission"
)

// BuildImplementationContract creates the prompt for a Worker sub-agent.
func BuildImplementationContract(task mission.Task, depArtifacts []mission.Artifact) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Task: %s\n\n", task.Title)
	if len(task.Input.Goal) > 0 {
		fmt.Fprintf(&b, "### Goal\n%s\n\n", task.Input.Goal)
	}
	if len(task.Input.Acceptance) > 0 {
		b.WriteString("### Acceptance Criteria\n")
		for _, a := range task.Input.Acceptance {
			fmt.Fprintf(&b, "- %s\n", a)
		}
		b.WriteString("\n")
	}
	if len(task.Input.AllowedTools) > 0 {
		b.WriteString("### Allowed Tools\n")
		fmt.Fprintf(&b, "%s\n\n", strings.Join(task.Input.AllowedTools, ", "))
	}
	if len(depArtifacts) > 0 {
		b.WriteString("### Dependency Artifacts\n")
		for _, a := range depArtifacts {
			content := string(a.Content)
			if len(content) > 200 {
				content = content[:200]
			}
			fmt.Fprintf(&b, "- %s: %s\n", a.Kind, content)
		}
		b.WriteString("\n")
	}
	b.WriteString("### Instructions\n")
	b.WriteString("1. Implement the task in this worktree\n")
	b.WriteString("2. Run tests to verify\n")
	b.WriteString("3. Commit your work\n")
	b.WriteString("4. Output a TASK_RESULT JSON block at the end:\n")
	b.WriteString("```json\n{\"commit\": \"<sha>\", \"files\": [\"<list>\"], \"summary\": \"<text>\"}\n```\n")
	return b.String()
}

// TaskResult is the structured output parsed from the Worker's final text.
type TaskResult struct {
	Commit  string   `json:"commit"`
	Files   []string `json:"files"`
	Summary string   `json:"summary"`
}

// ParseResult extracts the TASK_RESULT JSON block from the Worker output.
func ParseResult(output string) (TaskResult, error) {
	idx := strings.Index(output, `{"commit"`)
	if idx < 0 {
		return TaskResult{}, fmt.Errorf("no TASK_RESULT found in output")
	}
	end := strings.Index(output[idx:], "}")
	if end < 0 {
		return TaskResult{}, fmt.Errorf("malformed TASK_RESULT")
	}
	jsonStr := output[idx : idx+end+1]
	var result TaskResult
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return TaskResult{}, fmt.Errorf("parse TASK_RESULT: %w", err)
	}
	return result, nil
}
