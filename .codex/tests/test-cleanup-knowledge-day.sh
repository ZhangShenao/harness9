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
