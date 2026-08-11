---
description: Push a committed feature branch and open a draft pull request with GitHub CLI.
---

# Open a Draft Pull Request

## Overview

Push the current feature branch and open a draft PR. Keep it simple: verify safety invariants, push, create PR.

## Safety Contract

- Never push from `main`, `master`, or detached HEAD
- Never use `--force` or `--force-with-lease`
- Push the frozen commit OID, not symbolic `HEAD`
- Always create a **draft** PR (`--draft`)
- Never add AI attribution or `Co-Authored-By` lines
- Never commit inside this workflow; require a prior `/commit` or explicit user authorization of existing HEAD

## Workflow

### 1. Pre-flight checks

```bash
git rev-parse --show-toplevel
git status --porcelain=v1 -z --untracked-files=all
git symbolic-ref --quiet --short HEAD
git rev-parse HEAD
```

Validate:
- **Branch**: must not be `main`, `master`, or detached HEAD. Must match `^[A-Za-z0-9][A-Za-z0-9_/-]*$`.
- **Worktree**: must be clean (empty status). If not clean, stop and ask user to commit first.
- **HEAD OID**: must match `^[0-9a-f]{40}$`. Record as `RECORDED_HEAD`.

If there is no `/commit` result in the current conversation, ask the user to explicitly authorize publishing the existing HEAD before proceeding.

### 2. Validate remote and topology

```bash
gh auth status --hostname github.com
gh repo view "$ORIGIN_REPO" --json nameWithOwner,isFork,defaultBranchRef
```

- Get the sole `origin` push URL via `git remote get-url --all --push origin`. Validate it matches `git@github.com:<owner>/<repo>.git` or `https://github.com/<owner>/<repo>.git`. Reject credentials, ports, or non-GitHub hosts.
- Set `ORIGIN_REPO` = `<owner>/<repo>`, `BASE_REPO` = `ORIGIN_REPO` (if not a fork), `BASE_BRANCH` = default branch from `gh repo view`.
- If the repo is a fork, ask the user for the explicit base repository.
- Fetch `BASE_OID` via GraphQL to pin the base commit.

### 3. Check for existing PR

```bash
gh api --method GET "repos/$BASE_REPO/pulls" \
  -f state=open \
  -f "head=$HEAD_OWNER:$BRANCH"
```

If an open PR already exists for this branch, return its URL without pushing.

### 4. Push the branch

```bash
git push --porcelain "$PINNED_PUSH_URL" "$RECORDED_HEAD:refs/heads/$BRANCH"
```

- Push the frozen OID, never symbolic `HEAD`.
- Parse the porcelain output: expect `[new branch]` or `[up to date]`.
- On rejection, stop and report. Never retry with force.
- After push, verify the remote OID equals `RECORDED_HEAD` via `git ls-remote`.
- Set up upstream tracking: `git branch --set-upstream-to="origin/$BRANCH" "$BRANCH"`.

### 5. Create draft PR

Capture the title from the commit subject:

```bash
git show -s --format=%s "$RECORDED_HEAD"
```

Validate: non-empty, at most 70 Unicode code points, no AI-attribution phrases.

Create the PR via `gh pr create`:

```bash
gh pr create \
  --repo "$BASE_REPO" \
  --draft \
  --base "$BASE_BRANCH" \
  --head "$HEAD_OWNER:$BRANCH" \
  --title "$TITLE" \
  --body "$BODY"
```

The body should include a concise summary of changes and a test plan. No AI attribution.

### 6. Verify

```bash
gh api --method GET "repos/$BASE_REPO/pulls" \
  -f state=open \
  -f "head=$HEAD_OWNER:$BRANCH"
```

Confirm exactly one PR exists with matching title, base, and head. Return the `html_url`.

## Failure Responses

| Condition | Response |
|---|---|
| Worktree not clean | Stop; ask user to commit first |
| On main/master | Stop; create a feature branch first |
| Push rejected | Stop; report error, never force |
| Existing PR found | Return its URL without pushing |
| Fork repo | Ask for explicit base repository |
| No commit evidence | Ask user to authorize existing HEAD or run `/commit` first |
