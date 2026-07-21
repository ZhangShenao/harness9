# Sandbox System

harness9's Sandbox system runs all tool calls inside a Docker container, providing OS-level isolation — independent process space, capability dropping, resource quotas — while remaining completely transparent to the Agent: the tool interface stays identical and behavior is fully consistent whether Sandbox is enabled or not.

---

## Why do we need Sandbox?

By default, harness9's tools (bash, read_file, etc.) execute directly in the host process, without container-level isolation. This is safe enough for local development, but stronger isolation is needed in the following scenarios:

- The Agent executes untrusted code or scripts
- Multiple users share the same machine
- Production deployments that need resource quota protection

---

## Quick Start

```bash
# 1. Confirm Docker is running
docker info

# 2. Enable Sandbox in .env
echo "SANDBOX_ENABLED=true" >> .env

# 3. Start harness9
harness9
```

Once enabled, a SandboxBar appears below the TUI StatusBar:

```
model: deepseek-v4-pro  |  ~/project  |  session: ...  |  ctx: 12K/256K (5%)
[Sandbox] 3a2f (main) Running
> > Enter a task...
```

---

## Architecture Design

### Overall Structure

```
internal/sandbox/
├── config.go              # SandboxConfig (read from environment variables)
├── environment.go         # Environment interface
├── local_environment.go   # LocalEnvironment (default when Sandbox is off)
├── docker_environment.go  # DockerEnvironment (docker exec routing)
├── container.go           # Container five-state lifecycle state machine
└── manager.go             # Manager (concurrent Sandbox management)
```

### Environment Interface

```go
type Environment interface {
    RunBash(ctx context.Context, cmd, workDir string) (string, error)
    ReadFile(ctx context.Context, path string) ([]byte, error)
    WriteFile(ctx context.Context, path string, data []byte) error
    ID() string
    Close(ctx context.Context) error
}
```

Two implementations:
- **LocalEnvironment**: process-level, used by default when `SANDBOX_ENABLED=false`, behavior is fully identical to before Sandbox was introduced
- **DockerEnvironment**: container-level, `bash` commands are routed into the container via `docker exec`, file I/O is shared with the container via bind mount

### Tool Routing

```
LLM issues ToolCall "bash"
  → BashTool.Execute(ctx, args)
      ├── env == nil → exec.Command("bash", "-c", cmd)    [Sandbox off]
      └── env != nil → env.RunBash() → docker exec …      [Sandbox on]
```

File tools (read_file / write_file / edit_file) share the workDir via bind mount, operating directly on the host side, consistent with the in-container view.

### Container Lifecycle

```
         Creation request
            ↓
        [Pending]     ← docker run issued, waiting for readiness
            ↓ docker inspect Running=true
        [Running]     ← accepts tool calls
            ↓ Close() / Agent exits
       [Stopping]     ← docker stop -t 5
            ↓
      [Terminated]    ← docker rm completed

   Any state → [Failed]  ← docker command error / timeout
```

### Manager: Concurrent Sandbox Management

```
Manager (singleton)
  ├── Create(ctx, workDir)   → Container + DockerEnvironment
  ├── Destroy(ctx, id)       → stop and remove the specified Container
  ├── DestroyAll(ctx)        → concurrently stop all Containers (called on defer exit)
  ├── ReapOrphans(ctx)       → clean up orphaned containers left over from a crash (called once at startup)
  └── ListAll()              → []SandboxInfo read-only snapshot (data source for TUI SandboxBar)
```

---

## Security Hardening

Every container is started with the following parameters, following the HermesAgent DockerEnvironment security standard as a reference:

| Parameter | Value | Purpose |
|------|-----|------|
| `--cap-drop all` | — | Drop all Linux Capabilities |
| `--cap-add DAC_OVERRIDE` | — | File permission bypass (needed for package manager writes) |
| `--cap-add SETUID` | — | Allow apt-get to drop privileges to the `_apt` user |
| `--cap-add SETGID` | — | Allow apt-get to switch groups (works together with SETUID) |
| `--security-opt no-new-privileges:true` | — | Disallow setuid privilege escalation |
| `--pids-limit` | 256 | Prevent fork bombs |
| `--tmpfs /tmp` | 256m,nosuid,noexec,nodev | Temp directory isolation |
| bind mount | `workDir` | Host and container share the working directory |

---

## Concurrent Isolation Model

Every Agent (including Sub-Agents) has its own independent Sandbox container:

```
main agent Sandbox (main)
  ├── workDir bind mount
  └── independent process space, independent tmpfs

Sub-Agent A Sandbox (sub-1)           ← Created when the task starts, destroyed when it ends
Sub-Agent B Sandbox (sub-2)           ← independent container, does not affect others
```

**Security constraint**: a Sub-Agent's Sandbox inherits the parent Manager's configuration (same image, same resource limits) and cannot escalate privileges.

---

## TUI SandboxBar

When there are active Sandboxes, the SandboxBar is automatically displayed below the TUI StatusBar:

```
[Sandbox] 3a2f (main) Running │ 7b1c (sub-1) Running │ 9d4e (sub-2) Pending
```

| Color | Status |
|------|------|
| Green | Running (operating normally) |
| Yellow | Pending (starting up) |
| Gray | Stopping / Terminated |
| Red | Failed |

The SandboxBar is automatically hidden when the terminal width is insufficient, to avoid line wrapping breaking the layout.

---

## Configuration Parameters

| Environment Variable | Default | Description |
|---------|--------|------|
| `SANDBOX_ENABLED` | `false` | Whether to enable Docker Sandbox |
| `SANDBOX_IMAGE` | `ubuntu:22.04` | Container image |
| `SANDBOX_CPUS` | `1.0` | CPU limit (docker --cpus) |
| `SANDBOX_MEMORY` | `512m` | Memory limit (docker --memory) |

Set in the `.env` file:

```bash
SANDBOX_ENABLED=true
SANDBOX_IMAGE=ubuntu:22.04
SANDBOX_MEMORY=1g
```

---

## Orphaned Container Reclamation

harness9 tags all managed containers with `label=harness9=1`. If the process crashes unexpectedly, leftover containers are automatically cleaned up by `Manager.ReapOrphans()` on the next startup:

```bash
# Equivalent operation (executed internally by harness9)
docker ps -a --filter label=harness9=1 --filter status=exited --format {{.ID}} | xargs docker rm
```

---

## Implementation References

The Sandbox system draws on best practices from mainstream frameworks:

| Framework | What was borrowed |
|------|--------|
| HermesAgent | Docker security hardening parameters (cap-drop/no-new-privileges/pids-limit/tmpfs), orphaned container reclamation |
| OpenHarness | path_validator path validation |
| OpenSandbox | Seven-state lifecycle model (simplified to five states), execd communication pattern (simplified to docker exec) |

See the [Sandbox System Design Spec](https://github.com/ZhangShenao/harness9/blob/master/docs/%E8%AE%BE%E8%AE%A1%E8%A7%84%E6%A0%BC/2026-06-05-sandbox-design.md) and the [Research Report](https://github.com/ZhangShenao/harness9/blob/master/docs/%E6%8A%80%E6%9C%AF%E8%B0%83%E7%A0%94/sandbox-design-research.md) for more detail.
