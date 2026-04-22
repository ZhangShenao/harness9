# Agent Loop 核心实现原理

## 1. 架构总览

harness9 的核心是一个 **Two-Stage ReAct** 循环引擎，将传统 Reasoning → ToolCall → Observation 三阶段升级为 **Thinking → Action → Observation** 模式。引擎编排三个核心抽象协同工作：

```
┌──────────────────────────────────────────────────────────────────────┐
│                         AgentEngine                                   │
│               (核心编排器 / Two-Stage ReAct Loop)                      │
│                                                                      │
│  ┌────────────────────────────────────────────────────────────────┐  │
│  │                    每个 Turn 的两阶段流程                        │  │
│  │                                                                │  │
│  │  Phase 1: Thinking                                              │  │
│  │  ┌───────────────┐  Generate(tools=nil)  ┌───────────────┐    │  │
│  │  │  Context       │ ───────────────────── │  LLMProvider   │    │  │
│  │  │  History       │ ◄──── 纯推理文本 ───── │  (深度思考)     │    │  │
│  │  └───────┬───────┘                       └───────────────┘    │  │
│  │          │ (临时注入，不持久化)                                │  │
│  │          ▼                                                      │  │
│  │  Phase 2: Action                                                │  │
│  │  ┌───────────────┐  Generate(tools=all)  ┌───────────────┐    │  │
│  │  │  phase2History │ ────────────────────► │  LLMProvider   │    │  │
│  │  │  (含临时思考)   │ ◄── 文本+ToolCalls ── │  (采取行动)     │    │  │
│  │  └───────┬───────┘                       └───────────────┘    │  │
│  │          │                                                      │  │
│  │          │ 合并为单条 assistant 消息                              │  │
│  │          │ → 注入到 contextHistory                               │  │
│  │          │                                                      │  │
│  │          │ ToolCalls                                             │  │
│  │          ▼                                                      │  │
│  │  ┌───────────────┐  Execute()  ┌───────────────┐              │  │
│  │  │  Observation   │ ◄────────── │  Registry      │              │  │
│  │  │  (工具结果)     │             │  (双手 / 工具) │              │  │
│  │  └───────────────┘             └───────────────┘              │  │
│  └────────────────────────────────────────────────────────────────┘  │
│                                                                      │
└──────────────────────────────────────────────────────────────────────┘
```

| 组件 | 代码位置 | 职责 |
|------|---------|------|
| `schema` | `internal/schema/message.go` | 定义跨组件共享的核心数据类型 |
| `LLMProvider` | `internal/provider/interface.go` | 抽象 LLM 通信层，封装 API 差异 |
| `Registry` | `internal/tools/registry.go` | 解耦工具发现与执行 |
| `AgentEngine` | `internal/engine/agent_loop.go` | 编排 Two-Stage ReAct 主循环，驱动各组件协作 |

## 2. Two-Stage ReAct 设计理念

### 2.1 为什么需要两阶段

传统 ReAct 循环在每个 Turn 中执行一次 LLM 调用，让模型同时完成推理和行动。这在复杂任务中容易出现"未经深思的冲动行为"——模型在充分理解问题之前就急于调用工具。

```
传统 ReAct（单阶段）:
  Turn N: LLM(messages, tools) → 立即行动（可能缺乏深思熟虑）

Two-Stage ReAct（harness9）:
  Turn N: 
    Phase 1: LLM(messages, tools=nil) → 被迫深度思考
    Phase 2: LLM(messages + thinking, tools=all) → 基于思考的精准行动
```

### 2.2 核心机制：剥夺-恢复工具策略

harness9 通过控制 `Generate` 调用时的 `availableTools` 参数来实现阶段分离：

```go
// Phase 1: Thinking — 传入 nil，剥夺所有工具
thinkResp, err := e.provider.Generate(ctx, contextHistory, nil)

// Phase 2: Action — 传入完整工具列表，恢复行动能力
actionResp, err := e.provider.Generate(ctx, phase2History, availableTools)
```

**为什么有效？** 当 LLM 没有任何工具可用时，它只有两个选择：
1. 进行深度推理和分析（纯文本输出）
2. 什么都不做（空输出）

模型无法"偷懒"直接调用工具试错，必须先在认知层面理清问题，制定行动计划。

### 2.3 上下文一致性保证

**核心问题**：如果 Phase 1 的思考作为一条 assistant 消息注入，Phase 2 的行动也作为一条 assistant 消息注入，会导致上下文中出现**连续两条 assistant 消息**。Anthropic Messages API 要求 user/assistant 严格交替，连续 assistant 消息会导致 API 报错。

**解决方案**：Thinking + Action 合并为单条 assistant 消息。

```
错误的上下文结构（连续 assistant 消息）:
  system → user → assistant(thinking) → assistant(action) → ...

正确的上下文结构（合并后）:
  system → user → assistant(thinking + action, ToolCalls) → ...
```

实现方式：

```go
// 1. Phase 1 思考 → 临时注入到 phase2History（不持久化到 contextHistory）
phase2History := make([]schema.Message, len(contextHistory), len(contextHistory)+1)
copy(phase2History, contextHistory)
phase2History = append(phase2History, *thinkResp)

// 2. Phase 2 行动 → 基于临时上下文生成
actionResp, err := e.provider.Generate(ctx, phase2History, availableTools)

// 3. 合并为单条 assistant 消息 → 持久化到 contextHistory
mergedMsg := &schema.Message{
    Role:      schema.RoleAssistant,
    Content:   joinContent(thinkResp.Content, actionResp.Content),
    ToolCalls: actionResp.ToolCalls,
}
contextHistory = append(contextHistory, *mergedMsg)
```

**关键设计要点：**

| 要点 | 说明 |
|------|------|
| 临时上下文 `phase2History` | Phase 1 思考仅在 Phase 2 调用期间存在，调用结束后丢弃 |
| 单条消息持久化 | 只有合并后的 assistant 消息进入 contextHistory |
| 内容保留 | 合并消息同时包含思考文本和行动文本，后续 Turn 的 LLM 仍能看到完整上下文 |
| API 兼容性 | 保证 user/assistant 严格交替，兼容 Anthropic / OpenAI 等主流 API |

### 2.4 Phase 1 安全清除

即使 LLM 在 Thinking 阶段（tools=nil）不应返回 ToolCalls，引擎仍会防御性清除：

```go
thinkResp.ToolCalls = nil // 安全清除：防止 LLM 不遵守指令时污染上下文
```

### 2.5 模式切换

`EnableThinking` 配置项控制是否启用两阶段模式：

| 模式 | EnableThinking | 每 Turn LLM 调用次数 | 适用场景 |
|------|:-:|:-:|------|
| Two-Stage ReAct | `true` | 2 次 | 复杂任务，需要深度规划 |
| 标准 ReAct | `false` | 1 次 | 简单任务，追求速度和效率 |

## 3. 数据模型 (`internal/schema`)

### 3.1 消息角色体系

```
Role (string)
├── "system"     → 系统提示词：定义 Agent 身份、约束与行为边界
├── "user"       → 用户输入 & 工具执行结果 (Observation)
└── "assistant"  → 模型输出：Thinking 推理文本 + 行动文本 + 工具调用请求
```

在 Two-Stage ReAct 模式下，每个 Turn 只产生一条 assistant 消息（Thinking + Action 合并）。

### 3.2 核心类型关系

```
┌──────────────────────────────────────────────────┐
│  Message                                         │
│  ├── Role        Role        消息作者角色          │
│  ├── Content     string      纯文本内容            │
│  ├── ToolCalls   []ToolCall  模型发出的工具调用请求  │
│  └── ToolCallID  string      关联原始 ToolCall 的 ID│
│                                                  │
│  ToolCall                 ToolResult              │
│  ├── ID         string     ├── ToolCallID  string │
│  ├── Name       string     ├── Output      string │
│  └── Arguments  RawMessage └── IsError      bool  │
│                                                  │
│  ToolDefinition                                  │
│  ├── Name        string   工具唯一标识             │
│  ├── Description string   用途描述                │
│  └── InputSchema RawMessage 参数 JSON Schema      │
└──────────────────────────────────────────────────┘
```

**关键设计决策：**

- **`ToolCall.Arguments` 使用 `json.RawMessage`**：延迟反序列化，将参数解析责任交给具体工具实现。
- **`ToolDefinition.InputSchema` 使用 `json.RawMessage`**：与 Arguments 保持一致的反序列化策略，避免 `interface{}` 类型在序列化时的不可控行为。
- **`ToolCallID` 关联机制**：工具执行结果 (Observation) 通过 `ToolCallID` 与原始 `ToolCall` 关联。
- **`ToolResult.IsError` 自愈标记**：当工具执行失败时，引擎将错误暴露给 LLM，使其能尝试修正参数并重试（Self-Healing）。

## 4. Agent Loop 循环流程

```
                     ┌─────────────────────┐
                     │   初始化对话上下文     │
                     │   System(含WorkDir)  │
                     │   + User             │
                     └──────────┬──────────┘
                                │
                ┌───────────────▼───────────────┐
                │   Turn 计数 ++                  │
                │   检查 MaxTurns / ctx.Done()   │
                └───────────────┬───────────────┘
                                │
               ┌────────────────▼────────────────┐
               │   EnableThinking == true ?       │
               └───────┬───────────────┬─────────┘
                   Yes │               │ No
                       ▼               │
           ┌───────────────────┐       │
           │  Phase 1: Thinking │       │
           │  Generate(nil)    │       │
           │  → 清除 ToolCalls │       │
           │  → 临时注入        │       │
           └─────────┬─────────┘       │
                     │                 │
                     ▼                 ▼
           ┌─────────────────────────────────┐
           │     Phase 2: Action              │
           │     Generate(availableTools)     │
           └───────────────┬─────────────────┘
                           │
                  ┌────────▼────────┐
                  │  合并为单条       │
                  │  assistant 消息  │
                  │  → 注入 Context  │
                  └────────┬────────┘
                           │
                  ┌────────▼────────┐    有 ToolCalls
                  │  终止条件检测     │──────────────────┐
                  │  ToolCalls == 0? │                   │
                  └────────┬────────┘                   │
                           │ 无 ToolCalls               │
                  ┌────────▼────────┐    ┌──────────────┴───────────┐
                  │  任务完成         │    │  ToolCall 阶段 (并发)     │
                  │  退出循环         │    │  每工具独立超时            │
                  └─────────────────┘    └────────────┬─────────────┘
                                                      │
                                        ┌──────────────▼───────────┐
                                        │  Observation 阶段         │
                                        │  追加工具结果到上下文      │
                                        └────────────┬─────────────┘
                                                      │
                                        ┌──────────────▼───────────┐
                                        │  回到 Turn 计数 ++        │
                                        └──────────────────────────┘
```

### 4.1 初始化阶段

引擎启动时，构造初始对话上下文。**WorkDir 会被注入到 system prompt** 中，使 LLM 了解其工作目录：

```go
contextHistory := []schema.Message{
    {
        Role: schema.RoleSystem,
        Content: fmt.Sprintf(
            "You are harness9, an expert coding assistant. "+
                "You have full access to tools in the workspace. "+
                "Your working directory is: %s",
            e.WorkDir,
        ),
    },
    {Role: RoleUser, Content: userPrompt},
}
```

### 4.2 Phase 1: Thinking 阶段（条件启用）

当 `EnableThinking == true` 时，引擎在 Action 之前先执行一次 Thinking 调用：

```go
thinkResp, err := e.provider.Generate(ctx, contextHistory, nil) // nil 剥夺工具
thinkResp.ToolCalls = nil // 防御性清除
```

**设计要点：**

| 问题 | 解决方案 |
|------|---------|
| 如何强制模型思考而非行动？ | 传入 `nil` 作为工具列表 |
| 思考结果如何传递给 Action？ | 注入临时 `phase2History`，Phase 2 调用后丢弃 |
| 如何避免连续 assistant 消息？ | Thinking + Action 合并为单条消息后才注入主 contextHistory |
| LLM 违规返回 ToolCalls 怎么办？ | 防御性 `thinkResp.ToolCalls = nil` 清除 |

### 4.3 Phase 2: Action 阶段

恢复完整工具列表，LLM 基于临时上下文（含 Phase 1 的思考）决定行动：

```go
// 临时上下文（Phase 2 调用专用）
phase2History := make([]schema.Message, len(contextHistory), len(contextHistory)+1)
copy(phase2History, contextHistory)
phase2History = append(phase2History, *thinkResp)

actionResp, err := e.provider.Generate(ctx, phase2History, availableTools)
```

### 4.4 消息合并

Phase 1 思考与 Phase 2 行动合并为单条 assistant 消息：

```go
mergedMsg := &schema.Message{
    Role:      schema.RoleAssistant,
    Content:   joinContent(thinkResp.Content, actionResp.Content),
    ToolCalls: actionResp.ToolCalls,
}
contextHistory = append(contextHistory, *mergedMsg)
```

`joinContent` 逻辑：

| thinking | action | 结果 |
|----------|--------|------|
| `""` | `""` | `""` |
| `""` | `"act"` | `"act"` |
| `"think"` | `""` | `"think"` |
| `"think"` | `"act"` | `"think\n\nact"` |

### 4.5 终止条件检测

引擎实现三重安全保障：

```go
// 1. MaxTurns 限制：防止无限循环
if e.MaxTurns > 0 && turnCount > e.MaxTurns {
    return fmt.Errorf("已达最大 Turn 数 (%d)，循环终止", e.MaxTurns)
}

// 2. Context 取消：支持超时和手动中断
select {
case <-ctx.Done():
    return fmt.Errorf("context 已取消: %w", ctx.Err())
default:
}

// 3. 自然终止：模型不再请求工具调用
if len(responseMsg.ToolCalls) == 0 {
    break
}
```

### 4.6 ToolCall 阶段 — 并发执行（带独立超时）

当模型请求调用多个工具时，引擎使用 **goroutine + `sync.WaitGroup`** 并发执行。**每个工具有独立的超时控制**：

```go
go func(idx int, tc schema.ToolCall, currentTurn int) {
    defer wg.Done()

    // 独立超时：单个工具超时不影响其他工具
    toolCtx := ctx
    if e.ToolTimeout > 0 {
        toolCtx, cancel = context.WithTimeout(ctx, e.ToolTimeout)
        defer cancel()
    }

    results[idx] = e.registry.Execute(toolCtx, tc)
}(i, toolCall, turn) // turnCount 显式传参，避免闭包竞争
```

**并发安全设计要点：**

| 问题 | 解决方案 |
|------|---------|
| 多个 goroutine 写入同一结果集 | 预分配切片，每个 goroutine 按索引 `idx` 写入独立位置 |
| 结果顺序一致性 | 索引与原始 `ToolCalls` 顺序一一对应 |
| 单工具超时 | `context.WithTimeout` 为每个工具创建独立子 context |
| 闭包变量捕获 | `turnCount` 显式传参，避免数据竞争 |

### 4.7 Observation 阶段

工具执行完毕后，结果按原始顺序追加到上下文：

```go
for i, toolCall := range responseMsg.ToolCalls {
    observationMsg := schema.Message{
        Role:       schema.RoleUser,        // Observation 以 user 角色回传
        Content:    results[i].Output,
        ToolCallID: toolCall.ID,             // 关联原始请求
    }
    contextHistory = append(contextHistory, observationMsg)
}
```

## 5. 接口抽象与解耦设计

### 5.1 LLMProvider 接口

```go
type LLMProvider interface {
    Generate(ctx context.Context, messages []schema.Message,
             availableTools []schema.ToolDefinition) (*schema.Message, error)
}
```

**设计理念：**
- `availableTools` 参数支持 `nil`（Phase 1 剥夺工具）和非空（Phase 2 恢复工具）
- 引擎只依赖接口，切换模型只需替换 Provider 实现

### 5.2 Registry 接口

```go
type Registry interface {
    GetAvailableTools() []schema.ToolDefinition
    Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult
}
```

### 5.3 依赖注入 + 函数选项

```go
eng := engine.NewAgentEngine(p, r, workDir, true,
    engine.WithMaxTurns(100),
    engine.WithToolTimeout(30 * time.Second),
)
```

`Option` 函数选项模式支持灵活配置，同时保持构造函数签名简洁：

| 选项 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `WithMaxTurns(n)` | `int` | 50 | 单次 Run 最大 Turn 数，0 = 不限制 |
| `WithToolTimeout(d)` | `time.Duration` | 60s | 单个工具执行超时，0 = 使用原始 context |

## 6. 日志与可观测性

引擎采用结构化日志格式，统一使用 `[engine]` 前缀和 `key=value` 风格：

```
[engine] 启动 | workdir=/Users/zsa/project thinking=true maxTurns=50 toolTimeout=1m0s
[engine] Turn 1 | contextMessages=2
[engine] Turn 1 | Phase 1: Thinking (tools=none)
[engine] Turn 1 | Phase 1 完成 | 思考长度=87 chars
[engine] Turn 1 | Phase 2: Action (tools=1)
[engine] Turn 1 | Two-Stage 合并完成 | thinking=87 chars action=42 chars toolCalls=1
[engine] Turn 1 | 执行 1 个工具调用
[engine] Turn 1 | 工具启动 | name=bash id=call_123
[engine] Turn 1 | 工具完成 | name=bash bytes=45
[engine] Turn 1 | Observation 注入完成 | contextMessages=4
[engine] 循环结束 | 总Turns=2 | contextMessages=5
```

**日志分层：**

| 层级 | 前缀 | 内容 | 输出方式 |
|------|------|------|---------|
| 引擎内部 | `[engine]` | Turn 计数、阶段转换、工具状态 | `log.Printf`（stderr，带时间戳） |
| 模型输出 | `[thinking]` / `[assistant]` | LLM 产出的文本内容 | `fmt.Printf`（stdout，无时间戳） |

## 7. 完整数据流图

以一个启用 Two-Stage ReAct 的两轮对话为例：

```
Turn 1:
  [Context]
    system:    "You are harness9... working directory is: /test"
    user:      "帮我检查当前目录的文件"

  Phase 1 (Thinking): → Generate(ctx, history, nil)
    assistant: "【深度思考】目标是检查文件。我需要先调用 bash..."
    → ToolCalls 清除为 nil
    → 注入到临时 phase2History

  Phase 2 (Action): → Generate(ctx, phase2History, [bash])
    assistant: "我要执行我刚才规划的步骤了。" + ToolCall{id:"call_123", name:"bash"}

  合并: assistant = "【深度思考】...\n\n我要执行..." + ToolCalls
    → 注入到 contextHistory（单条消息）

  ToolCall: → Registry.Execute(bash, "ls -la")
    ToolResult{id:"call_123", output:"-rw-r--r-- ... main.go"}

  Observation: user: "-rw-r--r-- ... main.go" (toolCallID:"call_123")

Turn 2:
  [Context = 4 messages: system, user, assistant(merged), user(obs)]

  Phase 1 (Thinking): → Generate(ctx, history, nil)
    assistant: "【深度思考】已经看到文件列表..."

  Phase 2 (Action): → Generate(ctx, phase2History, [bash])
    assistant: "任务圆满完成！" (无 ToolCall)

  合并: → 注入到 contextHistory

  → 终止条件满足，循环退出
```

## 8. 与主流框架的对比

> 详细调研报告见 `docs/技术调研/two-stage-react-research.md`

| 框架 | 两阶段分离 | 思考方式 | 与 harness9 的区别 |
|------|:---------:|---------|-------------------|
| **harness9** | ✅ | 剥夺工具强制思考 + 合并消息 | 独创的 nil-tools 两阶段 + 单消息合并 |
| **Claude SDK** | ❌ | Extended Thinking（API 内置） | 思考在单次 API 调用内完成，不分离阶段 |
| **OpenAI SDK** | ❌ | o1/o3 推理模型（内部机制） | 推理 token 不暴露给调用者 |
| **HermesAgent** | ❌ | reasoning callback | 仅捕获推理内容，不控制工具可用性 |
| **OpenHarness** | ❌ | 无独立思考阶段 | 最清晰的显式循环但无思考分离 |
| **DeepAgents** | ⚠️ | `write_todos` 工具 | Planning 是工具而非独立阶段 |
| **OpenCode** | ⚠️ | plan/build Agent 切换 | 通过独立 Agent 模式而非同一 Turn 内阶段 |

## 9. 已知限制与未来演进

| 限制 | 当前状态 | 演进方向 |
|------|---------|---------|
| **上下文窗口控制** | 无 token 估算和截断，contextHistory 无限增长 | 双层压缩：micro-compact（清除旧工具输出）+ LLM summarize |
| **流式输出** | 仅支持非流式 `Generate` | 新增 `Stream` 接口方法，支持 SSE/WebSocket |
| **权限控制** | 无 | 工具执行前统一 PermissionChecker，支持交互式确认 |
| **Hook 系统** | 无 | PreToolUse / PostToolUse / Stop / TurnComplete 事件钩子 |
| **自适应思考** | EnableThinking 为静态配置 | 根据任务复杂度自动决定是否启用 Phase 1 |

## 10. 设计原则总结

| 原则 | 体现 |
|------|------|
| **Two-Stage ReAct** | Thinking → Action → Observation，先思后行 |
| **剥夺-恢复策略** | Phase 1 传 nil tools 剥夺行动能力，Phase 2 恢复工具 |
| **单消息合并** | Thinking + Action 合并为一条 assistant 消息，保证 API 兼容性 |
| **接口隔离** | `LLMProvider` 和 `Registry` 各司其职，引擎只依赖抽象 |
| **函数选项** | `WithMaxTurns` / `WithToolTimeout` 可选配置，保持构造函数简洁 |
| **并发安全** | 索引隔离写入 + WaitGroup + 显式参数传递，无数据竞争 |
| **三重保障终止** | 自然终止 + MaxTurns 限制 + Context 取消 |
| **可观测性** | 结构化日志 `[engine]` 前缀 + key=value 格式 |
| **防御性编程** | Phase 1 ToolCalls 清除、工具独立超时 |
| **延迟解析** | `json.RawMessage` 统一用于 Arguments 和 InputSchema |
| **自愈能力** | `ToolResult.IsError` 支持模型感知错误并自动重试 |
