# AGENTS.md — harness9 项目开发指南

## 项目概述

harness9 是一款基于 Go 语言构建的轻量级、功能完备、生产可用的 Agent Harness 框架，旨在提供简洁、高效、可扩展的 Agent 编排能力。

## 技术栈

- **语言**: Go 1.25+（go.mod 中指定 `go 1.25.3`）
- **模块路径**: `github.com/harness9`
- **第三方依赖**: `github.com/openai/openai-go`、`github.com/anthropics/anthropic-sdk-go`

## 核心代码结构

```
harness9/
├── cmd/
│   └── harness9/
│       └── main.go                  # 程序入口，组装各模块并启动引擎
├── internal/
│   ├── engine/                      # Agent 核心引擎 — Two-Stage ReAct 主循环
│   │   ├── agent_loop.go            # 阻塞式主循环：推理 → 工具调用 → 观察 → 继续推理
│   │   ├── agent_loop_test.go       # 主循环单元测试
│   │   └── stream.go                # 流式主循环（RunStream + engine.Event）
│   ├── provider/                    # 大模型接口抽象与具体厂商 SDK 实现
│   │   ├── interface.go             # LLMProvider 接口定义（Generate + GenerateStream）
│   │   ├── openai.go                # OpenAI 兼容 API 适配器（OpenAI / OpenRouter / Azure）
│   │   ├── anthropic.go             # Anthropic 兼容 API 适配器
│   │   └── mock.go                  # 测试用 Mock Provider
│   ├── schema/                      # 跨组件共享的核心数据类型
│   │   ├── message.go               # Message、ToolCall、ToolResult、ToolDefinition 等
│   │   └── stream.go                # StreamChunk、StreamChunkType（Provider 层流式类型）
│   ├── tools/                       # 工具注册表 + 内置工具
│   │   ├── base.go                  # BaseTool 接口定义
│   │   ├── registry.go              # 工具注册中心（Register / Execute / GetAvailableTools）
│   │   ├── safe_path.go             # 共享路径沙箱校验（防 Path Traversal）
│   │   ├── safe_path_test.go        # 路径沙箱单元测试
│   │   ├── bash.go                  # bash 工具（Shell 命令执行，YOLO 哲学）
│   │   ├── read_file.go             # read_file 工具（沙箱保护，4096 字节截断）
│   │   └── write_file.go            # write_file 工具（沙箱保护，Auto-Mkdir）
│   ├── env/                         # 环境配置
│   │   ├── env.go                   # 零依赖 .env 文件加载器（系统变量优先）
│   │   └── env_test.go              # 配置加载单元测试
│   ├── memory/                      # 基于文件系统的记忆状态存取（规划中）
│   │   └── memory.go                # 会话记忆持久化
│   └── feishu/                      # 飞书机器人交互回调（规划中）
│       └── feishu.go                # 飞书 Webhook / 事件订阅处理
├── docs/
│   └── 核心功能/
│       ├── agent-loop.md            # Agent Loop 核心实现原理（Two-Stage ReAct 详解）
│       └── tool-calling.md          # Tool Calling 工具调用系统详解
├── .env.example                     # 环境变量配置模板
├── go.mod
├── AGENTS.md                        # 本文件 — 项目开发规范与上下文
├── CLAUDE.md -> AGENTS.md           # 符号链接，保持同步
└── README.md
```

## 核心架构设计

### 1. Engine（核心引擎）
- 实现 **Two-Stage ReAct** 主循环：`Phase 1(Thinking) → Phase 2(Action) → ToolCall → Observation → 继续推理`
- 支持阻塞模式（`Run`）和流式模式（`RunStream`），共享同一引擎实例
- 并发执行同 Turn 内的多个工具调用，每工具独立超时控制
- 三重终止保障：自然终止 + MaxTurns + Context 取消

### 2. Provider（模型提供者）
- 定义统一的 `LLMProvider` 接口（`Generate` + `GenerateStream`），抽象不同大模型厂商的 API 差异
- 已实现：OpenAI 兼容适配器（支持 OpenRouter / Azure）、Anthropic 适配器
- `availableTools=nil` 时不传递工具列表（Thinking 阶段剥夺工具）

### 3. Schema（数据类型层）
- 跨所有组件共享的核心类型：`Message`、`ToolCall`、`ToolResult`、`ToolDefinition`
- Provider 层流式类型：`StreamChunk`，Engine 层事件类型：`engine.Event`
- `ToolCall.Arguments` 使用 `json.RawMessage` 延迟反序列化

### 4. Tools（工具系统）
- 统一的工具注册表（`Registry`），支持动态注册
- 内置工具：`bash`（Shell 执行）、`read_file`（文件读取）、`write_file`（文件写入）
- 共享路径沙箱 `safePath`，防止 Path Traversal 攻击

### 5. Env（配置加载）
- 零依赖 `.env` 文件加载器，系统环境变量优先，支持引号值和注释

### 6. Memory（记忆系统）[规划中]
- 基于文件系统的会话状态持久化
- 支持跨会话记忆检索

### 7. Feishu（飞书集成）[规划中]
- 飞书机器人 Webhook 回调处理
- 事件订阅与消息路由

## Go 编码规范

### 格式化
- **所有代码必须通过 `gofmt` 格式化**，无例外
- 使用 `goimports` 管理导入排序
- Tab 缩进，不使用空格

### 命名规范
- 包名：小写、单词、无下划线（如 `engine`、`provider`）
- 导出类型/函数：PascalCase（如 `AgentEngine`、`NewProvider`）
- 未导出类型/函数：camelCase（如 `mainLoop`、`maxRetries`）
- 接口名：以 `-er` 后缀为惯例（如 `Provider`、`Runner`），或不加后缀（如 `Tool`）
- 常量：PascalCase（导出）或 camelCase（未导出），不使用全大写
- 测试文件：`xxx_test.go`，测试函数以 `Test` 开头

### 错误处理
- 显式检查所有 `error` 返回值，禁止使用 `_` 忽略
- 错误消息不以大写字母开头，不以句号结尾
- 使用 `fmt.Errorf("context: %w", err)` 包装错误，保留错误链
- 自定义错误类型放在所属包内，命名以 `Error` 结尾（如 `TimeoutError`）

### 并发
- 优先使用 channel 而非共享内存
- 使用 `context.Context` 管理生命周期和取消
- goroutine 必须有明确的退出机制

### 测试
- 使用标准库 `testing` 包
- 表驱动测试优先（Table-Driven Tests）
- 运行命令：`go test ./...`
- 测试覆盖率：`go test -cover ./...`

### 代码组织
- 同一目录下所有 `.go` 文件必须属于同一个包
- 导入分组：标准库 / 第三方库 / 项目内部包，组间空行分隔
- 接口定义在使用者侧，而非实现者侧
- 避免 `init()` 函数，除非有充分理由

## 第三方 API / SDK 使用规范

**重要**: 在确认使用某个第三方 API 或 SDK 时，**必须优先通过 context7 MCP 工具获取最新的官方文档和 API Doc**，确保：
1. 使用最新的 API 版本和推荐用法
2. 了解 Breaking Changes 和 Migration 指引
3. 获取准确的函数签名、参数类型和返回值定义
4. 参考官方最佳实践和示例代码

### 已使用的第三方库
- `github.com/openai/openai-go` — OpenAI 官方 Go SDK（Chat Completions + 流式）
- `github.com/anthropics/anthropic-sdk-go` — Anthropic 官方 Go SDK（Messages + 流式）

### 第三方库选型原则
- 优先选择官方或社区维护良好的 SDK
- 优先选择轻量级、依赖少的库
- 引入新依赖前需评估维护状态、Issue 响应速度、License

## 参考项目

| 框架 | 来源 | GitHub |
|------|------|--------|
| DeepAgents | LangChain | https://github.com/langchain-ai/deepagents |
| OpenHarness | HKUDS | https://github.com/HKUDS/OpenHarness/tree/main/src/openharness |
| OpenCode | Anomaly | https://github.com/anomalyco/opencode |
| OpenClaw | OpenClaw | https://github.com/openclaw/openclaw |
| HermesAgent | NousResearch | https://github.com/NousResearch/hermes-agent |
| Claude Agent SDK | Anthropic | https://code.claude.com/docs/en/agent-sdk/overview |
| OpenAI Agent SDK | OpenAI | https://developers.openai.com/api/docs/guides/agents |
