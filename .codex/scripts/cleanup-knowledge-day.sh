#!/bin/bash
set -euo pipefail

KNOWLEDGE_ROOT="/Users/zsa/Desktop/workspace/harness9/知识库日报"
if [[ "${HARNESS9_CLEANUP_TESTING:-0}" == "1" ]]; then
	KNOWLEDGE_ROOT=$(python3 - "${HARNESS9_KNOWLEDGE_ROOT:?test root required}" <<'PY'
import os
import re
import sys

root = os.path.realpath(sys.argv[1])
is_macos_temp = re.match(r"^/private/var/folders/[^/]+/[^/]+/T/.+", root)
is_trusted_temp = root.startswith("/tmp/") or root.startswith("/private/tmp/") or is_macos_temp
if not os.path.isdir(root) or not is_trusted_temp:
    print("test root must be temporary", file=sys.stderr)
    raise SystemExit(64)
print(root)
PY
)
fi

[[ $# -eq 1 ]] || { echo "usage: $0 YYYYMMDD" >&2; exit 64; }
DAY="$1"
[[ "$DAY" =~ ^[0-9]{8}$ ]] || { echo "invalid date: $DAY" >&2; exit 64; }

found=0
shopt -s nullglob
articles=("$KNOWLEDGE_ROOT/articles/${DAY}-daily"*.md)
if [[ -n "${articles[*]-}" ]]; then
	for article in "${articles[@]}"; do
		[[ -f "$article" ]] && found=1
	done
fi
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
