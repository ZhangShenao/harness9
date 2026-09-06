# Planning 模块重构设计：Plan 作为 Agent 原生能力

- **日期**：2026-09-05
- **状态**：**已废弃** —— 注入机制与数据模型决策已于 2026-09-06 重新确认，以 `docs/superpowers/specs/2026-09-06-native-planning-design.md` 为准（引擎级视图注入 + 轻量更名模型，取代本文的压缩器级注入 + 版本化 Plan 模型）
- **分支**：`feature/native-planning`
- **范围**：取消 Plan Mode，建立 Session 级、带版本化检查点、压缩免疫、主/子代理隔离的原生规划能力

---

## 1. 背景与问题

现状（`internal/planning`、`internal/engine/planmode.go`、`cmd/harness9` TUI）：

1. **Plan Mode 是用户手动切换的模式**（Shift+Tab / `/plan`）：工具白名单硬过滤 + prompt 前缀，规划完成后需人工审批（TUI「Plan Mode 完成」对话框）才能执行。规划不是 Agent 的自主行为。
2. **TodoStore 持久化不足**：仅在 runLoop 退出时（defer）写 Session；Markdown 文件仅人工可读、不参与恢复。进程崩溃后 runLoop 中途的规划进度丢失。
3. **压缩注入不完整**：仅 SummarizationCompactor / ProgressiveCompactor 注入活跃条目（过滤 completed）；TokenBudgetCompactor / SlidingWindowCompactor 完全不注入。压缩后 LLM 可能遗忘计划。
4. **子代理无规划能力**：`subAgentBaseTools` 不含 todo_write，属于「阉割式隔离」。

## 2. 需求与已确认决策

| # | 需求 | 已确认决策 |
|---|------|-----------|
| 1 | 取消 Plan Mode，规划成为原生能力，复杂任务 Agent 主动先规划 | 彻底移除（含审批对话框、Shift+Tab、`/plan` 命令、状态栏标签） |
| 2 | Plan 隔离与持久化（Checkpoint），异常恢复后基于 Plan 继续 | Plan 生命周期 = **整个 Session**（跨请求演进）；每写即落盘检查点 |
| 3 | Plan 不被压缩，每次 Context Compaction 后原样注入 | **全量原样注入**（含 completed/cancelled，带状态标记） |
| 4 | 主 Agent 与 Sub-Agent 的 Plan 隔离 | **子代理拥有自己的独立 Plan**（双向隔离，非阉割式） |

**方案选型**：用户选择方案 B——新建 `Plan` / `PlanStore` / 版本化 Checkpoint 抽象，`todo_write` 更名 `plan_write`，Plan 携带 Goal / Version / UpdatedAt 元数据。（方案 A 为在 TodoStore 上演进；方案 C 为对齐 mission.Store 外置领域模块，均未采纳。）

## 3. 架构总览

```
┌────────────────────────────────────────────────────────────┐
│ plan_write 工具（tools 包）                                  │
│   防作弊校验 → PlanStore.Write → MultiPlanSink 扇出           │
└───────────────┬────────────────────────────────────────────┘
                │ 每写即检查点（不等 runLoop 退出）
   ┌────────────┼──────────────────┐
   ▼            ▼                  ▼
SessionPlanSink JSONLPlanAudit   MarkdownPlanSink
(SQLite 恢复事实源) (版本链审计)    (人工可读视图)
   │
   ▼ GetPlan
beginInteraction 恢复 → PlanStore.Restore → Run 开始时注入 user prompt 前缀
   │
   ▼ 压缩发生时
Summarization / Progressive / TokenBudget / SlidingWindow 四压缩器
   → "## Current Plan" 全量原样注入
```

子代理：`Runner.Run` 每次委派创建独立 `PlanStore` + 独立 `plan_write` 实例 + `WithPlanStore(childStore)`，sink 只挂内存 childSession。

## 4. 核心数据模型（`internal/planning/plan.go`）

```go
type StepStatus string // pending / in_progress / completed / cancelled（沿用现有状态机与转换约束）

type PlanStep struct {
    ID      string     `json:"id"`
    Content string     `json:"content"`
    Status  StepStatus `json:"status"`
}

type Plan struct {
    Goal      string     `json:"goal"`      // 一句话任务目标
    Steps     []PlanStep `json:"steps"`
    Version   int        `json:"version"`   // 每次 Write 自增；恢复校验与审计依据
    UpdatedAt time.Time  `json:"updated_at"`
}
```

JSON tag 遵循项目规范（snake_case + omitempty 由使用场景定，Version/UpdatedAt 始终序列化）。

## 5. PlanStore（`internal/planning/plan_store.go`，替代 TodoStore）

| 方法 | 语义 |
|------|------|
| `NewPlanStore() *PlanStore` | 空 Plan（Version=0） |
| `Write(goal string, steps []PlanStep) Plan` | 全量替换 + `Version++` + `UpdatedAt=now`；双重复制语义沿用 TodoStore 设计 |
| `Read() Plan` | 深拷贝快照 |
| `Restore(p Plan)` | 恢复持久化快照，**不递增版本**（恢复后下一写 = p.Version+1） |
| `Version() int` | 当前版本号 |
| `FormatForInjection() string` | **全量原样**：Goal 头 + 全部步骤（`[ ]` pending / `[>]` in_progress / `[x]` completed / `[-]` cancelled），不过滤已完结条目 |
| `ActiveCount() (active, total int)` | TUI autoExecuting 续跑判断沿用 |
| `Reset()` | `/new` 清空 |

线程安全：`sync.RWMutex`，沿袭 TodoStore 并发模型。

## 6. 检查点（Checkpoint）机制

### 6.1 统一接缝（`internal/planning/plan_sink.go`）

```go
// PlanSink 接收每次 Plan 变更后的完整快照。
type PlanSink interface {
    SavePlan(ctx context.Context, p Plan) error
}

// NewMultiPlanSink(sinks ...PlanSink) PlanSink：逐个扇出，
// 错误聚合记日志、不中断（检查点失败不阻断任务执行，fail-open）。
```

### 6.2 三层存储职责

| 层 | 实现 | 职责 | 失败策略 |
|----|------|------|---------|
| 恢复事实源 | `memory.Session.SavePlan/GetPlan`（SQLite `session_plans` 表） | WAL 保证崩溃安全；`/resume` 与进程重启恢复 | 告警不中断 |
| 版本链审计 | `JSONLPlanAudit`（`.harness9/plans/<sessionID>.jsonl`，每行完整 Plan 快照，追加即版本链） | Plan 演化审计 | fail-open |
| 人工可读 | `MarkdownPlanSink`（现 FilePlanWriter 改造，输出 Goal + 状态标记） | 人类查看 | fail-open |

### 6.3 SQLite schema（`internal/memory/manager.go`）

```sql
CREATE TABLE IF NOT EXISTS session_plans (
    session_id TEXT PRIMARY KEY,
    plan_json  TEXT NOT NULL,
    version    INTEGER NOT NULL DEFAULT 0,
    updated_at DATETIME NOT NULL
);
```

- 替代 `session_todos`（建表语句移除；新装环境不建旧表；存量环境旧表残留无害，后续版本再清理）
- `*SQLiteSession` 实现 `SavePlan`（事务内 UPSERT，write-replace）与 `GetPlan`（无记录返回 `nil, nil`）
- `*MemorySession` 同步实现（内存字段）
- `Session` 接口：移除 `GetTodos/SaveTodos`，新增 `GetPlan/SavePlan`
- `DeleteSession` 级联 GC 扩展：同时清理 `plans/<sessionID>.jsonl` 与对应 Markdown 文件

## 7. plan_write 工具（`internal/tools/plan_write.go`，替代 todo_write）

```go
type planWriteArgs struct {
    Goal  string     `json:"goal"`
    Steps []PlanStep `json:"steps"` // 省略 = 读模式
}
```

- **写模式**：防作弊校验原样迁移（directCompletions ≤ 1；cancelled→completed 始终拒绝）→ `PlanStore.Write` → 扇出注入的 `MultiPlanSink`（每写即检查点）
- **读模式**：省略 `steps` 返回当前 Plan JSON
- 工具描述强化：「复杂任务先规划、随做随更、计划是权威状态」
- 构造：`NewPlanWriteTool(store *planning.PlanStore, opts ...PlanWriteOption)`，`WithPlanSinks(sinks ...planning.PlanSink)` 内部包装 MultiPlanSink

## 8. 压缩免疫（`internal/memory`）

- `TodoInjector` 接口更名为 `PlanInjector { FormatForInjection() string }`，由 `PlanStore` 实现（接口仍定义在 memory 使用者侧）
- **SummarizationCompactor / ProgressiveCompactor**：注入段更名 `## Current Plan`，内容为全量原样；Option 更名 `WithPlanInjector`
- **TokenBudgetCompactor / SlidingWindowCompactor**：**新增** `PlanInjector` 字段与 Option——压缩实际发生时在结果尾部追加 Plan 块（修复现状缺口）
- 注入语义：**仅当本次压缩实际发生**时注入；未压缩不注入（避免每轮重复刷屏）
- 孤立工具对修复逻辑不受影响（注入块为纯文本 user/system 内容，无 tool_call 配对需求）

## 9. 恢复与 Run 开始注入（`internal/engine`）

- `beginInteraction`：`sess.GetPlan()` → 非 nil 则 `planStore.Restore(plan)`（升级现有恢复点；失败告警降级，不终止 Run）
- 原 `applyPlanModePrefix` 注入点改造为 `applyPlanContext`：**Run 开始时 store 非空即注入** user prompt 前缀：

```
## 当前执行计划（Session 权威状态，持续有效）
Goal: <goal>
[ ] / [>] / [x] / [-] <steps...>
请基于此计划继续推进；已完成步骤勿重复执行；计划变化时用 plan_write 更新。
```

- 规则统一：同 Session 第二个请求自动携带 Plan 上下文（Session 级演进语义）；进程崩溃恢复后同样生效
- 运行中演化：plan_write 的 Observation 自然可见；压缩发生后由第 8 节压缩器注入兜底
- 收尾 `saveTodos` → `savePlan`：所有退出路径 `defer` 执行 `Session.SavePlan`（幂等兜底，正常路径每写已实时落盘）

## 10. 生命周期（TUI）

- **`/new`**：`planStore.Reset()` + 新 Session（杜绝跨会话泄漏）
- **`/resume`**：从目标 Session `GetPlan` 重载 store（TUI 与 engine 共享同一 store 实例，现有模式不变）
- autoExecuting 续跑：`ActiveCount()` 语义不变

## 11. 子代理 Plan 隔离（`internal/subagent`）

`Runner.Run` 每次委派：

1. `childPlanStore := planning.NewPlanStore()`（委派级生命周期）
2. 子注册表注册**独立** `plan_write` 实例（绑定 childPlanStore）
3. 子引擎 `WithPlanStore(childPlanStore)` → 自动获得恢复/保存语义；sink 只挂内存 childSession（委派结束即弃，不写主代理审计/Markdown 文件）

**三重隔离保证**：store 实例零共享；子代理 system prompt 无主 Plan 数据通路（`subagent/prompt.go` 仅补充一句规划指引）；子注册表的 plan_write 只触达子 store。

## 12. Plan Mode 移除清单

| 位置 | 动作 |
|------|------|
| `planning/mode.go` + `mode_test.go` | 删除（PlanMode 枚举整体移除） |
| `planning/todo.go` + `tools/todo_write.go` + 测试 | 删除（被 Plan/PlanStore/plan_write 替代） |
| `engine/planmode.go` | 删除；`hasProgressTool`/`appendUserNudge`/`progressToolNames` 迁至 `engine/nudge.go` |
| `engine/agent_loop.go` / `options.go` / `loop_phases.go` | 移除 `planMode` 字段、`WithPlanMode`、`SetPlanMode`、loopContext 快照、`prepareTurnInput` 过滤分支；相关测试改写 |
| TUI（tui.go / tui_update.go / tui_view.go） | 移除 Shift+Tab 切换、`/plan` 命令、「Plan Mode 完成」对话框（视图 + 键盘 + 状态字段）、`[PLAN]` 标签、琥珀 accent 分支、`planModeLabelStyle` |
| `engine/permission.go` | 注释更新（移除 PlanMode 正交性描述） |
| `hooks/plan_writer.go` | 改造为 `MarkdownPlanSink`（实现新 PlanSink 接口） |

## 13. 原生规划指引（`internal/context/builder.go`）

PromptBuilder 新增 Planning 段落：

- 复杂任务（多步骤 / 多文件 / 有依赖链）：**先 `plan_write` 制定计划再执行**
- 每完成一步立即更新对应步骤状态（in_progress → completed）
- 计划是权威状态：即使上下文被压缩，计划始终可见，以计划为准继续
- 简单任务（1-2 步）无需规划

## 14. 错误处理与降级

| 场景 | 行为 |
|------|------|
| GetPlan 失败 | 告警降级，Run 以空 Plan 继续（沿袭 Todo 恢复语义） |
| SavePlan / 审计 / Markdown 写入失败 | 告警 fail-open，不阻断工具执行与主循环 |
| 压缩器注入 | Plan 文本随压缩结果追加，压缩失败走既有 fallback 链 |
| plan_store 为 nil（未注入） | plan_write 工具不注册 / 引擎跳过注入（向后兼容最小引擎） |

## 15. 测试与评估（DOD）

**单元测试**：

- planning：PlanStore 写读恢复、版本自增、全量注入格式（含 completed）、MultiPlanSink 扇出与 fail-open、Reset
- tools：plan_write 防作弊迁移用例（批量伪造拒绝 / cancelled→completed 拒绝 / 单个直通允许）、读模式、检查点触发
- memory：SQLiteSession GetPlan/SavePlan 持久化 + write-replace + 跨会话隔离 + schema 迁移；TokenBudget / SlidingWindow 压缩注入；Summarization/Progressive 更名后回归
- engine：GetPlan 恢复注入（prompt 前缀含计划）、savePlan defer、PlanMode 移除后回归（原 PlanMode 测试删除或改写）
- subagent：子代理写 Plan 不影响父 store、父 Plan 不出现于子上下文

**evals 黄金数据集**（只增不减）：

- `planning_test.go`：既有 4 用例从 todo_write 改写为 plan_write（语义保留）
- 新增 3 用例：`planning/recovery_resume`（恢复后基于 Plan 继续执行）、`planning/compaction_visibility`（压缩后 Plan 可见）、`planning/subagent_isolation`（子代理 Plan 隔离，视 RunCase 能力以 hook 轨迹断言）
- 提交前本地验证：`go test ./internal/evals/... ./internal/evals/dataset/... -v`

**文档（双语 DOD）**：

- `docs/核心功能/planning.md` 与 `docs/core-features-en/planning.md` 重写
- `AGENTS.md` 模块表（planning / engine / memory / tools / subagent / tui 行）与项目结构树同步
- 验证命令：`go test ./...`、`gofmt -l .`、`go vet ./...`

## 16. 非目标（Out of Scope）

- Mission 级 Plan 挂载（`internal/mission` 仍未接入主循环）
- Plan 版本冲突合并 / 多端协同
- 规划审批 UX（用户已确认彻底移除）
- 旧 `session_todos` 表数据迁移（残留无害，不做迁移脚本）
