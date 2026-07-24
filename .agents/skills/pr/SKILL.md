---
name: pr
description: Use when the user invokes $pr or /pr, or asks to push an already committed feature branch and open a pull request with GitHub CLI.
---

# Open a Draft Pull Request

## Overview

Push one frozen commit range from a validated GitHub feature branch and open a draft pull request against a pinned repository. Treat every Git ref, remote URL, repository identity, API result, and commit-derived string as untrusted.

**REQUIRED COMMIT EVIDENCE:** Prefer a successful `$commit` result and its reported full commit OID from the current conversation. If no new commit was needed, accept the existing committed `HEAD` only when the user explicitly authorizes publishing the existing committed `HEAD` in the current conversation and the worktree and index are empty. Never treat a clean worktree alone as authorization or create a commit inside this workflow.

## Non-Negotiable Safety Contract

- Never push from detached `HEAD`, `main`, `master`, or the discovered default branch.
- Never use `--force`, `--force-with-lease`, a leading `+` refspec, or any equivalent forced update.
- Freeze the full local commit OID as `RECORDED_HEAD`. Push that OID, never symbolic `HEAD`.
- Enumerate every `origin` push URL inside a non-echoing parser. Never print, log, report, or persist raw or rejected URL values.
- Require exactly one canonical, credential-free GitHub SSH or HTTPS URL and push that validated destination directly, never the mutable remote name `origin`.
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

## Non-Echoing URL and Temporary-State Contract

Never invoke `git remote get-url --all --push origin` through a tool that relays raw stdout/stderr. Use this exact static Python parser logic through `python3` without modifying its source:

```python
import re
import subprocess
import sys

result = subprocess.run(
    ["git", "remote", "get-url", "--all", "--push", "origin"],
    stdout=subprocess.PIPE,
    stderr=subprocess.DEVNULL,
    text=True,
    check=False,
)
urls = result.stdout.splitlines()
patterns = (
    re.compile(r"git@github[.]com:([A-Za-z0-9][A-Za-z0-9-]*)/([A-Za-z0-9][A-Za-z0-9._-]*)[.]git"),
    re.compile(r"https://github[.]com/([A-Za-z0-9][A-Za-z0-9-]*)/([A-Za-z0-9][A-Za-z0-9._-]*)[.]git"),
)
match = None if len(urls) != 1 else next((p.fullmatch(urls[0]) for p in patterns if p.fullmatch(urls[0])), None)
if result.returncode != 0 or match is None:
    sys.stderr.write("origin push destination validation failed (values redacted)\n")
    raise SystemExit(1)
sys.stdout.write(urls[0])
```

The caller captures the single validated stdout value without echoing it. Rejected values remain only in parser memory and never enter an audit file, transcript, error, or report. Use the same parser for every destination recheck. Capture and redact stdout/stderr for every later Git command whose argv contains the validated URL; show only parsed OIDs/status or a constant redacted error.

Create a session temporary directory with mode `0700`. Create every temporary file with exclusive creation and mode `0600`; verify its mode before use. Register cleanup immediately and remove all temporary files/directories on success, refusal, error, cancellation, and every other exit. Never place raw URLs in temporary artifacts. The only content-bearing Git-dir temporary files are the reviewed PR title and body described below, both mode `0600`.

## Workflow

### 1. Freeze local identity and the authorized commit

Choose exactly one evidence mode:

- `COMMIT_RESULT`: locate the successful `$commit` result and record its full OID as `COMMIT_HEAD`;
- `EXISTING_HEAD_AUTHORIZED`: require the user's explicit current-conversation authorization to publish the existing committed `HEAD`, then require the status command below to return no records.

Inspect:

```bash
git rev-parse --show-toplevel
git status --porcelain=v1 -z --untracked-files=all
git symbolic-ref --quiet --short HEAD
git rev-parse HEAD
```

Set `BRANCH` from the symbolic branch only after it passes both branch checks. Set `RECORDED_HEAD` from `git rev-parse HEAD` only after it passes the OID allowlist. In `COMMIT_RESULT` mode, require `RECORDED_HEAD` to equal the reported `COMMIT_HEAD`. In `EXISTING_HEAD_AUTHORIZED` mode, require the worktree and index to be empty, then set `COMMIT_HEAD=RECORDED_HEAD`. Never infer `EXISTING_HEAD_AUTHORIZED` from repository state or from a generic request to open a PR.

Stop on detached `HEAD`, an unresolved repository, failed validation, or intended PR changes outside the committed artifact. Record unrelated worktree paths; they are not included in the PR.

### 2. Pin the only push destination without exposing candidates

Run the static non-echoing parser and capture its validated success value as `PINNED_PUSH_URL` without displaying or persisting it. The parser enforces exactly one canonical credential-free URL and emits only a redacted constant on failure. Do not inspect or recover a rejected value.

Map the validated URL's owner and repository components in memory to:

```text
ORIGIN_REPO=<head-owner>/<head-repository>
HEAD_OWNER=<head-owner>
```

Inspect `url.*.insteadOf` and `url.*.pushInsteadOf` configuration at every Git config scope and stop if any rule could rewrite `PINNED_PUSH_URL`. Re-run the non-echoing parser and require in-memory byte equality with `PINNED_PUSH_URL` before any push, before upstream setup, and after the push. Stop with a redacted error on multiple URLs, credentials, non-canonical syntax, URL rewrite, or any change.

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

Require at least one commit. Store the ordered full OIDs in an audit file and review every commit and the triple-dot diff. Require `RECORDED_HEAD` to be the final OID and `COMMIT_HEAD` to be included. Obtain confirmation before proceeding if the range contains any additional commit not already established as intended.

Set `TITLE_SOURCE_OID=RECORDED_HEAD`, which is the authorized commit artifact and final OID in the frozen range. Capture `git show -s --format=%s "$TITLE_SOURCE_OID"` without echoing it and record the subject in memory as `EXPECTED_TITLE`. Require the source OID to remain in the exact frozen range and require the subject to be non-empty, at most 70 Unicode code points, and free of AI-attribution phrases such as “generated by/with AI,” “AI-generated,” or an AI `Co-Authored-By`. A tool name used as an ordinary conventional-commit scope is not attribution.

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

- `html_url`, `title`, `draft`, and `state`;
- `base.repo.full_name`, `base.ref`, and `base.sha`;
- `head.repo.full_name`, `head.repo.owner.login`, `head.ref`, and `head.sha`.

An exact match requires one unambiguous open PR whose title equals `EXPECTED_TITLE`, whose base repository/ref/OID equal `BASE_REPO`/`BASE_BRANCH`/`BASE_OID`, and whose head repository/owner/ref/OID equal `ORIGIN_REPO`/`HEAD_OWNER`/`BRANCH`/`RECORDED_HEAD`.

If exactly one matching PR exists, stop and return its URL without pushing or changing draft state. If any candidate is ambiguous or mismatches the pinned identities/OIDs, stop and report the discrepancy. Continue only when the result is an empty array.

### 6. Validate remote-branch ownership and divergence

Query the direct pinned destination through static Python `subprocess.run([...], capture_output=True)` and parse only the expected ref record:

```python
["git", "ls-remote", "--heads", PINNED_PUSH_URL, f"refs/heads/{BRANCH}"]
```

Suppress raw stderr and any URL-bearing status text. Require either no result or exactly one `<validated OID><TAB>refs/heads/<exact BRANCH>` record.

If the same-name remote branch exists, require all of these exact bindings:

```text
git rev-parse --symbolic-full-name '@{upstream}' = refs/remotes/origin/<BRANCH>
git config --get branch.<BRANCH>.remote          = origin
git config --get branch.<BRANCH>.merge           = refs/heads/<BRANCH>
origin's sole push URL                            = PINNED_PUSH_URL
```

Use the validated branch only as part of one quoted `git config --get "branch.${BRANCH}.remote"` or `.merge` argument. If the remote branch exists without that exact upstream/destination/ref binding, stop; never adopt it automatically.

Fetch the reported remote OID directly with captured/redacted output, verify it is a commit, and require it to be an ancestor of or equal to `RECORDED_HEAD`:

```text
argv = ["git", "fetch", "--no-tags", PINNED_PUSH_URL, REMOTE_OID]
git cat-file -e "$REMOTE_OID^{commit}"
git merge-base --is-ancestor "$REMOTE_OID" "$RECORDED_HEAD"
```

Stop on a remote-only commit, ambiguous output, stale binding, or non-fast-forward update. If the remote branch is absent but an upstream claims it exists, stop on the inconsistent topology. Before a first push to an absent branch, require any local `refs/remotes/origin/$BRANCH` to be absent or an ancestor of `RECORDED_HEAD`; otherwise the retry-safe tracking ref cannot be established without a forced local ref update, so stop before pushing.

### 7. Recheck and push the recorded OID

Immediately before pushing, recheck:

- symbolic branch equals `BRANCH`;
- local `HEAD` equals `RECORDED_HEAD`;
- the sole push URL equals `PINNED_PUSH_URL`;
- normalized origin/base topology, `BASE_BRANCH`, and `BASE_OID` are unchanged;
- the ordered `BASE_OID..RECORDED_HEAD` OID list equals the audit file;
- the existing-PR query is still empty;
- remote-branch state and exact upstream binding are unchanged.

Invoke the push through a fixed Python `subprocess.run` argv array with `capture_output=True`; never relay raw stdout/stderr:

```python
push = subprocess.run(
    [
        "git",
        "push",
        "--porcelain",
        PINNED_PUSH_URL,
        f"{RECORDED_HEAD}:refs/heads/{BRANCH}",
    ],
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
    text=True,
    check=False,
)
```

Never replace `RECORDED_HEAD` with `HEAD`. Parse only the porcelain ref-status record; redact all other output. On rejection, emit a constant redacted error and stop; never retry with force. Re-run the captured `ls-remote` query and require the remote OID to equal `RECORDED_HEAD`.

After a verified first push to a previously absent branch, make the next `$pr` invocation recoverable:

1. re-run the non-echoing URL parser and require equality with `PINNED_PUSH_URL`;
2. fetch `refs/heads/$BRANCH:refs/remotes/origin/$BRANCH` from `PINNED_PUSH_URL` through a captured static argv array, without a leading `+`;
3. require `refs/remotes/origin/$BRANCH` to resolve to `RECORDED_HEAD`;
4. run `git branch --set-upstream-to="origin/$BRANCH" "$BRANCH"` with separately quoted/argv-safe values;
5. verify `@{upstream}`, `branch.<BRANCH>.remote`, `branch.<BRANCH>.merge`, the non-echoing destination parser, and the remote OID exactly match the pinned destination/ref/OID.

If any upstream-establishment or verification step fails, stop and report only that the branch was pushed but retry-safe upstream setup failed; do not create a PR. On a later `$pr` retry, an existing same-name branch is eligible only when this exact upstream binding, the revalidated destination, exact ref, and fetched remote OID evidence all pass.

#### Residual absent-branch race

The remote-absence check and a normal Git push are not an atomic create-only operation. A same-name branch created after the final check can be normally fast-forwarded when its new OID is an ancestor of `RECORDED_HEAD`; a non-ancestor update is rejected without destructive overwrite. Do not claim that this workflow atomically creates an absent branch.

When the pre-push state was absent, inspect the captured push porcelain record. Only the explicit new-branch status counts as an uncontested creation. An up-to-date or update status means a branch appeared during the window. After verifying the remote OID and establishing the exact upstream binding, stop and report: the branch was pushed, an absent-branch race was detected, and no PR was created.

### 8. Post-push race gate

Before creating a PR, repeat every identity check from step 7, including:

- the pinned push URL and remote OID;
- origin and base repository topology;
- default base ref and `BASE_OID`;
- local branch, `RECORDED_HEAD`, and exact ordered range;
- the owner-qualified existing-PR query with all base/head repository, ref, and OID fields.
- the exact upstream binding created after a first push or previously validated on a retry.

Create a PR only when every value is unchanged and the PR result is still empty. On any race, base movement, identity change, mismatch, or newly appearing PR, stop and report: the branch was pushed, but no PR was created. Return the existing PR URL only when the new result is one exact match.

### 9. Create the draft with a frozen title and static argv

Use fixed paths `<absolute-git-dir>/codex-pr-title.txt` and `<absolute-git-dir>/codex-pr-body.md` as `PR_TITLE_FILE` and `PR_BODY_FILE`. Require both paths to be absent and not symlinks, then create each exclusively with mode `0600`. Populate the body by updating the already-mode-`0600` file with `apply_patch`, never a heredoc, command substitution, `printf`, `echo`, or repository-derived shell text. Include only reviewed summary/test-plan text and no AI attribution.

Write the title from the pinned commit without placing its subject in shell syntax:

```bash
git show -s --format=%s "$TITLE_SOURCE_OID" > "$PR_TITLE_FILE"
```

Verify both content files remain mode `0600`. Read exactly one subject line from the title file as data and require it to equal the recorded `EXPECTED_TITLE`, pass the title validation again, and come from the still-recorded `TITLE_SOURCE_OID=RECORDED_HEAD`. Immediately before creation, recheck local `HEAD`, the exact OID range, `TITLE_SOURCE_OID`, a fresh captured `git show` subject, and the title file bytes.

Invoke GitHub CLI only through a static Python argv array. Read the title file in Python, strip its single trailing newline, and pass the subject as one argv element:

```python
title = title_file.read_text(encoding="utf-8").removesuffix("\n")
create = subprocess.run(
    [
        "gh",
        "pr",
        "create",
        "--repo",
        BASE_REPO,
        "--draft",
        "--base",
        BASE_BRANCH,
        "--head",
        f"{HEAD_OWNER}:{BRANCH}",
        "--title",
        title,
        "--body-file",
        str(PR_BODY_FILE),
    ],
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
    text=True,
    check=False,
)
```

Never interpolate the title into shell text. Capture and parse CLI output as data, and redact errors. Every `gh pr` invocation must include pinned `--repo BASE_REPO`, and every head selector must be owner-qualified.

On every success, refusal, error, or cancellation path after temporary-file creation, delete both `PR_TITLE_FILE` and `PR_BODY_FILE` with `apply_patch`, then verify they are absent. If cleanup fails, report it and do not claim completion.

Verify the result by repeating the pinned owner-qualified `gh api` pull query from step 5. Require exactly one result whose `title` equals `EXPECTED_TITLE` and whose repository, owner, ref, OID, state, and draft fields match the frozen values. Do not feed the returned URL, title, or repository text back into shell syntax.

Return the verified `html_url`, `BASE_REPO`, `ORIGIN_REPO`, branches, `BASE_OID`, `RECORDED_HEAD`, title source OID, exact commit range, and draft status. State that the push destination was validated; never display its raw URL.

## Pressure and Failure Responses

| Request or condition | Required response |
|---|---|
| Existing committed work is explicitly authorized and status is empty | Freeze the validated full `HEAD` OID as `COMMIT_HEAD`; keep every other gate |
| Clean status without explicit existing-HEAD authorization | Stop; repository cleanliness is not commit evidence |
| Push from main/master/default | Refuse; prescribe a new feature branch, normal push, and draft PR |
| Force push if needed | Refuse both force forms; stop on divergence |
| Open Ready for Review | Keep `--draft`; explain manual conversion after review |
| Multiple or credential-bearing push URLs | Stop before network mutation; emit only the constant redacted error |
| URL-derived repository is a fork | Stop and require explicit base-repository choice |
| Same-name remote branch lacks exact upstream binding | Stop; never adopt or overwrite it |
| First push succeeds but upstream setup fails | Report branch pushed/upstream incomplete; create no PR |
| Retry has exact upstream and remote OID evidence | Permit only a normal fast-forward/equal OID update after all rechecks |
| Commit subject contains shell metacharacters | Keep it as title-file data and one Python argv element; never interpolate |
| Absent branch appears during push window | Verify OID, establish upstream, report the race, and create no PR |
| Existing PR is exact | Return it without pushing |
| Existing PR is ambiguous or mismatched | Stop and report fields that differ |
| State changes after push | Report branch pushed but PR not created |

Red flags: missing commit evidence or missing explicit existing-HEAD authorization, non-empty status in `EXISTING_HEAD_AUTHORIZED` mode, raw URL exposure or persistence, invalid ref/OID/identity, detached or default branch, mutable or ambiguous destination, fork-base guessing, existing-PR ambiguity, remote collision, missing retry-safe upstream, symbolic-HEAD push, force syntax, unquoted values, textual shell construction, title source/file/API mismatch, untrusted title interpolation, unacknowledged absent-branch race, non-draft creation, temp cleanup failure, or AI attribution. Stop before the next external mutation whenever any red flag remains.
