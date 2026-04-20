---
name: pr
description: Use when the user invokes /pr or asks to push changes and open a pull request, after commits are ready to be pushed to a remote branch and merged into the main branch.
---

# pr — Push & Pull Request

## Overview

将本地提交推送到远程分支，并使用 `gh` CLI 创建 **Draft** Pull Request，目标为仓库主分支。默认以 Draft 状态创建，避免误触发自动合并。

## 前置条件检查

1. 确认本次对话中已执行过 `/commit`，且有新提交未推送
2. 确认当前分支**不是** `main` / `master`（若在主分支，停止并提示用户切换到功能分支）
3. 确认 `gh` CLI 已登录：`gh auth status`

## 执行步骤

### 1. 了解当前分支状态

```bash
git branch --show-current          # 当前分支名
git log --oneline origin/HEAD..HEAD  # 待推送的提交列表
git diff origin/HEAD...HEAD --stat   # 本次 PR 涉及的文件变更
```

### 2. 确定目标分支

按以下顺序判断：

1. 仓库默认分支（`gh repo view --json defaultBranchRef -q .defaultBranchRef.name`）
2. 若无法获取，依次尝试 `main` → `master`
3. 仍不确定时，询问用户

### 3. 推送到远程

```bash
git push -u origin <当前分支名>
```

若远程已有同名分支且有分歧，**不使用 `--force`**，先告知用户手动处理冲突。

### 4. 起草 PR 内容

根据 `git log` 和 `git diff` 的输出，整理：

- **标题**：70 字符以内，描述本次变更的核心目的
- **正文**：包含变更摘要（2-4 条要点）和测试计划

### 5. 创建 Draft Pull Request

**默认以 Draft 状态创建**，除非用户明确要求创建正式 PR。

```bash
gh pr create \
  --draft \
  --base <目标分支> \
  --title "<PR 标题>" \
  --body "$(cat <<'EOF'
## Summary
- <要点 1>
- <要点 2>

## Test Plan
- [ ] <测试项 1>
- [ ] <测试项 2>
EOF
)"
```

### 6. 输出结果

返回 PR URL，告知用户 Draft PR 已创建成功，并提醒用户确认无误后可手动 Mark as ready for review。

## 常见错误

| 问题 | 处理 |
|------|------|
| `gh` 未登录 | 提示用户执行 `gh auth login` |
| 当前分支无新提交 | 告知用户没有可推送的内容，建议先执行 `/commit` |
| 该分支已存在 PR | 使用 `gh pr view` 查看现有 PR，提示用户是否需要更新 |
| 推送被拒绝（non-fast-forward） | 不强制推送，提示用户检查远程分支状态 |
