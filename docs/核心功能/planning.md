# Planning 模块实现原理

harness9 的 Planning 模块解决一个核心问题：**如何让 Agent 在开始行动之前先想清楚，而不是一边做一边猜？**

一般的 Agent 遇到复杂任务时，容易陷入"走一步看一步"的模式——它无法确定自己当前完成了多少、还剩多少、下一步做什么。Planning 模块把 **Plan 变成 Agent 的原生能力**：面对复杂任务时 Agent 会主动先用 `plan_write` 制定执行计划，再逐步执行；计划是 Session 级的权威状态——带写时检查点（Checkpoint）持久化、对上下文压缩免疫、在主代理与子代理之间完全隔离。

> 设计历史：早期版本采用用户手动切换的 Plan Mode（Shift+Tab + 工具白名单 + 人工审批对话框）。
> 该设计已被移除——规划不应是用户按需开启的"模式"，而应是 Agent 自主运用的"能力"。
> 详见 `docs/技术调研/planning-native-redesign-v2.md`。

---

## 核心设计决策

| 决策 | 选择 | 理由 |
|------|------|------|
| 规划触发方式 | System Prompt 准则，LLM 自主判断 | 简单任务强制规划是开销浪费；复杂任务漏规划由准则 + 停滞检测兜底 |
| Plan 粒度 | 一个 Session 一个 Plan | 与会话生命周期对齐，`/resume` 语义自然 |
| 持久化机制 | 写时检查点（plan_write 成功即落盘） | 崩溃窗口从"整个 Run"缩小到"单轮之内" |
| 压缩免疫 | 引擎级视图注入（Compactor 零改动） | 单一代码路径覆盖压缩/恢复/运行中三场景，且保证"原样注入"不被摘要器转述 |
| Sub-Agent 隔离 | 委派级独立 PlanStore | 双向隔离靠构造实现，无共享通道可泄露 |

## 系统架构

```
internal/planning/
├── plan.go       # PlanStore（线程安全）+ PlanItem / PlanStatus + FormatPlan
└── plan_writer.go # PlanWriter 接口（供 plan_write 依赖，避免循环导入）

internal/tools/
└── plan_write.go # plan_write 工具：读写 PlanStore + 批量防作弊校验

internal/engine/
├── nudge.go      # appendUserNudge（Plan 注入复用）+ progressToolNames（停滞检测）
├── loop_phases.go # beginInteraction（Plan 恢复）/ prepareTurnInput 4c（Plan 注入）
│                 # checkpointPlan（写时检查点）/ savePlan（defer 幂等兜底）
└── agent_loop.go # runLoop 编排器

internal/memory/
├── session.go    # Session 接口：GetPlan / SavePlan
└── sqlite_session.go # session_plans 表（UPSERT 单行 JSON）

internal/subagent/
└── runner.go     # 每次委派构造独立 PlanStore + 独立 plan_write 实例

cmd/harness9/
├── tui_update.go # EventToolResult 激活 autoExecuting + EventDone 续跑循环 + 停滞检测
└── tui_view.go   # renderPlanLines()（带图标的计划渲染）+ 状态栏进度展示
```

---

## 数据模型

```go
// internal/planning/plan.go

type PlanStatus string // pending / in_progress / completed / cancelled

type PlanItem struct {
    ID      string     `json:"id"`      // LLM 自行分配，防作弊校验依据
    Content string     `json:"content"` // 一个具体可执行动作
    Status  PlanStatus `json:"status"`
}
```

合法状态转换路径（由 plan_write 工具校验，PlanStore 本身不做校验）：

```
pending ──► in_progress ──► completed
   │              │
   └──────────────┴──► cancelled
```

### PlanStore：全量替换语义

PlanStore 采用**全量替换**（atomic replace）而非增量更新：LLM 每次调用 plan_write 输出完整的当前计划，全量替换与这种输出形式完全匹配，同时避免增量 API 的状态一致性问题。

线程安全：`sync.RWMutex`，`Read` 允许多读并发，`Write` 排他；双重复制保证调用方、内部存储与返回值三者解耦。

核心 API：

| 方法 | 语义 |
|------|------|
| `Write(items []PlanItem) []PlanItem` | 全量替换，返回替换后的副本 |
| `Read() []PlanItem` | 当前快照副本 |
| `FormatPlan() string` | 活跃条目（pending/in_progress）格式化为注入文本；无活跃条目返回空串 |
| `ActiveCount() (active, total int)` | TUI autoExecuting 续跑判断 |

---

## plan_write 工具

plan_write 是 Planning 模块对 LLM 暴露的唯一计划管理接口，两种调用模式：

- **写模式**（提供 `steps` 数组）：防作弊校验通过后全量替换 PlanStore
- **读模式**（省略 `steps`）：返回当前计划 JSON，不修改状态

```json
{
  "steps": [
    {"id": "1", "content": "创建 parser.go 解析配置文件", "status": "pending"},
    {"id": "2", "content": "实现 load 逻辑", "status": "in_progress"}
  ]
}
```

### 防作弊校验（Anti-Cheat Validation）

LLM 存在"伪造进度"的失败模式——不做实际工作却批量把条目标记为 completed。防作弊校验规则：

1. **批量直通拒绝**：一次调用中最多允许 1 个 pending/新建条目直接跳转 completed（`directCompletions ≤ 1`）；超过即拒绝整批写入，错误回传给 LLM 触发自愈
2. **cancelled → completed 始终拒绝**：取消的条目表明已放弃，需先恢复为 pending/in_progress
3. **阈值设为 1 而非 0**：保留"完成实际工作后直接记录结果"的正常用法，只阻止批量伪造

### Markdown 计划文件（PlanWriter）

通过 `WithPlanWriter` 注入 `hooks.FilePlanWriter`，每次写入 PlanStore 成功后同步覆写 markdown 计划文件（git 项目写入 `workDir/.harness9/plans/`，否则写入 homeDir）。这是**人工可读视图**，写入失败 fail-open（告警不阻断）。

---

## 原生规划：System Prompt 准则

规划行为由 `DefaultPromptBuilder` 注入 System Prompt 的准则驱动（`WithPlanEnabled(true)`，仅在 plan_write 已注册时注入）：

```
## 规划（Planning）
面对复杂多步任务（多文件改动、有依赖链的步骤、需要探索后实施）时，
先用 plan_write 制定执行计划，再逐步执行：
- 计划条目必须对应具体可执行动作（创建文件、实现函数、运行命令），
  禁止"需求澄清"、"方案设计"类无法直接执行的条目
- 开始某条目前将其标记为 in_progress，完成后立即标记为 completed
- 计划是权威状态：即使对话上下文被压缩，计划始终可见，以计划为准继续
- 简单任务（1-2 步、问答、单命令）无需规划，直接执行
```

LLM 自主判断何时规划——**无工具过滤、无 prompt 前缀、无运行时检测**。这是"能力"与"模式"的本质区别：Agent 像资深工程师一样，复杂任务先列计划、简单任务直接动手。

---

## 写时检查点（Checkpoint）

### 机制

```
runLoop：executeTools 之后
  → 扫描本轮 ToolCalls：存在成功的 plan_write 调用
  → lc.sess.SavePlan(ctx, planStore.Read())   // 立即落盘 SQLite
```

```go
// internal/engine/loop_phases.go
func (lc *loopContext) checkpointPlan(calls []schema.ToolCall, results []schema.ToolResult) {
    if lc.sess == nil || lc.planStore == nil {
        return
    }
    for i, tc := range calls {
        if tc.Name == "plan_write" && i < len(results) && !results[i].IsError {
            lc.savePlan(lc.obsCtx)
            return
        }
    }
}
```

- **崩溃窗口**：从"整个 Run"缩小到"单轮之内"——进程在任意时刻被杀，最多丢失"正在执行的那一轮"的计划进度
- **defer 兜底**：`savePlan` 仍以 defer 注册在所有退出路径执行（幂等），兜底写时检查点中途失败的重试
- **fail-open**：SavePlan 失败仅告警，不阻断工具执行与主循环

### 持久化存储：session_plans 表

```sql
CREATE TABLE IF NOT EXISTS session_plans (
    session_id TEXT PRIMARY KEY,
    items      TEXT    NOT NULL DEFAULT '[]',
    updated_at INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);
```

- `SQLiteSession.SavePlan`：UPSERT 单行 JSON（write-replace）
- `SQLiteSession.GetPlan`：无记录返回 `nil, nil`
- 外键级联：`DeleteSession` 删除会话时自动清理对应计划（`PRAGMA foreign_keys=ON`）
- `MemorySession` 同步实现（内存字段），供子代理与测试使用
- 旧 `session_todos` 表不做数据迁移：新装环境不建旧表，存量环境旧表残留无害

### 异常恢复路径

```
进程崩溃 ──► 用户重启 harness9 ──► /resume 恢复会话
  ──► beginInteraction：sess.GetPlan() → PlanStore 恢复（失败降级为空计划）
  ──► 首轮 prepareTurnInput：活跃计划注入发送视图
  ──► 用户发一句"继续" ──► Agent 基于未完成条目继续执行
```

---

## 压缩免疫：引擎级 Plan 注入

### 问题：为什么不在压缩器里保留 Plan？

早期设计把活跃条目追加到 LLM 摘要文本末尾，存在两个缺陷：

1. **非原样**：注入内容经过摘要器/拼装逻辑转述，不满足"原样注入"的严格语义
2. **不完整**：仅 SummarizationCompactor / ProgressiveCompactor 注入；TokenBudgetCompactor / SlidingWindowCompactor（截断回退）完全不注入——fallback 截断恰恰是最容易丢失计划的场景

### 方案：压缩之后、LLM 调用之前注入

`prepareTurnInput` 的步骤 4c（在记忆 nudge、停滞 nudge 之后）：

```go
// internal/engine/loop_phases.go
if lc.planStore != nil {
    if planText := lc.planStore.FormatPlan(); planText != "" {
        compactedHistory = appendUserNudge(compactedHistory, planText)
    }
}
```

关键性质：

- **压缩器零改动**：无论哪个 Compactor 实现（Summarization / TokenBudget / SlidingWindow / Progressive）产出了怎样的压缩视图，注入步骤都在其后执行——Plan 必然可见，包括 fallback 字符截断的场景
- **单一代码路径覆盖三场景**：自动压缩后（视图即压缩产物）、会话恢复后（PlanStore 已恢复）、运行中（每轮重算，plan_write 更新次轮即可见）
- **临时副本**：注入只作用于当次发送视图，不写入 `lc.history`、不持久化、不累积（与 nudge 同机制）——历史完整性不被污染
- **手动 /compact 同路径受益**

### 注入格式（FormatPlan 输出）

```
## 当前执行计划（权威状态，压缩或恢复后仍以此为准继续执行）
[ ] 创建 parser.go 解析配置文件
[>] 实现 load 逻辑
```

仅含 pending / in_progress 条目（已完成条目无需重复注入）；`[ ]` 表示 pending，`[>]` 表示 in_progress。

### 成本分析

活跃计划通常 < 500 字节，每轮重复注入的 token 开销可忽略；换取的是"压缩/恢复后计划永不丢失"的硬保证。压缩器内的 `TodoInjector` 冗余注入机制已随之移除——引擎级注入是唯一事实源。

---

## Sub-Agent Plan 隔离

### 设计：委派级独立 PlanStore

`subagent.Runner.Run` 每次委派：

```go
// internal/subagent/runner.go
childPlanStore := planning.NewPlanStore()
effectiveBaseTools = append(effectiveBaseTools, tools.NewPlanWriteTool(childPlanStore))
// ...
opts := []engine.Option{
    engine.WithSession(childSession),      // MemorySession（已有）
    engine.WithPlanStore(childPlanStore),  // 子引擎完整规划语义
    // ...
}
```

### 三重隔离保证

| 层 | 保证 |
|----|------|
| 存储实例 | 子引擎只持有自己的 PlanStore 引用，与父代理零共享——隔离靠构造实现，无共享通道可泄露 |
| System Prompt | 子代理 prompt（`subagent/prompt.go`）仅含规划准则，无任何父 Plan 数据通路 |
| 工具实例 | 子注册表的 plan_write 绑定 childPlanStore，物理上无法触达父存储 |

### 生命周期

- 子代理 Plan 随 `MemorySession` 消亡（委派结束即弃），不写 SQLite、不写 markdown 审计文件
- 子代理拥有与主代理相同的规划能力（先规划后执行），`Runner` 自动追加独立 plan_write 实例，无需在 main.go 重复注册
- **白名单式自定义 agent 定义**（`.harness9/agents/*.md` 的 `tools:` 列表）需显式列出 `plan_write` 才能使用规划能力；不列则该子代理无规划工具（自然降级为纯执行者）
- 防递归 `denyTaskHook` 不变：子代理仍不允许再派生子代理

---

## TUI 集成

### Plan 渲染

- **对话流快照**：plan_write 成功完成后，在工具完成行下方追加带图标的计划快照（`renderPlanLines`）：`▶` in_progress（黄）/ `✔` completed（绿）/ `⊘` cancelled（灰）/ `○` pending
- **状态栏进度**：`N/M tasks` 展示完成比例

### autoExecuting 自动续跑

规划是全自动无确认的：Agent 规划后直接执行，用户可随时 Ctrl+C/ESC 打断。

- **激活**：plan_write 成功写入后自动激活（`autoExecuting = true`）
- **续跑**：Run 结束（EventDone）时若计划仍有未完成条目，自动 dispatch 续跑指令继续执行
- **停滞保护**：连续 3 次 EventDone 后 completed 数无增加，判定为空转，停止续跑并提示用户手动介入
- **取消即停**：Ctrl+C/ESC 取消时立即关闭 autoExecuting
- **完成退出**：全部条目 completed 后自动退出续跑模式

---

## 与旧设计的差异

| 维度 | 旧设计（已移除） | 新设计 |
|------|----------------|--------|
| 规划触发 | 用户 Shift+Tab 手动切换 Plan Mode | System Prompt 准则，LLM 自主判断 |
| 工具控制 | Plan Mode 下白名单过滤（只读工具） | 恒为全量工具列表 |
| 计划确认 | "Plan Mode 完成"审查对话框（4 选项） | 全自动无确认，随时可打断 |
| 状态存储 | TodoStore + session_todos 表（行式） | PlanStore + session_plans 表（JSON 行） |
| 持久化时机 | 仅 runLoop 退出时（defer） | 写时检查点 + defer 幂等兜底 |
| 压缩注入 | 摘要器内拼接 Active Tasks（部分压缩器覆盖） | 引擎级每轮原样注入（全压缩器通吃） |
| Sub-Agent | 无规划能力（阉割式隔离） | 独立 PlanStore（双向隔离） |
| TUI | Shift+Tab / [PLAN] 标签 / 审查对话框 / 琥珀色调 | 移除；保留计划渲染与 autoExecuting |

## 测试与评估

- **单元测试**：`planning/plan_test.go`（写读/快照隔离/FormatPlan/ActiveCount/并发）、`tools/plan_write_test.go`（防作弊校验/读模式/PlanWriter 触发）、`engine`（写时检查点时序、压缩后注入与视图隔离、恢复注入、无活跃条目跳过）、`memory`（GetPlan/SavePlan 持久化 + 跨会话隔离）、`subagent`（子代理写 Plan 不影响父存储）
- **黄金数据集**（24 用例）：planning 5 例（生成计划/探索后规划/先规划后执行/只读探索/简单任务不规划）+ compaction 4 例（含 plan_survives 压缩免疫）
