---
name: commit
description: Use when the user invokes /commit or asks to commit changes, after a code review has been completed and the changes are confirmed ready to stage and commit to git.
---

# commit — Stage & Commit

## Overview

在 Code Review 通过后，将有效的代码变更暂存并提交到本地 Git 仓库。

## 前置条件检查

1. 确认本次对话中已执行过 `/cr`（Code Review）
2. 若 Review 报告存在 **Critical** 问题，**停止执行**，提示用户先修复
3. 若未执行过 `/cr`，先调用 `cr` skill 完成 Review，再继续

## 执行步骤

### 1. 确认变更范围

```bash
git status
git diff --stat
git diff --cached --stat
```

### 2. 精确暂存文件

**不使用 `git add -A` 或 `git add .`**，逐文件或逐目录添加：

```bash
git add <具体文件或目录>
```

排除规则：
- `.env`、包含敏感信息的文件 → 不暂存，提示用户确认
- 与本次功能无关的临时文件 → 不暂存

### 3. 起草提交信息

参考以下格式（优先遵循仓库现有风格，通过 `git log --oneline -10` 判断）：

```
<type>: <简明描述>（50 字符以内）

<可选正文：说明 why，不是 what>
```

常用 `type`：`feat` / `fix` / `docs` / `refactor` / `test` / `chore`

### 4. 执行提交

```bash
git commit -m "$(cat <<'EOF'
<提交信息>
EOF
)"
```

### 5. 确认结果

```bash
git log --oneline -3   # 确认提交已记录
git status             # 确认工作区干净
```

输出提交的 hash 与摘要，告知用户提交成功。

## 常见错误

| 问题 | 处理 |
|------|------|
| pre-commit hook 失败 | 修复 hook 报告的问题，重新 `git add` 后创建**新** commit，不使用 `--amend` |
| 暂存了不该提交的文件 | `git restore --staged <file>` 取消暂存，重新操作 |
| 工作区已无变更 | 告知用户没有可提交的内容 |
