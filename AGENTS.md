# AGENTS.md — harness9 项目开发指南

## 1. 项目概述

harness9 是一款基于 Go 语言构建的**轻量级、功能完备、生产可用**的 Agent Harness 框架，旨在提供简洁、高效、可扩展的 Agent 编排能力。

### 核心设计理念

| 原则 | 说明 |
|------|------|
| **简洁** | 最小化抽象层，代码直白易读；极少的直接依赖数 |
| **完备** | 覆盖 Agent 运行所需的全部核心模块（Engine / Provider / Schema / Tools / Env） |
| **生产可用** | 错误恢复、上下文管理、超时控制、并发工具执行、Path Traversal 防护等生产级特性 |

### 核心架构

- **Two-Stage ReAct**: 每个 Turn 拆分 Thinking（剥夺工具的深度思考）→ Action（恢复工具的精准行动），避免模型的冲动行为
- **并发工具执行**: 同 Turn 内多个工具调用并发执行，每工具独立超时控制
- **双模式运行**: 阻塞式 `Run` + 流式 `RunStream`，共享同一引擎实例
- **自愈能力**: 工具执行失败时，错误信息原样回传给 LLM，触发自动重试

### 参考框架

| 框架 | 来源 | GitHub |
|------|------|--------|
| DeepAgents | LangChain | https://github.com/langchain-ai/deepagents |
| OpenHarness | HKUDS | https://github.com/HKUDS/OpenHarness |
| OpenCode | Anomaly | https://github.com/anomalyco/opencode |
| OpenClaw | OpenClaw | https://github.com/openclaw/openclaw |
| HermesAgent | NousResearch | https://github.com/NousResearch/hermes-agent |
| Claude Agent SDK | Anthropic | https://code.claude.com/docs/en/agent-sdk/overview |
| OpenAI Agent SDK | OpenAI | https://developers.openai.com/api/docs/guides/agents |

---

## 2. 核心技术栈

### 语言与运行时

- **Go**: `1.25.3`（`go.mod` 中指定 `go 1.25.3`）
- **模块路径**: `github.com/harness9`

### 直接依赖

| 依赖 | 版本 | 用途 |
|------|------|------|
| `github.com/openai/openai-go/v3` | v3.32.0 | OpenAI 兼容 API 适配器（Chat Completions + 流式） |
| `github.com/anthropics/anthropic-sdk-go` | v1.38.0 | Anthropic 兼容 API 适配器（Messages + 流式） |

### 间接依赖（自动引入，无需手动管理）

- `github.com/tidwall/gjson` / `match` / `pretty` / `sjson` — JSON 解析
- `github.com/bahlo/generic-list-go` / `github.com/wk8/go-ordered-map/v2` — 有序集合
- `github.com/buger/jsonparser` — JSON 解析
- `github.com/invopop/jsonschema` — JSON Schema 生成
- `github.com/mailru/easyjson` — JSON 序列化
- `golang.org/x/sync` — 并发原语
- `gopkg.in/yaml.v3` — YAML 解析

### 开发工具

- `gofmt` / `goimports` — 代码格式化
- `go test` — 标准库测试框架
- `go build` / `go run` — 编译与运行

---

## 3. 编码规范

### 3.1 格式化

- **所有代码必须通过 `gofmt` 格式化**，无例外
- 使用 `goimports` 管理导入排序
- Tab 缩进，不使用空格

### 3.2 命名规范

| 类别 | 规范 | 示例 |
|------|------|------|
| 包名 | 小写、单单词、无下划线 | `engine`、`provider`、`schema` |
| 导出类型/函数 | PascalCase | `AgentEngine`、`NewRegistry`、`LLMProvider` |
| 未导出类型/函数 | camelCase | `mainLoop`、`maxRetries`、`joinContent` |
| 接口名 | 以 `-er` 后缀为惯例，或不加后缀 | `Provider`、`Registry`、`BaseTool` |
| 常量 | PascalCase（导出）或 camelCase（未导出），**不使用全大写** | `RoleSystem`、`maxLogOutputLen` |
| 测试文件 | `xxx_test.go`，测试函数以 `Test` 开头 | `agent_loop_test.go` |
| 配置选项函数 | `With` 前缀 | `WithMaxTurns`、`WithToolTimeout` |

### 3.3 错误处理

- 显式检查所有 `error` 返回值，**禁止使用 `_` 忽略**
- 错误消息**不以大写字母开头、不以句号结尾**
- 使用 `fmt.Errorf("context: %w", err)` 包装错误，保留错误链
- 自定义错误类型放在所属包内，命名以 `Error` 结尾（如 `TimeoutError`）
- 工具执行失败通过 `ToolResult{IsError: true}` 回传，而非终止循环

### 3.4 并发

- 优先使用 `channel` 而非共享内存
- 使用 `context.Context` 管理生命周期和取消
- goroutine 必须有明确的退出机制
- 并发工具执行使用 `sync.WaitGroup` + 预分配切片 + 索引写入，确保结果顺序

### 3.5 测试

- 使用标准库 `testing` 包（不引入第三方断言库）
- 表驱动测试优先（Table-Driven Tests）
- Mock 实现放在同包 `mock.go` 或 `*_test.go` 文件中
- 运行命令：`go test ./...`
- 测试覆盖率：`go test -cover ./...`

### 3.6 代码组织

- 同一目录下所有 `.go` 文件必须属于同一个包
- 导入分组顺序：标准库 → 第三方库 → 项目内部包，组间空行分隔
- **接口定义在使用者侧，而非实现者侧**（如 `Registry` 接口定义在 `tools` 包中，被 `engine` 包依赖）
- 避免 `init()` 函数，除非有充分理由
- 构造函数命名：`New` + 类型名（如 `NewRegistry`、`NewAgentEngine`）

### 3.7 包文档

- 每个包必须有包级注释（`// Package xxx ...`），描述包的职责和设计决策
- 导出类型、函数、常量必须有关联注释
- 注释使用中文描述设计理念，英文描述 API

---

## 4. 项目结构

```
harness9/
├── cmd/
│   └── harness9/
│       └── main.go                  # 程序入口，组装各模块并启动引擎
├── internal/
│   ├── engine/                      # Agent 核心引擎 — Two-Stage ReAct 主循环
│   │   ├── agent_loop.go            # 阻塞式主循环（Run）+ 两阶段编排逻辑
│   │   ├── agent_loop_test.go       # 主循环单元测试
│   │   └── stream.go                # 流式主循环（RunStream）+ engine.Event 事件类型
│   ├── provider/                    # 大模型接口抽象与具体厂商 SDK 实现
│   │   ├── interface.go             # LLMProvider 接口定义（Generate + GenerateStream）
│   │   ├── openai.go                # OpenAI 兼容 API 适配器（支持 OpenRouter / Azure）
│   │   ├── anthropic.go             # Anthropic 兼容 API 适配器（Messages API）
│   │   └── mock.go                  # 测试用 Mock Provider
│   ├── schema/                      # 跨组件共享的核心数据类型
│   │   ├── message.go               # Message、Role、ToolCall、ToolResult、ToolDefinition
│   │   └── stream.go                # StreamChunk、StreamChunkType（Provider 层流式类型）
│   ├── tools/                       # 工具注册表 + 内置工具
│   │   ├── base.go                  # BaseTool 接口定义（Name / Definition / Execute）
│   │   ├── registry.go              # 工具注册中心（Register / GetAvailableTools / Execute）
│   │   ├── safe_path.go             # 共享路径沙箱校验（防 Path Traversal 攻击）
│   │   ├── safe_path_test.go        # 路径沙箱单元测试
│   │   ├── bash.go                  # bash 工具（Shell 命令执行，YOLO 哲学）
│   │   ├── read_file.go             # read_file 工具（沙箱保护，4096 字节截断）
│   │   └── write_file.go            # write_file 工具（沙箱保护，Auto-Mkdir）
│   ├── env/                         # 环境配置
│   │   ├── env.go                   # 零依赖 .env 文件加载器（系统变量优先）
│   │   └── env_test.go              # 配置加载单元测试
│   ├── memory/                      # 会话记忆持久化（规划中）
│   │   └── memory.go
│   └── feishu/                      # 飞书机器人集成（规划中）
│       └── feishu.go
├── docs/
│   └── 核心功能/
│       ├── agent-loop.md            # Agent Loop 核心实现原理（Two-Stage ReAct 详解）
│       └── tool-calling.md          # Tool Calling 工具调用系统详解
├── .env.example                     # 环境变量配置模板
├── go.mod                           # Go 模块定义
├── go.sum                           # 依赖锁定
├── AGENTS.md                        # 本文件 — 项目开发规范与上下文
├── CLAUDE.md -> AGENTS.md           # 符号链接，保持同步
└── README.md                        # 项目介绍与快速开始
```

### 架构分层

```
┌─────────────────────────────────────────────────┐
│                    cmd/harness9                   │
│         (入口: 组装 Engine + Provider + Registry)  │
└──────────────────────┬──────────────────────────┘
                       │
┌──────────────────────▼──────────────────────────┐
│                engine (编排层)                     │
│   Run / RunStream — Two-Stage ReAct 主循环        │
│   上下文管理 / 工具调度 / 终止条件检测               │
└────┬──────────────┬──────────────┬──────────────┘
     │              │              │
┌────▼────┐  ┌──────▼──────┐  ┌───▼───────────┐
│ provider │  │   schema    │  │    tools      │
│ (模型层)  │  │  (数据层)    │  │  (工具层)      │
│ OpenAI   │  │ Message     │  │ Registry      │
│ Anthropic│  │ ToolCall    │  │ bash/read/    │
│          │  │ ToolResult  │  │ write_file    │
└──────────┘  └─────────────┘  └───────────────┘
     │
┌────▼────┐
│   env   │
│ (配置层)  │
└─────────┘
```

### 模块职责

| 模块 | 职责 | 状态 |
|------|------|:----:|
| **cmd/harness9** | 程序入口，组装 Provider、Registry、Engine 并启动 | ✅ |
| **engine** | Two-Stage ReAct 主循环，阻塞 + 流式双模式，并发工具调度 | ✅ |
| **provider** | LLM 统一接口 + OpenAI / Anthropic SDK 适配器 | ✅ |
| **schema** | 跨组件共享的核心数据类型（Message、ToolCall 等） | ✅ |
| **tools** | 工具注册表 + 内置工具（bash / read_file / write_file）+ 路径沙箱 | ✅ |
| **env** | 零依赖 `.env` 配置加载器（系统变量优先） | ✅ |
| **memory** | 会话记忆持久化 | 🔲 规划中 |
| **feishu** | 飞书机器人 Webhook / 事件订阅处理 | 🔲 规划中 |

---

## 5. 开发流程

### 5.1 环境准备

```bash
# 克隆项目
git clone https://github.com/harness9/harness9
cd harness9

# 配置 API Key
cp .env.example .env
# 编辑 .env，填入 OPENAI_API_KEY 和/或 ANTHROPIC_API_KEY

# 安装依赖
go mod download
```

### 5.2 构建与运行

```bash
# 构建二进制
go build ./cmd/harness9

# 阻塞模式运行
go run ./cmd/harness9

# 流式模式运行
go run ./cmd/harness9 stream
```

### 5.3 测试

```bash
# 运行全部测试
go test ./...

# 带覆盖率
go test -cover ./...

# 带详细输出
go test -v ./...

# 运行特定包的测试
go test -v ./internal/engine/
```

### 5.4 代码检查

```bash
# 格式化检查
gofmt -l .

# 格式化所有文件
gofmt -w .

# 导入排序
goimports -w .

# 运行 go vet
go vet ./...
```

### 5.5 添加新工具

1. 在 `internal/tools/` 下创建 `xxx.go`，实现 `BaseTool` 接口：

```go
type XxxTool struct {
    workDir string
}

func (t *XxxTool) Name() string                   { return "xxx" }
func (t *XxxTool) Definition() schema.ToolDefinition { /* ... */ }
func (t *XxxTool) Execute(ctx context.Context, args json.RawMessage) (string, error) { /* ... */ }
```

2. 使用 `safePath()` 校验所有文件路径参数，防止 Path Traversal
3. 在 `internal/tools/xxx_test.go` 中添加表驱动测试
4. 在 `cmd/harness9/main.go` 中 `registry.Register(NewXxxTool(workDir))` 注册

### 5.6 添加新 Provider

1. 在 `internal/provider/` 下创建 `xxx.go`
2. 实现 `LLMProvider` 接口（`Generate` + `GenerateStream`）
3. 负责将 `schema.Message` / `schema.ToolDefinition` 转换为厂商 SDK 的类型
4. 在 `internal/provider/xxx_test.go` 中添加测试（可使用 Mock API 或录制回放）

### 5.7 Git 工作流

- 主分支：`main`
- 功能分支命名：`feature/<描述>`、`fix/<描述>`、`refactor/<描述>`
- Commit 消息：中文描述，简洁明确，聚焦"为什么"而非"做了什么"
- 所有代码提交前必须通过 `go test ./...` 和 `gofmt -l .` 检查

---

## 6. 特殊约束

### 6.1 Provider 实现约束

#### Anthropic Messages API — user/assistant 严格交替
Anthropic Messages API **禁止连续 assistant 消息**，也禁止多条 system 消息。项目通过以下机制保证兼容性：

- Two-Stage ReAct 的 Thinking + Action 两个 assistant 消息在注入 contextHistory 时**合并为一条**（`joinContent` 函数）
- System Prompt 仅在初始化 contextHistory 时注入一次（`RoleSystem` 消息）

Provider 实现者需注意：`convertMessages()` 方法应负责将 `schema.Message` 的 `role` 正确映射为厂商 API 的消息角色格式。

#### OpenAI 兼容接口 — 不限制连续 assistant 消息
OpenAI 及 OpenRouter 等兼容接口**不限制**连续 assistant 消息，但项目统一采用合并策略以保持一致性。

#### 工具剥夺机制
- `availableTools == nil` → Thinking 阶段，Provider **不传递** tools 参数给 API
- `availableTools != nil` → Action 阶段，Provider **完整传递** tools 参数

Provider 实现者必须正确处理 `nil` 工具列表。

### 6.2 工具系统约束

#### 路径沙箱（Path Traversal 防护）
所有涉及文件操作的工具（`read_file`、`write_file`、`bash`）必须使用 `safePath()` 校验路径：

- 拒绝绝对路径跨越 `workDir` 边界的请求
- 拒绝 `../` 路径穿越攻击
- `safePath()` 位于 `internal/tools/safe_path.go`，是所有文件工具的共享校验入口

#### 工具超时
- 每个工具调用拥有独立超时控制（`WithToolTimeout` 配置项）
- 超时不影响同一 Turn 内其他工具的并发执行
- 超时的工具会返回 `IsError: true` 的结果，LLM 可据此重试

#### 工具结果的截断
- 日志输出截断至 512 字节（`maxLogOutputLen`）
- `read_file` 工具截断至 4096 字节
- 截断时应在返回文本末尾添加明确的截断标记

### 6.3 引擎约束

#### 三重终止保障
Agent 循环通过以下三种机制确保不会无限运行：

1. **自然终止**: 模型不再发起 ToolCall（`len(responseMsg.ToolCalls) == 0`）
2. **MaxTurns**: 超过最大 Turn 数（默认 50，可通过 `WithMaxTurns` 配置）
3. **Context 取消**: 外部调用 `cancel()` 或 `context.WithTimeout` 到期

#### Context 管理
- `eng.Run(ctx, prompt)` 的 `ctx` 控制整个循环的生命周期
- 工具执行从 `ctx` 派生独立子 Context（`context.WithTimeout(ctx, e.ToolTimeout)`）
- 引擎在每轮循环开始时检查 `ctx.Done()`

### 6.4 配置加载约束

- `.env` 文件使用零依赖的 `internal/env/env.go` 加载器
- **系统环境变量优先于 .env 文件**：已存在的环境变量不会被覆盖
- `.env` 文件不存在时不报错，程序可继续运行（需外部提供环境变量）
- 配置加载必须在 Provider 初始化之前完成
- 支持注释行（`#` 开头）、空行、引号值（`"value"` 或 `'value'`）

### 6.5 消息结构约束

#### JSON Tag 规范
`schema.Message` 的 JSON tag 使用 `json:"tool_calls,omitempty"` 格式（snake_case + omitempty）：

- `role`、`content`、`tool_calls`、`tool_call_id` 等字段使用 snake_case
- `ToolCallID` 用于 Observation 消息的请求-响应关联
- `ToolCall.Arguments` 使用 `json.RawMessage` 延迟反序列化，避免引擎层过早类型断言

### 6.6 安全约束

- `.env` 文件包含 API Key 等敏感信息，**禁止提交到 Git 仓库**（已在 `.gitignore` 中）
- 工具执行不进行输出过滤，LLM 可通过观察工具输出来调整行为（YOLO 哲学）
- 所有文件路径操作必须通过 `safePath()` 沙箱校验

### 6.7 第三方 API / SDK 使用规范

**重要**: 在确认使用某个第三方 API 或 SDK 时，**必须优先通过 context7 MCP 工具获取最新的官方文档和 API Doc**，确保：

1. 使用最新的 API 版本和推荐用法
2. 了解 Breaking Changes 和 Migration 指引
3. 获取准确的函数签名、参数类型和返回值定义
4. 参考官方最佳实践和示例代码

#### 已使用的第三方库

- `github.com/openai/openai-go` — OpenAI 官方 Go SDK（Chat Completions + 流式）
- `github.com/anthropics/anthropic-sdk-go` — Anthropic 官方 Go SDK（Messages + 流式）

#### 选型原则

- 优先选择官方或社区维护良好的 SDK
- 优先选择轻量级、依赖少的库
- 引入新依赖前需评估：维护状态、Issue 响应速度、License 兼容性
