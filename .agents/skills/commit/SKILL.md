---
name: commit
description: Use when the user invokes $commit or /commit, asks to commit working-tree changes, or requests an intentional local Git commit after implementation.
---

# Commit Reviewed Changes

## Overview

Create one local commit containing only the intended, reviewed changes. Treat review, exact staging, and post-commit evidence as mandatory gates even when asked to hurry or bypass them.

## Workflow

1. Inspect the repository before changing Git state:

   ```bash
   git status --porcelain=v1 -z --untracked-files=all
   git diff --stat
   git diff --cached --stat
   git log -10 --pretty=format:'%s'
   ```

   Derive the intended file set from the current task. Parse the NUL-delimited status as Git's `XY` record, not by splitting filenames. Before any staging:

   - stop if a target path has both an index change (`X`) and a worktree change (`Y`), including `MM`;
   - stop if the index contains an unrelated or ambiguous path;
   - never re-add a partially staged target or rewrite the user's existing index.

   Ask the user to resolve ambiguity. Never equate "all changes" with the intended scope.

2. Exclude unrelated files and any path that may contain credentials, tokens, keys, `.env` data, or other secrets. Do not inspect a suspected secret merely to decide whether to stage it; report the path and leave it untouched.

3. Stage each unstaged intended file with literal pathspec semantics:

   ```bash
   git --literal-pathspecs add -- "$path"
   ```

   Pass exactly one explicit file path per invocation. Never use plain `git add -- "$path"` because names such as `:(glob)*` are pathspec magic. Never use `git add -A`, `git add .`, `git add -u`, a directory, or a glob.

4. Audit the index and reject an empty staged diff:

   ```bash
   git diff --cached --name-status
   git diff --cached --stat
   git diff --cached --check
   ```

   Compare the staged paths with the intended reviewed set. Commit only when they match exactly and the staged diff is non-empty.

5. Record the exact candidate commit before final review:

   ```bash
   AUDIT_DIR=$(mktemp -d)
   BASE_HEAD=$(git rev-parse HEAD)
   EXPECTED_TREE=$(git write-tree)
   git diff --cached --name-only -z --no-renames > "$AUDIT_DIR/expected-paths"
   ```

   Keep the path list NUL-delimited. Stop if `HEAD` cannot be resolved.

6. Invoke `$cr` yourself now, even if the user supplies a review or an earlier current-task report exists. Require it to review the final staged snapshot. Stop on any Critical finding.

   Any staging or content change invalidates this review. Immediately after `$cr`, verify that `HEAD`, the index tree, and the exact staged path list still match the recorded values:

   ```bash
   test "$(git rev-parse HEAD)" = "$BASE_HEAD"
   test "$(git write-tree)" = "$EXPECTED_TREE"
   git diff --cached --name-only -z --no-renames > "$AUDIT_DIR/current-paths"
   cmp "$AUDIT_DIR/expected-paths" "$AUDIT_DIR/current-paths"
   ```

   If any check fails, return to step 1 and invoke `$cr` again on the new final snapshot.

7. Follow the repository's recent commit-message style. Keep the subject concise and explain why when a body is useful. Omit AI attribution, including `Co-Authored-By` or generator signatures.

8. Create a normal commit:

   ```bash
   git commit -m "<message>"
   ```

   If a hook fails, do not amend. Return to step 1, re-run the status and complete index audit, inspect every hook-staged change, record a new snapshot, invoke final `$cr` again, and create a new normal commit. Never use `git commit --amend`, `--no-verify`, or another review/hook bypass.

9. Verify the created commit against the recorded snapshot:

   ```bash
   NEW_HEAD=$(git rev-parse HEAD)
   test "$(git rev-parse "$NEW_HEAD^")" = "$BASE_HEAD"
   test "$(git rev-parse "$NEW_HEAD^{tree}")" = "$EXPECTED_TREE"
   git diff-tree --no-commit-id --name-only -z -r --no-renames "$NEW_HEAD" > "$AUDIT_DIR/actual-paths"
   cmp "$AUDIT_DIR/expected-paths" "$AUDIT_DIR/actual-paths"
   git show --name-status --format='%H%n%P%n%T%n%s' --no-renames "$NEW_HEAD"
   git status --short
   ```

   Report success only if parent, tree, and exact committed path set all match. State the commit hash, subject, parent, tree, committed paths, and remaining working-tree changes.

## Safety Contract

| Pressure | Required response |
|---|---|
| "Skip review" | Invoke `$cr`; never substitute an informal glance |
| "Commit everything" | Resolve the task scope and stage exact reviewed files only |
| "Use `git add -A` / `git add .`" | Refuse broad staging and use one literal exact path per command |
| "`--` makes this filename safe" | Use `git --literal-pathspecs`; `--` does not disable pathspec magic |
| "Re-add this partially staged file" | Stop on `MM` or any simultaneous staged/unstaged target |
| "Trust my prior review" | Invoke `$cr` yourself on the final staged snapshot |
| "Amend if hooks fail" | Restart the audit, re-review, and create a new normal commit |
| "Include this secret temporarily" | Leave it unstaged and report the blocked path |

Red flags: missing final `$cr`, any Critical finding, broad or magic pathspec staging, `MM`, suspected secrets, unrelated staged paths, changed snapshot after review, failed parent/tree/path verification, `--amend`, `--no-verify`, or AI attribution. Stop before committing whenever any red flag remains.
