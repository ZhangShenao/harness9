# Agent OS：本地多 Agent 操作系统

## 概述

harness9 Agent OS 是 M2 里程碑的核心交付物，将 harness9 从单次 Agent 运行时升级为 **local-first 的多 Agent 操作系统**。它让一组本地、无状态的通用 Agent 在可恢复、可审计、可隔离的环境中协作完成跨包、带测试和文档的复杂功能开发。

### 核心设计

**统一运行时 + 升级路由器**架构：

- **Fast Lane**：现有 `engine.Run/RunStream`，简单任务零摩擦，**代码完全不改**
- **Deep Lane（Mission Control）**：Coordinator 分解 -> Scheduler 调度 -> Worker 并行执行 -> Verifier 证据验收 -> Integration 汇合 -> Mission 自动完成
- **Router**：启发式 + 可选 LLM triage，自动决定简单任务走 Fast Lane、复杂任务走 Deep Lane

### 与现有系统的关系

Agent OS **不废弃**任何现有功能。`Run`、`RunStream`、TUI、Skills、Sandbox、MCP Client、`/autodev` 全部保留。Agent OS 的新模块建在现有基础设施之上：

| 现有组件 | Agent OS 中的角色 |
|---------|------------------|
| `engine.Run/RunStream` | Fast Lane 主体，不改 |
| `subagent.Runner` | Deep Lane Worker 的执行内核 |
| `sandbox.Manager` | Worker 的 OS 级隔离 |
| `internal/mission` | Mission Control Store（增强） |
| `internal/ltm` | Memory Plane 基建（增强） |

---

## 架构

```
用户输入 (TUI/CLI/Dashboard)
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

### 包结构

| 包 | 职责 |
|----|------|
| `internal/mission` | 领域模型 + Store + CommandService（Mission/Plan/Task/Attempt/Artifact/Evidence/Lease/Policy/AuditEvent） |
| `internal/scheduler` | 确定性调度器（LLM-free）+ Dispatcher 接口 + RoutingDispatcher + 崩溃恢复 |
| `internal/worker` | WorkerAdapter + git worktree 管理 + ImplementationContract + ParseResult |
| `internal/verifier` | VerifierAdapter（go build/vet/test 证据产出） |
| `internal/integration` | IntegrationAdapter（分支合并 + 联合测试） |
| `internal/router` | 智能路由器（启发式信号检测 + `/mission` 前缀） |
| `internal/coordinator` | Coordinator（DecomposeGoal + CreateTaskFromPlan + Monitor） |
| `internal/dashboard` | 本地 Web 控制台（HTTP + html/template + 命令操作） |
| `internal/ltm` | Long-Term Memory（增强：四级作用域 + Mission 晋升/归档） |

---

## 领域模型

### 核心对象

| 对象 | 职责 |
|------|------|
| **Mission** | 用户目标、冻结验收合同、Policy、全局生命周期 |
| **Plan / PlanVersion** | Task 图的草案与已批准不可变快照，版本化 |
| **PlanChangeRequest** | 执行期变更请求，需人工审批 |
| **Task** | 输入合同（Contract）、依赖、资源边界、验收条件 |
| **TaskAttempt** | 一次 Worker 执行，关联 Lease、事件和产物 |
| **WorkspaceLease** | Task 独占的 worktree + branch + sandbox |
| **Artifact** | Worker 产物（commit/diff/文件清单），SHA256 固定 |
| **Evidence** | 验证结果（build/test/vet），不可变，SHA256 固定 |
| **Policy** | 每 Mission 并发上限、工具范围、预算、重试规则 |
| **AuditEvent** | 不可变审计记录（操作者/动作/目标/原因/幂等键） |

### Task Contract（合同驱动行为）

Task 的行为由 Contract 描述，而非 Agent 人格：

```go
type ContractKind string  // "implementation" | "verification" | "integration"

type TaskInput struct {
    Kind         ContractKind
    Goal         string
    DependsOn    []string
    Acceptance   []string
    AllowedTools []string
    Budget       Budget
    MaxRetries   int
}
```

三种 ContractKind 路由到不同 Dispatcher，Scheduler 自身对 Contract 类型无感。

### 状态机

**Mission 状态**：`draft -> planning -> ready -> running -> verifying -> succeeded/failed/needs_attention/cancelled`

**Task 状态**：`blocked -> queued -> leased -> running -> verifying -> succeeded/failed/awaiting_input/indeterminate`

### 关键不变式

1. Coordinator 只能提议，Scheduler/Control Plane 才能调度
2. Worker 只能提交 Artifact，**不能标记成功**
3. 只有 Verifier 能基于 Evidence 推进最终验收
4. 已批准 Plan 不可原地修改（只能新建版本）
5. `indeterminate` 必须先对账，禁止盲目重试

---

## Mission Control Plane

### Store（`internal/mission`）

SQLite 持久化，复用 `~/.harness9/state.db`。幂等 schema 迁移，包含 10+ 张表（missions/tasks/task_attempts/artifacts/evidence/workspace_leases/plans/plan_versions/plan_change_requests/policies/audit_events）。

### CommandService（唯一状态变更入口）

所有状态变更通过 `CommandService.Execute(Command)` 流转：

- **幂等**：`IdempotencyKey` 去重，相同 key 的重复命令不二次执行
- **审计**：每次执行产出不可变 `AuditEvent`（applied/rejected + before/after state）
- 12 个 CommandKind：plan submit/approve/reject + change request request/approve/reject + mission pause/resume/cancel + retry/escalate/exempt

### Plan 治理

```
Coordinator submit_plan_draft
    -> Plan v1 (draft, 可编辑)
    -> 用户编辑
    -> approve_plan -> PlanVersion v1 (不可变快照, 唯一可调度版本)
    -> 执行期变更 -> request_plan_change
    -> 用户 approve -> PlanVersion v2 (supersedes v1)
```

---

## 执行闭环

### Scheduler（`internal/scheduler`）

确定性、LLM-free 的调度循环：

1. `ListSchedulableTasks`：查找 queued + 依赖已满足 + 有 active PlanVersion
2. `ActiveTaskCounts`：检查全局 + 每 Mission 并发上限
3. 合格则 `StartAttempt` -> `AcquireLease` -> 异步 `Dispatch`
4. 事件驱动 + 定时 tick，非忙轮询

### WorkerAdapter（`internal/worker`）

每次 Attempt 独占一个执行环境：

1. `CreateWorktree` + branch
2. 构建 `ImplementationContract` prompt
3. 调用 `subagent.Runner.Run`（background 模式，隔离 Session）
4. `ParseResult` 解析 `TASK_RESULT` JSON（commit/files/summary）
5. `AddArtifact` 记录产物
6. `RemoveWorktree` 清理（finally）

### 崩溃恢复

进程重启后 `Scheduler.Reconcile()`：
- 查找所有 `running` 状态的 Attempt（进程已不在）
- 标记为 `indeterminate`（**绝不盲目重试**）
- GC 过期 Lease

---

## 验收与集成

### VerifierAdapter（`internal/verifier`）

在全新 worktree 中运行确定性检查，**绝不验证自己实现的产物**：

- `go build ./...` -> Evidence(build)
- `go vet ./...` -> Evidence(vet)
- `go test ./... -count=1` -> Evidence(test)
- 全 passed -> Task `verifying -> succeeded`
- 任一 failed -> Task `verifying -> failed`

### IntegrationAdapter（`internal/integration`）

在 Mission 级 worktree 中合并依赖 Task 分支 + 联合测试：

1. 依次 `git merge` 每个依赖 Task 的分支
2. 冲突 -> `integration_fail` Evidence + 中止
3. 合并后 `go test ./...` + `go vet ./...`
4. 全 passed -> Integration Task succeeded -> 触发 Mission 自动完成

### Mission 自动完成

`TryCompleteMission`：当所有 Task 成功时，Mission `running -> verifying -> succeeded`。

---

## 智能路由

### Router（`internal/router`）

三路决策：

| 信号 | 判定 | 去向 |
|------|------|------|
| `/mission <goal>` 显式前缀 | 强制 Deep | Deep Lane |
| 启发式命中复杂信号（重构/跨包/实现+测试+文档） | 疑似复杂 | Deep Lane |
| 启发式未命中 | 简单 | Fast Lane |

**非破坏性升级**：Fast 任务可 `/escalate` 转为 Mission；Router 的 LLM triage 失败时 fail-open 到 Fast Lane。

### Coordinator（`internal/coordinator`）

- **DecomposeGoal**：创建 Mission + 草拟 Plan（单 Task 或 LLM 分解）
- **CreateTaskFromPlan**：从已批准 Plan 创建实际 Task 记录
- **Monitor**：观察进度，返回状态摘要

---

## Memory Plane

扩展现有 `internal/ltm`，增加四级作用域：

| 作用域 | 生命周期 | 注入对象 |
|--------|---------|---------|
| Project | 跨所有 Mission | 所有 Agent |
| User | 跨项目 | 所有 Agent |
| Mission | Mission 期间 | 该 Mission 的 Agent |
| Agent | 跨 Mission | 同类型 Agent |

- **Mission 成功**：`PromoteMissionToProject` 晋升记忆
- **Mission 失败**：`ArchiveMission` 归档（不晋升，避免污染）
- SHA256 去重 + TTL 过期 + 命中强化 + 陈旧识别

---

## Dashboard

本地 Web 控制台（`harness9 dashboard` 子命令）：

- **监听**：`127.0.0.1:7777`（仅本地，不开放外网）
- **技术**：Go `net/http` + `html/template` 服务端渲染，零外部前端依赖
- **功能**：
  - Mission 列表（状态徽章 + Task 计数）
  - 创建 Mission（goal 表单）
  - Mission 详情（Task 表 + Plan 版本 + Audit 审计流 + ChangeRequest 审批）
  - 提交 Plan Draft（JSON 编辑器）
  - 添加 Task（标题 + contract_kind 选择）
  - 命令操作（Pause/Resume/Cancel/Approve/Reject）
  - JSON API（`GET /api/missions`）

---

## Dashboard 使用

```bash
# 启动 Dashboard
harness9 dashboard
# 或指定地址
harness9 dashboard 127.0.0.1:8888

# 浏览器打开
open http://127.0.0.1:7777
```

---

## M2 完成标准

1. **智能路由**：简单任务走 Fast Lane 零摩擦，复杂任务自动分解
2. **可恢复**：进程重启后 Mission 可恢复，不重复执行无法确认副作用的 Attempt
3. **隔离**：并行代码 Task 不共享可写 worktree 或 Sandbox
4. **审批门控**：未批准的 PlanChangeRequest 不被调度
5. **证据驱动**：验证失败或缺失 Evidence 不能将 Mission 标记成功
6. **端到端交付**：多 Task Mission 能交付代码+测试+集成+独立 Evidence
7. **双入口一致**：Dashboard 与 TUI 显示相同 Control Plane 状态
8. **记忆持久**：Mission 成功后知识晋升为 project memory
9. **不退化**：`go build/test/vet ./...` + 既有 eval 全绿
10. **审计可查**：所有状态变更通过 CommandService，AuditEvent 完整可追溯
