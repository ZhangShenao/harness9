# harness9 Agent OS：自举开发 Mission Control 设计

**状态：已确认，待实现规划**
**日期：2026-07-23**

## 1. 决策摘要

harness9 的下一演进方向是成为一个 local-first、开放的 **Agent OS**：它把本地 harness9 子代理、外部 A2A Agent 与 MCP 能力接入统一的控制面，协同完成可持续数小时或数天的复杂开发任务，并由管理 UI 提供可观测、可审批、可恢复的运维体验。

第一个端到端 Showcase 是 **Self-Hosting Development Mission**：Agent OS 自己编排 Agent 团队，完成 harness9 的开发、测试、验证与运行；随后由新 OS 完成一个事先冻结、未参与其构建过程的复杂 coding Mission。成功由独立测试和证据判定，不能以 Agent 的文字总结为准。

近期产品切口是 **Mission Control Agent OS**，而非 Agent Marketplace、自由 Agent Mesh 或重型通用 DAG 引擎。

## 2. 背景与问题

v1.0 已具备 ReAct 引擎、Sub-Agent、Skills、Sandbox、SQLite Session、Human-in-the-Loop、MCP Client、Eval 与 AutoDev 等能力。现有 `/autodev` 已把“需求澄清 → 规范确认 → worktree → dev 子代理 → build/test/commit”串成最小自举闭环，但仍是 Skill 驱动的单开发者流程：

- `TaskTracker` 仅在内存中保存后台子任务，进程退出后不能恢复；
- 子代理的上下文隔离，但没有持久化的 Mission/Task 依赖图、Artifact 或 Evidence；
- MCP 仅作为 Client 消费远程 `tools/list`/`tools.call`，不提供 MCP Server；
- 不支持 A2A Agent Card、远程 Agent Task 生命周期、重连与取消；
- TUI 能展示流和后台任务，但无法作为多 Agent 长程任务的运维控制面；
- 最终是否完成主要依赖 Agent 流程约束，缺少独立的发布验收门。

因此，下一阶段不应只是给 AutoDev 添加更多 prompt 或给 MCP 增加若干适配器，而应将任务、工件、验证和 Agent 生命周期提升为运行时的一等对象。

## 3. 产品定义与边界

### 3.1 产品定义

> harness9 Agent OS 是一个轻量、local-first 的 Go Agent 运行时：它用持久化 Mission Control Plane 编排本地与远程 Agent，隔离工作区，记录工件与验证证据，并通过 MCP/A2A 对外互操作。

稳定中心是 **Mission**，不是某一个 Agent。Agent 是可替换 Worker；Task、Artifact、Evidence、Policy 与 Workspace Lease 才是跨模型、跨协议、跨进程保持稳定的系统对象。

### 3.2 非目标

- 不构建公网 Agent Marketplace 或多租户云控制面；
- 不允许任意 Agent 点对点自由通信；协作始终经 Mission Supervisor；
- 不引入通用 DAG/workflow 框架；仅实现 Mission/Task 所需的有向依赖；
- 不做无人值守生产发布、自动扩权或自动采纳未知外部 Agent；
- 不把 MCP 工具伪装为 Agent，也不把 A2A Agent 降格为普通工具；
- 不废弃既有 `Run`、`RunStream`、TUI、Skills、Sandbox、MCP Client 或 `/autodev`。

## 4. 架构

```text
用户 / API / 外部 Client
          │
          ▼
   Management UI / MCP Server / A2A Server
          │
          ▼
     Mission Control Plane
  ┌───────┼────────┬───────────────┐
  ▼       ▼        ▼               ▼
Task DAG  Agent     Workspace       Artifact &
Store     Registry  Lease Manager   Evidence Store
  │       │        │               │
  └───────┴────────┴───────┬───────┘
                           ▼
                       Scheduler
              ┌────────────┼─────────────┐
              ▼            ▼             ▼
       Local Worker    A2A Worker     MCP Provider
       (Sub-Agent)     (remote)       (tools/resources)
```

### 4.1 核心领域对象

| 对象 | 责任 |
|---|---|
| Mission | 用户目标、预算、策略、整体生命周期与最终验收 |
| Task | 可分配的工作单元、输入合同、依赖、重试规则和验收条件 |
| Task Attempt | 某次 Agent 执行记录，绑定 Agent、Lease、事件和产物 |
| Agent | Agent Card/能力、协议、信任级别、成本限制、健康状态 |
| Workspace Lease | Task 独占的 git worktree、Sandbox、路径与过期时间 |
| Artifact | Spec、diff、代码提交、报告、二进制、日志和哈希 |
| Evidence | build/test/vet/eval/smoke-test/审查等不可变验收证据 |
| Policy | 工具 scope、预算、审批、合并与发布规则 |

### 4.2 任务状态

Mission：

```text
draft → planning → ready → running → verifying
                                  ├→ succeeded
                                  ├→ failed
                                  ├→ needs_attention
                                  └→ cancelled
```

Task：

```text
blocked → queued → leased → running → verifying → succeeded
                                           ├→ failed
                                           ├→ awaiting_input
                                           └→ indeterminate
```

`indeterminate` 表示远程 Agent、进程或网络在副作用期间中断。系统必须对账、恢复或请求人工决定，不能盲目重试。

### 4.3 调度与交接

- Planner 只能提交结构化 Task Plan；只有 Mission Supervisor 能创建真实 Task，防止递归任务爆炸；
- Scheduler 只调度依赖已满足的 Task，按能力、信任级别、预算、语言/工具需求和并发配额选 Agent；
- 修改代码的 Task 必须获得独占 Workspace Lease；每个 Lease 使用独立 git worktree；
- Worker 以 Task Contract、Artifact 与 Evidence 交接，不共享冗长对话历史；
- Worker 只提交 Artifact Manifest，不得自行把任务标为成功；
- Release Verifier 是唯一可以把 Mission 标记为 `succeeded` 的组件。

## 5. 协议边界

### 5.1 MCP

harness9 保持并扩展 MCP Host 能力，同时新增 MCP Server。

- **MCP Host**：消费外部工具、资源和声明 durable task 能力的服务；
- **MCP Server Resources**：只读提供 Mission、Task、Agent、Artifact、Evidence；
- **MCP Server Tools**：受控创建 Mission、查询状态、提交 Artifact、请求审批和取消 Task；
- 所有写操作都验证 capability token 与 Mission Policy；
- 耗时操作使用 MCP Task，支持轮询与取消；
- Client 升级到 Streamable HTTP、资源、结构化内容和新任务能力，同时保留现有配置兼容。

MCP 负责能力与控制面，不承担独立 Agent 的发现和长期协作语义。

### 5.2 A2A

harness9 同时实现 A2A Client 与 A2A Server。

- 通过 Agent Card 注册/发现显式批准的远程 Agent；
- 提交、流式观察、取消、轮询和重连远程长程 Task；
- 将远程 Task ID、状态、Artifact 和错误映射到本地 Task Attempt；
- 将 harness9 Agent OS 发布为可被其他 OS 委派的 A2A Agent；
- v1 只支持本地配置或人工批准的 Agent Card，不做公网自动发现；
- 网络中断后保留 remote task ID 并查询恢复，避免重复创建远程 Task。

## 6. 信任与安全

外部 Agent 始终是未信任 Worker：

- Scheduler 发放绑定 Mission、Task、worktree、工具 scope、预算和过期时间的 capability token；
- Agent 不得读取主机全局凭据、其他 Task 的工作区或未授权 Artifact；
- A2A/MCP 的文本、URL 和文件均视为未验证输入，不能直接触发命令、合并或权限升级；
- Agent 的 Artifact 必须经 Verifier 变为 Evidence；
- schema 变更、凭据、merge、release、删除和网络扩权进入既有审批链；
- Browser 管理 UI 默认只绑定 loopback；远程访问需要独立认证配置；
- 测试证据提交后使用内容摘要关联，不可被 Worker 静默改写。

## 7. 管理 UI

UI 由当前 TUI 与新增的本地浏览器 Dashboard 共同组成，二者读写同一份 Control Plane 数据。

### 7.1 Mission Dashboard

- 以 Mission 为首页，而非聊天流；
- 展示 Task 已验证进度、活动 Agent、Workspace Lease、待审批项；
- 以拓扑和 Wave 展示 Task 依赖与阻塞原因；
- 展示 Agent Fleet、健康、协议、当前任务和信任状态；
- 展示最近 Artifact/Evidence 与实时事件。

### 7.2 Mission Detail

- Task Graph、Artifacts、Evidence、Events、Policy 五个可切换视图；
- 展示每个 Task 的 Agent、Lease、输入合同、尝试和状态；
- 右侧集中展示不可变 Evidence 和高风险审批；
- 审批必须带原因、风险等级、受影响的 Task/Artifact 与可选反馈；
- UI 不能直接绕过 Policy 执行写操作。

视觉原型存于本地 `.superpowers/brainstorm/`，不是发布工件。

## 8. Self-Hosting Development Mission

首个 Showcase 固定为如下流水线：

```text
需求
  → 规划与架构
  → Task DAG 审批
  → 并行实现 Wave
  → 构建 / 测试修复 Wave
  → 独立代码审查
  → 集成与发布验证
  → 新 Agent OS 执行新的复杂任务
```

每个阶段的交付物：

| 阶段 | 必需工件 |
|---|---|
| 规划 | Spec、架构决策、Task DAG、验收合同 |
| 实现 | worktree、提交、diff、依赖变更 |
| 测试 | build/test/vet/eval 日志与结果 |
| 审查 | 独立 review、阻断项与修复证明 |
| 发布 | 可运行二进制、配置与 smoke test |
| 递归证明 | 新 OS 执行独立复杂 Mission 的 Artifact 与 Evidence |

Mission 只有在所有 required Task 成功/获豁免、受控分支集成、发布验证通过且新 OS 已完成独立复杂 Mission 后才能进入 `succeeded`。

## 9. 发布路线

| 版本 | 交付重点 | 用户可见价值 |
|---|---|---|
| v1.1 Mission Foundation | SQLite Mission/Task/Artifact/Evidence、Scheduler、Local Worker Adapter、worktree lease | `/autodev` 升级为可恢复、可审计的多 Agent Mission |
| v1.2 Open Control Plane | MCP Server、资源与受控 tools、Agent Registry、浏览器 Dashboard | 外部客户端可查看和管理 Mission |
| v1.3 Federated Workers | A2A Client/Server、Agent Card、远程 Task 流、重连与取消 | 可接入外部专业 Agent 协同开发 |
| v1.4 Self-Hosting Proof | Release Verifier、冻结复杂 coding Mission、完整证据报告 | 新 OS 用同一机制完成新的复杂任务 |

兼容性：既有 `Run`、`RunStream`、TUI、Session、Skills、Sandbox、MCP Client 和 `/autodev` 保持可用；`/autodev` 变为创建单个自举开发 Mission 的快捷入口。现有内存 `TaskTracker` 仍可支持即时 UI 反馈，但长期任务的唯一事实来源是 Mission Store。

## 10. 验收与测试

### 10.1 工程测试

- 单元：状态转换、依赖调度、lease 冲突、重试、权限和 Artifact 完整性；
- 集成：SQLite 恢复、进程中断、Sandbox/worktree 隔离、MCP/A2A mock 对端；
- 协议：MCP Streamable HTTP 与 A2A Agent Card/Task 生命周期兼容；
- 回归：`go build ./...`、`go test ./...`、`go vet ./...`、既有 eval 与 Terminal-Bench 基线不退化。

### 10.2 自举验收

**Mission 1（构建 OS）** 必须证明：新 OS 能启动、注册 Worker、创建 Mission、分配 worktree、渲染 UI，且 build/test/vet/相关 eval/smoke test 均产生 Evidence。

**Mission 2（递归证明）** 是一项事先冻结、未参与 Mission 1 构建过程的复杂 coding 需求。新 OS 必须自行规划、委派、开发、测试、审查与验证；其验收测试必须通过，且不得由生成代码的 Worker 自评。

## 11. 风险与停止条件

停止或重新评估 Agent OS 投入的条件：

- Mission Store 无法在不显著耦合现有 Engine 的前提下实现可靠恢复；
- 远程 Agent 失联导致大量 `indeterminate`，人工恢复成本超过协作收益；
- 任务依赖和 worktree 管理无法稳定支持并发开发；
- MCP/A2A 接入只增加复杂度，未带来可验证的协作能力；
- Self-Hosting Mission 只能完成自身构建，不能通过独立 Mission 验收；
- 性能或依赖体积违背 harness9 轻量、local-first 的核心定位。

## 12. 后续

本设计获得审核后，下一步是生成详细实施计划：按 v1.1 Mission Foundation 拆成可独立验证的代码任务、迁移、测试和接口契约；不在本规范阶段写入实现代码。
