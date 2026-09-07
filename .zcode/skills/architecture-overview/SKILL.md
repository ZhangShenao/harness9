---
name: architecture-overview
description: Use when asked about harness9 architecture, module design, or how components interact — explains the system design
trigger: architecture, design, how does, module, component, structure
---

# harness9 架构概览

## 核心设计原则

| 原则 | 说明 |
|------|------|
| **简洁** | 最小化抽象层，极少的直接依赖 |
| **完备** | 覆盖 Agent 运行所需的全部核心模块 |
| **生产可用** | 错误恢复、超时控制、路径沙箱、并发安全 |

## 标准 ReAct 循环

```
Turn N:
  LLM(messages + tools) → 推理 + 工具调用决策
  → 并发执行所有工具调用 → Observation 注入 context
  → Turn N+1

自然终止：模型不再发起工具调用 → 输出最终回复
```

三重终止保障：
1. **自然终止**：`len(responseMsg.ToolCalls) == 0`
2. **MaxTurns**：默认 50，可通过 `WithMaxTurns` 配置
3. **Context 取消**：外部 `cancel()` 或超时

## 模块依赖关系

```
cmd/harness9 (入口)
    ├── internal/context (System Prompt 组装)
    │       └── internal/skills (Skills 解析 + 索引)
    ├── internal/engine (ReAct 主循环)
    │       ├── internal/provider (LLM 调用)
    │       ├── internal/tools (工具注册 + 执行)
    │       └── internal/schema (数据类型)
    └── internal/env (配置加载)
```

**关键设计决策：接口定义在使用者侧**

- `tools.Registry` 接口定义在 `tools` 包，`engine` 包依赖它
- `engine.PromptBuilder` 接口定义在 `engine` 包，`context` 包实现它
- `skills.UseSkillTool` 通过 Go 结构类型满足 `tools.BaseTool` 接口，不需要 import `tools` 包（避免循环依赖）

## 关键数据流

### TUI 模式

```
用户输入 → RunTUI → eng.RunStream(ctx, prompt)
    → engine.Event stream → 逐 token 追加到对话视图
    → ToolCalls → Spinner 动画 + 耗时计数
    → EventDone → 最终回复渲染到屏幕
```

### CLI 模式（管道 / CI）

```
用户输入 → RunCLI → eng.Run(ctx, prompt)
    → runLoop → LLM Generate
    → ToolCalls → 并发执行 → ToolResults → 继续循环
    → 最终回复打印到 stdout
```

## System Prompt 组装

`DefaultPromptBuilder.Build()` 按顺序组装：

1. **基础 Prompt**：角色定义 + workDir
2. **AGENTS.md**：项目级规范（文件不存在时跳过）
3. **Skills 索引**：`- name: description` 列表（为空时跳过）

完整内容示例：
```
You are harness9, an expert coding assistant...

## Project Guidelines (AGENTS.md)
{AGENTS.md 全文}

## Available Skills
Use the `use_skill` tool to load full content of any skill when needed.
- go-coding-standards: Use when writing or reviewing Go code...
- debugging-guide: Use when debugging Go errors...
```

## Provider 抽象

```go
type LLMProvider interface {
    Generate(ctx, messages, tools) (Message, error)
    GenerateStream(ctx, messages, tools) (<-chan StreamChunk, error)
}
```

当前实现：
- `OpenAIProvider`：兼容所有 OpenAI Chat Completions API（包括 OpenRouter、Azure）
- `AnthropicProvider`：Anthropic Messages API

**Anthropic 约束**：user/assistant 消息必须严格交替，禁止连续 assistant 消息。

## 工具系统

```go
type BaseTool interface {
    Name() string
    Definition() schema.ToolDefinition  // JSON Schema，传给 LLM
    Execute(ctx context.Context, args json.RawMessage) (string, error)
}
```

内置工具：

| 工具 | 说明 |
|------|------|
| `bash` | Shell 命令执行，workDir 为 CWD |
| `read_file` | 文件读取，4096 字节截断 |
| `write_file` | 文件写入，自动 mkdir |
| `edit_file` | 字符串替换编辑，多级模糊匹配 |
| `use_skill` | 按需加载 Skill 全文 |

所有文件工具通过 `safePath()` 校验路径，防止 Path Traversal 攻击。
