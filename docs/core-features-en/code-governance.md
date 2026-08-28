# Code Governance

Code governance in harness9 rests on **three automated quality gates** (jscpd duplication, autocorrect + typos prose formatting, and check-doc-drift documentation drift detection) plus **one documentation sync pipeline** (/sync-docs). The goal is singular: keep code, comments, and docs from drifting apart. When code changes, comments follow; when comments change, docs follow — and CI enforces all of it, so it never depends on individual discipline.

```
Duplication gate    jscpd                → code duplication never grows (threshold gate)
Prose formatting    autocorrect + typos  → comments and docs are consistently formatted and typo-free
Drift detection     check-doc-drift.sh   → code changes must carry doc changes
Sync pipeline       /sync-docs           → the execution path: code → Chinese docs → English mirror
```

All three gates run in the CI `lint` job alongside the existing checks (gofmt / goimports / go vet), while jscpd gets a dedicated `duplication` job. This document covers the principles, configuration, and usage of each.

---

## Duplication Gate

### How Detection Works

The duplication gate is built on [jscpd](https://github.com/kucherenko/jscpd) (copy-paste detector) and works in three steps:

1. **Tokenization**: source code is parsed into a unified token stream according to the language grammar (Go in this project), stripping whitespace and formatting differences so only semantic units remain;
2. **Window matching**: a sliding-window hash match runs over the token stream; two identical stretches qualify as a clone when the segment is at least `minTokens` long and spans at least `minLines`;
3. **Duplicated-lines percentage**: the ratio of lines covered by clones to total lines is compared against `threshold` — exceeding it fails the run with a non-zero exit code.

### Gate Configuration

The configuration lives in `.jscpd.json` at the repository root and is shared by CI and the local `npx jscpd@5.0.16 .` invocation:

| Field | Value | Meaning |
|-------|-------|---------|
| `path` | `["internal", "cmd"]` | Only production code directories are scanned |
| `formats` | `["go"]` | Only Go source files are checked |
| `minTokens` | `50` | A segment needs at least 50 tokens to count as a clone (filters trivial short repeats) |
| `minLines` | `5` | ... and at least 5 lines |
| `threshold` | `5` | Upper bound on duplicated-lines percentage; exceeding it exits 1 (blocks CI) |
| `reporters` | `["consoleFull"]` | Full console report |
| `gitignore` | `true` | Honor `.gitignore`; generated artifacts are excluded automatically |
| `ignore` | `["**/*_test.go"]` | File-level exclusion of test code |

Excluding test files via `ignore` is a **data-driven** decision: measurements showed roughly 86% of duplicated lines come from `*_test.go` (table-driven test case tables and setup boilerplate are reasonable test scaffolding, not production code smells). Excluding them keeps the gate focused on implementation code that actually deserves attention.

### Measure First, Then Set the Threshold

The threshold was not picked arbitrarily. Before the gate landed, the existing codebase was measured to establish a baseline (jscpd 5.0.16, full `internal/` + `cmd/` scope):

| Scope | Files | Total lines | Clones | Duplicated lines |
|-------|-------|-------------|--------|------------------|
| Full (internal + cmd) | 209 | 36365 | 87 | 754 (**2.07%**) |
| Excluding `*_test.go` (the gate's actual observation scope) | 110 | 19396 | 13 | 107 (0.55%) |

The threshold follows a fixed formula:

```
threshold = ceil(baseline duplicated-lines %) + 2
          = ceil(2.07) + 2
          = 5
```

The "baseline + 2" strategy means the gate **prevents regression without punishing existing code**. Reasonable duplication already in the repository is never a false positive; the gate only lights up when someone pastes a large block of copy-pasted code into the tree. As refactoring drives the baseline down, the threshold can be lowered alongside it, tightening the gate step by step.

### Behavior in CI

The `duplication` job runs `npx jscpd@5.0.16 .` on every build (the version is pinned explicitly, no `@latest`):

```yaml
- name: Run jscpd
  shell: bash
  run: set -o pipefail; npx jscpd@5.0.16 . 2>&1 | tee jscpd-output.txt

- name: Report summary
  if: always()
  run: |
    echo "## Duplication Report" >> $GITHUB_STEP_SUMMARY
    grep -E "Clone|duplicated" jscpd-output.txt | head -50 >> $GITHUB_STEP_SUMMARY || true
```

Two details matter: `set -o pipefail` ensures jscpd's over-threshold non-zero exit code propagates through `tee` and fails the job, and `if: always()` guarantees the duplication report is still written to the PR's Step Summary even when the gate fails, so the clone list is right there for triage.

---

## Documentation Drift Detection

### The Mapping Table

`docs/doc-map.json` maps code modules to technical documentation. It currently holds **23** entries, each with this shape:

```json
{ "paths": ["cmd/harness9/tui.go", "cmd/harness9/tui_*.go"], "docs": ["docs/核心功能/tui.md"] }
```

- `paths`: an array of glob patterns matching the module's source paths (e.g. `cmd/harness9/tui_*.go`, or a plain directory like `internal/engine`);
- `docs`: the list of Chinese docs that must stay in sync;
- An empty `docs` array (`[]`) means the module has no doc yet — it is **registered but not checked**. This is the incremental path for onboarding modules before their docs exist, so a missing doc never blocks registering a module.

### Detection Algorithm

`scripts/check-doc-drift.sh` works as follows:

1. **Collect the changeset**: `git diff --name-only <base>...HEAD` (defaults to `origin/master...HEAD`, falling back to `master...HEAD`, then the `HEAD` working tree; an explicit base-ref can be passed). `git -c core.quotepath=off` keeps non-ASCII paths such as `docs/核心功能/` unescaped, so path comparison never breaks on quote escaping;
2. **Test-file exemption**: changes to `*_test.go` never trigger doc checks — internal test adjustments (case additions, assertion tweaks) carry no documentation obligation;
3. **Path matching**: each changed file is matched against the entry's `paths` using glob semantics; a directory pattern matches every file underneath it via prefix matching;
4. **Doc-sync verification**: once a module is hit, **all** docs in its `docs` list must appear in the changeset, otherwise the script reports `DRIFT: 代码已变更但文档未同步` (code changed but docs not updated).

### Exit Codes and the warn/strict Split

| Exit code | Meaning |
|-----------|---------|
| `0` | Pass; or drift detected in warn mode (warning only, non-blocking) |
| `1` | Drift detected in strict mode (blocks CI) |
| `2` | Environment error (missing `jq`, missing `doc-map.json`) |

Detection runs at two levels: with `DOC_DRIFT_STRICT=1`, drift exits 1 and blocks merging; in the default warn mode the script only emits `::warning::` and exits 0.

The split is a deliberate evolution strategy: **run in warn as an observation period, then flip the default to strict**. A freshly written mapping table is bound to have gaps (some code changes genuinely need no doc updates), and going strict immediately would flood developers with false positives and teach them to bypass the gate. Start in warn mode, observe the mapping table's precision, and only then flip CI's default. CI currently sets `DOC_DRIFT_STRICT: "0"` explicitly — the observation period is in effect.

### CI Integration Point

Drift detection runs inside the `lint` job: on pull requests it uses `origin/${{ github.base_ref }}` as the base (i.e. "what did this PR change relative to the target branch"); on pushes to master it compares against `origin/master` explicitly — at that point HEAD is already the latest commit, so the diff is empty and the check passes explicitly (the real gate runs during the PR phase).

---

## Comments and Prose Formatting

### autocorrect: Mixed-Script Spacing and Punctuation

[autocorrect](https://github.com/huacnlee/autocorrect) parses source files by type and **identifies comment and prose regions** (`//` comments in Go, Markdown body text, and so on). It only touches comments, string literals, and prose — never code logic — so formatting can never change program behavior.

Two rule families do the work:

- **CJK spacing**: insert spaces between Chinese text and English words, numbers, and inline code (`配置位于仓库根目录.jscpd.json` → `配置位于仓库根目录 .jscpd.json`);
- **Fullwidth/halfwidth punctuation**: convert punctuation to fullwidth in Chinese context and halfwidth in English context, and normalize fullwidth alphanumerics to halfwidth.

Configuration lives in `.autocorrectrc`; the key settings:

| Rule | Level | Meaning |
|------|-------|---------|
| `space-word` / `space-punctuation` / `space-bracket` / `space-backticks` | 1 | Insert spaces between CJK and words/punctuation/brackets/backticks |
| `fullwidth` / `no-space-fullwidth` | 1 | Fullwidth punctuation in CJK context; no space next to fullwidth punctuation |
| `halfwidth-word` / `halfwidth-punctuation` | 1 | Halfwidth alphanumerics and punctuation in English context |
| `spellcheck` | 0 | **Spell check disabled** — typos owns spelling, avoiding two dictionaries fighting each other |
| `context.codeblock` | 1 | Markdown code block contents are never corrected (code is code) |

`.autocorrectignore` excludes non-prose assets: `.worktrees/` (full nested worktree copies), `benchmarks/`, `swebench-baseline-v1/`, `test_fixtures/`, `*.svg`, `*.png`.

### typos: Spell Detection and the Allowlist

[typos](https://github.com/crate-ci/typos) uses a built-in dictionary to detect English spelling errors across source and docs, covering comments, strings, and identifiers. False positives are exempted via the `_typos.toml` allowlist, currently:

```toml
[default]
extend-ignore-identifiers-re = [
  "UDDG",       # DuckDuckGo encrypted-URL parameter name (decodeUDDG)
  "harness9",
  "Ratatui",    # Rust TUI framework name, not a Ratatouille misspelling
  "HASS",       # Home Assistant token prefix (HASS_TOKEN)
  "provid",     # intentional truncation of provider in AGENTS.md's ASCII diagram
]

[default.extend-words]
harness9 = "harness9"
```

The allowlist strategy is to **grow it as real false positives appear**: no speculative entries up front; every time CI hits a false positive, the proper noun joins the list, so the allowlist always mirrors the project's actual glossary.

### Line-Wrapping Decision: No Prose Wrap

Unlike many English lint tools, autocorrect runs with **no automatic hard wrapping** (prose wrap) here. The reason: hard-wrapping Chinese text at a fixed column width means a localized edit to one sentence re-wraps the entire paragraph, producing wall-of-diff noise that cripples review and blame. Paragraphs flow as natural single lines; line length is left to editor soft wrap.

### Known Limitations

- typos' dictionary mechanism only detects **English** misspellings; for Chinese typos (misused particles, visually similar characters) there is no mature open-source detector today — human review remains the backstop;
- autocorrect fixes "format", not "content": grammar and phrasing are out of scope.

---

## The /sync-docs Three-Phase Pipeline

Drift detection can only tell you that docs did not move; it cannot move them for you. The execution path is the `/sync-docs` command (`.opencode/commands/sync-docs.md`), which covers three phases in a single run:

### Phase 1: Code Changes → Chinese Docs

1. Determine the changeset (user-supplied range / uncommitted working-tree changes / `git diff --name-only origin/master...HEAD`, falling back to `master...HEAD`, then the working tree), filtering out `*_test.go` and generated artifacts;
2. Consult `docs/doc-map.json`: entries whose `paths` hit the changeset and whose `docs` is non-empty form the candidate list;
3. For each candidate doc: read it in full alongside the relevant code diff, then update stale descriptions, add sections for new features, and fix outdated code snippets; skipping a change with no real doc impact is allowed but must be justified.

### Phase 2: Chinese → English Mirror

1. **Scan**: compare every `docs/核心功能/*.md` against its same-named file in `docs/core-features-en/` for existence and modification time, classifying each as MISSING (English doc absent — must be created) / STALE (English doc outdated — review and update) / OK;
2. **Create MISSING docs**: English versions follow the **independently written** principle — not sentence-by-sentence machine translation, but re-authored with English technical writing conventions; structure stays aligned (same sections, tables, and code blocks), and code blocks and CLI examples pass through unchanged;
3. **Update STALE docs**: compare section by section — Chinese sections added are mirrored, modified ones aligned, removed ones dropped;
4. **Verify**: re-run the scan and require zero MISSING and zero STALE.

### Phase 3: Wrap-Up Report

The final report lists, for both phases, every updated doc with its change highlights, the reasons for any skipped items, items needing human confirmation, and the doc counts of both directories (which must match).

### Command vs. CI

The two mechanisms **complement** rather than duplicate each other: `check-doc-drift.sh` is the mechanical gate that makes "did the docs move" objectively verifiable in CI, while `/sync-docs` is the LLM-assisted execution path that answers "did they move correctly". The workflow convention: run `/sync-docs` after code changes, and the CI drift check passes naturally when the PR opens.

---

## Local Usage

### Installing the Tools

```bash
# autocorrect (Homebrew on macOS, or the Rust toolchain)
brew install autocorrect
cargo install autocorrect-cli

# typos
brew install typos-cli
cargo install typos-cli

# jscpd needs no installation — run it via npx
```

### Command Cheat Sheet

| Command | Purpose |
|---------|---------|
| `autocorrect --fix .` | Auto-fix prose formatting across the repo (run before committing) |
| `autocorrect --lint` | CI-equivalent format check, report-only |
| `typos` | Repo-wide spell check |
| `typos -w` | Auto-fix spelling errors (inspect with `git diff` first) |
| `npx jscpd@5.0.16 .` | Local duplication report (reads `.jscpd.json`) |
| `scripts/check-doc-drift.sh` | Doc drift detection, warn mode by default |
| `DOC_DRIFT_STRICT=1 scripts/check-doc-drift.sh` | Strict mode: drift exits 1 |
| `/sync-docs` | opencode command: the three-phase doc sync pipeline |

CI pins jscpd to the same version (`npx jscpd@5.0.16`); when upgrading jscpd, update the version in both CI and this cheat sheet, and re-measure the duplication baseline.

Recommended personal workflow: run `autocorrect --fix .` and `typos` over your changes before committing, and run `/sync-docs` whenever you touched anything under `internal/`, `cmd/`, or `skills/`.

---

## CI Integration Overview

All governance-related checks in `.github/workflows/ci.yml`:

| Job | Check | Implementation | On failure |
|-----|-------|----------------|------------|
| lint | gofmt | `gofmt -l .` — any output fails the job | Blocking |
| lint | goimports | `goimports -l .` (v0.33.0) | Blocking |
| lint | Static analysis | `go vet ./...` | Blocking |
| lint | Prose format | `huacnlee/autocorrect-action@main` | Blocking |
| lint | Spelling | `crate-ci/typos@v1.49.1` | Blocking |
| lint | Doc drift | `scripts/check-doc-drift.sh` (`DOC_DRIFT_STRICT: "0"`) | Warning only, non-blocking |
| duplication | Code duplication | `npx jscpd@5.0.16 .` (threshold 5) | Blocking + Step Summary report |
| build | Compilation | `go build ./...` | Blocking |
| test | Tests | `go test -race ./...` | Blocking |

Planned evolution: flip `DOC_DRIFT_STRICT` to `1` once the warn observation period ends, and tighten the jscpd threshold as the baseline drops. Both are one-line config changes — no structural work required.
