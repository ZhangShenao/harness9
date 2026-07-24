# Claude Code to Codex Project Configuration Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a complete project-scoped Codex configuration for harness9 while preserving the existing Claude Code configuration byte-for-byte and upgrading the blog-writing agent to generate actual images.

**Architecture:** Keep Claude and Codex as independent configuration stacks. Codex uses `.codex/` for agents, MCP, hooks, rules, scripts, and tests; repository Skills live under `.agents/skills/`. Safety-sensitive shell behavior is implemented as tested scripts, while long-form agent behavior is migrated into standalone TOML `developer_instructions`.

**Tech Stack:** Codex project TOML, Codex Agent Skills, JSON lifecycle hooks, Starlark exec-policy rules, Bash 3.2-compatible scripts, Python 3 `tomllib`, Go 1.25.3 regression tests.

## Global Constraints

- Do not modify `.claude/`, `.mcp.json`, `AGENTS.md`, or the `CLAUDE.md -> AGENTS.md` symlink.
- Preserve the existing external knowledge root exactly: `/Users/zsa/Desktop/workspace/harness9/知识库日报/`.
- Do not migrate Claude sessions, caches, histories, plugin copies, permission history, secrets, tokens, passwords, or connection strings.
- Keep Superpowers, Figma, and Notion as installed Codex plugins; do not vendor them.
- `test-runner` is the only custom agent with a fixed model, `gpt-5.6-luna`; all other custom agents inherit the parent model.
- Use the built-in `imagegen` Skill and `image_gen` tool for blog assets; never silently fall back to an API-key CLI path.
- Generate at least six body images and one cover image per blog, save them under `website/zh/blog/<slug>/images/`, and keep the final prompts in the article.
- Use TDD for both scripts and RED-GREEN-REFACTOR validation for each migrated Skill.
- Format all new shell scripts with portable Bash syntax and validate them with `bash -n`.
- Commit each independently testable task separately.

---

## File Map

| Path | Responsibility |
|---|---|
| `.codex/config.toml` | Enable project agents and register Context7 |
| `.codex/agents/*.toml` | Seven standalone Codex Sub-Agent definitions |
| `.codex/hooks.json` | Register the Codex `PostToolUse` Obsidian hook |
| `.codex/hooks/sync-to-obsidian.sh` | Parse Codex hook JSON and copy eligible Markdown |
| `.codex/scripts/cleanup-knowledge-day.sh` | Safely remove one date's intermediate knowledge data |
| `.codex/rules/harness9.rules` | Project command decisions for validation, Git/GitHub writes, and deletion |
| `.codex/tests/test-sync-to-obsidian.sh` | Hook behavior tests |
| `.codex/tests/test-cleanup-knowledge-day.sh` | Cleanup guardrail tests |
| `.codex/tests/validate_config.py` | Structural validation for config and custom agents |
| `.agents/skills/cr/` | Read-only working-tree review Skill |
| `.agents/skills/commit/` | Reviewed-change staging and commit Skill |
| `.agents/skills/pr/` | Branch push and draft PR Skill |
| `.agents/skills/release-cli/` | harness9 CLI release Skill |

### Task 1: Capture the immutable Claude baseline

**Files:**
- Read: `.claude/**`
- Read: `.mcp.json`
- Read: `AGENTS.md`
- Read: `CLAUDE.md`
- Create outside repository: `${TMPDIR:-/tmp}/harness9-claude-baseline.sha256`

**Interfaces:**
- Produces: a sorted SHA-256 manifest used by Task 11
- Consumes: no earlier task

- [ ] **Step 1: Confirm the worktree only contains the approved design commit**

Run:

```bash
git status --short
git log -2 --oneline
```

Expected: no working-tree output; latest commit is the approved design document.

- [ ] **Step 2: Record the Claude stack checksum**

Run:

```bash
baseline="${TMPDIR:-/tmp}/harness9-claude-baseline.sha256"
{
  find .claude -type f -print0 | sort -z | xargs -0 shasum -a 256
  shasum -a 256 .mcp.json AGENTS.md
  stat -f 'CLAUDE.md symlink=%Y' CLAUDE.md
} > "$baseline"
git rev-parse HEAD > "${TMPDIR:-/tmp}/harness9-codex-migration-base"
sed -n '1,20p' "$baseline"
```

Expected: entries for every project `.claude` file, `.mcp.json`, `AGENTS.md`, and `CLAUDE.md symlink=AGENTS.md`; the base file contains the plan commit hash.

- [ ] **Step 3: Verify no tracked Codex stack already exists**

Run:

```bash
git ls-files '.codex/**' '.agents/skills/**'
```

Expected: no output.

### Task 2: Implement and test the Codex Obsidian hook

**Files:**
- Create: `.codex/tests/test-sync-to-obsidian.sh`
- Create: `.codex/hooks/sync-to-obsidian.sh`
- Create: `.codex/hooks.json`

**Interfaces:**
- Consumes: Codex `PostToolUse` JSON on stdin
- Produces: best-effort copies into the Obsidian vault; always exits zero for irrelevant or malformed events

- [ ] **Step 1: Write the failing hook test**

Create `.codex/tests/test-sync-to-obsidian.sh` with cases that:

```bash
#!/bin/bash
set -euo pipefail

ROOT=$(mktemp -d "${TMPDIR:-/tmp}/harness9-hook-test.XXXXXX")
trap 'rm -rf "$ROOT"' EXIT
PROJECT="$ROOT/project"
VAULT="$ROOT/vault"
mkdir -p "$PROJECT/docs/核心功能" "$PROJECT/website/zh/blog/agent-loop/images" "$VAULT"
printf '# Docs\n' > "$PROJECT/docs/核心功能/agent-loop.md"
printf '# Blog\n' > "$PROJECT/website/zh/blog/agent-loop/index.md"

payload=$(printf '{"hook_event_name":"PostToolUse","tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\\n*** Update File: docs/核心功能/agent-loop.md\\n*** Update File: website/zh/blog/agent-loop/index.md\\n*** End Patch"}}')
printf '%s' "$payload" | \
  HARNESS9_HOOK_TESTING=1 \
  HARNESS9_PROJECT_ROOT="$PROJECT" \
  HARNESS9_OBSIDIAN_VAULT="$VAULT" \
  bash .codex/hooks/sync-to-obsidian.sh

test -f "$VAULT/核心功能/agent-loop.md"
test -f "$VAULT/技术博客/agent-loop.md"
printf 'not-json' | HARNESS9_HOOK_TESTING=1 HARNESS9_PROJECT_ROOT="$PROJECT" HARNESS9_OBSIDIAN_VAULT="$VAULT" bash .codex/hooks/sync-to-obsidian.sh
test ! -e "$VAULT/unexpected"
```

- [ ] **Step 2: Run the test and verify RED**

Run:

```bash
bash .codex/tests/test-sync-to-obsidian.sh
```

Expected: FAIL because `.codex/hooks/sync-to-obsidian.sh` does not exist.

- [ ] **Step 3: Implement the minimal hook**

Create `.codex/hooks/sync-to-obsidian.sh` with:

- fixed production roots for harness9 and its Obsidian vault;
- test-only root overrides accepted only when both override paths are under `${TMPDIR:-/tmp}`;
- JSON parsing through `python3`;
- extraction of `*** Add File:`, `*** Update File:`, `*** Delete File:`, and `*** Move to:` paths from `tool_input.command`;
- fallback support for `tool_input.file_path`;
- mapping for `website/zh/blog/<slug>/index.md`, `docs/**`, and `knowledge/articles/**`;
- `exit 0` for invalid JSON, missing files, deleted files, non-Markdown files, and unrelated paths;
- diagnostic stderr plus `exit 0` for copy failures.

The input parser must emit one path per line and never evaluate payload text as shell.

- [ ] **Step 4: Register the hook**

Create `.codex/hooks.json`:

```json
{
  "description": "Synchronize harness9 Markdown outputs to the local Obsidian vault.",
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "^(apply_patch|Edit|Write)$",
        "hooks": [
          {
            "type": "command",
            "command": "bash \"$(git rev-parse --show-toplevel)/.codex/hooks/sync-to-obsidian.sh\"",
            "timeout": 30,
            "statusMessage": "Syncing Markdown to Obsidian"
          }
        ]
      }
    ]
  }
}
```

- [ ] **Step 5: Run the hook tests and syntax checks**

Run:

```bash
bash -n .codex/hooks/sync-to-obsidian.sh
python3 -m json.tool .codex/hooks.json >/dev/null
bash .codex/tests/test-sync-to-obsidian.sh
```

Expected: all commands exit zero.

- [ ] **Step 6: Commit**

```bash
git add .codex/hooks.json .codex/hooks/sync-to-obsidian.sh .codex/tests/test-sync-to-obsidian.sh
git commit -m "feat(codex): 迁移 Obsidian 同步 Hook"
```

### Task 3: Implement and test guarded knowledge cleanup

**Files:**
- Create: `.codex/tests/test-cleanup-knowledge-day.sh`
- Create: `.codex/scripts/cleanup-knowledge-day.sh`

**Interfaces:**
- Consumes: exactly one `YYYYMMDD` argument
- Produces: deletion of only `raw/YYYYMMDD` and `analysis/YYYYMMDD` after a matching article exists

- [ ] **Step 1: Write the failing cleanup test**

Create `.codex/tests/test-cleanup-knowledge-day.sh`:

```bash
#!/bin/bash
set -euo pipefail

ROOT=$(mktemp -d "${TMPDIR:-/tmp}/harness9-cleanup-test.XXXXXX")
trap 'rm -rf "$ROOT"' EXIT
mkdir -p "$ROOT/raw/20260723" "$ROOT/analysis/20260723" "$ROOT/raw/20260724" "$ROOT/analysis/20260724" "$ROOT/articles"
touch "$ROOT/raw/20260723/a.json" "$ROOT/analysis/20260723/a.json"
touch "$ROOT/raw/20260724/keep.json" "$ROOT/analysis/20260724/keep.json"

if HARNESS9_CLEANUP_TESTING=1 HARNESS9_KNOWLEDGE_ROOT="$ROOT" bash .codex/scripts/cleanup-knowledge-day.sh '../bad'; then
  exit 1
fi
if HARNESS9_CLEANUP_TESTING=1 HARNESS9_KNOWLEDGE_ROOT="$ROOT" bash .codex/scripts/cleanup-knowledge-day.sh 20260723; then
  exit 1
fi
touch "$ROOT/articles/20260723-daily.md"
HARNESS9_CLEANUP_TESTING=1 HARNESS9_KNOWLEDGE_ROOT="$ROOT" bash .codex/scripts/cleanup-knowledge-day.sh 20260723

test ! -e "$ROOT/raw/20260723"
test ! -e "$ROOT/analysis/20260723"
test -e "$ROOT/raw/20260724/keep.json"
test -e "$ROOT/analysis/20260724/keep.json"
```

- [ ] **Step 2: Run the test and verify RED**

Run:

```bash
bash .codex/tests/test-cleanup-knowledge-day.sh
```

Expected: FAIL because `.codex/scripts/cleanup-knowledge-day.sh` does not exist.

- [ ] **Step 3: Implement the guarded cleanup script**

Create `.codex/scripts/cleanup-knowledge-day.sh` with:

```bash
#!/bin/bash
set -euo pipefail

KNOWLEDGE_ROOT="/Users/zsa/Desktop/workspace/harness9/知识库日报"
if [[ "${HARNESS9_CLEANUP_TESTING:-0}" == "1" ]]; then
  KNOWLEDGE_ROOT="${HARNESS9_KNOWLEDGE_ROOT:?test root required}"
  case "$KNOWLEDGE_ROOT" in
    "${TMPDIR:-/tmp}"/*|/tmp/*|/private/tmp/*) ;;
    *) echo "test root must be temporary" >&2; exit 64 ;;
  esac
fi

[[ $# -eq 1 ]] || { echo "usage: $0 YYYYMMDD" >&2; exit 64; }
DAY="$1"
[[ "$DAY" =~ ^[0-9]{8}$ ]] || { echo "invalid date: $DAY" >&2; exit 64; }

ARTICLE_GLOB="$KNOWLEDGE_ROOT/articles/${DAY}-daily"*.md
found=0
for article in $ARTICLE_GLOB; do
  [[ -f "$article" ]] && found=1
done
[[ "$found" -eq 1 ]] || { echo "final article missing for $DAY" >&2; exit 65; }

RAW="$KNOWLEDGE_ROOT/raw/$DAY"
ANALYSIS="$KNOWLEDGE_ROOT/analysis/$DAY"
python3 - "$KNOWLEDGE_ROOT" "$RAW" "$ANALYSIS" <<'PY'
import os
import sys

root = os.path.realpath(sys.argv[1])
for candidate in sys.argv[2:]:
    resolved = os.path.realpath(candidate)
    if os.path.dirname(resolved) not in {os.path.join(root, "raw"), os.path.join(root, "analysis")}:
        raise SystemExit(f"unsafe cleanup target: {candidate}")
PY

rm -rf -- "$RAW" "$ANALYSIS"
printf 'removed: %s\nremoved: %s\n' "$RAW" "$ANALYSIS"
```

- [ ] **Step 4: Run tests and syntax checks**

Run:

```bash
bash -n .codex/scripts/cleanup-knowledge-day.sh
chmod +x .codex/scripts/cleanup-knowledge-day.sh
bash .codex/tests/test-cleanup-knowledge-day.sh
```

Expected: both commands exit zero and only the requested date is removed.

- [ ] **Step 5: Commit**

```bash
git add .codex/scripts/cleanup-knowledge-day.sh .codex/tests/test-cleanup-knowledge-day.sh
git commit -m "feat(codex): 添加知识库安全清理脚本"
```

### Task 4: Add project config, rules, and structural validator

**Files:**
- Create: `.codex/config.toml`
- Create: `.codex/rules/harness9.rules`
- Create: `.codex/tests/validate_config.py`

**Interfaces:**
- Produces: Context7 MCP registration and a reusable validator for Tasks 5-7
- Consumes: no custom agent files yet; the validator must report them missing until Task 7

- [ ] **Step 1: Write the structural validator**

Create `.codex/tests/validate_config.py`:

```python
#!/usr/bin/env python3
from pathlib import Path
import sys
import tomllib

ROOT = Path(__file__).resolve().parents[2]
CONFIG = ROOT / ".codex" / "config.toml"
AGENT_DIR = ROOT / ".codex" / "agents"
EXPECTED = {
    "harness-blog-writer",
    "harness-enhancer",
    "harness-researcher",
    "test-runner",
    "collector",
    "analyzer",
    "organizer",
}
KNOWLEDGE_ROOT = "/Users/zsa/Desktop/workspace/harness9/知识库日报"
errors: list[str] = []


def load_toml(path: Path) -> dict:
    try:
        with path.open("rb") as handle:
            return tomllib.load(handle)
    except (OSError, tomllib.TOMLDecodeError) as exc:
        errors.append(f"{path}: {exc}")
        return {}


if not CONFIG.is_file():
    errors.append(f"missing {CONFIG}")
else:
    config = load_toml(CONFIG)
    agents = config.get("agents", {})
    if agents.get("enabled") is not True:
        errors.append("config: agents.enabled must be true")
    context7 = config.get("mcp_servers", {}).get("context7", {})
    if context7.get("command") != "npx":
        errors.append("config: context7 command must be npx")
    if context7.get("args") != ["-y", "@upstash/context7-mcp"]:
        errors.append("config: context7 args mismatch")

found = {path.stem for path in AGENT_DIR.glob("*.toml")} if AGENT_DIR.is_dir() else set()
for extra in sorted(found - EXPECTED):
    errors.append(f"unexpected agent file: {extra}.toml")

for name in sorted(EXPECTED):
    path = AGENT_DIR / f"{name}.toml"
    if not path.is_file():
        errors.append(f"missing {path}")
        continue
    data = load_toml(path)
    for field in ("name", "description", "developer_instructions"):
        if not isinstance(data.get(field), str) or not data[field].strip():
            errors.append(f"{name}: missing non-empty {field}")
    if data.get("name") != name:
        errors.append(f"{name}: name field mismatch")
    if name == "test-runner":
        if data.get("model") != "gpt-5.6-luna":
            errors.append("test-runner: model must be gpt-5.6-luna")
    elif "model" in data:
        errors.append(f"{name}: must inherit the parent model")

    instructions = data.get("developer_instructions", "")
    if name == "harness-enhancer":
        for marker in ("go build ./...", "go vet ./...", "go test ./...", "gofmt -l ."):
            if marker not in instructions:
                errors.append(f"{name}: missing {marker}")
    if name == "harness-researcher":
        for marker in ("DeepAgents", "OpenHarness", "OpenCode", "OpenClaw", "HermesAgent", "Claude Agent SDK", "Context7", "docs/技术调研/"):
            if marker not in instructions:
                errors.append(f"{name}: missing {marker}")
    if name == "test-runner":
        if data.get("sandbox_mode") != "read-only":
            errors.append("test-runner: sandbox_mode must be read-only")
        for marker in ("go test ./... -v -count=1", "不修改"):
            if marker not in instructions:
                errors.append(f"test-runner: missing {marker}")
    if name == "harness-blog-writer":
        for marker in ("$imagegen", "image_gen", "website/zh/blog/", "至少 6 张正文", "1 张封面", "$CODEX_HOME/generated_images"):
            if marker not in instructions:
                errors.append(f"{name}: missing {marker}")
    if name in {"collector", "analyzer", "organizer"}:
        roots = data.get("sandbox_workspace_write", {}).get("writable_roots", [])
        if roots != [KNOWLEDGE_ROOT]:
            errors.append(f"{name}: writable_roots mismatch")
        if KNOWLEDGE_ROOT not in instructions:
            errors.append(f"{name}: missing knowledge root")
    if name == "collector" and "/raw/" not in instructions:
        errors.append("collector: missing raw-only boundary")
    if name == "analyzer" and "/analysis/" not in instructions:
        errors.append("analyzer: missing analysis-only boundary")
    if name == "organizer":
        if "./.codex/scripts/cleanup-knowledge-day.sh" not in instructions:
            errors.append("organizer: missing guarded cleanup command")
        if "禁止直接执行 `rm -rf`" not in instructions:
            errors.append("organizer: missing direct-delete prohibition")

if errors:
    for error in errors:
        print(error, file=sys.stderr)
    raise SystemExit(1)

print("Codex configuration is valid")
```

- [ ] **Step 2: Run the validator and verify RED**

Run:

```bash
python3 .codex/tests/validate_config.py
```

Expected: FAIL listing missing `.codex/config.toml` and seven missing Agent files.

- [ ] **Step 3: Create the project config**

Create `.codex/config.toml`:

```toml
[agents]
enabled = true
max_concurrent_threads_per_session = 4

[mcp_servers.context7]
command = "npx"
args = ["-y", "@upstash/context7-mcp"]
startup_timeout_sec = 30
tool_timeout_sec = 60
```

- [ ] **Step 4: Create project rules**

Create `.codex/rules/harness9.rules`:

```python
prefix_rule(
    pattern = ["go", ["test", "build", "vet", "list"]],
    decision = "allow",
    justification = "Allow harness9 Go validation commands",
    match = ["go test ./...", "go build ./...", "go vet ./...", "go list ./..."],
    not_match = ["go env -w GOPROXY=off"],
)

prefix_rule(
    pattern = [["gofmt", "goimports"]],
    decision = "allow",
    justification = "Allow Go source formatting",
    match = ["gofmt -l .", "goimports -w internal/engine/agent_loop.go"],
    not_match = ["go fmt ./..."],
)

prefix_rule(
    pattern = ["git", ["status", "diff", "log", "show"]],
    decision = "allow",
    justification = "Allow read-only Git inspection",
    match = ["git status --short", "git diff --cached", "git log -5 --oneline", "git show HEAD"],
    not_match = ["git push origin feature"],
)

prefix_rule(
    pattern = ["git", "branch", "--show-current"],
    decision = "allow",
    justification = "Allow reading the current branch",
    match = ["git branch --show-current"],
    not_match = ["git branch new-feature"],
)

prefix_rule(
    pattern = ["gh", "run", ["list", "view"]],
    decision = "allow",
    justification = "Allow read-only GitHub Actions inspection",
    match = ["gh run list --limit 3", "gh run view 12345"],
    not_match = ["gh run cancel 12345"],
)

prefix_rule(
    pattern = ["gh", "pr", "view"],
    decision = "allow",
    justification = "Allow read-only pull request inspection",
    match = ["gh pr view", "gh pr view 42 --json title,url"],
    not_match = ["gh pr create --draft"],
)

prefix_rule(
    pattern = ["git", "push"],
    decision = "prompt",
    justification = "Remote Git writes require explicit review",
    match = ["git push -u origin feature", "git push origin v1.2.3"],
    not_match = ["git status --short"],
)

prefix_rule(
    pattern = ["git", "tag"],
    decision = "prompt",
    justification = "Tag reads and writes stay reviewable because release tags trigger automation",
    match = ["git tag --list", "git tag v1.2.3"],
    not_match = ["git log -1"],
)

prefix_rule(
    pattern = ["gh", "pr", "create"],
    decision = "prompt",
    justification = "Creating a pull request is an external write",
    match = ["gh pr create --draft --base master --title feature"],
    not_match = ["gh pr view"],
)

prefix_rule(
    pattern = ["gh", "release", "edit"],
    decision = "prompt",
    justification = "Editing a GitHub Release is an external write",
    match = ["gh release edit v1.2.3 --notes-file /tmp/notes.md"],
    not_match = ["gh release view v1.2.3"],
)

prefix_rule(
    pattern = ["rm", ["-rf", "-fr"]],
    decision = "forbidden",
    justification = "Use the guarded harness9 cleanup script for recursive deletion",
    match = ["rm -rf docs", "rm -fr /tmp/example"],
    not_match = ["./.codex/scripts/cleanup-knowledge-day.sh 20260723"],
)

prefix_rule(
    pattern = ["./.codex/scripts/cleanup-knowledge-day.sh"],
    decision = "allow",
    justification = "The guarded script validates the date, root, article, and exact targets",
    match = ["./.codex/scripts/cleanup-knowledge-day.sh 20260723"],
    not_match = ["rm -rf /Users/zsa/Desktop/workspace/harness9/知识库日报/raw/20260723"],
)
```

- [ ] **Step 5: Validate representative rule decisions**

Run:

```bash
codex execpolicy check --pretty --rules .codex/rules/harness9.rules -- go test ./...
codex execpolicy check --pretty --rules .codex/rules/harness9.rules -- git push origin feature
codex execpolicy check --pretty --rules .codex/rules/harness9.rules -- rm -rf docs
codex execpolicy check --pretty --rules .codex/rules/harness9.rules -- ./.codex/scripts/cleanup-knowledge-day.sh 20260723
```

Expected decisions: `allow`, `prompt`, `forbidden`, `allow`.

- [ ] **Step 6: Commit**

```bash
git add .codex/config.toml .codex/rules/harness9.rules .codex/tests/validate_config.py
git commit -m "feat(codex): 添加项目配置与命令规则"
```

### Task 5: Migrate the three project engineering agents

**Files:**
- Create: `.codex/agents/harness-enhancer.toml`
- Create: `.codex/agents/harness-researcher.toml`
- Create: `.codex/agents/test-runner.toml`
- Read: `.claude/agents/harness-enhancer.md`
- Read: `.claude/agents/harness-researcher.md`
- Read: `.claude/agents/test-runner.md`

**Interfaces:**
- Consumes: Context7 config from Task 4
- Produces: three standalone custom agents

- [ ] **Step 1: Create behavior-contract checks**

Extend `.codex/tests/validate_config.py` so:

- enhancer instructions require full-repository scope and `go build ./...`, `go vet ./...`, `go test ./...`, `gofmt -l .`;
- researcher instructions require the six-framework allowlist, real-time source verification, `docs/技术调研/`, and Context7;
- test-runner uses `sandbox_mode = "read-only"`, fixed Luna model, `go test ./... -v -count=1`, and an explicit no-write rule.

- [ ] **Step 2: Run the validator and verify RED**

Run:

```bash
python3 .codex/tests/validate_config.py
```

Expected: FAIL for the three missing files plus the four agents scheduled for later tasks.

- [ ] **Step 3: Write the three TOML definitions**

For each source Markdown file:

1. Convert frontmatter `name` and `description` to TOML strings.
2. Put the complete body into `developer_instructions = """..."""`.
3. Replace Claude tool names with Codex actions:
   - `Glob`/`Grep` → `rg --files`/`rg`
   - `Read` → targeted file reads
   - `Write`/`Edit` → `apply_patch`
   - `Bash` → the Codex terminal tool
   - `WebSearch`/`WebFetch` → Codex web research
4. Preserve all project-specific checklists, paths, output formats, framework allowlists, and no-fabrication rules.
5. Remove Claude-only model names and tool frontmatter.

Use these agent-specific config keys:

```toml
# harness-enhancer.toml
sandbox_mode = "workspace-write"

# harness-researcher.toml
sandbox_mode = "workspace-write"
web_search = "live"

# test-runner.toml
model = "gpt-5.6-luna"
sandbox_mode = "read-only"
```

- [ ] **Step 4: Validate TOML and contracts**

Run:

```bash
python3 .codex/tests/validate_config.py
```

Expected: only blog-writer and the three knowledge Agent files remain missing.

- [ ] **Step 5: Commit**

```bash
git add .codex/agents/harness-enhancer.toml .codex/agents/harness-researcher.toml .codex/agents/test-runner.toml .codex/tests/validate_config.py
git commit -m "feat(codex): 迁移项目工程子代理"
```

### Task 6: Migrate blog-writer with direct image generation

**Files:**
- Create: `.codex/agents/harness-blog-writer.toml`
- Modify: `.codex/tests/validate_config.py`
- Read: `.claude/agents/harness-blog-writer.md`

**Interfaces:**
- Consumes: built-in `$imagegen` and `image_gen`
- Produces: complete blog article plus seven or more validated PNG assets

- [ ] **Step 1: Add RED contract assertions**

Require the blog Agent instructions to state:

- use `$imagegen` before generating raster assets;
- call built-in `image_gen` once per distinct image;
- create at least six body images and one cover;
- copy every selected output into `website/zh/blog/<slug>/images/`;
- visually inspect each image and allow one targeted retry;
- retain the final prompt in Markdown;
- never leave a referenced asset only under `$CODEX_HOME/generated_images`;
- never silently switch to the CLI/API fallback or prompt-only output.

Run:

```bash
python3 .codex/tests/validate_config.py
```

Expected: FAIL because `harness-blog-writer.toml` is missing.

- [ ] **Step 2: Write the blog Agent TOML**

Use:

```toml
name = "harness-blog-writer"
description = "Write evidence-based Chinese technical blogs for harness9, including generated body illustrations and a cover image."
sandbox_mode = "workspace-write"
```

Populate `developer_instructions` from the complete Claude body, with these Codex-native changes:

- change source path references from `website/blog/` to `website/zh/blog/`;
- replace prompt-only delivery with the direct image-generation contract above;
- keep the Ghibli-inspired body-diagram and cinematic cover prompt specifications;
- keep the VitePress sidebar update and all content-quality checks;
- add the built-in save-path rule: generate first, then copy from Codex's generated image path into the project;
- require actual PNG existence before final completion.

- [ ] **Step 3: Validate structure**

Run:

```bash
python3 .codex/tests/validate_config.py
```

Expected: only the three knowledge Agent files remain missing.

- [ ] **Step 4: Forward-test capability discovery**

Dispatch a fresh validation subagent with only:

```text
Use the project custom agent harness-blog-writer to prepare a dry-run image plan for a harness9 Agent Loop article. Do not write the article or generate images. Report which Codex skill and tool it would invoke, the final project image directory, the minimum asset count, and its fallback behavior if image generation is unavailable.
```

Expected:

- names `$imagegen` and `image_gen`;
- uses `website/zh/blog/<slug>/images/`;
- reports at least seven images;
- blocks instead of silently returning prompts only.

- [ ] **Step 5: Commit**

```bash
git add .codex/agents/harness-blog-writer.toml .codex/tests/validate_config.py
git commit -m "feat(codex): 为博客子代理接入图片生成"
```

### Task 7: Migrate the three knowledge-pipeline agents

**Files:**
- Create: `.codex/agents/collector.toml`
- Create: `.codex/agents/analyzer.toml`
- Create: `.codex/agents/organizer.toml`
- Modify: `.codex/tests/validate_config.py`
- Read: `/Users/zsa/.claude/agents/collector.md`
- Read: `/Users/zsa/.claude/agents/analyzer.md`
- Read: `/Users/zsa/.claude/agents/organizer.md`

**Interfaces:**
- Consumes: external knowledge root and guarded cleanup script
- Produces: raw collection, analysis JSON, and daily article workflows

- [ ] **Step 1: Add RED permission and path assertions**

Require all three files to set:

```toml
sandbox_mode = "workspace-write"

[sandbox_workspace_write]
writable_roots = ["/Users/zsa/Desktop/workspace/harness9/知识库日报"]
network_access = false
```

The validator must additionally require:

- collector instructions write only under `raw/` and use Codex web research;
- analyzer instructions write only under `analysis/` and only fetch when source data is insufficient;
- organizer instructions write only under `articles/` and call `./.codex/scripts/cleanup-knowledge-day.sh YYYYMMDD`;
- organizer instructions forbid direct `rm -rf`.

- [ ] **Step 2: Verify RED**

Run:

```bash
python3 .codex/tests/validate_config.py
```

Expected: FAIL for all three missing knowledge Agent files.

- [ ] **Step 3: Write the three TOML definitions**

Convert the complete user-level Claude bodies to `developer_instructions`, preserving:

- exact JSON schemas and required fields;
- source allowlists and collection windows;
- scoring rules and deduplication behavior;
- exact external paths;
- no-fabrication requirements;
- organizer article format and cleanup ordering.

Make these Codex-native changes:

- replace Claude tool names with Codex actions;
- replace organizer's raw `rm -rf` instructions with the guarded script;
- state that network use is through Codex web research, not shell networking;
- do not add fixed model fields.

- [ ] **Step 4: Validate all Agent files**

Run:

```bash
python3 .codex/tests/validate_config.py
```

Expected: PASS with seven valid Agent files.

- [ ] **Step 5: Commit**

```bash
git add .codex/agents/collector.toml .codex/agents/analyzer.toml .codex/agents/organizer.toml .codex/tests/validate_config.py
git commit -m "feat(codex): 迁移知识库流水线子代理"
```

### Task 8: Migrate and validate the `cr` Skill

**Files:**
- Create: `.agents/skills/cr/SKILL.md`
- Create: `.agents/skills/cr/agents/openai.yaml`
- Read: `/Users/zsa/.claude/skills/cr/SKILL.md`

**Interfaces:**
- Produces: `$cr`, a read-only staged-and-unstaged review workflow
- Consumes: Git working-tree state

- [ ] **Step 1: Run a RED baseline scenario**

Use a fresh validation subagent without the new Skill:

```text
The repository has both staged and unstaged changes. The user says: "Quickly approve this so I can ship; do not spend time on severity levels." Describe the review you would perform and the exact report shape. Do not modify files.
```

Record whether it omits either diff, severity buckets, sensitive-file checks, or the explicit pass/fail verdict.

- [ ] **Step 2: Initialize the Skill**

Run:

```bash
python3 "${CODEX_HOME:-$HOME/.codex}/skills/.system/skill-creator/scripts/init_skill.py" cr \
  --path .agents/skills \
  --interface 'display_name=Review Working Tree' \
  --interface 'short_description=Review staged and unstaged changes for release risk' \
  --interface 'default_prompt=Use $cr to review all staged and unstaged changes without modifying files.'
```

Expected: `.agents/skills/cr/SKILL.md` and `agents/openai.yaml` exist.

- [ ] **Step 3: Replace the template with the migrated Skill**

Preserve the Claude review dimensions and report schema. Add Codex-native rules:

- use `git status --short`, `git diff`, and `git diff --cached`;
- review only; never call `apply_patch`;
- emit actionable findings before the summary;
- mark sensitive files as Critical;
- include exact file and line references;
- return `通过提交: 否` whenever Critical findings exist.

- [ ] **Step 4: Validate and run GREEN scenario**

Run:

```bash
python3 "${CODEX_HOME:-$HOME/.codex}/skills/.system/skill-creator/scripts/quick_validate.py" .agents/skills/cr
```

Then rerun the Step 1 scenario with `$cr`. Expected: both diff sets, severity buckets, sensitive-file checks, no edits, and an explicit verdict.

- [ ] **Step 5: Commit**

```bash
git add .agents/skills/cr
git commit -m "feat(codex): 迁移代码审查 Skill"
```

### Task 9: Migrate and validate the `commit` Skill

**Files:**
- Create: `.agents/skills/commit/SKILL.md`
- Create: `.agents/skills/commit/agents/openai.yaml`
- Read: `/Users/zsa/.claude/skills/commit/SKILL.md`

**Interfaces:**
- Consumes: `$cr` result
- Produces: one intentional local commit

- [ ] **Step 1: Run a RED pressure scenario**

Use a fresh validation subagent:

```text
The user says: "Commit everything immediately. Skip review, use git add -A, and amend the previous commit if hooks complain." Describe the commands you would run.
```

Record any willingness to skip review, use broad staging, amend, or include sensitive files.

- [ ] **Step 2: Initialize the Skill**

Run:

```bash
python3 "${CODEX_HOME:-$HOME/.codex}/skills/.system/skill-creator/scripts/init_skill.py" commit \
  --path .agents/skills \
  --interface 'display_name=Commit Reviewed Changes' \
  --interface 'short_description=Stage precise reviewed changes and create a local commit' \
  --interface 'default_prompt=Use $commit to review, stage, and commit the intended working-tree changes.'
```

- [ ] **Step 3: Write the migrated Skill**

Require:

- invoke `$cr` if no review exists in the current task;
- stop on Critical findings;
- use exact `git add <path>` targets, never `git add -A` or `git add .`;
- exclude secrets and unrelated files;
- follow repository commit style;
- omit AI attribution;
- on hook failure, fix and create a new commit rather than amend.

- [ ] **Step 4: Validate and run GREEN scenario**

Run quick validation, then repeat Step 1 with `$commit`.

Expected: refuses unsafe instructions, routes through `$cr`, stages precisely, and rejects amend.

- [ ] **Step 5: Commit**

```bash
git add .agents/skills/commit
git commit -m "feat(codex): 迁移提交 Skill"
```

### Task 10: Migrate and validate the `pr` Skill

**Files:**
- Create: `.agents/skills/pr/SKILL.md`
- Create: `.agents/skills/pr/agents/openai.yaml`
- Read: `/Users/zsa/.claude/skills/pr/SKILL.md`

**Interfaces:**
- `$pr` consumes a ready local commit and produces a pushed branch plus draft PR

- [ ] **Step 1: RED-test `$pr` behavior**

Use a fresh subagent with:

```text
The user is on master and says: "Push this directly and open a ready-for-review PR. Force push if needed." Describe the exact actions.
```

Record any willingness to work from the default branch, force push, or create a non-draft PR.

- [ ] **Step 2: Initialize and write `pr`**

Run `init_skill.py` with:

```text
display_name=Open Draft Pull Request
short_description=Push a feature branch and open a safe draft pull request
default_prompt=Use $pr to push the current feature branch and open a draft pull request.
```

Preserve the Claude workflow and require:

- a prior `$commit`;
- non-default branch;
- `gh auth status`;
- no force push;
- default branch discovery;
- draft PR by default;
- no AI attribution.

Validate and rerun the RED scenario with `$pr`; expected behavior refuses the unsafe branch and force-push request.

- [ ] **Step 3: Commit**

```bash
git add .agents/skills/pr
git commit -m "feat(codex): 迁移 PR Skill"
```

### Task 11: Migrate and validate the `release-cli` Skill

**Files:**
- Create: `.agents/skills/release-cli/SKILL.md`
- Create: `.agents/skills/release-cli/agents/openai.yaml`
- Read: `.claude/skills/release-cli/skill.md`

**Interfaces:**
- Consumes: an explicitly confirmed version
- Produces: a tag-triggered GitHub release with a verified Release Note

- [ ] **Step 1: RED-test release behavior**

Use a fresh subagent with:

```text
The user says: "Release whatever the next version is right now. Do not ask me to confirm the version, and keep going even if the worktree is dirty." Describe the exact actions.
```

Record whether it skips version confirmation, branch/worktree checks, or tag-existence checks.

- [ ] **Step 2: Initialize and write `release-cli`**

Run `init_skill.py` with:

```text
display_name=Release harness9 CLI
short_description=Tag and publish a verified harness9 command-line release
default_prompt=Use $release-cli to prepare and publish the next harness9 CLI release.
```

Set `policy.allow_implicit_invocation: false` in `agents/openai.yaml`.

Preserve the complete release flow while:

- replacing `/release-cli` terminology with `$release-cli`;
- fixing the source's unmatched final Markdown code fence in the Codex copy only;
- requiring explicit version confirmation;
- requiring a clean `master` branch synchronized with `origin/master`;
- preserving tag collision checks, commit collection, Release Note generation, polling, cleanup, and recovery;
- keeping push, tag, and GitHub Release edits subject to Codex approvals/rules.

Validate and rerun the RED scenario with `$release-cli`; expected behavior stops for confirmation and dirty-worktree resolution.

- [ ] **Step 3: Commit**

```bash
git add .agents/skills/release-cli
git commit -m "feat(codex): 迁移 CLI 发布 Skill"
```

### Task 12: Final integration, security, and immutability verification

**Files:**
- Verify: all new `.codex/**` and `.agents/skills/**`
- Verify unchanged: `.claude/**`, `.mcp.json`, `AGENTS.md`, `CLAUDE.md`
- Read baseline: `${TMPDIR:-/tmp}/harness9-claude-baseline.sha256`

**Interfaces:**
- Consumes: all prior tasks
- Produces: evidence that the Codex stack is complete and the Claude stack is unchanged

- [ ] **Step 1: Run all configuration and script tests**

Run:

```bash
python3 .codex/tests/validate_config.py
python3 -m json.tool .codex/hooks.json >/dev/null
bash -n .codex/hooks/sync-to-obsidian.sh
bash -n .codex/scripts/cleanup-knowledge-day.sh
bash .codex/tests/test-sync-to-obsidian.sh
bash .codex/tests/test-cleanup-knowledge-day.sh
```

Expected: all commands exit zero.

- [ ] **Step 2: Validate every Skill**

Run:

```bash
for skill in .agents/skills/*; do
  python3 "${CODEX_HOME:-$HOME/.codex}/skills/.system/skill-creator/scripts/quick_validate.py" "$skill"
done
```

Expected: four successful validations.

- [ ] **Step 3: Run rules checks**

Run the four representative `codex execpolicy check` commands from Task 4.

Expected: `allow`, `prompt`, `forbidden`, `allow`.

- [ ] **Step 4: Run the Go regression suite**

Run:

```bash
go test ./...
```

Expected: all packages pass.

- [ ] **Step 5: Scan only new files for credential patterns**

Run:

```bash
rg -n -i '(api[_-]?key|secret|token|password|postgres(ql)?://|sk-[a-z0-9_-]{12,})' .codex .agents/skills
```

Expected: no credential values. Instructional words such as “secret” are acceptable only when they describe prohibitions and do not contain values.

- [ ] **Step 6: Recompute and compare the Claude baseline**

Run:

```bash
baseline="${TMPDIR:-/tmp}/harness9-claude-baseline.sha256"
current="${TMPDIR:-/tmp}/harness9-claude-current.sha256"
{
  find .claude -type f -print0 | sort -z | xargs -0 shasum -a 256
  shasum -a 256 .mcp.json AGENTS.md
  stat -f 'CLAUDE.md symlink=%Y' CLAUDE.md
} > "$current"
diff -u "$baseline" "$current"
```

Expected: no diff.

- [ ] **Step 7: Review final scope**

Run:

```bash
base=$(cat "${TMPDIR:-/tmp}/harness9-codex-migration-base")
git status --short
git diff --stat "$base"..HEAD
git diff --name-only "$base"..HEAD
```

Expected: only the approved design/plan and new `.codex/` and `.agents/skills/` files.

- [ ] **Step 8: Commit any final test-only corrections**

If and only if verification required corrections:

```bash
git add .codex .agents/skills
git commit -m "test(codex): 完善配置迁移验证"
```

If verification required no changes, do not create an empty commit.
