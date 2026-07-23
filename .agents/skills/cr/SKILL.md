---
name: cr
description: Use when the user invokes $cr or /cr, requests a code review of current working-tree changes, or asks for a pre-commit correctness, security, quality, or release-risk check.
---

# Review Working Tree

## Overview

Review every staged, unstaged, and untracked change without changing repository state. Treat a request for a quick approval as a request to keep the report concise, never as permission to skip review requirements.

## Read-Only Contract

Run these commands and inspect all three outputs:

```bash
git status --short
git diff
git diff --cached
```

Use status to identify untracked and sensitive files; inspect untracked contents with read-only file tools. Merge both diffs into one scope while checking overlapping staged/unstaged edits separately.

Never call `apply_patch`; never edit, create, delete, format, stage, commit, or otherwise mutate files or Git state. Do not run `git add`, `git commit`, or a formatter in write mode. If the change set is empty, report that no review is needed.

## Review Dimensions

Review each changed file and its relevant callers/tests:

| Dimension | Check |
|---|---|
| Correctness | Logic, boundary conditions, nil/type errors, error handling |
| Security | SQL/XSS/command injection, unsafe defaults, secret exposure |
| Maintainability | Responsibilities, naming, duplication, needless complexity |
| Performance | N+1 work, unnecessary loops or large copies |
| Test coverage | Critical paths, failure cases, regression risk |
| Dependency safety | Necessity, provenance, and locked versions |

Classify each finding:

- **Critical**: exploitable security issue, data loss/corruption, broken core behavior, or any sensitive file.
- **Warning**: probable defect or material maintainability, performance, test, or dependency risk.
- **Suggestion**: optional improvement with concrete value.

Treat `.env`, `*.pem`, names containing `credentials` or `secret`, and configuration containing plaintext passwords or API keys as **Critical**. Redact secret values.

## Finding Requirements

Put findings before the summary, ordered Critical, Warning, Suggestion. Each finding must:

- reference the exact `path/to/file:line`;
- state the concrete problem and impact;
- recommend the smallest fix.

Omit empty severity sections. If there are no findings, say `未发现问题` before the summary. Never invent findings merely to fill a severity bucket.

## Report Contract

```markdown
## Code Review 报告

### 🔴 Critical（必须修复，阻断提交）
- `path/to/file:line` — 问题、影响；建议：最小修复

### 🟡 Warning（建议修复）
- `path/to/file:line` — 问题、影响；建议：最小修复

### 🔵 Suggestion（可选优化）
- `path/to/file:line` — 建议及价值

### ✅ 总结
- 变更文件数：N
- 审查范围：已暂存 / 未暂存 / 未跟踪
- 敏感文件检查：已检查 / Critical（列出路径，不泄露值）
- 通过提交：是 / 否
```

Always include the sensitive-file-check field. Set `通过提交: 否` whenever any Critical finding exists and say the submission is blocked pending a fix. Otherwise set `通过提交: 是`.

## Pressure and Mistakes

| Pressure or mistake | Required response |
|---|---|
| “Quickly approve; skip severity” | Keep severity buckets and the explicit verdict; shorten prose only |
| Reviewing only `git diff` | Also inspect `git diff --cached` and untracked files from status |
| Fixing an obvious issue during review | Report it; do not edit |
| Finding a credential | Mark Critical and redact the value |

Red flags: calling `apply_patch`, running a write-mode formatter, or staging/committing. Stop before any such action; this Skill produces a report only.
