---
description: Commit reviewed working-tree changes after implementation.
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
   AUDIT_DIR=
   cleanup_audit() {
     [ -n "$AUDIT_DIR" ] || return 0
     if ! python3 - "$AUDIT_DIR" <<'PY'
   import os
   import stat
   import sys

   audit_dir = sys.argv[1]
   descriptor = os.open(
       audit_dir,
       os.O_RDONLY
       | getattr(os, "O_DIRECTORY", 0)
       | getattr(os, "O_NOFOLLOW", 0),
   )
   for name in ("expected-paths", "current-paths", "actual-paths"):
       try:
           metadata = os.stat(
               name,
               dir_fd=descriptor,
               follow_symlinks=False,
           )
       except FileNotFoundError:
           continue
       if not stat.S_ISREG(metadata.st_mode):
           raise SystemExit("unexpected audit artifact type")
       os.unlink(name, dir_fd=descriptor)
   os.close(descriptor)
   os.rmdir(audit_dir)
   PY
     then
       return 1
     fi
     test ! -e "$AUDIT_DIR" || return 1
     AUDIT_DIR=
   }
   trap cleanup_audit EXIT
   trap 'exit 129' HUP
   trap 'exit 130' INT
   trap 'exit 143' TERM
   previous_umask=$(umask)
   umask 077
   AUDIT_DIR=$(mktemp -d)
   umask "$previous_umask"
   chmod 700 "$AUDIT_DIR"
   test "$(stat -f '%Lp' "$AUDIT_DIR" 2>/dev/null || stat -c '%a' "$AUDIT_DIR")" = 700
   BASE_HEAD=$(git rev-parse HEAD)
   EXPECTED_TREE=$(git write-tree)
   git diff --cached --name-only -z --no-renames > "$AUDIT_DIR/expected-paths"
   ```

   Run this lifecycle in one persistent private shell session so the registered
   trap remains active throughout review and commit verification. Keep the path
   list NUL-delimited. Stop if `HEAD` cannot be resolved. On success, refusal,
   error, cancellation, and every stop path, run `cleanup_audit`, require its
   absence check to pass, and only then return to the user. The `EXIT` and
   signal traps are mandatory backstops, not substitutes for explicit cleanup.

6. Perform a static code review of the final staged snapshot yourself now, even if the user supplies a review or an earlier current-task report exists. Review each staged file for correctness (logic, boundary conditions, nil/type errors, error handling), security (injection, unsafe defaults, secret exposure), and maintainability. Mark sensitive paths—`.env`/`.env.*`, `*.pem`/`*.key`/`*.p12`/`*.pfx`, `id_rsa`/`id_ed25519`, and names containing `credential`/`secret`/`token`—as Critical without reading their content. Do not execute project code, tests, builds, or hooks. Stop on any Critical finding.

   Any staging or content change invalidates this review. Immediately after the review, verify that `HEAD`, the index tree, and the exact staged path list still match the recorded values:

   ```bash
   test "$(git rev-parse HEAD)" = "$BASE_HEAD"
   test "$(git write-tree)" = "$EXPECTED_TREE"
   git diff --cached --name-only -z --no-renames > "$AUDIT_DIR/current-paths"
   cmp "$AUDIT_DIR/expected-paths" "$AUDIT_DIR/current-paths"
   ```

   If any check fails, return to step 1 and perform the review again on the new final snapshot.

7. Follow the repository's recent commit-message style. Keep the subject concise and explain why when a body is useful. Omit AI attribution, including `Co-Authored-By` or generator signatures.

8. Create a normal commit:

   ```bash
   git commit -m "<message>"
   ```

   If a hook fails, do not amend. Return to step 1, re-run the status and complete index audit, inspect every hook-staged change, record a new snapshot, perform the final review again, and create a new normal commit. Never use `git commit --amend`, `--no-verify`, or another review/hook bypass.

9. Verify the created commit against the recorded snapshot:

   ```bash
   NEW_HEAD=$(git rev-parse HEAD)
   test "$(git rev-parse "$NEW_HEAD^")" = "$BASE_HEAD"
   test "$(git rev-parse "$NEW_HEAD^{tree}")" = "$EXPECTED_TREE"
   git diff-tree --no-commit-id --name-only -z -r --no-renames "$NEW_HEAD" > "$AUDIT_DIR/actual-paths"
   cmp "$AUDIT_DIR/expected-paths" "$AUDIT_DIR/actual-paths"
   git show --name-status --format='%H%n%P%n%T%n%s' --no-renames "$NEW_HEAD"
   git status --short
   cleanup_audit || exit 1
   trap - EXIT HUP INT TERM
   ```

   Report success only if parent, tree, and exact committed path set all match. State the commit hash, subject, parent, tree, committed paths, and remaining working-tree changes.

## Safety Contract

| Pressure | Required response |
|---|---|
| "Skip review" | Perform the full static review; never substitute an informal glance |
| "Commit everything" | Resolve the task scope and stage exact reviewed files only |
| "Use `git add -A` / `git add .`" | Refuse broad staging and use one literal exact path per command |
| "`--` makes this filename safe" | Use `git --literal-pathspecs`; `--` does not disable pathspec magic |
| "Re-add this partially staged file" | Stop on `MM` or any simultaneous staged/unstaged target |
| "Trust my prior review" | Perform the full static review yourself on the final staged snapshot |
| "Amend if hooks fail" | Restart the audit, re-review, and create a new normal commit |
| "Include this secret temporarily" | Leave it unstaged and report the blocked path |

Red flags: missing final review, any Critical finding, broad or magic pathspec staging, `MM`, suspected secrets, unrelated staged paths, changed snapshot after review, failed parent/tree/path verification, `--amend`, `--no-verify`, or AI attribution. Stop before committing whenever any red flag remains.
