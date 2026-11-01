package appservice

import (
	"fmt"
	"os/exec"
	"strings"
)

// DiffResult 是会话工作目录的 git 变更审查视图。
//
// 一期语义：整个 workDir 的未提交变更（对标 Codex 的 diff review 闭环；
// 待二期引入 worktree 任务隔离后，diff 将按任务分支精确圈定）。
type DiffResult struct {
	// IsGitRepo 表示 workDir 是否位于 git 仓库内；false 时其余字段无意义。
	IsGitRepo bool `json:"is_git_repo"`
	// Branch 是当前分支名（detached HEAD 时为空）。
	Branch string `json:"branch,omitempty"`
	// Stat 是 `git diff HEAD --stat` 的统计摘要。
	Stat string `json:"stat,omitempty"`
	// Patch 是 `git diff HEAD` 的完整补丁文本（超大截断）。
	Patch string `json:"patch,omitempty"`
	// Untracked 是未跟踪文件列表（`git status --porcelain` 中 ?? 前缀条目）。
	Untracked []string `json:"untracked,omitempty"`
	// Dirty 表示是否存在任何未提交变更（含未跟踪文件）。
	Dirty bool `json:"dirty"`
}

// diffPatchMaxBytes 限制补丁文本大小，防止超大 diff 拖垮前端渲染。
const diffPatchMaxBytes = 512 * 1024

// GetSessionDiff 采集活跃会话工作目录的 git 变更。
// 全部 git 调用 fail-soft：非 git 目录返回 IsGitRepo=false 而非错误。
func (s *Service) GetSessionDiff() (DiffResult, error) {
	workDir := s.cfg.WorkDir
	res := DiffResult{}

	inside, err := gitOutput(workDir, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(inside) != "true" {
		return res, nil
	}
	res.IsGitRepo = true

	if branch, err := gitOutput(workDir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		res.Branch = strings.TrimSpace(branch)
	}

	stat, _ := gitOutput(workDir, "diff", "HEAD", "--stat")
	res.Stat = stat

	patch, _ := gitOutput(workDir, "diff", "HEAD")
	res.Patch = truncateUTF8Safe(patch, diffPatchMaxBytes)

	status, err := gitOutput(workDir, "status", "--porcelain")
	if err == nil {
		for _, line := range strings.Split(status, "\n") {
			line = strings.TrimRight(line, "\r")
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "??") {
				res.Untracked = append(res.Untracked, strings.TrimSpace(strings.TrimPrefix(line, "??")))
			}
		}
	}
	res.Dirty = res.Stat != "" || len(res.Untracked) > 0
	return res, nil
}

// gitOutput 在 dir 下执行 git 子命令并返回合并 stdout。
func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s：%w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
