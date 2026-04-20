# AGENTS.md — harness9 项目开发指南

## 项目概述

harness9 是一款基于 Go 语言构建的轻量级、功能完备、生产可用的 Agent Harness 框架，旨在提供简洁、高效、可扩展的 Agent 编排能力。

## 技术栈

- **语言**: Go 1.25+（go.mod 中指定 `go 1.25.3`）
- **模块路径**: `github.com/harness9`
- **无外部依赖**: 当前项目为零依赖起步，按需引入第三方库

## 核心代码结构

```
harness9/
├── cmd/
│   └── harness9/
│       └── main.go              # 程序入口，组装各模块并启动引擎
├── internal/
│   ├── engine/                  # Agent 核心引擎 — MainLoop 实现
│   │   └── engine.go            # 循环调度：推理 → 工具调用 → 观察 → 继续推理
│   ├── provider/                # 大模型接口抽象与具体厂商 SDK 实现
│   │   └── provider.go          # Provider 接口定义 + 具体厂商实现
│   ├── context/                 # Token 监控、Prompt 动态组装
│   │   └── manager.go           # 上下文窗口管理、消息截断策略
│   ├── tools/                   # 工具注册表、Middleware、基础极简工具
│   │   ├── registry.go          # 工具注册中心
│   │   ├── middleware.go        # 工具中间件（日志、超时、权限等）
│   │   ├── bash.go              # Bash 工具实现
│   │   └── edit.go              # 文件编辑工具实现
│   ├── memory/                  # 基于文件系统的记忆状态存取
│   │   └── memory.go            # 会话记忆持久化
│   └── feishu/                  # 飞书机器人交互回调
│       └── feishu.go            # 飞书 Webhook / 事件订阅处理
├── docs/                        # 项目文档
├── go.mod
├── AGENTS.md                    # 本文件 — 项目开发规范与上下文
├── CLAUDE.md -> AGENTS.md       # 符号链接，保持同步
└── README.md
```

## 核心架构设计

### 1. Engine（核心引擎）
- 实现 Agent 主循环（MainLoop）：`推理 → 工具调用 → 观察 → 继续推理`
- 负责调度 Provider 和 Tool Registry
- 管理会话生命周期和错误恢复

### 2. Provider（模型提供者）
- 定义统一的 `Provider` 接口，抽象不同大模型厂商的 API 差异
- 支持多厂商切换（OpenAI、Anthropic、Google 等）
- 处理流式/非流式响应、重试、错误处理

### 3. Context（上下文管理）
- 监控 Token 使用量，防止超出模型上下文窗口
- 动态组装 Prompt（系统提示 + 历史消息 + 工具结果）
- 实现消息截断和优先级策略

### 4. Tools（工具系统）
- 统一的工具注册表（Registry），支持动态注册
- 中间件机制（日志、超时、权限校验）
- 内置基础工具：Bash 执行、文件编辑等

### 5. Memory（记忆系统）
- 基于文件系统的会话状态持久化
- 支持跨会话记忆检索

### 6. Feishu（飞书集成）
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
- （当前为零依赖，随项目推进逐步更新此列表）

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
