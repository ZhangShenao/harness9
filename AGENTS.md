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

- **标准 ReAct**: 每个 Turn 执行一次 LLM 调用（携带完整工具列表），工具调用结果作为 Observation 注入上下文
- **并发工具执行**: 同 Turn 内多个工具调用并发执行，每工具独立超时控制
- **双模式运行**: 阻塞式 `Run` + 流式 `RunStream`，共享同一引擎实例
- **自愈能力**: 工具执行失败时，错误信息原样回传给 LLM，触发自动重试
- **双重压缩策略**: SummarizationCompactor（默认，LLM 摘要，保留语义 + 增量更新）和 TokenBudgetCompactor（回退，字符截断），均在 80% 阈值触发，双向修复孤立工具对
- **实际 Token 用量**: 从 API 响应 usage 字段提取，LLM 调用后实时更新 TUI 展示
- **Planning（先规划后执行）**: Plan Mode（工具层权限过滤）+ TodoStore（状态机校验）+ 自动续跑 + 停滞检测
- **Sub-Agent（子代理委派）**: 主代理通过 `task` 工具把边界清晰的子任务委派给运行在隔离 Session 上的专门子代理（独立上下文 + 受限工具集 + 可选模型覆盖）；内置 general-purpose 通用子代理（对标 Claude Code / DeepAgents，继承父全部可用工具与模型），支持 `.harness9/agents/*.md` 文件式定义、前台/后台双模式、`@agent` 直跑、TaskTracker 后台任务管理；安全保障：禁止递归 + 权限只能更严不能扩权 + 上下文完全隔离
- **文件系统能力**: OffloadHook（超大工具输出自动写入文件，context 保留摘要引用 + 分页检索）+ FilePlanWriter（todo 计划持久化到 markdown）+ DeleteSession 级联 GC
- **推理内容展示（Reasoning Display）**: Anthropic extended thinking（StreamChunkThinkingDelta）和 OpenRouter/DeepSeek reasoning_content 均路由为 EventThinkingDelta，TUI 以 `│` 前缀深灰色块流式渲染，与正文回复形成视觉层次区分
- **Shell 执行（`!` 前缀）**: 输入框以 `!` 开头进入 Shell 模式，命令通过 `bash -c` 异步执行（30s 超时），输出 inline 追加到对话流，并在下次 LLM dispatch 时前置注入为上下文；已知交互式命令（vim/ssh 等）自动拦截
- **Human-in-the-Loop 权限控制**: HookDecision（allow/deny/ask）三级决策 + DangerHook（19 条高危模式）+ PermissionHook（JSON 白名单，按需重载）+ 敏感路径硬保护（~/.ssh、~/.aws 等）+ TUI 五选项审批对话框 + PermissionMode 枚举
- **Long-Term Memory（跨会话长期记忆）**: `internal/ltm/` 包；SQLite `long_term_memories` + standalone FTS5 `memories_fts`，复用 `state.db` 连接；MEMORY.md 物化视图（top-N 有界注入，≤5KB，规避 token bomb）+ `memory_search` 按需 FTS5 检索；三路触发：显式 `memory_write`/`memory_search` 工具 + 压缩前 `Extractor`（LLM 提取，fail-open）+ `WithMemoryReminder`（每 N 轮注入提示，防御性副本，不持久化）；SHA256 内容签名去重 + TTL 过期 + 软删除（signature=NULL 释放槽位）+ 命中强化（use_count/last_used_at）+ 陈旧识别（`StaleCandidates`）；Phase 3 接缝：Provider/Embedder/Consolidator 接口 + noopProvider
- **Sandbox（Docker 容器级隔离）**: `internal/sandbox/` 包；`Environment` 接口（LocalEnvironment 进程级 / DockerEnvironment 容器级）+ `Container` 五状态生命周期（Pending→Running→Stopping→Terminated/Failed）+ `Manager`（Create/Destroy/DestroyAll/ReapOrphans/ListAll，并发安全）；工具透明路由（bash 命令通过 docker exec 进容器，文件工具通过 bind mount 共享 workDir）；安全加固：`--cap-drop all` + `--cap-add DAC_OVERRIDE/SETUID/SETGID`（包管理器所需最小能力）+ `--security-opt no-new-privileges:true` + `--pids-limit 256` + tmpfs nosuid/noexec/nodev；Agent 级隔离（主 Agent 和每个 Sub-Agent 各自拥有独立容器）；TUI SandboxBar 实时展示状态（颜色编码）；`label=harness9=1` 标记 + 启动时孤儿回收；`SANDBOX_ENABLED=false` 时关闭（默认启用，Docker 不可用自动降级为本地进程模式），关闭时行为与引入前完全一致（向后兼容）
- **Observability（OpenTelemetry 可观测性）**: `internal/observability/` 包；三条非侵入式接入路径——`OTELEngineObserver`（实现 `EngineObserver` 接口，管理 Interaction Span + Turn Span）+ `TracingProvider`（包装 `LLMProvider`，为每次 LLM 调用创建 Span + Token Metrics）+ `ObservabilityHook`（实现 `ToolHook`，为每次工具调用创建 Span）；Span 四层嵌套：`harness9.interaction → harness9.turn → harness9.llm_request / harness9.tool`；6 个关键 Metrics（LLM 延迟/Token 消耗/工具调用次数/工具执行耗时/Turn 总数）；`langfuse.trace.input`（trace 根节点 prompt）/ `langfuse.observation.input/output`（observation 层 LLM 消息与回复 + 工具参数与结果）/ `gen_ai.usage.*`（Token 用量，Langfuse 自动换算费用）；非法 UTF-8 字节自动净化，防止 OTLP 序列化失败；三种 Exporter：`noop`（默认零开销）/ `stdout`（开发调试）/ `otlp`（生产接入 Langfuse / Grafana / Jaeger）；通过环境变量 `OTEL_ENABLED` / `OTEL_EXPORTER_TYPE` / `OTEL_EXPORTER_OTLP_ENDPOINT` 驱动，默认关闭向后兼容
- **网页搜索与抓取**: `internal/tools/web_search.go` / `web_fetch.go` / `web_safety.go` / `web_content.go`；`web_search` 工具（DuckDuckGo HTML 端点 POST，无 API Key，20s 超时 + 10s dial 超时，`golang.org/x/net/html` DOM 解析，`decodeUDDG` 还原真实 URL）+ `web_fetch` 工具（HTTP GET，15s 超时，5 次重定向上限，`text/html` → `go-readability` 提取主内容 → `html-to-markdown` 转 Markdown，`text/*` → 原始文本，其他 → 不支持提示）；`isSafeURL` 共享 SSRF 安全门（scheme + userinfo + DNS 解析 + 9 个 IP 段检查（含 IPv6 ULA `fc00::/7` 和链路本地 `fe80::/10`）+ IPv4-mapped IPv6 规范化 + IPv6 loopback，DNS 失败 fail-closed，重定向链每跳复检）；`DefaultPromptBuilder` 实时注入 `当前日期：YYYY-MM-DD`，防止 LLM 因训练截止日期偏差产生陈旧搜索词；主 Agent + 所有 Sub-Agent 均可使用，零额外配置
- **Test & Eval（自动化测试与评估）**: `internal/evals/` 包；`ScriptedProvider`（确定性 LLM mock，按预设 Turn 序列返回回复，不发起真实 API 调用）+ `Assertion` 接口（Hard 断言：ToolCalled/ToolNotCalled/OutputContains/OutputExcludes/NoError/Error；Soft 断言：MaxTurns/MaxToolCalls，仅记警告不影响通过率）+ `EvalHarness`（`RunCase` 构建最小化隔离引擎 + `recordingHook` 记录工具调用轨迹 + `Suite` 批量运行）+ `SetupHermeticEnv`（清除所有 API Key，标准 Hermetic 隔离环境（密封测试，防止 eval 调用真实 API），本地与 CI 环境一致）+ `BuildReport`/`WriteJSON`/`WriteMarkdown`（JSON + Markdown 评估报告生成）；`internal/evals/dataset/` 黄金数据集（22 个用例：工具调用准确性 × 6 + Planning 完成率 × 4 + Context Engineering × 4 + Error Handling/Self-Healing × 3 + Memory 持久化 × 2 + Context Compaction × 3）；`.github/workflows/eval.yml` CI Quality Gate（PR 触发 hermetic eval，失败则阻断合并）
- **MCP 工具集成（Model Context Protocol）**: `internal/mcp/` 包；JSON-RPC 2.0 over stdio/HTTP；`Config`（`.mcp.json` 加载，file-not-found 静默返回空）+ `StdioTransport`（subprocess + NDJSON async reader goroutine + pending map ID 关联 + 三路 select ctx/done/response；`transport_proc_unix.go` 独立进程组 + SIGKILL 终止 npx 孤儿 node 子进程，`transport_proc_windows.go` stub 回退单进程 kill）+ `HTTPTransport`（无状态 POST）+ `Client`（initialize → notifications/initialized → tools/list → tools/call 握手与调用）+ `Manager`（并发 Start 30s per-server timeout，fail-soft，`ServerStatus` + `ToolDetails` TUI 通知链，`InjectTools` 闭包捕获避免循环变量 bug，`WithNotify` channel 回调；`Start` 始终返回 nil，失败状态通过 `Statuses()` 的 `StatusFailed` 暴露）+ `MCPToolAdapter`（实现 `BaseTool` 接口，`mcp__{server}__{tool}` 双下划线命名，对 Engine 完全透明）；TUI MCPBar（状态栏实时展示）+ `/mcp` 模态工具面板（工具列表，`e` 键 `tea.ExecProcess` 打开 `$EDITOR` 编辑 `.mcp.json`；鼠标滚轮 + ↑↓/jk 双支持）；`.mcp.json` 配置文件驱动，main.go 中异步启动不阻塞 TUI 渲染；`defer mcpMgr.Stop()` Session 级生命周期；`mcpPanelOverhead=4` 与 `contentH=m.height-4` 保持一致，确保面板撑满屏幕不遮挡对话区
- **AutoDev（自举开发闭环）**: `skills/autodev/SKILL.md`（`/autodev` AgentSkill，三阶段：需求澄清→Spec 生成与强制确认关卡→委派 dev sub-agent）+ `.harness9/agents/dev.md`（dev sub-agent 定义：读规范→探索→实现→`go build/test` 迭代循环（≤3次）→gofmt→commit→push→`gh pr create`）；git worktree（`.autodev/<slug>/`，代码隔离）+ Docker Sandbox（执行隔离，需 `SANDBOX_IMAGE=golang:1.25-bookworm`）；零新 Go 代码，完全由 Skill 文件 + Agent 定义文件驱动，复用现有 Skills 系统、Sub-Agent 系统、Sandbox 基础设施

### 参考框架

| 框架 | 来源 | GitHub |
|------|------|--------|
| DeepAgents | LangChain | https://github.com/langchain-ai/deepagents |
| OpenHarness | HKUDS | https://github.com/HKUDS/OpenHarness |
| OpenCode | Anomaly | https://github.com/anomalyco/opencode |
| OpenClaw | OpenClaw | https://github.com/openclaw/openclaw |
| HermesAgent | NousResearch | https://github.com/NousResearch/hermes-agent |
| Claude Agent SDK | Anthropic | https://code.claude.com/docs/en/agent-sdk/overview |
| OpenAI Agent SDK | OpenAI | https://github.com/openai/openai-agents-python |

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
| `github.com/charmbracelet/bubbletea` | v1.3.10 | Elm Architecture TUI 框架（AltScreen 模式） |
| `github.com/charmbracelet/lipgloss` | v1.1.1 | 终端样式与颜色（Header / StatusBar） |
| `github.com/charmbracelet/bubbles` | v1.0.0 | TUI 组件：spinner（工具进度）+ textinput（输入框） |
| `go.opentelemetry.io/otel` | v1.44.0 | OpenTelemetry 核心 API（Trace + Metric 接口） |
| `go.opentelemetry.io/otel/sdk` | v1.44.0 | OTEL SDK（TracerProvider / MeterProvider 实现） |
| `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp` | v1.44.0 | OTLP HTTP Trace 导出器（对接 Langfuse / Grafana / Jaeger） |
| `go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp` | v1.44.0 | OTLP HTTP Metric 导出器 |
| `go.opentelemetry.io/otel/exporters/stdout/stdouttrace` | v1.44.0 | Stdout Trace 导出器（本地调试） |
| `go.opentelemetry.io/otel/exporters/stdout/stdoutmetric` | v1.44.0 | Stdout Metric 导出器 |
| `github.com/go-shiori/go-readability` | latest | Mozilla Readability Go 移植，`web_fetch` HTML 主内容提取 |
| `github.com/JohannesKaufmann/html-to-markdown` | v1.6.0 | HTML → Markdown 转换，`web_fetch` 输出格式化 |

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
| 未导出类型/函数 | camelCase | `mainLoop`、`maxRetries`、`runLoop` |
| 接口名 | 以 `-er` 后缀为惯例，或不加后缀 | `Provider`、`Registry`、`BaseTool` |
| 常量 | PascalCase（导出）或 camelCase（未导出），**不使用全大写** | `RoleSystem`、`maxOutputLen` |
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

### 3.8 日志规范

**所有 `log.Print*` 调用必须通过 `internal/logfmt` 包格式化，禁止裸调用 `log.Printf` / `log.Println`。**

| 场景 | 调用方式 |
|------|---------|
| 通用单行日志 | `log.Print(logfmt.FormatMsg("prefix", msg))` |
| 工具启动 | `log.Print(logfmt.FormatToolStart("engine", turn, tc))` |
| 工具完成 | `log.Print(logfmt.FormatToolDone("engine", turn, tc, result, d))` |
| 循环启动/结束 | `log.Print(logfmt.FormatLoopStart(...))` / `FormatLoopEnd(...)` |

`prefix` 约定：`"main"`、`"engine"`、`"engine-stream"`、`"skills"` 等，与所在模块名对应。

---

## 4. 项目结构

```
harness9/
├── cmd/
│   ├── harness9/
│   │   ├── main.go                  # 程序入口：TUI（TTY）/ CLI（管道）；--version / --help flag；upgrade 子命令分发
│   │   ├── tui.go                   # TUI 核心：tuiModel struct、样式变量、Init、RunTUI
│   │   ├── tui_update.go            # Update 逻辑：事件处理、键盘、滚动、Tab 补全、Markdown 渲染、Thinking 块、Shell 执行
│   │   ├── tui_view.go              # View 渲染：renderConversation/ToolProgress/StatusBar/Input/Footer
│   │   ├── tui_banner.go            # WelcomeBanner：HARNESS9 ASCII Art + bannerContent()
│   │   ├── tui_test.go              # TUI Update 逻辑单元测试（含 thinking block 测试、Shell 执行测试、truncateUTF8 测试）
│   │   ├── cli.go                   # 交互式 CLI REPL 实现
│   │   └── upgrade.go               # 自动升级：GitHub Releases API + SHA256 校验 + 原子替换
│   ├── swebench/                    # SWE-bench Lite benchmark runner（评估真实 Issue 修复能力）
│   │   ├── main.go                  # CLI 入口：--dataset / --work-dir / --instance 等 flag
│   │   ├── dataset.go               # SWE-bench 数据集加载与实例解析（HuggingFace JSON）
│   │   ├── runner.go                # 运行器：构建隔离引擎 + 执行修复 + 收集轨迹
│   │   ├── prompt.go                # 实例 prompt 组装（注入 repo 结构 / 问题描述 / patch 提示）
│   │   └── report.go                # 评分与报告生成（apply_patch 校验 + 汇总输出）
│   └── starhistory/                 # GitHub Star History 图表生成（写入本地 SVG）
│       ├── main.go                  # CLI 入口：仓库名 / 输出路径 / 颜色参数
│       └── render.go                # SVG 渲染：时间序列曲线 + 标注 + 主题
├── internal/
│   ├── engine/                      # Agent 核心引擎 — 标准 ReAct 主循环
│   │   ├── agent_loop.go            # 共享 runLoop 主循环内核 + 阻塞式 Run
│   │   ├── agent_loop_test.go       # 主循环单元测试
│   │   ├── compact.go               # Compact 方法：TUI /compact 命令触发的手动强制压缩
│   │   ├── observer.go              # EngineObserver 接口 + noopObserver（可观测层无侵入接入点）
│   │   ├── permission.go            # PermissionMode 枚举（Default/AutoApprove/ReadOnly/BypassAll）+ WithPermissionMode
│   │   ├── stream.go                # 流式入口 RunStream + engine.Event 事件类型 + ToolResultData
│   │   └── stream_test.go           # 流式接口单元测试
│   ├── hooks/                       # 工具拦截器（Hooks）— 文件系统能力
│   │   ├── hook.go                  # ToolHook 接口 + HookRegistry（洋葱模型）+ ApprovalFunc context 工具
│   │   ├── decision.go              # HookDecision 结构体 + Allow/Deny/Ask 三种决策构造函数
│   │   ├── danger_hook.go           # DangerHook：19 条高危 bash 模式检测（高/中风险两级 Ask）
│   │   ├── offload.go               # OffloadHook：超大工具输出自动写入文件系统
│   │   ├── plan_writer.go           # FilePlanWriter：todo 计划持久化到 markdown
│   │   ├── hook_test.go             # HookRegistry 单元测试
│   │   ├── offload_test.go          # OffloadHook 单元测试
│   │   ├── plan_writer_test.go      # FilePlanWriter 单元测试
│   │   ├── subagent_progress.go     # SubAgentProgressFunc：子代理进度 context 注入/提取
│   │   └── subagent_progress_test.go # SubAgentProgressFunc 单元测试
│   ├── subagent/                    # Sub-Agent 模块 — 子代理委派系统
│   │   ├── definition.go            # SubAgentDefinition 结构体 + ResolveTools + Validate
│   │   ├── registry.go              # Registry：Register / Get / List（启动注册，运行期只读）
│   │   ├── frontmatter.go           # parseAgentFile：YAML frontmatter + 正文 → 定义
│   │   ├── loader.go                # Registry.LoadFromDir：扫描 .harness9/agents/*.md
│   │   ├── prompt.go                # promptBuilder：子代理 system prompt + Skills 预加载 + workDir 注入
│   │   ├── tracker.go               # TaskTracker：后台任务单一事实源（日志缓冲 + 结果注入）
│   │   ├── runner.go                # Runner：构建隔离子引擎 + RunStream + 桥接审批与进度
│   │   ├── task_tool.go             # TaskTool：主代理委派入口（task 工具，前台/后台）
│   │   └── *_test.go                # 各组件单元测试
│   ├── skills/                      # Agent Skills — Progressive Disclosure 技能系统
│   │   ├── skill.go                 # Skill 结构体 + parseFrontmatter（YAML frontmatter 解析）
│   │   ├── index.go                 # Index：已加载 skill 集合 + Summary/GetFullContent/Names
│   │   ├── loader.go                # LoadSkills：扫描 skills/<name>/SKILL.md → Index
│   │   ├── use_skill_tool.go        # UseSkillTool：use_skill 工具（按需加载 skill 全文）
│   │   └── skills_test.go           # 全组件单元测试（parseFrontmatter/Index/LoadSkills/UseSkillTool）
│   ├── planning/                    # Planning 模块 — Plan Mode + 任务列表
│   │   ├── mode.go                  # PlanMode 枚举（Default/Plan/AutoEdit）+ Next()/Label()
│   │   ├── mode_test.go             # PlanMode 单元测试
│   │   ├── plan_writer.go           # PlanWriter 接口（供 TodoWriteTool 依赖，避免循环导入）
│   │   ├── todo.go                  # TodoStore（线程安全）+ TodoItem/TodoStatus + FormatForInjection
│   │   └── todo_test.go             # TodoStore 单元测试
│   ├── memory/                      # Short-Term Memory — 会话历史持久化与上下文压缩
│   │   ├── session.go               # Session 接口 + SessionInfo 类型
│   │   ├── manager.go               # Manager：SQLite 连接持有者 + 会话 CRUD（NewSession/OpenSession/List/Delete）+ DB() 访问器
│   │   ├── sqlite_session.go        # SQLiteSession：WAL + 事务 + tool_calls JSON 序列化
│   │   ├── mem_session.go           # MemorySession：纯内存实现（测试用）
│   │   ├── compaction.go            # Compactor 接口 + TokenBudgetCompactor + SlidingWindowCompactor
│   │   ├── summarization.go         # SummarizationCompactor：LLM 摘要压缩 + Summarizer 接口 + 增量更新；MemoryExtractor 接口 + WithMemoryExtractor
│   │   ├── progressive_compactor.go # ProgressiveCompactor：按 token 占用比例分级压缩（Warn/Soft/Full/Emergency）+ RecordedCompactor
│   │   ├── anchor.go                # Anchor 类型 + ParseAnchorsAndSummary + MergeAnchors（五类结构化锚点系统）
│   │   ├── compaction_offloader.go  # CompactionOffloader：压缩期超大 tool_result 外存到文件系统
│   │   ├── record_store.go          # CompactionRecord + CompactionTier + FileRecordStore（JSONL 压缩审计持久化）
│   │   ├── token.go                 # EstimateTokens/EstimateToolTokens（字符数÷4 估算）+ FormatTokenCount
│   │   ├── sqlite_session_test.go   # SQLiteSession 集成测试
│   │   ├── mem_session_test.go      # MemorySession 单元测试
│   │   ├── manager_test.go          # Manager 单元测试
│   │   ├── compaction_test.go       # TokenBudgetCompactor + SlidingWindowCompactor 单元测试
│   │   └── summarization_test.go    # SummarizationCompactor 单元测试
│   ├── ltm/                         # Long-Term Memory — 跨会话长期记忆
│   │   ├── entry.go                 # Entry 结构体、Category 类型、Signature（SHA256 去重）、Expired
│   │   ├── store.go                 # Store：schema 迁移 + Add/Get/Search/Update/SoftDelete/List/PurgeExpired/StaleCandidates；ErrNotFound
│   │   ├── precis.go                # Precis：Regenerate/Read（MEMORY.md 物化视图）+ truncateUTF8
│   │   ├── extractor.go             # Extractor（实现 memory.MemoryExtractor）：LLM 压缩前事实提取 + Generator 接口
│   │   ├── provider.go              # Phase 3 接缝：Provider/Embedder/Consolidator 接口 + noopProvider
│   │   ├── entry_test.go
│   │   ├── store_test.go
│   │   ├── precis_test.go
│   │   ├── extractor_test.go
│   │   └── provider_test.go
│   ├── provider/                    # 大模型接口抽象与具体厂商 SDK 实现
│   │   ├── interface.go             # LLMProvider 接口定义（Generate + GenerateStream）
│   │   ├── openai.go                # OpenAI 兼容 API 适配器（支持 OpenRouter / Azure）；WithIncludeReasoning + extractReasoningContent
│   │   ├── anthropic.go             # Anthropic 兼容 API 适配器（Messages API）；WithThinkingBudget（extended thinking）
│   │   ├── model_limits.go          # 已知模型上下文窗口注册表 + GetModelLimits（剥离前缀，未知回退 256K）
│   │   ├── tool_call_accumulator.go # OpenAI/Anthropic 共享的流式工具调用累积器
│   │   ├── anthropic_thinking_test.go # WithThinkingBudget 单元测试（含 clamp 测试）
│   │   ├── openai_reasoning_test.go # WithIncludeReasoning + extractReasoningContent + auto-detect 测试
│   │   └── providertest/            # 测试基础设施（仅在 _test 编译单元中可见）
│   │       └── mock.go              # 确定性 mock provider（NewMock / NewMockWithCallback）
│   ├── schema/                      # 跨组件共享的核心数据类型
│   │   ├── message.go               # Message、Role、ToolCall、ToolResult、ToolDefinition
│   │   ├── stream.go                # StreamChunk、StreamChunkType（Provider 层流式类型）
│   │   ├── subagent.go              # SubAgentUpdate / SubAgentUpdateKind（子代理进度类型）
│   │   └── message_test.go          # JSON tag 契约测试（snake_case + omitempty，锁定序列化向后兼容）
│   ├── tools/                       # 工具注册表 + 内置工具 + 路径沙箱 + SSRF 防护
│   │   ├── base.go                  # BaseTool 接口定义（Name / Definition / Execute）
│   │   ├── registry.go              # 工具注册中心（Register / GetAvailableTools / Execute）
│   │   ├── safe_path.go             # 共享路径沙箱校验（防 Path Traversal 攻击）
│   │   ├── path_locker.go           # 路径级 RWMutex + 引用计数，并发文件操作保护
│   │   ├── bash.go                  # bash 工具（Shell 命令执行，YOLO 哲学）
│   │   ├── read_file.go             # read_file 工具（沙箱保护，offset/limit 分页，8192 字节默认上限）
│   │   ├── write_file.go            # write_file 工具（沙箱保护，Auto-Mkdir）
│   │   ├── edit_file.go             # edit_file 工具（多级模糊匹配文件编辑，沙箱保护）
│   │   ├── todo_write.go            # todo_write 工具（读写 TodoStore + 状态转换校验 + WithPlanWriter 注入）
│   │   ├── todo_write_test.go       # todo_write 工具单元测试（含状态校验测试）
│   │   ├── memory_write.go          # memory_write 工具（add/update[merge]/remove 三动作 + Precis 重建）
│   │   ├── memory_search.go         # memory_search 工具（FTS5 全文检索 + 命中强化）
│   │   ├── mcp_adapter.go           # MCPToolAdapter：MCP 工具包装为 BaseTool（mcp__{server}__{tool} 命名 + SanitizeMCPName）
│   │   ├── web_safety.go            # SSRF 防护（isSafeURL：scheme/userinfo/DNS/9 个 IP 段检查，含 IPv6 ULA/链路本地）
│   │   ├── web_safety_test.go       # 19 个用例（IP 段 / scheme / DNS fail-closed / IPv6 loopback / ULA / 链路本地）
│   │   ├── web_content.go           # HTML→Markdown 管线（go-readability + html-to-markdown + tokenizer fallback，UTF-8 安全截断）
│   │   ├── web_content_test.go      # 5 个用例（基础转换 / 截断 / fallback / 1MB 限制 / UTF-8 安全截断）
│   │   ├── web_fetch.go             # web_fetch 工具（HTTP GET + SSRF + Content-Type 分支）
│   │   ├── web_fetch_test.go        # 6 个用例（SSRF / HTML / text / unsupported / 空URL / 4xx）
│   │   ├── web_search.go            # web_search 工具（DuckDuckGo POST + DOM 解析 + decodeUDDG）
│   │   └── web_search_test.go       # 6 个用例（DDG 解析 / max_results / URL 解码 / Execute / 空结果）
│   ├── sandbox/                     # Sandbox — Docker 容器级执行环境
│   │   ├── config.go                # SandboxConfig（从环境变量读取）+ DefaultConfig
│   │   ├── environment.go           # Environment 接口（RunBash/ReadFile/WriteFile/ID/Close）
│   │   ├── local_environment.go     # LocalEnvironment：进程级，Sandbox 关闭时默认
│   │   ├── docker_environment.go    # DockerEnvironment：docker exec 路由 + bind mount 文件系统
│   │   ├── container.go             # Container 五状态机（Pending→Running→Stopping→Terminated/Failed）+ cmdRunner 可注入
│   │   ├── manager.go               # Manager：Create/Destroy/DestroyAll/ReapOrphans/ListAll；并发安全
│   │   └── *_test.go                # 各组件单元测试（container mock + manager 并发测试）
│   ├── observability/               # OpenTelemetry 可观测性 — Tracing + Metrics
│   │   ├── config.go                # Config 结构体 + ConfigFromEnv()（env var 驱动）
│   │   ├── attributes.go            # Span 名称 / Metric 名称 / 属性键常量
│   │   ├── setup.go                 # OTEL SDK 初始化（Providers + Setup + NewNoopProviders）
│   │   ├── observer.go              # OTELEngineObserver：Interaction Span + Turn Span
│   │   ├── provider.go              # TracingProvider：LLM Request Span + Token Metrics
│   │   ├── hook.go                  # ObservabilityHook：Tool Execution Span + Tool Metrics
│   │   ├── helpers.go               # truncateAttr / serializeMessages / serializeOutput（Span 属性净化与序列化）
│   │   └── *_test.go                # 各组件单元测试（noop tracer；helpers_test.go 为同包白盒测试）
│   ├── evals/                       # 自动化评估框架 — Test & Eval
│   │   ├── provider.go              # ScriptedProvider：确定性 LLM mock（按脚本序列返回回复）
│   │   ├── assertions.go            # Assertion 接口 + Case/Result 类型 + 8 种断言（Hard/Soft）
│   │   ├── harness.go               # RunCase / Suite / recordingHook / extractFinalOutput
│   │   ├── testenv.go               # SetupHermeticEnv：标准 Hermetic 隔离环境（清除 API Key，禁止真实 LLM 调用）
│   │   ├── report.go                # BuildReport / WriteJSON / WriteMarkdown（评估报告生成）
│   │   └── dataset/                 # 黄金数据集（go test 可直接运行，22 个用例）
│   │       ├── tool_calling_test.go # 工具调用准确性（6 用例：bash/read_file/write+read/L4 模糊匹配/并行工具/纯对话）
│   │       ├── planning_test.go     # Planning 完成率（4 用例：生成计划/不写文件/先规划后执行/只读探索）
│   │       ├── memory_test.go       # Memory 持久化（2 用例：memory_write/memory_search）
│   │       ├── context_test.go      # Context Engineering（4 用例：多步工具链/多轮对话/工具错误观察/停滞 nudge）
│   │       ├── error_handling_test.go # Error Handling（3 用例：工具失败降级/写入失败停止/MaxTurns 保护）
│   │       └── compaction_test.go   # Context Compaction（3 用例：锚点保留/offload 检索/渐进式分层压缩）
│   ├── mcp/                         # MCP Client 集成 — 外部工具服务器接入
│   │   ├── config.go                # ServerConfig + Config 类型 + LoadConfig（读 .mcp.json，file-not-found 静默返回空）
│   │   ├── transport.go             # Transport 接口 + StdioTransport（subprocess + NDJSON async reader goroutine + pending map + 三路 select）+ HTTPTransport
│   │   ├── transport_proc_unix.go   # setPgid / killProcessGroup POSIX 实现（进程组 SIGKILL，终止 npx 孤儿 node 子进程）
│   │   ├── transport_proc_windows.go # setPgid / killProcessGroup Windows stub（回退单进程 kill）
│   │   ├── client.go                # Client（initialize → notifications/initialized → tools/list → tools/call 握手与调用）
│   │   ├── manager.go               # Manager（并发连接、fail-soft、ServerStatus + ToolDetails、InjectTools 闭包、WithNotify channel 回调）
│   │   ├── manager_test.go          # Manager + mockTransport 单元测试
│   │   ├── client_test.go           # Client.Connect / CallTool 边界条件测试（cancelledMockTransport）
│   │   ├── config_test.go           # LoadConfig / ServerConfig.transportType 测试
│   │   └── transport_test.go        # newTransport / HTTPTransport / StdioTransport ctx 取消测试
│   ├── context/                     # Context Engineering — System Prompt 组装
│   │   └── builder.go               # DefaultPromptBuilder：基础 prompt + AGENTS.md + Skills 索引 + todo/offload/Sandbox/LTM 段落
│   ├── env/                         # 环境配置
│   │   ├── env.go                   # 零依赖 .env 文件加载器（系统变量优先）
│   │   └── env_test.go              # 配置加载单元测试
│   ├── permission/                  # 工具权限规则系统（JSON 配置驱动）
│   │   ├── rules.go                 # Rules：有序规则列表 + Evaluate/matchPattern（glob 匹配）+ LoadRules/SaveRules
│   │   └── hook.go                  # Hook：实现 hooks.ToolHook（NewHook 内存 / NewFileHook 每次调用重载配置文件）
│   ├── mission/                     # Mission Control 持久化领域（协调长期运行的 Agent 工作，不依赖单个会话）
│   │   ├── types.go                 # MissionStatus/TaskStatus 枚举 + Mission/Task/TaskAttempt/Artifact/Evidence 类型 + validTaskTransition
│   │   └── store.go                 # Store：SQLite 事实源（CreateMission/CreateTask/TransitionTask/AddArtifact/AddEvidence，含依赖就绪排队与不可变触发器）
│   └── logfmt/                      # 跨模块共享的块状日志格式化工具
│       ├── format.go                # 块状日志格式化（FormatMsg/ToolStart/LoopStart 等）
│       └── format_test.go           # 格式化函数单元测试
├── skills/                          # Agent Skills（与项目一起提交，用户可复制使用）
│   ├── autodev/
│   │   └── SKILL.md                 # /autodev AgentSkill：自举开发三阶段工作流
│   ├── debugging-guide/
│   │   └── SKILL.md                 # Go 调试指南
│   ├── go-coding-standards/
│   │   └── SKILL.md                 # Go 编码规范
│   └── architecture-overview/
│       └── SKILL.md                 # harness9 架构总览
├── docs/
│   ├── 核心功能/
│   │   ├── autodev.md               # AutoDev 自举开发技术方案
│   │   ├── tui.md                   # TUI 交互界面实现原理
│   │   ├── cli.md                   # CLI 使用指南
│   │   ├── agent-skills.md          # Agent Skills 设计原理
│   │   ├── agent-loop.md            # Agent Loop 核心实现原理
│   │   ├── tool-calling.md          # Tool Calling 工具调用系统详解
│   │   ├── context-engineering.md   # Context Engineering 技术方案（含 Short-Term Memory）
│   │   ├── planning.md              # Planning 模块：Plan Mode、TodoStore、执行自动化
│   │   ├── sub-agent.md             # Sub-Agent 系统：general-purpose、task 工具、前台/后台、@agent、TaskTracker
│   │   ├── file-system.md           # 文件系统能力：OffloadHook、FilePlanWriter、分页读取、GC
│   │   ├── shell-execution.md       # Shell 执行功能：! 前缀、异步机制、LLM 上下文注入、截断策略
│   │   ├── long-term-memory.md      # Long-Term Memory：跨会话记忆、存储 schema、MEMORY.md、三路触发、冲突/强化机制
│   │   ├── eval.md                  # 测试·评估·可观测体系：Test/Eval/Observability 完整体系、ScriptedProvider、黄金数据集、OTEL 链路追踪、Langfuse 接入
│   │   └── web_search.md            # 网页搜索与抓取：web_search/web_fetch 工具、SSRF 防护、HTML→Markdown 管线、DuckDuckGo 后端、当前日期注入
│   └── core-features-en/            # docs/核心功能/ 的英文镜像，文件名一一对应，官网英文 locale 唯一信息源
├── website/                          # 官网（VitePress，中英双语）
│   ├── index.md                     # 英文首页（默认 locale，根路径）
│   ├── blog/                        # 英文技术博客（根路径）
│   ├── zh/
│   │   ├── index.md                 # 中文首页（/zh/ locale）
│   │   └── blog/                    # 中文技术博客（/zh/ locale）
│   ├── public/blog/<slug>/images/    # 博客图片（中英文两个 locale 共用，仅一份，避免体积翻倍）
│   ├── scripts/sync-docs.mjs        # 把 docs/核心功能/ 与 docs/core-features-en/ 分别同步为 website/zh/docs/ 与 website/docs/
│   └── .vitepress/config.ts         # VitePress locales 配置（root = 英文，zh = 中文）
├── .env.example                     # 环境变量配置模板
├── go.mod                           # Go 模块定义
├── go.sum                           # 依赖锁定
├── AGENTS.md                        # 本文件 — 项目开发规范与上下文
├── CLAUDE.md -> AGENTS.md           # 符号链接，保持同步
├── LICENSE                          # MIT 许可证
├── README.md                        # 项目介绍与快速开始（英文，默认展示）
└── README.zh-CN.md                  # 项目介绍与快速开始（中文）
```

### 架构分层

```
┌─────────────────────────────────────────────────┐
│                    cmd/harness9                   │
│   main.go — 程序入口（TUI / CLI 自动检测）           │
└──────────────────────┬──────────────────────────┘
                       │
           ┌──────────▼──────────┐
           │    engine (编排层)    │
           │  Run / RunStream     │
           │  标准 ReAct          │
           │  工具调度 / 终止检测   │
           │  Session/Compactor  │
           └────┬────────┬────────┘
                │        │
           ┌────▼──┐  ┌──▼──────────────────┐  ┌──────────────┐
           │provid │  │  hooks (拦截层)       │  │   memory     │
           │ (模型) │  │  HookRegistry        │  │  (记忆层)    │
           │OpenAI │  │  OffloadHook         │  │ Session      │
           │Anthro │  │  FilePlanWriter       │  │ Manager      │
           └───────┘  └──────────┬───────────┘  │ Compactor    │
                                 │               └──────┬───────┘
                      ┌──────────▼───────────┐         │
                      │  tools (工具层)        │  ┌───────▼──────┐
                      │  Registry             │  │   SQLite     │
                      │  bash/read_file       │  │~/.harness9/  │
                      │  write_file/edit_file │  └──────────────┘
                      └──────────┬────────────┘
                                 │
                      ┌──────────▼───────────┐
                      │  schema (数据层)       │
                      │  Message / ToolCall   │
                      │  ToolResult / Usage   │
                      └──────────┬────────────┘
                                 │
                      ┌──────────▼───────────┐
                      │   env (配置层)         │
                      └──────────────────────┘
```

### 模块职责

| 模块 | 职责 | 状态 |
|------|------|:----:|
| **cmd/harness9** | 主入口：TTY 自动检测选择 TUI / CLI；`--help` / `--version` flag；`upgrade` 子命令；初始化 Memory Manager + SummarizationCompactor + Session + OffloadHook + FilePlanWriter + HookRegistry | ✅ |
| **tui** | 全屏 TUI（Bubbletea）：WelcomeBanner + 对话页双 Phase、Spinner 动词轮换、内置命令 Tab 补全 + Skills 补全、Markdown 渲染、会话管理、状态栏 token 用量实时展示（颜色告警）+ 压缩通知 + Plan Mode 色调 + 审查对话框 + autoExecuting 续跑；ToolResultData 携带引擎侧精确耗时；Thinking 块流式渲染（EventThinkingDelta → 深灰 │ 前缀行，flushPendingThinking 在工具/正文边界自动关闭）；Shell 执行模式（`!` 前缀 → dispatchShellCommand → tea.Cmd 异步 → shellResultMsg → pendingShellOutput 注入 LLM 上下文） | ✅ |
| **engine** | 标准 ReAct 主循环，阻塞 + 流式双模式，并发工具调度，Session/Compactor 集成，EventTokenUpdate / EventCompaction / EventToolResult（ToolResultData）/ EventThinkingDelta 事件，WithContextWindow 选项，PlanMode 工具过滤 + prompt 注入，TodoStore 跨会话持久化，WithMemoryReminder（每 N 轮向防御性副本注入长期记忆提示，不持久化），WithStallReminder（连续 N 轮无进展工具（edit_file/write_file）调用时注入一次停滞提示打断空转，防御性副本，不持久化）；行为护栏：loopGuard 四维熔断（MaxTurns/墙钟超时/Token 预算/重复死循环，仅在 Turn 边界裁决）+ 重复签名检测与升级（sha256(toolName+canonical args)，先定向提醒后硬终止 ReasonRepetitionLoop）+ Reminder 三源仲裁（重复>停滞>记忆，每轮至多一条）+ LoopState 显式状态机（idle→turn_start⇄(compacting→generating→tool_executing)→done|terminated，局部值无锁）+ EventTerminated/EventStateChange 流式事件 + 统一 terminate 受控出口（修复熔断路径丢失轨迹的缺陷） | ✅ |
| **hooks** | 文件系统能力：ToolHook 接口 + HookRegistry（洋葱模型）；OffloadHook（超大输出 offload 到 `~/.harness9/tool_results/`，fail-open）；FilePlanWriter（todo 计划持久化为 markdown，git 项目写入 workDir/.harness9/plans/） | ✅ |
| **planning** | PlanMode 枚举（Default/Plan/AutoEdit）、PlanWriter 接口（解耦 TodoWriteTool 与 FilePlanWriter）、TodoStore（线程安全，全量替换）、TodoItem 状态机、todo_write 工具（状态转换校验 + WithPlanWriter 注入） | ✅ |
| **subagent** | Sub-Agent 子代理委派：SubAgentDefinition（ResolveTools 白名单∩全集-黑名单-task）、Registry（编程式 + `.harness9/agents/*.md` 文件式定义）、Runner（构建隔离子引擎 + RunStream + 桥接审批与进度）、TaskTool（task 工具，前台/后台双模式）、TaskTracker（后台任务单一事实源）；内置 general-purpose 通用子代理（继承父全部可用工具与模型）；防递归 + 权限不扩权 + 上下文隔离 | ✅ |
| **skills** | Progressive Disclosure 技能系统：`Skill` 结构体（YAML frontmatter 解析）、`Index`（技能集合 + Summary/GetFullContent/Names）、`LoadSkills`（扫描 `skills/<name>/SKILL.md` → Index，目录不存在静默跳过）、`UseSkillTool`（`use_skill` 工具，LLM 按需按名称加载 skill 全文，避免启动时整体注入 context）；结构类型隐式满足 `tools.BaseTool` 接口，无循环依赖 | ✅ |
| **memory** | Context Engineering：Session 接口、Manager（SQLite CRUD + WithToolResultsDir + DeleteSession 级联 GC + DB() 访问器）、SQLiteSession（WAL + 事务）、SummarizationCompactor（默认，LLM 摘要压缩 + 增量更新 + 错误回退）、TokenBudgetCompactor（回退，80% 阈值 + 孤立工具对双向修复）、SlidingWindowCompactor（回退方案）、token 估算工具；MemoryExtractor 接口 + WithMemoryExtractor（压缩前提取钩子） | ✅ |
| **ltm** | Long-Term Memory：Store（`long_term_memories` 表 + standalone FTS5 `memories_fts`，复用 `state.db`，Add 签名去重 / Search FTS5 强化 / SoftDelete signature=NULL / List top-N / PurgeExpired / StaleCandidates）、Precis（MEMORY.md 物化视图，top-30 渲染 + 5KB 截断）、Extractor（LLM 压缩前事实提取，fail-open，实现 MemoryExtractor 接口）、Phase 3 接缝（Provider/Embedder/Consolidator + noopProvider） | ✅ |
| **context** | DefaultPromptBuilder：System Prompt 结构化组装（基础 prompt + AGENTS.md + Skills 索引 + todo 指引 + offload 检索指引 + 长期记忆精华），WithOffloadEnabled 注入分页检索说明，WithLongTermMemory 接收读取闭包、每轮 Build 时实时重读 MEMORY.md 精华注入（写入即下一轮可见）；**每次 Build() 注入 `当前日期：YYYY-MM-DD`**（`time.Now()` 实时生成，防止 Agent 因训练截止日期偏差产生陈旧搜索词） | ✅ |
| **provider** | LLM 统一接口 + OpenAI / Anthropic SDK 适配器 + 实际 token 用量提取（Usage 类型）+ 模型 context window 注册表；AnthropicProvider 支持 WithThinkingBudget（extended thinking，≥1024 clamp）；OpenAIProvider 支持 WithIncludeReasoning + OpenRouter 自动检测，流式中通过 extractReasoningContent 提取 reasoning_content / reasoning 字段 | ✅ |
| **schema** | 跨组件共享的核心数据类型（Message、ToolCall、Usage 等）；StreamChunk 定义 text_delta / thinking_delta / done / error 四种流式增量类型 | ✅ |
| **tools** | 工具注册表 + 内置工具（bash / read_file（offset/limit 分页）/ write_file / edit_file / todo_write / memory_write（add/update/remove + Precis 重建）/ memory_search（FTS5 检索 + 命中强化）/ web_search（DuckDuckGo，无 API Key）/ web_fetch（go-readability + html-to-markdown，Markdown 输出））+ 路径沙箱（safe_path.go）+ SSRF 防护（web_safety.go） | ✅ |
| **env** | 零依赖 `.env` 配置加载器（系统变量优先） | ✅ |
| **logfmt** | 跨模块共享的块状日志渲染（FormatMsg/ToolStart/LoopStart 等 11 个格式函数） | ✅ |
| **sandbox** | Docker 容器级 Sandbox：Environment 接口（LocalEnvironment 进程级 / DockerEnvironment 容器级）、Container 五状态机（Pending/Running/Stopping/Terminated/Failed）、Manager（Create/Destroy/DestroyAll/ReapOrphans/ListAll，并发安全）、工具透明路由（bash via docker exec，文件 via bind mount）、安全加固（cap-drop all + security-opt no-new-privileges + pids-limit + tmpfs）、Agent 级隔离（主 Agent + 每个 Sub-Agent 独立容器）、TUI SandboxBar、孤儿容器回收、向后兼容（默认启用，`SANDBOX_ENABLED=false` 才关闭，行为与引入前完全一致） | ✅ |
| **observability** | OpenTelemetry 可观测层：`Config`/`Setup`（noop/stdout/otlp 三种 Exporter）、`OTELEngineObserver`（Interaction + Turn Span）、`TracingProvider`（LLM Request Span + Token Metrics）、`ObservabilityHook`（Tool Execution Span + Tool Metrics）；默认 noop 零开销，`OTEL_ENABLED=true` 激活 | ✅ |
| **evals** | 自动化评估框架：`ScriptedProvider`（确定性 mock）、`Assertion`（Hard/Soft 断言，8 种实现）、`RunCase`/`Suite`（最小化隔离引擎 + `recordingHook`）、`SetupHermeticEnv`（Hermetic CI 隔离）、`BuildReport`/`WriteJSON`/`WriteMarkdown`（评估报告）；`dataset/` 黄金数据集 22 用例（tool_calling/planning/context/error_handling/memory/compaction）；`.github/workflows/eval.yml` Quality Gate | ✅ |
| **permission** | 工具权限规则系统（JSON 配置驱动）：`Rules`（有序规则列表，glob 模式匹配，无匹配默认 ask）、`LoadRules`/`SaveRules`（配置文件重载，TUI "总是允许"动态生效）、`Hook`（实现 hooks.ToolHook，`NewHook` 内存 / `NewFileHook` 每次调用重载） | ✅ |
| **mission** | Mission Control 持久化领域：`MissionStatus`/`TaskStatus` 枚举 + `Mission`/`Task`/`TaskAttempt`/`Artifact`/`Evidence` 类型 + `validTaskTransition` 状态机校验；`Store`（SQLite 事实源：CreateMission/CreateTask/TransitionTask/AddArtifact/AddEvidence，依赖就绪自动排队 + evidence/artifact 不可变触发器 + workspace lease 唯一索引） | ✅ |
| **mcp** | MCP Client 集成：`Config`（`.mcp.json` 加载，file-not-found 静默返回空）、`StdioTransport`（subprocess + NDJSON async reader goroutine + pending map ID 关联 + 三路 select）、`HTTPTransport`（无状态 POST）、`Client`（initialize/notifications-initialized/tools-list/tools-call）、`Manager`（并发 Start 30s per-server timeout fail-soft、`ServerStatus`+`ToolDetails` TUI 通知、`InjectTools` 无循环变量 bug 闭包捕获、`WithNotify` channel 回调）；`MCPToolAdapter` 实现 `BaseTool` 接口、`mcp__{server}__{tool}` 双下划线命名、对 Engine 完全透明；TUI MCPBar + `/mcp` 模态面板（`e` 键 `tea.ExecProcess` 编辑配置） | ✅ |
| **autodev** | 自举开发闭环：`skills/autodev/SKILL.md`（`/autodev` AgentSkill，三阶段工作流：需求澄清→Spec 强制确认→委派）+ `.harness9/agents/dev.md`（dev sub-agent：读规范→探索→实现→go build/test 循环≤3次→gofmt→commit→push→gh pr create）；git worktree（`.autodev/<slug>/`）+ Docker Sandbox（`SANDBOX_IMAGE=golang:1.25-bookworm`）；零新 Go 代码，复用 Skills/Sub-Agent/Sandbox 基础设施 | ✅ |
| **provider/providertest** | 测试基础设施（mock provider），不进入生产二进制 | ✅ |

> **Roadmap（后续方向）**：
> - **短期记忆**：FTS5 全文会话搜索（P3，跨会话检索历史对话消息，区别于 LTM 对记忆条目的检索）。
> - **Long-Term Memory Phase 3**：向量嵌入语义检索（`Embedder`，接入 Ollama / OpenAI Embeddings）、Dreaming 巩固（`Consolidator`，cron 批量晋升）、外部记忆提供者（`Provider`，接入 Mem0 / Honcho）、基于 `StaleCandidates` 的陈旧记忆自动清理。

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

# 启动（TTY 自动进入 TUI 模式，管道/CI 环境退回 CLI REPL）
go run ./cmd/harness9
```

> `engine.Run`（阻塞模式）和 `engine.RunStream`（流式模式）作为内部 API 供 TUI/CLI 调用。

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

- **隔离开发（强制）**：每次准备修改项目文件前，必须基于当前任务新建独立的 Git worktree 和对应分支；所有代码、文档、配置及测试修改均在该 worktree 内完成，禁止直接在主工作目录修改。
- 主分支：`master`
- 功能分支命名：`feature/<描述>`、`fix/<描述>`、`refactor/<描述>`
- Commit 消息：中文描述，简洁明确，聚焦"为什么"而非"做了什么"
- 所有代码提交前必须通过 `go test ./...` 和 `gofmt -l .` 检查

### 5.8 Feature 完成后的测试与评估规范

**每完成一个大的 feature 开发后，必须同步补充测试和评估用例，这是 Definition of Done 的一部分。**

#### 单元测试（`*_test.go`）

- 新增的所有导出函数/方法必须有对应单元测试
- 覆盖正常路径、错误路径和边界条件（如 nil 输入、空切片、超长字符串）
- 测试文件与实现文件同目录，命名为 `xxx_test.go`
- 使用标准库 `testing` 包，表驱动优先，不引入第三方断言库

#### Eval 黄金数据集（`internal/evals/dataset/`）

每个涉及 **Agent 行为**的新功能必须在 `dataset/` 下新增或扩充 eval 用例：

| 功能维度 | 对应 dataset 文件 | 核心验证点 |
|---------|----------------|-----------|
| Tool Calling | `tool_calling_test.go` | 工具被调用、工具不被调用、多工具顺序/并行调用 |
| Planning | `planning_test.go` | todo_write 生成计划、Plan Mode 不写文件、先规划后执行 |
| Memory | `memory_test.go` | memory_write/memory_search 工具调用 |
| Context Engineering | `context_test.go` | 多轮工具链、工具结果被观察后调整行为 |
| Error Handling | `error_handling_test.go` | 工具失败后 self-healing、MaxTurns 受控终止 |
| Context Compaction | `compaction_test.go` | 压缩后锚点保留、offload 检索、渐进式分层压缩 |

**编写用例的强制要求**

```go
func TestMyFeature(t *testing.T) {
    evals.SetupHermeticEnv(t)  // 必须首行调用：隔离 API Key，禁止真实 LLM 调用

    c := &evals.Case{
        ID:       "feature_name/case_name",
        Category: "feature_name",
        Prompt:   "...",
        Provider: evals.NewScriptedProvider(/* 脚本化行为 */),
        Assertions: []evals.Assertion{
            // 必须：验证工具调用行为
            &evals.ToolCalledAssertion{ToolName: "xxx"},
            // 必须：RunError 断言（正常路径用 NoError，异常路径用 Error）
            &evals.NoErrorAssertion{},
            // 可选：效率告警（Soft，不影响通过率）
            &evals.MaxTurnsAssertion{Max: N},
        },
    }
    // ...
}
```

- 每个功能至少覆盖**正向用例**（功能正常工作）和**反向用例**（约束被正确执行）
- `ScriptedProvider` 脚本序列需要真实反映 Agent 在该场景下的预期行为路径
- 用例注释必须说明测试意图和验证的核心不变量

#### Eval 通过要求

提交前必须本地验证：

```bash
# 运行全量 eval（包含黄金数据集）
go test ./internal/evals/... ./internal/evals/dataset/... -v

# CI 门控（PR 触发时自动运行）
# 见 .github/workflows/eval.yml —— eval 失败则阻断合并
```

当前黄金数据集：22 个用例，覆盖 tool_calling（6）/ planning（4）/ memory（2）/ context（4）/ error_handling（3）/ compaction（3）。新 feature 只能**增加**用例，不能删除或降低覆盖率。

### 5.9 文档双语规范

**核心功能文档必须同时维护中英文双语版本。**

| 中文路径 | 英文路径 | 说明 |
|---------|---------|------|
| `docs/核心功能/<name>.md` | `docs/core-features-en/<name>.md` | 文件名一一对应 |

**强制要求：**
- 在 `docs/核心功能/` 下新增或修改任何 `.md` 文件时，**必须同步创建或更新** `docs/core-features-en/` 下的同名英文版本
- 英文版本不是机器翻译，而是独立编写的技术文档，内容与中文版对齐但符合英文技术写作惯例
- `website/scripts/sync-docs.mjs` 依赖两个目录的文件名一一对应关系，缺失英文版本会导致官网英文 locale 缺少该页面
- 这是 Definition of Done 的一部分：未生成英文文档的功能不算完成

**自动化保障：** 执行 `/sync-docs` 命令自动扫描并补全缺失或过期的英文文档（详见 `.opencode/commands/sync-docs.md`）。

---

## 6. 特殊约束

### 6.1 Provider 实现约束

#### Anthropic Messages API — user/assistant 严格交替
Anthropic Messages API **禁止连续 assistant 消息**，也禁止多条 system 消息。项目通过以下机制保证兼容性：

- System Prompt 仅在初始化 contextHistory 时注入一次（`RoleSystem` 消息）
- 每个 Turn 只产生一条 assistant 消息，Observation（工具结果）以 user 角色注入

Provider 实现者需注意：`convertMessages()` 方法应负责将 `schema.Message` 的 `role` 正确映射为厂商 API 的消息角色格式。

#### 工具列表传递
- 每次 LLM 调用均传递完整工具列表（`availableTools`）
- Provider 实现者需正确处理空工具列表（`len(tools) == 0`）与非空工具列表两种情况

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
- 日志输出截断至 512 字节（`logfmt.MaxOutputLen`，UTF-8 安全截断）
- `read_file` 工具截断至 8192 字节
- `bash` 工具输出截断至 16000 字节（保留首尾）
- 截断时应在返回文本末尾添加明确的截断标记

### 6.3 引擎约束

#### 三重终止保障
Agent 循环通过以下三种机制确保不会无限运行：

1. **自然终止**: 模型不再发起 ToolCall（`len(responseMsg.ToolCalls) == 0`）
2. **MaxTurns**: 超过最大 Turn 数（默认 500，可通过 `WithMaxTurns` 配置）
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
