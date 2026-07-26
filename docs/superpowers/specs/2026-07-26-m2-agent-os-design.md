# harness9 M2：本地 Agent OS 设计

**状态：已确认，待实施计划**
**日期：2026-07-26**

## 1. 决策摘要

M2 将 harness9 从单次 Agent 运行时升级为 local-first 的 Agent OS。首个目标不是通用工作流平台或 Agent Marketplace，而是让一组本地、无状态的通用 Agent 在可恢复、可审计、可隔离的环境中完成一个跨多个包、带测试和文档的新功能。

系统采用 **Mission Control Plane + Coordinator Agent**：Coordinator 使用 LLM 理解目标并提出计划或调度建议；Go Control Plane 保存状态、执行状态机、分配资源并强制策略。所有代码改动在独立 worktree 和 Sandbox 中完成，通过显式集成任务汇合，最终由独立验证证据而非 Agent 文字结论判定完成。

一期提供基础本地 GUI 控制台和 TUI 入口，满足查看、编辑 Plan、审批、暂停、恢复和资源管理；它们共享 Control Plane，不直接操作数据库或绕过权限策略。

## 2. 范围与非目标

### 2.1 M2 范围

- 持久化 Mission、Plan、Task、Attempt、Lease、Artifact、Evidence 和审批记录；
- 可编辑、版本化的 Plan，并在执行期通过人工审批的 Plan Change Request 修改；
- Coordinator 驱动、确定性 Scheduler 执行的本地通用 Worker 集群；
- 每个代码 Task 独占 worktree、分支和 Sandbox；
- 独立集成 Task 与独立验证 Task；
- 进程重启后的任务、租约和证据恢复；
- 本地 Dashboard 与 TUI 的基础管理能力；
- 一个冻结的跨包 Feature Mission，用代码、测试、文档和验证证据完成端到端验收。

### 2.2 非目标

- 不引入通用 DAG DSL、通用 workflow 平台或自由 Agent Swarm；
- 不实现公网 Agent Marketplace、多租户云控制面或自动发现外部 Agent；
- 不让 Agent 点对点共享对话、直接调度其他 Agent 或直接合并代码；
- 不在一期开放局域网/公网 Dashboard；
- 不以 MCP/A2A 联邦 Worker 阻塞本地执行闭环；
- 不废弃现有 `Run`、`RunStream`、TUI、Skills、Sandbox、MCP Client 或 `/autodev`。

## 3. 架构与职责

```text
用户 / GUI / TUI
       │ semantic commands + event subscription
       ▼
Mission Control Plane ──────────────── Evidence / Artifact Store
       ▲                                           ▲
       │ structured proposals                      │ manifests and results
Coordinator Agent                                  │
       │                                           │
       ▼                                           │
Deterministic Scheduler ──► generic local Workers ┘
       │                         │
       └──► worktree lease + Sandbox lease
```

### 3.1 Coordinator Agent

Coordinator 是可替换、可重启的 LLM Agent。它没有持久化私有记忆，也不拥有调度特权。它只能提交结构化意图：

- `submit_plan_draft`；
- `request_plan_change`；
- `recommend_dispatch`；
- `request_retry`；
- `request_verification`。

Coordinator 读取 Mission、Plan、Task、Artifact、Evidence 和事件的受控视图。Control Plane 校验其意图；无效、越权或状态不允许的意图不会改变运行时状态。

### 3.2 Control Plane 与 Scheduler

Control Plane 是唯一事实来源，保存于本地 SQLite `state.db`，并使用事务保护状态转换、Lease 分配、计划版本切换和审批命令。Scheduler 是普通 Go 运行时组件，不使用 LLM 决定安全关键状态。

Scheduler 只对依赖已满足、预算可用、策略允许且无资源冲突的 Task 分配 Worker。它强制全局和单 Mission 并发配额、工作区排他性、Sandbox 生命周期和重试上限。

### 3.3 无状态通用 Worker

所有 Worker 使用同一种通用 Agent 定义。每次 Attempt 只接收 Task Contract：目标、输入 Artifact、验收条件、允许工具、预算和 Lease 路径。Worker 不读取其他 Worker 的对话历史或可写工作区；完成时只提交结构化 Artifact Manifest、事件和结果。

Task 的行为模式由 Contract 描述，而非由 Agent 人格描述：例如实现、测试修复、集成、审查、文档或验证。这样 Agent 可以替换、重启和横向扩展，长期事实始终留在 Control Plane。

## 4. 领域对象与状态机

| 对象 | 职责 |
|---|---|
| Mission | 用户目标、冻结验收合同、预算、Policy、全局生命周期 |
| Plan / PlanVersion | Task 图的草案和已批准快照 |
| PlanChangeRequest | 执行期新增任务、依赖变化或权限/预算变化的人工审批请求 |
| Task | 输入合同、依赖、资源边界、验收条件和重试规则 |
| TaskAttempt | 一次 Worker 执行，关联 Agent、Lease、事件和产物 |
| WorkspaceLease | Task 独占的 worktree、分支、Sandbox、期限和清理状态 |
| Artifact | commit、diff、文档、报告和其他可交接产物，按内容摘要固定 |
| Evidence | 独立 build/test/vet/review/smoke 结果，按内容摘要固定 |
| Policy | 工具范围、预算、审批、取消、重试和集成规则 |

Mission 状态：

```text
draft → planning → awaiting_plan_approval → ready → running → verifying
                                                   ├→ succeeded
                                                   ├→ failed
                                                   ├→ needs_attention
                                                   └→ cancelled
```

Task 状态：

```text
blocked → queued → leased → running → verifying → succeeded
                                     ├→ failed
                                     ├→ awaiting_input
                                     └→ indeterminate
```

关键不变式：

- Coordinator 只能提议，Scheduler/Control Plane 才能调度；
- Worker 只能提交产物，不能标记 Mission 成功；
- Verifier 才能基于 Evidence 推进最终验收；
- 已批准 Plan 不可原地修改；
- 代码 Task 必须持有独占 WorkspaceLease；
- `indeterminate` 必须先对账 Git、Lease 与 Artifact，禁止盲目重试。

## 5. Plan 治理和人工审批

Planner 先提交可编辑的 `Plan v1`。用户可以编辑 Task、依赖、验收条件、预算和允许的 Agent 能力。批准后，Plan 形成不可变快照并成为唯一可调度版本。

执行期间，Coordinator 发现缺失工作、需要新增任务、改变依赖、扩大工具权限或扩大预算时，必须创建 `PlanChangeRequest`。请求包含原因、影响任务、权限变化、预算变化、触发 Attempt 和建议的新 Plan。用户批准后生成新的 Plan Version；系统不就地改写正在执行的版本。用户可以驳回并附原因，相关 Task 进入 `awaiting_input` 或按既有合同继续执行。

在批准 Task Contract 的边界内，Worker 可自动重试和修复测试。超过该边界的行动必须等待人工审批。

## 6. 调度、隔离与集成

Scheduler 以 Wave 方式从 `queued` Task 中选择依赖已满足的任务。每一个写代码 Task 获得独占 worktree、分支和 Sandbox；测试、审查、文档和验证任务同样在自己的 Lease 中执行。并行只发生在没有共享可写工作区的 Task 之间。

Worker 交付 commit/diff、测试结果、文档和 Artifact Manifest。Control Plane 校验 Manifest 后将 Task 推入 `verifying`。当实现 Task 的依赖满足时，显式的 Integration Task 在专属集成 worktree 中按确定性规则合并产物、解决冲突并运行联合测试。Coordinator 不能直接向集成分支写入。

失败处理如下：

| 情况 | 处理 |
|---|---|
| 可重试错误、工具错误或既有范围内的测试失败 | 在重试预算内创建新 Attempt |
| Worker 发现缺失工作 | 创建 PlanChangeRequest 并等待审批 |
| 新依赖、权限或预算扩大 | 不调度，等待审批 |
| 进程、Sandbox 或本地运行时中断 | 标记 `indeterminate`，对账后恢复或创建新 Attempt |
| 集成冲突 | 产生集成失败 Evidence；Coordinator 可提出修复或计划变更 |
| 验收不通过 | 产生失败 Evidence；既有范围内可创建修复 Attempt，超出范围则请求变更 |

## 7. GUI、TUI 与 API 边界

一期 Dashboard 是本地运维控制台，默认只监听 `127.0.0.1`，由 Go `net/http` 提供。使用 SSE 订阅 Mission、Task、Agent、Sandbox、审批和 Evidence 事件；一期不引入 WebSocket 或外部前端运行时。

首版界面包括：

- Mission 列表和 Mission 总览；
- Task Graph、Attempt、Agent、Sandbox、Lease、Artifact 和 Evidence 详情；
- Plan Draft 编辑器、版本历史和 Change Request 审批；
- 暂停、取消、重试、恢复等基础操作；
- 待审批队列和高风险操作的影响说明。

既有 TUI 增加 Mission 入口和审批提示，但保留聊天体验。Dashboard 和 TUI 只能调用 Control Plane Command Service，例如 `approve_plan`、`approve_change_request`、`pause_mission`、`retry_task`。每个命令记录操作者、理由、关联对象、时间和幂等键；UI/TUI 不直接写数据库、启动 Worker 或操作 worktree。未来远程访问必须作为独立配置并增加认证和 CSRF 防护。

设计原型位于本地 `.superpowers/brainstorm/`，仅作设计参考，不是发布工件。

## 8. Artifact、Evidence 与验收

Worker Attempt 只提交 Artifact Manifest：commit/diff、文件清单、测试命令与摘要、文档路径和上游 Artifact 引用。Control Plane 为内容生成摘要并固定版本。

独立 Verifier 在新的 worktree/Sandbox 中，按照 Mission 的冻结验收合同复跑：

- 编译、单包测试、全量测试和 `go vet`；
- Feature 验收测试和 smoke test；
- 文档存在性、链接和命令示例检查；
- 集成分支 commit、diff 与依赖一致性检查；
- 独立审查结论、阻断项和修复证明。

Verifier 不得验证由自己实现的产物。Agent 的完成文本只是候选信号，不能构成成功证据。Mission 只有在 required Task 成功或被显式豁免、集成通过、所有 required Evidence 通过且不存在未决高风险审批时，才能进入 `succeeded`。

## 9. 实施分期与验收

### 9.1 分期

1. **Mission Foundation**：SQLite Store、对象/状态机、Plan Version、Change Request、事件审计和 Command Service。
2. **本地执行闭环**：Scheduler、通用 Worker Adapter、worktree/Sandbox Lease、Artifact/Evidence 和崩溃对账恢复。
3. **冻结 Feature Mission**：用一个跨多个包、带测试和文档的新功能验证计划、并行实现、集成和验证闭环。
4. **基础控制台**：Mission Dashboard、Plan 编辑/审批、Task/资源/Evidence 查看和基础人工管理。
5. **硬化与回归**：状态机、Lease、SQLite 恢复、Sandbox/Worker mock、端到端 Mission，以及既有 build/test/vet/eval 回归。

### 9.2 M2 完成标准

M2 完成时必须证明：

- 进程重启后 Mission 可恢复，且不会重复执行无法确认副作用的 Attempt；
- 并行代码 Task 不共享可写 worktree 或 Sandbox；
- 未批准的 Plan Change Request 不会被调度；
- 验证失败或缺失 Evidence 不能将 Mission 标记成功；
- 冻结 Feature Mission 能交付跨包代码、测试、文档、集成提交和独立 Evidence；
- Dashboard 与 TUI 显示相同的 Control Plane 状态，基础管理命令通过统一 Policy 生效；
- `go build ./...`、`go test ./...`、`go vet ./...` 和既有 eval 不退化。

## 10. 后续

本设计确认后，应先为 Mission Foundation 编写逐文件、逐测试、逐迁移的实施计划。MCP Server、A2A Client/Server、外部 Agent Card 和联邦 Worker 属于本地闭环获得验证后的后续里程碑。
