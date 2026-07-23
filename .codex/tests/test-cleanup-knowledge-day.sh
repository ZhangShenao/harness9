#!/bin/bash
set -euo pipefail

ROOT=$(mktemp -d "${TMPDIR:-/tmp}/harness9-cleanup-test.XXXXXX")
trap 'rm -rf "$ROOT"' EXIT
mkdir -p "$ROOT/raw/20260723" "$ROOT/analysis/20260723" "$ROOT/raw/20260724" "$ROOT/analysis/20260724" "$ROOT/articles"
touch "$ROOT/raw/20260723/a.json" "$ROOT/analysis/20260723/a.json"
touch "$ROOT/raw/20260724/keep.json" "$ROOT/analysis/20260724/keep.json"

assert_exit_status() {
	expected=$1
	shift
	set +e
	"$@" >/dev/null 2>&1
	status=$?
	set -e
	if [[ "$status" -ne "$expected" ]]; then
		echo "expected exit $expected, got $status" >&2
		return 1
	fi
}

assert_unsafe_root_rejected() {
	assert_exit_status 64 "$@"
}

assert_unsafe_root_rejected env TMPDIR="$PWD" HARNESS9_CLEANUP_TESTING=1 HARNESS9_KNOWLEDGE_ROOT="$PWD/.codex" bash .codex/scripts/cleanup-knowledge-day.sh 20260723
ln -s "$PWD/.codex" "$ROOT/non-temporary-link"
assert_unsafe_root_rejected env HARNESS9_CLEANUP_TESTING=1 HARNESS9_KNOWLEDGE_ROOT="$ROOT/non-temporary-link" bash .codex/scripts/cleanup-knowledge-day.sh 20260723

if HARNESS9_CLEANUP_TESTING=1 HARNESS9_KNOWLEDGE_ROOT="$ROOT" bash .codex/scripts/cleanup-knowledge-day.sh '../bad'; then
	exit 1
fi
assert_exit_status 65 env HARNESS9_CLEANUP_TESTING=1 HARNESS9_KNOWLEDGE_ROOT="$ROOT" bash .codex/scripts/cleanup-knowledge-day.sh 20260723
touch "$ROOT/articles/20260723-daily.md"
HARNESS9_CLEANUP_TESTING=1 HARNESS9_KNOWLEDGE_ROOT="$ROOT" bash .codex/scripts/cleanup-knowledge-day.sh 20260723

test ! -e "$ROOT/raw/20260723"
test ! -e "$ROOT/analysis/20260723"
test -e "$ROOT/raw/20260724/keep.json"
test -e "$ROOT/analysis/20260724/keep.json"

mkdir -p "$ROOT/raw/20260725" "$ROOT/analysis/20260725"
touch "$ROOT/raw/20260725/a.json" "$ROOT/analysis/20260725/a.json"
touch "$ROOT/articles/20260725-daily with spaces.md"
HARNESS9_CLEANUP_TESTING=1 HARNESS9_KNOWLEDGE_ROOT="$ROOT" bash .codex/scripts/cleanup-knowledge-day.sh 20260725
test ! -e "$ROOT/raw/20260725"
test ! -e "$ROOT/analysis/20260725"
