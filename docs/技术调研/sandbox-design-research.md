# Agent Harness 框架 Sandbox 沙箱能力深度调研报告

> 调研时间：2026-06-05（OpenSandbox 补充：2026-06-05）
> 调研范围：DeepAgents / OpenHarness / OpenCode / OpenClaw / HermesAgent / Claude Agent SDK / **OpenSandbox**
> 调研方法：直接读取 GitHub 仓库源码 + 官方文档

---

## 目录

1. [调研背景](#1-调研背景)
2. [各框架 Sandbox 能力横向对比表](#2-各框架-sandbox-能力横向对比表)
3. [各框架深度分析](#3-各框架深度分析)
   - 3.1 DeepAgents（LangChain）
   - 3.2 OpenHarness（HKUDS）
   - 3.3 OpenCode（Anomaly）
   - 3.4 OpenClaw
   - 3.5 HermesAgent（NousResearch）
   - 3.6 Claude Agent SDK（Anthropic）
4. [设计模式总结](#4-设计模式总结)
5. [对 harness9 的借鉴建议](#5-对-harness9-的借鉴建议)
6. [OpenSandbox 专项深度调研](#6-opensandbox-专项深度调研)
7. [OpenSandbox vs Docker：选型评估](#7-opensandbox-vs-docker选型评估)
8. [参考资料](#8-参考资料)

---

## 1. 调研背景

随着 AI Agent 从实验室走向生产，Sandbox 沙箱已成为 Agent Harness 框架的核心安全基础设施。沙箱决定了 Agent 能在多大范围内"为所欲为"而不危及宿主系统，也直接影响多 Agent 并发时的资源隔离与调度效率。

本报告系统梳理六个主流 Agent Harness 框架的 Sandbox 设计，覆盖：实现技术、创建/回收机制、生命周期管理、Agent 与 Sandbox 的通信方式、安全隔离边界、多 Sandbox 并发策略。后续章节对 OpenSandbox 进行专项深度调研，并给出与裸 Docker 方案的完整选型评估。

---

## 2. 各框架 Sandbox 能力横向对比表

| 维度 | DeepAgents | OpenHarness | OpenCode | OpenClaw | HermesAgent | Claude Agent SDK |
|------|-----------|-------------|----------|----------|-------------|-----------------|
| **主语言** | Python | Python | TypeScript | TypeScript | Python | Python / TypeScript |
| **沙箱实现方式** | 抽象 BaseSandbox + 插件式后端 | Docker（`openharness-sandbox:latest`）+ `srt` 工具包装 | 进程隔离 + Git Worktree（代码快照）| Docker / SSH / OpenShell 多后端 | 本地进程 / Docker / SSH / Modal / Daytona / Singularity | 宿主进程权限控制（Agent SDK 自身）/ Managed Agents 托管沙箱 |
| **沙箱级别** | 框架层提供抽象，OS 级隔离委托给使用者 | OS 级（Docker 网络隔离 + 权限分级）| 进程级 + 文件系统快照级 | 进程级 / 容器级 | 进程级到容器级（随后端变化）| 进程内权限过滤（SDK）/ 完整托管容器（Managed Agents）|
| **网络隔离** | 由后端决定，LocalShellBackend 无隔离 | `--network none`，无网络访问 | 无显式网络隔离，依赖权限控制 | 可选，Docker 后端支持 | Docker 后端支持 `--network=none` | 权限模式控制（不直接管理网络层）|
| **文件系统限制** | virtual_mode 虚拟路径 + root_dir 锁定 | path_validator 路径沙箱 + cwd 边界 | Git Worktree 快照 + 权限请求 | 权限审批 + 路径检查 | safePath + 敏感路径硬保护 + file_safety | 权限模式 + 工作目录 + additionalDirectories 限制 |
| **资源配额** | 无内置（由后端 execute 超时控制）| CPU/Memory（docker run 参数）| 输出大小上限（16MB）+ 超时 | 无显式 cgroup 配额 | CPU/Memory/Disk（Docker/Modal/Daytona 后端）| 无直接配额（Managed Agents 由 Anthropic 管理）|
| **创建方式** | 用户实例化后端类，注入 agent | `start_docker_sandbox()` 启动容器，`atexit` 注册清理 | 按需创建子进程 / Git Worktree | 配置驱动 + 动态创建 | `_create_environment()` 工厂函数，按 `TERMINAL_ENV` 选型 | 每次 `query()` 启动 agent 进程（SDK）|
| **Agent 通信方式** | `execute()` → 服务端 Python 脚本，单行 JSON 响应 | `docker exec -i bash -c` + stdio | subprocess PIPE / ChildProcess stdio | stdio / gateway HTTP | subprocess.PIPE / `docker exec` / SSH / Modal RPC | subprocess stdio（claude 进程）|
| **生命周期状态机** | 由使用方管理（无内置状态机）| inactive → active，atexit 兜底 | 按需启动 / 超时终止 | session 生命周期 | 完整：初始化 → 运行 → 快照 → 清理 / 孤儿回收 | query 级生命周期（start → 流式输出 → stop）|
| **多 Sandbox 并发** | 多实例独立，框架层无并发池 | 单例（module-level global），无池化 | 无限制并发子进程 | session 级隔离 + 多 session 并发 | 并发支持，孤儿容器回收机制 | 并发 query() 调用，session 机制管理上下文 |
| **重用策略** | 无内置重用 | 单实例复用（一个 session 一个容器）| 无重用（每次新建）| session 级别复用 | 跨进程重用（`hermes-` 标签容器），持久化快照 | session resume（跨 query 复用上下文）|
| **安全哲学** | 委托给后端 + HITL 中间件推荐 | fail-closed（无网络访问）+ 多级权限 | 显式权限请求 + AST 分析命令 | 分层审计 + 多级权限模式 | 深度防御（多层 + 行为守卫）+ "非安全边界"声明 | 权限模式 + Hooks 拦截 + 规则引擎 |
| **MCP 支持** | 有（via LangChain 生态）| 有（`openharness/mcp/` 模块）| 有 | 有 | 有（`mcp_tool.py` 163KB）| 有（内置 MCP Server 连接）|

---

## 3. 各框架深度分析

### 3.1 DeepAgents (LangChain)

**仓库**：https://github.com/langchain-ai/deepagents  
**Stars**：~23,897 | 语言：Python | 许可：MIT | 状态：活跃

#### 3.1.1 沙箱实现方式

DeepAgents 采用**插件式后端架构**，将沙箱能力抽象为 `SandboxBackendProtocol` 和 `BaseSandbox` 两个关键接口。

核心层次：

```
SandboxBackendProtocol（接口）
 └── BackendProtocol（文件操作接口）
      ├── FilesystemBackend（本地文件系统，可选 virtual_mode）
      ├── LocalShellBackend（继承 FilesystemBackend，无隔离 Shell）
      └── BaseSandbox（抽象沙箱基类）
           └── [用户自定义] E2B / Modal / Docker 后端
```

`BaseSandbox` 是沙箱层的核心抽象。它**不提供 OS 级隔离**，而是通过以下机制建立软边界：

- **base64 参数编码**：所有文件操作（glob/grep/read/write/edit）使用 base64 编码参数，通过服务端 Python 脚本执行，避免 shell 字符注入
- **virtual_mode 路径限制**：`FilesystemBackend(virtual_mode=True)` 将所有路径锁定在 `root_dir` 之内，拒绝 `..` 和 `~` 穿越

`LocalShellBackend` 继承 `FilesystemBackend`，通过 `subprocess.run(shell=True)` 直接执行命令，官方文档明确标注为"**无任何隔离**"，仅适合本地开发。

**THREAT_MODEL.md 原文**（已验证）：
> "Users who require isolation for untrusted workloads are expected to extend BaseSandbox or use container/VM-level sandboxing — the library does not provide OS-level process isolation."

#### 3.1.2 创建与回收机制

框架**不管理**沙箱生命周期，全部委托给具体后端实现。典型模式：

```python
# 用户负责创建和关闭
sandbox = MySandboxBackend(root_dir="/workspace")
agent = create_deepagents_agent(backend=sandbox)
# 用户负责 sandbox.close()
```

`LocalShellBackend` 直接 `subprocess.run`，无资源回收负担。

#### 3.1.3 Agent 与 Sandbox 通信

通过 `execute()` 方法进行同步/异步 RPC：

1. Agent 调用 `backend.execute(command, timeout=120)` 或文件操作方法
2. 后端封装服务端 Python 脚本，base64 编码参数，通过 shell 执行
3. 脚本返回单行 JSON（success data 或 error code）
4. 框架解析 JSON，以统一错误码（`file_not_found`/`permission_denied` 等）传回 agent

文件操作阈值：
- `MAX_BINARY_BYTES` = 500 KiB（超限报错）
- `MAX_OUTPUT_BYTES` = 500 KiB（超限截断并提示分页）
- `_EDIT_INLINE_MAX_BYTES` = 50 KiB（超限切换为临时文件 upload 策略）

#### 3.1.4 安全隔离边界

**两个关键信任边界**（来自 THREAT_MODEL.md）：
- **TB5（Backend / Host OS）**：路径校验 + 超时，但不控制环境变量、已安装工具、OS 权限
- **TB3（Framework / Agent Code）**：验证 subagent_type 白名单，但不净化 LLM 生成的命令字符串

**LocalShellBackend 的已知威胁**（T3）：即使 `virtual_mode=True` 限制了文件操作路径，`execute()` 对 shell 命令**无路径限制**——这是框架级别的公开已知缺陷。

**推荐防御**：配合 Human-in-the-Loop (HITL) 中间件，在工具层审批所有操作。

#### 3.1.5 CompositeBackend 多路由

`CompositeBackend` 提供路径前缀路由，将不同路径分发给不同后端：

```python
composite = CompositeBackend(
    default=StateBackend(),
    routes={"/memories/": StoreBackend()}
)
```

路由按最长前缀优先匹配，支持批量 upload/download 合并。

#### 3.1.6 优势与不足

**优势**：
- 抽象干净，后端可插拔，适合接入 E2B、Modal 等云沙箱
- base64 编码有效防止参数注入
- 错误码统一，跨后端一致性好

**不足**：
- 框架本身不提供 OS 级隔离，重度依赖使用者配置正确后端
- `LocalShellBackend` 的 shell=True + 无路径限制是生产高危
- 无并发 sandbox 池化、无生命周期状态机

---

### 3.2 OpenHarness (HKUDS)

**仓库**：https://github.com/HKUDS/OpenHarness  
**Stars**：~13,535 | 语言：Python | 许可：MIT | 状态：活跃

#### 3.2.1 沙箱实现方式

OpenHarness 提供**双轨沙箱架构**：

**轨道 1：Docker 容器沙箱**（`src/openharness/sandbox/`）

```python
# 核心类
DockerSandboxSession   # Docker 容器生命周期管理
SandboxAvailability    # 可用性检查
# 主要函数
start_docker_sandbox() # 启动容器，注册 atexit
stop_docker_sandbox()  # 停止容器，清理 session
wrap_command_for_sandbox()  # 命令包装
validate_sandbox_path()     # 路径校验
```

容器配置（`docker_backend.py` 分析）：
- 容器名：`openharness-sandbox-{session_id}`
- 启动命令：`docker run -d --rm ... tail -f /dev/null`
- **网络：`--network none`（完全禁用，fail-closed）**
- 资源：可配 `--cpus` / `--memory`
- 镜像：`openharness-sandbox:latest`（基于 `python:3.11-slim`，非 root 用户 `ohuser`）

**轨道 2：`srt` 工具包装**（适用于 Linux 的 bubblewrap）

`adapter.py` 负责命令包装：
1. 检测平台可用性（Linux/WSL 需要 `bwrap`，macOS 需要 `sandbox-exec`）
2. 将安全配置序列化为临时 JSON 文件
3. 命令包装为：`["srt", "--settings", <path>, "-c", <escaped_command>]`

Docker 命令**绕过** srt 包装，直接走容器隔离。

#### 3.2.2 创建与回收机制

```
startup
  └─ get_docker_availability()   # 检查 Docker 可用性
  └─ DockerSandboxSession()      # 创建 session 对象
  └─ await session.start()       # docker run -d ...
  └─ atexit.register(stop_sync)  # 注册进程退出兜底
```

**关键特点**：
- 使用 **module-level 全局变量** `_active_session`，设计为**单例**
- 无锁保护（注释 `# noqa: PLW0603`），单线程假设
- 停止流程：`docker stop -t 5` + 15 秒整体 deadline

#### 3.2.3 生命周期状态机

```
[Inactive]
    ↓ start_docker_sandbox()
[Active] ← → 命令执行（docker exec）
    ↓ stop_docker_sandbox() 或 atexit
[Inactive]
```

仅两个状态，无 suspended/paused。

#### 3.2.4 Agent 与 Sandbox 通信

```python
# 命令执行
docker exec -i {container_id} bash -c {command}
# 工作目录
docker exec -i -w {workdir} {container_id} ...
# 环境变量注入
docker exec -e KEY=VALUE ...
```

`DockerSandboxSession` 返回 `asyncio.subprocess.Process`，完整的 stdin/stdout/stderr 流控制。

#### 3.2.5 路径安全

`path_validator.py` 实现：

```python
# 两级路径允许
1. cwd 内的路径（os.path.realpath + Path.resolve）
2. extra_allowed 列表中的路径
# 拒绝
relative_to() 失败 → "path {resolved} is outside the sandbox boundary"
```

三重防线：`expanduser()` → `resolve()` → `relative_to()` 验证。

#### 3.2.6 权限系统

`permissions/` 模块提供三级权限模式：

| 模式 | 行为 |
|------|------|
| Default | 写/执行前交互式审批对话框 |
| Auto | 无限制访问（受信任环境）|
| Plan | 阻止所有写/执行操作 |

PreToolUse/PostToolUse Hooks 拦截每次工具调用。

#### 3.2.7 优势与不足

**优势**：
- Docker 网络完全禁用（fail-closed），安全默认值优于多数框架
- 非 root 镜像用户 `ohuser`
- 路径校验逻辑简洁但有效
- srt 双轨方案兼顾 Linux 内核级沙箱（bubblewrap）

**不足**：
- 全局单例设计，不支持多 Sandbox 并发
- 无并发保护（race condition 风险）
- 生命周期状态机过于简单（无 suspended/error 状态）
- 无资源池化

---

### 3.3 OpenCode（Anomaly）

**仓库**：https://github.com/anomalyco/opencode  
**Stars**：~170,025 | 语言：TypeScript | 许可：MIT | 状态：极活跃

#### 3.3.1 沙箱实现方式

OpenCode 的沙箱策略是**进程级隔离 + Git Worktree 文件快照**的组合，而非容器级硬隔离。

**两个核心维度**：

**维度 1：Shell 进程隔离**（`packages/opencode/src/tool/shell.ts`）

通过 Effect 库封装 `child_process`，实现：
- `ChildProcess.make()` 按平台创建隔离子进程（Unix detached，Windows 独立进程树）
- `ShellTool` 在执行前通过 AST（tree-sitter）解析命令，提取文件路径，请求 `external_directory` 权限
- 可配超时（默认 2 分钟），超时后 3 秒宽限期再 SIGKILL
- 输出上限 `maxBytes * 2`，超限写临时文件

**维度 2：Git Worktree 代码快照**（`packages/opencode/src/snapshot/` + `src/worktree/`）

```
每个 session/worktree 对应独立 Git 仓库目录
数据路径：data/snapshot/{projectId}/{hashedWorktreePath}
配置：core.autocrlf=false / core.longpaths=true / core.symlinks=true
```

Worktree 生命周期：
1. `track()` → `git write-tree` 捕获初始快照，建立索引
2. `add()` → 持续追踪文件变更（2MB 单文件上限，`gitignore` 过滤）
3. `remove()` → `git worktree remove --force` + `git branch -D` + `fs.rm()`
4. 后台 GC：每小时 `git gc --prune=7.days`

**安全防护（exec.ts）**：

```typescript
// 禁止 shell=true（防命令注入）
shouldSpawnWithShell() → false
// 拒绝 Windows cmd.exe 危险字符
escapeForCmdExe() 拒绝 [&|<>^%\r\n]，抛出 "Unsafe Windows cmd.exe argument detected"
// NPM 命令特殊处理（防 CVE-2024-27980）
npm/npx → node.exe + cli.js，避免直接执行 .cmd 文件
```

#### 3.3.2 权限系统（`src/permission/`）

声明式权限引擎，三级决策：

```
allow（预批准）→ 直接执行
deny（明确拒绝）→ 立即返回错误
ask（默认）→ 等待用户确认
```

`ask()` 流程：
1. 检查所有 pattern 对应的 approved/provided ruleset
2. 发现 deny 规则 → 立即拒绝
3. 全部 allow → 跳过
4. 其余 → 发布 Permission Event，等待 UI 响应

会话级批量批准（`always` 标记）支持自动解决同 session 内相同规则的后续请求。

#### 3.3.3 Shell 执行环境

`src/shell/shell.ts` 管理 Shell 元数据注册表：

| Shell | 登录模式 | Profile 来源 | 平台 |
|-------|---------|-------------|------|
| bash | -l | ~/.bashrc | Linux/macOS |
| zsh | -l | ~/.zshrc | macOS default |
| powershell | 无 | 无 | Windows |
| fish/nu | deny | — | 禁止使用 |

进程终止：SIGTERM → 200ms 宽限期 → SIGKILL（Windows：`taskkill /f /t`）。

工作目录通过 `cd -- "$1"` 注入（shell 转义防注入）。

#### 3.3.4 多 Sandbox 并发

OpenCode 本身无"Sandbox 池"概念，并发由以下机制保证：
- 每个 `query()` / session 独立子进程
- Worktree 级信号量保护并发访问同一 gitdir
- 并发 session 各自维护独立 worktree 路径

#### 3.3.5 优势与不足

**优势**：
- Git Worktree 快照是代码级隔离的创新设计，支持状态回滚
- AST 驱动的权限分析（tree-sitter），权限请求精确到文件路径
- 严格防止 shell=true 的命令注入
- 双超时机制（主超时 + 无输出超时）

**不足**：
- 无 OS 级容器隔离（网络、CPU、内存均无硬性限制）
- 进程级隔离对恶意代码的防护有限
- 权限系统较复杂，开发者学习成本高

---

### 3.4 OpenClaw

**仓库**：https://github.com/openclaw/openclaw  
**Stars**：~376,884 | 语言：TypeScript | 许可：Other | 状态：极活跃

#### 3.4.1 沙箱实现方式

OpenClaw 采用**多后端可配置沙箱**，安全审计（`src/security/audit.ts`）作为核心安全层。

**执行主机策略**：

| 执行主机 | 说明 |
|---------|------|
| `gateway` | 通过 gateway 转发（远程执行）|
| `sandbox` | 独立沙箱进程 |
| `auto` | 自动选择 |

**安全级别**：

| 安全级别 | 说明 |
|---------|------|
| `deny` | 禁止所有执行 |
| `allowlist` | 白名单许可命令 |
| `full` | 完全放开 |

**沙箱模式**：

```yaml
# 典型配置（来自 README 分析）
agents:
  defaults:
    sandbox:
      mode: "non-main"  # 非 main session 启用沙箱
```

后端选项：Docker（默认）、SSH、OpenShell。

**工具访问控制**（沙箱内典型配置）：

```
允许：bash, process, read, write, edit, sessions_list, sessions_history, sessions_send, sessions_spawn
禁止：browser, canvas, nodes, cron, discord, gateway
```

#### 3.4.2 进程执行（`src/process/exec.ts`）

类似 OpenCode 的安全实践，但更细致：

```typescript
// 安全：绝不 shell=true（防命令注入）
shouldSpawnWithShell() → false
// Windows 危险字符拒绝
escapeForCmdExe() 拒绝 [&|<>^%\r\n]
// NPM 特殊处理（防 CVE-2024-27980）
```

**双超时机制**：
- `timeoutMs`：主超时（kill 进程）
- `noOutputTimeoutMs`：无输出超时（无数据到达则 kill）

**输出上限**：默认 16MB，超限维持滑动窗口（新数据挤出旧数据）。

**终止方式**：SIGKILL（`taskkill /PID /T /F` on Windows）。

#### 3.4.3 安全审计体系（`src/security/audit.ts`）

OpenClaw 的安全审计是六个框架中**最系统化**的，覆盖五个维度：

**1. 执行安全审计**

检测 `tools.exec.host="sandbox"` 而 `sandbox.mode="off"` 的配置矛盾，fail-closed 处理。

**2. 频道暴露分析**

递归扫描 channel 配置中的 `groupPolicy="open"` / `dmPolicy="open"`，交叉引用 exec-enabled agents，识别不可信频道能否触达可执行 scope。

**3. 权限模式校验**

Claude CLI 的受限权限模式与 OpenClaw 的 YOLO 执行并存时，标记为控制绕过（bypass）。

**4. 安全二进制管理**

`safeBins` 限制可信解释器二进制，防止广泛 shell 类执行入口；flagging 危险可信目录（tmp、home、包管理器 bin）防二进制投毒。

**5. 插件信任审计**

`audit-plugins-trust.ts` 评估已安装插件的代码安全性。

#### 3.4.4 Dockerfile 安全配置

多阶段构建，运行时阶段：
- 基础镜像：`node:24-bookworm-slim`（SHA256 固定，Dependabot 管理）
- 非 root 用户 `node`（uid 1000）
- 目录权限：`700`（仅 owner 访问）
- 默认绑定 `127.0.0.1`（本地回环，防外部暴露）
- `tini` 作为 PID 1（信号处理 + 僵尸进程回收）
- 健康检查：`/healthz`（liveness）+ `/readyz`（readiness）

#### 3.4.5 优势与不足

**优势**：
- 安全审计体系最为完整，覆盖配置、执行、频道、插件多维度
- 双超时机制健壮
- Dockerfile 安全实践规范（固定 SHA256、非 root、tini）
- 跨平台适配完善（launchd/systemd/schtasks 守护进程管理）

**不足**：
- 沙箱具体实现细节文档较少，主要依赖配置驱动
- 多后端支持程度参差
- 无内置资源配额（CPU/内存）

---

### 3.5 HermesAgent（NousResearch）

**仓库**：https://github.com/NousResearch/hermes-agent  
**Stars**：~181,148 | 语言：Python | 许可：MIT | 状态：极活跃

HermesAgent 拥有六个框架中**最完整的 Sandbox 架构**，通过统一抽象支持 6 种执行环境后端。

#### 3.5.1 核心抽象：BaseEnvironment

```python
class BaseEnvironment(ABC):
    # 子类必须实现
    @abstractmethod
    def _run_bash(self, ...) -> ProcessHandle: ...
    @abstractmethod
    def cleanup(self): ...
    
    # 公共接口（基类实现）
    def execute(command, timeout=60, stdin=None): ...
```

**统一接口，6 种后端实现**：

| 后端 | 隔离级别 | 持久化 | 网络控制 | 适用场景 |
|------|---------|--------|---------|---------|
| `LocalEnvironment` | 进程级 | 无 | 无 | 本地开发 |
| `DockerEnvironment` | 容器级 | 可选（bind mount）| `--network=none` 可选 | CI/隔离执行 |
| `SSHEnvironment` | 远程进程级 | 远程文件系统 | 网络隔离（远端）| 远程开发机 |
| `ModalEnvironment` | 云容器级 | 快照持久化 | Modal 网络 | 云 AI 工作负载 |
| `ManagedModalEnvironment` | 云托管沙箱 | 快照 | Modal 网络 | Nous 托管 |
| `DaytonaEnvironment` | 云 Workspace | 持久化 | Daytona 控制 | 企业开发环境 |
| `SingularityEnvironment` | 容器（HPC）| overlay 持久化 | HPC 网络 | 高性能计算集群 |

后端选择由 `TERMINAL_ENV` 环境变量驱动 `_create_environment()` 工厂函数。

#### 3.5.2 LocalEnvironment 详解

**环境变量过滤**（安全关键）：

`_HERMES_PROVIDER_ENV_BLOCKLIST` 黑名单过滤：
- LLM 提供商 API Key（OpenAI、Anthropic、Bedrock Bearer Token）
- 消息平台 Token（Slack、Discord、Telegram、Signal）
- 工具凭证（GitHub、Modal、Daytona）
- 系统 Token（Home Assistant、邮件凭证）

特别说明：`env_passthrough` 模块允许选择性恢复被阻断的变量（应对 GHSA-rhgp-j443-p4rf 漏洞修复）。

**安全工作目录恢复**：

`_resolve_safe_cwd()` 处理目录被删除的场景，沿文件树向上找到最近存在的祖先，防止 `FileNotFoundError` 锁死终端工具。

**进程清理**：

POSIX：`os.killpg(pgid, SIGTERM)` → 1 秒宽限期 → `SIGKILL`  
兜底：reap wrapper process。

#### 3.5.3 DockerEnvironment 详解

**容器创建参数**（全面安全加固）：

```bash
docker run -d \
  --name hermes-{8个随机hex字符} \
  --label hermes-agent=1 \
  --cap-drop all \
  --no-new-privileges \
  --pids-limit 256 \
  --cap-add DAC_OVERRIDE,CHOWN,FOWNER \
  --network none \
  --cpus {float} \
  --memory {int}m \
  --tmpfs /tmp:size=512m,nosuid \
  --tmpfs /var/tmp:size=256m,noexec,nosuid \
  --tmpfs /run:size=64m,noexec,nosuid \
  sleep infinity
```

关键安全设计：
1. `--cap-drop all`：丢弃所有 Linux Capabilities
2. 仅恢复 `DAC_OVERRIDE/CHOWN/FOWNER`（包管理器需要）
3. `--no-new-privileges`：防止 setuid 特权提升
4. `--pids-limit 256`：防 fork bomb
5. tmpfs 尺寸限制 + noexec/nosuid 挂载选项

**s6-overlay 检测**：自动检测镜像 entrypoint 是否为 `/init`（s6-overlay），若是则不加 `--init` 且 `/run` 不用 noexec 挂载（两者不兼容）。

**文件系统策略**：

| 模式 | 挂载方式 | 路径 |
|------|---------|------|
| Ephemeral | tmpfs | `/workspace`(10GB)、`/home`(1GB)、`/root`(1GB) |
| Persistent | bind mount | `~/.hermes/sandboxes/docker/{task_id}/` |

**Agent 与容器通信**：

```bash
docker exec -i {container_id} bash -c {command}
```

环境变量通过 session snapshot 文件传递（初始化一次，后续 source），避免每次 `-e KEY=VALUE`。

**跨进程容器重用**：

通过 `label=hermes-agent=1` 标签识别已有容器，同一 task_id + profile 的新进程直接 attach 复用，无需重新 create。

**孤儿容器回收**：

`reap_orphan_containers()` 周期性清理：
- 条件：`label=hermes-agent=1` + status=exited + 超过 600 秒
- 恢复场景：SIGKILL/OOM 导致 cleanup 被跳过

**容器故障自愈**：

命令返回 "No such container" → 探测可复用容器 → 失败则新建 → 重试命令，一次透明恢复。

#### 3.5.4 ModalEnvironment 详解

**Sandbox 创建**：

1. 通过 `Sandbox.create()` 创建 Modal sandbox（而非旧版 runtime wrapper）
2. `_resolve_modal_image()` 解析镜像规格（registry 引用或快照 ID）
3. 尝试从持久化快照恢复（失败则回退到 base image）
4. 挂载凭证文件、Skills、Cache 目录
5. 启动 `sleep infinity` 持续进程

**后台线程管理**：

`_AsyncWorker` 类在独立线程维护事件循环，封装 Modal SDK 的异步调用，实现线程安全的 Modal 操作。

**通信**：

```python
sandbox.exec.aio()  → _ThreadedProcessHandle
# 取消：sandbox.terminate.aio()
# stdin: heredoc 模式（1MB 块，绕过 Modal 2MB 上限）
# 大文件：gzip tar + base64，绕过 ARG_MAX_BYTES 限制
```

**清理与快照**：

`cleanup()` 流程：文件同步回 host → `snapshot_filesystem.aio()` 捕获快照 → 存储快照 ID 到 `modal_snapshots.json` → `sandbox.terminate()`。

#### 3.5.5 DaytonaEnvironment 详解

**资源规格**：

| 资源 | 默认值 | 上限 |
|------|------|------|
| CPU | 1 core | — |
| Memory | 5 GiB | — |
| Disk | 10 GiB | 10 GiB（平台上限）|

使用 `threading.Lock()` 保护并发 sandbox 状态修改。

`_ensure_sandbox_ready()` 在每次命令执行前检查 sandbox 状态，自动重启 stopped/archived sandbox。

#### 3.5.6 SingularityEnvironment（HPC 场景）

- 使用 `apptainer exec instance://[id] bash -c [command]` 执行
- `--containall --no-home` 硬化参数
- 可选 writable tmpfs 或 persistent overlay
- HPC scratch 目录优先（`/scratch`），减少 I/O 延迟
- `apptainer` / `singularity` 二进制自动检测

#### 3.5.7 file_safety.py：多层文件保护

**禁止写入的路径**（exact match）：
`.bashrc`、`.zshrc`、`.env`、`/etc/sudoers`、`/etc/passwd` 等

**禁止写入的目录前缀**：
`~/.ssh`、`~/.aws`、`~/.gnupg`、`~/.kube`、`/etc/sudoers.d` 等

**禁止读取**：
`HERMES_HOME/skills/.hub`（防提示注入）、`mcp-tokens/`（OAuth token）、`.env`/`.envrc`（项目凭证）

特别说明：所有保护**明确声明非安全边界**，文档原文："NOT a security boundary — terminal access allows bypassing via `cat`"。设计意图是"defense-in-depth 信号"而非硬性围栏。

#### 3.5.8 tool_guardrails.py：行为守卫

循环检测与失败追踪：
- **精确失败**（相同工具 + 相同参数）：warn@2 → block@5
- **同工具失败**（不同参数）：warn@3 → halt@8
- **幂等无进展**（只读操作重复）：warn@2 → block@5

工具分类：幂等工具（文件读/搜索/Web）vs 变更工具（终端/写文件/浏览器）。

#### 3.5.9 优势与不足

**优势**：
- 最完整的多后端沙箱体系（6 种），统一 BaseEnvironment 接口
- Docker 安全加固最彻底（cap-drop + no-new-privileges + pids-limit + tmpfs）
- 跨进程容器复用机制成熟
- 孤儿容器自动回收
- 多层文件保护（file_safety + guardrails + 环境变量过滤）
- HPC/云原生环境覆盖（Singularity/Modal/Daytona）

**不足**：
- file_safety "非安全边界"声明说明 terminal 工具仍可绕过所有限制
- LocalEnvironment 仅有进程级隔离
- SSH 后端依赖网络连通性，对断连处理有复杂性

---

### 3.6 Claude Agent SDK (Anthropic)

**文档**：https://code.claude.com/docs/en/agent-sdk/overview  
**语言**：Python + TypeScript | 状态：活跃

#### 3.6.1 两种沙箱模式

Claude Agent SDK 提供两种截然不同的沙箱模式：

**模式 A：Agent SDK 自持模式**

Agent 运行在**使用者基础设施上**，文件操作、命令执行均在本地或使用者配置的环境中进行。安全边界由 SDK 的权限模式和 Hooks 实现。

**模式 B：Managed Agents（Anthropic 托管沙箱）**

Anthropic 完整托管 Agent 运行环境，提供**每 session 独立的托管沙箱**。对比如下：

| 维度 | Agent SDK（自持）| Managed Agents（托管）|
|------|----------------|----------------------|
| 运行位置 | 使用者进程/基础设施 | Anthropic 托管基础设施 |
| 接口 | Python/TS 库 | REST API |
| Agent 操作对象 | 使用者文件系统 | 托管沙箱（per session）|
| Session 状态 | JSONL（本地文件系统）| Anthropic 托管事件日志 |
| 自定义工具 | 进程内函数 | Claude 触发，使用者执行并返回结果 |

#### 3.6.2 权限模式体系

六种权限模式，构成 Agent SDK 的安全核心：

| 模式 | 描述 | 沙箱含义 |
|------|------|---------|
| `default` | 未审批工具触发 `canUseTool` 回调 | 最谨慎，要求显式批准 |
| `dontAsk` | 未预批准工具直接 deny，不弹窗 | 固定工具集，适合无头 Agent |
| `acceptEdits` | 文件编辑类操作自动批准 | 限工作目录内文件操作 |
| `bypassPermissions` | 所有工具无需审批 | 最危险，仅限受控环境 |
| `plan` | 只读工具（探索不修改）| read-only 沙箱 |
| `auto`（仅 TS）| 模型分类器自动决策 | 智能化权限判断 |

**权限评估顺序**（已验证文档）：

```
1. Hooks（PreToolUse）→ 可 deny/allow
2. Deny 规则（disallowed_tools）→ 移除工具定义 or 模式匹配拦截
3. Permission Mode → bypassPermissions 放行一切
4. Allow 规则（allowed_tools）→ 预批准列表
5. canUseTool 回调 → 运行时决策
```

**重要警告**（来自官方文档）：
> "`allowed_tools` does not constrain `bypassPermissions`."
> "Subagent inheritance: When the parent uses `bypassPermissions`, all subagents inherit that mode."

#### 3.6.3 Hooks 系统（`/en/agent-sdk/hooks`）

19 种 Hook 事件，覆盖完整 Agent 生命周期：

| Hook 事件 | 安全用途 |
|-----------|---------|
| `PreToolUse` | 阻止危险命令（最核心） |
| `PostToolUse` | 审计/修改工具输出 |
| `Stop` / `SessionStart` / `SessionEnd` | 生命周期资源管理 |
| `SubagentStart` / `SubagentStop` | 子 Agent 监控 |
| `WorktreeCreate` / `WorktreeRemove` | Git 工作区管理 |
| `PermissionRequest` | 自定义权限处理 |

**路径重定向到沙箱目录**（官方示例）：

```python
async def redirect_to_sandbox(input_data, tool_use_id, context):
    if input_data["tool_name"] == "Write":
        original_path = input_data["tool_input"].get("file_path", "")
        return {
            "hookSpecificOutput": {
                "hookEventName": "PreToolUse",
                "permissionDecision": "allow",
                "updatedInput": {
                    **input_data["tool_input"],
                    "file_path": f"/sandbox{original_path}",  # 重定向到沙箱目录
                },
            }
        }
```

**并行 Hooks 安全规则**：多 Hook 并行执行时，`deny` 优先级最高（任意一个 deny → 阻止操作）。

#### 3.6.4 Subagent 权限继承

子 Agent 从父 Agent 继承权限模式，**且不可被子 Agent 覆盖**：

- `bypassPermissions` → 子 Agent 自动获得完整系统访问，无任何审批
- 这是官方文档明确标注的安全风险

子 Agent 通过 `AgentDefinition` 定义独立 system prompt 和工具集，运行在隔离 session 上，但权限模式不隔离。

#### 3.6.5 acceptEdits 的路径边界

`acceptEdits` 模式的自动批准范围：

```
- 文件编辑（Edit/Write 工具）
- 文件系统命令：mkdir/touch/rm/rmdir/mv/cp/sed
```

**路径边界**：仅限工作目录（`cwd`）或 `additionalDirectories` 指定路径内。外部路径和敏感路径仍需审批。

#### 3.6.6 Worktree Hooks（TypeScript 专属）

`WorktreeCreate` / `WorktreeRemove` 两个 Hook 专为 Git worktree 工作区管理设计，与 OpenCode 的 worktree 快照机制对应，可用于：
- 追踪隔离工作区创建
- 清理 worktree 相关资源

#### 3.6.7 优势与不足

**优势**：
- 权限评估链路清晰，文档详尽
- 19 种 Hook 事件覆盖最全面的拦截点
- Managed Agents 提供完全托管的真正沙箱（Anthropic 负责基础设施）
- 路径输入重定向（Hooks 中 `updatedInput`）是优雅的软沙箱方案
- 动态权限模式切换（`setPermissionMode()` 流式切换）

**不足**：
- 自持模式无 OS 级容器隔离
- `bypassPermissions` 下 `disallowed_tools` 是唯一硬性拦截
- 子 Agent 权限模式不可精细化控制（继承而非定制）
- Managed Agents 为托管服务，不开源，用户对沙箱实现细节无控制权

---

## 4. 设计模式总结

### 4.1 沙箱实现的三个层次

通过对六个框架的分析，可以将沙箱实现归纳为三个层次，从"软"到"硬"递增：

```
Layer 1：权限控制层（软沙箱）
  - 基于规则的 allow/deny 过滤
  - Hook 拦截与路径重定向
  - 代表框架：Claude Agent SDK（自持模式）、DeepAgents（HITL 推荐）

Layer 2：进程隔离层（中等隔离）
  - 独立子进程 + 禁止 shell=true
  - 文件系统路径沙箱（virtual_mode / path_validator）
  - Git Worktree 代码快照（版本级隔离）
  - 代表框架：OpenCode、OpenClaw、HermesAgent（LocalEnvironment）

Layer 3：容器/VM 层（硬隔离）
  - Docker/Singularity/Modal 容器级隔离
  - 网络禁用、capability 丢弃、资源配额
  - 跨进程 session 复用 + 快照持久化
  - 代表框架：HermesAgent（DockerEnvironment）、OpenHarness（DockerSandbox）
```

### 4.2 五种核心设计模式

**模式 1：Spawn-Per-Call 模型**

每次工具调用启动新 bash 进程，通过 session snapshot 恢复环境变量。既保证命令间隔离，又避免重复加载 profile。

HermesAgent 的 `BaseEnvironment._wrap_command()` 是最完整的实现：
- source 环境快照 → cd 工作目录 → 执行命令 → dump 环境变量 → emit CWD 标记

**模式 2：标签化容器复用**

用标签（如 `hermes-agent=1`）标记框架管理的容器，支持跨进程 attach 复用，配合孤儿容器回收机制防止泄漏。HermesAgent 是典型代表。

**模式 3：洋葱模型权限拦截**

多个 Hook 按顺序（或并行）拦截工具调用，deny 优先级最高。Claude Agent SDK 的权限评估链和 HookRegistry 的洋葱模型是主流实现。

**模式 4：fail-closed 网络策略**

不确定是否需要网络时，默认禁用（`--network none`）而非开放。OpenHarness 和 HermesAgent Docker 模式均采用此策略。相比 allowlist，fail-closed 大幅降低网络泄露风险。

**模式 5：快照持久化 + 增量恢复**

Modal、Daytona 后端通过文件系统快照持久化 sandbox 状态，下次创建时从快照恢复（而非全量初始化），显著降低冷启动时间。OpenCode 的 Git Worktree 快照是文件级版本管理的变体。

### 4.3 安全哲学对比

| 框架 | 安全哲学 | 核心主张 |
|------|---------|---------|
| DeepAgents | 委托给基础设施 | "用工具/沙箱层执行边界，而非期待模型自我约束" |
| OpenHarness | fail-closed | 网络默认禁用，宁可误拒也不误放 |
| OpenCode | 显式声明 + AST 分析 | 权限请求需说明原因，命令解析到文件路径级 |
| OpenClaw | 系统化审计 | 安全审计作为独立模块，多维度交叉分析 |
| HermesAgent | 深度防御 + 明确声明限制 | 多层保护，但所有层均声明"非安全边界" |
| Claude Agent SDK | 层次化权限链 + 托管选项 | 自持模式权限模式，Managed 完全托管 |

### 4.4 多 Sandbox 并发模式

| 框架 | 并发模式 | 隔离粒度 |
|------|---------|---------|
| DeepAgents | 多实例独立（用户管理）| 后端实例级 |
| OpenHarness | 全局单例（不支持并发）| session 级（限 1）|
| OpenCode | 无限制子进程 + Worktree 信号量 | session 级 |
| OpenClaw | session 级隔离 | session 级 |
| HermesAgent | 并发支持 + 标签化复用 | task + profile 级 |
| Claude Agent SDK | 并发 query() + session resume | session 级 |

---

## 5. 对 harness9 的借鉴建议

### 5.1 当前 harness9 的沙箱状态

harness9 当前实现（基于 AGENTS.md 分析）：

- **进程级隔离**：`bash` 工具通过 `subprocess` 执行命令
- **路径沙箱**：`safePath()` 防 Path Traversal，`~/.ssh`/`~/.aws` 等敏感路径硬保护
- **权限控制**：HookDecision（allow/deny/ask）+ DangerHook（19 条高危模式）+ PermissionHook（JSON 白名单）
- **Sub-Agent 隔离**：独立 Session + 受限工具集 + 禁止权限扩张

相比调研框架，harness9 当前属于**Layer 1~Layer 2 之间**，权限控制完备但缺少容器级硬隔离。

### 5.2 分级建议（优先级排序）

---

**P0：进程级安全加固**（立即可做，无需引入容器）

**建议 1：禁止 `shell=true` + 参数向量化**

参考 OpenCode/OpenClaw 的 `shouldSpawnWithShell() → false`：

```go
// 当前：可能使用 bash -c 字符串插值
// 建议：使用 exec.Command 参数数组，避免 shell 解析
cmd := exec.Command("bash", "-c", userCommand)
// 进一步：对 userCommand 进行 AST 分析（参考 OpenCode tree-sitter）
```

**建议 2：双超时机制**

参考 OpenCode/OpenClaw 的主超时 + 无输出超时：

```go
type ToolTimeout struct {
    ExecTimeout    time.Duration // 命令总执行超时
    IdleTimeout    time.Duration // 无输出超时（当前无此机制）
}
```

**建议 3：输出大小硬性上限**

参考 OpenCode（16MB）/ HermesAgent（可配）在引擎层统一截断：

```go
// 当前 maxLogOutputLen = 512 bytes（仅日志层）
// 建议在 engine 层设置工具输出的字节上限（如 1MB），
// 超限写临时文件并在工具结果中注入摘要引用
```

---

**P1：进程隔离强化**（中期，需要适度改动）

**建议 4：环境变量过滤**

参考 HermesAgent `_HERMES_PROVIDER_ENV_BLOCKLIST`，在 Sub-Agent 和工具执行时过滤敏感 env：

```go
// 子 Agent 启动时过滤
blocklist := []string{
    "ANTHROPIC_API_KEY", "OPENAI_API_KEY",
    "AWS_SESSION_TOKEN", // ... 更多凭证变量
}
for _, key := range blocklist {
    subEnv.Unset(key)
}
```

**建议 5：safe CWD 恢复**

参考 HermesAgent `_resolve_safe_cwd()`：工具执行时工作目录不存在时，向上溯找存在的祖先：

```go
func resolveSafeCWD(workDir string) string {
    for d := workDir; d != "/"; d = filepath.Dir(d) {
        if _, err := os.Stat(d); err == nil {
            return d
        }
    }
    return os.TempDir()
}
```

**建议 6：基于 Git Worktree 的代码快照**（可选）

参考 OpenCode 的 snapshot 机制，为每个 Session 维护 Git 快照：
- 任务开始时 `git write-tree` 捕获状态
- 提供 `session.Reset()` 恢复到初始状态
- 实现沙箱任务的副作用回滚

---

**P2：容器级隔离**（长期，生产级需求）

**建议 7：可插拔 Sandbox 后端**

参考 HermesAgent 的 `BaseEnvironment` 接口设计，为 harness9 抽象 `Sandbox` 接口：

```go
// internal/sandbox/sandbox.go
type Sandbox interface {
    Execute(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error)
    Cleanup(ctx context.Context) error
    ID() string
}

// 实现层：
// sandbox/local.go  - 当前默认，进程级
// sandbox/docker.go - Docker 容器（参考 HermesAgent 安全配置）
```

**建议 8：Docker 安全加固配置**

若实现 Docker 后端，完整参考 HermesAgent 的 `DockerEnvironment`：

```go
dockerArgs := []string{
    "run", "-d",
    "--cap-drop", "all",
    "--no-new-privileges",
    "--pids-limit", "256",
    "--cap-add", "DAC_OVERRIDE,CHOWN,FOWNER",
    "--network", "none",  // fail-closed
    "--cpus", "1.0",
    "--memory", "512m",
    "--tmpfs", "/tmp:size=256m,nosuid",
    "sleep", "infinity",
}
```

**建议 9：孤儿容器回收**

参考 HermesAgent `reap_orphan_containers()`，在 harness9 启动时清理残留容器：

```go
// 通过 label=harness9=1 标记所有 harness9 管理的容器
// 启动时扫描 status=exited 且超过 N 分钟的容器并删除
```

---

**P3：权限系统完善**（持续优化）

**建议 10：行为守卫（Behavioral Guardrails）**

参考 HermesAgent `tool_guardrails.py`，在 engine 层追踪工具调用模式：

```go
type ToolGuardrail struct {
    // 精确失败计数（相同工具 + 参数）
    exactFailures map[string]int
    // 同工具失败计数
    toolFailures  map[string]int
}

// warn@2 → block@5（精确失败）
// warn@3 → halt@8（同工具失败）
```

**建议 11：沙箱路径级审计**

参考 OpenClaw `security/audit.ts` 的审计体系，为 harness9 实现配置一致性检查：

```go
// 检测：Sub-Agent 工具集比父 Agent 更宽泛（权限扩张）
// 检测：DangerHook 禁用时 bash 工具仍可访问敏感路径
// 检测：PermissionMode=bypass 时 sensitive path 保护是否生效
```

### 5.3 优先级矩阵

| 建议 | 实现复杂度 | 安全收益 | 优先级 |
|------|----------|---------|--------|
| 双超时机制 | 低 | 中 | P0 ★★★ |
| 输出大小硬性上限 | 低 | 中 | P0 ★★★ |
| 环境变量过滤（Sub-Agent）| 低 | 高 | P1 ★★★ |
| safe CWD 恢复 | 低 | 低 | P1 ★★ |
| 行为守卫（loop detection）| 中 | 高 | P1 ★★★ |
| Git Worktree 快照 | 高 | 中 | P2 ★★ |
| 可插拔 Sandbox 接口 | 中 | 高（架构）| P2 ★★★ |
| Docker 安全加固后端 | 高 | 极高 | P3 ★★ |
| 孤儿容器回收 | 中 | 中 | P3 ★★ |
| 配置审计体系 | 高 | 中 | P3 ★ |

---

## 6. OpenSandbox 专项深度调研

**仓库**：https://github.com/opensandbox-group/OpenSandbox  
**Stars**：~11,286 | 语言：Python（server）/ Go（execd/egress）| 许可：Apache 2.0 | 状态：活跃  
**CNCF Landscape**：已收录 | **创建**：2025-12-17 | **最近更新**：2026-06-05

### 6.1 项目定位

OpenSandbox 是一个**面向 AI Agent 的通用沙箱平台**，而非框架内部的一个模块。它的定位是：

> "Secure, Fast, and Extensible Sandbox runtime for AI agents."

与 HermesAgent 的 `DockerEnvironment`（框架内内联 Docker 管理）不同，OpenSandbox 将沙箱能力**外置为独立服务**：一个 FastAPI 生命周期服务器管理所有沙箱的创建/销毁/状态，所有 AI Agent 框架（HermesAgent、OpenCode、LangGraph 等）通过 SDK 或 HTTP API 接入。OpenSandbox 仓库的 `examples/` 目录下有针对 OpenClaw、LangGraph、OpenCode、Google ADK、Gemini CLI 等框架的集成示例。

### 6.2 整体架构

OpenSandbox 由四个核心组件构成：

```
┌─────────────────────────────────────────────────────────────────┐
│                    AI Agent / Harness 框架                       │
│   （通过 Python/Go/JS/Java/C# SDK 或直接 HTTP 调用）              │
└──────────────────────────┬──────────────────────────────────────┘
                           │ REST API / SDK
           ┌───────────────▼────────────────┐
           │     Lifecycle Server           │
           │   (FastAPI / Python)           │
           │  sandbox_service.py            │
           │  snapshot_service.py           │
           │  validators.py                 │
           │  SQLite: ~/.opensandbox/db     │
           └──────┬─────────────┬───────────┘
                  │             │
      ┌───────────▼──┐   ┌──────▼──────────────┐
      │ Docker 运行时  │   │ Kubernetes 运行时    │
      │ docker_service│   │ kubernetes_service   │
      │               │   │ agent_sandbox CRD    │
      └───────┬───────┘   │ batchsandbox CRD     │
              │           │ Pool CRD             │
              │           └──────────────────────┘
              │ 容器内
   ┌──────────▼────────────────────────────────────────┐
   │  Sandbox 容器                                      │
   │  ┌─────────────────┐   ┌──────────────────────┐   │
   │  │  execd（Go）     │   │  Egress Sidecar（Go）  │   │
   │  │  HTTP API + SSE  │   │  DNS Proxy（:15353）  │   │
   │  │  /code           │   │  nftables 策略        │   │
   │  │  /session        │   │  FQDN allowlist       │   │
   │  │  /command        │   │  mitmproxy（实验性）   │   │
   │  │  /files/*        │   │                      │   │
   │  │  /pty (WS)       │   └──────────────────────┘   │
   │  └─────────────────┘                               │
   └───────────────────────────────────────────────────┘
```

**组件职责**：

| 组件 | 语言 | 职责 |
|------|------|------|
| Lifecycle Server | Python | 沙箱 CRUD / 快照 / 端点管理 / 签名访问 |
| execd | Go | 容器内命令执行守护进程（HTTP + SSE + WebSocket PTY）|
| Egress Sidecar | Go | FQDN 级出站流量过滤（DNS Proxy + nftables）|
| Ingress Gateway | 可配置 | 对外路由（直连 / 共享 Gateway）|

### 6.3 沙箱实现方式

#### 6.3.1 运行时层次（四档隔离强度）

OpenSandbox 支持四种运行时，安全强度递增：

| 运行时 | 技术 | 隔离强度 | 适用场景 |
|--------|------|---------|---------|
| 标准 Docker | runc | 进程 + namespace | 开发/低风险 |
| gVisor | runsc | 用户态内核（syscall 拦截）| 不受信任代码 |
| Kata Containers | kata-runtime / kata-qemu | 轻量 VM（独立 kernel）| 高安全生产 |
| Firecracker | kata-fc（仅 K8s）| microVM | 最强隔离（仅 K8s）|

配置方式（来自 `config.py`）：

```toml
[docker]
secure_runtime.type = "gvisor"  # "" / "gvisor" / "kata" / "firecracker"
docker_runtime = "runsc"         # 对应 Docker runtime 名称

[kubernetes]
k8s_runtime_class = "gvisor"    # 对应 K8s RuntimeClass
```

服务器启动时做 **fail-fast 验证**：
- Docker：`docker info()` 检查 OCI runtime 是否存在
- K8s：API 查询 RuntimeClass 是否存在，不存在则拒绝启动

#### 6.3.2 Docker 安全配置

来自 `config.py` 和 `docker_service.py` 的实测默认值：

**默认丢弃的 Capabilities**（保守集合，来自源码）：

```python
drop_capabilities = [
    "AUDIT_WRITE", "MKNOD", "NET_ADMIN", "NET_RAW",
    "SYS_ADMIN", "SYS_MODULE", "SYS_PTRACE",
    "SYS_TIME", "SYS_TTY_CONFIG"
]
```

**其他安全参数默认值**：

| 参数 | 默认值 | 说明 |
|------|------|------|
| `no_new_privileges` | `true` | 防特权提升 |
| `pids_limit` | `4096` | 防 fork bomb（可配，null 禁用）|
| `seccomp_profile` | `None`（Docker 默认 seccomp）| 可指定自定义 profile |
| `apparmor_profile` | `None`（Docker 默认）| 可指定 `docker-default` |
| `network_mode` | `"host"` | 注意：默认 host 模式，需改为 bridge 才能启用 egress |

**Host bind mounts**：默认 allowlist 为空（secure-by-default），必须显式配置才允许挂载宿主路径。

**Egress sidecar 的 NET_ADMIN 隔离**：

主容器 egress 启用时，从 cap_drop 中**移除 NET_ADMIN**（sidecar 才需要），主容器运行 unprivileged：

```python
# docker_service.py 中
if network_policy:
    cap_drop.add("NET_ADMIN")  # 主容器不得操作网络
```

#### 6.3.3 Kubernetes 安全配置

来自 `k8s/security_context.py`：

Kubernetes security context 支持 capabilities（add/drop）和 privileged 标志，**seccomp/AppArmor/runAsUser 需使用者在 K8s 层自行配置**（当前 SDK 未封装）。

Egress sidecar 注入逻辑（`egress_helper.py`）：

```python
# 主容器：drop NET_ADMIN
# Egress sidecar：需要 NET_ADMIN + EGRESS_MODE=dns/dns+nft
# IPv6 禁用：privileged init container 写入 /proc/sys
```

### 6.4 生命周期状态机

七个明确定义的沙箱状态：

```
         创建请求
            ↓
        [Pending]    ← 等待容器/Pod 就绪（IP 分配）
            ↓ 就绪（execd /ping 成功）
        [Running]    ← 正常执行状态
         ↙     ↘
   [Pausing]  [Stopping]
      ↓           ↓
   [Paused]  [Terminated]
      ↓
   [Running]  (Resume)
   
   任何状态 → [Failed]（异常）
```

快照状态机（独立）：

```
Creating → Ready（成功）
Creating → Failed（失败）
Ready → Deleting → (removed)
```

**自动过期**：Lifecycle Server 为每个沙箱注册 daemon timer，TTL 到期自动调用删除。服务重启时 `_restore_existing_sandboxes()` 重建所有 timer，对已过期的容器立即处理。

**Pending 失败追踪**：provisioning 失败的沙箱进入 Pending 状态保留 3600 秒，让客户端可以轮询获取失败原因，无需持久化存储。

### 6.5 Agent 与 Sandbox 通信：execd 守护进程

execd 是 OpenSandbox 架构中的核心创新：一个 **Go 编写的 HTTP 守护进程运行在容器内**，暴露结构化 API 替代裸 `docker exec`。

**通信协议**：

```
Lifecycle Server → 宿主机端口（execd embedded port 44772）
                → HTTP REST（命令/文件操作）
                → SSE（流式输出）
                → WebSocket（PTY 终端）
```

**认证**：`X-EXECD-ACCESS-TOKEN` Header（共享 API 访问令牌）。

**核心 API 端点**：

| 端点 | 协议 | 功能 |
|------|------|------|
| `POST /code` | HTTP + SSE | 执行代码（含 Jupyter 委托），流式返回 stdout/stderr/error |
| `POST /session` | HTTP | 创建持久 Bash session（UUID 标识）|
| `POST /session/{id}/command` | HTTP + SSE | 在 session 内执行命令（继承 env + cwd）|
| `DELETE /session/{id}` | HTTP | 关闭 session（SIGKILL 进程组）|
| `GET/POST /command` | HTTP + SSE | 无状态命令执行 |
| `GET/POST /files/*` | HTTP | 文件上传/下载/搜索/删除 |
| `GET /directories/*` | HTTP | 目录操作 |
| `WS /pty` | WebSocket | 全功能 PTY 终端 |
| `GET /metrics` | HTTP + SSE | 实时系统指标 watch |

**Bash Session 的状态持久化**（`bash_session.go`）：

与 HermesAgent 的 Spawn-Per-Call 模型类似，但在同一容器内通过 session UUID 维持状态：

- 环境变量通过 `export -p` 捕获，上限 8KB/变量
- CWD 通过 `$(pwd)` 持久化到 session 对象
- session 关闭时 `syscall.Kill(-pid, SIGKILL)` 清理整个进程组

**进程组隔离**（`command.go`）：

```go
// 所有命令运行在独立进程组（Setpgid: true）
// 超时/取消 → SIGKILL 整个进程组，防孤儿进程
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
// 用户隔离：从请求参数设置 UID/GID + 补充组
syscall.Credential{Uid: uid, Gid: gid, Groups: supplementalGroups}
```

### 6.6 网络隔离：Egress Sidecar

Egress Sidecar 是 OpenSandbox 相较于裸 Docker 最显著的差异化能力。

#### 6.6.1 工作原理

Egress Sidecar 作为独立容器与主容器**共享网络 namespace**（`--network container:<main_id>`）：

```
Sandbox 容器
  ├── 主应用容器（无 NET_ADMIN）
  │     └── 所有出站流量
  └── Egress Sidecar（有 NET_ADMIN）
        ├── DNS Proxy（:15353）← iptables 重定向端口 53
        │     └── 查询 FQDN allowlist → NXDOMAIN / forward
        └── nftables（dns+nft 模式）
              └── 已解析 IP 加入动态 allow set（TTL 管理）
              └── 默认 deny-all（剩余 IP 全 drop）
```

#### 6.6.2 两种执行模式

| 模式 | 机制 | 适用 |
|------|------|------|
| `dns` | 仅 DNS 过滤，IP 层不拦截 | 非 root 镜像（无 NET_ADMIN）|
| `dns+nft` | DNS 过滤 + nftables IP 层双重拦截 | 强安全要求 |

**安全原则**：`CAP_NET_ADMIN` 仅授予 Egress Sidecar，主容器 unprivileged。

#### 6.6.3 策略动态更新

Egress policy 可通过 HTTP API 运行时修改，无需重启容器：

```json
POST /policy
{
  "default": "deny",
  "egress": [
    {"action": "allow", "target": "*.pypi.org"},
    {"action": "allow", "target": "github.com"}
  ]
}
```

支持操作：PUT（全量替换）/ PATCH（追加规则）/ DELETE（移除）。策略文件可持久化到磁盘，后台每分钟重载 always-rules。

#### 6.6.4 实际集成示例（openclaw/main.py）

```python
sandbox = SandboxSync.create(
    image="openclaw/openclaw:latest",
    timeout=timedelta(minutes=30),
    entrypoint=["node", "openclaw.mjs", "gateway"],
    network_policy=NetworkPolicy(
        default=NetworkPolicyAction.DENY,  # 默认拒绝所有出站
        egress=[
            NetworkRule(action="allow", target="*.pypi.org"),
            NetworkRule(action="allow", target="github.com"),
        ]
    ),
    connection=ConnectionConfig(server_url=OPENSANDBOX_URL, api_key=API_KEY),
    health_check=wait_for_openclaw
)
```

### 6.7 快照与池化

#### 6.7.1 快照机制（snapshot_service.py）

- 从 Running sandbox 异步创建快照（API 立即返回 202，后台 ThreadPoolExecutor 执行）
- 成功后存储 `restore_config.image` 引用，用于后续 sandbox 从快照初始化（warm-start）
- 服务重启时 `recover_unfinished_snapshots()` 检查并续传未完成的快照

**对池化的意义**：可以预先创建"黄金镜像"快照，从快照而非空白镜像实例化，大幅降低冷启动时间。

#### 6.7.2 Kubernetes 池化（pool_service.py）

Kubernetes 运行时支持 Pool CRD（Docker 运行时**不支持**，来自源码注释 `"poolRef is not supported by the Docker provider"`）：

```python
# Pool 配置
pool = {
    "template": {...},  # sandbox 模板
    "capacity": {
        "bufferMin": 2,    # 最低预热实例数
        "bufferMax": 5,    # 最高预热实例数
        "poolMin": 2,      # 池最小总量
        "poolMax": 20,     # 池最大总量
    }
}
```

池状态跟踪：total / allocated / available 实例数，通过 status metrics 实时更新。

### 6.8 多语言 SDK 生态

OpenSandbox 提供五语言 SDK，Go SDK 是本项目最相关的：

**Go SDK 核心接口**（`sdks/sandbox/go/sandbox.go`）：

```go
// 创建沙箱（含健康检查等待）
sandbox, err := opensandbox.CreateSandbox(ctx, connConfig, opensandbox.SandboxCreateOptions{
    Image:          "ubuntu:22.04",
    ResourceLimits: map[string]string{"cpu": "500m", "memory": "512Mi"},
    Timeout:        timedelta(30 * time.Minute),
    NetworkPolicy:  &NetworkPolicy{Default: "deny", Egress: []NetworkRule{...}},
})

// 执行命令（SSE 流式）
result, err := sandbox.Execd().RunCommand(ctx, "echo hello world")

// 持久 Bash session（保持 env/cwd 状态）
session, err := sandbox.Execd().CreateSession(ctx, "/workspace")
result, err := session.Run(ctx, "cd /app && go build ./...")

// 快照
snapshot, err := sandbox.CreateSnapshot(ctx, "my-snapshot")

// 生命周期
sandbox.Pause(ctx)
sandbox.Resume(ctx)
sandbox.Kill(ctx)
```

**生命周期状态**（`types.go`）：

```go
const (
    SandboxStatePending    = "Pending"
    SandboxStateRunning    = "Running"
    SandboxStatePausing    = "Pausing"
    SandboxStatePaused     = "Paused"
    SandboxStateStopping   = "Stopping"
    SandboxStateTerminated = "Terminated"
    SandboxStateFailed     = "Failed"
)
```

### 6.9 MCP 集成

OpenSandbox 通过 `sdks/mcp/` 将沙箱能力暴露为 MCP 工具，兼容 Claude Code / Cursor 等 MCP 客户端：

- 沙箱创建
- 命令执行
- 文本文件操作

这使得 AI Agent 可通过 MCP 协议将 OpenSandbox 当作标准工具调用，无需直接集成 SDK。

### 6.10 版本历程与成熟度

来自 `server/RELEASE_NOTES.md` 的关键里程碑：

| 版本 | 关键特性 |
|------|---------|
| v0.1.0 | 初始发布：FastAPI 生命周期服务 |
| v0.1.1 | 内置 Proxy + Egress 独立配置 |
| v0.1.2 | Docker 本地 volume + K8s NetworkPolicy |
| v0.1.3 | 多 Ingress 模式（header 路由）|
| v0.1.6 | 安全容器端到端测试 + 文档 |
| v0.1.7 | PVC + 用户自定义 Docker 网络 + RBAC |
| v0.1.8 | OSSFS + 每沙箱 egress 认证 token |
| v0.1.9（当前）| WebSocket 代理 + 池 CRUD API（实验性）|

项目于 2025-12-17 创建，不足 6 个月即获得 11K+ Stars，进入 CNCF Landscape，活跃度高。**API 尚未进入 stable v1**，ROADMAP 明确指出"不会在语义稳定前宣布 v1"。

---

## 7. OpenSandbox vs Docker：选型评估

### 7.1 能力对比矩阵

| 维度 | 裸 Docker（参考 HermesAgent）| OpenSandbox | 优势方 |
|------|---------------------------|-------------|--------|
| **隔离强度（默认）** | runc（进程 namespace）| runc（进程 namespace）| 持平 |
| **隔离强度（最强）** | runc（用户手动配置 gVisor）| gVisor / Kata / Firecracker（配置化）| OpenSandbox |
| **网络过滤粒度** | `--network none`（全禁）| FQDN allowlist（可选特定域名放行）| OpenSandbox 更灵活 |
| **网络控制方式** | 启动时静态配置 | 运行时动态 API 更新 | OpenSandbox |
| **Agent 通信方式** | `docker exec` + stdio（每次一个进程）| execd HTTP REST + SSE（持久 session）| OpenSandbox |
| **PTY 终端支持** | `docker exec -it`（需要 TTY attach）| WebSocket PTY（远程友好）| OpenSandbox |
| **Jupyter/代码解释器** | 需自行在镜像集成 | execd 内置 Jupyter 委托 | OpenSandbox |
| **快照 / warm-start** | 手动 `docker commit`（繁琐）| API 调用，异步快照（集成）| OpenSandbox |
| **资源配额** | `--cpus / --memory / --pids-limit` | 同等（映射到 cgroups）+ GPU | 持平 |
| **并发沙箱管理** | 用户自行追踪容器 ID | Lifecycle Server 统一管理 + UUID | OpenSandbox |
| **池化（预热）** | 无内置（需自己实现）| K8s Pool CRD（bufferMin/bufferMax）| OpenSandbox（仅 K8s）|
| **孤儿容器回收** | 用户实现（如 HermesAgent 的 label 扫描）| 自动过期 timer + 服务重启恢复 | OpenSandbox |
| **多语言 SDK** | 无（用 Docker SDK 各语言自行封装）| Python / Go / JS / Java / C# | OpenSandbox |
| **安全审计** | 无内置（用户自行监控）| execd metrics + diagnostic API | OpenSandbox |
| **MCP 支持** | 无 | 有（sdks/mcp/）| OpenSandbox |
| **运维复杂度** | 低（一个 docker 命令）| 高（需运行 Lifecycle Server + execd）| 裸 Docker |
| **依赖项** | Docker Daemon | Docker/K8s + Lifecycle Server + execd | 裸 Docker |
| **API 稳定性** | 成熟稳定 | 尚未 v1，breaking changes 可能发生 | 裸 Docker |
| **生态成熟度** | 极成熟（10+ 年）| 新兴（< 6 个月）| 裸 Docker |
| **本地开发友好** | 极高（docker run 即可）| 中（需启动 Lifecycle Server）| 裸 Docker |
| **Kubernetes 原生** | 需 K8s 集成额外工作 | K8s 是一等公民（agent-sandbox CRD）| OpenSandbox |

### 7.2 核心差异深度分析

#### 7.2.1 通信模型：execd vs docker exec

这是两种方案最本质的架构差异。

**裸 Docker（docker exec 模型）**：

```
Agent → docker exec -i <container> bash -c <cmd>
  每次命令 = 一个新进程（冷启动 shell）
  环境状态：无持久化，靠 session snapshot 文件补偿
  延迟：每次 ~30-100ms（docker exec 开销）
  输出：stdio pipe，不支持真实 SSE / PTY
```

**OpenSandbox（execd 模型）**：

```
Agent → HTTP POST /session/{id}/command
  持久 Bash session（UUID 标识，维持 env + cwd 跨调用）
  通信：HTTP + SSE 原生流式，WebSocket PTY
  延迟：HTTP 请求延迟（同宿主 <1ms，跨网络 ~5-20ms）
  会话语义：天然支持有状态多步执行
```

对 harness9 的含义：execd 的 **Persistent Session** 语义与 HermesAgent 的 session snapshot 方法在目标上一致（保持 CWD/env 跨命令），但实现路径不同——OpenSandbox 在容器内内置了这个机制，而 HermesAgent 通过包装 bash 脚本在外部实现。

#### 7.2.2 网络控制粒度

| 方案 | 网络控制 | 适用场景 |
|------|---------|---------|
| 裸 Docker `--network none` | 完全禁用网络（fail-closed）| 不需要任何网络访问的纯计算任务 |
| 裸 Docker 自定义网络 + iptables | 需用户手工配置，无动态更新 | 固定网络需求 |
| OpenSandbox Egress（dns 模式）| FQDN allowlist，软过滤（DNS 层）| 需要访问特定 API/包源 |
| OpenSandbox Egress（dns+nft 模式）| FQDN + IP 双层，强过滤 | 高安全要求 + 部分网络访问 |

**关键洞察**：如果 Agent 场景需要访问 PyPI、npm、GitHub API 等特定资源，`--network none` 会完全阻断，而 OpenSandbox 的 FQDN allowlist 可以精确放行这些域名同时屏蔽其他流量。这是 OpenSandbox 比裸 Docker 更灵活的核心能力。

#### 7.2.3 隔离强度选择

**裸 Docker + 手工配置 gVisor**：

```bash
docker run --runtime=runsc ...  # 需要用户自行安装配置
```

**OpenSandbox + gVisor**：

```toml
[docker]
secure_runtime.type = "gvisor"
```

配置化显著降低门槛，且有 fail-fast 启动验证保证不会静默回退到 runc。

**Kata Containers / Firecracker**：裸 Docker 同样支持，但 OpenSandbox 提供统一 API 屏蔽配置差异，Firecracker 仅在 K8s 模式下可用。

#### 7.2.4 Cap-drop 对比

| 方案 | 丢弃的 Capabilities |
|------|---------------------|
| HermesAgent DockerEnvironment | ALL（全部丢弃，仅加回 3 个）|
| OpenSandbox 默认 | 9 个（保守集合）|
| OpenSandbox（用户配置 drop all）| 同 HermesAgent |

**发现**：HermesAgent 的 `--cap-drop all` 策略比 OpenSandbox 的默认值更激进。OpenSandbox 默认丢弃 9 个关键 capabilities，但保留了其他如 `NET_BIND_SERVICE`、`SETUID` 等。对于高安全场景，建议在 OpenSandbox 配置中显式 drop all。

### 7.3 适用场景分析

#### OpenSandbox 显著优于裸 Docker 的场景

**场景 1：多 Agent 并发 + 大规模部署**

需要管理数十至数百个并发沙箱时，Lifecycle Server 的统一 CRUD + 自动过期 + K8s Pool 预热是核心价值。裸 Docker 需要自行实现调度、状态追踪、孤儿清理，工程量大。

**场景 2：Agent 需要受控的网络访问**

Agent 需要 pip install、调用第三方 API，但又不能放开全部网络时，Egress FQDN allowlist 是裸 Docker 无法轻易替代的。

**场景 3：生产级 Kubernetes 部署**

agent-sandbox CRD + batchsandbox + Pool 是 K8s 原生的一等公民设计。裸 Docker 在 K8s 上需要额外抽象层。

**场景 4：需要 PTY / Jupyter 支持**

execd 的 WebSocket PTY 和 Jupyter 委托适合需要交互式终端或代码解释器的 Agent。

**场景 5：多框架统一沙箱平台**

如果项目同时支持多个 Agent 框架（如 harness9 + 其他框架），OpenSandbox 作为独立服务避免了在每个框架内重复实现容器管理逻辑。

#### 裸 Docker 优于 OpenSandbox 的场景

**场景 1：轻量级本地开发 / 单机部署**

启动一个 FastAPI server + execd + Egress sidecar 的成本高于直接 `docker run`。对于 harness9 这类本地 CLI 工具，额外的服务依赖是明显的负担。

**场景 2：高度定制化安全配置**

需要精细控制 seccomp profile、AppArmor、SELinux 等时，裸 Docker 配置更直接。OpenSandbox 当前对 K8s security context 的封装尚不完整（seccomp/runAsUser 未内置），需在 K8s 层补充。

**场景 3：极简依赖原则**

harness9 当前的设计哲学是"极少的直接依赖数"。OpenSandbox 引入额外服务依赖（Python FastAPI server + Go binary）与此理念冲突。

**场景 4：API 稳定性要求高**

OpenSandbox 当前是 v0.1.x，明确表示 API 尚未稳定。生产关键路径依赖不稳定 API 有维护风险。

### 7.4 选型结论

对于 **harness9** 当前阶段：

**结论：不推荐立即引入 OpenSandbox，建议保持"裸 Docker 后端 + harness9 自实现管理层"路线，长期保留 OpenSandbox 作为可接入的外部沙箱提供商。**

理由如下：

| 维度 | 分析 |
|------|------|
| **定位匹配** | harness9 是本地 CLI 工具，OpenSandbox 更适合云端多租户场景。裸 Docker 足以满足单机 Agent 的硬隔离需求 |
| **依赖原则** | 引入 Lifecycle Server 服务依赖违背 harness9 的极简设计哲学 |
| **API 稳定性** | OpenSandbox v0.1.x 尚未稳定，harness9 有版本锁定风险 |
| **安全能力** | harness9 需要的核心安全能力（capability 丢弃、network=none、pids-limit、tmpfs）裸 Docker 完全支持，参考 HermesAgent 实现即可 |
| **网络访问** | harness9 的 Agent 通常不需要网络访问（`--network none` 就够），无需 OpenSandbox 的 FQDN allowlist |
| **可扩展路径** | 若 harness9 未来演进为云端多用户服务，届时接入 OpenSandbox 的 K8s 池化是合理的扩展路径 |

**如何接入 OpenSandbox（未来可选）**：

参考 [第 5.2 节建议 7](#建议-7可插拔-sandbox-后端)，在 harness9 设计可插拔 `Sandbox` 接口后，OpenSandbox 可以作为一个独立的后端实现：

```go
// 未来可扩展
// sandbox/opensandbox.go
type OpenSandboxBackend struct {
    client *opensandbox.Client  // Go SDK
    id     string
}

func (b *OpenSandboxBackend) Execute(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
    session, err := b.client.Sandbox(b.id).Execd().CreateSession(ctx, opts.WorkDir)
    // ...
}
```

### 7.5 四方案综合对比（为 harness9 定制）

| 方案 | 隔离强度 | 运维复杂度 | 本地友好 | 适配 harness9 | 推荐度 |
|------|---------|-----------|---------|--------------|--------|
| 进程级（当前）| 低 | 极低 | 极高 | 完全适配 | 现阶段默认 |
| 裸 Docker（HermesAgent 方案）| 高 | 低 | 高 | 适配 | P3 推荐 |
| OpenSandbox + Docker | 高 | 中 | 中 | 引入服务依赖 | 暂不推荐 |
| OpenSandbox + K8s + Firecracker | 极高 | 高 | 低 | 定位不符 | 长期可选 |

---

## 8. 参考资料

以下参考资料均经 WebFetch 实时验证可访问，内容与调研主题直接相关：

### 主报告参考资料

| 标题 | 来源 | URL | 摘要 |
|------|------|-----|------|
| DeepAgents THREAT_MODEL.md | LangChain GitHub | https://raw.githubusercontent.com/langchain-ai/deepagents/main/libs/deepagents/THREAT_MODEL.md | 完整威胁模型，描述 TB3/TB5 信任边界，LocalShellBackend T3 威胁，BaseSandbox 设计原则 |
| DeepAgents backends/sandbox.py | LangChain GitHub | https://github.com/langchain-ai/deepagents/blob/main/libs/deepagents/deepagents/backends/sandbox.py | BaseSandbox 抽象基类，MAX_BINARY_BYTES/MAX_OUTPUT_BYTES 常量，文件操作策略 |
| OpenHarness sandbox/ | HKUDS GitHub | https://github.com/HKUDS/OpenHarness/tree/main/src/openharness/sandbox | DockerSandboxSession、path_validator、adapter 完整沙箱模块 |
| OpenCode tool/shell.ts | Anomaly GitHub | https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/tool/shell.ts | Effect 框架封装的 Shell 工具，AST 权限分析，双超时机制 |
| OpenCode snapshot/index.ts | Anomaly GitHub | https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/snapshot/index.ts | Git Worktree 快照机制，并发信号量保护，GC 策略 |
| OpenClaw security/audit.ts | OpenClaw GitHub | https://github.com/openclaw/openclaw/blob/main/src/security/audit.ts | 多维度安全审计：执行安全、频道暴露、权限模式、插件信任 |
| OpenClaw process/exec.ts | OpenClaw GitHub | https://github.com/openclaw/openclaw/blob/main/src/process/exec.ts | 双超时机制、shell=false 防注入、16MB 输出上限、Windows taskkill |
| HermesAgent environments/base.py | NousResearch GitHub | https://github.com/NousResearch/hermes-agent/blob/main/tools/environments/base.py | BaseEnvironment 抽象接口，ProcessHandle 协议，Spawn-Per-Call 模型 |
| HermesAgent environments/docker.py | NousResearch GitHub | https://github.com/NousResearch/hermes-agent/blob/main/tools/environments/docker.py | Docker 安全加固（cap-drop/no-new-privileges/pids-limit）、孤儿容器回收、跨进程复用 |
| HermesAgent environments/modal.py | NousResearch GitHub | https://github.com/NousResearch/hermes-agent/blob/main/tools/environments/modal.py | Modal sandbox 创建、快照持久化、_AsyncWorker 线程安全 |
| HermesAgent agent/file_safety.py | NousResearch GitHub | https://github.com/NousResearch/hermes-agent/blob/main/agent/file_safety.py | 多层文件保护：exact path + 目录前缀 + 跨 profile 守卫 |
| HermesAgent agent/tool_guardrails.py | NousResearch GitHub | https://github.com/NousResearch/hermes-agent/blob/main/agent/tool_guardrails.py | 行为守卫：循环检测、失败追踪、幂等工具分类 |
| Claude Agent SDK - Overview | Anthropic 官方文档 | https://code.claude.com/docs/en/agent-sdk/overview | SDK vs Managed Agents 对比，内置工具列表，Subagent 架构 |
| Claude Agent SDK - Permissions | Anthropic 官方文档 | https://code.claude.com/docs/en/agent-sdk/permissions | 权限评估链路，六种权限模式详解，allowed_tools 与 bypassPermissions 交互 |
| Claude Agent SDK - Hooks | Anthropic 官方文档 | https://code.claude.com/docs/en/agent-sdk/hooks | 19 种 Hook 事件，路径重定向模式（updatedInput），并行 Hook 优先级规则 |

### OpenSandbox 专项参考资料

| 标题 | 来源 | URL | 摘要 |
|------|------|-----|------|
| OpenSandbox 仓库 | opensandbox-group GitHub | https://github.com/opensandbox-group/OpenSandbox | 主仓库：Apache 2.0，Python server + Go execd/egress，CNCF Landscape 已收录 |
| OpenSandbox 配置文档 | GitHub - server/configuration.md | https://raw.githubusercontent.com/opensandbox-group/OpenSandbox/main/server/configuration.md | 完整配置参数：drop_capabilities 默认列表、pids_limit、seccomp/apparmor、egress 模式 |
| OpenSandbox execd README | GitHub - components/execd | https://raw.githubusercontent.com/opensandbox-group/OpenSandbox/main/components/execd/README.md | execd HTTP API 架构：SSE 流式、WebSocket PTY、Bash session、认证机制 |
| OpenSandbox Egress README | GitHub - components/egress | https://raw.githubusercontent.com/opensandbox-group/OpenSandbox/main/components/egress/README.md | Egress Sidecar：FQDN allowlist、DNS Proxy、nftables、动态策略更新、CAP_NET_ADMIN 隔离 |
| OpenSandbox Go SDK | GitHub - sdks/sandbox/go | https://github.com/opensandbox-group/OpenSandbox/tree/main/sdks/sandbox/go | Go SDK 完整接口：CreateSandbox、Bash Session、快照、生命周期状态机 |
| OpenSandbox OpenClaw 集成示例 | GitHub - examples/openclaw | https://raw.githubusercontent.com/opensandbox-group/OpenSandbox/main/examples/openclaw/main.py | 端到端集成示例：deny-by-default 网络策略 + FQDN allowlist + 健康检查 |
| OpenSandbox Roadmap | GitHub - ROADMAP.md | https://raw.githubusercontent.com/opensandbox-group/OpenSandbox/main/ROADMAP.md | 2026 H1-H2 规划：pause/resume 快照、本地轻量沙箱、OpenTelemetry、API 稳定性策略 |
| OpenSandbox Release Notes | GitHub - server/RELEASE_NOTES.md | https://raw.githubusercontent.com/opensandbox-group/OpenSandbox/main/server/RELEASE_NOTES.md | v0.1.0 至 v0.1.9 特性历程：Egress、Pool CRUD、OSSFS、RBAC、WebSocket 代理 |

---

*本报告基于 2026-06-05 实时获取的源码与文档撰写，所有引用均经 WebFetch 确认可访问。框架代码随版本迭代可能有变化，建议结合最新源码核实关键实现细节。*
