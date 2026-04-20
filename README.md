# harness9

一款轻量级、功能完备、生产可用的 Agent Harness 框架。

## 项目简介

harness9 使用 Go 语言构建，旨在提供简洁、高效、可扩展的 Agent 编排能力。对标 DeepAgents、Claude SDK、OpenClaw、OpenHarness 等主流框架。

核心设计理念：

- **简洁** — 最小化抽象层，代码直白易读
- **完备** — 覆盖 Agent 运行所需的全部核心模块
- **生产可用** — 错误恢复、上下文管理、超时控制等生产级特性

## 架构概览

```
                    ┌─────────────────────┐
                    │       Engine        │
                    │   (MainLoop 核心)    │
                    └──┬───┬───┬───┬─────┘
                       │   │   │   │
          ┌────────────┘   │   │   └────────────┐
          ▼                ▼   ▼                 ▼
    ┌──────────┐   ┌──────────┐   ┌──────────────────┐
    │ Provider │   │ Context  │   │   Tool Registry  │
    │ (模型接口) │   │ (上下文) │   │  (工具注册/调用)  │
    └──────────┘   └──────────┘   └──────────────────┘
                                         │
                                    ┌────┴────┐
                                    ▼         ▼
                              ┌────────┐ ┌────────┐
                              │ Memory │ │ Feishu │
                              │ (记忆)  │ │ (飞书)  │
                              └────────┘ └────────┘
```

**Engine** 驱动 Agent 主循环：`推理 → 工具调用 → 观察 → 继续推理`

## 项目结构

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

## 快速开始

### 环境要求

- Go 1.25+

### 构建 & 运行

```bash
# 构建
go build ./cmd/harness9

# 运行
go run ./cmd/harness9
```

### 测试

```bash
go test ./...
```

## 核心模块

| 模块 | 说明 | 状态 |
|------|------|------|
| Engine | Agent 主循环调度 | 开发中 |
| Provider | 大模型统一接口（OpenAI/Anthropic/Google） | 开发中 |
| Context | Token 监控与 Prompt 组装 | 开发中 |
| Tools | 工具注册表 + 中间件 + 内置工具 | 开发中 |
| Memory | 会话记忆持久化 | 开发中 |
| Feishu | 飞书机器人集成 | 开发中 |

## License

MIT
