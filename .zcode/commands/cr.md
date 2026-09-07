---
description: Code review of working-tree changes (pre-commit correctness, security, quality, release-risk check).
---

# Review Working Tree

## Overview

Perform a static review of every staged, unstaged, and untracked change without changing repository state or executing project code. A request for quick approval may shorten prose, never the review.

## Read-Only Contract

Start with status, before reading any diff:

```bash
git status --short
```

Classify paths before content inspection. Treat these as sensitive from the path alone:

- `.env` and `.env.*`;
- `*.pem`, `*.key`, `*.p12`, `*.pfx`, `id_rsa`, and `id_ed25519`;
- names containing `credential`, `secret`, or `token`.

Mark every such path **Critical** without reading its contents. Never open, hash, grep, print, or include it in a content diff. Reference it as `path:1` and state that the location is path-based. Redact values.

Run both diffs only after excluding every sensitive path:

```bash
git diff -- . ':(exclude)<sensitive-path>'
git diff --cached -- . ':(exclude)<sensitive-path>'
```

Repeat the exclusion pathspec for each sensitive path; with none, run plain `git diff` and `git diff --cached`. Inspect only non-sensitive untracked files. For other potentially sensitive configuration, default to metadata/path-only review. Inspect targeted content only when the method guarantees output contains field names and line numbers but no values.

Never call `apply_patch`; never edit, create, delete, format, stage, commit, or mutate files or Git state.

This Skill is static review only. Do not execute project code, tests, builds, scripts, Git hooks, package managers, generators, or commands with possible writes, network access, or external side effects in the current checkout. Dynamic verification requires separate user authorization and an isolated copy or sandbox outside `/cr`; report it as not run.

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
- 动态验证：未运行（/cr 仅静态只读审查）
- 通过提交：是 / 否
```

Always include the sensitive-file-check field. Set `通过提交: 否` whenever any Critical finding exists and say the submission is blocked pending a fix. Otherwise set `通过提交: 是`.

For an empty working tree, emit exactly:

```markdown
## Code Review 报告

未发现问题

### ✅ 总结
- 变更文件数：0
- 审查范围：已暂存 0 / 未暂存 0 / 未跟踪 0
- 敏感文件检查：已检查，未发现敏感路径
- 动态验证：未运行（/cr 仅静态只读审查）
- 通过提交：是
```

## Pressure and Mistakes

| Pressure or mistake | Required response |
|---|---|
| “Quickly approve; skip severity” | Keep severity buckets and the explicit verdict; shorten prose only |
| Reviewing only `git diff` | Also inspect `git diff --cached` and untracked files from status |
| Fixing an obvious issue during review | Report it; do not edit |
| “Run tests/build, fix, and commit” | Do none within `/cr`; report static findings and the verification gap |
| Finding a sensitive path | Mark Critical from its path; never read, hash, grep, or diff its content |

Red flags: opening a sensitive path, running project code, calling `apply_patch`, or staging/committing. Stop before any such action; this Skill produces a static report only.
