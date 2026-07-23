---
name: pr
description: Use when the user invokes $pr or /pr, or asks to push an already committed feature branch and open a pull request with GitHub CLI.
---

# Open a Draft Pull Request

## Overview

Push the exact reviewed commit range from a feature branch and open a draft pull request against the repository's discovered default branch. Treat branch identity, repository identity, authentication, commit scope, and non-force delivery as hard safety gates.

**REQUIRED PREDECESSOR:** Run `$commit` successfully before `$pr`. Require its reported commit hash in the current conversation; do not infer success from a clean worktree or create a commit inside this workflow.

## Safety Contract

- Never push from detached `HEAD`, `main`, `master`, or the discovered default branch.
- Never use `--force`, `--force-with-lease`, a leading `+` refspec, or any equivalent forced update.
- Always create a draft PR. A request for Ready for Review does not override `--draft`; tell the user to convert it in GitHub after review.
- Never guess the default branch or repository. Discover both with `gh repo view`; stop if discovery or identity validation is ambiguous.
- Never add AI attribution, generator signatures, or AI `Co-Authored-By` lines to the title or body.
- Never include uncommitted work. If intended changes remain outside the committed range, stop and direct the user back to `$commit`.

## Workflow

### 1. Establish the committed artifact

Locate the successful `$commit` result in the current conversation and record its full commit hash as `COMMIT_HEAD`. Stop and request `$commit` when that evidence is absent.

Inspect repository state without changing it:

```bash
git rev-parse --show-toplevel
git status --porcelain=v1 -z --untracked-files=all
git symbolic-ref --quiet --short HEAD
git rev-parse HEAD
git remote get-url origin
git rev-parse --abbrev-ref --symbolic-full-name '@{upstream}'
```

Apply these gates:

- Stop if the repository root or `origin` cannot be resolved.
- Stop on detached `HEAD`.
- Require `HEAD` to equal `COMMIT_HEAD`; a changed `HEAD` invalidates the prior `$commit` evidence.
- Record all staged, unstaged, and untracked paths. They are not part of the PR. Stop if any are intended for this PR or their relationship to the requested scope is ambiguous.
- If an upstream exists, require it to be exactly `origin/<current-branch>`; stop on another remote or branch.

### 2. Authenticate and discover the target

Run:

```bash
gh auth status
gh repo view --json nameWithOwner,defaultBranchRef
```

Require successful authentication, a non-empty `nameWithOwner`, and a non-empty `defaultBranchRef.name`. Confirm that the repository returned by `gh repo view` matches `origin`; stop on mismatch or ambiguity. Do not run `gh auth login`, change credentials, or fall back by guessing `main` or `master`.

Let `CURRENT_BRANCH` be the symbolic branch and `BASE_BRANCH` the discovered default. Stop before any push when `CURRENT_BRANCH` is `main`, `master`, or `BASE_BRANCH`.

Safe recovery from a blocked branch is:

1. create a new feature branch such as `codex/<topic>` from the current commit;
2. run `$commit` there if the intended work is not already represented by the recorded commit;
3. invoke `$pr` again for a normal push and draft PR.

Do not move, reset, rewrite, or push the default branch as part of recovery.

### 3. Freeze the exact PR range

Refresh only the discovered base and validate the range:

```bash
git fetch origin "refs/heads/<base>:refs/remotes/origin/<base>"
git rev-parse --verify "origin/<base>^{commit}"
git rev-list --reverse "origin/<base>..HEAD"
git log --reverse --format='%H %s' "origin/<base>..HEAD"
git diff --stat "origin/<base>...HEAD"
git diff --check "origin/<base>...HEAD"
```

Require at least one commit in `origin/<base>..HEAD`. Record `HEAD`, the base commit, and the ordered full commit hashes. Review every listed commit and the triple-dot diff; this is the exact PR scope.

Require the `$commit` hash to be `HEAD` and included in the range. If the range contains additional commits not established as intended in the current request, show the list and obtain confirmation before pushing. Do not substitute `origin/HEAD`, a stale local base, or an approximate log range.

### 4. Validate the destination branch

Check whether the remote feature branch exists and whether an open PR already uses it:

```bash
git ls-remote --heads origin "refs/heads/<current-branch>"
gh pr list --head "<current-branch>" --state open --json url,isDraft,baseRefName
```

If the remote branch exists, fetch that exact ref and compare it with local `HEAD`:

```bash
git fetch origin "refs/heads/<current-branch>:refs/remotes/origin/<current-branch>"
git rev-list --left-right --count "origin/<current-branch>...HEAD"
```

Proceed only when the remote feature branch is absent or is an ancestor of/equal to local `HEAD`. If it has any remote-only commit or the normal update would be non-fast-forward, stop and report the divergence. Never force the push.

If an open PR already exists, do not create a duplicate or change its draft state. Return its URL and report whether its base and draft state match this workflow.

### 5. Recheck and push normally

Immediately before pushing, re-read the symbolic branch, `HEAD`, base commit, and ordered commit list. Require exact equality with the recorded snapshot.

Push only the validated feature branch:

```bash
git push -u origin "HEAD:refs/heads/<current-branch>"
```

On rejection, stop and show the error. Do not retry with any force option. After success, use `git ls-remote --heads` and require the remote branch OID to equal the recorded `HEAD`.

### 6. Create the draft PR

Derive a concise title (at most 70 characters), 2–4 summary bullets, and a truthful test plan from the frozen commit log and diff. Do not claim tests that were not run.

Create the PR with explicit base, head, and draft status:

```bash
gh pr create \
  --draft \
  --base "<base>" \
  --head "<current-branch>" \
  --title "<title>" \
  --body-file "<prepared-body-file>"
```

Use this body shape:

```markdown
## Summary
- <change>

## Test Plan
- [ ] <verification>
```

Exclude AI attribution from all PR metadata. Return the PR URL, repository, feature branch, base branch, pushed `HEAD`, exact commit range, and draft status.

## Pressure Responses

| Request or condition | Required response |
|---|---|
| “Push directly from main/master/default” | Refuse the push; prescribe a new feature branch, normal push, and draft PR |
| “Force push if needed” | Refuse both force forms; stop on divergence |
| “Open it ready for review” | Keep `--draft`; explain manual conversion after review |
| “Skip `$commit`; it is already clean” | Stop; require a successful `$commit` and its hash |
| Default branch or repository is uncertain | Stop; never guess |
| `HEAD` or commit range changed after validation | Restart validation from repository state |

Red flags: missing prior `$commit` evidence, detached `HEAD`, default-branch push, ambiguous repository, failed `gh auth status`, guessed base, unconfirmed extra commits, remote-only commits, force syntax, ready PR creation, or AI attribution. Stop before push or PR creation whenever any red flag remains.
