# Tool Calling 工具调用系统

## 概述

Tool Calling 是 harness9 Agent 框架的核心能力之一，使 LLM 能够通过结构化的函数调用与外部环境交互。本文档详细描述工具调用系统的架构设计、数据流、关键接口和实现细节。

## 架构总览

```
┌─────────────────────────────────────────────────────────────┐
│                        Agent Engine                         │
│                                                             │
│  ┌──────────┐    ToolCall[]    ┌─────────────────────────┐  │
│  │          │ ──────────────► │                          │  │
│  │  LLM     │                  │   Tool Registry          │  │
│  │ Provider │ ◄────────────── │                          │  │
│  │          │    ToolResult[]  │  ┌──────┐ ┌──────┐      │  │
│  └──────────┘                  │  │bash  │ │read  │ ...  │  │
│       │                        │  │Tool  │ │file  │      │  │
│       │ Message                │  └──────┘ └──────┘      │  │
│       │ (ToolCalls)            └─────────────────────────┘  │
│       │                                                   │
│       ▼                                                   │
│  ┌──────────────────────────────────────────┐             │
│  │          Context History                  │             │
│  │  [system] → [user] → [assistant+TC] →   │             │
│  │  [observation] → [assistant] → ...        │             │
│  └──────────────────────────────────────────┘             │
└─────────────────────────────────────────────────────────────┘
```

## 核心数据流

一次完整的 Tool Calling 周期由以下步骤组成：

```
1. LLM 生成响应，包含 ToolCalls
2. Engine 检测到 ToolCalls，并发执行每个工具
3. 每个 工具调用 返回 ToolResult
4. ToolResult 转换为 Observation 消息 (Role=user, ToolCallID=xxx)
5. Observation 注入 Context History
6. 进入下一轮 LLM 调用
```

时序图：

```
Engine                LLMProvider           Registry            BaseTool
  │                       │                     │                   │
  │  Generate(msgs,tools) │                     │                   │
  │──────────────────────►│                     │                   │
  │  Message{ToolCalls}   │                     │                   │
  │◄──────────────────────│                     │                   │
  │                       │                     │                   │
  │  Execute(call)        │                     │                   │
  │─────────────────────────────────────────────►│                   │
  │                       │                     │  Execute(ctx,args)│
  │                       │                     │──────────────────►│
  │                       │                     │  (string, error)  │
  │                       │                     │◄──────────────────│
  │  ToolResult           │                     │                   │
  │◄─────────────────────────────────────────────│                   │
  │                       │                     │                   │
  │  [Observation → ctx]  │                     │                   │
  │  Generate(msgs,tools) │                     │                   │
  │──────────────────────►│                     │                   │
```

## 核心类型定义

### 工具调用请求 — ToolCall

```go
type ToolCall struct {
    ID        string          // LLM 分配的唯一标识符，用于关联请求和结果
    Name      string          // 目标工具名称（如 "bash", "read_file"）
    Arguments json.RawMessage // 原始 JSON 参数，延迟反序列化
}
```

**设计决策**：`Arguments` 使用 `json.RawMessage` 而非 `map[string]interface{}`，将解析责任推迟到具体工具实现。这避免了引擎层的过早类型断言，也允许工具接受任意 JSON 结构作为输入。

### 工具执行结果 — ToolResult

```go
type ToolResult struct {
    ToolCallID string // 关联原始 ToolCall.ID
    Output     string // 工具执行的 stdout 或错误信息
    IsError    bool   // 标记执行是否失败
}
```

**关键设计**：`IsError` 字段使引擎能够将失败信息回传给 LLM，触发自愈（self-healing）行为 — 例如 LLM 可以修正命令语法后重试。

### 工具定义 — ToolDefinition

```go
type ToolDefinition struct {
    Name        string      // 工具唯一标识符
    Description string      // 自然语言描述，供 LLM 理解工具用途
    InputSchema interface{} // JSON Schema 描述参数格式
}
```

**设计决策**：`InputSchema` 使用 `interface{}` 而非具体类型，因为不同 LLM SDK 对参数格式的要求不同：
- OpenAI SDK 要求 `shared.FunctionParameters`（即 `map[string]interface{}`）
- Anthropic SDK 要求分离的 `Properties` + `Required` 字段

各 Provider 实现负责将 `interface{}` 转换为 SDK 要求的格式。

## 关键接口

### BaseTool — 工具实现契约

```go
type BaseTool interface {
    Name() string
    Definition() schema.ToolDefinition
    Execute(ctx context.Context, args json.RawMessage) (string, error)
}
```

| 方法 | 职责 |
|------|------|
| `Name()` | 返回工具在 Registry 中的唯一标识符 |
| `Definition()` | 返回工具的元信息（描述、参数 Schema），供 LLM 理解 |
| `Execute()` | 执行工具逻辑，接收原始 JSON 参数，返回文本输出 |

### Registry — 工具注册中心

```go
type Registry interface {
    Register(tool BaseTool)
    GetAvailableTools() []schema.ToolDefinition
    Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult
}
```

| 方法 | 职责 |
|------|------|
| `Register()` | 注册工具到注册表，重复名称会覆盖并打印警告 |
| `GetAvailableTools()` | 返回所有已注册工具的 ToolDefinition 列表，传递给 LLM |
| `Execute()` | 根据 ToolCall.Name 查找工具并执行，返回 ToolResult |

### LLMProvider — 模型提供者接口

```go
type LLMProvider interface {
    Generate(ctx context.Context, messages []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, error)
}
```

`availableTools` 为 `nil` 时表示不提供工具（Thinking 阶段），为空切片 `[]` 和 `nil` 的语义不同：
- `nil`：不传递 tools 参数给 API（模型不调用工具）
- `[]`：传递空 tools 数组（理论上不常见，但引擎使用 `nil` 表示 Thinking）

## 并发执行模型

引擎通过 `executeToolsConcurrently` 并发执行同一 Turn 中的所有 ToolCall：

```go
func (e *AgentEngine) executeToolsConcurrently(ctx context.Context, turn int, toolCalls []schema.ToolCall) []schema.ToolResult {
    results := make([]schema.ToolResult, len(toolCalls))
    var wg sync.WaitGroup

    for i, toolCall := range toolCalls {
        wg.Add(1)
        go func(idx int, tc schema.ToolCall, currentTurn int) {
            defer wg.Done()

            toolCtx := ctx
            var cancel context.CancelFunc
            if e.ToolTimeout > 0 {
                toolCtx, cancel = context.WithTimeout(ctx, e.ToolTimeout)
                defer cancel()
            }

            results[idx] = e.registry.Execute(toolCtx, tc)
        }(i, toolCall, turn)
    }

    wg.Wait()
    return results
}
```

**关键设计点**：

1. **预分配 + 索引写入**：`results` 切片在 goroutine 启动前预分配，每个 goroutine 通过索引 `idx` 写入对应位置，避免竞态条件。

2. **独立超时控制**：每个工具获得独立的 `context.WithTimeout` 子上下文。一个工具超时不影响其他工具执行，仅将当前工具标记为失败。

3. **WaitGroup 同步**：所有工具执行完毕后才统一将结果注入 Observation，保证消息顺序与 ToolCalls 一致。

## 日志系统

工具调用过程通过结构化日志记录，参数和结果以 JSON 格式输出：

```
[engine] Turn 1 | 工具启动 | name=read_file id=call_abc arguments={"path":"main.go"}
[engine] Turn 1 | 工具完成 | name=read_file id=call_abc result={"is_error":false,"output":"package main\n..."}
[engine] Turn 1 | 工具失败 | name=bash id=call_xyz result={"is_error":true,"output":"command not found"}
```

日志覆盖以下关键节点：

| 阶段 | 日志内容 |
|------|---------|
| 引擎启动 | workdir、thinking 模式、maxTurns、toolTimeout |
| Turn 开始 | 当前 Turn 数、上下文消息数量 |
| Phase 1 (Thinking) | 禁用工具的 LLM 调用 |
| Phase 2 (Action) | 恢复工具的 LLM 调用 |
| 工具启动 | 工具名称、ID、**JSON 格式参数** |
| 工具完成/失败 | 工具名称、ID、**JSON 格式结果** |
| Observation 注入 | 消息数量变化 |
| 循环结束 | 总 Turn 数、最终消息数 |

## Provider 适配层

### OpenAI 兼容适配器

**文件**：`internal/provider/openai.go`

**类型转换规则**：

| schema 类型 | OpenAI SDK 类型 |
|-------------|----------------|
| `RoleSystem` | `openai.SystemMessage` |
| `RoleUser` (无 ToolCallID) | `openai.UserMessage` |
| `RoleUser` (有 ToolCallID) | `openai.ToolMessage` |
| `RoleAssistant` | `ChatCompletionAssistantMessageParam` |
| `ToolDefinition` | `ChatCompletionFunctionTool` |

**环境变量**：
- `OPENAI_API_KEY`：API 认证密钥（必需）
- `OPENAI_BASE_URL`：API 端点基址（必需，支持 OpenRouter 等兼容服务）

### Anthropic 兼容适配器

**文件**：`internal/provider/anthropic.go`

**与 OpenAI 适配器的关键差异**：

| 差异点 | OpenAI | Anthropic |
|--------|--------|-----------|
| System Prompt | 在 messages 数组中 | 作为独立参数 `params.System` |
| 工具结果 | `ToolMessage(content, toolCallID)` | `ToolResultBlock(toolCallID, content, isError)` |
| 工具调用参数 | 原始 JSON 字符串 | 反序列化为 `map[string]interface{}` |
| MaxTokens | 可选 | **必需**参数 |
| 工具定义 Schema | 完整 JSON Schema | 分离的 `Properties` + `Required` |

**环境变量**：
- `ANTHROPIC_API_KEY`：API 认证密钥（必需）
- `ANTHROPIC_BASE_URL`：API 端点基址（必需）

## 已实现的工具

### read_file — 文件读取工具

**文件**：`internal/tools/read_file.go`

| 属性 | 值 |
|------|-----|
| 名称 | `read_file` |
| 参数 | `path` (string, 必需) — 相对工作区的文件路径 |
| 输出 | 文件内容文本 |
| 截断策略 | 超过 4096 字节时截断并附加提示信息 |

**安全措施**：
- 通过 `safePath` 方法校验解析后路径是否以 `workDir` 为前缀，阻止 `..` 路径穿越
- 使用 `io.LimitReader` 限制单次读取量，防止超大文件占用上下文窗口

## 扩展指南

### 添加新工具

1. 在 `internal/tools/` 下创建新文件（如 `write_file.go`）
2. 实现 `BaseTool` 接口的三个方法
3. 在 `cmd/harness9/main.go` 中注册：

```go
writeTool := tools.NewWriteFileTool(workDir)
registry.Register(writeTool)
```

### 添加新 Provider

1. 在 `internal/provider/` 下创建新文件（如 `google.go`）
2. 实现 `LLMProvider` 接口的 `Generate` 方法
3. 负责 schema 类型到 SDK 类型的转换
4. 在 `main.go` 中替换 Provider 初始化

### 添加工具中间件

当前 Registry 的 `Execute` 方法直接调用工具。如需添加中间件能力（日志、权限校验、速率限制），可在 `registryImpl.Execute` 中包装调用链：

```go
func (r *registryImpl) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
    // 前置中间件：权限校验、日志、限流
    if !r.isAllowed(call.Name) {
        return schema.ToolResult{...}
    }

    output, err := tool.Execute(ctx, call.Arguments)

    // 后置中间件：结果转换、审计日志
    return schema.ToolResult{...}
}
```

## 设计决策记录

### 1. 为什么 ToolCall.Arguments 使用 json.RawMessage？

延迟反序列化将类型安全责任交给具体工具实现。引擎不需要知道每个工具的参数结构，降低了耦合度。同时避免了 `map[string]interface{}` 在嵌套结构中的类型断言复杂性。

### 2. 为什么 ToolResult 使用 string 而非 interface{}？

LLM 的工具结果通过文本通道传递。无论工具输出是命令行输出、文件内容还是错误信息，最终都以文本形式注入上下文。使用 `string` 简化了 Provider 适配层的实现。

### 3. 为什么 Observation 使用 RoleUser？

遵循 OpenAI 和 Anthropic 的 API 规范：工具执行结果以 user 角色消息回传，通过 `ToolCallID` 字段与原始请求关联。

### 4. 为什么支持并行 ToolCall？

主流 LLM（GPT-4、Claude）支持在单次响应中发出多个工具调用请求。并行执行显著减少总延迟，特别是当多个工具之间无依赖关系时（如同时读取多个文件）。

### 5. 为什么 IsError 字段很重要？

错误信息对 LLM 是有价值的上下文。当工具执行失败时，LLM 能够看到错误原因并尝试自愈 — 修正命令、调整参数或选择替代方案。这比静默失败或直接终止循环更具鲁棒性。

## 文件索引

| 文件 | 职责 |
|------|------|
| `internal/schema/message.go` | ToolCall、ToolResult、ToolDefinition 等核心类型定义 |
| `internal/tools/registry.go` | 工具注册表接口和实现 |
| `internal/tools/read_file.go` | read_file 工具实现 |
| `internal/provider/interface.go` | LLMProvider 接口定义 |
| `internal/provider/openai.go` | OpenAI 兼容 API 适配器 |
| `internal/provider/anthropic.go` | Anthropic 兼容 API 适配器 |
| `internal/provider/mock.go` | 测试用 Mock Provider |
| `internal/engine/agent_loop.go` | Agent 主循环，编排 Tool Calling 全流程 |
| `internal/engine/agent_loop_test.go` | 主循环单元测试 |
