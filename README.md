# harness9

> 轻量级、功能完备、生产可用的 Go Agent Harness 框架

---

## 为什么选择 harness9？

大多数 Agent 框架要么过于臃肿（满屏抽象层、数百个依赖），要么过于简陋（仅能跑个 demo）。harness9 走中间路线：**最小抽象、完整功能、生产就绪**。

```
核心设计理念:

  简洁 — 最小化抽象层，代码直白易读
  完备 — 覆盖 Agent 运行所需的全部核心模块
  生产可用 — 错误恢复、上下文管理、超时控制等生产级特性
```

---

## 架构总览

```
                    ┌─────────────────────┐
                    │       Engine        │
                    │   (MainLoop 核心)    │
                    └──┬───┬───┬───┬─────┘
                       │   │   │   │
          ┌────────────┘   │   │   └────────────┐
          ▼                ▼   ▼                 ▼
    ┌──────────┐   ┌──────────┐   ┌──────────────────┐
    │ Provider │   │ Schema   │   │   Tool Registry  │
    │ (模型接口) │   │ (类型层)  │   │  (工具注册/调用)  │
    └──────────┘   └──────────┘   └──────────────────┘
          │                               │
    ┌─────┴────┐                    ┌────┴────┐
    ▼          ▼                    ▼         ▼
┌────────┐ ┌────────┐         ┌────────┐ ┌────────┐
│ OpenAI │ │Anthropic│        │  bash  │ │read /  │
│Provider│ │Provider │        │  Tool  │ │write   │
└────────┘ └────────┘         └────────┘ └────────┘
```

**Engine** 驱动 Agent 主循环：**推理 → 工具调用 → 观察 → 继续推理**

---

## 核心特性

### Two-Stage ReAct 循环

harness9 独创的 **剥夺-恢复工具策略**：

```
传统 ReAct（单阶段）:
  Turn N: LLM(messages, tools) → 可能缺乏深思熟虑的行动

Two-Stage ReAct（harness9）:
  Turn N:
    Phase 1: LLM(messages, tools=nil) → 被迫深度思考
    Phase 2: LLM(messages + thinking, tools=all) → 基于思考的精准行动
```

Phase 1 传入 `nil` 工具列表，剥夺模型的行动能力，强制其进行纯文本推理。Phase 2 恢复完整工具列表，模型基于 Phase 1 的思考制定精准行动计划。

详见 [Agent Loop 核心实现原理](docs/核心功能/agent-loop.md)。

### 并发工具执行

同一 Turn 中的多个工具调用**并发执行**，每个工具拥有独立超时控制：

```go
// 每个工具独立超时，互不影响
toolCtx, cancel = context.WithTimeout(ctx, e.ToolTimeout)
results[idx] = e.registry.Execute(toolCtx, tc)
```

### 流式输出

支持 `Run`（阻塞）和 `RunStream`（流式）双模式，共享同一引擎配置：

```go
// 阻塞式
err := eng.Run(ctx, prompt)

// 流式：逐 token 消费
stream, _ := eng.RunStream(ctx, prompt)
for evt := range stream {
    switch evt.Type {
    case engine.EventActionDelta:
        fmt.Print(evt.Data.(string))
    case engine.EventDone:
        return
    }
}
```

### 自愈能力

工具执行失败时，错误信息原样回传给 LLM，触发自动重试：

```go
// IsError=true 时，LLM 能看到错误原因并尝试自愈
ToolResult{ToolCallID: id, Output: "command not found: foo", IsError: true}
```

引擎在注入 Observation 时会把 `IsError` 带到 `schema.Message` 上，Provider 适配器
（Anthropic 的 `tool_result.is_error`）原样透传给 LLM，让模型明确感知到"这是失败"
而不是从文本里去猜。

详见 [Tool Calling 工具调用系统](docs/核心功能/tool-calling.md)。

### 工具层默认安全基线

各内置工具在"默认开箱即用"和"给调用方足够钩子"之间取舍：

- **`bash`**：默认启用基础 deny-list，拦截 `rm -rf` 系统路径/家目录/上级路径、
  fork bomb、`mkfs`、`dd` 写块设备、`curl | bash` 等明显破坏性模式。命中时
  作为 Observation 回给 LLM 让其自己调整，不中断循环。命令字符串如果包含
  未闭合引号等无法安全解析的结构，按 **fail-closed** 语义拦截（"无法确定 →
  保守拦截"）。需要在研究 / 受信任环境里显式放行时用：

  ```go
  tools.NewBashTool(workDir, tools.WithAllowDangerous(true))
  ```

  其他可选项：`WithBashMaxOutputLen(n)`、`WithBashHardTimeout(d)`。

- **`read_file`**：默认单次读取上限 64 KB，支持 `offset` / `limit` 参数分页读取
  大文件；截断发生时输出保证是合法 UTF-8（在 rune 边界回退），并提示下一个
  可用的 `offset` 便于 LLM 续读。可选 `WithMaxReadLen(n)`。

- **`write_file`**：沙箱边界 + 父目录 auto-mkdir + 覆盖写入。

这三者共享的路径沙箱 `safePath` 拒绝任何超出工作区的路径遍历尝试
（`../../etc/passwd` 类）。

---

## 快速开始

### 环境要求

- Go 1.25+
- OpenAI 或 Anthropic API Key

### 安装 & 运行

```bash
# 克隆项目
git clone https://github.com/harness9/harness9
cd harness9

# 复制并填写 API 配置
cp .env.example .env
# 编辑 .env，填入 OPENAI_API_KEY 等配置

# 构建
go build ./cmd/harness9

# 运行
go run ./cmd/harness9
```

### 测试

```bash
go test ./...
go test -cover ./...
```

### 最小示例

```go
package main

import (
    "context"
    "log"
    "time"

    "github.com/harness9/internal/engine"
    "github.com/harness9/internal/provider"
    "github.com/harness9/internal/tools"
)

func main() {
    workDir := "/your/project"

    p, err := provider.NewOpenAIProvider("gpt-4o")
    if err != nil {
        log.Fatalf("init provider: %v", err)
    }

    registry := tools.NewRegistry()
    registry.Register(tools.NewBashTool(workDir))
    registry.Register(tools.NewReadFileTool(workDir))
    registry.Register(tools.NewWriteFileTool(workDir))

    eng := engine.NewAgentEngine(p, registry, workDir, true,
        engine.WithMaxTurns(50),
        engine.WithToolTimeout(30*time.Second),
    )

    if err := eng.Run(context.Background(), "帮我列出当前目录下的所有 Go 文件"); err != nil {
        log.Fatalf("run: %v", err)
    }
}
```

---

## 核心模块

| 模块 | 说明 | 状态 |
|------|------|:----:|
| **Engine** | Two-Stage ReAct 主循环，阻塞 + 流式双模式 | ✅ |
| **Provider** | LLM 统一接口，OpenAI / Anthropic 适配器 | ✅ |
| **Schema** | 跨组件共享的核心数据类型 | ✅ |
| **Tools** | 工具注册表 + 内置工具（bash / read_file / write_file） | ✅ |
| **Env** | 零依赖 `.env` 配置加载器 | ✅ |
| **Memory** | 会话记忆持久化 | 规划中 |
| **Feishu** | 飞书机器人集成 | 规划中 |

---

## 项目结构

```
harness9/
├── cmd/
│   └── harness9/
│       └── main.go                  # 程序入口，组装各模块并启动引擎
├── internal/
│   ├── engine/
│   │   ├── agent_loop.go            # 阻塞式 Two-Stage ReAct 主循环
│   │   ├── agent_loop_test.go       # 主循环单元测试
│   │   └── stream.go                # 流式 Two-Stage ReAct 主循环
│   ├── provider/
│   │   ├── interface.go             # LLMProvider 接口定义
│   │   ├── openai.go                # OpenAI 兼容 API 适配器
│   │   ├── anthropic.go             # Anthropic 兼容 API 适配器
│   │   └── mock.go                  # 测试用 Mock Provider
│   ├── schema/
│   │   ├── message.go               # 核心消息类型（Message、ToolCall、ToolResult 等）
│   │   └── stream.go                # 流式增量类型（StreamChunk）
│   ├── tools/
│   │   ├── base.go                  # BaseTool 接口定义
│   │   ├── registry.go              # 工具注册中心
│   │   ├── safe_path.go             # 共享路径沙箱校验（防 Path Traversal）
│   │   ├── safe_path_test.go        # 路径沙箱单元测试
│   │   ├── bash.go                  # bash 工具（Shell 命令执行）
│   │   ├── read_file.go             # read_file 工具（文件读取）
│   │   └── write_file.go            # write_file 工具（文件写入）
│   ├── env/
│   │   ├── env.go                   # 零依赖 .env 配置加载器
│   │   └── env_test.go              # 配置加载单元测试
│   ├── memory/
│   │   └── memory.go                # 会话记忆持久化（规划中）
│   └── feishu/
│       └── feishu.go                # 飞书 Webhook / 事件订阅处理（规划中）
├── docs/
│   └── 核心功能/
│       ├── agent-loop.md            # Agent Loop 核心实现原理（Two-Stage ReAct 详解）
│       └── tool-calling.md          # Tool Calling 工具调用系统详解
├── .env.example                     # 环境变量配置模板
├── go.mod
├── AGENTS.md                        # 项目开发规范与上下文
├── CLAUDE.md -> AGENTS.md           # 符号链接，保持同步
└── README.md
```

---

## 文档索引

| 文档 | 内容 |
|------|------|
| [Agent Loop 核心实现原理](docs/核心功能/agent-loop.md) | Two-Stage ReAct 设计理念、数据模型、流式架构、Provider 对比 |
| [Tool Calling 工具调用系统](docs/核心功能/tool-calling.md) | 工具接口、并发模型、内置工具详解、扩展指南 |
| [AGENTS.md](AGENTS.md) | 项目开发规范、编码标准、架构决策 |

---

## 对标框架

| 框架 | 优势 | 与 harness9 的差异 |
|------|------|-------------------|
| Claude Agent SDK | 深度 Anthropic 集成 | Extended Thinking 在 API 内置，不分离阶段 |
| OpenAI Agents SDK | 官方支持，生态完整 | Handoffs 机制，非 Go 原生 |
| OpenHarness | 显式循环设计清晰 | Python，无两阶段思考分离 |
| OpenCode | Plan/Build Agent 切换 | TypeScript，通过独立 Agent 而非 Turn 内阶段分离 |

---

## License

MIT
