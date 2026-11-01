# Sandbox 沙箱系统

harness9 的 Sandbox 系统在 Docker 容器内运行所有工具调用，提供操作系统级隔离——独立进程空间、Capability 丢弃、资源配额——同时对 Agent 完全透明：启用前后工具接口不变，行为完全一致。

---

## 为什么需要 Sandbox？

默认情况下，harness9 的工具（bash、read_file 等）直接在宿主进程中执行，没有容器级隔离。这对本地开发足够安全，但在以下场景需要更强的隔离：

- Agent 执行不受信任的代码或脚本
- 多用户共享同一台机器
- 生产环境部署，需要资源配额保护

---

## 快速启动

```bash
# 1. 确认 Docker 已运行
docker info

# 2. 在 .env 中启用 Sandbox
echo "SANDBOX_ENABLED=true" >> .env

# 3. 启动 harness9
harness9
```

启用后，TUI StatusBar 下方出现 SandboxBar：

```
model: deepseek-v4-pro  |  ~/project  |  session: ...  |  ctx: 12K/256K (5%)
[Sandbox] 3a2f (main) Running
> > 输入任务...
```

---

## 架构设计

### 整体结构

```
internal/sandbox/
├── config.go              # SandboxConfig（从环境变量读取）
├── environment.go         # Environment 接口
├── local_environment.go   # LocalEnvironment（Sandbox 关闭时默认）
├── docker_environment.go  # DockerEnvironment（docker exec 路由）
├── container.go           # Container 五状态生命周期状态机
└── manager.go             # Manager（并发 Sandbox 管理）
```

### Environment 接口

```go
type Environment interface {
    RunBash(ctx context.Context, cmd, workDir string) (string, error)
    ReadFile(ctx context.Context, path string) ([]byte, error)
    WriteFile(ctx context.Context, path string, data []byte) error
    ID() string
    Close(ctx context.Context) error
}
```

两个实现：
- **LocalEnvironment**：进程级，`SANDBOX_ENABLED=false` 时默认使用，行为与引入 Sandbox 前完全一致
- **DockerEnvironment**：容器级，`bash` 命令通过 `docker exec` 路由进容器，文件读写通过 bind mount 与容器共享

### 工具路由

```
LLM 发起 ToolCall "bash"
  → BashTool.Execute(ctx, args)
      ├── env == nil → exec.Command("bash", "-c", cmd)    [Sandbox 关闭]
      └── env != nil → env.RunBash() → docker exec …      [Sandbox 开启]
```

文件工具（read_file / write_file / edit_file）通过 bind mount 共享 workDir，在宿主机侧直接操作，与容器内视图一致。

### Container 生命周期

```
         创建请求
            ↓
        [Pending]     ← docker run 已发出，等待就绪
            ↓ docker inspect Running=true
        [Running]     ← 接受工具调用
            ↓ Close() / Agent 退出
       [Stopping]     ← docker stop -t 5
            ↓
      [Terminated]    ← docker rm 完成

   任意状态 → [Failed]  ← docker 命令报错 / 超时
```

### Manager：并发 Sandbox 管理

```
Manager（单例）
  ├── Create(ctx, workDir)   → Container + DockerEnvironment
  ├── Destroy(ctx, id)       → 停止并移除指定 Container
  ├── DestroyAll(ctx)        → 并发停止所有 Container（defer 退出时调用）
  ├── ReapOrphans(ctx)       → 清理崩溃遗留的孤儿容器（启动时调用一次）
  └── ListAll()              → []SandboxInfo 只读快照（TUI SandboxBar 数据源）
```

---

## 安全加固

每个容器以以下参数启动，参考 HermesAgent DockerEnvironment 安全标准：

| 参数 | 值 | 作用 |
|------|-----|------|
| `--cap-drop all` | — | 丢弃所有 Linux Capabilities |
| `--cap-add DAC_OVERRIDE` | — | 文件权限绕过（包管理器写入所需） |
| `--cap-add SETUID` | — | 允许 apt-get 降权到 `_apt` 用户 |
| `--cap-add SETGID` | — | 允许 apt-get 切换组（与 SETUID 配合） |
| `--security-opt no-new-privileges:true` | — | 禁止 setuid 特权提升 |
| `--pids-limit` | 256 | 防 fork bomb |
| `--tmpfs /tmp` | 256m,nosuid,noexec,nodev | 临时目录隔离 |
| bind mount | `workDir` | 宿主机与容器共享工作目录 |

---

## 并发隔离模型

每个 Agent（包括 Sub-Agent）拥有独立的 Sandbox 容器：

```
主 Agent Sandbox（main）
  ├── workDir bind mount
  └── 独立进程空间、独立 tmpfs

Sub-Agent A Sandbox（sub-1）           ← 任务启动时 Create，结束时 Destroy
Sub-Agent B Sandbox（sub-2）           ← 独立容器，互不影响
```

**安全约束**：子代理 Sandbox 继承父 Manager 的配置（同镜像、同资源限制），不可扩权。

---

## TUI SandboxBar

有活跃 Sandbox 时，TUI StatusBar 下方自动显示 SandboxBar：

```
[Sandbox] 3a2f (main) Running │ 7b1c (sub-1) Running │ 9d4e (sub-2) Pending
```

| 颜色 | 状态 |
|------|------|
| 绿色 | Running（正常运行） |
| 黄色 | Pending（启动中） |
| 灰色 | Stopping / Terminated |
| 红色 | Failed |

终端宽度不足时 SandboxBar 自动隐藏，避免折行破坏布局。

---

## Seatbelt 后端（macOS 原生沙箱）

除 Docker 外，harness9 还提供 macOS 原生的 Seatbelt 后端（`internal/sandbox/seatbelt.go`，对标 OpenAI Codex 的 macOS 沙箱实现）：每次 `RunBash` 通过 `sandbox-exec -p <profile>` 以 **deny-by-default** profile 拉起子进程，无容器生命周期、零守护进程依赖、秒级启动。

两档策略：

| 档位 | 写入 | 网络 | 适用场景 |
|------|------|------|---------|
| `read-only` | 全盘禁写 | 禁止 | 只读探索、代码审查类任务 |
| `workspace-write`（默认） | workDir / TMPDIR / ~/.harness9 可写（符号链接已解析） | 允许 | 常规编码任务 |

```go
env := sandbox.NewSeatbeltEnvironment(workDir, sandbox.SeatbeltWorkspaceWrite)
```

定位是行为护栏而非对抗性安全边界（与 `SANDBOX_BLOCKED_HOSTS` 同一口径）；需要硬隔离的场景仍应使用 Docker 后端。桌面客户端（harness9 Desktop）默认使用该后端，可在设置页切换。详见 [Desktop App](/docs/desktop-app)。

---

## 配置参数

| 环境变量 | 默认值 | 说明 |
|---------|--------|------|
| `SANDBOX_ENABLED` | `false` | 是否启用 Docker Sandbox |
| `SANDBOX_IMAGE` | `ubuntu:22.04` | 容器镜像 |
| `SANDBOX_CPUS` | `1.0` | CPU 限制（docker --cpus） |
| `SANDBOX_MEMORY` | `512m` | 内存限制（docker --memory） |

在 `.env` 文件中设置：

```bash
SANDBOX_ENABLED=true
SANDBOX_IMAGE=ubuntu:22.04
SANDBOX_MEMORY=1g
```

---

## 孤儿容器回收

harness9 用 `label=harness9=1` 标记所有管理的容器。进程意外崩溃后，残留容器在下次启动时由 `Manager.ReapOrphans()` 自动清理：

```bash
# 等价操作（harness9 内部执行）
docker ps -a --filter label=harness9=1 --filter status=exited --format {{.ID}} | xargs docker rm
```

---

## 实现参考

Sandbox 系统参考了主流框架的最佳实践：

| 框架 | 借鉴点 |
|------|--------|
| HermesAgent | Docker 安全加固参数（cap-drop/no-new-privileges/pids-limit/tmpfs）、孤儿容器回收 |
| OpenHarness | path_validator 路径校验 |
| OpenSandbox | 七状态生命周期模型（简化为五状态）、execd 通信模式（简化为 docker exec） |

详见 [Sandbox 系统设计规格](https://github.com/ZhangShenao/harness9/blob/master/docs/%E8%AE%BE%E8%AE%A1%E8%A7%84%E6%A0%BC/2026-06-05-sandbox-design.md) 和 [调研报告](https://github.com/ZhangShenao/harness9/blob/master/docs/%E6%8A%80%E6%9C%AF%E8%B0%83%E7%A0%94/sandbox-design-research.md)。
