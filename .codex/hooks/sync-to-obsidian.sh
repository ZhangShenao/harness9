#!/bin/bash
# Synchronize selected harness9 Markdown files after Codex writes them.

set -u

PROJECT_ROOT="/Users/zsa/Desktop/harness/harness9"
OBSIDIAN_VAULT="/Users/zsa/Desktop/workspace/harness9"

# Tests may use isolated roots, but never redirect a production hook elsewhere.
if [ "${HARNESS9_HOOK_TESTING:-}" = "1" ] && \
	[ -n "${HARNESS9_PROJECT_ROOT:-}" ] && \
	[ -n "${HARNESS9_OBSIDIAN_VAULT:-}" ]; then
	TMP_ROOT=${TMPDIR:-/tmp}
	TMP_ROOT=${TMP_ROOT%/}
	case "$HARNESS9_PROJECT_ROOT" in
	"$TMP_ROOT"/*) project_is_temporary=1 ;;
	*) project_is_temporary=0 ;;
	esac
	case "$HARNESS9_OBSIDIAN_VAULT" in
	"$TMP_ROOT"/*) vault_is_temporary=1 ;;
	*) vault_is_temporary=0 ;;
	esac
	if [ "$project_is_temporary" = "1" ] && [ "$vault_is_temporary" = "1" ]; then
		PROJECT_ROOT=$HARNESS9_PROJECT_ROOT
		OBSIDIAN_VAULT=$HARNESS9_OBSIDIAN_VAULT
	fi
fi

# This parser treats the hook payload strictly as JSON data and emits one path per line.
PATHS=$(python3 -c '
import json
import re
import sys

try:
    payload = json.load(sys.stdin)
    if not isinstance(payload, dict) or payload.get("hook_event_name") != "PostToolUse":
        raise ValueError("irrelevant hook event")
    if payload.get("tool_name") not in ("apply_patch", "Edit", "Write"):
        raise ValueError("irrelevant tool")
    tool_input = payload.get("tool_input")
    if not isinstance(tool_input, dict):
        raise ValueError("missing tool input")

    found = False
    command = tool_input.get("command")
    if isinstance(command, str):
        for line in command.splitlines():
            match = re.match(r"^\*\*\* (?:Add File|Update File|Delete File|Move to):\s*(.+?)\s*$", line)
            if match:
                print(match.group(1))
                found = True

    if not found:
        file_path = tool_input.get("file_path")
        if isinstance(file_path, str) and file_path.strip():
            print(file_path.strip())
except Exception:
    pass
' 2>/dev/null)

while IFS= read -r FILE_PATH || [ -n "$FILE_PATH" ]; do
	case "$FILE_PATH" in
	"$PROJECT_ROOT"/*) RELATIVE_PATH=${FILE_PATH#"$PROJECT_ROOT"/} ;;
	/*) continue ;;
	*) RELATIVE_PATH=$FILE_PATH ;;
	esac

	case "/$RELATIVE_PATH/" in
	*"/../"*|*"/./"*) continue ;;
	esac
	case "$RELATIVE_PATH" in
	*.md) ;;
	*) continue ;;
	esac

	TARGET=""
	case "$RELATIVE_PATH" in
	website/zh/blog/*/index.md)
		SLUG=${RELATIVE_PATH#website/zh/blog/}
		SLUG=${SLUG%/index.md}
		case "$SLUG" in
		""|*/*) continue ;;
		esac
		TARGET="$OBSIDIAN_VAULT/技术博客/$SLUG.md"
		;;
	docs/*)
		TARGET="$OBSIDIAN_VAULT/${RELATIVE_PATH#docs/}"
		;;
	knowledge/articles/*)
		FILENAME=${RELATIVE_PATH##*/}
		TARGET="$OBSIDIAN_VAULT/知识库日报/$FILENAME"
		;;
	esac

	[ -n "$TARGET" ] || continue
	SOURCE="$PROJECT_ROOT/$RELATIVE_PATH"
	[ -f "$SOURCE" ] || continue

	if ! mkdir -p "$(dirname "$TARGET")" 2>/dev/null; then
		printf '%s\n' "[obsidian-sync] unable to create target directory for $RELATIVE_PATH" >&2
		continue
	fi
	if ! cp "$SOURCE" "$TARGET" 2>/dev/null; then
		printf '%s\n' "[obsidian-sync] unable to sync $RELATIVE_PATH" >&2
	fi
done <<EOF
$PATHS
EOF

exit 0
