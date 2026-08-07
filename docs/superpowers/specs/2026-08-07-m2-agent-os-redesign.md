# harness9 M2：本地 Agent OS 重做设计

**状态：已确认，待实施计划**
**日期：2026-08-07**
**前置设计：`docs/superpowers/specs/2026-07-26-m2-agent-os-design.md`（复盘后重做）**
**关联 Milestone：GitHub Milestone #2 `M2 · 打造本地 Agent OS - 多 Agent 操作系统`**

---

## 0. 复盘与重做动机

### 0.1 上一轮（2026-07-26 设计 + PR #89-94）教训

| 教训 | 说明 |
|------|------|
| 6 PR 链式依赖、一口气 ~5600 行 | 无法增量合并/回滚；任一 PR 出问题整链阻塞；review 负担过重 |
| 批量关闭无技术反馈 | 6 个 PR 在 2026-07-29 批量关闭、零 review comment--问题在流程（工具链迁移清理）不在代码 |
| Phase 1 未完成就做 Phase 2-3 | Store 缺 Plan/Policy/Events/CommandService，上层却已建 Scheduler/Worker |
| 合成演示无法验证真实价值 | "冻结 Feature Mission"是自验证，不证明日常可用 |

### 0.2 本轮改进

- **智能路由**：简单任务走 Fast Lane 零摩擦，复杂任务自动拆分为多 Task Mission（用户明确要求）
- **垂直切片交付**：每片端到端可独立合并，绝不链式阻塞
- **先补完 Foundation 再建上层**：Plan/Policy/CommandService/Events 先行
- **全量 M2**：覆盖 6 大产品目标（runtime / coordination / workspace / memory / eval / operator）

---

## 1. 决策摘要

M2 将 harness9 从单次 Agent 运行时升级为 local-first 的 Agent OS。核心是**统一运行时 + 升级路由器**架构：

- **Router（升级路由器）** 评估任务复杂度（启发式 + 可选 LLM triage），路由到两条车道
- **Fast Lane**：现有 `engine.Run/RunStream` + TUI 流，**零改动**，简单任务零摩擦
- **Deep Lane（Mission Control）**：Coordinator 草拟 Plan -> 确定性 Scheduler 调度 -> 通用 Worker 在隔离 worktree/Sandbox 执行 -> 独立 Verifier 证据验收 -> Integration Task 汇合 -> Mission 自动完成

所有代码改动在独立 worktree 和 Sandbox 中完成，通过显式集成任务汇合，最终由独立验证证据而非 Agent 文字结论判定完成。一期提供 TUI Mission 视图和本地 Dashboard，满足查看、编辑 Plan、审批、暂停、恢复和资源管理；两者共享 Control Plane，不直接操作数据库或绕过权限策略。

---

## 2. 范围与非目标

### 2.1 M2 范围（6 大产品目标）

1. **Multi-Agent runtime** - 角色、能力、隔离边界、Task 图、前台/后台执行、取消、重试、可恢复状态
2. **Coordination plane** - 共享 Task Contract、依赖感知调度、事件路由、冲突感知文件归属、人工升级
3. **Local-first workspace OS** - worktree/session/sandbox 生命周期、持久化产物、资源配额、审计日志、secure-by-default
4. **Memory and knowledge plane** - project/user/agent/mission 四级作用域、provenance、检索、consolidation、aging、显式用户控制
5. **Evaluation-driven evolution** - 可重复 SWE-bench / Terminal-Bench 运行、回归仪表盘、trace-to-failure 分析、benchmark 门控
6. **Operator experience** - TUI/CLI/浏览器视图查看 agents、tasks、approvals、sandboxes、cost/tokens、traces、长时任务

### 2.2 非目标

- 不引入通用 DAG DSL、通用 workflow 平台或自由 Agent Swarm
- 不实现公网 Agent Marketplace、多租户云控制面或自动发现外部 Agent
- 不让 Agent 点对点共享对话、直接调度其他 Agent 或直接合并代码
- 不在一期开放局域网/公网 Dashboard
- 不以 MCP/A2A 联邦 Worker 阻塞本地执行闭环
- 不废弃现有 `Run`、`RunStream`、TUI、Skills、Sandbox、MCP Client 或 `/autodev`

---

## 3. 整体架构与智能路由器

### 3.1 双车道架构

```
用户输入 (TUI/CLI)
     |
     v
+---------------------------------------------+
|           Router (升级路由器)                 |
|  启发式 + 可选 LLM triage -> simple|complex    |
+----------+------------------+---------------+
           | simple            | complex
           v                   v
   +---------------+   +------------------------------+
   |  Fast Lane    |   |  Deep Lane (Mission Control)  |
   | engine.Run/   |   |  Coordinator -> Plan ->       |
   | RunStream     |   |  Scheduler -> Workers ->      |
   | (现有代码不改) |   |  Integration -> Verifier      |
   +---------------+   +------------------------------+
```

**核心原则**：Fast Lane 是现有 `engine.Run/RunStream` + TUI 流，零改动。Deep Lane 是新建的 Mission Control，建在现有 `subagent.Runner` + `sandbox.Manager` 之上。两条车道共享 Provider、Tools、Sandbox、Memory 基建。

### 3.2 Router 决策逻辑

Router 是新核心件，三路决策：

| 信号 | 判定 | 去向 |
|------|------|------|
| `/mission <goal>` 显式前缀 | 强制 Deep | Deep Lane |
| 启发式命中复杂信号（多文件/跨包/"重构"/"实现 X 并测试并文档"） | 疑似复杂 | LLM triage |
| 启发式未命中 | 简单 | Fast Lane |
| LLM triage 判定可分解 | complex | Deep Lane（Coordinator 草拟 Plan） |
| LLM triage 判定单一 | simple | Fast Lane |

**非破坏性升级**：
- Deep 任务若实际简单，Coordinator 产出单 Task Plan（退化为带审计的 fast path）
- Fast 任务变复杂时，用户 `/escalate` 可转为 Mission（携带当前对话上下文作为 Mission Goal 的一部分）
- Router 的 LLM triage 失败时 fail-open 到 Fast Lane（绝不因路由故障阻塞用户）

### 3.3 Coordinator 角色

Router 的 LLM triage 即 Coordinator 的入口职能。Coordinator 是可替换、可重启的 LLM Agent，全生命周期三个职能：

1. **Triage**（Router 入口）：simple/complex 分类
2. **Decompose**：complex 时草拟 Plan（Task 图 + 依赖 + 验收条件 + 预算），提交 `submit_plan_draft`，不直接调度
3. **Monitor**：执行期观察进度，发现缺失工作/需扩权时提交 `request_plan_change`，等待人工审批

Coordinator 无持久私有记忆、无调度特权，只提交结构化意图；Control Plane 校验并执行。

### 3.4 与现有系统的关系

| 现有组件 | M2 中的角色 |
|---------|------------|
| `engine.Run/RunStream` | Fast Lane 主体，不改 |
| `subagent.Runner` | Deep Lane Worker 的执行内核（每个 Task Attempt = 一个隔离子引擎） |
| `sandbox.Manager` | Worker 的 OS 级隔离（每 Task 独占容器） |
| `planning.TodoStore` | Fast Lane 内轻量计划；Deep Lane 用 Mission Plan（更重，版本化） |
| `internal/mission`（已有 Phase 1） | 补完为完整 Control Plane Store |
| `ltm`/`memory` | Memory & Knowledge Plane 基建，Deep Lane 共享 |
| `evals` | Eval-driven evolution 切片复用 |
| TUI | 增加 Mission 视图（复用现有 Bubbletea 基建） |

---

## 4. 领域模型与状态机

### 4.1 领域对象全景（补完 Phase 1）

现有 `internal/mission` 仅有 Mission/Task/TaskAttempt/Artifact/Evidence。下表列出完整对象，标注新增与增强：

| 对象 | 状态 | 职责 |
|------|------|------|
| Mission | 增强 | 增加 `PolicyJSON`、`AcceptanceContract`（冻结验收合同）、`CurrentPlanVersion` |
| Plan / PlanVersion | 新增 | Task 图的草案与已批准不可变快照，版本化 |
| PlanChangeRequest | 新增 | 执行期变更请求（新增 Task/改依赖/扩权/扩预算），需人工审批 |
| Task | 增强 | 增加 `PlanVersionID`、`ContractKind`、`Input`(Contract)、`Acceptance`、`Budget`、`AllowedTools`、`MaxRetries` |
| TaskAttempt | 增强 | 增加 `LeaseID`、`ExitReason`、`StartedAt`/`FinishedAt`（崩溃对账用） |
| WorkspaceLease | 新增 | Task 独占的 worktree + branch + sandbox container + 期限 + 清理状态（表已存在，补 Go 结构） |
| Artifact | 完备 | 已完备（append-only，SHA256 固定） |
| Evidence | 完备 | 已完备（append-only，immutable trigger 已有） |
| Policy | 新增 | 每 Mission 并发上限、工具范围、预算、审批/取消/重试/集成规则 |
| AuditEvent | 新增 | append-only 事件流（操作者/动作/目标/原因/时间/幂等键），供审计与 Dashboard SSE |

### 4.2 Task Contract（合同驱动行为）

Task 的行为由 Contract 描述，而非 Agent 人格。这吸收了已关闭 PR #90/#91 的有效设计：

```go
type ContractKind string
const (
    ContractImplementation ContractKind = "implementation"
    ContractVerification   ContractKind = "verification"
    ContractIntegration    ContractKind = "integration"
)

type TaskInput struct {
    Kind             ContractKind
    Goal             string
    DependsOn        []string
    Acceptance       []string
    AllowedTools     []string
    Budget           Budget
    MaxRetries       int
    SettingsPath     string
}
```

三种 ContractKind 路由到不同 Dispatcher（实现/验证/集成），Scheduler 自身对 Contract 类型保持无感。

### 4.3 状态机

**Mission 状态**（现有 9 态，补全转换规则）：

```
draft --(submit plan draft)--> planning --(approve)--> ready --(dispatch)--> running
                                  |                                          |
                                  |(reject)                                  |(all tasks done)
                                  v                                          v
                             awaiting_input                           verifying --(evidence pass)--> succeeded
                                                                                  |
planning/ready/running/verifying --(operator)--> cancelled   needs_attention <__(evidence/integration fail)__|
needs_attention --(operator resolve)--> running/verifying/failed
任何非终态 --(exhausted)--> failed
```

**Task 状态**（现有 9 态，关键不变式强化）：

```
blocked --(deps satisfied)--> queued --(lease)--> leased --(worker start)--> running
                                                                          |
                    +-----------------------------------------------+
                    v                v                  v              v
               verifying        failed         awaiting_input   indeterminate
                    |                                |                |
           +--------+-----+                     (human)          (reconcile)
           v              v                                       |
      succeeded    failed/awaiting_input                           v
                                                             queued(新 attempt)/failed
```

### 4.4 关键不变式（验收门控）

1. Coordinator 只能提议，Scheduler/Control Plane 才能调度
2. Worker 只能提交 Artifact，不能标记 Task/Mission 成功
3. 只有 Verifier 能基于 Evidence 推进最终验收
4. 已批准 Plan 不可原地修改（只能新建版本）
5. 代码 Task 必须持有独占 WorkspaceLease（DB 唯一索引 `idx_workspace_leases_active_task` 已保证）
6. `indeterminate` 必须先对账 Git/Lease/Artifact，禁止盲目重试
7. Mission 进入 `succeeded` 的充要条件：所有 required Task 成功或显式豁免 + 集成通过 + required Evidence 全通过 + 无未决高风险审批

### 4.5 Store 演进策略

现有 `internal/mission/store.go` 的 schema 基础保留，通过增量 migration 补完：

- 新增表：`plans`、`plan_versions`、`plan_change_requests`、`policies`、`audit_events`
- 增强 `tasks`：加列 `plan_version_id`、`contract_kind`、`input_json`、`budget_json`、`max_retries`
- 增强 `task_attempts`：加列 `lease_id`、`exit_reason`、`started_at`、`finished_at`
- 现有 `workspace_leases` 表补 Go 结构与方法
- 所有 migration 幂等（`CREATE TABLE IF NOT EXISTS` / `ALTER TABLE ... ADD COLUMN` 带存在性检查）

---

## 5. 执行闭环（Scheduler + Worker + Lease + 崩溃恢复）

### 5.1 包结构

```
internal/
├── scheduler/          # 确定性调度器（LLM-free）
│   ├── dispatcher.go   # Dispatcher 接口 + RoutingDispatcher（按 ContractKind 路由）
│   ├── scheduler.go    # 主调度循环 + 并发控制 + 崩溃恢复
│   └── *_test.go
├── worker/             # 实现类 Task 的 Worker Adapter
│   ├── adapter.go      # 实现 scheduler.Dispatcher
│   ├── worktree.go     # git worktree + branch 生命周期
│   ├── contract.go     # ImplementationContract 构建 + ParseResult
│   └── *_test.go
├── verifier/           # 验证类 Task 的 Adapter（第 6 节）
└── integration/        # 集成类 Task 的 Adapter（第 6 节）
```

### 5.2 Dispatcher 接口与路由

```go
type Dispatcher interface {
    Dispatch(ctx context.Context, task mission.Task, attempt mission.TaskAttempt) (Result, error)
}

type RoutingDispatcher struct {
    impl map[mission.ContractKind]Dispatcher
}
```

Scheduler 只调用 `RoutingDispatcher.Dispatch`，不关心是 implementation/verification/integration。新增 Contract 类型只需注册新 Dispatcher，开闭原则。

### 5.3 Scheduler 主循环（确定性，无 LLM）

```
loop (tick 或事件触发):
  1. ListSchedulableTasks: 跨 Mission 查询 queued + 依赖已满足 + 属于当前已批准 PlanVersion
  2. ActiveTaskCounts: 全局 + 每 Mission 在途 Task 数
  3. 对每个候选 Task 检查: 全局并发 < cap? Mission 并发 < policy? 无 worktree 冲突? 预算可用?
  4. 合格则: MarkMissionRunning(幂等) -> StartAttempt -> acquire Lease -> Task: queued->leased->running
  5. 异步调用 RoutingDispatcher.Dispatch(attempt)，返回后:
     - 成功: 记录 Artifact -> Task running->verifying
     - 可重试失败: 新建 Attempt（重试预算内）
     - 超预算/越权: Task->awaiting_input
     - 中断: Task->indeterminate（等崩溃对账）
```

触发机制：事件驱动（Task 状态变更 / 审批完成）+ 定时 tick（崩溃恢复兜底），非忙轮询。

### 5.4 Worker Adapter（实现类 Task）

每次 Attempt 独占一个执行环境，复用现有 `subagent.Runner` + `sandbox.Manager`：

```
WorkerAdapter.Dispatch(task, attempt):
  1. CreateWorktree(repo, ".missions/<mid>/<tid>/<aid>") + branch "mission/<mid>/<tid>/<aid>"
  2. sandbox.Manager.Create() -> 独占容器（bind mount worktree）
  3. 构建 ImplementationContract prompt (Goal + 验收 + 允许工具 + 预算 + 依赖 Artifact 引用)
     权限: 无 SettingsPath 时自动生成临时白名单（修复 PR #94 发现的 bug）
  4. subagent.Runner.RunStream(ctx, contract)  # background=true, 隔离 Session
  5. ParseResult: 解析 sub-agent 输出 -> commit SHA / diff / 文件清单 / 测试摘要
  6. AddArtifact(Manifest)  # SHA256 固定
  7. RemoveWorktree + sandbox.Destroy（finally，无论成功失败）
  8. 返回 Result{AttemptStatus, Evidence hints}
```

Worker 只读依赖 Task 的 Artifact 摘要，不读其他 Worker 对话历史或可写工作区。

### 5.5 WorkspaceLease 生命周期

- DB 唯一索引 `idx_workspace_leases_active_task` 保证一 Task 一活跃 Lease
- Lease 携带 `expires_at`，Scheduler tick 扫描过期 Lease -> 标记 expired -> Task 进 indeterminate
- 容器孤儿回收复用现有 `sandbox.Manager.ReapOrphans`（启动时 + 定期）

### 5.6 崩溃恢复（重启对账）

进程重启后 `Scheduler.Reconcile()`：

```
对每个 running 状态的 Attempt（进程已不在）:
  1. 检查 worktree git status:
     - 有干净 commit 且 SHA 可确认? -> Attempt 成功, Task->verifying
     - 有未提交改动? -> indeterminate
     - worktree 不存在? -> indeterminate
  2. 检查 sandbox 容器状态: 仍 running? -> Destroy + indeterminate; 已退出? -> 按上一步判定
  3. indeterminate 的 Task: 重试预算内->新建 Attempt（不重复不可确认侧效）; 超预算->failed
  4. GC: 释放所有 expired/abandoned Lease
```

核心安全属性：绝不盲目重试可能有未确认副作用的 Attempt（对账优先）。

---

## 6. 验收与集成（Verifier + Integration + Mission 自动完成）

### 6.1 核心原则：证据驱动验收

Agent 的完成文本只是候选信号，不能构成成功证据。只有独立 Verifier 产生的 Evidence 才能推进最终验收。

### 6.2 Verifier Adapter（`internal/verifier`）

验证类 Task 在全新的 worktree/Sandbox 中复跑，绝不验证自己实现的产物：

```
VerifierAdapter.Dispatch(task, attempt):
  1. 取被验证 implementation Task 的最新 Attempt Artifact（commit SHA）
  2. CreateWorktree + checkout 该 commit（只读视角）
  3. sandbox.Manager.Create() 独占容器
  4. 按冻结验收合同逐项复跑，每项产出一条 Evidence:
     build / unittest / testall / vet / feature / smoke / docs / review
  5. AddEvidence 幂等（同 attempt+kind+digest 去重）
  6. required Evidence 全 passed? -> Task verifying->succeeded; 否 -> failed（预算内可建修复 Attempt）
  7. 清理 worktree + sandbox
```

Verifier 不可提交 Artifact，只提交 Evidence。接口层面强制：VerifierAdapter 不持有 AddArtifact 能力。

### 6.3 Integration Adapter（`internal/integration`）

集成类 Task 在专属 Mission 级 worktree 中按确定性规则汇合多个实现 Task 的产物：

```
IntegrationAdapter.Dispatch(task, attempt):
  1. CreateWorktree(mission 级): ".missions/<mid>/integration/"
  2. 依次 MergeBranch(每个依赖 Task 的分支):
     - 无冲突: fast-forward 或 merge commit
     - 冲突: 尝试确定性自动解决（如 import 排序），失败则产出 integration_fail Evidence + 中止
     - 每次合并后: go build ./... 增量校验
  3. 全部合并后跑联合测试: go test ./... + go vet ./...
  4. 产出 Evidence: integration_merge / integration_build / integration_test
  5. 全 passed? -> Integration Task succeeded -> 触发 Mission 自动完成
     否 -> integration_fail Evidence, Mission->needs_attention
  6. 清理
```

Coordinator 不能直接向集成分支写入；集成工作区由 IntegrationAdapter 独占。

### 6.4 ContractKind 路由汇总

| ContractKind | Dispatcher | 产出 | 可推进验收? |
|-------------|-----------|------|-----------|
| implementation | WorkerAdapter | Artifact | 否（只候选） |
| verification | VerifierAdapter | Evidence | 是 |
| integration | IntegrationAdapter | Evidence | 是（Mission 级） |

### 6.5 Mission 自动完成机制

```
TransitionTask(succeeded) 时 tryCompleteMission(missionID, planVersion):
  1. 查当前 PlanVersion 下所有 Task 状态
  2. 全部 succeeded? 否->return
  3. 有 integration Task 且未 succeeded? -> return（等集成）
  4. Mission: running->verifying（第一跳）
  5. required Evidence 全 passed + 无未决高风险审批?
     是 -> Mission: verifying->succeeded（第二跳）
     否 -> Mission: verifying->needs_attention
```

无 Integration Task 的 Mission（只有 impl+verification）也能自动收尾。`MarkMissionNeedsAttention` 幂等升级，供集成/验收失败时使用。

### 6.6 失败处理矩阵

| 情况 | 处理 |
|------|------|
| 可重试错误/工具错误/范围内测试失败 | 重试预算内新建 Attempt |
| Worker 发现缺失工作 | PlanChangeRequest + 等审批 |
| 新依赖/扩权/扩预算 | 不调度，等审批 |
| 进程/Sandbox/运行时中断 | indeterminate -> 对账恢复或新 Attempt |
| 集成冲突 | integration_fail Evidence -> needs_attention |
| 验收不通过 | fail Evidence -> 范围内建修复 Attempt，超范围请求变更 |

---

## 7. Plan 治理与人工审批 + Command Service

### 7.1 Plan 生命周期与版本化

```
Coordinator submit_plan_draft
    |
    v
Plan v1 (draft, 可编辑)
    |
用户编辑（Task/依赖/验收/预算/工具）
    |
approve_plan -----------> PlanVersion v1 (不可变快照, 唯一可调度版本)
    |                              |
reject_plan                   执行期 Coordinator 发现:
    |                              |
awaiting_input              request_plan_change
                                 |
                           用户 approve --> PlanVersion v2 (supersedes v1)
                           用户 reject  --> 继续按 v1 / awaiting_input
```

关键规则：
- 已批准 PlanVersion 不可原地修改，只能新建版本
- 同一时刻一个 Mission 只有一个 active PlanVersion（可调度版本）
- 版本切换时，v1 下 in-flight 的 Task 按策略处置：继续完成 / 取消重建 / 挂起待人工决定（Policy 指定）
- draft 状态的 Plan 可无限编辑，不触发调度

### 7.2 PlanChangeRequest 结构

```go
type PlanChangeRequest struct {
    ID              string
    MissionID       string
    Reason          string
    TriggerAttemptID string
    AffectedTasks   []string
    AddedTasks      []TaskInput
    DependencyDelta map[string][]string
    PermissionDelta []string
    BudgetDelta     *Budget
    ProposedPlan    Plan
    Status          ChangeRequestStatus  // pending/approved/rejected
    ReviewedBy      string
    ReviewedAt      *time.Time
    ReviewReason    string
}
```

越权即拦截：Worker/Coordinator 在已批准 Task Contract 边界内可自动重试和修复测试；超出边界（新增依赖、扩权、扩预算）必须创建 ChangeRequest 等待审批，Scheduler 不调度越权 Task。

### 7.3 Command Service（唯一状态变更入口）

UI/TUI/Dashboard/Coordinator 不直接写数据库，只能通过 Command Service 提交命令。每个命令：校验状态 -> 事务执行状态转换 -> 记录 AuditEvent -> 返回结果。

```go
type CommandService struct { store *Store }

type CommandKind string
const (
    CmdSubmitPlanDraft   CommandKind = "submit_plan_draft"
    CmdApprovePlan       CommandKind = "approve_plan"
    CmdRejectPlan        CommandKind = "reject_plan"
    CmdRequestPlanChange CommandKind = "request_plan_change"
    CmdApproveChange     CommandKind = "approve_change_request"
    CmdRejectChange      CommandKind = "reject_change_request"
    CmdPauseMission      CommandKind = "pause_mission"
    CmdResumeMission     CommandKind = "resume_mission"
    CmdCancelMission     CommandKind = "cancel_mission"
    CmdRetryTask         CommandKind = "retry_task"
    CmdEscalateToMission CommandKind = "escalate_to_mission"
    CmdExemptTask        CommandKind = "exempt_task"
)
```

### 7.4 AuditEvent（append-only 审计流）

```go
type AuditEvent struct {
    ID             string
    MissionID      string
    CommandKind    string
    Actor          string   // operator / coordinator / system
    Target         string
    Reason         string
    IdempotencyKey string   // 幂等去重
    Result         string   // applied / rejected
    BeforeState    string
    AfterState     string
    CreatedAt      time.Time
}
```

- 幂等：相同 IdempotencyKey 的重复命令只生效一次（防 UI 重试/网络重放）
- Dashboard SSE 订阅 AuditEvent 流，实时推送状态变更

### 7.5 权限模型分层

| 主体 | 能做 | 不能做 |
|------|------|--------|
| Coordinator (LLM) | 提交 plan draft / change request / retry 建议 | 直接调度、写 DB、合并代码、标记成功 |
| Worker (sub-agent) | 在 Contract 边界内执行 + 重试 + 提交 Artifact | 越权操作、读其他 Worker 工作区、标记成功 |
| Verifier | 提交 Evidence | 提交 Artifact、验证自己产物 |
| Operator (人) | 审批/暂停/取消/重试/豁免/升级 | 绕过 Command Service 直写 DB |
| Scheduler (Go) | 分配 Lease、执行状态机、强制并发/预算 | 使用 LLM 决定安全关键状态 |

---

## 8. Memory & Knowledge Plane

### 8.1 设计原则：扩展现有 ltm，而非重建

现有 `internal/ltm` 已有：SQLite `long_term_memories` + FTS5 `memories_fts`、MEMORY.md 物化视图、Extractor（压缩前提取）、去重/强化/陈旧识别/软删除。M2 Memory Plane 在此基础上增加 scope + provenance + mission 生命周期，不重建。

### 8.2 四级作用域

| 作用域 | 生命周期 | 存储 | 注入对象 |
|--------|---------|------|---------|
| Project | 跨所有 Mission，绑 workDir | 现有 ltm Store + MEMORY.md | 所有 Agent |
| User | 跨项目，绑 ~/.harness9/ | ltm Store（scope=user） | 所有 Agent |
| Mission | Mission 期间；成功晋升，失败归档 | ltm Store（scope=mission, mission_id） | 该 Mission 的 Agent |
| Agent | 跨 Mission，绑 agent 定义 | ltm Store（scope=agent, agent_name） | 同类型 Agent |

### 8.3 Provenance（来源可追溯）

```go
type Entry struct {
    // ...现有字段...
    Scope      string   // project/user/mission/agent
    ScopeRef   string   // mission_id / agent_name / ""
    Provenance Provenance
}

type Provenance struct {
    MissionID  string
    TaskID     string
    AttemptID  string
    AgentName  string
    Confidence float64
    Source     string   // extractor / worker / coordinator / user_explicit
}
```

### 8.4 Mission 记忆生命周期

```
Mission 运行期:
  Worker/Coordinator 写入 mission-scoped memory
  该 Mission 的 Agent 检索时: mission memory + project memory 联合检索
    |
Mission succeeded:
  Consolidate: mission memory 晋升为 project memory
  - 去重（SHA256 + 语义近邻）
  - 合并（同主题多条 -> 一条带合并 provenance）
  - 标注 source=mission_success
    |
Mission failed/cancelled:
  Archive: mission memory 保留但 scope 降级为 archived
  - 不晋升（避免污染 project knowledge）
  - 保留 provenance 供复盘
  - 用户可 /memory promote 手动晋升有价值的失败教训
```

### 8.5 检索与注入策略

| 角色 | 注入内容 | 时机 |
|------|---------|------|
| Coordinator | project memory 精华 + mission memory + 相关 user memory | 每次 triage/decompose/monitor |
| Worker | mission memory + 相关 project memory（按 Task goal FTS5 top-K） | 每次 Attempt 构建 Contract 时 |
| Verifier | mission memory + 验收合同相关 project memory | 验证时 |
| Fast Lane | project MEMORY.md 精华（<=5KB） | 每轮 Build()（现有行为不变） |

Token 预算控制：mission/project memory 注入均有 top-K + 字节上限，复用现有 ltm 的 truncateUTF8 + 5KB 限制，防 token bomb。

### 8.6 Consolidation 与 Aging

- 晋升合并（Mission 成功时）：同主题 mission memory 条目 -> LLM 合并为 1 条 project memory
- 强化（现有）：命中检索时 use_count++、last_used_at 更新
- 陈旧识别（现有 StaleCandidates）：长期未命中 + 低置信度 -> 候选清理
- TTL 过期（现有）：PurgeExpired
- 用户显式控制：/memory TUI 命令 -- 按作用域浏览/编辑/删除/晋升/设置 TTL

### 8.7 与现有 ltm 的兼容

- 现有 ltm entry 默认 scope=project，provenance.source=user_explicit/extractor
- MEMORY.md 物化视图只渲染 scope=project 的 top-30（mission/agent scope 不进 MEMORY.md）
- memory_write / memory_search 工具增加可选 scope 参数，默认 project
- Phase 3 接缝（Provider/Embedder/Consolidator 接口）保留，向量检索作为后续增强

---

## 9. Operator Experience（TUI + Dashboard + 审批流）

### 9.1 双入口共享 Control Plane

TUI 与 Dashboard 是两个对等入口，都只能通过 CommandService 变更状态，不直写 DB、不启 Worker、不操作 worktree。两者展示相同的 Control Plane 状态。

### 9.2 TUI Mission 视图（复用现有 Bubbletea 基建）

| 组件 | 说明 |
|------|------|
| /mission 命令 | 进入 Mission 视图（类似 /mcp 面板切换） |
| MissionStatusBar | 状态栏新增段：活跃 Mission 数 + 待审批数（颜色编码） |
| Mission 列表面板 | 所有 Mission 摘要（ID/Goal/状态/进度） |
| Task Graph 面板 | 选中 Mission 的 Task 图（ASCII 树/依赖箭头 + 状态色） |
| Attempt/Lease/Evidence 详情 | 选中 Task 展开详情 |
| 审批队列 | 待审批的 Plan/ChangeRequest，复用现有 5 选项审批对话框 |
| /escalate 命令 | Fast Lane 对话中升级为 Mission |
| /memory 命令 | 按作用域浏览/编辑 memory |

设计约束：Mission 视图不破坏现有聊天体验--默认仍是 Fast Lane 对话，/mission 切换查看，Esc 回对话。Mission 事件以非阻塞 toast 通知。

### 9.3 本地 Dashboard

技术选型：Go net/http + html/template 服务端渲染 + SSE。不引入 WebSocket、SPA 框架、Node 构建链、外部前端运行时。保持 harness9 零前端依赖原则。

| 路由 | 功能 |
|------|------|
| GET / | Mission 列表总览 |
| GET /missions/:id | Mission 详情：Task Graph + Attempt + Lease + Artifact + Evidence |
| GET /missions/:id/plan | Plan Draft 编辑器 + 版本历史 + ChangeRequest 审批 |
| GET /approvals | 待审批队列 |
| GET /events | SSE 流：Mission/Task/Agent/Sandbox/Lease/Approval/Evidence 事件 |
| POST /command | 提交 Command（表单 -> CommandService） |
| GET /agents | Agent/Worker/Sandbox 资源状态 + cost/token |

安全：默认只监听 127.0.0.1，一期不开放局域网/公网。所有写操作走 POST /command（CSRF token + IdempotencyKey）。

### 9.4 审批流

```
需要审批的事件 -> CommandService 产出 pending AuditEvent
  -> TUI toast 通知 + Dashboard /approvals 高亮
  -> 用户在任一入口审批:
     approve(reason) / reject(reason) -> CommandService 执行 -> AuditEvent
  -> SSE 推送结果到所有入口
```

高风险操作（扩权/扩预算/删除 worktree/豁免 required Task）需额外影响说明展示，复用现有 DangerHook 的风险分级。

### 9.5 触发与集成

- TUI 启动时若 state.db 有活跃 Mission -> StatusBar 显示，不自动切走对话
- Dashboard 通过 harness9 dashboard 子命令或 TUI 内 /dashboard 启动
- Coordinator 的 request_plan_change / submit_plan_draft 通过 CommandService 进入审批队列，不自动生效

---

## 10. Eval-driven Evolution + 交付切片 + M2 完成标准

### 10.1 Eval-driven Evolution

复用现有 internal/evals 框架（ScriptedProvider / Assertion / Suite / Hermetic env / 黄金数据集），为 M2 增加 Mission 级 eval：

| Eval 维度 | dataset 文件 | 核心验证点 |
|-----------|-------------|-----------|
| Mission Foundation | mission_foundation_test.go | Plan 版本化、状态机转换、ChangeRequest 审批门控、Command 幂等 |
| Execution Loop | mission_execution_test.go | Scheduler 调度、并发上限、Lease 独占、崩溃对账不盲目重试 |
| Verification | mission_verification_test.go | Evidence 门控、Verifier 不验证自己产物 |
| Integration | mission_integration_test.go | 多 Task 合并、冲突失败 Evidence、Mission 自动完成 |
| Routing | mission_routing_test.go | simple->Fast、complex->Deep、/escalate、triage fail-open |
| Memory | mission_memory_test.go | 作用域隔离、Mission 成功晋升、失败不污染 project |

ScriptedProvider 模拟 Coordinator：按脚本提交 plan_draft / change_request，验证 Control Plane 校验逻辑，不发真实 LLM 调用（Hermetic）。

Benchmark gating：Terminal-Bench（已接入，PR #83）作为回归门控；SWE-bench 集成作为 stretch goal。.github/workflows/eval.yml 扩展：PR 触发 hermetic mission eval，失败阻断合并。

Trace-to-failure：现有 OTEL 链路扩展 Mission 维度：harness9.mission -> task -> attempt -> llm/tool，AuditEvent 与 Span 关联。

### 10.2 交付切片计划（垂直切片，每片独立可合并）

核心过程改进：上一轮 6 PR 链式依赖不可合并。本轮每片是端到端可独立合并、可测试、有验收的垂直切片，绝不链式阻塞。

| 切片 | 内容 | 端到端验收 | 依赖 |
|------|------|-----------|------|
| S1 Foundation | 补完领域模型 + Plan/PlanVersion/Policy/CommandService/AuditEvent + Store migration | 能建 Mission、草拟 Plan、审批、版本化、Command 幂等 | 现有 internal/mission |
| S2 Execution | Scheduler + WorkerAdapter + worktree/Lease + 崩溃恢复 + ContractKind 路由骨架 | 已批准 Plan 可调度，Worker 在 worktree/sandbox 跑，Artifact 记录，重启后对账恢复 | S1 |
| S3 Verify+Integrate | VerifierAdapter + IntegrationAdapter + Evidence 门控 + Mission 自动完成 | 多 Task Mission 端到端：并行实现 + 独立验证 + 集成合并 + 证据驱动成功 | S2 |
| S4 Router+Coordinator | 智能路由器 + Coordinator(triage/decompose/monitor) + /escalate + ChangeRequest 闭环 | 用户输入自动路由 Fast/Deep，Coordinator 草拟 Plan，执行期变更走审批 | S2,S3 |
| S5 Operator | TUI Mission 视图 + 本地 Dashboard(SSE) + 审批流 + /mission /escalate /memory | TUI 与 Dashboard 展示相同状态，审批双入口一致 | S1-S4 |
| S6 Memory Plane | 四级作用域 + Provenance + Mission 晋升/归档 + Consolidation + /memory 控制 | Mission 成功晋升记忆、失败不污染、跨 Mission 检索 | S2 |
| S7 Eval+Benchmark | Mission eval 数据集 + Terminal-Bench 回归门 + OTEL Mission trace | hermetic eval 全过 + Terminal-Bench 不退化 | S1-S4 |
| S8 Hardening | 全量崩溃恢复 + mock sandbox/worker + e2e Mission + 既有 build/test/vet/eval 回归 | 进程重启可恢复、无重复副作用、全量回归绿 | S1-S7 |

每片 Definition of Done：go build ./... + go test ./... + go vet ./... + 该片 eval 全过 + gofmt -l . 干净。

### 10.3 M2 完成标准

M2 完成时必须证明：

1. 智能路由：简单任务走 Fast Lane 零摩擦，复杂任务自动分解为多 Task Mission
2. 可恢复：进程重启后 Mission 可恢复，不重复执行无法确认副作用的 Attempt
3. 隔离：并行代码 Task 不共享可写 worktree 或 Sandbox
4. 审批门控：未批准的 PlanChangeRequest 不被调度；越权操作被拦截
5. 证据驱动：验证失败或缺失 Evidence 不能将 Mission 标记成功
6. 端到端交付：冻结 Feature Mission 能交付跨包代码+测试+文档+集成提交+独立 Evidence
7. 双入口一致：Dashboard 与 TUI 显示相同 Control Plane 状态，基础管理命令统一生效
8. 记忆持久：Mission 成功后知识晋升为 project memory，失败不污染
9. 不退化：go build/test/vet ./... + 既有 eval + Terminal-Bench 全绿
10. 审计可查：所有状态变更通过 CommandService，AuditEvent 完整可追溯

### 10.4 非目标（重申）

- 不做公网/多租户控制面
- 不做 Agent Marketplace / 自动发现外部 Agent
- 不做 Agent 点对点共享对话或直接调度其他 Agent
- 一期 Dashboard 不开放局域网/公网
- 不以 MCP/A2A 联邦 Worker 阻塞本地闭环
- 不废弃现有 Run/RunStream/TUI/Skills/Sandbox/MCP/autodev

---

## 11. 后续

本设计确认后，应先为 S1（Mission Foundation）编写逐文件、逐测试、逐迁移的实施计划。MCP Server、A2A Client/Server、外部 Agent Card 和联邦 Worker 属于本地闭环获得验证后的后续里程碑。Long-Term Memory Phase 3（向量嵌入语义检索、Dreaming 巩固、外部记忆提供者）在 Memory Plane 稳定后推进。
