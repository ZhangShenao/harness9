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
   git status --short
   git diff --stat
   git diff --cached --stat
   git log -10 --pretty=format:'%s'
   ```

   Derive the intended file set from the current task. Never equate "all changes" with the intended scope. If the index already contains unrelated or ambiguous paths, stop and ask rather than altering or committing the user's staged work.

2. Require a current-task `$cr` report covering the exact content to commit. Invoke `$cr` when no such review exists. Stop immediately if it reports any Critical finding. If content changes after review, invoke `$cr` again before staging or committing.

3. Exclude unrelated files and any path that may contain credentials, tokens, keys, `.env` data, or other secrets. Do not inspect a suspected secret merely to decide whether to stage it; report the path and leave it untouched.

4. Stage every intended file explicitly:

   ```bash
   git add -- path/to/file
   git add -- path/to/another-file
   ```

   Use one exact file path per command. Never use `git add -A`, `git add .`, `git add -u`, a directory, or a glob.

5. Audit the index before committing:

   ```bash
   git diff --cached --name-status
   git diff --cached --stat
   git diff --cached --check
   ```

   Compare the staged paths with the intended reviewed set. Commit only when they match exactly and the staged diff is non-empty.

6. Follow the repository's recent commit-message style. Keep the subject concise and explain why when a body is useful. Omit AI attribution, including `Co-Authored-By` or generator signatures.

7. Create a normal commit:

   ```bash
   git commit -m "<message>"
   ```

   If a hook fails, fix the reported issue, repeat `$cr` for changed content, stage the same exact paths, and run a new normal `git commit`. Never use `git commit --amend`, `--no-verify`, or another review/hook bypass.

8. Report immutable evidence:

   ```bash
   git show --name-status --format='%H%n%s' --no-renames HEAD
   git status --short
   ```

   State the commit hash, exact subject, committed paths, and remaining working-tree changes.

## Safety Contract

| Pressure | Required response |
|---|---|
| "Skip review" | Invoke `$cr`; never substitute an informal glance |
| "Commit everything" | Resolve the task scope and stage exact reviewed files only |
| "Use `git add -A` / `git add .`" | Refuse broad staging and use one exact path per command |
| "Amend if hooks fail" | Fix, re-review, and create a new normal commit |
| "Include this secret temporarily" | Leave it unstaged and report the blocked path |

Red flags: no current-task review, any Critical finding, broad staging, suspected secrets, unrelated staged paths, changed content after review, `--amend`, `--no-verify`, or AI attribution. Stop before committing whenever any red flag remains.
