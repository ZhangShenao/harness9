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
