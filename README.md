# harness9

**Local-First · 轻量级 · 功能完备 · 生产可用的通用 Agent 框架**

---

![harness9 欢迎界面](welcome.png)

![harness9 对话界面](quickstart.png)

---

## 为什么选择 harness9？

大多数 Agent 框架要么过于臃肿（满屏抽象层、数百个依赖），要么过于简陋（仅能跑个 demo）。harness9 走中间路线：


| 原则       | 说明                           |
| -------- | ---------------------------- |
| **Local-First** | 数据全部存储在本机（SQLite、tool_results、plans），工具在本地 Docker 容器内执行，无云端依赖，代码不离机 |
| **简洁**   | 最小化抽象层，代码直白易读，极少的直接依赖        |
| **完备**   | 覆盖 Agent 运行所需的全部核心模块         |
| **生产可用** | 错误恢复、上下文管理、超时控制、并发工具执行等生产级特性 |


---

## 快速开始

```bash
# 安装
curl -fsSL https://raw.githubusercontent.com/ZhangShenao/harness9/master/scripts/install.sh | bash

# 配置 API Key
export OPENAI_API_KEY="sk-..."

# 进入你的项目目录并启动
cd /your/project && harness9

# 查看所有 CLI 参数与说明
harness9 --help

# 查看版本号
harness9 --version
```

> 完整安装选项、Anthropic/OpenRouter 配置、AGENTS.md 设置和常见问题，见 [快速启动指南](docs/核心功能/quick_start.md)。

---

## 核心特性

### 全屏 TUI

在 TTY 中直接运行，自动进入全屏 TUI：欢迎页 → 对话页双 Phase 切换，流式输出逐 token 追加，工具执行期间实时 Spinner + 耗时计数。

- `Tab` 键补全命令和 Skill 名称
- `Ctrl-C` 中断 Agent，再按一次退出
- 非 TTY 环境（管道、CI）自动退回 CLI REPL 模式

详见 [TUI 交互界面实现原理](docs/核心功能/tui.md)。

### Shell 执行（`!` 前缀）

在对话框中直接运行 Bash 命令，无需切换终端：

```
› !git log --oneline -5
$ git log --oneline -5
a1b2c3d feat: add shell execution
...
  ✓ 完成 — 12ms
```

输入 `!` 时 TUI 自动切换 Shell 模式（状态栏变深绿、输入区显示 `[SHELL] $` 徽章）。命令输出追加到对话流，并在下次向 LLM 发送消息时自动注入上下文，Agent 可直接引用命令结果进行推理。

- 异步执行，不阻塞 TUI，默认 30s 超时
- vim/ssh 等交互式命令自动拦截，提示在独立终端运行
- `Esc` 退出 Shell 模式，`Enter` 执行命令

详见 [Shell 执行功能技术方案](docs/核心功能/shell-execution.md)。

### Context Engineering（上下文管理）

对话历史自动持久化到 SQLite（`~/.harness9/sessions.db`），进程重启后可通过 `/resume` 恢复。

```
ctx: 45.2K/128K (35%)  ← 绿色：正常
ctx: 92.1K/128K (72%)  ← 黄色：警告，即将压缩
```

**LLM 摘要压缩**（默认）：上下文超过 80% 时自动调用 LLM 生成结构化摘要，保留关键语义后继续会话，远优于简单截断；Provider 不可用时自动回退到 `TokenBudgetCompactor`。

```
⚡ 上下文已压缩 — 12.5K → 6.2K tokens（45 → 22 条消息）
```

会话命令：`/new` 开启新会话，`/resume` 恢复历史。

详见 [Context Engineering 技术方案](docs/核心功能/context-engineering.md)。

### Long-Term Memory（跨会话长期记忆）

短期记忆只在单次会话内有效。**Long-Term Memory** 让 Agent 跨会话积累知识、记住用户偏好、复用历史决策——记忆持久化到 SQLite（`long_term_memories` 表 + FTS5 全文索引，复用 `state.db`，零新增依赖）。

```
› 记住：我偏好简洁的中文回复，测试用标准库 testing
  ✓ memory_write({"action":"add", ...}) — 0s

# 下一次会话，无需重新说明，Agent 已知晓你的偏好
```

- **MEMORY.md 物化视图**：由 top-N 高价值条目（按 importance）自动渲染为有界文件（≤5KB），实时注入 System Prompt——天然规避「token bomb」，写入即下一轮可见
- **三路触发**：① 显式 `memory_write`（add/update/remove）/ `memory_search`（FTS5 按需检索）工具；② 上下文压缩前由 LLM 自动提取持久事实（fail-open）；③ 每 N 轮注入记忆提示 nudge（防御性副本，不持久化）
- **遗忘与去重**：SHA256 内容签名去重、TTL 过期回收、软删除（释放唯一槽位）、检索命中强化（`use_count`/`last_used_at`）、陈旧记忆识别
- **唯一事实源**：SQLite 为权威存储，MEMORY.md 为其物化视图，避免双源漂移

详见 [Long-Term Memory 实现原理](docs/核心功能/long-term-memory.md)。

### Agent Skills（按需加载的领域知识）

在 `skills/<name>/SKILL.md` 下放置领域知识，Agent 按需加载，System Prompt 始终精简：

```
skills/
├── refactor-guide/SKILL.md    # 重构规范
└── testing-standards/SKILL.md # 测试标准
```

详见 [Agent Skills 设计原理](docs/核心功能/agent-skills.md)。

### Human-in-the-Loop 权限控制

Agent 执行工具时，内置规则引擎自动评估风险等级，只有真正需要人类判断的操作才会暂停并弹出审批对话框：

```
╭─────────────────────────────────────────────────╮
│  ⚠  工具审批请求 [高风险]                         │
│                                                 │
│  工具：bash                                     │
│  原因：强制递归删除文件/目录                      │
│                                                 │
│  ▶ [1] 允许（仅本次）                           │
│    [2] 允许（本会话不再提示）                     │
│    [3] 总是允许（写入白名单）                     │
│    [4] 拒绝                                     │
│    [5] 拒绝并提供反馈...                         │
╰─────────────────────────────────────────────────╯
```

- **三级决策**：`allow`（放行）/ `deny`（拒绝）/ `ask`（弹框审批），通过洋葱模型 Hook 链依次评估
- **内置高危拦截**：`DangerHook` 自动识别 `rm -rf`、`curl | bash`、`sudo`、`dd if=` 等 19 条高危模式
- **JSON 白名单配置**：`.harness9/settings.json` 支持 glob 语法规则，选择「总是允许」后立即生效无需重启
- **敏感路径硬保护**：`~/.ssh`、`~/.aws`、`~/.kube`、`~/.gnupg` 等路径无论任何配置均拒绝访问
- **非交互兼容**：管道/CI 模式下 `ask` 决策自动视为 Allow，不破坏无人值守流程

详见 [Human-in-the-Loop 权限控制](docs/核心功能/human-in-the-loop.md)。

### Planning 模块（先规划、再执行）

通过 `Shift+Tab` 切换到 Plan Mode（状态栏显示 `[PLAN]`，色调切换为琥珀黄）。

```
用户：帮我写一个 Go Web API

[PLAN]  分析请求 → read_file/bash 只读探索
        → todo_write 生成实现计划
        → 文字简述后停止

        ╭──────────────────────────────────────╮
        │  Plan Mode 完成 — 选择下一步操作      │
        │  [1] 批准并自动执行                  │
        │  [2] 批准并逐步确认编辑               │
        │  [3] 继续修改计划                    │
        │  [4] 取消                            │
        ╰──────────────────────────────────────╯

批准后 Agent 按清单逐项执行，todo 快照实时追加在对话流中：

  ✓ todo_write({...}) — 0s
  ☰  Tasks  ·  3/11  ·  1 active
   1.  ✔  创建目录结构
   2.  ✔  初始化 go.mod
   3.  ▶  实现 main.go
```

- **工具层权限控制**：Plan Mode 下 `write_file`、`edit_file` 被从工具列表移除，无论 prompt 如何，LLM 根本看不到写工具
- **作弊防护**：`todo_write` 校验状态转换，`pending → completed` 直跳被拒绝，LLM 必须经过 `in_progress` 才能完成条目
- **停滞检测**：连续 3 次 `EventDone` 无进度后停止自动执行，提示手动干预

详见 [Planning 模块实现原理](docs/核心功能/planning.md)。

### 文件系统能力（Context Offload + Plan 持久化）

工具输出超过阈值（默认 10,000 字符）时，**OffloadHook** 自动将完整内容写入文件，context 中仅保留摘要引用和预览，防止单次输出爆炸 context 窗口：

```
  ✓ bash(grep -r "TODO" .) — 1.2s
[输出已保存至 ~/.harness9/tool_results/{sessionID}/{id}.txt，共 847 行 / 32416 字节。
预览（前 20 行）：
...
```

LLM 可通过 `read_file` 的 `offset/limit` 参数分页检索完整内容：

```json
read_file({"path": "~/.harness9/tool_results/.../id.txt", "offset": 4096, "limit": 4096})
```

**FilePlanWriter** 在每次 `todo_write` 写入后将执行计划同步输出为 markdown 文件：

```markdown
# 执行计划
session: abc12345
updated: 2026-05-22T15:30:00+08:00

## 任务列表
- [x] 创建目录结构
- [>] 实现 main.go
- [ ] 编写测试
```

- **Git 项目**写入 `{workDir}/.harness9/plans/`，纳入版本控制
- 删除会话时自动级联清理 offload 文件，无磁盘残留

详见 [文件系统能力技术方案](docs/核心功能/file-system.md)。

### Sub-Agent（子代理委派）

主代理可通过 `task` 工具，把**边界清晰的子任务**委派给运行在独立上下文、受限工具集的专门子代理执行——子代理只回传最终结论，不让冗长的中间过程污染主上下文。

harness9 内置一个 **`general-purpose`（通用）子代理**，设计对标 [Claude Code](https://code.claude.com/docs/en/sub-agents#general-purpose) 与 [DeepAgents](https://docs.langchain.com/oss/python/deepagents/subagents#the-general-purpose-subagent) 的同名能力：继承父代理可用的全部工具与模型，用于需要兼顾探索与修改、复杂推理或多步依赖的任务，是「没有更专门子代理时」的兜底委派目标。

```
@general-purpose 调查 internal/tools/bash.go 的超时处理逻辑并总结实现要点
  [general-purpose] 子代理启动…
  [general-purpose] ▸ read_file ✓
  [general-purpose] ✓ 完成
```

- **内置 general-purpose**：`Tools`/`Model` 留空即继承父代理可用的全部工具与模型，开箱即用
- **文件式扩展**：在 `.harness9/agents/*.md` 用 YAML frontmatter 定义专门子代理（白名单/黑名单工具、模型覆盖、预加载 Skills）
- **前台 / 后台双模式**：前台阻塞返回结果，后台异步执行 + `/tasks` 面板查看 + 下次对话注入
- **`@agent` 直跑**：输入 `@<agent> <task>` 绕过主 LLM 决策，直接前台调用（`Tab` 补全子代理名）
- **安全隔离**：禁止递归（子代理无 `task` 工具）、权限只能更严不能扩权、上下文完全隔离

详见 [Sub-Agent 系统实现原理](docs/核心功能/sub-agent.md)。

### Observability（OpenTelemetry 链路追踪）

通过三条非侵入式接入路径，将 OTEL Span 和 Metrics 无缝嵌入 Agent 运行全链路——引擎层、LLM 调用层、工具执行层各自独立，不改动核心代码：

```
harness9.interaction          ← 一次完整 Agent 运行（含 sessionID）
  └── harness9.turn           ← 单个 ReAct Turn
        ├── harness9.llm_request  ← LLM API 调用（含 token 数）
        └── harness9.tool         ← 工具执行（含工具名、成功/失败）
```

```bash
# 接入 Jaeger（本地开发）
docker run -p 16686:16686 -p 4318:4318 jaegertracing/all-in-one
export OTEL_ENABLED=true OTEL_EXPORTER_TYPE=otlp
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
harness9
# 打开 http://localhost:16686 查看完整调用链

# 接入 Langfuse（生产）
export OTEL_ENABLED=true OTEL_EXPORTER_TYPE=otlp
export OTEL_EXPORTER_OTLP_ENDPOINT=https://cloud.langfuse.com/api/public/otel
harness9
```

- **默认零开销**：`OTEL_ENABLED` 不设置时使用 noop 实现，无任何性能影响
- **6 个关键 Metrics**：LLM 请求延迟、Token 消耗（Input/Output）、工具调用次数（by name + status）、工具执行耗时、Agent Turn 总数
- **三种 Exporter**：`noop`（默认）/ `stdout`（本地调试）/ `otlp`（生产接入 Langfuse、Grafana、Jaeger）
- **完整 Input/Output 上报**：每次 LLM 调用完整的消息列表和回复内容都记录到 Langfuse，工具调用的参数和结果也完整可追溯
- **自动 UTF-8 净化**：工具输出中的 binary 数据自动清理，不因非法字节导致 trace 丢失

详见 [测试·评估·可观测体系](docs/核心功能/eval.md)。

### Test & Eval（自动化测试与评估）

内置评估框架，用 `ScriptedProvider` 把 LLM 行为脚本化，配合 `Assertion` 断言体系验证 Agent 的工具调用轨迹——无需真实 API，CI 中 hermetic 运行：

```go
// 定义一个确定性 eval 用例
c := &evals.Case{
    ID:     "tool_calling/bash_basic",
    Prompt: "运行 echo hello",
    Provider: evals.NewScriptedProvider(
        evals.ScriptedTurn{ToolCalls: []schema.ToolCall{
            evals.MakeToolCall("tc1", "bash", `{"command":"echo hello"}`),
        }},
        evals.ScriptedTurn{Text: "命令已执行，输出 hello。"},
    ),
    Assertions: []evals.Assertion{
        &evals.ToolCalledAssertion{ToolName: "bash"},
        &evals.NoErrorAssertion{},
        &evals.MaxTurnsAssertion{Max: 3}, // soft：仅记警告
    },
}
result := evals.RunCase(context.Background(), c)
```

```bash
# 运行黄金数据集（16 个内置用例，无需 API Key）
go test ./internal/evals/dataset/... -v
```

- **确定性测试**：`ScriptedProvider` 按脚本序列返回回复，相同输入永远得到相同结果
- **行为轨迹验证**：`recordingHook` 记录所有工具调用，Hard/Soft 双层断言覆盖正确性与效率
- **Hermetic CI 隔离**：`SetupHermeticEnv` 清除所有 API Key，防止 eval 意外调用付费服务
- **CI Quality Gate**：PR 触发自动评估（`.github/workflows/eval.yml`），eval 失败则阻断合并
- **黄金数据集**：内置 16 个用例（工具调用准确性 × 4、Planning 完成率 × 4、Context Engineering × 3、Error Handling/Self-Healing × 3、Memory 持久化 × 2）

详见 [测试·评估·可观测体系](docs/核心功能/eval.md)。

### Sandbox（Docker 容器级隔离）

harness9 默认在本地 Docker 容器内执行所有工具调用（需本地安装并运行 Docker）。Docker 不可用时自动降级为本地进程模式，Agent 行为不变。

```
[Sandbox] 3a2f (main) Running │ 7b1c (sub-1) Running
```

- **OS 级隔离**：独立进程空间、Capability 最小化（`--cap-drop all`）、防 fork bomb（`--pids-limit 256`）
- **透明路由**：`bash` 命令通过 `docker exec` 进容器，文件工具通过 bind mount 共享 workDir——Agent 行为不变，无需修改 prompt
- **Agent 级隔离**：主 Agent 和每个 Sub-Agent 各自拥有独立 Sandbox 容器，互不影响
- **TUI SandboxBar**：StatusBar 下方实时展示所有活跃 Sandbox 的 ID 和状态（绿=Running、黄=Pending、红=Failed）
- **孤儿回收**：以 `label=harness9=1` 标记所有管理的容器，进程崩溃后下次启动自动清理残留容器

```bash
# Sandbox 默认开启，直接启动即可
harness9

# 如需关闭 Sandbox（不使用 Docker 容器）
echo "SANDBOX_ENABLED=false" >> .env
```

详见 [Sandbox 沙箱系统](docs/核心功能/sandbox.md)。

### AutoDev（`/autodev` 自举开发）

在 harness9 TUI 中输入 `/autodev <功能描述>`，主 Agent 通过对话澄清需求、生成 Feature Spec，用户确认后自动委派 dev sub-agent 完成编码、测试、PR 创建——全程无需人工写代码：

```
› /autodev 实现一个 token_count 工具

[主 Agent 澄清需求（2-3 轮对话）]
> 这个工具需要支持多语言吗？

用户: 是的，中文按字符计数，英文按 word 计数

[主 Agent 生成 Spec 并等待确认]
## Feature Spec: Add token_count Tool
...

用户: 确认

[Dev Sub-Agent 启动，在 git worktree + Docker Sandbox 中工作]
  ✓ go build ./...
  ✓ go test ./... — PASS（第 1 次）
  ✓ git push origin HEAD
  ✓ gh pr create

✓ PR 已创建：https://github.com/.../pull/42
```

- **三阶段流程**：需求澄清（对话）→ Spec 确认（强制人工审批）→ dev sub-agent 自动实现
- **双重隔离**：git worktree（代码隔离）+ Docker Sandbox（执行隔离，需 Go 镜像）
- **自愈迭代**：dev sub-agent 遇到测试失败后自主分析错误、修复代码、最多重试 3 次
- **零额外配置**：复用现有 Skills 系统和 Sub-Agent 系统，无新 Go 代码

前置条件：`gh` CLI 已登录；如需 Docker 隔离，在 `.env` 中设置 `SANDBOX_IMAGE=golang:1.25-bookworm`

详见 [AutoDev 自举开发技术方案](docs/核心功能/autodev.md)。

### MCP 工具集成（Model Context Protocol）

harness9 支持通过 MCP 协议连接外部工具服务器，将第三方工具无缝注入 Agent 工具注册表：

```
[MCP] context7 ● 2 tools
```

在项目根目录创建 `.mcp.json` 即可接入任意 MCP Server：

```json
{
  "mcpServers": {
    "context7": {
      "command": "npx",
      "args": ["-y", "@upstash/context7-mcp"]
    }
  }
}
```

- **零侵入集成**：MCP 工具以 `mcp__{server}__{tool}` 格式注册到统一工具注册表，对 Engine 完全透明，自动获得并发执行、超时控制、错误回传等所有现有机制
- **异步冷启动**：MCP Server 在 TUI 启动后于后台异步连接，不阻塞用户交互；`npx` 类需要下载的 Server 也不会卡住启动流程
- **双传输支持**：`stdio`（子进程 stdin/stdout NDJSON，async reader goroutine + pending map ID 关联）+ `http`（无状态 POST）
- **TUI MCPBar**：StatusBar 下方实时展示所有 MCP Server 连接状态与工具数（绿=已连接、黄=连接中、红=失败）
- **`/mcp` 命令**：Tab 补全可用，打开模态工具面板查看所有已连接工具；按 `e` 用 `$EDITOR` 或系统关联程序打开 `.mcp.json` 编辑配置

详见 [MCP 工具集成技术方案](docs/核心功能/mcp.md)。

### 网页搜索与抓取（`web_search` + `web_fetch`）

Agent 可直接搜索互联网并抓取页面内容，无需任何 API Key 或外部账号：

```
› 搜索 Go 1.25 的新特性并总结

  ✓ web_search("Go 1.25 release notes") — 0.8s
  [1] Go 1.25 Release Notes | URL: https://go.dev/doc/go1.25
  [2] ...

  ✓ web_fetch("https://go.dev/doc/go1.25") — 1.2s
  # Go 1.25 Release Notes
  > 来源：https://go.dev/doc/go1.25
  ...
```

- **`web_search`**：POST 到 DuckDuckGo HTML 端点（`html.duckduckgo.com/html/`），解析返回标题、URL、摘要列表
- **`web_fetch`**：HTTP GET + `go-readability` 提取主内容 + `html-to-markdown` 转换，输出对 LLM 友好的 Markdown
- **内置 SSRF 防护**：每次请求前校验 URL——禁止 `ftp://`、userinfo、127.x.x.x、169.254.x.x（云 metadata）、RFC1918 私网等；DNS 解析后二次检查 IP，防御 DNS rebinding；重定向链每跳复检
- **当前日期注入**：System Prompt 实时注入 `当前日期：YYYY-MM-DD`，Agent 搜索时自然带入正确年份

详见 [网页搜索与抓取技术方案](docs/核心功能/web_search.md)。

### 标准 ReAct 循环

每个 Turn 执行一次 LLM 调用（携带完整工具列表），工具结果作为 Observation 注入上下文，驱动下一轮推理：

```
Turn N: LLM(messages, tools) → Action → 并发执行工具 → Observation → Turn N+1
自然终止：模型不再发起工具调用 → 输出最终回复
```

详见 [Agent Loop 核心实现原理](docs/核心功能/agent-loop.md)。

### 并发工具执行 + 自愈能力

同一 Turn 内多个工具并发执行，每个工具独立超时控制。执行失败时，错误信息原样回传给 LLM 触发自动重试。

详见 [Tool Calling 工具调用系统](docs/核心功能/tool-calling.md)。

### 流式输出

`Run`（阻塞）和 `RunStream`（流式）双模式，共享同一引擎实例：

```go
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

---

## 架构总览

![harness9 整体架构图](harness9_architecture.png)

---

## 核心模块


| 模块             | 说明                                                                                      | 状态  |
| -------------- | --------------------------------------------------------------------------------------- | --- |
| **TUI**        | 全屏 TUI（Bubbletea）：双 Phase、流式输出、Spinner + 精确耗时、Tab 补全、Token 用量实时展示、Shell 执行模式（`!` 前缀）| ✅   |
| **Engine**     | 标准 ReAct 主循环，阻塞 + 流式双模式，EventTokenUpdate / EventCompaction / EventToolResult（精确耗时）事件   | ✅   |
| **Hooks**      | 工具拦截器：HookRegistry（洋葱模型）+ OffloadHook（超大输出 offload）+ FilePlanWriter（计划持久化）+ DangerHook（高危命令拦截）| ✅   |
| **Permission** | Human-in-the-Loop 权限控制：PermissionHook（JSON 规则）+ 审批对话框（5 选项）+ 白名单动态更新 + 敏感路径硬保护        | ✅   |
| **Sub-Agent**  | 子代理委派：内置 general-purpose 通用子代理、文件式定义（`.harness9/agents/*.md`）、task 工具（前台/后台）、`@agent` 直跑、TaskTracker、上下文隔离 + 防递归 + 权限不扩权 | ✅   |
| **Planning**   | Plan Mode（先规划后执行）、TodoStore、todo_write 工具、PlanWriter 接口、工具层权限过滤、自动续跑 + 停滞检测           | ✅   |
| **Memory**     | 短期记忆：SQLiteSession（WAL）、Manager（WithToolResultsDir + DeleteSession GC + DB() 访问器）、SummarizationCompactor（默认，LLM 摘要 + MemoryExtractor 压缩前提取钩子）、TokenBudgetCompactor（回退） | ✅   |
| **LTM**        | 长期记忆：Store（`long_term_memories` 表 + FTS5 `memories_fts`，复用 state.db；Add 签名去重 / Search 强化 / SoftDelete / List / PurgeExpired / StaleCandidates）、Precis（MEMORY.md 物化视图）、Extractor（LLM 压缩前提取，fail-open）、Phase 3 接缝（Provider/Embedder/Consolidator） | ✅   |
| **Context**    | System Prompt 结构化组装（基础 + AGENTS.md + Skills 索引 + todo 指引 + offload 检索指引 + 长期记忆精华实时注入）                | ✅   |
| **Skills**     | Skills 解析、索引、按需加载（`use_skill` 工具）                                                       | ✅   |
| **Provider**   | LLM 统一接口，OpenAI / Anthropic 适配器，实际 token 用量提取                                           | ✅   |
| **Schema**     | 跨组件共享的核心数据类型（Message、ToolCall、Usage 等）                                                  | ✅   |
| **Tools**      | 工具注册表 + 内置工具（bash / read_file（offset/limit 分页）/ write_file / edit_file / todo_write / memory_write / memory_search / **web_search / web_fetch**）| ✅   |
| **Sandbox**    | Docker 容器级隔离：OS 级进程沙箱（cap-drop/no-new-privileges）、Agent 级独立容器、bind mount 工具透明路由、TUI SandboxBar、孤儿容器回收；默认开启；`SANDBOX_ENABLED=false` 关闭 | ✅   |
| **Observability** | OpenTelemetry 链路追踪：`OTELEngineObserver`（Interaction/Turn Span）+ `TracingProvider`（LLM Span + Token Metrics）+ `ObservabilityHook`（Tool Span）；默认 noop 零开销；支持接入 Langfuse / Grafana / Jaeger | ✅   |
| **Evals**      | 自动化评估框架：`ScriptedProvider`（确定性 mock）+ `Assertion`（Hard/Soft 断言）+ `EvalHarness`（RunCase/Suite）+ `SetupHermeticEnv` + `BuildReport`（JSON/Markdown）；16 个黄金数据集用例；CI Quality Gate | ✅   |
| **MCP**        | MCP Client 集成：`internal/mcp/`（Config/StdioTransport/HTTPTransport/Client/Manager）+ MCPToolAdapter（`mcp__{server}__{tool}` 命名，BaseTool 接口）+ TUI MCPBar + `/mcp` 工具面板（e 键编辑配置）；异步启动，fail-soft，session 级生命周期 | ✅   |
| **AutoDev**    | 自举开发闭环：`/autodev` AgentSkill（需求澄清→Spec 生成→确认关卡）+ dev sub-agent（`.harness9/agents/dev.md`，git worktree + Docker Sandbox）；零新 Go 代码，复用 Skills/Sub-Agent/Sandbox 基础设施 | ✅   |
| **Env**        | 零依赖 `.env` 配置加载器                                                                        | ✅   |


---

## 项目结构

```
harness9/
├── cmd/harness9/
│   ├── main.go              # 程序入口：TUI（TTY）/ CLI（管道）自动检测
│   ├── tui.go               # TUI 核心：tuiModel、Init、RunTUI、包级样式变量
│   ├── tui_update.go        # Update 逻辑：事件、键盘、滚动、Tab 补全、Shell 执行
│   ├── tui_view.go          # View 渲染：对话区 / StatusBar / Input / Footer
│   ├── tui_banner.go        # WelcomeBanner：HARNESS9 ASCII Art
│   ├── cli.go               # 交互式 CLI REPL 实现
│   └── upgrade.go           # 自动升级：GitHub Releases API + SHA256 校验 + 原子替换
├── internal/
│   ├── engine/              # ReAct 主循环（Run + RunStream + ToolResultData）
│   ├── hooks/               # 工具拦截器（OffloadHook + FilePlanWriter + HookRegistry）
│   ├── subagent/            # Sub-Agent：定义/注册表/Runner/task 工具/TaskTracker
│   ├── planning/            # PlanMode 枚举 + TodoStore + PlanWriter 接口
│   ├── memory/              # 短期记忆：Session 持久化 + Compactor 压缩策略 + DeleteSession GC + MemoryExtractor 钩子
│   ├── ltm/                 # 长期记忆：Store（SQLite + FTS5）+ Precis（MEMORY.md）+ Extractor + Phase3 接缝
│   ├── provider/            # OpenAI / Anthropic 适配器 + 模型限制注册表
│   ├── schema/              # 共享数据类型（Message、StreamChunk、Usage）
│   ├── tools/               # 工具注册表 + 内置工具（read_file 支持 offset/limit）+ 路径沙箱
│   ├── mcp/                 # MCP Client 集成（Config/Transport/Client/Manager + MCPToolAdapter）
│   ├── context/             # System Prompt 组装（DefaultPromptBuilder + WithOffloadEnabled）
│   ├── skills/              # Skills 解析、索引、use_skill 工具
│   ├── env/                 # 零依赖 .env 加载器
│   └── logfmt/              # 块状日志格式化
├── docs/核心功能/            # 技术文档
├── skills/                  # 示例 Skills（可直接复制到项目中使用）
├── AGENTS.md                # 项目开发规范（自动注入 System Prompt）
└── CLAUDE.md -> AGENTS.md   # 符号链接，保持同步
```

---

## 文档索引


| 文档                                                           | 内容                                                   |
| ------------------------------------------------------------ | ---------------------------------------------------- |
| [快速启动指南](docs/核心功能/quick_start.md)                           | 安装、API Key 配置、TUI 首次使用、基本命令、常见问题                          |
| [TUI 交互界面实现原理](docs/核心功能/tui.md)                             | Bubbletea 架构、布局、事件流、键盘交互                                |
| [CLI 使用指南](docs/核心功能/cli.md)                                 | 启动、环境变量、AGENTS.md、Skills 配置                             |
| [Agent Skills 设计原理](docs/核心功能/agent-skills.md)               | Progressive Disclosure、frontmatter 规范、use_skill 工具      |
| [Agent Loop 核心实现原理](docs/核心功能/agent-loop.md)                 | 标准 ReAct 设计原理、PromptBuilder、流式架构                        |
| [Tool Calling 工具调用系统](docs/核心功能/tool-calling.md)             | 工具接口、并发模型、内置工具详解、扩展指南                                   |
| [Context Engineering 技术方案](docs/核心功能/context-engineering.md) | SQLiteSession、SummarizationCompactor、Token 估算、并发安全设计    |
| [Long-Term Memory 实现原理](docs/核心功能/long-term-memory.md) | 跨会话长期记忆、SQLite+FTS5 存储、MEMORY.md 物化视图、三路触发、冲突/遗忘/强化机制 |
| [Sub-Agent 系统实现原理](docs/核心功能/sub-agent.md)                     | 内置 general-purpose 子代理、文件式定义、task 工具、前台/后台执行、@agent 直跑、TaskTracker、安全隔离 |
| [Planning 模块实现原理](docs/核心功能/planning.md)                      | Plan Mode、TodoStore、工具层权限控制、自动续跑、停滞检测、跨会话持久化            |
| [文件系统能力技术方案](docs/核心功能/file-system.md)                         | OffloadHook、FilePlanWriter、read_file 分页、Session GC、Hooks 扩展 |
| [Shell 执行功能技术方案](docs/核心功能/shell-execution.md)                   | `!` 前缀触发、异步执行机制、LLM 上下文注入、截断策略、交互式命令拦截          |
| [Human-in-the-Loop 权限控制](docs/核心功能/human-in-the-loop.md)           | HookDecision、DangerHook、PermissionHook、审批对话框、白名单配置、敏感路径保护  |
| [Sandbox 沙箱系统](docs/核心功能/sandbox.md)                               | Docker 容器级隔离、Environment 接口、五状态生命周期、安全加固参数、TUI SandboxBar、孤儿回收 |
| [测试·评估·可观测体系](docs/核心功能/eval.md)                 | Test/Eval/Observability 完整体系：ScriptedProvider、黄金数据集（16 用例）、CI Quality Gate、OTEL 链路追踪、Langfuse 接入指南 |
| [网页搜索与抓取技术方案](docs/核心功能/web_search.md)           | web_search/web_fetch 工具、SSRF 防护（isSafeURL）、HTML→Markdown 管线、DuckDuckGo 后端、当前日期注入 |
| [MCP 工具集成技术方案](docs/核心功能/mcp.md)                    | JSON-RPC 2.0 协议、StdioTransport 异步设计、Manager 生命周期、MCPToolAdapter、TUI 集成、.mcp.json 配置 |
| [AutoDev 自举开发技术方案](docs/核心功能/autodev.md)            | /autodev 命令三阶段流程、dev sub-agent 设计、git worktree 隔离、Docker Sandbox 集成、使用指南 |
| [AGENTS.md](AGENTS.md)                                       | 项目开发规范、编码标准、架构决策                                        |


---

## 对标框架


| 框架 | 来源 | 与 harness9 的差异 |
| --- | --- | --- |
| DeepAgents | LangChain | Python，图编排（LangGraph StateGraph）；harness9 显式 ReAct 循环，无图引擎依赖，Go 原生 |
| OpenHarness | HKUDS | Python，asyncio 并发；harness9 goroutine 并发模型，Go 原生 |
| OpenCode | Anomaly | TypeScript，委托 Vercel AI SDK streamText，放弃循环控制权；harness9 自持显式循环，对内核有完全控制权 |
| OpenClaw | OpenClaw | TypeScript，多代理路由，委托 AI SDK；harness9 Go 原生单 Agent，聚焦单体 ReAct |
| HermesAgent | NousResearch | Python，ThreadPool 并发工具，三级上下文压缩；harness9 goroutine 并发，更轻量 |
| Claude Agent SDK | Anthropic | 官方 SDK，仅支持 Anthropic，黑盒循环；harness9 多 Provider，透明可控的显式循环，Go 原生 |
| OpenAI Agent SDK | OpenAI | Python，Handoffs 多 Agent，依赖 OpenAI Compaction API；harness9 Go 原生，自持压缩，无云 API 依赖 |


---

## Star History

[![Star History Chart](star-history.svg)](https://github.com/ZhangShenao/harness9/stargazers)

> GitHub 于 2026-06-30 起限制了 stargazers API 的访问权限（仅 owner/collaborator 可读），star-history.com 等第三方实时徽章因此普遍失效。上图由 [`.github/workflows/star-history.yml`](.github/workflows/star-history.yml) 每日用仓库自身 `GITHUB_TOKEN` 离线生成并提交为静态文件，不依赖任何第三方服务的实时可用性。

---

## SWE-bench Benchmark

评估 harness9 在 [SWE-bench Lite](https://github.com/princeton-nlp/SWE-bench) 上的 Agent 能力。

### 前置条件

- Docker daemon 运行中（用于 Sandbox 隔离）
- `git` 命令可用
- `OPENAI_API_KEY` 已配置

### 运行步骤

**1. 下载数据集**

```bash
pip install datasets
python -c "
from datasets import load_dataset
ds = load_dataset('princeton-nlp/SWE-bench_Lite', split='test')
ds.to_json('swe-bench-lite.jsonl')
"
```

**2. 运行 benchmark**

```bash
SANDBOX_IMAGE=python:3.11-slim \
OPENAI_API_KEY=your_key \
LLM_MODEL=openai/gpt-4o \
go run ./cmd/swebench \
  --dataset swe-bench-lite.jsonl \
  --sample 10 \
  --output ./swebench-results \
  --max-turns 30 \
  --parallel 2
```

**3. 评估结果**

```bash
pip install swebench
python -m swebench.harness.run_evaluation \
    --dataset_name princeton-nlp/SWE-bench_Lite \
    --predictions_path ./swebench-results/predictions.jsonl \
    --max_workers 4 \
    --run_id harness9-lite-v1
```

### 参数说明

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `--dataset` | JSONL 文件路径 | 必填 |
| `--sample` | 每 repo 抽取条数 | 10 |
| `--output` | 输出目录 | ./swebench-results |
| `--max-turns` | 每 instance 最大 Turn 数 | 30 |
| `--parallel` | 并发数 | 1 |
| `--resume` | 断点续跑 | false |
| `--timeout` | 单 instance 超时（分钟） | 10 |

---

## License

MIT
