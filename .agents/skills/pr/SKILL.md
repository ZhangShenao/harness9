---
name: pr
description: Use when the user invokes $pr or /pr, or asks to push an already committed feature branch and open a pull request with GitHub CLI.
---

# Open a Draft Pull Request

## Overview

Push one frozen commit range from a validated GitHub feature branch and open a draft pull request against a pinned repository. Treat every Git ref, remote URL, repository identity, API result, and commit-derived string as untrusted.

**REQUIRED PREDECESSOR:** Run `$commit` successfully before `$pr`. Require its reported full commit OID in the current conversation; never infer success from a clean worktree or create a commit inside this workflow.

## Non-Negotiable Safety Contract

- Never push from detached `HEAD`, `main`, `master`, or the discovered default branch.
- Never use `--force`, `--force-with-lease`, a leading `+` refspec, or any equivalent forced update.
- Freeze the full local commit OID as `RECORDED_HEAD`. Push that OID, never symbolic `HEAD`.
- Enumerate every `origin` push URL. Require exactly one canonical, credential-free GitHub SSH or HTTPS URL and push that recorded URL directly, never the mutable remote name `origin`.
- Always create a draft PR. A request for Ready for Review does not override `--draft`.
- Never guess repository topology or a fork's base repository.
- Never construct shell commands, refspecs, titles, or bodies with `eval`, `sh -c`, `bash -c`, `xargs`, generated shell text, or unquoted variables.
- Never add AI attribution, generator signatures, or AI `Co-Authored-By` lines.

## Untrusted-Value Rules

Run `git check-ref-format --branch "$BRANCH"` and then require `BRANCH` and `BASE_BRANCH` to match the stricter allowlist:

```text
^[A-Za-z0-9][A-Za-z0-9_/-]*$
```

This deliberately rejects whitespace, `$`, backticks, parentheses, backslashes, colons, leading dashes, dots, and all other punctuation. Validate owner and repository components separately before constructing `owner/repo`:

```text
owner: ^[A-Za-z0-9][A-Za-z0-9-]*$
repo:  ^[A-Za-z0-9][A-Za-z0-9._-]*$
OID:   ^[0-9a-f]{40}$
```

Accept exactly one of these complete push-URL shapes, with the validated owner/repository substituted and a required `.git` suffix:

```text
git@github.com:<owner>/<repo>.git
https://github.com/<owner>/<repo>.git
```

Reject URL userinfo, embedded credentials, tokens, query strings, fragments, alternate hosts, aliases, ports, schemes, paths, or trailing slashes. Parse outputs as data. Pass every validated variable as its own quoted argument; never splice data into shell source.

## Workflow

### 1. Freeze local identity and the prior commit

Locate the successful `$commit` result and record its full OID as `COMMIT_HEAD`. Inspect:

```bash
git rev-parse --show-toplevel
git status --porcelain=v1 -z --untracked-files=all
git symbolic-ref --quiet --short HEAD
git rev-parse HEAD
git remote get-url --all --push origin
```

Set `BRANCH` from the symbolic branch only after it passes both branch checks. Set `RECORDED_HEAD` from `git rev-parse HEAD` only after it passes the OID allowlist. Require `RECORDED_HEAD` to equal `COMMIT_HEAD`.

Stop on detached `HEAD`, an unresolved repository, failed validation, or intended PR changes outside the committed artifact. Record unrelated worktree paths; they are not included in the PR.

### 2. Pin the only push destination

Write the NUL-safe or line-preserving output of `git remote get-url --all --push origin` to an audit file. Require exactly one non-empty line and no second line. Validate the whole line against one canonical URL shape; do not extract a plausible GitHub substring from a larger URL.

Record that exact line as `PINNED_PUSH_URL`. Map its validated owner and repository components to:

```text
ORIGIN_REPO=<head-owner>/<head-repository>
HEAD_OWNER=<head-owner>
```

Inspect `url.*.insteadOf` and `url.*.pushInsteadOf` configuration at every Git config scope and stop if any rule could rewrite `PINNED_PUSH_URL`. Re-enumerate all push URLs and require byte-for-byte equality with the recorded single URL before any push and again after the push. Stop on multiple URLs, a credential-bearing URL, a non-canonical URL, a URL rewrite, or any change.

### 3. Authenticate and pin repository topology

Run `gh auth status --hostname github.com`, then query the URL-derived repository explicitly:

```bash
gh repo view --repo "$ORIGIN_REPO" \
  --json nameWithOwner,isFork,parent,defaultBranchRef
```

Require `nameWithOwner` to equal `ORIGIN_REPO`. Validate every returned owner, repository, and ref component before reuse.

If `isFork` is false, set `BASE_REPO` to `ORIGIN_REPO`. If `isFork` is true, stop without changing local or remote state and require the user to choose an explicit `owner/repo` base. On reinvocation, accept only an explicitly supplied, allowlisted repository that equals either `ORIGIN_REPO` or the reported `parent.nameWithOwner`; query it directly and restart all validation. Never select the parent automatically.

Query the chosen base explicitly and record its normalized topology:

```bash
gh repo view --repo "$BASE_REPO" \
  --json nameWithOwner,isFork,parent,defaultBranchRef
```

Require its `nameWithOwner` to equal `BASE_REPO`. Record and validate its default branch as `BASE_BRANCH`. Obtain and record the default branch's full `BASE_OID` with a fixed GraphQL query and owner/name supplied as separate quoted variables:

```bash
gh api graphql \
  -f owner="$BASE_OWNER" \
  -f name="$BASE_NAME" \
  -f query='query($owner:String!,$name:String!){repository(owner:$owner,name:$name){defaultBranchRef{name target{oid}}}}'
```

Require the GraphQL branch name to equal `BASE_BRANCH` and its target OID to pass the OID allowlist. Stop if `BRANCH` is `main`, `master`, or `BASE_BRANCH`.

### 4. Freeze the exact commit range

Construct `BASE_FETCH_URL` only as `https://github.com/<validated BASE_REPO>.git`. Fetch the pinned object without updating a named remote:

```bash
git fetch --no-tags "$BASE_FETCH_URL" "$BASE_OID"
git cat-file -e "$BASE_OID^{commit}"
git cat-file -e "$RECORDED_HEAD^{commit}"
git rev-list --reverse "$BASE_OID..$RECORDED_HEAD"
git log --reverse --format='%H %s' "$BASE_OID..$RECORDED_HEAD"
git diff --stat "$BASE_OID...$RECORDED_HEAD"
git diff --check "$BASE_OID...$RECORDED_HEAD"
```

Require at least one commit. Store the ordered full OIDs in an audit file and review every commit and the triple-dot diff. Require `RECORDED_HEAD` to be the final OID and the prior `$commit` OID to be included. Obtain confirmation before proceeding if the range contains any additional commit not already established as intended.

All later range checks use `BASE_OID` and `RECORDED_HEAD`, never `origin/HEAD`, a local base name, or symbolic `HEAD`.

### 5. Check for an existing PR before any push

Query the pinned base repository with the owner-qualified head:

```bash
gh api --method GET \
  "repos/$BASE_REPO/pulls" \
  -f state=open \
  -f "head=$HEAD_OWNER:$BRANCH"
```

Treat the response as structured data. For every candidate inspect:

- `html_url`, `draft`, and `state`;
- `base.repo.full_name`, `base.ref`, and `base.sha`;
- `head.repo.full_name`, `head.repo.owner.login`, `head.ref`, and `head.sha`.

An exact match requires one unambiguous open PR whose base repository/ref/OID equal `BASE_REPO`/`BASE_BRANCH`/`BASE_OID` and whose head repository/owner/ref/OID equal `ORIGIN_REPO`/`HEAD_OWNER`/`BRANCH`/`RECORDED_HEAD`.

If exactly one matching PR exists, stop and return its URL without pushing or changing draft state. If any candidate is ambiguous or mismatches the pinned identities/OIDs, stop and report the discrepancy. Continue only when the result is an empty array.

### 6. Validate remote-branch ownership and divergence

Query the direct pinned destination:

```bash
git ls-remote --heads "$PINNED_PUSH_URL" "refs/heads/$BRANCH"
```

Require either no result or exactly one `<validated OID><TAB>refs/heads/<exact BRANCH>` record.

If the same-name remote branch exists, require all of these exact bindings:

```text
git rev-parse --symbolic-full-name '@{upstream}' = refs/remotes/origin/<BRANCH>
git config --get branch.<BRANCH>.remote          = origin
git config --get branch.<BRANCH>.merge           = refs/heads/<BRANCH>
origin's sole push URL                            = PINNED_PUSH_URL
```

Use the validated branch only as part of one quoted `git config --get "branch.${BRANCH}.remote"` or `.merge` argument. If the remote branch exists without that exact upstream/destination/ref binding, stop; never adopt it automatically.

Fetch the reported remote OID directly, verify it is a commit, and require it to be an ancestor of or equal to `RECORDED_HEAD`:

```bash
git fetch --no-tags "$PINNED_PUSH_URL" "$REMOTE_OID"
git cat-file -e "$REMOTE_OID^{commit}"
git merge-base --is-ancestor "$REMOTE_OID" "$RECORDED_HEAD"
```

Stop on a remote-only commit, ambiguous output, stale binding, or non-fast-forward update. If the remote branch is absent but an upstream claims it exists, stop on the inconsistent topology.

### 7. Recheck and push the recorded OID

Immediately before pushing, recheck:

- symbolic branch equals `BRANCH`;
- local `HEAD` equals `RECORDED_HEAD`;
- the sole push URL equals `PINNED_PUSH_URL`;
- normalized origin/base topology, `BASE_BRANCH`, and `BASE_OID` are unchanged;
- the ordered `BASE_OID..RECORDED_HEAD` OID list equals the audit file;
- the existing-PR query is still empty;
- remote-branch state and exact upstream binding are unchanged.

Push only this argument-safe refspec to the direct pinned URL:

```bash
git push "$PINNED_PUSH_URL" \
  "$RECORDED_HEAD:refs/heads/$BRANCH"
```

Never replace `RECORDED_HEAD` with `HEAD`. On rejection, stop; never retry with force. Verify `git ls-remote --heads "$PINNED_PUSH_URL" "refs/heads/$BRANCH"` returns exactly `RECORDED_HEAD`.

### 8. Post-push race gate

Before creating a PR, repeat every identity check from step 7, including:

- the pinned push URL and remote OID;
- origin and base repository topology;
- default base ref and `BASE_OID`;
- local branch, `RECORDED_HEAD`, and exact ordered range;
- the owner-qualified existing-PR query with all base/head repository, ref, and OID fields.

Create a PR only when every value is unchanged and the PR result is still empty. On any race, base movement, identity change, mismatch, or newly appearing PR, stop and report: the branch was pushed, but no PR was created. Return the existing PR URL only when the new result is one exact match.

### 9. Create the draft without shell-interpolating repository text

Use the fixed path `<absolute-git-dir>/codex-pr-body.md` as `PR_BODY_FILE`. Require it not to exist and not to be a symlink. Create it with the `apply_patch` tool, never a heredoc, command substitution, `printf`, `echo`, or repository-derived shell text. Include only reviewed summary/test-plan text and no AI attribution.

Let GitHub CLI derive the title from commits with `--fill-first`; never place a commit-derived title in shell syntax:

```bash
gh pr create \
  --repo "$BASE_REPO" \
  --draft \
  --base "$BASE_BRANCH" \
  --head "$HEAD_OWNER:$BRANCH" \
  --fill-first \
  --body-file "$PR_BODY_FILE"
```

Every `gh pr` command must include `--repo "$BASE_REPO"` and every head selector must be owner-qualified. Use `apply_patch` to delete `PR_BODY_FILE` after the command, whether creation succeeds or fails. If cleanup fails, report it and do not claim completion.

Verify the result by repeating the pinned owner-qualified `gh api` pull query from step 5. Require exactly one result whose repository, owner, ref, OID, state, and draft fields match the frozen values. Do not feed the returned URL or repository text back into shell syntax.

Return the verified `html_url`, `BASE_REPO`, `ORIGIN_REPO`, branches, `BASE_OID`, `RECORDED_HEAD`, exact commit range, direct push destination, and draft status.

## Pressure and Failure Responses

| Request or condition | Required response |
|---|---|
| Push from main/master/default | Refuse; prescribe a new feature branch, normal push, and draft PR |
| Force push if needed | Refuse both force forms; stop on divergence |
| Open Ready for Review | Keep `--draft`; explain manual conversion after review |
| Multiple or credential-bearing push URLs | Stop before network mutation |
| URL-derived repository is a fork | Stop and require explicit base-repository choice |
| Same-name remote branch lacks exact upstream binding | Stop; never adopt or overwrite it |
| Existing PR is exact | Return it without pushing |
| Existing PR is ambiguous or mismatched | Stop and report fields that differ |
| State changes after push | Report branch pushed but PR not created |

Red flags: missing `$commit` evidence, invalid ref/OID/identity, detached or default branch, mutable or ambiguous destination, fork-base guessing, existing-PR ambiguity, remote collision, symbolic-HEAD push, force syntax, unquoted values, textual shell construction, untrusted title interpolation, non-draft creation, or AI attribution. Stop before the next external mutation whenever any red flag remains.
