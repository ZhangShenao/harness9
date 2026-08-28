---
description: 一站式文档同步管线：代码变更 → 中文文档 → 英文镜像（三阶段）。
---

# Sync Docs（文档同步管线）

## Overview

维护 `docs/核心功能/`（中文，唯一信息源）与 `docs/core-features-en/`（英文镜像）的完整同步：

- **Phase 1**: 代码变更 → 中文文档更新（基于 `docs/doc-map.json` 模块映射 + git diff）
- **Phase 2**: 中文文档 → 英文镜像同步（MISSING / STALE 扫描）
- **Phase 3**: 统一收尾报告

CI 中有对应的机械式漂移检测（`scripts/check-doc-drift.sh`，warn 模式），本命令是其 LLM 辅助的落地执行手段：提 PR 前先跑本命令补文档。

## When to Run

- 修改了任何 `internal/`、`cmd/`、`skills/` 源码之后、提交 PR 之前
- 创建或修改了 `docs/核心功能/` 下任何文件之后
- 用户执行 `/sync-docs`（可附带 diff 范围参数，如 `/sync-docs HEAD~3`）

## Phase 1: 代码变更 → 中文文档

1. 确定变更范围：用户显式指定时以用户为准；否则优先工作区未提交改动（`git status --porcelain` + `git diff`），无未提交改动时用 `git diff --name-only master...HEAD`。
2. 过滤出变更的源码路径（忽略 `*_test.go` 与生成产物）。
3. 读取 `docs/doc-map.json`，对每个映射条目：`paths` 中任一模式命中变更文件、且 `docs` 非空时，将其 `docs` 列入候选更新清单。映射表之外的模块不强制；若发现明显的未登记模块→文档对应关系，在报告中建议补充映射表。
4. 对每个候选文档：
   - 通读文档全文 + 相关代码 diff
   - 判断需要更新的内容：新增功能 / 新增接口与类型 / 行为变更 / 删除的内容 / 过时的代码片段与文件结构描述
   - 直接编辑中文文档，保持现有章节结构与写作风格；无实质影响的变更（注释措辞、测试内部调整）可跳过并在报告中说明理由
5. 候选清单为空时，报告"代码变更无需文档同步"，直接进入 Phase 3。

## Phase 2: 中文 → 英文镜像

### 2.1 Scan for mismatches

Run this check to identify missing or stale English docs:

```bash
for f in docs/核心功能/*.md; do
  name=$(basename "$f")
  en_path="docs/core-features-en/$name"
  if [ ! -f "$en_path" ]; then
    echo "MISSING: $en_path"
  elif [ "$f" -nt "$en_path" ]; then
    echo "STALE: $en_path (Chinese version is newer)"
  fi
done
```

Classify results:
- **MISSING**: English doc does not exist--must be created
- **STALE**: English doc exists but Chinese version was modified more recently--should be reviewed and updated
- **OK**: Both exist and English is not older than Chinese

If all docs are OK, report success and go to Phase 3.

### 2.2 For each MISSING doc: create the English version

Read the Chinese doc in full. Then create the English counterpart at `docs/core-features-en/<same-filename>` following these rules:

**Translation principles:**
- The English version is an **independently written technical document**, not a machine translation
- Content and structure must align with the Chinese version (same sections, same code blocks, same tables)
- Follow English technical writing conventions: active voice, concise sentences, no unnecessary hedging
- Code blocks, Go struct definitions, and CLI examples remain identical (they are language-neutral)
- Chinese-specific UI strings shown in examples (e.g., TUI output) should be translated to English equivalents
- Comments in code blocks: translate Chinese comments to English

**File header:** Start with an English `# Title` that translates the Chinese title.

### 2.3 For each STALE doc: review and update

Read both the Chinese and English versions. Compare section by section:
- If the Chinese version added new sections, add corresponding English sections
- If the Chinese version modified content, update the English to match
- If the Chinese version removed content, remove the corresponding English content
- Preserve any English-specific phrasing that is already good

### 2.4 Verify

Re-run the scan from step 2.1. Require zero MISSING and zero STALE results.

Also verify that `website/scripts/sync-docs.mjs` will pick up the new files correctly (it scans both directories for `*.md` files with matching names).

## Phase 3: 收尾报告

统一输出：

- **Phase 1**: 更新的中文文档清单（每份列出更新要点）；无需更新的候选说明理由
- **Phase 2**: 创建的英文文档（MISSING -> created）/ 更新的英文文档（STALE -> updated）/ 已同步（OK）
- **需人工确认的事项**（内容存疑的行为变更、建议补充的映射表条目等）
- 两个目录的文档总数（应一致）

## Constraints

- Never delete an English doc without explicit user instruction
- Phase 2 never modifies Chinese docs--only Phase 1 edits Chinese docs, Phase 2 only creates/updates English counterparts
- If a Chinese doc has no corresponding English doc and the content is too large to translate in one pass, ask the user whether to proceed or split the work
- Phase 1 编辑必须以代码 diff 为准，不得虚构行为；拿不准的变更先向用户确认再落笔
