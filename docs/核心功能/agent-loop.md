# Agent Loop 核心实现原理

## 1. 架构总览

harness9 的核心是一个 **Reasoning → ToolCall → Observation** 循环引擎，模仿人类解决复杂问题时的"思考-行动-观察"范式。引擎编排三个核心抽象协同工作：

```
┌─────────────────────────────────────────────────────┐
│                    AgentEngine                       │
│                 (核心编排器 / Loop)                   │
│                                                     │
│   ┌─────────────┐  Generate()  ┌───────────────┐   │
│   │  Context     │◄─────────────│  LLMProvider  │   │
│   │  History     │              │  (大脑 / 模型) │   │
│   │  (对话上下文) │────────────►│               │   │
│   └──────┬──────┘  Response    └───────────────┘   │
│          │                                          │
│          │ ToolCall                                 │
│          ▼                                          │
│   ┌──────────────┐  Execute()  ┌───────────────┐   │
│   │  Observation  │◄─────────────│  Registry     │   │
│   │  (工具结果)    │              │  (双手 / 工具) │   │
│   └──────────────┘              └───────────────┘   │
│                                                     │
└─────────────────────────────────────────────────────┘
```

| 组件 | 代码位置 | 职责 |
|------|---------|------|
| `schema` | `internal/schema/message.go` | 定义跨组件共享的核心数据类型 |
| `LLMProvider` | `internal/provider/interface.go` | 抽象 LLM 通信层，封装 API 差异 |
| `Registry` | `internal/tools/registry.go` | 解耦工具发现与执行 |
| `AgentEngine` | `internal/engine/agent_loop.go` | 编排主循环，驱动各组件协作 |

## 2. 数据模型 (`internal/schema`)

### 2.1 消息角色体系

```
Role (string)
├── "system"     → 系统提示词：定义 Agent 身份、约束与行为边界
├── "user"       → 用户输入 & 工具执行结果 (Observation)
└── "assistant"  → 模型输出：推理文本 + 工具调用请求
```

遵循主流 Chat Completion API（OpenAI / Anthropic / Google）的 system / user / assistant 三元组。

### 2.2 核心类型关系

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
│  └── InputSchema interface 参数 JSON Schema       │
└──────────────────────────────────────────────────┘
```

**关键设计决策：**

- **`ToolCall.Arguments` 使用 `json.RawMessage`**：延迟反序列化，将参数解析责任交给具体工具实现，引擎层无需了解每种工具的参数结构。
- **`ToolCallID` 关联机制**：工具执行结果 (Observation) 通过 `ToolCallID` 与原始 `ToolCall` 关联，使 LLM 能准确匹配请求与响应。
- **`ToolResult.IsError` 自愈标记**：当工具执行失败时，引擎将错误暴露给 LLM，使其能尝试修正参数并重试（Self-Healing）。

## 3. Agent Loop 循环流程

```
                    ┌─────────────────┐
                    │  初始化上下文     │
                    │  System + User   │
                    └────────┬────────┘
                             │
                    ┌────────▼────────┐
              ┌─────│  Reasoning 阶段  │◄────────────────┐
              │     │  调用 LLMProvider │                  │
              │     │  .Generate()     │                  │
              │     └────────┬────────┘                  │
              │              │                            │
              │     ┌────────▼────────┐                  │
              │     │  追加 Response   │                  │
              │     │  到 Context      │                  │
              │     └────────┬────────┘                  │
              │              │                            │
              │     ┌────────▼────────┐    有 ToolCalls   │
              │     │  终止条件检测     │──────────────────┤
              │     │  ToolCalls == 0? │                  │
              │     └────────┬────────┘                  │
              │              │ 无 ToolCalls               │
              │     ┌────────▼────────┐     ┌────────────┴───────────┐
              │     │  ✅ 任务完成      │     │  ToolCall 阶段 (并发)   │
              │     │  退出循环        │     │  goroutine + WaitGroup  │
              │     └─────────────────┘     └────────────┬───────────┘
              │                                          │
              │                               ┌──────────▼───────────┐
              │                               │  Observation 阶段     │
              └───────────────────────────────│  追加工具结果到上下文   │
                                              └──────────────────────┘
```

### 3.1 初始化阶段

引擎启动时，构造初始对话上下文：

```go
contextHistory := []schema.Message{
    {Role: RoleSystem, Content: "You are harness9, ..."},  // 定义 Agent 身份
    {Role: RoleUser,   Content: userPrompt},                // 用户任务描述
}
```

**System Prompt** 注入 Agent 身份、能力和行为约束，作为所有后续推理的基石。

### 3.2 Reasoning 阶段

每个 Turn 的第一步，引擎将完整上下文历史发送给 LLM：

```go
responseMsg, err := e.provider.Generate(ctx, contextHistory, availableTools)
```

LLM Provider 返回的 `Message` 可能包含：
- **纯文本推理** (`Content` 非空)：模型在"思考"
- **工具调用请求** (`ToolCalls` 非空)：模型决定"行动"
- **两者兼有**：模型边思考边调用工具

### 3.3 终止条件检测

```go
if len(responseMsg.ToolCalls) == 0 {
    break  // 模型不再调用工具 → 任务完成
}
```

当模型认为已收集到足够信息，不再发出 ToolCall，而是直接产出最终文本回复时，循环终止。

### 3.4 ToolCall 阶段 — 并发执行

当模型请求调用多个工具时，引擎使用 **goroutine + `sync.WaitGroup`** 并发执行：

```go
results := make([]schema.ToolResult, len(responseMsg.ToolCalls))
var wg sync.WaitGroup

for i, toolCall := range responseMsg.ToolCalls {
    wg.Add(1)
    go func(idx int, tc schema.ToolCall) {
        defer wg.Done()
        results[idx] = e.registry.Execute(ctx, tc)  // 按索引写入
    }(i, toolCall)
}

wg.Wait()
```

**并发安全设计要点：**

| 问题 | 解决方案 |
|------|---------|
| 多个 goroutine 写入同一结果集 | 预分配切片，每个 goroutine 按索引 `idx` 写入独立位置，零竞争 |
| 结果顺序一致性 | 索引与原始 `ToolCalls` 顺序一一对应，`Wait` 后按序追加 Observation |
| Context 取消传播 | 每个工具执行都接收 `ctx`，支持超时和取消 |

### 3.5 Observation 阶段

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

每个 Observation 携带 `ToolCallID`，使 LLM 能将结果与自己的请求精确匹配。随后进入下一个 Turn 的 Reasoning 阶段。

## 4. 接口抽象与解耦设计

### 4.1 LLMProvider 接口

```go
type LLMProvider interface {
    Generate(ctx context.Context, messages []schema.Message,
             availableTools []schema.ToolDefinition) (*schema.Message, error)
}
```

**设计理念：**
- 引擎只依赖接口，不依赖具体 LLM 实现（OpenAI、Anthropic、Google 等）
- Provider 封装认证、端点、重试等 API 细节
- 切换模型只需替换 Provider 实现，引擎代码零修改

### 4.2 Registry 接口

```go
type Registry interface {
    GetAvailableTools() []schema.ToolDefinition
    Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult
}
```

**设计理念：**
- **工具发现与执行解耦**：引擎无需了解具体工具的实现
- **动态注册**：工具可在运行时增删，`GetAvailableTools()` 每轮动态查询
- **统一执行入口**：所有工具通过同一个 `Execute` 方法调用，便于添加中间件（日志、超时、权限）

### 4.3 依赖注入

```go
eng := engine.NewAgentEngine(p, r, workDir)
err := eng.Run(ctx, "用户任务")
```

`AgentEngine` 通过构造函数接收 `Provider` 和 `Registry`，遵循**依赖注入**原则：
- 便于单元测试（注入 Mock 实现）
- 便于运行时切换（注入真实实现）
- 组件之间松耦合

## 5. 完整数据流图

以一个两轮对话为例（Mock Provider 场景）：

```
Turn 1:
  [Context]
    system:  "You are harness9..."
    user:    "帮我检查当前目录的文件"

  → Provider.Generate() →
    assistant: "让我来看看..." + ToolCall{id:"call_123", name:"bash", args:"ls -la"}

  → Registry.Execute(bash, "ls -la") →
    ToolResult{id:"call_123", output:"-rw-r--r-- ... main.go"}

  → 追加到 Context:
    system:    "You are harness9..."
    user:      "帮我检查当前目录的文件"
    assistant: "让我来看看..." + ToolCall[...]
    user:      "-rw-r--r-- ... main.go"  (Observation, toolCallID:"call_123")

Turn 2:
  → Provider.Generate() →
    assistant: "我看到了文件列表，里面包含 main.go，任务完成！"  (无 ToolCall)

  → 终止条件满足，循环退出
```

## 6. 设计原则总结

| 原则 | 体现 |
|------|------|
| **ReAct 模式** | Reasoning → Action (ToolCall) → Observation 三阶段循环 |
| **接口隔离** | `LLMProvider` 和 `Registry` 各司其职，引擎只依赖抽象 |
| **依赖注入** | 组件通过构造函数注入，便于测试和替换 |
| **并发安全** | 索引隔离写入 + WaitGroup 汇聚，无锁无竞争 |
| **可观测性** | 每个阶段均有日志输出，Turn 计数追踪 |
| **延迟解析** | `json.RawMessage` 将参数解析推迟到工具层 |
| **自愈能力** | `ToolResult.IsError` 支持模型感知错误并自动重试 |
