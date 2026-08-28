# Sub-Agent 设计调研报告

> 调研时间：2026-05-30
> 调研范围：DeepAgents / OpenHarness / OpenCode / OpenClaw / HermesAgent / Claude Agent SDK
> 调研重点：子代理/多代理功能的设计思想与实现原理

---

## 目录

1. [调研背景](#1-调研背景)
2. [各框架逐项分析](#2-各框架逐项分析)
   - 2.1 Claude Agent SDK（Anthropic）
   - 2.2 OpenHarness（HKUDS）
   - 2.3 HermesAgent（NousResearch）
   - 2.4 OpenCode（Anomaly）
   - 2.5 DeepAgents（LangChain）
   - 2.6 OpenClaw
3. [横向对比](#3-横向对比)
4. [共性设计模式与差异化取舍](#4-共性设计模式与差异化取舍)
5. [对 harness9 的设计建议](#5-对-harness9-的设计建议)
6. [权威参考资料](#6-权威参考资料)

---

## 1. 调研背景

Sub-Agent（子代理）是 Agent Harness 框架中最具技术深度的模块之一。它解决的核心问题是：**当单个 Agent 的 context window 不够用、任务边界不清晰、工具权限需要隔离时，如何优雅地把工作委派给专门的子代理？**

本报告基于对六个主流框架的 GitHub 源码、官方文档、官方 API 文档（context7）的一手调研，聚焦以下六个维度：
1. Sub-Agent 的创建方式
2. Sub-Agent 的调度与生命周期管理
3. Agent 间通信、Handoff、协作
4. Context 的注入与传递机制
5. Sub-Agent 的权限管理
6. Tools 与 Skills 管理

---

## 2. 各框架逐项分析

### 2.1 Claude Agent SDK (Anthropic)

**基础信息**：GitHub 星数未单独统计（内嵌于 Claude Code），Python + TypeScript 双 SDK，官方文档站 code.claude.com。

#### 2.1.1 Sub-Agent 的创建方式

Claude Agent SDK 支持**三种创建方式**，设计极为完整：

**方式一：编程式声明（Programmatic，推荐）**

通过 `query()` 的 `agents` 参数传入 `AgentDefinition` 字典，在代码中声明子代理：

```python
from claude_agent_sdk import query, ClaudeAgentOptions, AgentDefinition

async for message in query(
    prompt="Review the authentication module for security issues",
    options=ClaudeAgentOptions(
        allowed_tools=["Read", "Grep", "Glob", "Agent"],  # Agent tool enables subagent invocation
        agents={
            "code-reviewer": AgentDefinition(
                description="Expert code review specialist. Use for quality, security reviews.",
                prompt="You are a code review specialist...",
                tools=["Read", "Grep", "Glob"],   # read-only restriction
                model="sonnet"
            ),
            "test-runner": AgentDefinition(
                description="Runs and analyzes test suites.",
                prompt="You are a test execution specialist...",
                tools=["Bash", "Read", "Grep"]    # bash access for test execution
            )
        }
    )
):
    ...
```

`AgentDefinition` 支持的全部字段：

| 字段 | 必填 | 说明 |
|------|------|------|
| `description` | 是 | 告知主代理何时使用此子代理（自然语言） |
| `prompt` | 是 | 子代理的 system prompt |
| `tools` | 否 | 允许的工具列表（省略则继承全部） |
| `disallowedTools` | 否 | 显式排除的工具 |
| `model` | 否 | 模型别名（sonnet/opus/haiku/inherit）或完整 model ID |
| `skills` | 否 | 预加载 Skills 名称列表 |
| `memory` | 否 | 持久记忆范围（user/project/local） |
| `mcpServers` | 否 | 子代理专属 MCP 服务器 |
| `maxTurns` | 否 | 最大轮数 |
| `background` | 否 | 是否异步后台运行 |
| `effort` | 否 | 推理强度（low/medium/high/xhigh/max） |
| `permissionMode` | 否 | 权限模式（default/acceptEdits/auto/dontAsk/bypassPermissions/plan） |
| `isolation` | 否 | 设为 `worktree` 可在独立 git worktree 中运行 |

**方式二：文件系统声明（Filesystem-based）**

将子代理定义为 `.claude/agents/` 目录下的 Markdown 文件，YAML frontmatter 作为配置：

```markdown
---
name: code-reviewer
description: Reviews code for quality and best practices. Use proactively after code changes.
tools: Read, Glob, Grep
model: sonnet
permissionMode: acceptEdits
memory: user
skills:
  - api-conventions
  - error-handling-patterns
---

You are a code review specialist with expertise in security, performance, and best practices.

When reviewing code:
- Identify security vulnerabilities
- Check for performance issues
- Suggest specific improvements
```

存储位置决定作用域：

| 位置 | 作用域 | 优先级 |
|------|--------|--------|
| 托管设置 | 组织全局 | 最高（1） |
| `--agents` CLI 参数 | 当前会话 | 2 |
| `.claude/agents/` | 当前项目 | 3 |
| `~/.claude/agents/` | 所有项目 | 4 |
| 插件 `agents/` 目录 | 插件生效范围 | 最低（5） |

**方式三：CLI 参数传入**

```bash
claude --agents '{
  "code-reviewer": {
    "description": "Expert code reviewer.",
    "prompt": "You are a senior code reviewer...",
    "tools": ["Read", "Grep", "Glob", "Bash"],
    "model": "sonnet"
  }
}'
```

#### 2.1.2 Sub-Agent 的调度与生命周期管理

**调度机制**：完全 LLM 驱动。主代理通过 `Agent` 工具（旧版叫 `Task`，v2.1.63 重命名）发起子代理调用。调度逻辑由 LLM 根据 `description` 字段语义匹配决定。

```
主代理 LLM → 决定使用哪个子代理 → 生成 tool_use: {name: "Agent", input: {subagent_type: "code-reviewer"}}
          ↓
子代理实例（独立 context window）
          ↓
返回最终消息作为 Agent tool result
```

**生命周期**：
- **创建**：每次 `Agent` tool 调用新建子代理实例，子代理有独立 context window
- **执行**：子代理在独立会话中运行，产生自己的工具调用链
- **终止**：子代理自然结束或达到 `maxTurns` 上限后返回结果
- **恢复**：通过 `SendMessage` 工具（需开启 agent teams）可恢复已停止的子代理

**并发支持**：支持后台（background）模式，多个子代理可并发运行。按 `Ctrl+B` 或在请求中注明"run in the background"可将子代理后台化。

**递归嵌套**：**明确禁止**。子代理不能再派生子代理（Agent 工具在子代理中不可用）。官方文档明确指出："Subagents cannot spawn their own subagents."

#### 2.1.3 Agent 间通信、Handoff、协作

**通信模型**：单向结果返回。子代理完成后，最终消息作为 `Agent` tool 的 result 返回给主代理。主代理 context 中只出现这条汇总结果，不包含子代理的中间工具调用过程。

**Handoff 模式**：不支持传统 Handoff（控制权永久转移）。调用结束后控制权始终回到主代理。

**协作拓扑**：
- **Supervisor 模式**：主代理作为编排者，调用多个专业子代理完成工作
- **链式（Pipeline）模式**：通过自然语言指令可要求先用 A 子代理，再用 B 子代理，串行处理
- **并行模式**：可请求多个子代理同时处理独立任务

**检测子代理调用**：通过 `parent_tool_use_id` 字段识别消息来自哪个子代理上下文：

```python
if hasattr(message, 'parent_tool_use_id') and message.parent_tool_use_id:
    print("  (running inside subagent)")
```

**Fork 模式**（实验性，v2.1.117+）：通过 `CLAUDE_CODE_FORK_SUBAGENT=1` 开启。Fork 是继承完整父对话历史的特殊子代理，与普通子代理（全新 context）的区别是：Fork 的第一个请求可复用父会话 prompt cache，成本更低。

#### 2.1.4 Context 的注入与传递机制

**隔离策略**：子代理拥有完全独立的 context window，不共享父对话历史。

**子代理启动时的上下文包含**：
- 子代理自身的 system prompt（来自 `AgentDefinition.prompt` 或文件 body）
- `Agent` tool 调用时传入的任务描述（task prompt）
- 项目 CLAUDE.md 文件（通过 `settingSources` 加载）
- 预加载的 Skills 内容（通过 `skills` 字段）
- Git 状态快照（非 Explore/Plan 子代理）

**子代理不包含**：
- 父对话历史记录
- 父代理的 system prompt
- 父代理已加载的 Skills 内容（除非 `skills` 字段列出）

**传递给子代理的唯一渠道**：是 `Agent` tool 调用时的 prompt 字符串。所以主代理在委托任务时需要将所有必要的文件路径、错误信息、上下文决策等直接写入这个 prompt。

**结果回注**：子代理最终消息逐字返回给主代理作为 `Agent` tool result。主代理可能会在自己的回复中对其进行摘要。

**上下文压缩**：子代理独立支持自动压缩（auto-compaction），默认触发阈值约 95%。子代理 transcript 存储于 `~/.claude/projects/{project}/{sessionId}/subagents/agent-{agentId}.jsonl`，独立于主会话，30 天后自动清理。

#### 2.1.5 Sub-Agent 的权限管理

**工具白名单/黑名单**：
- `tools` 字段：allowlist，只允许列出的工具
- `disallowedTools` 字段：denylist，从继承集合中排除
- 两者共存时：先应用 denylist，再应用 allowlist

**权限模式继承**：子代理继承父会话的权限上下文，可通过 `permissionMode` 字段覆盖（但父会话若为 `bypassPermissions` 或 `acceptEdits`，则此覆盖无效）。

**不可用工具**：无论配置如何，以下工具在子代理中始终不可用：`Agent`（禁止嵌套）、`AskUserQuestion`、`EnterPlanMode`、`ExitPlanMode`、`ScheduleWakeup`、`WaitForMcpServers`。

**Worktree 隔离**：设置 `isolation: worktree` 可为子代理分配独立 git worktree，文件修改在隔离副本中进行，不影响主 checkout。

**禁用特定子代理**：在 `settings.json` 的 `permissions.deny` 中添加 `Agent(subagent-name)` 可全局禁用某子代理类型。

**Human-in-the-Loop**：
- 前台子代理的权限提示透传给用户
- 后台子代理自动拒绝任何会弹出权限提示的操作（fail-closed）

#### 2.1.6 Tools 与 Skills 管理

**工具配置**：继承主代理工具集（默认），或通过 `tools`/`disallowedTools` 字段精确控制。

**Tools 的典型组合**：

| 使用场景 | 推荐工具 | 说明 |
|--------|--------|------|
| 只读分析 | Read, Grep, Glob | 不能修改或执行 |
| 测试执行 | Bash, Read, Grep | 可运行命令并分析输出 |
| 代码修改 | Read, Edit, Write, Grep, Glob | 无命令执行 |
| 全访问 | 省略 tools 字段 | 继承父代理全部工具 |

**Skills 预加载**：`skills` 字段指定在子代理启动时注入到 context 的 Skill 名称列表。未列出的 Skill 仍可通过 Skill 工具在运行时发现和调用。

**MCP 独占服务器**：`mcpServers` 字段允许子代理独享特定 MCP 服务器，不污染主代理 context（主代理不会看到这些 MCP 工具描述）。

**限制哪些子代理可被派生**：主代理可用 `Agent(worker, researcher)` 语法限制只能派生指定类型的子代理。

---

### 2.2 OpenHarness (HKUDS)

**基础信息**：Python，★13,310，内置"个人代理 Ohmo"，活跃开发中（最近推送 2026-05-27）。核心多代理模块在 `src/openharness/swarm/`。

#### 2.2.1 Sub-Agent 的创建方式

OpenHarness 使用**编程式 + 配置混合**方式，通过 `TeammateSpawnConfig` 数据类定义子代理（Teammate）：

```python
# TeammateSpawnConfig 核心字段（来自 swarm/types.py）
@dataclass
class TeammateSpawnConfig:
    name: str                    # 代理名称
    team: str                    # 所属团队
    prompt: str                  # 任务 prompt
    working_dir: str             # 工作目录
    session_id: str              # 会话 ID
    model: str | None            # 模型（可覆盖）
    system_prompt_mode: str      # 处理模式（default/replace/append）
    permissions: list[str]       # 工具权限列表
    plan_mode_required: bool     # 是否要求先进入计划模式
    allow_permission_prompts: bool  # 是否允许权限弹窗
    task_type: str               # local_agent / remote_agent / in_process_teammate
    event_subscriptions: list    # 事件订阅列表
```

子代理的身份标识为 `agent_id`，格式为 `agentName@teamName`。

#### 2.2.2 Sub-Agent 的调度与生命周期管理

**调度机制**：混合式。通过工具层暴露给主代理（`Agent` 工具），同时有 Swarm Coordinator 层进行程序化管理。

**多后端支持**：子代理可以在不同后端运行：
- `subprocess`：独立子进程（最常用）
- `in_process`：同进程内（测试/轻量场景）
- `tmux`：开启新 tmux pane（可视调试）
- `iterm2`：开启新 iTerm2 窗口

后端选择通过 `BackendRegistry` 自动检测：优先 in-process → tmux → subprocess。

**生命周期管理**（`TeamLifecycleManager`）：
- **创建**：`create_team()` 创建团队，`add_member()` 添加成员，元数据持久化到 `~/.openharness/teams/<name>/team.json`
- **执行**：`set_member_active()` 标记活跃状态，通过 `TeammateExecutor` 协议执行
- **终止**：`cleanup_session_teams()` 触发优雅关闭，先杀掉孤立 pane，再删除团队目录；`git worktree remove` 销毁 worktree
- **成员管理**：`remove_member()`/`remove_member_by_agent_id()` 移除单个成员

**并发支持**：✅ 原生支持。多个子代理可并行在各自后端运行。

**递归嵌套**：✅ 支持（通过 `team_allowed_paths` 协调）。

#### 2.2.3 Agent 间通信、Handoff、协作

**通信模型**：**基于文件系统的异步消息队列**（mailbox 模式）。每条消息存为 `~/.openharness/teams/<teamName>/` 下的独立 JSON 文件，采用 atomic write（先写 `.tmp` 再 rename）防止并发读写冲突：

```python
# TeammateMessage（来自 swarm/mailbox.py）
@dataclass
class TeammateMessage:
    text: str           # 消息内容
    sender_id: str      # 发送方代理 ID
    timestamp: float    # 时间戳
    summary: str        # 摘要
```

支持的消息类型：
- `user_message`：用户→代理消息
- `idle_notification`：代理空闲通知
- `permission_request`/`permission_response`：权限申请/批准
- `sandbox_permission_request`/`sandbox_permission_response`：沙箱权限
- `shutdown`：关闭指令

**工具暴露**：通过工具集暴露 Agent 间通信能力：
- `Agent`：生成子代理并通信
- `SendMessage`：直接向特定代理发消息
- `TeamCreate/Delete`：建立/解散代理团队
- `TaskCreate/Get/List/Update/Stop/Output`：全任务生命周期管理

**协作拓扑**：Swarm 模式（蜂群）。团队有 `lead_agent_id` 作为决策中心，其他成员并行工作。不是严格的 Supervisor 层级结构，更接近扁平协作。

#### 2.2.4 Context 的注入与传递机制

**CLI 继承**：`build_inherited_cli_flags()` 自动传播 40+ 环境变量（API 凭证、代理设置、CA 证书、OpenHarness 配置），通过 tmux 边界时尤为重要（tmux 可能启动新登录 shell，不继承父进程环境）。

**系统 Prompt 处理**：通过 `system_prompt_mode` 参数控制：
- `default`：使用子代理自身默认 prompt
- `replace`：完全替换为指定 prompt
- `append`：在默认 prompt 后追加

**共享知识机制**：
- `CLAUDE.md` 发现与注入：自动发现项目 CLAUDE.md 并注入所有子代理
- `MEMORY.md` 持久记忆：跨代理边界共享知识
- 会话恢复：子代理可恢复先前会话历史

**上下文压缩**：自动压缩（Auto-Compact），维护跨压缩会话的任务状态。

#### 2.2.5 Sub-Agent 的权限管理

**双通道权限同步协议**（`swarm/permission_sync.py`）：

子代理（Worker）提交权限申请：
1. Worker 调用 `write_permission_request()` → 写入 `pending/{id}.json`
2. Leader 通过 `read_pending_permissions()` 获取待审批请求
3. Leader 调用 `resolve_permission()` 批准/拒绝，并可选修改输入参数
4. Worker 轮询 `poll_for_response()` 获取审批结果

**读写分离**：只读工具（文件读取、搜索、Web）自动审批，通过 `_is_read_only()` 启发式方法判断。高危工具（bash、文件编辑）需要 Leader 明确批准。

**权限模式层级**：
- Plan Mode 优先于 bypass-permissions（安全设计）
- Worker 自动设置 `OPENHARNESS_AGENT_TEAMS=1`，防止递归进入协调器模式

**Human-in-the-Loop**：Leader 代理（通常是人工操控的主实例）作为权限决策者，支持手动审批、拒绝、修改参数。

#### 2.2.6 Tools 与 Skills 管理

**工具集**：43+ 内置工具，涵盖文件 I/O、Shell、搜索、MCP 协议。

**子代理工具限制**：通过 `TeammateSpawnConfig.permissions` 字段传入白名单。Sandbox 模式下默认允许：bash、process、read、write、edit、session 管理；默认拒绝：browser、canvas、外部集成。

**Skills 机制**：
- Skills 存储于 `~/.openclaw/workspace/skills/<skill>/SKILL.md`（注：文档引用了 OpenClaw 路径，实际路径待确认）
- 通过 ClawHub 注册表管理

---

### 2.3 HermesAgent (NousResearch)

**基础信息**：Python，★172,963，描述为"the agent that grows with you"，面向多平台消息（Discord/Slack/Telegram 等），包含 Kanban、Cron、Skills 等完整生态。

#### 2.3.1 Sub-Agent 的创建方式

**编程式，基于 `delegate_task` 工具**。子代理不需要预先声明，通过工具调用动态创建：

```python
# delegate_task 工具参数（来自 tools/delegate_tool.py）
{
    "goal": str,           # 任务目标
    "context": str,        # 可选：传递给子代理的上下文
    "toolsets": list[str]  # 可选：指定子代理可用的工具集
}
# 批量并行版本
{
    "tasks": [
        {"goal": "...", "context": "...", "toolsets": ["file", "web"]},
        {"goal": "...", "toolsets": ["terminal"]}
    ]
}
```

子代理的定义是**纯动态的**，没有预注册的代理类型，主代理在调用时直接指定目标和工具集。

另有 **Kanban 系统**（`cron/jobs.py`，`tools/kanban_tools.py`）用于持久化多代理工作队列：

```python
# Kanban worker 由环境变量激活
os.environ["HERMES_KANBAN_TASK"] = "task_id"
# Worker 获得专属工具集：kanban_show, kanban_complete, kanban_block,
#                          kanban_heartbeat, kanban_comment, kanban_create, kanban_link
```

#### 2.3.2 Sub-Agent 的调度与生命周期管理

**调度**：LLM 驱动，通过 `delegate_task` 工具决策。

**嵌套深度控制**：
- `delegation.max_spawn_depth`（默认 2）：最大嵌套层数
- **Leaf agents**（达到最大深度）：禁用 `delegate_task`、`clarify`、`memory`、`send_message`、`execute_code`
- **Orchestrator agents**（未达到最大深度）：保留 `delegate_task` 能力

**并发支持**：✅ 通过 `tasks` 参数批量派发，`delegation.max_concurrent_children`（默认 3）控制最大并发数。

**生命周期管理**：
- 父代理阻塞等待子代理完成（`delegate_task` 是同步的，非持久化）
- 超时控制：`delegation.child_timeout_seconds`
- 中断传播：父代理收到中断信号时同步取消所有子代理

**Kanban 持久化**：`cron/jobs.py` 定时器（默认 60 秒间隔）：
1. 回收超时认领
2. 推进就绪任务
3. 原子认领任务
4. 派生对应 profile 的 Worker

Worker 状态机：`pending` → `claimed` → `complete` / `blocked`

**执行线程绑定**：每个子代理绑定到特定执行线程，`interrupt()`/`clear_interrupt()` 可精确作用于该线程，不影响并发的其他子代理。

#### 2.3.3 Agent 间通信、Handoff、协作

**通信模型**：隐式（通过返回值）。`delegate_task` 阻塞直到子代理完成，结果通过函数返回值传回父代理。

**父代理 context 展示**：子代理返回结果以 tool result 形式追加到父代理对话，父代理 LLM 在下一轮看到子代理完成的工作。

**协作拓扑**：
- **层级委派（Hierarchical Delegation）**：多级父→子代理链，受 `max_spawn_depth` 约束
- **并行 Swarm**：`tasks` 参数支持同时派发多个子代理
- **Kanban 工作队列**：多个 Worker profile 并发消费同一队列

**Kanban 多代理隔离**：通过 `HERMES_KANBAN_BOARD` 环境变量硬隔离，Workers 只能看到自己的 board。

#### 2.3.4 Context 的注入与传递机制

**子代理 context 内容**：
- 独立终端会话（isolated terminal session）
- 独立 context workspace
- 父代理通过 `context` 参数显式传递的字符串
- 继承的 MCP 工具集（`delegation.inherit_mcp_toolsets = true` 时）
- `toolsets` 参数指定的工具集

**重要设计**：子代理获得的 context 不包含父代理的对话历史，仅有调用时显式传递的 `context` 字符串。父代理需要将关键信息序列化为字符串传入。

**上下文压缩（ContextCompressor）**：五阶段算法：
1. Tool result 预剪枝
2. Head 保护（系统 prompt + 初始对话）
3. Tail 保护（最近 ~20K token，基于预算动态计算）
4. 中间段 LLM 摘要（仅摘要中间轮次）
5. 迭代更新（保留先前摘要，跨多次压缩）

**Prompt Caching**：Claude 模型上自动启用（约降低 75% 输入成本）。

**Memory 系统**：`agent/memory_manager.py`：
- `sync_turn(turn_messages)`：每轮后持久化
- `prefetch(query)`：响应生成前检索记忆
- `shutdown()`：清理资源

#### 2.3.5 Sub-Agent 的权限管理

**Leaf-level 限制**：达到最大嵌套深度的子代理自动被剥夺高危工具。

**工具集门控（Gating）**：
- 需要特定环境变量的工具（如 `HASS_TOKEN`）在未满足条件时不出现在工具列表
- `check_requirements()` 模式：工具自我报告可用性

**安全边界**：
- `hermes-webhook` 工具集只包含安全工具（web_search、web_extract、vision_analyze、clarify），防止来自不可信第三方内容的注入攻击
- `delegate_task` 是**非持久化**的，不适合长时间运行的任务；长任务需用 `cronjob` 或 `terminal(background=True)`

**容器隔离**：支持容器化部署，子代理可在隔离容器中运行。

#### 2.3.6 Tools 与 Skills 管理

**工具集系统**（`toolsets.py`）：
- 工具集是工具的命名集合，支持通过 `includes` 字段层级组合
- `resolve_toolset()` 展平嵌套定义，处理循环依赖
- 平台级默认工具集（`_HERMES_CORE_TOOLS`），各消息平台在此基础上定制

**子代理工具配置**：通过 `delegate_task` 的 `toolsets` 参数指定，无需预注册类型。

**Skills 机制**（创新设计）：
- 封闭学习循环：代理在完成复杂任务后自动创建 Skills
- Skills 自我改进：在使用中持续优化
- 兼容 `agentskills.io` 开放标准
- 存储于 `~/.hermes/skills/`，`/skills` 浏览，`/<skill-name>` 调用
- 注入方式：Skill 内容作为用户消息（而非系统 prompt）注入，保护 prompt cache

---

### 2.4 OpenCode (Anomaly)

**基础信息**：TypeScript，★167,199，描述为"The open source coding agent"，开发分支为 `dev`，活跃度极高。

#### 2.4.1 Sub-Agent 的创建方式

**声明式 + 内置预定义**。子代理定义在代码中的 `Info` schema 结构：

```typescript
// 来自 packages/opencode/src/agent/agent.ts
const agents = {
  build: {
    mode: "primary",
    native: true,
    // ...full-access default agent
  },
  plan: {
    mode: "subagent",
    // read-only planning
  },
  general: {
    mode: "subagent",
    // parallel task execution
  },
  explore: {
    mode: "subagent",
    // codebase analysis, read-only
  },
  scout: {
    mode: "subagent",
    experimental: true,
    // external dependency inspection
  }
};
```

**内置子代理类型**：
| 代理 | 模式 | 特点 |
|------|------|------|
| `build` | primary | 全功能开发代理 |
| `plan` | subagent | 只读，用于分析和规划 |
| `general` | subagent | 复杂搜索和多步骤任务 |
| `explore` | subagent | 代码库分析，快/中/深三种模式 |
| `scout` | subagent（实验性）| 外部依赖检查 |

**动态生成**：`generate` 函数支持从自然语言描述 AI 生成代理配置，自动检查 ID 冲突。

#### 2.4.2 Sub-Agent 的调度与生命周期管理

**调度**：LLM 驱动 + 程序化结合。`task.ts` 中的 `task` 工具执行子代理调用：

```typescript
// task.ts 核心逻辑
const agentConfig = agent.get(params.subagent_type);
const nextSession = await sessions.create({
    parentID: ctx.sessionID,    // 建立父子关系
    permission: deriveSubagentSessionPermission(...)
});
```

**生命周期**：
- **前台模式（foreground）**：阻塞直到子代理完成，结果以 XML 格式返回：
  ```xml
  <task id="..." state="completed"><task_result>text</task_result></task>
  ```
- **后台模式（background）**：立即返回，通过 `inject("completed", text)` 异步通知父会话
- 错误时通过 `inject("error", errorText(...))` 注入错误

**并发支持**：✅ 后台模式支持并发。

**递归嵌套**：`deriveSubagentSessionPermission` 默认禁止子代理再派生子代理（`task` 权限默认设为 deny）。

#### 2.4.3 Agent 间通信、Handoff、协作

**通信模型**：结果注入（inject 模式）。后台子代理通过 `inject` 函数将结果推送到父会话 context，前台模式则直接返回。

**协作拓扑**：Supervisor 模式，`build` 代理作为主代理，调度各专业子代理。

**Session 分叉**（`fork` 方法）：
- 在特定消息边界创建分叉
- 克隆消息历史（新消息 ID，重映射父引用）
- 保留 `workspaceID`，可选传入 `permission` 规则集

#### 2.4.4 Context 的注入与传递机制

**权限派生**（`subagent-permissions.ts`）：`deriveSubagentSessionPermission` 从三个来源合并权限：
1. 父代理的编辑限制（Plan Mode 的文件编辑限制在代理 ruleset 层面，而非 session 层面）
2. 父会话的 deny 规则和外部目录限制
3. 默认禁止：`todowrite` 和 `task`（防止子代理再派生）

**Session 继承**：子代理创建时通过 `parentID: ctx.sessionID` 建立父子关系，权限通过 `permission: [...parentPermissions]` 数组传递（值拷贝，非引用）。

**目录隔离**：每个 Session 有独立 `directory` 和可选 `path`，`sessionPath()` 将绝对路径转换为相对 workspace 路径。

**模型配置继承**（三层优先级）：
1. 子代理专属配置（`subagents.model`）
2. 父代理配置
3. 默认子代理配置（`agents.defaults.subagents.model`）

#### 2.4.5 Sub-Agent 的权限管理

**权限层级设计**：代码注释明确说明：
> "Plan Mode's file-edit restriction lives on the agent ruleset, not on the session" — 防止子代理通过 session 继承绕过父代理限制。

**叠加限制**：父代理的 deny 规则与 session deny 规则叠加，子代理总是处于比父代理更紧的约束下（累积限制，无法升级权限）。

**工具级门控**：`task` 和 `todowrite` 权限单独检查，默认 deny，防止权限升级攻击（privilege escalation through task spawning）。

**Explore 代理**：仅允许 grep、glob、list、bash、web 操作；外部目录默认"ask"，白名单路径允许；默认禁止读 `.env` 文件。

#### 2.4.6 Tools 与 Skills 管理

**工具继承**：子代理工具集通过 `deriveSubagentSessionPermission` 派生，选择性继承父代理权限。

**Skills**：`packages/opencode/src/skill/` 目录，通过 `skill.ts` 工具调用，具体细节未深入调研。

**Agent 生成（AI 生成代理）**：`generate` 函数使用 AI 从自然语言描述生成代理配置，保证 ID 唯一性。

---

### 2.5 DeepAgents (LangChain)

**基础信息**：Python + TypeScript，★23,550，"batteries-included agent harness"，建立在 LangGraph 之上，适合生产部署。

#### 2.5.1 Sub-Agent 的创建方式

**两种方式**：

**方式一：`subagents` 参数传入 `AsyncSubAgent` 字典**

```python
from deepagents import create_deep_agent

async_subagents = [
    {
        "name": "researcher",
        "description": "A research agent that investigates any topic...",
        "graph_id": "researcher",           # LangGraph graph ID
        "url": RESEARCHER_URL,              # 远程 Agent Protocol URL
        "headers": {"x-auth-scheme": "custom"},  # 认证头
    },
]

supervisor = create_deep_agent(
    model=ChatAnthropic(model="claude-sonnet-4-5"),
    checkpointer=checkpointer,
    system_prompt="...",
    subagents=async_subagents,
)
```

**方式二：传入 LangGraph `CompiledStateGraph`**

```python
# 任何 LangGraph CompiledStateGraph 都可以作为子代理传入
custom_graph = create_custom_langgraph_workflow()
agent = create_deep_agent(
    model="openai:gpt-5.5",
    subagents=[custom_graph],  # 本地 graph 作为子代理
)
```

这是 DeepAgents 的独特设计：**允许将现有 LangGraph 工作流直接作为子代理复用**，实现与生态系统的无缝对接。

**配置文件**（`deepagents.toml`）：

```toml
[agent]
name = "deepagents-deploy-gtm-agent"
model = "openai:gpt-5.4-nano"
description = "Go-to-market strategy agent that coordinates research and content creation"

[sandbox]
provider = "none"
```

主代理配置极简：名称、模型、描述，System Prompt 来自项目 AGENTS.md 文件（运行时读取注入）。工具通过 `skills/` 目录和 `mcp.json` 自动发现。

#### 2.5.2 Sub-Agent 的调度与生命周期管理

**调度**：LLM 驱动，通过工具调用委派。主代理（Supervisor）拥有五个异步子代理管理工具：
- `start_async_task`：启动后台任务
- `check_async_task`：轮询状态
- `update_async_task`：更新运行中任务的指令
- `cancel_async_task`：取消任务
- `list_async_tasks`：列出所有任务状态

**设计原则**：每次请求一个工具调用，避免轮询循环（"one tool call per request" 原则）。

**生命周期**：
- **远程子代理**：通过 Agent Protocol（HTTP REST API），HTTP 状态轮询（`pending → running → success/error/cancelled`）
- **本地 LangGraph 子代理**：通过 `ainvoke()` 异步调用
- **状态持久化**：通过 LangGraph 的 checkpointer 维护状态

**并发支持**：✅ 基于 `asyncio.ensure_future` 的 fire-and-forget 模式，HTTP 服务端异步执行不阻塞。

**远程子代理服务器**：子代理可作为独立 FastAPI 服务部署，暴露 Agent Protocol 接口：
- `POST /threads`：创建对话线程
- `POST /threads/{thread_id}/runs`：启动执行
- `GET /threads/{thread_id}/runs/{run_id}`：轮询状态
- `GET /threads/{thread_id}`：获取最终结果

#### 2.5.3 Agent 间通信、Handoff、协作

**通信模型**：HTTP REST（远程）或 LangGraph state（本地）。

**协作拓扑**：
- **Supervisor 模式**：主代理作为 Supervisor，持有所有子代理任务的 ID，轮询结果
- **远程 Worker 池**：子代理作为独立服务部署，Supervisor 通过 Agent Protocol 协调

**上下文中断策略**（`multitask_strategy: interrupt`）：新任务到达时取消当前运行，清除线程状态，启动新任务。

#### 2.5.4 Context 的注入与传递机制

**Agent Protocol 隔离**：每个子代理线程（`thread_id`）有完全独立的消息历史和 state，通过 UUID 唯一标识。

**本地 graph 上下文**：通过 LangGraph `MemorySaver` checkpointer + `thread_id` 维护持久 context，`ainvoke()` 调用时传入完整消息链。

**共享内存命名空间**：AGENTS.md 被注入到"共享内存命名空间"，代理运行时可读取，但写入被只读中间件阻止（除非 `memories.agent_writable = true`）。

**上下文压缩**：内置"summarize long threads and offload tool outputs to disk"功能，延续 LangGraph 的 context 管理能力。

#### 2.5.5 Sub-Agent 的权限管理

**沙箱配置**：`deepagents.toml` 中通过 `[sandbox]` 配置执行环境，支持 E2B 等沙箱提供商。

**AGENTS.md 写保护**：只读中间件阻止代理修改自身的 AGENTS.md（system prompt 文件），防止 prompt 注入攻击。

**权限继承**：未发现细粒度的子代理权限控制机制，主要依赖 LangGraph 底层的 checkpointing 和 state 隔离。

#### 2.5.6 Tools 与 Skills 管理

**工具自动发现**：
- Skills 从 `skills/` 目录自动发现
- MCP 服务器从 `mcp.json` 自动检测
- 支持"bring your own functions"（任意 Python 函数）

**LangGraph 互操作**：子代理可以是任意 `CompiledStateGraph`，实现工具与工作流的最大复用。

---

### 2.6 OpenClaw

**基础信息**：TypeScript，★375,532（最高），描述为"Your own personal AI assistant. Any OS. Any Platform."，定位为跨平台个人助手，强调数据所有权。

#### 2.6.1 Sub-Agent 的创建方式

**配置文件驱动（workspace-based）**。子代理通过工作区配置文件定义，而非代码声明：

```
~/.openclaw/workspace/
├── AGENTS.md      # 代理行为定义
├── SOUL.md        # 代理个性/价值观
└── TOOLS.md       # 工具权限配置
```

**设计哲学**（来自 VISION.md）：**明确反对** Agent 层级框架（manager-of-managers/nested planner trees）作为默认架构，也反对重复底层基础设施的重型编排层。核心保持精简，复杂多代理功能通过插件系统扩展。

#### 2.6.2 Sub-Agent 的调度与生命周期管理

**路由机制**：基于渠道的多代理路由，8 层优先级匹配（不是子代理调度，而是将不同的入站消息渠道/账号/用户路由到独立的隔离代理实例）：

```
1. 精确 Peer 匹配（exact user/chat/group ID）
2. 父 Peer 继承（thread parent fallback）
3. Peer 通配（所有私信/所有频道）
4. Guild + 角色（Discord 特定）
5. Guild 全局
6. Team（Microsoft Teams）
7. 账号范围默认
8. 频道全局兜底
```

这是**水平扩展的多租户路由**，而非垂直的 Supervisor/Worker 层级。

**Session 隔离级别**：
- `main`：所有私信合并为单一 session
- `per-peer`：每个联系人独立 session
- `per-channel-peer`：按频道 + 用户隔离
- `per-account-channel-peer`：完全隔离

#### 2.6.3 Agent 间通信、Handoff、协作

**Session 工具**：
- `sessions_list`：枚举活跃 sessions
- `sessions_history`：访问对话历史
- `sessions_send`：跨 session 发消息（agent-to-agent 通信的核心）

这三个工具在**叶层级 sub-agent（depth >= maxSpawnDepth）处始终被 deny**，防止 terminal 节点进行协调操作。

**协作拓扑**：多租户路由（水平），而非 Supervisor-Worker（垂直）。

#### 2.6.4 Context 的注入与传递机制

**workspace 路径映射**：`resolveAgentWorkspaceDir()` 按 workspace 目录隔离 context。`resolveAgentIdsByWorkspacePath()` 通过路径包含关系识别适用代理，词法路径比较，Windows 大小写不敏感。

**模型配置三层继承**：
1. 子代理专属设置（`subagents.model`）
2. 父代理配置
3. 默认子代理设置（`agents.defaults.subagents.model`）

**自动回退探测**：跟踪主模型可用性，配置可重试间隔的回退探测。

#### 2.6.5 Sub-Agent 的权限管理

**基于角色的工具访问控制（RBAC）**：
- `main` session：工具在宿主机上运行，代理拥有完全访问权
- 非 `main` session（群组/频道）：可在受限容器中运行，工具白名单管控

**工具 deny 列表（来自 `agent-tools.policy.ts`）**：

所有子代理始终禁用：`gateway`、`agents_list`、`session_status`、`cron`、`sessions_send`

叶层级（depth >= maxSpawnDepth）额外禁用：`subagents`、`sessions_list`、`sessions_history`、`sessions_spawn`

**信任组验证**（fail-closed 设计）：`resolveTrustedGroupId` 不允许非群组 session 通过 arbitrary groupId 声明群组级工具策略——必须是服务端派生的 session 上下文才能授权群组分配。

**技术策略层（`agent-tools.policy.ts`）**：
1. 基于 Profile 的策略（命名权限集）
2. Provider 专属策略（模型 ID 匹配）
3. 群组范围策略（频道上下文感知）
4. 继承策略（子代理继承父 session 的 allowlist/denylist）

#### 2.6.6 Tools 与 Skills 管理

**Skills 注册**：通过 **ClawHub 注册表**管理，存储于 `~/.openclaw/workspace/skills/<skill>/SKILL.md`，提供结构化的代理能力扩展。

**工具集**：browser、canvas、nodes、cron jobs、平台专用 actions 等一流工具支持。

**插件系统**：核心保持精简，能力通过插件系统扩展，插件通过 `openclaw/plugin-sdk/*` 访问核心能力，受严格 API 边界约束。

---

## 3. 横向对比

### 3.1 Sub-Agent 创建方式对比

| 框架 | 创建方式 | 声明时机 | 预注册 vs 动态 | 定义格式 |
|------|---------|---------|--------------|---------|
| **Claude Agent SDK** | 编程式 + 文件系统 + CLI | 调用前静态声明 | 预注册 | Python/TS 代码 或 Markdown YAML frontmatter |
| **OpenHarness** | 编程式 + 数据类 | 运行时构建 | 半动态 | `TeammateSpawnConfig` dataclass |
| **HermesAgent** | 完全动态（tool 参数） | 运行时 | 纯动态 | `delegate_task` 参数字典 |
| **OpenCode** | 声明式（代码内置） | 编译时 | 预注册 | TypeScript Info schema |
| **DeepAgents** | 编程式 + TOML + LangGraph | 初始化时 | 预注册 | Python 字典 或 `CompiledStateGraph` |
| **OpenClaw** | 配置文件（workspace） | 工作区配置 | 预注册 | Markdown 文件 |

### 3.2 调度机制对比

| 框架 | 调度驱动 | 并发支持 | 递归嵌套 | 最大深度控制 |
|------|---------|---------|---------|------------|
| **Claude Agent SDK** | LLM 驱动（Agent tool） | ✅（background 模式） | ❌ 明确禁止 | 无需（单层） |
| **OpenHarness** | LLM + 程序化混合 | ✅ 原生支持 | ✅ 支持 | 未明确限制 |
| **HermesAgent** | LLM 驱动（delegate_task） | ✅（max_concurrent_children=3） | ✅（max_spawn_depth=2） | ✅ 显式配置 |
| **OpenCode** | LLM 驱动（task tool） | ✅（background 模式） | ❌ 默认禁止 | 无（通过权限防止） |
| **DeepAgents** | LLM 驱动（async tools） | ✅（asyncio） | 未明确说明 | 未明确说明 |
| **OpenClaw** | 消息路由（非 Supervisor） | ✅（多渠道） | N/A（水平扩展） | depth 控制 session 工具访问 |

### 3.3 Context 传递机制对比

| 框架 | 子代理 Context | 父→子传递方式 | 子→父传递方式 | 历史共享 |
|------|-------------|------------|------------|---------|
| **Claude Agent SDK** | 完全独立（fresh） | Agent tool prompt 字符串 | 最终消息 verbatim 作为 tool result | ❌（Fork 除外） |
| **OpenHarness** | 独立（subprocess 隔离） | CLI flags + 环境变量 + prompt | 结果返回 + mailbox | 通过 MEMORY.md 共享 |
| **HermesAgent** | 独立（isolated terminal） | `context` 参数字符串 + 工具集配置 | 函数返回值（阻塞等待） | ❌（显式传递） |
| **OpenCode** | Session 隔离（parentID 关联） | session permission + 父 session 任务描述 | inject 注入 / XML 结果 | 通过 fork 可继承 |
| **DeepAgents** | Thread 隔离（thread_id） | Agent Protocol + checkpointer state | HTTP 轮询结果 / LangGraph state | 通过 AGENTS.md 共享 |
| **OpenClaw** | Workspace 隔离 | 渠道上下文 + session key | sessions_send 工具 | 通过 workspace 共享 |

### 3.4 权限管理对比

| 框架 | 工具白名单 | 工具黑名单 | 嵌套权限继承 | Human-in-the-Loop | 沙箱 / 隔离 |
|------|----------|----------|------------|------------------|------------|
| **Claude Agent SDK** | ✅ tools 字段 | ✅ disallowedTools | 累积（父不可超越） | ✅ 前台透传 | ✅ worktree |
| **OpenHarness** | ✅ permissions 列表 | ✅ 沙箱默认 deny | 双通道权限同步 | ✅ Leader 审批 | ✅ 容器 + worktree |
| **HermesAgent** | ✅ toolsets 参数 | ✅ leaf agent 自动限制 | 叶节点剥夺高危工具 | ✅ 工具调用拦截 | ✅ 容器 |
| **OpenCode** | ✅ 派生权限集 | ✅ task/todowrite 默认 deny | 累积（叠加不可升级） | ✅ 前台透传 | ✅ session 隔离 |
| **DeepAgents** | 依赖 LangGraph | AGENTS.md 只读保护 | 未明确 | 未明确 | ✅ 沙箱提供商 |
| **OpenClaw** | ✅ profile 策略 | ✅ 叶节点 deny 列表 | 继承 + 覆盖 | ✅ fail-closed 设计 | ✅ 容器 + trust 验证 |

### 3.5 核心能力矩阵

| 能力 | Claude Agent SDK | OpenHarness | HermesAgent | OpenCode | DeepAgents | OpenClaw |
|------|:--------------:|:-----------:|:-----------:|:--------:|:----------:|:--------:|
| 文件系统定义（YAML/MD） | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ |
| 编程式 API 定义 | ✅ | ✅ | ✅（动态） | ✅ | ✅ | ❌ |
| 子代理并发执行 | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| 递归嵌套子代理 | ❌ | ✅ | ✅ | ❌ | 未明确 | N/A |
| Worktree 隔离 | ✅ | ✅ | ❌ | ❌ | ❌ | ❌ |
| 持久化 Skills | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| 子代理专属 MCP | ✅ | ✅ | ✅ | ❌ | ✅ | ❌ |
| 持久记忆（跨会话） | ✅ | ✅ | ✅ | ❌ | ✅ | ❌ |
| 远程子代理（HTTP） | ❌ | ❌ | ❌ | ❌ | ✅ | ❌ |
| Fork（继承父 context） | ✅（实验性） | ❌ | ❌ | ✅ | ❌ | ❌ |
| Human-in-the-Loop | ✅ | ✅ | ✅ | ✅ | 未明确 | ✅ |
| 子代理 Hooks | ✅ | ❌ | ✅（插件） | ❌ | ❌ | ❌ |
| AI 生成子代理定义 | ✅（/agents 命令） | ❌ | ❌ | ✅（generate 函数） | ❌ | ❌ |

---

## 4. 共性设计模式与差异化取舍

### 4.1 共性设计模式

#### 模式一：Agent Tool 作为调度入口

**五个框架中有四个**（Claude Agent SDK、OpenHarness、HermesAgent、OpenCode）都将子代理调用封装为 LLM 可见的工具（`Agent`/`task`/`delegate_task`）。这使得：
- 调度决策由 LLM 自然推理，而非硬编码规则
- 主代理可以根据任务语义动态选择最合适的子代理
- 调试时可通过工具调用日志追踪整个决策链

**关键洞察**：`description` 字段是调度的核心——它是写给 LLM 看的"当应该调用我"的自然语言说明，而非写给程序看的参数。

#### 模式二：Context 隔离 + 精确传递

所有框架都选择了**子代理 context 隔离**（fresh context window），原因：
1. 防止父代理的无关 context 污染子代理的推理
2. 节省 token（子代理的中间工具调用不回传到父代理 context）
3. 独立的压缩/清理策略

**传递机制**：由于 context 隔离，父→子的信息传递需要显式、精确。主代理在 `Agent` tool 的 prompt 参数中必须序列化所有子代理需要的关键信息（文件路径、错误消息、决策背景等）。这是"精确传递"原则。

#### 模式三：权限累积不升级

所有实现了多层权限的框架（Claude Agent SDK、OpenHarness、OpenCode、OpenClaw）都遵循同一原则：**子代理只能获得父代理权限的子集，不能通过创建子代理来获取更高权限**。

技术实现：
- 值拷贝权限（不是引用），防止子代理修改影响父代理
- 默认 deny 高危工具（`task`/`Agent`），子代理无法再派生子代理
- 叶节点在达到最大深度时强制剥夺编排工具

#### 模式四：结果摘要回传

所有框架都设计了子代理运行结果以**摘要形式**回传到父 context，而非原始工具调用链。这保持了父 context 的简洁性。Claude Agent SDK 文档中明确指出："The parent receives the subagent's final message verbatim as the Agent tool result, but may summarize it in its own response."

#### 模式五：Skills 作为知识封装单元

四个框架（Claude Agent SDK、HermesAgent、OpenCode、OpenClaw）都有 Skills 机制。Skills 的共同特征：
- 以文件（Markdown）形式持久化
- 可按需加载到特定代理
- 注入时机：要么预加载到 context（静态），要么运行时调用（动态）
- Hermes 还支持代理自主创建 Skills（封闭学习循环）

### 4.2 差异化取舍

#### 取舍一：静态声明 vs 完全动态

**Claude Agent SDK / OpenCode**：静态声明（编译前已知所有子代理类型），优势是可预测、可审计、描述字段驱动 LLM 自动选择。

**HermesAgent**：完全动态（运行时按需创建），优势是灵活，任何任务都可以创建专门的子代理，但缺乏可复用的命名类型。

**取舍**：静态声明更适合团队协作（version control 管理代理定义），动态创建更适合通用个人代理场景。

#### 取舍二：禁止嵌套 vs 允许递归

**Claude Agent SDK / OpenCode**：禁止子代理再派生子代理。优势：安全边界清晰、调试简单、防止失控递归。

**HermesAgent / OpenHarness**：允许多级嵌套（max_spawn_depth 控制）。优势：支持复杂层级分工，如"Orchestrator → Researcher + Writer → Editor"。

**取舍**：Claude 系列倾向保守安全设计，Hermes 更注重能力完整性。对于生产环境，禁止嵌套更易于推理和 debug。

#### 取舍三：文件系统定义 vs 纯代码定义

**Claude Agent SDK（文件系统）/ OpenClaw（workspace）**：代理定义与代码解耦，可以 version control，团队可协作编辑，支持运行时发现新代理（无需重启）。

**HermesAgent / DeepAgents**：纯代码定义，类型安全，IDE 友好，但更新需要重新部署。

**取舍**：文件系统方式适合代理定义频繁变化的场景；代码方式适合严格类型化的工程化场景。

#### 取舍四：同步等待 vs 异步后台

**HermesAgent**：`delegate_task` 阻塞等待，简化父代理的状态机（子代理完成后才继续），但无法并行。

**Claude Agent SDK / OpenCode**：支持 background 模式，子代理后台运行，父代理可继续接受输入，但增加状态管理复杂度。

**DeepAgents**：Remote 子代理天然异步（HTTP 轮询），最适合长时间运行的任务。

#### 取舍五：Context Fork vs 全新 Context

**Claude Agent SDK（Fork 模式）/ OpenCode（fork 方法）**：Fork 继承父对话全部历史，适合"相同背景下尝试不同方法"场景，可复用 prompt cache 降低成本。

**其他框架**：始终全新 context，更干净隔离，但子代理需要从头建立上下文理解。

**取舍**：Fork 牺牲隔离换取效率，适合探索性任务；全新 context 更适合专业化的隔离任务。

---

## 5. 对 harness9 的设计建议

基于以上调研，以下是为 harness9 实现 Sub-Agent 功能的具体设计建议，充分结合 harness9 的 Go 语言技术栈和现有架构：

### 5.1 设计原则建议

**P0 原则（不可妥协）**：
1. **安全优先**：默认禁止子代理再派生子代理（参考 Claude Agent SDK 的明确禁止设计）
2. **Context 隔离**：子代理必须有独立的 context window，不共享父代理对话历史
3. **权限不升级**：子代理权限只能是父代理权限的子集，通过值拷贝传递

**P1 原则（重要）**：
4. **LLM 驱动调度**：将子代理暴露为 LLM 可见的工具（如 `task` 工具），让模型基于语义推理何时调用哪个子代理
5. **类型安全**：利用 Go 的强类型系统定义 `SubAgentDefinition` 结构体，而非动态 map

### 5.2 核心数据类型设计

```go
// internal/schema/subagent.go

// SubAgentDefinition 子代理定义
type SubAgentDefinition struct {
    Name        string   // 唯一标识符（小写字母和连字符）
    Description string   // 告知主代理何时使用此子代理（写给 LLM 看）
    SystemPrompt string  // 子代理的 system prompt
    Tools       []string // 允许的工具名称列表（nil 表示继承全部）
    DisallowedTools []string // 禁止的工具名称
    MaxTurns    int      // 最大轮数（0 表示继承默认）
    // Model override: empty string means inherit from parent
    ModelOverride string
}

// SubAgentRequest 主代理调用子代理时的请求
type SubAgentRequest struct {
    SubAgentType string // 对应 SubAgentDefinition.Name
    Prompt       string // 具体任务描述（传递给子代理的唯一信息渠道）
    Background   bool   // 是否后台异步运行
}

// SubAgentResult 子代理执行结果
type SubAgentResult struct {
    AgentID    string
    FinalText  string
    Error      error
    TurnCount  int
    TokenUsage Usage
}
```

### 5.3 子代理创建方式建议

参考 Claude Agent SDK 的双轨制，建议 harness9 支持：

**方式一：编程式（Go 结构体，推荐）**

```go
// cmd/harness9/main.go 中初始化
registry.RegisterSubAgent(tools.SubAgentDefinition{
    Name:        "code-reviewer",
    Description: "Expert code review specialist. Use after writing or modifying code.",
    SystemPrompt: "You are a code review specialist...",
    Tools:       []string{"read_file", "bash"},  // 只读工具
    MaxTurns:    20,
})
```

**方式二：文件系统（Markdown frontmatter）**

兼容 Claude Code 的 `.claude/agents/` 格式，在 `internal/skills/` 目录的加载器基础上扩展，扫描 `.harness9/agents/` 目录：

```markdown
---
name: code-reviewer
description: Expert code review. Use proactively after code changes.
tools: read_file, bash
max_turns: 20
---

You are a code review specialist...
```

### 5.4 Task 工具设计

```go
// internal/tools/task.go

type TaskTool struct {
    subAgentDefs map[string]SubAgentDefinition
    workDir      string
    // Engine factory: creates a new engine for the sub-agent
    engineFactory func(def SubAgentDefinition) Engine
}

// task 工具的 JSON Schema（传递给 LLM）
// {
//   "subagent_type": "string (one of: code-reviewer, ...)",
//   "prompt": "string (specific task for the subagent)",
//   "background": "boolean (run asynchronously)"
// }

func (t *TaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
    var req SubAgentRequest
    json.Unmarshal(args, &req)

    def, ok := t.subAgentDefs[req.SubAgentType]
    if !ok {
        return "", fmt.Errorf("unknown subagent type: %s", req.SubAgentType)
    }

    // Create isolated engine for sub-agent
    subEngine := t.engineFactory(def)

    if req.Background {
        // Non-blocking: return immediately, inject result later
        go func() {
            result, _ := subEngine.Run(ctx, req.Prompt)
            // Inject result back to parent session
        }()
        return fmt.Sprintf(`<task id="%s" state="running"/>`, generateID()), nil
    }

    // Blocking: wait for completion
    result, err := subEngine.Run(ctx, req.Prompt)
    if err != nil {
        return fmt.Sprintf(`<task state="error">%s</task>`, err.Error()), nil
    }
    return fmt.Sprintf(`<task state="completed"><task_result>%s</task_result></task>`, result), nil
}
```

### 5.5 权限传递设计

参考 OpenCode 的 `deriveSubagentSessionPermission` 模式：

```go
// internal/permission/subagent.go

// DeriveSubAgentPermissions 从父代理权限派生子代理权限
// 原则：子代理只能获得父代理权限的子集，且默认禁止 task 工具（防止递归）
func DeriveSubAgentPermissions(
    parentRules PermissionRules,
    def SubAgentDefinition,
) PermissionRules {
    derived := parentRules.Clone()  // 值拷贝，不修改父代理

    // Always deny recursive sub-agent spawning
    derived.Deny("task")

    // Apply tool allowlist if specified
    if len(def.Tools) > 0 {
        derived.RestrictToTools(def.Tools)
    }

    // Apply tool denylist
    for _, t := range def.DisallowedTools {
        derived.Deny(t)
    }

    return derived
}
```

### 5.6 Context 管理建议

子代理应使用**独立的 memory.Session**，与父代理 Session 完全隔离：

```go
// 在 engine 层创建子代理时
subSession := memory.NewMemorySession()  // 全新独立 session，不继承父代理历史

// 子代理只得到：
// 1. SubAgentDefinition.SystemPrompt 作为 system prompt
// 2. SubAgentRequest.Prompt 作为第一条 user 消息
// 3. 子代理定义允许的工具列表
// 不得到：父代理的对话历史、父代理的 system prompt
```

子代理 context 压缩：继承 harness9 现有的 `SummarizationCompactor` 逻辑，为子代理独立触发压缩，不影响父代理 session。

### 5.7 TUI 显示建议

参考 OpenHarness 的颜色标识和 Claude Code 的子代理面板：
- 在 `tui_view.go` 中增加子代理进度展示（参考现有 `renderToolProgress`）
- 子代理运行时用不同颜色前缀（如 `[reviewer]`）标识其流式输出
- 后台子代理完成后以 Observation 形式注入父会话，TUI 展示"子代理 {name} 完成"通知

### 5.8 实施路径建议

**第一阶段（MVP）**：
- 定义 `SubAgentDefinition` 结构体和 `Task` 工具（前台同步模式）
- 子代理使用 `MemorySession`（内存，不持久化）
- 权限通过 `DeriveSubAgentPermissions` 派生
- 无递归嵌套（Task 工具在子代理中不可用）

**第二阶段**：
- 后台异步子代理（background 模式）
- 文件系统代理定义（`.harness9/agents/*.md`）
- 并发子代理（参考现有并发工具执行机制）
- TUI 子代理进度面板

**第三阶段**：
- 子代理专属工具集（不同于父代理）
- 子代理持久化 Session（SQLiteSession）
- Context Fork 模式（子代理继承父代理历史快照）

---

## 6. 权威参考资料

以下资料均经 WebFetch 验证可正常访问，内容与调研主题直接相关：

### 6.1 官方文档

| 标题 | 来源 | URL | 摘要 |
|------|------|-----|------|
| Claude Agent SDK - Subagents | Anthropic 官方文档 | https://code.claude.com/docs/en/agent-sdk/subagents | 完整的 SDK 子代理 API 参考，涵盖 AgentDefinition 字段、生命周期、context 隔离、工具限制、背景运行 |
| Create custom subagents | Anthropic 官方文档 | https://code.claude.com/docs/en/sub-agents | 文件系统定义子代理的完整指南，含 frontmatter 格式、作用域优先级、Fork 模式、hooks 配置 |
| Claude Agent SDK - Overview | Anthropic 官方文档 | https://code.claude.com/docs/en/agent-sdk/overview | SDK 总体能力概览，含 subagents、hooks、sessions、permissions、MCP 各模块 |

### 6.2 GitHub 源码（经 API 确认存在且活跃）

| 项目 | 来源 | URL | 关键文件 |
|------|------|-----|---------|
| deepagents | LangChain | https://github.com/langchain-ai/deepagents | `examples/async-subagent-server/`、`libs/acp/` |
| OpenHarness | HKUDS | https://github.com/HKUDS/OpenHarness | `src/openharness/swarm/`（types.py/mailbox.py/permission_sync.py） |
| opencode | Anomaly | https://github.com/anomalyco/opencode | `packages/opencode/src/agent/`（agent.ts/subagent-permissions.ts/task.ts） |
| openclaw | OpenClaw | https://github.com/openclaw/openclaw | `src/agents/`（agent-tools.policy.ts/agent-scope.ts） |
| hermes-agent | NousResearch | https://github.com/NousResearch/hermes-agent | `agent/agent_init.py`、`toolsets.py`、`AGENTS.md` |

### 6.3 调研时重要的设计文档

| 标题 | 来源 | URL | 摘要 |
|------|------|-----|------|
| OpenClaw VISION.md | OpenClaw GitHub | https://raw.githubusercontent.com/openclaw/openclaw/main/VISION.md | 明确反对 manager-of-managers 层级架构，阐述"核心精简，插件扩展"哲学 |
| HermesAgent AGENTS.md | NousResearch GitHub | https://raw.githubusercontent.com/NousResearch/hermes-agent/main/AGENTS.md | 完整的 delegate_task/Kanban/Cron 多代理系统设计文档 |

---

*本报告基于 2026-05-30 的实时调研，所有信息来源于各框架 GitHub 仓库及官方文档站点的第一手获取，不依赖本地缓存或训练数据推断。*
