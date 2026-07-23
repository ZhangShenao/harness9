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
