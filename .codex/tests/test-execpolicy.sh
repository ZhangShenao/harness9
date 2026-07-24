#!/bin/bash
set -euo pipefail

RULES=.codex/rules/harness9.rules

assert_decision() {
	expected=$1
	shift
	actual=$(
		codex execpolicy check --pretty --rules "$RULES" -- "$@" |
			python3 -c 'import json, sys; print(json.load(sys.stdin).get("decision", "none"))'
	)
	if [ "$actual" != "$expected" ]; then
		printf 'expected %s, got %s: %s\n' "$expected" "$actual" "$*" >&2
		return 1
	fi
}

assert_decision allow git status --short
assert_decision prompt git push origin feature
assert_decision prompt /usr/bin/git push origin feature
assert_decision prompt /opt/homebrew/bin/gh pr create --draft --repo ZhangShenao/harness9
assert_decision prompt git -c tag.gpgSign=false tag v1.2.3 HEAD
assert_decision prompt /usr/bin/git -c credential.helper= -c credential.helper=/trusted/helper push https://github.com/ZhangShenao/harness9.git refs/tags/v1.2.3
assert_decision prompt python3 - git push origin feature
assert_decision prompt /opt/anaconda3/bin/python3 - gh pr create --draft
assert_decision prompt ./.codex/scripts/cleanup-knowledge-day.sh 20260723
assert_decision forbidden rm -rf docs
