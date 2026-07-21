# AutoDev — harness9 Self-Bootstrapping Development Loop

## 1. Design Motivation

harness9 already has a complete eval system and SWE-bench benchmark. The next goal is **self-bootstrapping**: letting the framework develop new features for itself, without a human writing code. AutoDev combines harness9's existing core capabilities — the Skills system, Sub-Agent delegation, Docker Sandbox, and git worktree — into a complete automated pipeline from "feature description" to "PR creation".

```
User describes a requirement
     │
     ▼
Main agent clarifies the requirement + generates a Spec
     │
User confirms the Spec
     │
     ▼
Dev Sub-Agent (Docker Sandbox + git worktree)
     ├── Read the spec → explore the code → implement the feature → write tests
     ├── go build + go test iteration (up to 3 times)
     └── gofmt → git commit → git push → gh pr create
     │
     ▼
Main agent shows the PR URL in the TUI
```

---

## 2. Overall Architecture

### 2.1 Component Relationships

```
┌─────────────────────────────────────────────────────────────┐
│                     harness9 TUI                             │
│                                                              │
│  User: /autodev implement feature X                          │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐  │
│  │              Main agent (spec generation phase)         │  │
│  │                                                        │  │
│  │  Load /autodev AgentSkill ──► guides main agent workflow│  │
│  │  Read AGENTS.md ──► understand current project state   │  │
│  │  Ask the user 2-3 clarifying questions ──► confirm scope│  │
│  │  Generate a structured Spec ──► wait for user "confirm" │  │
│  └──────────────────────────┬─────────────────────────────┘  │
│                             │ User: confirm                   │
│  ┌──────────────────────────▼─────────────────────────────┐  │
│  │              Main agent (delegation phase)              │  │
│  │                                                        │  │
│  │  bash: git worktree add .autodev/<slug>                │  │
│  │  task("dev", spec + worktreePath)                      │  │
│  └──────────────────────────┬─────────────────────────────┘  │
└─────────────────────────────┼───────────────────────────────┘
                              │
          ┌───────────────────▼──────────────────────┐
          │         Dev Sub-Agent                     │
          │  ┌─────────────────────────────────────┐  │
          │  │  Docker Sandbox (Go image)           │  │
          │  │  bash commands → docker exec → in-container │  │
          │  │  file tools → bind mount → worktree   │  │
          │  └─────────────────────────────────────┘  │
          │                                            │
          │  git worktree (feature/autodev-<slug>)    │
          │  ├── Read AGENTS.md, understand coding conventions │
          │  ├── Explore related code                 │
          │  ├── Implement the feature + write *_test.go │
          │  ├── go build ./...                       │
          │  └── Test loop (≤3 times):                │
          │      go test ./... ──► FAIL → fix → retry │
          │                    └── PASS               │
          │                         │                  │
          │  gofmt → git commit → git push → gh pr create │
          └───────────────────────────────────────────┘
```

### 2.2 Core Capabilities Relied On

| Capability Layer | Component | How AutoDev Uses It |
|--------|------|-------------------|
| **Skill system** | `internal/skills/` + `skills/autodev/SKILL.md` | Injected into the main agent's system prompt, defining the three-phase workflow |
| **Sub-Agent delegation** | `internal/subagent/` + `.harness9/agents/dev.md` | Main agent delegates coding work to the dev Sub-Agent via the `task` tool |
| **Docker Sandbox** | `internal/sandbox/` | Dev Sub-Agent runs `go build/test` inside a Go image container |
| **File tools** | `internal/tools/` | Dev Sub-Agent reads and writes code files within the worktree |
| **bash tool** | `internal/tools/bash.go` | Transparently routes to the container, running git, go, gh, and other commands |

---

## 3. Three-Phase Workflow

### Phase 1 — Requirement Clarification

After the main agent loads the `/autodev` skill, it first reads `AGENTS.md` to understand the project's modules and conventions, then asks the user **one key clarifying question at a time**:

1. **Feature boundary**: what is being implemented, and what is explicitly not
2. **Acceptance criteria**: what tests prove the feature is complete
3. **Impact scope**: mainly new files, or modifications to existing modules

After 2-3 rounds of clarification it moves directly to Spec generation, without over-asking.

### Phase 2 — Spec Generation and Confirmation

The main agent generates a structured Spec and presents it to the user:

```markdown
## Feature Spec: <title>

### Feature Description
<concise description of what this feature does>

### Implementation Scope
New files:
- internal/<pkg>/<file>.go
- internal/<pkg>/<file>_test.go

Modified files:
- cmd/harness9/main.go (if a new tool needs to be registered)

### Acceptance Criteria
- [ ] go build ./... passes
- [ ] go test ./... passes
- [ ] (if agent behavior is involved) new eval case added under internal/evals/dataset/

### Out of Scope
- <explicitly list what will not be done>
```

**Key design**: after presenting the Spec, the main agent **calls no tools and ends its turn immediately**, and the TUI naturally waits for user input. Only once the user explicitly enters "confirm" does it proceed. If the user requests changes, the Spec is regenerated and presented again for confirmation, repeating until it is approved.

### Phase 3 — Pre-Flight Checks + Delegation

Once the user confirms, the main agent performs the following in sequence:

**3.1 Pre-flight checks** (stop and wait for a fix if any fails):
```bash
go version          # confirm Go >=1.25
gh auth status      # confirm the gh CLI is logged in
git --version       # confirm git is available
# if SANDBOX_ENABLED=true, prompt to check whether SANDBOX_IMAGE is a Go image
```

**3.2 Create the git worktree**:
```bash
git worktree add .autodev/<slug> -b feature/autodev-<slug>
readlink -f .autodev/<slug>   # get the absolute path to pass to the sub-agent
```

**3.3 Delegate to the dev Sub-Agent**:
```
task("dev", "Here is the Feature Spec to implement:\n\n<full spec text>\n\nWorking directory (git worktree absolute path): <worktreePath>")
```

**3.4 Handle the result**:
- Success: show `✓ PR created: <URL>`, run `git worktree remove .autodev/<slug>`
- Failure: keep the worktree for troubleshooting, tell the user the path, and suggest `git worktree remove .autodev/<slug> --force` for manual cleanup

---

## 4. Dev Sub-Agent Design

The dev Sub-Agent (`.harness9/agents/dev.md`) is the execution core of AutoDev. It receives the Spec and worktreePath, and completes the full development loop:

### Path Conventions

The dev Sub-Agent's working directory (workDir) is the harness9 root directory, but the actual workspace is the git worktree:

| Operation | Path Form |
|------|---------|
| bash command | `cd <worktreePath> && go build ./...` |
| file tool path | `.autodev/<slug>/internal/tools/xxx.go` (relative to workDir) |

### Iteration Loop

```
go build ./...
    ├── FAIL → fix compile errors
    └── PASS
         │
         └── go test ./... -timeout 5m
                  ├── FAIL (1st/2nd/3rd time) → analyze the error → fix the code → retry
                  ├── FAIL (after the 3rd time) → output AUTODEV_RESULT: FAILED
                  └── PASS
                       │
                       └── gofmt -w .
                            → git add -A
                            → git commit -m "feat: <description>"
                            → git push origin HEAD
                            → gh pr create --base master
                            → output AUTODEV_RESULT: SUCCESS + PR URL
```

### Constraints

- Do not modify existing test cases in `*_test.go` (new test functions may be added)
- Do not introduce new dependencies not already present in `go.mod`
- The `git commit` message must start with `feat:`
- If still failing after 3 iterations, honestly report the failure rather than faking a PASS

---

## 5. git worktree Isolation Design

AutoDev uses git worktree rather than modifying code directly in the main repository, for the following reasons:

| Scenario | git worktree Guarantee |
|------|-------------------|
| Dev Sub-Agent breaks the code | Does not affect the main branch; the worktree is an independent file tree |
| Test failure, mid-run termination | The worktree is kept for troubleshooting; the main repository stays clean |
| Concurrent autodev tasks | Each slug has its own directory, with no interference between them |
| Successful completion | `git worktree remove` cleans up; the PR has already been pushed to the remote |

Worktree path convention: `.autodev/<slug>/`, already added to `.gitignore`, so it does not pollute the main repository's `git status`.

---

## 6. Docker Sandbox Integration

When `SANDBOX_ENABLED=true`, the dev Sub-Agent's bash tool is automatically routed to a Docker container:

```
dev sub-agent
    │
    bash("cd <worktreePath> && go test ./...")
    │
    ├── SANDBOX_ENABLED=false → runs as a local process
    └── SANDBOX_ENABLED=true  → docker exec → Go image container
                                         ↕ bind mount
                                    worktree directory (shared files)
```

**Prerequisite**: the default Sandbox image (`ubuntu:22.04`) does not include Go, so it needs to be configured in `.env`:

```bash
SANDBOX_ENABLED=true
SANDBOX_IMAGE=golang:1.25-bookworm
```

If Docker is not used (`SANDBOX_ENABLED=false` or unset), the dev Sub-Agent runs `go test` in a local process; you just need to make sure Go 1.25+ is installed locally.

---

## 7. Usage Guide

### 7.1 Prerequisites

| Condition | Description |
|------|------|
| harness9 running | Start `go run ./cmd/harness9` in the project root directory |
| `gh` CLI logged in | `gh auth status` shows Logged in |
| Go installed | `go version` >= 1.25 |
| `SANDBOX_IMAGE` configured (optional) | Set to `golang:1.25-bookworm` when Docker isolation is needed |

### 7.2 Usage Example

```
# In the harness9 TUI, enter:
/autodev implement a token_count tool that returns an estimated token count for the input text

# The main agent begins clarifying (example dialogue):
> What kind of tokenizer does this tool need to support? A simple whitespace/character estimate, or a real tiktoken integration?

User: A simple estimate, using character count divided by 4 as an approximation

> Got it. Does this tool need to support multiple languages (e.g., counting Chinese by character)?

User: Yes, count Chinese by character and English by word

# The main agent generates the Spec and presents it, waiting for confirmation...

User: confirm

# The main agent creates the worktree and delegates to the dev Sub-Agent
# The dev Sub-Agent starts working... (the TUI shows progress in real time)

# Once complete:
✓ PR created: https://github.com/ZhangShenao/harness9/pull/42
```

### 7.3 Troubleshooting on Failure

If the dev Sub-Agent still hasn't passed the tests after 3 iterations:

```bash
# The worktree is kept; go in to check the current state
cd .autodev/<slug>
go test ./... -v                        # see the specific failure
git diff HEAD                           # see the current changes
git log --oneline -5                    # see the commit history

# After fixing, commit and push manually
git add -A && git commit -m "fix: ..."
git push origin HEAD
gh pr create --base master

# Clean up the worktree
cd -
git worktree remove .autodev/<slug> --force
```

---

## 8. File Location Reference

| File | Responsibility |
|------|------|
| `skills/autodev/SKILL.md` | `/autodev` AgentSkill: three-phase workflow instructions |
| `.harness9/agents/dev.md` | Dev Sub-Agent definition: coding→testing→PR flow (local configuration, not committed to git) |
| `.autodev/` in `.gitignore` | Ignores git worktree temporary directories |
| `internal/skills/loader.go` | Skills loader (`skills/<name>/SKILL.md` → `Index`) |
| `internal/subagent/runner.go` | Sub-Agent Runner (builds an isolated sub-engine + Docker Sandbox) |
| `internal/sandbox/` | Docker container-level isolation infrastructure |
