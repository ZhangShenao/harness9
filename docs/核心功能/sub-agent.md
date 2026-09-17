# Sub-Agent 系统实现原理

harness9 的 Sub-Agent 系统让主代理可以把**边界清晰的子任务**委派给拥有独立上下文、受限工具集与可选模型覆盖的专门代理执行。子代理不是新的抽象——它就是一个运行在隔离 Session 上的普通 `engine.AgentEngine` 实例，复用现有 `RunStream` 流水线，不改动核心 `runLoop` 一行代码。

---

## 系统架构

```
internal/subagent/
├── definition.go   # SubAgentDefinition 结构体 + ResolveTools + Validate
├── registry.go     # Registry：Register / Get / List（启动阶段注册，运行期只读）
├── frontmatter.go  # parseAgentFile：YAML frontmatter + 正文 → SubAgentDefinition
├── loader.go       # Registry.LoadFromDir：扫描 .harness9/agents/*.md 文件式定义
├── builtin.go      # RegisterBuiltins：六个内置子代理（编译进二进制，可被同名文件覆盖）
├── prompt.go       # promptBuilder：子代理 system prompt + Skills 预加载 + workDir 注入
├── tracker.go      # TaskTracker：后台任务单一事实源（Start/AppendLog/Finish/Control/List/Get）
├── control.go      # TaskController：单个后台任务的控制平面（实现 engine.TaskGate）
├── runner.go       # Runner：构建隔离子引擎 + 运行 RunStream + 桥接审批与进度
├── task_tool.go    # TaskTool：主代理调用的唯一委派入口（tools.BaseTool）
├── task_status.go  # TaskStatusTool：观察后台任务（task_status 工具）
├── task_wait.go    # TaskWaitTool：等待后台任务完成（task_wait 工具，join 原语）
└── task_control.go # TaskControlTool：控制后台任务（task_control 工具）

internal/engine/
└── task_gate.go    # TaskGate 接口：轮边界控制门（接口定义在使用者侧，WithTaskGate 注入）

cmd/harness9/
├── main.go         # 接线：RegisterBuiltins、LoadFromDir、NewRunner、task + 协调三工具注册
├── tui_update.go   # EventSubAgent 渲染 + DrainCompleted 注入 + 自动唤醒（maybeAutoWake）+ @agent 直跑 + 任务面板按键
└── tui_view.go     # renderSubAgentProgress()、renderTaskPanel()（五状态着色）、renderStatusBar() 后台任务状态栏
```

---

## 子代理定义

### 内置子代理库（六个）

harness9 内置了 **六个子代理**（`general-purpose` / `explorer` / `researcher` / `implementer` / `reviewer` / `planner`），编译进二进制、开箱即用，完整清单见下文[「异步调度与协调控制」章节](#异步调度与协调控制)的内置子代理表。其中 `general-purpose`（通用）是兜底委派目标，设计直接对标两个主流框架的同名能力：

- **Claude Code** 的 [general-purpose subagent](https://code.claude.com/docs/en/sub-agents#general-purpose)：「A capable agent for complex, multi-step tasks that require both exploration and action」，继承主对话的全部工具与模型，是「没有更专门子代理时」的兜底委派目标。
- **DeepAgents** 的 [general-purpose subagent](https://docs.langchain.com/oss/python/deepagents/subagents#the-general-purpose-subagent)：每个 deep agent 默认都携带，用于「上下文隔离但无需专门行为」的场景——主代理把多步任务整体委派出去，只拿回一份简洁结论，避免中间过程污染主上下文。

两者共同的设计内核被 harness9 完整继承：

| 维度 | general-purpose 的取值 | 含义 |
|------|----------------------|------|
| `Tools` | 留空（nil） | **继承父代理全部可用工具**，能读写文件、执行命令、调用 skill |
| `Model` | 留空（`""`） | **继承父代理模型**，不额外覆盖 |
| `MaxTurns` | 留空（0） | 继承引擎默认轮数（与主代理一致） |
| 定位 | 兜底委派目标 | 任务边界清晰、可独立完成、希望隔离上下文时使用 |

**何时委派给它**：任务需要兼顾探索与修改、需要复杂推理来解释中间结果、或包含多个相互依赖的步骤，且你只想要最终结论而非冗长的中间过程。

### 编程式定义

六个内置子代理在 `internal/subagent/builtin.go` 的 `RegisterBuiltins(reg)` 中集中定义并注册（`main.go` 启动时一次性调用，定义非法或重名返回 error）。以 `general-purpose` 为例：

```go
subagent.SubAgentDefinition{
    Name:         "general-purpose",
    Description:  "通用子代理，处理需要兼顾探索与修改、复杂推理或多步依赖的任务。……继承父代理可用的全部工具与模型。",
    SystemPrompt: generalPurposeSystemPrompt, // 强调「上下文隔离 + 自包含结论」
    Source:       "builtin", // Tools/Model/MaxTurns 均留空：工具与模型继承父，轮数继承引擎默认
}
```

> 需要更专门的能力（如安全审计、文档撰写）时，推荐通过下文的**文件式定义**新增子代理，而非堆叠更多编程式内置——保持内核精简，专门角色交给项目侧定义。

### SubAgentDefinition 字段说明

| 字段 | 类型 | 说明 |
|------|------|------|
| `Name` | `string` | 唯一标识，须匹配 `^[a-z0-9][a-z0-9-]*$` |
| `Description` | `string` | 写给 LLM 的"何时使用我"，是 `task` 工具调度依据的核心 |
| `SystemPrompt` | `string` | 子代理 system prompt 正文 |
| `Tools` | `[]string` | 工具白名单；nil/空 = 继承父全部可用工具 |
| `DisallowedTools` | `[]string` | 工具黑名单（先 deny 后 allow） |
| `Model` | `string` | 模型覆盖；`""` = 继承父代理模型 |
| `MaxTurns` | `int` | 最大轮数；`0` = 继承默认值（与主代理一致，当前 50） |
| `Skills` | `[]string` | 启动时预加载的 skill 名称（正文注入子代理 system prompt） |
| `Source` | `string` | 诊断字段：`"builtin"` 或文件路径 |

### 文件式定义

在工作目录的 `.harness9/agents/` 下创建 `*.md` 文件，harness9 启动时自动扫描加载。**文件定义覆盖同名编程式定义**（记录日志，不报错）。若文件未包含 `name` 字段，自动回退用文件名（去 `.md` 后缀）作为 Name。

**完整示例 `.harness9/agents/security-auditor.md`**：

```markdown
---
name: security-auditor
description: 安全审计专家。对涉及认证、鉴权、输入校验的代码变更后使用，检测 OWASP Top 10 漏洞。
tools: read_file, bash
disallowed_tools: write_file, edit_file
model: openai/gpt-4o
max_turns: 30
skills: security-review
---

你是一名应用安全工程师，专注于识别代码中的安全漏洞。
审查时按优先级输出：严重 > 高危 > 中危 > 低危，每条附上 CWE 编号与修复建议。
不要修改文件，只输出审查报告。
```

**frontmatter 字段速查**：

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | 同 SubAgentDefinition.Name |
| `description` | string | 同 SubAgentDefinition.Description |
| `tools` | 逗号分隔字符串 | 白名单，如 `read_file, bash` |
| `disallowed_tools` | 逗号分隔字符串 | 黑名单 |
| `model` | string | 模型覆盖 |
| `max_turns` | int | 最大轮数 |
| `skills` | 逗号分隔字符串 | 预加载 skill 名称 |

---

## task 工具

`task` 是注册在父代理工具注册表中的普通工具（`tools.BaseTool`）。LLM 通过调用 `task` 工具委派子任务；子代理的 registry 永不包含 `task`，从根上禁止递归。

### 工具参数

| 参数 | 类型 | 必填 | 说明 |
|------|------|:----:|------|
| `subagent_type` | string（枚举） | ✅ | 已注册子代理的 Name，`Definition()` 动态枚举 |
| `prompt` | string | ✅ | 传给子代理的完整任务描述。子代理看不到父对话历史，所有必要信息都要写在这里 |
| `description` | string | ❌ | 3–5 词的简短标题（UI 展示用） |
| `background` | bool | ❌ | 是否后台异步运行（默认 `false`） |

`Definition()` 在每次被调用时**动态生成**，将所有已注册子代理的 Name 作为 `subagent_type` 的 `enum`，Description 拼入工具描述，是 LLM 选择"调用哪个子代理"的依据：

```
把一个边界清晰的任务委派给专门的子代理执行。子代理拥有独立上下文与受限工具集。
可用子代理：
- general-purpose: 通用子代理，处理需要兼顾探索与修改、复杂推理或多步依赖的任务。当任务边界清晰、可独立完成、且希望隔离上下文时使用；没有更专门的子代理时它是默认兜底选择。继承父代理可用的全部工具与模型。
- explorer: 只读深探索专家：理解项目结构、定位实现、梳理调用关系。…
- security-auditor: 安全审计专家。…（文件式定义）
使用模式：
- 并行委派：同一回复中发起多个 background=true 任务可并行执行，随后用 task_wait 聚合结果
- 观察/等待：task_status 查询后台任务状态，task_wait 阻塞等待完成
- 调整方向：后台任务方向偏差时用 task_control 的 steer 注入转向指令（下一轮生效），而非取消重跑
- 控制：task_control 支持 pause/resume/cancel/steer
```

### 前台执行（`background=false`，默认）

```
task 工具调用
    │ execCtx = 父调用方 ctx
    ▼
Runner.Run(..., background=false)
    │ 构建隔离子引擎，调用 RunStream，消费事件流
    │ 审批请求 → parentApproval(ctx, ...) → 透传父 TUI 审批对话框
    ▼
阻塞直到子引擎 channel 关闭
    │
    ▼
返回 <task state="completed"><task_result>...最终文本...</task_result></task>
```

前台执行的 tool result 直接作为工具调用的 Output 注入父代理的上下文历史，主代理可立即读取子代理输出。

### 后台执行（`background=true`）

```
task 工具调用
    │
    ▼
task 工具立即返回 <task id="task-general-purpose-1" state="running"/>
    │
    ▼ 同时：go func(){...}()
        execCtx 从会话级 baseCtx 派生（独立于父 turn，不受工具 60s 超时影响）
        审批请求 → 一律拒绝（fail-closed），返回"子代理无可用审批通道，已自动拒绝"
        子引擎事件流 → tracker.AppendLog(id, update)（全过程日志写入内存，加锁，不经 channel）
        子引擎执行完成 → tracker.Finish(id, finalText, isErr)
            │ 触发 SetNotify 回调 → tea.Program.Send(subAgentNotifyMsg) → TUI 即时显示完成提示

下一次 dispatch() 前：
    tracker.DrainCompleted() → 拼入 prompt 前缀 → 注入 LLM 上下文
```

---

## 执行模型与 Context 传递

### Runner 的两阶段执行

`Runner.Run` 是子代理执行的核心：

1. **构建隔离 registry**：`buildChildRegistry` 按 `ResolveTools`（白名单∩全集 - 黑名单 - task）筛选工具，包上 `permission.NewFileHook`（继承同一 `settings.json`）+ `denyTaskHook`（防递归） + sharedHooks（dangerHook + offloadHook）。
2. **解析 Provider**：`def.Model != ""` 时新建 OpenAI Provider 并查询对应 context window；`""` 时复用父代理模型。
3. **构建 PromptBuilder**：子代理 system prompt + workDir 注入 + def.Skills 列表中的 skill 正文（通过 `skills.Index.GetFullContent` 加载，失败静默忽略）。
4. **独立 MemorySession**：`memory.NewMemorySession(childID)`（纯内存，不含父对话历史，不含父 system prompt）。
5. **启动 RunStream**：`sub.RunStream(execCtx, prompt)`，消费事件流，转发进度、桥接审批，累积最终文本。

### Context 传递规则

```
父代理 ──► task 工具 ──► prompt 字符串 ──► 子代理（唯一信息来源）
子代理 ──► FinalText ──► tool result ──► 父代理上下文（前台）
子代理 ──► TaskTracker ──► DrainCompleted ──► 父代理下次 prompt 前缀（后台）
```

子代理**看不到**父代理的对话历史和 system prompt。文件路径、背景信息、需求细节必须通过 `task` 工具的 `prompt` 参数显式传递。

### 执行 Context 差异

| 维度 | 前台（`background=false`） | 后台（`background=true`） |
|------|--------------------------|--------------------------|
| execCtx 来源 | 父调用方 `ctx`（工具超时 60s 以内） | 会话级 `baseCtx` 派生（独立于父 turn） |
| 审批策略 | 透传父 `ApprovalFunc`，TUI 审批对话框可用 | 一律拒绝（fail-closed） |
| 结果交付 | tool result 同步返回 | `TaskTracker.Finish` 写入内存，下次 dispatch 时 `DrainCompleted` 注入 |
| 进度日志 | 经 `EventSubAgent` 实时渲染到 subAgentLines | `TaskTracker.AppendLog` 缓冲到内存，可通过 `/tasks` 面板查看 |
| 取消传播 | 父 ctx 取消 → 子代理随之取消 | baseCtx 取消（进程关闭）才取消 |

---

## TUI 实时进度渲染

前台子代理执行期间，TUI 在工具进度区下方实时追加 `[agent-name]` 前缀的暗青色进度行：

```
  [general-purpose] 子代理启动…
  [general-purpose] ▸ read_file
  [general-purpose]   ✓
  [general-purpose] ▸ bash
  [general-purpose]   ✓
  [general-purpose] 已定位问题根因，正在汇总结论...
  [general-purpose] ✓ 完成
```

进度行最多保留最近 `maxSubAgentLines = 12` 行，防止长时间运行的子代理无界增长。`SubAgentThinking`（推理增量）故意不展示，减少噪声。

进度数据流：`Runner.emit(SubAgentUpdate)` → `hooks.SubAgentProgressFunc`（注入 context）→ `RunStream` 转为 `EventSubAgent` 事件 → TUI `EventSubAgent` case → `m.subAgentLines` 追加。

---

## 安全保障

| 安全层 | 机制 | 说明 |
|--------|------|------|
| 禁止递归 | 子 registry 永不含 task 家族四工具 | `ResolveTools` 硬编码剥离 `task` / `task_status` / `task_wait` / `task_control`（`alwaysDeniedTools`，无论白名单黑名单如何声明） |
| 禁止递归（纵深） | `denyTaskHook.BeforeExecute` | 双重防御：即使未来代码引入 task 家族工具，hook 也会在运行期拒绝 |
| 防越权操纵 | 同上 `alwaysDeniedTools` | 子代理不得操纵兄弟任务、查询或控制主代理的 TaskTracker（协调平面是主代理专属） |
| 权限不升级 | 继承同一 `.harness9/settings.json` | `permission.NewFileHook(settingsPath)` 复用同一规则文件 |
| 权限只叠加更严 | 子代理额外叠加 DisallowedTools + denyTaskHook | 只能比父代理更受限，不能扩权 |
| Context 隔离 | 独立 `MemorySession`（纯内存） | 不含父对话历史，不含父 system prompt，无数据泄漏路径 |
| 工具隔离 | `ResolveTools`（白名单∩全集 - 黑名单 - task 家族） | 仅注册显式允许的工具实例 |
| 后台审批 fail-closed | 后台子代理审批一律拒绝 | 无 TUI 通道时宁可拒绝，不自动放行危险操作 |
| 结果恰好注入一次 | `injected` 标志三方共用 | `DrainCompleted`（自动注入）/ `task_status` / `task_wait` 共用同一标志，同一结果不会重复进入上下文 |
| 敏感路径 | sharedHooks 含 `dangerHook` | 19 条高危模式（`~/.ssh`、`~/.aws` 等）同样保护子代理 |

---

## TaskTracker — 后台任务单一事实源

`TaskTracker` 是后台子代理任务的线程安全单一事实源，替代旧版 `Mailbox`，同时承担全过程日志缓冲与结果注入两项职责：

### API 一览

| 方法 | 调用方 | 说明 |
|------|--------|------|
| `Start(agentName, description, prompt) string` | 后台 goroutine 启动时 | 注册 Running 任务，返回唯一 `id`（格式 `task-{agent}-{seq}`） |
| `AppendLog(id, SubAgentUpdate)` | 后台 goroutine 流式推进中 | 将进度事件追加到内存缓冲（加锁），不经任何 channel |
| `Finish(id, finalText, isErr)` | 后台 goroutine 完成时 | 标记 Done/Failed，触发 `SetNotify` 回调（锁外调用）；已终态不可覆写 |
| `Attach(id, *TaskController)` | TaskTool 后台路径启动前 | 把控制平面挂接到任务记录，供 Control 路由 |
| `Control(id, action, message)` | `task_control` 工具 / TUI 面板 | 控制唯一入口：校验存在性与终态后路由到 controller，`action ∈ pause/resume/cancel/steer`；pause/resume 成功后同步落账快照状态 |
| `Cancel(id, reason)` | TaskTool 后台 goroutine | 标记 TaskCancelled（区别于 Failed），原因写入 finalText 并通知 |
| `MarkInjected(ids...)` | `task_status` / `task_wait` | 标记结果已消费，与 `DrainCompleted` 共用 `injected` 标志（结果恰好注入一次） |
| `DrainCompleted() []CompletedTask` | TUI `dispatch()` 前 | 返回已完成未注入结果，标记为 injected（幂等）；终态（Done/Failed/Cancelled）才 Drain，Paused 不 Drain |
| `List() []TaskSnapshot` | TUI 任务面板 | 全量快照，按创建顺序 |
| `Get(id) (TaskDetail, bool)` | TUI 任务详情 | 返回含全过程日志深拷贝的 `TaskDetail` |
| `RunningCount() int` | TUI 状态栏 | 活跃（非终态）任务数：运行中 + 已暂停 |
| `DoneCount() int` | TUI 状态栏 | 已结束（完成 + 失败 + 取消）任务数 |
| `SetNotify(fn func())` | TUI 初始化时 | 注册完成通知回调 |

### 两条独立路径

**注入路径**：`Finish` 将最终文本写入内存，父代理**下次 dispatch** 时 `DrainCompleted` 排空并前置拼入 LLM 上下文（`pendingSubAgentInject` 缓冲）。`DrainCompleted` 是幂等的，已注入的结果不会被再次取走。

**提示路径**：`Finish` 同时触发 `SetNotify` 回调——TUI 在启动时将其注册为 `tea.Program.Send(subAgentNotifyMsg{})`，后台任务完成瞬间即向 scrollback 追加一条「✓ 后台子代理完成」提示（仅展示，不消费注入缓冲，二者互不干扰）。

**全过程日志**：`AppendLog` 直接写入内存缓冲（加锁），完全不经 channel，从根本上杜绝 send-on-closed-channel 风险。日志通过 `Get(id).Log` 暴露给 `/tasks` 面板详情页。

---

## 后台任务查看器

### 状态栏指示

状态栏在存在后台任务时自动显示任务计数段：

```
⚙ 2 运行/3 完成
```

由 `renderStatusBar()` 调用 `TaskTracker.RunningCount()` 和 `DoneCount()` 实时读取，仅在至少有一个任务（运行中或已完成）时展示，零任务时不占用状态栏空间。

### 打开面板

两种等价方式：

| 方式 | 说明 |
|------|------|
| `Ctrl+T` | 键盘快捷键切换（空闲态可用；运行中、审批、审查、恢复选择等模态冲突时忽略） |
| `/tasks` + Enter | 斜杠命令，效果与 `Ctrl+T` 完全相同 |

面板为**模态视图**：激活时 `taskPanelMode = true`，`View()` 将输入区替换为 `renderTaskPanel()` 渲染的面板内容，普通输入和其他快捷键全部由 `handleTaskPanelKey` 接管。

### 列表视图

面板打开时默认展示任务列表，每行格式：

```
{● 运行/⏸ 已暂停/✓ 完成/✗ 失败/· 已取消}  {id} [{状态}]  {agent}  "{描述}"  {耗时}；最近：{活动}
```

状态字样按五状态着色（运行绿 / 暂停黄 / 取消灰 / 完成蓝 / 失败红），行格式与 `task_status` 工具的单行摘要保持一致。当前选中行以 `▶` 高亮。按键说明：

| 按键 | 行为 |
|------|------|
| `↑` / `↓` | 移动光标 |
| `Enter` | 进入选中任务的详情视图 |
| `Esc` 或 `Ctrl+T` | 关闭面板，返回正常输入模式 |

暂停 / 恢复 / 取消 / 转向四类控制操作也直接在列表态按键发起（`p` / `r` / `x` / `s`），详见下文[「异步调度与协调控制」章节的「TUI 面板控制」小节](#tui-面板控制)。

### 详情视图

按 `Enter` 选中任务后进入详情视图，展示该后台子代理的全过程日志（通过 `TaskTracker.Get(id)` 取 `TaskDetail.Log` 深拷贝）：

```
general-purpose — 完成  （↑↓ 滚动，Esc 返回）

启动…
▸ read_file(main.go)
▸ bash(go vet ./...)
  ✗ 工具执行失败
发现 2 处安全问题…

— 最终结果 —
建议修复以下两处…
```

日志渲染由 `formatTaskLog` 完成，覆盖 `SubAgentStart / SubAgentToolStart / SubAgentDelta / SubAgentToolResult（仅失败）/ SubAgentError` 五种事件，`SubAgentDone` 及 `FinalText` 合并为结尾「最终结果」块。

| 按键 | 行为 |
|------|------|
| `↑` / `↓` | 滚动日志（`taskDetailScroll` 偏移） |
| `Esc` | 返回列表视图（`taskDetailID = ""`） |
| `Ctrl+T` | 关闭整个面板 |

### 实时刷新

运行中的任务每次面板渲染时直接读取 `TaskTracker` 快照（`List()` / `Get()`），无需订阅通知，TUI 主循环驱动即可保持日志行数（`LogLines`）的实时更新。

---

## 异步调度与协调控制

`background=true` 的后台任务不是「发射后不管」——harness9 为其构建了完整的控制、观察与结果回流闭环：主代理可以随时暂停、恢复、取消或转向一个正在运行的后台子代理，等待聚合多个并行任务的结果，并在任务完成时被系统自动唤醒。这一能力由三个平面协作完成。

### 三平面架构

```
┌──────────────────────────────────────────────────────────────────┐
│ 控制平面（Control Plane）                                          │
│   TaskController（internal/subagent/control.go，每后台任务一份）    │
│   实现 engine.TaskGate（internal/engine/task_gate.go）             │
│   Pause / Resume / Cancel / Steer —— 轮边界门控 + execCtx 中断     │
└───────────────▲──────────────────────────────────┬───────────────┘
                │ tracker.Control(id, action, msg) │ WithTaskGate(ctl) 注入子引擎
┌───────────────┴──────────────────────────────────▼───────────────┐
│ 协调平面（Coordination Plane）—— 主代理专属工具                     │
│   task_status（观察） task_wait（join 等待） task_control（控制）   │
│   全部路由到 TaskTracker（单一事实源）                              │
└───────────────┬──────────────────────────────────────────────────┘
                │ TaskTracker.Start / Attach / Finish / DrainCompleted
┌───────────────▼──────────────────────────────────────────────────┐
│ 数据平面（Data Plane）                                             │
│   Runner：构建隔离子引擎（独立 registry + MemorySession + PlanStore）│
│   运行 RunStream，事件流经 AppendLog 缓冲、终态经 Finish 落账       │
└──────────────────────────────────────────────────────────────────┘
```

- **控制平面**：`TaskController` 是单个后台任务的控制句柄，并发安全，`Pause` / `Resume` / `Cancel` 对非目标状态幂等。它实现 `engine.TaskGate` 接口（接口定义在使用者侧的 engine 包），由 `Runner` 经 `engine.WithTaskGate(ctl)` 注入子引擎——主引擎路径不注入（nil），零开销。
- **协调平面**：`task_status` / `task_wait` / `task_control` 三个工具只注册进**主代理**的 registry，是主代理 LLM 观察、等待与控制后台任务的唯一窗口；全部经 `TaskTracker.Control` 等入口路由，不直接触碰子引擎。
- **数据平面**：`Runner` 为每次委派构建完全隔离的子引擎并运行 `RunStream`，进度经 `AppendLog` 缓冲、终态经 `Finish` / `Cancel` 落账到 `TaskTracker`。

### 轮边界门控语义

控制动作的生效时机由 `engine.TaskGate` 的插入点决定：子引擎 `runLoop` 在**每轮开始前**（`beginTurn` 之前）调用 `AwaitTurn`——暂停时阻塞，放行时取出排队的转向消息。

| 动作 | 生效时机 | 语义 |
|------|---------|------|
| `pause` | 当前轮结束后 | 进行中的 LLM 调用与工具执行**跑完当前轮**后在轮边界停住；暂停期间 `AwaitTurn` 阻塞，**不消耗 MaxTurns 配额** |
| `resume` | 立即 | 关闭阻塞通道放行门控，从暂停处继续下一轮 |
| `cancel` | 立即 | cancel 子代理 execCtx——进行中的 LLM 调用与工具执行**当场中断**（与用户 Ctrl+C 同语义），同时唤醒暂停中的门控等待 |
| `steer` | 下一轮开始时 | 转向消息进入信箱（steerBox）；门控放行时一次性全部取出，以 **user 角色**持久化到子代理历史（前缀 `[主代理转向指令]`）后继续循环 |

三条关键语义边界：

1. **暂停不耗 MaxTurns**：暂停区间在 `beginTurn` 计数之前，子代理不会因为「暂停过久而耗尽轮数」；这是轮边界门控相对轮内抢断的核心取舍——不破坏进行中的调用，代价是暂停生效有最长一轮的延迟。
2. **steer 以 user 消息持久化**：转向是子代理对话的一部分（区别于 nudge 的防御性副本），随历史持久化、参与后续每一轮推理。steer **不自动恢复**暂停中的任务（恢复由主代理显式 resume，职责分离）；任务在取走前结束则信箱消息丢弃（best-effort）。
3. **cancel 立即中断**：`Runner` 派生 execCtx 后经 `ctl.bindExec(cancel)` 绑定 cancel 函数；`Cancel` 先行到达（sandbox 创建等窗口）时 `bindExec` 补发取消，堵住 lost-cancel。终态（Done / Failed / Cancelled）不可迁移——取消后迟到的 `Finish` 是无操作，不会覆写。

### 协调三工具

#### task_status — 观察

| 参数 | 类型 | 必填 | 说明 |
|------|------|:----:|------|
| `task_id` | string | ❌ | 要查询的任务 id；**省略则返回全部任务** |

单任务输出（终态附最终结果，截断 2048 字符）：

```
task-general-purpose-1 [完成] general-purpose "调研超时处理" 1m32s；最近：详见任务面板
建议修复以下两处…
```

全部任务输出为每任务一行摘要，终态任务追加结果文本。**已完成任务的结果随查询直接返回并标记已注入**（`MarkInjected`，与 `DrainCompleted` / `task_wait` 共用标志）——此后自动注入通道不再重复投递。运行中任务**不**标记（否则 `Finish` 后 `DrainCompleted` 永久跳过，切断自动注入通道）。

#### task_wait — 等待（join 原语）

| 参数 | 类型 | 必填 | 说明 |
|------|------|:----:|------|
| `task_ids` | string[] | ❌ | 要等待的任务 id 列表（逐个校验存在性，未命中直接报错而非静默空等） |
| `all` | bool | ❌ | 等待全部运行中任务（省略 `task_ids` 时的默认语义） |
| `timeout_sec` | int | ❌ | 等待上限秒数（默认 60，clamp 至 600） |

关键语义：

- **等待 ctx 从会话级 baseCtx 派生并自带 timeout**，刻意忽略父 Turn 的 60s 工具超时（手法同 `Runner.Run` 的 execCtx 派生）；用户 Ctrl+C 经 baseCtx 传播结束等待。
- **超时不是 error**：返回仍在运行任务的当前状态快照，由 LLM 决定继续等还是先做别的：

```
[general-purpose 仍在运行 task-explorer-2]（运行中）
[general-purpose 完成 task-researcher-3]
调研结论：…
等待超时，以上仍在运行的任务未完成。可再次 task_wait 继续等待，或先处理其他事项。
```

- 终态按 State 三分渲染：**完成 / 失败 / 已取消**——主代理主动取消（重跑）与子代理出错（读原因排查）的后续处置不同，不得混同。已完成结果同样 `MarkInjected`（恰好注入一次）。

#### task_control — 控制

| 参数 | 类型 | 必填 | 说明 |
|------|------|:----:|------|
| `task_id` | string | ✅ | 目标任务 id |
| `action` | string（枚举） | ✅ | `pause` / `resume` / `cancel` / `steer` |
| `message` | string | ❌ | `steer` 的转向指令内容（必填）；`cancel` 的原因 |

状态机错误（对终态任务操作、`steer` 缺 message 等）**以正常文本结果返回而非 Go error**——LLM 能读到失败原因并自行调整，例如：

```
操作失败：任务 task-explorer-2 已结束（完成），无法执行 steer
```

成功输出（每种动作一条确认语）：

```
task-explorer-2 已暂停（当前轮完成后停住，resume 恢复）
task-explorer-2 已注入转向指令，子代理下一轮开始时生效
```

### 自动唤醒闭环

后台任务完成后，结果不会静静躺在 TaskTracker 里等待用户下次发言——TUI 构建了一条「通知 → 收获 → 空闲且有预算 → 合成 dispatch」的异步闭环：

```
tracker.Finish(id, ...)
    │ 触发 SetNotify 回调
    ▼
tea.Program.Send(subAgentNotifyMsg)          # 即时：结果显示到对话区（用户立即可见）
    ▼
handleSubAgentNotify()
    ├─ harvestSubAgentResults()               # DrainCompleted：显示一次 + 写入 pendingSubAgentInject 注入缓冲
    └─ maybeAutoWake()                        # 自动唤醒判定
         │ 条件：启用 && 主代理空闲（非 running）&& 非压缩窗口 && 注入缓冲非空 && 预算 > 0
         ├─ 预算 -1
         ├─ 取走注入缓冲（防 dispatch 兜底 harvest 双重前缀）
         ├─ 对话区追加「⟳ 后台子代理任务完成，自动唤醒主代理」
         └─ dispatch("[系统自动唤醒] 以下后台子代理任务已结束，请处理其结果并继续推进整体任务；…\n\n{结果块}")
```

预算与开关：

| 项 | 值 | 说明 |
|----|----|------|
| 会话级预算 | `autoWakeBudgetMax = 10` | 防止后台任务连环完成触发主代理链式跑飞 |
| 预算重置 | 任意真实用户输入 | 重置收敛在 Enter 提交分支（普通 prompt、`/` 命令、`@mention`、Shell 模式均经此进入）；不能放 dispatch 内——自动唤醒自身走 dispatch，会自我续满预算 |
| 耗尽行为 | 提示一次后停用 | 「⚠ 自动唤醒已达上限，后台结果将在你下次发送消息时注入」；`exhausted` 随预算一起按周期复位（用户输入即开启新周期） |
| 压缩窗口跳过 | `compacting` 时跳过 | `/compact` 压缩期间（LLM 摘要耗时数秒），唤醒 dispatch 的历史落盘会与 Compact 的写回竞态互抹；结果留注入缓冲，由压缩后的下次 dispatch 兜底消费 |
| 关闭开关 | `HARNESS9_AUTOWAKE=false` | 默认启用 |

### 后台任务生命周期时序

```
主代理 LLM                     TaskTool                      Runner / 子引擎                 TaskTracker
    │ task(background=true)       │                              │                            │
    ├────────────────────────────►│ Start(def, desc, prompt) ────┼───────────────────────────►│ 注册 Running，返回 id
    │                             │ NewTaskController(sink)      │                            │
    │                             │ Attach(id, ctl) ─────────────┼───────────────────────────►│ 控制平面挂接
    │ <task id state="running"/>  │ go func(){ Runner.Run(bgCtx, def, prompt, true, ctl) }    │
    │ （立即返回，Turn 继续）       │                              │ Sandbox + PlanStore + 隔离 registry
    │                             │                              │ engine.WithTaskGate(ctl)    │
    │                             │                              │ execCtx ← baseCtx 派生      │
    │                             │                              │ ctl.bindExec(cancel)       │
    │                             │                              ▼                            │
    │                             │                    ┌─ for 每轮：AwaitTurn（门控）          │
    │                             │                    │    ├─ 暂停中 → 阻塞（不耗 MaxTurns） │
    │                             │                    │    └─ 放行 → 取出 steer 信箱         │
    │                             │                    │        → 以 user 消息持久化 [主代理转向指令]
    │                             │                    │  beginTurn → LLM → 工具 → Observation │
    │                             │                    └─ 自然终止 / 出错 / 被取消              │
    │                             │                              │                            │
    │ task_control(cancel) ───────┼─ Control(id,"cancel",reason) │                            │
    │                             ├─────────────────────────────►│ ctl.Cancel：cancelExec()   │
    │                             │                              │ （in-flight 立即中断）       │
    │                             │                              ▼                            │
    │                             │          RunStream 返回 err；ctl.State()==TaskCancelled   │
    │                             │          tracker.Cancel(id, "已被主代理取消：reason") ────►│ Cancelled 终态
    │                             │                              │                            │
    │                             │          正常完成：ctl.Finish(nil) → emit Done             │
    │                             │          tracker.Finish(id, finalText, false) ───────────►│ Done 终态 + notify
    │                             │                              │                            │
    │                             │                              │            notify → subAgentNotifyMsg
    │ ◄── 对话区即时显示 + pendingSubAgentInject 缓冲 + maybeAutoWake 合成 dispatch ───────────┤
```

要点：**取消传播**走 `cancelExec → execCtx.Done() → RunStream 退出 → TaskTool goroutine 识别 `TaskCancelled` → `tracker.Cancel` 落账**（终态不可迁移，迟到的 Finish 不覆写）；**steer 注入**走信箱 → 门控放行时以 user 消息进入子代理历史，随压缩与持久化同普通对话消息对待。

### 内置子代理与委派引导

六个内置子代理覆盖「探索 / 调研 / 实现 / 审查 / 规划 / 兜底」全谱（`internal/subagent/builtin.go`，`RegisterBuiltins` 注册；`.harness9/agents/` 同名文件可覆盖内置）：

| 名称 | 工具白名单 | 定位 |
|------|-----------|------|
| `general-purpose` | 留空（继承父全部可用工具与模型） | 兜底：兼顾探索与修改、复杂推理、多步依赖的通用任务 |
| `explorer` | `read_file` / `glob` / `grep` / `bash` | 只读深探索：理解项目结构、定位实现，回传带 `文件:行号` 引用的结论 |
| `researcher` | `web_search` / `web_fetch` / `read_file` | Web 多源调研：选型、API 用法、最佳实践，回传带 URL 引用的结论 |
| `implementer` | `read_file` / `write_file` / `edit_file` / `bash` / `glob` / `grep` | 边界清晰的实现任务：改后自动构建 / 测试验证，回传改动清单与验证结果 |
| `reviewer` | `read_file` / `glob` / `grep` / `bash` | 只读代码审查：bug、安全、并发，按严重度分级输出（不修改代码） |
| `planner` | `read_file` / `glob` / `grep` | 只读产出实施计划：步骤分解、改动面、依赖顺序、风险与验证方式 |

系统提示词层的共同约束：`explorer` / `reviewer` / `planner` 的 `bash` 仅限只读命令，绝不修改 / 创建 / 删除文件；`researcher` 要求事实与推测分开陈述、关键结论交叉验证。

配套的**委派准则**（`internal/context/builder.go`，`task` 系工具注册后注入主代理 system prompt）与**委派 nudge**（`engine.WithDelegationNudge`，连续 3 轮「有探索无进展」时注入一次提示，单次交互至多 2 次）引导主代理把批量探索委派给 `explorer` 以保护主上下文。

### TUI 面板控制

后台任务面板（`Ctrl+T` 或 `/tasks`）的列表态支持四类控制操作键，与 `task_control` 工具同源（均经 `tracker.Control` 路由）：

| 按键 | 动作 | 说明 |
|------|------|------|
| `p` | 暂停 | 当前轮完成后停住（面板状态即时变黄「已暂停」） |
| `r` | 恢复 | 从暂停处继续 |
| `x` | 取消 | **两段确认防误触**：首按武装（底部出现「⚠ 再按 x 确认取消」），再按执行；期间任何其他按键（含 `↑↓` 移动）解除武装 |
| `s` | 转向 | 面板底部展开单行输入框；`Enter` 提交（注入转向指令，子代理下一轮生效）、`Esc` 取消、其余键进输入框 |

操作结果即时反馈到对话区（如「⏸ task-explorer-2 已暂停」「↪ 已注入转向指令」）；面板每帧重读 `TaskTracker` 快照，暂停 / 恢复在 pause/resume 成功后同步落账，状态色即时点亮。

---

## @ 提及调用

### 基本用法

在输入框中以 `@<agent> <task>` 格式发送，**绕过主 LLM 的工具决策**，直接前台调用指定子代理：

```
@general-purpose 调查 internal/tools/bash.go 的超时处理逻辑并总结实现要点
```

发送后：
1. TUI 立即追加用户消息行（`▶ You: @general-purpose …`）
2. 子代理名称行（`◆ general-purpose:`）追加到 scrollback
3. `running = true`，输入框禁用
4. 子代理流式进度实时渲染到 `subAgentLines`（与 `task` 工具前台执行完全相同的渲染路径）
5. 完成后，最终文本直接追加到 scrollback（作为 assistant 消息落入对话），`running = false`，输入框恢复

### Tab 补全子代理名

在输入框键入 `@` 后按 `Tab`，自动补全已注册的子代理名：

```
@gen[Tab] → @general-purpose 
```

补全逻辑在 `cycleCompletion()` 中处理，以 `@` 守卫与 `/` 斜杠命令补全并列，共享同一套 `typedPrefix / completions / completionIdx` 循环状态，多次 `Tab` 可在所有匹配名称中循环。

### Ctrl+C 取消

`@agent` 执行期间按 `Ctrl+C`：`cancelFn()` 取消派生的子 context，Runner 中 `execCtx.Done()` 触发，子引擎 `RunStream` 随之退出；`subAgentDirectMsg{done: true, err: ctx.Err()}` 经 channel 发回 TUI，`running = false`，输入框恢复。

### 前台 vs 后台

`@` 语法**仅支持前台执行**（`background=false`）。

需要后台执行时，通过自然语言向主代理表达意图（如「在后台用 general-purpose 检查一下最新提交」），由主 LLM 决策调用 `task` 工具并附 `background=true`，结果出现在 `/tasks` 面板。

| 维度 | `@agent task`（前台直跑） | 主 LLM → `task(background=true)` |
|------|--------------------------|-----------------------------------|
| 触发方 | 用户直接输入 | 主 LLM 工具决策 |
| 主 LLM 是否介入 | 否，完全绕过 | 是，由 LLM 选择子代理和 prompt |
| 执行模式 | 前台阻塞，流式进度可见 | 后台异步，结果存入 TaskTracker |
| 结果落点 | 直接展示在 scrollback | `/tasks` 面板 + 下次 dispatch 注入 |
| 取消 | `Ctrl+C` 即时取消 | baseCtx 取消（进程关闭）才取消 |

---

## 数据流总结

```
主代理 LLM
    │  决定调用 task 工具
    ▼
TaskTool.Execute(ctx, args)
    │  解析 subagent_type / prompt / background
    ▼
Runner.Run(ctx, def, prompt, background)
    ├─ buildChildRegistry(def)
    │       ResolveTools → 白名单∩全集 - 黑名单 - task
    │       hookChain: permFileHook → denyTaskHook → dangerHook → offloadHook
    │
    ├─ providerFor(def.Model) → LLMProvider + ctxWindow
    │
    ├─ newPromptBuilder(def.SystemPrompt, workDir, def.Skills, skillsLoader)
    │       systemPrompt + workDir + skills 正文
    │
    ├─ memory.NewMemorySession(childID)   # 独立纯内存 Session
    │
    └─ engine.NewAgentEngine(provider, childReg, workDir, opts...)
           │
           sub.RunStream(execCtx, prompt)
           │
           ▼
       事件流消费循环
           ├─ EventActionDelta   → emit(SubAgentDelta)   → EventSubAgent → TUI 进度行
           ├─ EventThinkingDelta → emit(SubAgentThinking)  （TUI 不展示）
           ├─ EventToolStart     → emit(SubAgentToolStart) → TUI 进度行
           ├─ EventToolResult    → emit(SubAgentToolResult)→ TUI 进度行
           ├─ EventApprovalRequired → 前台:透传父 ApprovalFunc / 后台:自动拒绝
           ├─ EventError         → emit(SubAgentError) → 返回 error
           └─ EventDone          → channel 关闭，循环自然结束
                   │
    前台: return FinalText → task tool result → 父代理上下文
    后台: tracker.AppendLog(id, update)（流式，全过程日志入内存）
          tracker.Finish(id, finalText, isErr)
              → 下次 dispatch() → DrainCompleted() → prompt 前缀注入 → 主代理 LLM
```

---

## 接线示例（main.go）

```go
// 1. 子代理基础工具实例（沙箱根目录 = workDir）
subAgentBaseTools := []tools.BaseTool{
    tools.NewReadFileTool(workDir),
    tools.NewWriteFileTool(workDir),
    tools.NewBashTool(workDir),
    tools.NewEditFileTool(workDir),
    tools.NewGlobTool(workDir),
    tools.NewGrepTool(workDir),
    skills.NewUseSkillTool(skillsIndex),
}

// 2. 定义注册表：先注册六个内置，再加载文件式定义（文件可覆盖同名内置）
subAgentReg := subagent.NewRegistry()
if err := subagent.RegisterBuiltins(subAgentReg); err != nil { /* … */ }
subAgentReg.LoadFromDir(filepath.Join(workDir, ".harness9", "agents"))

// 3. Runner：全局持有一份，运行期只读
subAgentTracker := subagent.NewTaskTracker()
subAgentRunner := subagent.NewRunner(subagent.RunnerConfig{
    BaseTools:       subAgentBaseTools,
    SharedHooks:     []hooks.ToolHook{dangerHook, offloadHook},
    SettingsPath:    settingsPath,
    SkillsIndex:     skillsIndex,
    WorkDir:         workDir,
    DefaultMaxTurns: agentMaxTurns, // = 主代理 500，子代理与主代理一致
    ToolTimeout:     60 * time.Second,
    ProviderFor:     func(model string) (provider.LLMProvider, int, error) { ... },
    CompactorFor:    func(p provider.LLMProvider, ctxWin int) memory.Compactor { ... },
    BaseCtx:         ctx,
})

// 4. 注册 task 工具 + 协调三工具进父代理 registry
//    （task_wait 的 baseCtx 用会话级 ctx——绕过父 Turn 的 60s 工具超时，Ctrl+C 仍可传播）
taskTool := subagent.NewTaskTool(subAgentReg, subAgentRunner, subAgentTracker)
registry.Register(taskTool)
for _, ct := range []tools.BaseTool{
    subagent.NewTaskStatusTool(subAgentTracker),
    subagent.NewTaskWaitTool(subAgentTracker, ctx),
    subagent.NewTaskControlTool(subAgentTracker),
} {
    registry.Register(ct)
}
```

---

## 文件索引

| 文件 | 职责 |
|------|------|
| `internal/subagent/definition.go` | `SubAgentDefinition` 结构体、`Validate`、`ResolveTools`（含 `alwaysDeniedTools`：task 家族四工具永久剥离） |
| `internal/subagent/registry.go` | `Registry`：`Register` / `Get` / `List` |
| `internal/subagent/frontmatter.go` | `parseAgentFile`：YAML frontmatter 解析 |
| `internal/subagent/loader.go` | `Registry.LoadFromDir`：文件式定义加载 |
| `internal/subagent/builtin.go` | `RegisterBuiltins`：六个内置子代理定义 |
| `internal/subagent/prompt.go` | `promptBuilder`：system prompt + skills + workDir 组装 |
| `internal/subagent/tracker.go` | `TaskTracker`：后台任务单一事实源（全过程日志 + 结果注入 + `Control` 控制路由） |
| `internal/subagent/control.go` | `TaskController`：控制平面（实现 `engine.TaskGate`，Pause/Resume/Cancel/Steer + `AwaitTurn` 门控） |
| `internal/subagent/runner.go` | `Runner`：构建隔离子引擎 + 执行 + 事件转发 + `WithTaskGate` / `bindExec` 接线 |
| `internal/subagent/task_tool.go` | `TaskTool`：`task` 工具实现（前台 / 后台，后台路径创建 controller 并 Attach） |
| `internal/subagent/task_status.go` | `TaskStatusTool`：`task_status` 观察工具 |
| `internal/subagent/task_wait.go` | `TaskWaitTool`：`task_wait` join 等待工具（超时返回快照非 error） |
| `internal/subagent/task_control.go` | `TaskControlTool`：`task_control` 控制工具（状态机错误以文本返回） |
| `internal/engine/task_gate.go` | `TaskGate` 接口 + `WithTaskGate`：轮边界控制门（接口定义在使用者侧） |
| `internal/schema/subagent.go` | `SubAgentUpdate` / `SubAgentUpdateKind` 类型定义（含 Paused / Resumed / Cancelled） |
| `internal/hooks/subagent_progress.go` | `SubAgentProgressFunc`：context 注入/提取 |
| `internal/engine/stream.go` | `EventSubAgent`、`EventApprovalRequired`、进度 sink 注入 |
| `internal/context/builder.go` | `WithDelegationGuide`：子代理委派准则段注入 system prompt |
| `cmd/harness9/main.go` | 完整接线：`RegisterBuiltins`、Runner 构建、task + 协调三工具注册、`WithDelegationNudge` |
| `cmd/harness9/tui_update.go` | `EventSubAgent` 处理、`DrainCompleted` 注入、`maybeAutoWake` 自动唤醒、`dispatchMention`（@ 前台直跑）、`handleTaskPanelKey`（任务面板按键 + 转向输入态） |
| `cmd/harness9/tui_view.go` | `renderSubAgentProgress()`（暗青色进度块）、`renderTaskPanel()`（面板列表/详情，五状态着色）、`renderStatusBar()` 中后台任务计数 |
