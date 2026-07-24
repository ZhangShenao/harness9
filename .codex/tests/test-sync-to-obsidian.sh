#!/bin/bash
set -euo pipefail

ROOT=$(mktemp -d /tmp/harness9-hook-test.XXXXXX)
trap 'rm -rf "$ROOT"' EXIT
PROJECT="$ROOT/project"
VAULT="$ROOT/vault"
mkdir -p \
	"$PROJECT/docs/核心功能" \
	"$PROJECT/website/zh/blog/agent-loop/images" \
	"$PROJECT/knowledge/articles" \
	"$VAULT"
printf '# Docs\n' > "$PROJECT/docs/核心功能/agent-loop.md"
printf '# Blog\n' > "$PROJECT/website/zh/blog/agent-loop/index.md"
printf '# Added\n' > "$PROJECT/docs/added.md"
printf '# Updated\n' > "$PROJECT/docs/updated.md"
printf '# Moved\n' > "$PROJECT/docs/moved.md"
printf '# Article\n' > "$PROJECT/knowledge/articles/20260724-daily.md"
printf '# Retained\n' > "$VAULT/deleted.md"

payload=$(printf '{"hook_event_name":"PostToolUse","tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\\n*** Add File: docs/added.md\\n*** Update File: docs/updated.md\\n*** Move to: docs/moved.md\\n*** Delete File: docs/deleted.md\\n*** Update File: docs/核心功能/agent-loop.md\\n*** Update File: website/zh/blog/agent-loop/index.md\\n*** End Patch"}}')
printf '%s' "$payload" | \
	TMPDIR="$PWD" \
	HARNESS9_HOOK_TESTING=1 \
	HARNESS9_PROJECT_ROOT="$PROJECT" \
	HARNESS9_OBSIDIAN_VAULT="$VAULT" \
	bash .codex/hooks/sync-to-obsidian.sh

test -f "$VAULT/核心功能/agent-loop.md"
test -f "$VAULT/技术博客/agent-loop.md"
test "$(cat "$VAULT/added.md")" = "# Added"
test "$(cat "$VAULT/updated.md")" = "# Updated"
test "$(cat "$VAULT/moved.md")" = "# Moved"
test "$(cat "$VAULT/deleted.md")" = "# Retained"

file_payload=$(printf '{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"knowledge/articles/20260724-daily.md"}}')
printf '%s' "$file_payload" |
	HARNESS9_HOOK_TESTING=1 \
	HARNESS9_PROJECT_ROOT="$PROJECT" \
	HARNESS9_OBSIDIAN_VAULT="$VAULT" \
	bash .codex/hooks/sync-to-obsidian.sh
test "$(cat "$VAULT/知识库日报/20260724-daily.md")" = "# Article"

printf 'not-json' |
	HARNESS9_HOOK_TESTING=1 \
	HARNESS9_PROJECT_ROOT="$PROJECT" \
	HARNESS9_OBSIDIAN_VAULT="$VAULT" \
	bash .codex/hooks/sync-to-obsidian.sh
test ! -e "$VAULT/unexpected"

printf '# Override symlink\n' > "$PROJECT/docs/override-symlink.md"
ln -s "$PROJECT" "$ROOT/project-link"
symlink_root_payload=$(printf '{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"docs/override-symlink.md"}}')
printf '%s' "$symlink_root_payload" |
	HARNESS9_HOOK_TESTING=1 \
	HARNESS9_PROJECT_ROOT="$ROOT/project-link" \
	HARNESS9_OBSIDIAN_VAULT="$VAULT" \
	bash .codex/hooks/sync-to-obsidian.sh
test ! -e "$VAULT/override-symlink.md"

ln -s "$VAULT" "$ROOT/vault-link"
printf '# Vault override symlink\n' > "$PROJECT/docs/vault-override-symlink.md"
vault_root_payload=$(printf '{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"docs/vault-override-symlink.md"}}')
printf '%s' "$vault_root_payload" |
	HARNESS9_HOOK_TESTING=1 \
	HARNESS9_PROJECT_ROOT="$PROJECT" \
	HARNESS9_OBSIDIAN_VAULT="$ROOT/vault-link" \
	bash .codex/hooks/sync-to-obsidian.sh
test ! -e "$VAULT/vault-override-symlink.md"

printf '# Outside source\n' > "$ROOT/outside-source.md"
ln -s "$ROOT/outside-source.md" "$PROJECT/docs/source-link.md"
source_link_payload=$(printf '{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"docs/source-link.md"}}')
printf '%s' "$source_link_payload" |
	HARNESS9_HOOK_TESTING=1 \
	HARNESS9_PROJECT_ROOT="$PROJECT" \
	HARNESS9_OBSIDIAN_VAULT="$VAULT" \
	bash .codex/hooks/sync-to-obsidian.sh
test ! -e "$VAULT/source-link.md"

mkdir -p "$ROOT/source-component"
printf '# Source component\n' > "$ROOT/source-component/escape.md"
ln -s "$ROOT/source-component" "$PROJECT/docs/source-component"
source_component_payload=$(printf '{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"docs/source-component/escape.md"}}')
printf '%s' "$source_component_payload" |
	HARNESS9_HOOK_TESTING=1 \
	HARNESS9_PROJECT_ROOT="$PROJECT" \
	HARNESS9_OBSIDIAN_VAULT="$VAULT" \
	bash .codex/hooks/sync-to-obsidian.sh
test ! -e "$VAULT/source-component/escape.md"

mkdir -p "$ROOT/target-component"
ln -s "$ROOT/target-component" "$VAULT/target-component"
mkdir -p "$PROJECT/docs/target-component"
printf '# Target component\n' > "$PROJECT/docs/target-component/escape.md"
target_component_payload=$(printf '{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"docs/target-component/escape.md"}}')
printf '%s' "$target_component_payload" |
	HARNESS9_HOOK_TESTING=1 \
	HARNESS9_PROJECT_ROOT="$PROJECT" \
	HARNESS9_OBSIDIAN_VAULT="$VAULT" \
	bash .codex/hooks/sync-to-obsidian.sh
test ! -e "$ROOT/target-component/escape.md"

printf '# Outside target\n' > "$ROOT/outside-target.md"
ln -s "$ROOT/outside-target.md" "$VAULT/final-link.md"
printf '# Final destination\n' > "$PROJECT/docs/final-link.md"
final_link_payload=$(printf '{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"docs/final-link.md"}}')
printf '%s' "$final_link_payload" |
	HARNESS9_HOOK_TESTING=1 \
	HARNESS9_PROJECT_ROOT="$PROJECT" \
	HARNESS9_OBSIDIAN_VAULT="$VAULT" \
	bash .codex/hooks/sync-to-obsidian.sh
test -L "$VAULT/final-link.md"
test "$(cat "$ROOT/outside-target.md")" = "# Outside target"

printf '# Escape\n' > "$ROOT/escape.md"
escape_payload=$(printf '{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"docs/../../escape.md"}}')
printf '%s' "$escape_payload" |
	HARNESS9_HOOK_TESTING=1 \
	HARNESS9_PROJECT_ROOT="$PROJECT" \
	HARNESS9_OBSIDIAN_VAULT="$VAULT" \
	bash .codex/hooks/sync-to-obsidian.sh
test ! -e "$VAULT/escape.md"
