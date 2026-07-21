---
title: "Planning 模块：Plan Mode、TodoStore 与执行自动化"
date: 2026-06-08
tags: [harness9, agent, golang, planning, todo, stagnation-detection]
summary: "harness9 的 Planning 模块如何用工具层硬约束替代 prompt 层软约束，以及 TodoStore 的全量替换语义、防作弊校验与跨 runLoop 的停滞检测机制。"
---

# Planning 模块：工具层权限门禁、TodoStore 状态机与执行自动化

## 关于 harness9

harness9 是一款 Local-First、轻量级、功能完备、生产可用的通用 Go Agent 框架。

- **官网**：[https://zhangshenao.github.io/harness9/](https://zhangshenao.github.io/harness9/)
- **GitHub**：[https://github.com/ZhangShenao/harness9](https://github.com/ZhangShenao/harness9)

⭐ Star 是对开源工作最直接的支持，欢迎提 Issue 和 PR。

---

## TL;DR

harness9 的 Planning 模块把"LLM 能做什么"的控制权从 prompt 下沉到工具 schema，把"LLM 有没有真正在干活"的校验从运行时观察变成前置拒绝，把"执行卡住了"的判断从人工干预变成停滞计数器。三件事各找最合适的层来做，没有上移也没有下移。

## 本文你将学到

- 你将看清 Plan Mode 为什么用工具层白名单硬过滤，而不是在 prompt 里说"不要创建文件"——以及这个区别在 Agent 工程中意味着什么
- 你将理解 TodoStore 为什么选择全量替换而非增量 API，以及"双重 copy"策略背后的数据竞争考量
- 你将看到 `todo_write` 防作弊校验如何从一个真实 bug（11 个任务被一次性批量完成）演化为"阈值 1"的设计决策
- 你将理解停滞检测（stagnation detection）为什么用 `done` 计数而非 `pending` 计数来判断进度
- 你将掌握 FilePlanWriter 的路径策略：git 项目与非 git 项目的持久化位置差异及其原因

---

## Plan Mode：一扇门，不是一句话

大多数 Agent 框架在"规划阶段"的实现方式是在 prompt 里加一段话："现在你处于规划阶段，不要修改文件，只做分析。" 这是软约束（soft constraint）。LLM 可以忘记它，可以被历史上下文里的工具用例"诱导"绕过它，可以在上下文压缩后丢失它。

harness9 的做法是从工具 schema 里把写工具直接拿掉。

```go
// internal/engine/agent_loop.go
var planModeWhitelist = map[string]bool{
    "read_file":  true,
    "bash":       true,
    "use_skill":  true,
    "todo_write": true,
}

func filterReadOnlyTools(tools []schema.ToolDefinition) []schema.ToolDefinition {
    var result []schema.ToolDefinition
    for _, t := range tools {
        if planModeWhitelist[t.Name] {
            result = append(result, t)
        }
    }
    return result
}
```

`write_file` 和 `edit_file` 不在白名单里。Plan Mode 下，LLM 收到的工具列表里根本不存在这两个工具——它在 API 层就不存在了，而不是"存在但被要求不要用"。

这是工具层硬约束（hard constraint）与 prompt 层软约束的本质差异：前者是物理限制，后者是行为建议。

![图：Plan Mode 权限门禁架构](./images/plan-mode-gatekeeper-01.png)


`filterReadOnlyTools` 在 `runLoop` 内部每个 Turn 开始时调用，而 `planMode` 本身在 `runLoop` 入口被快照：


```go
// agent_loop.go — runLoop 入口
e.mu.RLock()
planMode := e.planMode   // 快照一次，整个循环内不变
e.mu.RUnlock()

// 每个 Turn 开始时
if planMode == planning.PlanModePlan {
    availableTools = filterReadOnlyTools(availableTools)
}
```

快照的意义：TUI goroutine 可以在任何时候调用 `eng.SetPlanMode()`，但正在运行的 `runLoop` 已经拿到了开始时的模式副本，不会在循环中途被切换。这是 harness9 处理 goroutine 间状态一致性的惯用手法——不是加锁保护整个循环，而是在入口快照，循环内读只读变量。

工具层过滤之外，`runLoop` 还对用户 prompt 注入了行为引导前缀：

```go
if planMode == planning.PlanModePlan {
    userPrompt = "分析以下请求，用 todo_write 输出一份可直接执行的实现计划，然后用纯文字简述计划后停止。\n" +
        // ...
        "不要创建文件、执行 build/install 或做任何实际修改。\n\n" +
        userPrompt
}
```

注意措辞：prompt 说的是"不要这么做"，而不是"你没有权限这么做"。权限由工具层决定，prompt 只引导行为。两层分工清晰，互不越权。

---

## TodoStore：全量替换的设计取舍

`TodoStore` 是一个线程安全的内存任务列表，但它的 API 设计是反直觉的——它没有 `Add`、`Update`、`Delete`，只有 `Write` 和 `Read`。

```go
// internal/planning/todo.go
type TodoStore struct {
    mu    sync.RWMutex
    items []TodoItem
}

func (s *TodoStore) Write(items []TodoItem) []TodoItem {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.items = make([]TodoItem, len(items))
    copy(s.items, items)
    return s.copy()
}
```

为什么全量替换而非增量 API？

LLM 调用 `todo_write` 时，它的自然输出形式是完整的任务列表，而不是"把第 3 项的 status 从 pending 改为 in_progress"这样的增量指令。增量 API 要求 LLM 对当前状态有精确的认知——ID 拼错了，状态就发散了。全量替换则不依赖 LLM 对历史状态的记忆，每次写入都是一个确定性快照。

实现简单是次要好处：`Write` 方法 5 行代码，没有合并逻辑，没有冲突处理。

`Write` 的双重 copy 策略值得注意：

```go
// 第一次 copy：入参 items 与内部存储解耦
s.items = make([]TodoItem, len(items))
copy(s.items, items)
// 第二次 copy（s.copy()）：返回值与内部存储解耦
return s.copy()
```

调用方传进来的 `items` 切片、`TodoStore` 内部的 `s.items`、返回给调用方的副本，三者各自独立。如果直接 `s.items = items`，调用方后续修改原切片就会悄悄影响 `TodoStore` 内部状态。这类 bug 在并发环境下往往是间歇性的，极难复现。双重 copy 用 20 字节的内存代价换来了确定性的隔离。

![图：TodoStore 状态机与全量替换语义](./images/todostore-state-machine-02.png)



状态转换约束刻意没有放在 `TodoStore` 里：

```go
// TodoStatus 状态转换约束由 todo_write 工具（tools 包）负责执行，TodoStore 本身不做校验。
```

`TodoStore` 是无判断的存储层，业务约束由工具层表达。这个分层是蓄意的——`TodoStore` 可以被测试代码直接写入任意状态，不需要绕过校验逻辑；而工具层的校验逻辑可以独立变化，不需要改动存储层。

---

## todo_write：防作弊的工程故事

`todo_write` 工具的防作弊校验不是从设计文档里推导出来的，它来自一个具体的 bug。

在一次连续对话中，LLM 将 11 个任务中的 9 个一次性批量完成（从 2/11 跳到 11/11），没有对应的文件创建或 bash 执行操作。这是"幻觉执行"——LLM 省略了实际工作，直接伪造进度。

修复策略是：在一次 `todo_write` 调用中，最多允许 1 个任务从非 `in_progress` 状态直接跳转到 `completed`：

```go
// internal/tools/todo_write.go — Execute()
prev := t.store.Read()
prevStatus := make(map[string]planning.TodoStatus, len(prev))
for _, item := range prev {
    prevStatus[item.ID] = item.Status
}

var directCompletions int
for _, item := range input.Todos {
    if item.Status != planning.TodoCompleted {
        continue
    }
    prior, exists := prevStatus[item.ID]
    if !exists || prior == planning.TodoPending {
        directCompletions++  // pending → completed，计入
        continue
    }
    if prior == planning.TodoCancelled {
        return "", fmt.Errorf("任务 %q 已取消，不能直接标记为 completed...", item.ID)
    }
    // in_progress → completed：合法，不计入
}
if directCompletions > 1 {
    return "", fmt.Errorf(
        "不允许在一次调用中将 %d 个任务直接标记为 completed（未经 in_progress）...",
        directCompletions)
}
```

阈值为什么是 1 而不是 0？

阈值 0 在续跑场景中会产生误伤：Agent 在一次续跑中完成了一项真实工作（调用了 bash 或 write_file），然后直接把对应 todo 标记为 `completed` 而没有经过 `in_progress` 中间步骤——这是正当行为，Agent 省略了状态标记的中间步骤，但工作是真实的。阈值 0 会导致 Agent 反复收到拒绝错误并陷入重试循环。

阈值 1 保留了对原始 bug 模式（大量批量完成）的防护，同时允许单项直接完成这一正常用法。

校验失败时，`todo_write` 返回 `error`，引擎将其包装为 `ToolResult{IsError: true}` 注入上下文。LLM 看到工具调用失败的错误信息，被迫重新组织参数。循环不会终止，Agent 自己修正自己——这是 harness9"自愈"（self-healing）设计的标准模式。

![图：todo_write 防作弊双重防护](./images/todo-write-anticheat-03.png)


---

## 执行 Prompt 的设计意图

用户批准计划后，TUI 不是简单地发送"开始执行"，而是发送一段精心设计的规范：

```go
// cmd/harness9/tui_update.go
const execPrompt = "按照 todo 清单逐项执行。规则：\n" +
    "1. 每开始一项前，用 todo_write 将其状态设为 in_progress\n" +
    "2. 用工具完成该项的实际工作——创建文件、写代码、运行命令等；" +
    "仅更新 todo_write 状态而不调用其他工具，不算完成该项\n" +
    "3. 确认实际产出后，用 todo_write 将其状态设为 completed\n" +
    "4. 不要输出进度摘要文字，立即处理下一项\n" +
    "全部完成后，用一句话汇报整体结果。"
```

规则 2 是关键："仅更新状态而不调用其他工具，不算完成该项。" 这是 prompt 层对抗幻觉执行的约束，与工具层的批量完成检测形成双重防护。一层是硬拒绝，一层是行为引导——两层都在防同一件事，但机制不同。

续跑时用更精简的 `execContinuePrompt`：

```go
const execContinuePrompt = "继续处理 todo 清单中下一个 pending 或 in_progress 的任务项。" +
    "先用 todo_write 标记为 in_progress，然后用工具完成实际工作（写文件、执行命令等），" +
    "确认产出后标记为 completed，再处理下一项。" +
    "不要只更新状态而不做实际操作，不要输出进度摘要。"
```

续跑不需要重复完整规则——LLM 的上下文里有 `execPrompt` 的历史，已知晓基本框架。精简版只需要提示"继续下一项"，减少无效 token 消耗。

---

## 停滞检测：用 done 计数，而非 pending 计数

自动执行（autoExecuting）模式下，每次 `EventDone` 触发以下决策：

```go
// cmd/harness9/tui_update.go — EventDone handler
if m.autoExecuting && m.todoStore != nil {
    items := m.todoStore.Read()
    var pending, done int
    for _, item := range items {
        switch item.Status {
        case planning.TodoPending, planning.TodoInProgress:
            pending++
        case planning.TodoCompleted:
            done++
        }
    }
    if pending > 0 {
        if done > m.autoExecPrevDone {
            m.autoExecStuck = 0  // 有进度，重置
        } else {
            m.autoExecStuck++   // 无进度，计数
        }
        if m.autoExecStuck < 3 {
            m.autoExecPrevDone = done
            return m.dispatch(execContinuePrompt)
        }
        m.autoExecuting = false
        m.lines = append(m.lines, dimStyle.Render("  ⚠ 执行停滞，请手动描述下一步"))
    } else {
        m.autoExecuting = false  // 全部完成
    }
}
```

停滞检测的判断基准是 `done`（已完成数）而非 `pending`（待完成数）。这个选择值得展开说。

`pending` 的变化有两种来源：任务真正完成（`pending → in_progress → completed`）和任务被标记为进行中（`pending → in_progress`）。如果用 `pending` 减少来判断进度，LLM 只要不断把任务改成 `in_progress` 而不真正完成，就能持续通过进度检测——这是另一种形式的幻觉执行。

只有 `completed` 状态才代表真实的工作产出。`done` 计数在一轮 `EventDone` 后没有增加，意味着 LLM 运行了一整轮推理但没有推进任何任务到完成状态。连续 3 次如此，停滞检测介入。

阈值 3 是经验值：给 LLM 一些缓冲空间应对需要多轮探索才能完成的复杂任务，但不允许无限空转。

![图：停滞检测决策流](./images/stagnation-detection-04.png)



`dispatch()` 本身内置了并发保护：

```go
func (m tuiModel) dispatch(prompt string) (tuiModel, tea.Cmd) {
    if m.running {
        return m, nil  // 已有推理在进行，静默忽略
    }
    // ...
}
```

autoExecuting 续跑时，`dispatch` 由 `EventDone` handler 在 Elm Update 单线程循环内调用，不存在并发问题。`running` 检查是额外安全网，防止其他代码路径意外触发双路推理。

---

## FilePlanWriter：不只是写文件

每次 `todo_write` 工具成功写入后，如果注入了 `FilePlanWriter`，任务列表会被持久化为 Markdown 文件：

```go
// internal/hooks/plan_writer.go
func NewFilePlanWriter(workDir, homeDir, sessionID string) (*FilePlanWriter, error) {
    timestamp := time.Now().Unix()
    slug := sessionID[:8]
    filename := fmt.Sprintf("%d-%s.md", timestamp, slug)

    var base string
    if isGitRepo(workDir) {
        base = filepath.Join(workDir, ".harness9", "plans")
    } else {
        base = filepath.Join(homeDir, ".harness9", "plans")
    }
    // ...
}
```

路径策略有一个简单但有意思的分支：`isGitRepo(workDir)` 检测工作目录是否含有 `.git`。

git 项目写入 `workDir/.harness9/plans/`——这个路径在项目目录下，可以被纳入版本控制，也可以通过 `.gitignore` 排除。把规划产物放在项目旁边，让任务状态与代码变更保持上下文关联。

非 git 项目写入 `homeDir/.harness9/plans/`——没有项目目录的概念，集中存放在 home 目录下的个人数据区，不污染当前工作目录。

这个判断是在构造时做的，不是在每次写入时做的。`isGitRepo` 调用一次，路径固定下来，`Write` 方法每次覆写同一个文件而不是重新计算路径。

`PlanWriter` 接口定义在 `planning` 包而非 `hooks` 包：

```go
// internal/planning/plan_writer.go
type PlanWriter interface {
    Write(todos []TodoItem) error
}
```

这是 harness9 一贯的接口位置原则：接口定义在使用者侧，而非实现者侧。`TodoWriteTool` 使用 `PlanWriter`，接口就定义在 `planning` 包。`FilePlanWriter` 实现这个接口，但接口不在 `hooks` 包里声明。这个选择的实际作用是切断了 `tools` 包对 `hooks` 包的依赖——如果接口在 `hooks` 包，`tools` 就必须 import `hooks`，而 `hooks` 又会 import `tools`，循环导入立刻出现。

![图：FilePlanWriter 路径策略与接口位置](./images/file-plan-writer-05.png)



---

## 跨 runLoop 的状态连续性

`TodoStore` 的内容随 Session 持久化到 SQLite，每次 `runLoop` 启动和结束时自动同步：

```go
// agent_loop.go — runLoop
// 启动：从 Session 恢复
if sess != nil && todoStore != nil {
    if todos, err := sess.GetTodos(ctx); err == nil {
        todoStore.Write(todos)
    }
}

// 结束：defer 保证所有退出路径都执行
defer func() {
    if sess != nil && todoStore != nil {
        if err := sess.SaveTodos(ctx, todoStore.Read()); err != nil {
            log.Print(...)
        }
    }
}()
```

`autoExecuting` 模式下，每次续跑都是一次独立的 `runLoop` 调用。每次 `runLoop` 启动时从 DB 恢复 `TodoStore`，结束时写回——这确保了 `todo_write` 防作弊校验的正确性：`pending` 的任务在上次运行后保存到 DB，下次运行时加载回内存，`prevStatus` 快照能准确反映任务的历史状态。如果不做持久化，跨 `runLoop` 的状态对照就会失效，批量完成检测就成了哑炮。

`defer` 是关键细节：不管 `runLoop` 因为自然终止（LLM 不再调用工具）、MaxTurns 超限还是 context 取消而退出，`SaveTodos` 都会执行。

上下文压缩时，活跃任务会随摘要一起注入：

```go
// internal/memory/summarization.go — Compact()
if c.TodoInjector != nil {
    if todoText := c.TodoInjector.FormatForInjection(); todoText != "" {
        summaryContent += "\n\n## Active Tasks\n" + todoText
    }
}
```

压缩后的摘要消息末尾会追加：

```
## Active Tasks
[ ] 实现 handler/user.go
[>] 配置数据库连接
[ ] 添加路由注册
```

即使对话历史被压缩得面目全非，未完成的任务也不会从 LLM 的视野中消失。

![图：Planning 完整数据流：从 Shift+Tab 到任务完成](./images/planning-full-journey-06.png)



---

## todo_write 工具的设计细节

`todo_write` 是 Planning 模块对 LLM 暴露的唯一任务管理接口。它的设计值得多看一眼，因为细节里藏着几个有意思的工程决策。

### 双模式：一个工具，两种调用语义

`todo_write` 的参数定义只有一个字段：`todos`。但这个字段有两种完全不同的语义：

```go
// internal/tools/todo_write.go
type todoWriteArgs struct {
    Todos []planning.TodoItem `json:"todos"`
}

// Execute：通过 len(input.Todos) > 0 区分读写模式
if len(input.Todos) > 0 {
    // 写操作：全量替换 + 防作弊校验
    current = t.store.Write(input.Todos)
} else {
    // 读操作：返回当前快照，不修改状态
    current = t.store.Read()
}
```

省略 `todos` 字段或传空数组，工具变成只读查询；传入非空数组，工具执行全量替换。两种模式复用同一个工具注册名，LLM 不需要区分"读取 todo"和"写入 todo"两个工具——一个工具，用参数控制行为。

这不只是为了简洁。工具列表的长度会消耗 LLM 的上下文窗口，也影响模型对工具选择的分发准确性。在工具数量本来就不少的情况下，把读写合并进一个工具是减少认知负担的务实选择。

### Schema 里的状态机

`todo_write` 的 JSON Schema 把 `status` 字段定义为有限枚举：

```go
"status": map[string]interface{}{
    "type": "string",
    "enum": []string{"pending", "in_progress", "completed", "cancelled"},
},
```

四个合法值，其他值不会被提交到 API。这是把状态机的合法集合下推到 Schema 层——不需要在 `Execute` 里做枚举校验，模型在调用工具时就已经被约束在合法状态范围内了。

这和 Plan Mode 的工具过滤是同一个思路的不同粒度：Plan Mode 在工具列表层面做约束（某些工具整体不可见），Schema 在参数层面做约束（某个字段的合法值有限）。两者都在"工具定义"这一层动手，不依赖 prompt 里的措辞。

### nil 到 `[]` 的规范化

`Execute` 的返回路径有一个细节：

```go
if current == nil {
    current = []planning.TodoItem{}
}
b, err := json.Marshal(current)
```

`json.Marshal(nil)` 产生 `"null"`，`json.Marshal([]planning.TodoItem{})` 产生 `"[]"`。两者对 Go 程序来说语义等价，但对 LLM 来说差异很大——`null` 是一个无结构的值，`[]` 是一个明确的空列表。LLM 需要知道"当前没有任务"而不是"任务列表字段不存在"，这一个字符的差异决定了 LLM 能否正确推断下一步行为。

### WithPlanWriter：可选注入而非必选依赖

`TodoWriteTool` 通过 Option 模式注入 `PlanWriter`：

```go
// internal/tools/todo_write.go
type TodoWriteOption func(*TodoWriteTool)

func WithPlanWriter(pw planning.PlanWriter) TodoWriteOption {
    return func(t *TodoWriteTool) { t.planWriter = pw }
}
```

`planWriter` 字段默认为 `nil`，不注入时跳过持久化，工具本身仍然可用。这意味着 `TodoWriteTool` 在单元测试中可以直接实例化，不需要构造一个真实的 `FilePlanWriter`——测试只需要验证任务列表的状态变化，不需要关心文件系统。

Option 模式在 harness9 里是构造函数的标准约定（`WithMaxTurns`、`WithToolTimeout` 等都是这个模式），`WithPlanWriter` 遵循了同样的设计语言，新读者不需要额外学习就能理解注入语义。

持久化失败时的处理也值得注意：

```go
if t.planWriter != nil {
    if err := t.planWriter.Write(current); err != nil {
        log.Print(logfmt.FormatMsg("todo_write", fmt.Sprintf("写入计划文件失败: %v", err)))
    }
}
```

写文件失败只记日志，不向 LLM 回传错误。这是 fail-open 策略——持久化是辅助功能，不是任务管理的核心路径。如果 `FilePlanWriter` 因磁盘满或权限问题失败，任务列表本身已经写入 `TodoStore`（内存中），Agent 可以继续运行，只是这次的规划产物不会落盘。反过来如果 `planWriter.Write` 的失败被 propagate 给 LLM，会导致 Agent 进入错误恢复循环，为一个非核心功能的失败付出不必要的代价。

---

## 结语

Planning 模块的真正价值不是"给 Agent 加了个规划阶段"，而是把 Agent 行为的几个关键约束点从软层（prompt）挪到了硬层（代码）。每一个挪移都需要一个理由：为什么这件事不能靠 prompt 说清楚？答案通常是：prompt 可以被忘记、被压缩、被绕过——代码不会。

思考题：`todo_write` 的防作弊阈值是 1，如果改成 2 会影响什么场景？改成 0 又会影响什么？
