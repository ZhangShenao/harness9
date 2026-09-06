# Planning 模块重构设计：Plan 作为 Agent 原生能力（引擎级注入方案）

- **日期**：2026-09-06
- **状态**：已确认（用户逐节批准，取代 `docs/技术调研/planning-native-redesign.md`）
- **分支**：`feature/native-planning`
- **范围**：取消 Plan Mode，建立 Session 级、写时检查点、压缩免疫、主/子代理隔离的原生规划能力

> **与前一版设计的差异**：前一版（2026-09-05）选定压缩器级注入（4 个 Compactor 全部改造）+
> 版本化 Plan{Goal, Version, UpdatedAt} 数据模型 + 工具级 MultiPlanSink 扇出。本版改为：
> **引擎级视图注入**（Compactor 零改动，单一代码路径）+ **轻量更名模型**（PlanItem/PlanStore）+
> **引擎级写时检查点**。用户于 2026-09-06 重新确认。

---

## 1. 背景与问题

现状（`internal/planning`、`internal/engine/planmode.go`、`cmd/harness9` TUI）：

1. **Plan Mode 是用户手动切换的模式**（Shift+Tab）：工具白名单硬过滤 + prompt 前缀 + 「Plan Mode
   完成」人工审批对话框。规划不是 Agent 的自主行为，与"原生能力"的定位相悖。
2. **TodoStore 持久化不足**：仅在 runLoop 退出时（defer `saveTodos`）写 Session；进程崩溃后
   runLoop 中途的规划进度全部丢失，无法基于 Plan 恢复执行。
3. **压缩注入不完整且非原样**：仅 SummarizationCompactor / ProgressiveCompactor 把活跃条目
   **追加到 LLM 摘要文本末尾**（受摘要器转述影响）；TokenBudgetCompactor /
   SlidingWindowCompactor 完全不注入。不满足"Plan 原样注入、不被压缩"。
4. **子代理无规划能力**：子引擎未注入 TodoStore，`plan_write`（原 `todo_write`）不可用，
   属于"阉割式隔离"而非"双向隔离"。

## 2. 需求与已确认决策

| # | 需求 | 已确认决策（2026-09-06） |
|---|------|------------------------|
| 1 | 取消 Plan Mode，规划成为原生能力，复杂任务 Agent 主动先规划 | 彻底移除 Plan Mode（Shift+Tab、审批对话框、状态栏标签）；**System Prompt 准则**驱动，LLM 自主判断何时规划 |
| 2 | Plan 隔离与持久化（Checkpoint），异常恢复后基于 Plan 继续 | Plan 粒度 = **一个 Session 一个 Plan**；**写时检查点**（plan_write 成功即落盘）；恢复语义 = **会话恢复时注入**（/resume 后 Plan 可见，用户说"继续"即续跑） |
| 3 | Plan 不被压缩，每次 Context Compaction 后原样注入 | **引擎级视图注入**：prepareTurnInput 在压缩之后把活跃 Plan 原样注入发送视图；Compactor 零改动 |
| 4 | 主 Agent 与 Sub-Agent 的 Plan 隔离 | 子代理**拥有独立 PlanStore**（有规划能力），与父 Plan 双向隔离，隔离靠构造实现 |

**方案选型**：引擎级 Plan 视图注入 + 写时检查点（方案 A）。落选方案：
- 压缩器锚定（给消息打 IsPlan 标记，全部 Compactor 改造）——改动面大、fallback 截断仍可能丢、恢复注入需第二套机制
- PromptBuilder 动态重建——history[0] 在 Run 内静态，Run 中途 plan_write 更新不可见，违背检查点语义

**TUI 交互**：全自动无确认——Agent 规划后直接执行，用户可随时打断；TUI 仅做 Plan 渲染。
**工具命名**：`todo_write` 更名 `plan_write`，同步更新 evals 黄金数据集与文档。

## 3. 架构总览

```
┌──────────────────────────────────────────────────────────────┐
│ plan_write 工具（tools 包）→ 防作弊校验 → PlanStore.Write       │
└───────────────┬──────────────────────────────────────────────┘
                │ 写时检查点：runLoop 检测到本轮成功的 plan_write
                ▼
        lc.sess.SavePlan(ctx, store.Read())     ← SQLite session_plans 表
                │
                │ beginInteraction（Run 开始 / 会话恢复）
                ▼
        sess.GetPlan() → PlanStore 恢复
                │
                │ prepareTurnInput（每轮，压缩之后）
                ▼
        活跃 Plan 原样注入发送视图（临时副本，不持久化）
```

核心原则：**压缩器不感知 Plan**。Plan 的可见性由引擎在"压缩视图"上统一保证——
无论是自动压缩、手动 /compact 还是会话恢复，发送给 LLM 的视图末尾永远有活跃 Plan。

## 4. 数据模型与 planning 包 API（§1）

`internal/planning/todo.go` → `internal/planning/plan.go`（更名平移，JSON 结构不变）：

```go
type PlanStatus string // pending / in_progress / completed / cancelled（状态机与转换约束不变）

type PlanItem struct {
    ID      string     `json:"id"`
    Content string     `json:"content"`
    Status  PlanStatus `json:"status"`
}

// PlanStore（原 TodoStore 更名，全量替换 + 双重复制 + sync.RWMutex 语义不变）
func NewPlanStore() *PlanStore
func (s *PlanStore) Write(items []PlanItem) []PlanItem
func (s *PlanStore) Read() []PlanItem
func (s *PlanStore) FormatPlan() string // 原 FormatForInjection 更名
func (s *PlanStore) ActiveCount() (active, total int)
```

命名清理：`TodoStatus→PlanStatus`、`TodoPending/InProgress/Completed/Cancelled→Plan*`。
`PlanWriter` 接口（`plan_writer.go`）签名不变，FilePlanWriter（markdown 人工可读视图）继续生效。

**注入文本格式**（原样注入的内容体，`FormatPlan()` 输出）：

```
## 当前执行计划（权威状态，压缩或恢复后仍以此为准继续执行）
[ ] 创建 parser.go 解析配置文件
[>] 实现 load 逻辑
```

仅含 pending / in_progress 条目（已完成条目无需重复注入）。标题行措辞强调"权威状态、
持续有效"：注入每轮发生在发送视图上（压缩后、恢复后均生效），并非仅压缩后一次性注入。

## 5. 引擎生命周期：检查点 + 注入（§2）

### 5.1 写时检查点

```
runLoop：executeTools 之后扫描本轮 responseMsg.ToolCalls
  → 存在成功的 plan_write 调用
  → lc.sess.SavePlan(ctx, lc.planStore.Read())   // 立即落盘
```

- 替代现状"仅 defer 到 runLoop 结束"的持久化；崩溃窗口从"整个 Run"缩小到"单轮之内"
- `defer savePlan` 保留为幂等兜底：写时检查点若在 Run 中途失败，Run 结束时重试一次；
  同时覆盖未来新增的 Plan 状态变更路径
- SavePlan 失败：告警 fail-open，不阻断主循环（沿袭 saveTodos 语义）

### 5.2 引擎级 Plan 注入

`prepareTurnInput` 新增步骤 4c（位于 4a 记忆 nudge、4b 停滞 nudge 之后）：

```
4c. Plan 注入：若 PlanStore 非 nil 且存在活跃条目（pending/in_progress），
    在压缩视图末尾 append 一条 user 消息（FormatPlan() 文本）
```

- **临时视图消息**：不写入 `lc.history`、不持久化、不累积（与 nudge 同机制，复用
  `appendUserNudge`）；每轮重算，plan_write 更新后下一轮立即可见
- 覆盖三个场景，单一代码路径：
  1. **压缩后**：`applyCompactionWith` 产出压缩视图 → 注入步骤在其后执行，Plan 必然可见
     （Summarization / TokenBudget / SlidingWindow / Progressive 四压缩器通吃，fallback 截断也通吃）
  2. **会话恢复**：`beginInteraction` 已从 Session 恢复 PlanStore → 首轮注入即生效
  3. **运行中**：每轮注入保持 Plan 持续可见
- TUI `/compact` 手动压缩走同一 `prepareTurnInput` 路径，自动受益
- 成本：Plan 通常 < 500 字节，每轮重复注入可接受

### 5.3 beginInteraction 变更

- `sess.GetTodos` 恢复 → `sess.GetPlan` 恢复（失败告警降级为空 Plan，不终止 Run）
- 移除 `applyPlanModePrefix` 调用
- `loopContext.planMode` 字段移除

## 6. 原生规划能力（§3）

### 6.1 System Prompt 准则（`internal/context/builder.go`）

DefaultPromptBuilder 新增 Planning 段落（未注入 PlanStore 时省略）：

```
## 规划（Planning）
面对复杂多步任务（多文件改动、有依赖链的步骤、需要探索后实施）时，
先用 plan_write 制定执行计划，再逐步执行：
- 计划条目必须对应具体可执行动作（创建文件、实现函数、运行命令），
  禁止"需求澄清"、"方案设计"类无法直接执行的条目
- 开始某条目前标记 in_progress，完成后立即标记 completed
- 计划是权威状态：即使对话上下文被压缩，计划始终可见，以计划为准继续
- 简单任务（1-2 步、问答、单命令）无需规划，直接执行
```

LLM 自主判断何时规划——无工具过滤、无 prompt 前缀、无运行时检测（用户已确认）。

### 6.2 plan_write 工具（`internal/tools/todo_write.go` → `plan_write.go`）

- `TodoWriteTool` → `PlanWriteTool`，工具名 `todo_write` → `plan_write`
- Schema description 更新：「创建或更新当前任务的执行计划；复杂任务先规划后执行，随做随更」
- 参数 key：`todos` → `steps`（`{"steps":[{id,content,status}]}`；省略 steps = 读模式）
- **防作弊状态机校验原样迁移**（directCompletions ≤ 1；cancelled→completed 拒绝；全量替换语义）
- 读模式（省略 steps 返回当前计划 JSON）保留
- `WithPlanWriter`（FilePlanWriter markdown 持久化）继续生效

## 7. Sub-Agent 隔离（§4）

`internal/subagent/runner.go` 的 `Run()`：

```go
childPlanStore := planning.NewPlanStore()   // 每次委派新建，与父引擎零共享
opts = append(opts, engine.WithPlanStore(childPlanStore))
// childSession = memory.NewMemorySession(...)（已有）
```

- **隔离靠构造**：子引擎只持有自己的 PlanStore 引用，没有任何代码通路触达父 PlanStore
- 子代理 Plan 随 MemorySession 生命周期消亡（委派结束即弃），不写 SQLite、不写审计文件
- `plan_write` 进入子代理工具集：
  - general-purpose 继承全部基础工具，自动获得
  - 白名单式自定义 agent 定义（`.harness9/agents/*.md` 的 `tools:` 列表）需显式列出 `plan_write`（文档说明）
- 防递归 `denyTaskHook` 不变；子代理 system prompt（`subagent/prompt.go`）注入同款规划准则

## 8. 移除清单（§5）

| 位置 | 动作 |
|------|------|
| `planning/mode.go` + `mode_test.go` | 删除（PlanMode 枚举整体移除） |
| `engine/planmode.go` | 删除；`hasProgressTool` / `progressToolNames` / `appendUserNudge` 迁至 `engine/nudge.go` |
| `engine/options.go` | 移除 `WithPlanMode` / `SetPlanMode`；`WithTodoStore` → `WithPlanStore` |
| `engine/agent_loop.go` | 移除 `planMode` 字段与 loopContext 快照 |
| `engine/loop_phases.go` | 移除工具过滤分支；`saveTodos` → `savePlan`；新增 4c 注入与写时检查点 |
| TUI `tui.go` | 移除 `planMode` 字段、`planModeLabelStyle`、琥珀 accent 分支；`todoStore` → `planStore` |
| TUI `tui_update.go` | 移除 Shift+Tab 切换、「Plan Mode 完成」对话框键盘与状态处理、`/plan` 相关分支 |
| TUI `tui_view.go` | 移除 `[PLAN]` 标签与确认对话框渲染；Plan 渲染改读 PlanStore |
| `memory/summarization.go` | 移除 `TodoInjector` 接口、`WithTodoInjector`、摘要拼接 "Active Tasks"（引擎级注入是唯一事实源，避免双重注入） |
| `memory/progressive_compactor.go` | 移除 `WithProgressiveTodoInjector` |
| `cmd/harness9/main.go` | 移除 todoStore/planMode 接线，改接 PlanStore；`WithProgressiveTodoInjector` 调用删除 |

TUI 保留：Plan 渲染（对话流 + 状态栏待办计数）、`autoExecuting` 续跑（`ActiveCount()` 驱动）。
Shift+Tab 按键释放（不再绑定任何行为）。

## 9. 持久化与 memory 包（§6）

### 9.1 Session 接口（`memory/session.go`）

```go
// 替换 GetTodos / SaveTodos
GetPlan(ctx context.Context) ([]planning.PlanItem, error)              // 无记录返回 nil, nil
SavePlan(ctx context.Context, items []planning.PlanItem) error         // write-replace
```

- `SQLiteSession`：新表 `session_plans (session_id TEXT PRIMARY KEY, items JSON NOT NULL, updated_at DATETIME NOT NULL)`，
  UPSERT 事务写入；**不迁移旧 `session_todos` 表数据**（旧表残留无害，Plan 为空即无计划）
- `MemorySession`：同步更名实现（内存字段）
- `DeleteSession` 级联 GC 扩展清理 `session_plans` 记录

### 9.2 压缩器去耦

`TodoInjector` 接口及两个 Compactor 的注入逻辑整体移除（见 §8）——引擎级注入是唯一事实源。
压缩器对 Plan 完全无感知，四实现零改动。

## 10. 测试与评估（§7，DOD）

### 10.1 单元测试

- **planning**：todo_test.go 全量平移为 plan_test.go（Write/Read/FormatPlan/ActiveCount）
- **tools**：todo_write_test.go 平移为 plan_write_test.go（防作弊校验/读模式/PlanWriter 触发）
- **engine**：
  - 删除 PlanMode 相关测试（InjectsPlanPrefix / FiltersWriteTools）
  - 新增：① plan_write 成功轮触发即时 SavePlan；② 压缩发生后发送视图含 Plan 文本且
    `lc.history` 保持完整（视图隔离）；③ 无活跃条目不注入；④ Session 预置 Plan →
    beginInteraction 恢复 → 首轮视图可见
- **subagent**：子代理写 Plan 不影响父 store；父 Plan 不出现于子代理任何上下文；子代理可调用 plan_write
- **memory**：SQLiteSession GetPlan/SavePlan 持久化 + write-replace + 跨会话隔离 + 级联 GC

### 10.2 evals 黄金数据集（只增不减，22 → 24）

- `planning_test.go`：4 个现有用例迁移到 plan_write（语义保留：生成计划/不写文件/先规划后执行/只读探索）
- 新增 `planning/simple_task_no_plan`（反向：简单任务不强制规划）
- `compaction_test.go`：新增 `compaction/plan_survives`（压缩后 Plan 原样保留）
- 提交前本地验证：`go test ./internal/evals/... ./internal/evals/dataset/... -v`

### 10.3 文档（双语 DOD）

- `docs/核心功能/planning.md` 重写 + `docs/core-features-en/planning.md` 英文镜像
- `AGENTS.md`：模块表（planning / engine / memory / tools / subagent / tui 行）、项目结构树、
  核心架构 Planning 段落同步
- 验证命令：`go test ./...`、`gofmt -l .`、`go vet ./...`

## 11. 错误处理与降级

| 场景 | 行为 |
|------|------|
| GetPlan 失败 | 告警降级，Run 以空 Plan 继续（沿袭 Todo 恢复语义） |
| SavePlan 失败（写时检查点 / defer 兜底） | 告警 fail-open，不阻断工具执行与主循环 |
| planStore 为 nil（未注入） | 引擎跳过注入与保存（最小引擎向后兼容） |
| FilePlanWriter 写 markdown 失败 | 告警 fail-open（现状语义不变） |

## 12. 非目标（Out of Scope）

- Plan 版本化元数据（Goal/Version/UpdatedAt）——轻量模型已满足需求，需要时再演进
- 规划审批 UX（用户已确认彻底移除）
- Mission 级 Plan 挂载（`internal/mission` 仍未接入主循环）
- 旧 `session_todos` 表数据迁移（残留无害）
- 自动续跑增强（/resume 后是否免输入自动继续执行）——本期为"恢复注入 + 用户一句'继续'"
