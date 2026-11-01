# Desktop App 桌面客户端

harness9 Desktop 是 harness9 的 macOS 桌面客户端（一期），对标 OpenAI Codex 桌面版的产品形态：会话工作台、流式对话、人类审批、Plan/Diff 审查闭环，底层完整复用 harness9 的 Go 引擎。一期只做 Mac；技术栈为 Wails v2（Go 进程内嵌 WKWebView）+ React/TypeScript。

---

## 文件结构

```
desktop/                            # 嵌套 Go 模块（Wails 桌面端）
├── go.mod                          # module github.com/harness9/desktop + replace ../
├── wails.json                      # Wails 工程配置（产品名/版本/前端构建命令）
├── main.go                         # 入口：Wails 运行时装配 + 日志重定向
├── app.go                          # 绑定层：appservice → 前端方法 + 事件桥
├── build/
│   ├── appicon.png                 # 应用图标（1024x1024）
│   └── bin/harness9.app            # 构建产物（自签名）
└── frontend/                       # React + TypeScript + Vite
    └── src/
        ├── types.ts                # 与 Go DTO 一一对应的类型契约
        ├── store.ts                # zustand 全局状态 + 事件装配（对标 TUI handleEvent）
        ├── App.tsx                 # 布局骨架与初始化流程
        └── components/
            ├── Sidebar.tsx         # 会话列表 / 新建任务 / 沙箱与模型状态
            ├── Chat.tsx            # 对话流：Markdown/Thinking/工具卡片/token 条
            ├── Composer.tsx        # 输入区：斜杠补全（/skill + 内置命令）
            ├── ApprovalModal.tsx   # 五选项审批弹窗（多审批排队）
            ├── RightPanel.tsx      # Plan 进度 / Diff 审查双标签面板
            ├── SettingsDialog.tsx  # 设置（模型/权限模式/沙箱后端）
            └── Welcome.tsx         # 工作目录选择页
internal/appservice/                # 应用服务层（核心，见下文）
```

---

## 架构：对标 Codex 的三层设计

Codex 的产品线（CLI / IDE 扩展 / macOS 桌面版）共用一个 Rust core，其上以 JSON-RPC app-server 协议对接多种客户端。harness9 借鉴这一分层，但一期选择更轻的进程内形态：

```
┌─ harness9.app（Wails v2）─────────────────────────────┐
│  React 前端（WKWebView）                               │
│   ▲ agent:event 事件推送        ▼ 绑定方法（命令）       │
│  app.go 薄绑定层（无业务逻辑）                          │
└──────────────┬────────────────────────────────────────┘
               │ 同进程直接调用（desktop 嵌套模块 replace 主模块）
┌──────────────▼────────────────────────────────────────┐
│  internal/appservice —— 应用服务层                      │
│   sharedStack（跨会话共享）+ SessionStack（会话级栈）     │
│   事件泵 · 审批桥 · 会话 CRUD · diff · 设置              │
└──────┬───────────────────┬────────────────────────────┘
       │                   │
  engine / tools /      internal/sandbox
  hooks / memory        （Docker + Seatbelt 双后端）
  （全部复用，零改动）
```

关键取舍：

- **`internal/appservice` 是唯一的装配事实源**。`cmd/harness9/main.go` 的装配序列（env → observability → skills → provider → sandbox → memory → ltm → tools → mcp → hooks → subagent → compactor → engine）被参数化抽取，桌面端不再复制 main.go。
- **嵌套 Go 模块解决 `internal/` 可见性**。Go 的 internal 规则是目录树词法检查：`desktop/go.mod` 通过 `replace github.com/harness9 => ../` 引用主模块源码，导入方目录位于主模块树内，因此 `internal/` 可导入；同时嵌套模块被主模块 `go build ./...` 自动跳过，CI 与 GoReleaser 零感知。
- **DTO 优先**。服务层全部对外数据结构可 JSON 序列化（`SessionInfoDTO`/`ApprovalData`/`HistoryItem`/`DiffResult` 等），二期平移为 JSON-RPC over stdio 的独立 app-server 进程（完整对标 Codex）时接口无需重设计。

### sharedStack 与 SessionStack

| 栈 | 生命周期 | 内容 |
|----|---------|------|
| sharedStack | Service 级（随 workDir） | provider、OTEL、skills 索引、沙箱环境、memory.Manager、LTM Store/Precis、MCP Manager、公共 hooks（perm/danger/obs） |
| SessionStack | 会话级（打开/新建时重建） | engine、HookRegistry、工具 registry（含 task 工具）、PlanStore、OffloadHook、CompactionOffloader、ProgressiveCompactor、SubAgent Runner |

会话栈整体重建是刻意设计：OffloadHook / FilePlanWriter / CompactionOffloader 都绑定 sessionID，按会话重建保证了 offload 文件与压缩记录的归属永远正确（同时修复了 TUI 切换会话后 offload 仍写旧 sessionID 的隐患）。构造成本只是几次结构体初始化，provider 与沙箱容器等重资源保持共享。

---

## 事件协议

`Service.SendMessage` 以单飞语义启动 `engine.RunStream`，后台 goroutine 持续泵送事件到监听者（桌面端转发为 Wails `EventsEmit("agent:event", evt)`）：

| 事件 Type | 载荷 | 前端渲染 |
|-----------|------|---------|
| `action_delta` | string | 正文流式 Markdown |
| `thinking_delta` | string | 深灰折叠块（流式中自动展开） |
| `tool_start` | `schema.ToolCall` | 工具卡片（running 态） |
| `tool_result` | `engine.ToolResultData` | 卡片回填输出/耗时/错误态 |
| `token_update` | `engine.TokenUpdateData` | 底部 token 计量条（绿/黄/红分档） |
| `compaction` | `memory.CompactionRecord` | 「上下文已压缩」通知条 |
| `sub_agent` | `schema.SubAgentUpdate` | 子代理进度卡片（按 agent 聚合） |
| `approval_required` | **`ApprovalData`**（服务层 DTO，非引擎原始结构） | 审批弹窗入队 |
| `done` / `error` | nil / string | 收尾：固化流式缓冲、刷新 Plan 与 Diff |

两个关键并发设计（与 TUI 的差异）：

1. **审批不阻塞泵**。引擎事件通道无缓冲，TUI 在弹窗期间暂停读取，依赖「同一时刻只有一个工具请求审批」的约定；appservice 的泵收到审批事件后登记到 `pendingApprovals` 即继续消费，多工具并发审批天然不死锁，UI 侧排队展示。
2. **应答恰好一次且不阻塞**。`RespondApproval(id, ...)` 把决策送回引擎的 `ResponseCh`；若 run 已被取消（接收方消失），应答在 30s 超时后丢弃，goroutine 随 Service ctx 退出。

前端组装规则（`store.ts`）：流式 delta 进缓冲，工具边界（tool_start / done / error）触发 flush 固化为时间线条目；打开历史会话时由 `HistoryItem` 时间线直接还原（工具调用与结果归并到同一张卡片）。

---

## 人类审批（Human-in-the-Loop）

审批弹窗与 TUI 的五选项语义完全一致：

| 选项 | 行为 |
|------|------|
| 允许一次 | `ApprovalResponse{Approved: true}` |
| 总是允许 | `Approved: true, Remember: true` + 写白名单 |
| 拒绝 | `Approved: false`（工具以 IsError 结果回传 LLM） |
| 拒绝并反馈 | `Approved: false, Feedback: "…"`（反馈原文回传 LLM，触发自愈） |

「总是允许」的白名单持久化移植自 TUI 的 `writeApprovalToConfig`：bash 按命令首词构造 glob（如 `bash(*git*)`），其余工具按工具名整体放行，写入 `<workDir>/.harness9/settings.json`；`permission.NewFileHook` 每次工具调用重读该文件，规则即时生效。

弹窗顶部按 `RiskLevel`（来自 DangerHook 的 high/medium 或 permission 规则的默认 ask）着色，参数 JSON 以代码块预览。多个审批同时到达时排队逐个处理。

---

## Plan / Diff 审查闭环

对标 Codex 的 review 闭环：

- **Plan 面板**：右侧标签页实时渲染 `PlanStore` 的条目状态（pending ○ / in_progress ◐ / completed ● / cancelled ⊘）与进度条；`done` 事件后自动刷新。plan_write 成功触发的自动续跑逻辑仍在引擎 + TUI 语义内，桌面端只做展示。
- **Diff 面板**：`GetSessionDiff` 在 workDir 执行 `git status --porcelain` + `git diff HEAD`（--stat + patch），前端按 `diff --git` 切分为每文件折叠块，+/- 行着色，未跟踪文件单独列出。一期语义是「整个 workDir 的未提交变更」（无 worktree 任务隔离；二期引入后 diff 将按任务分支精确圈定）。

---

## Seatbelt：macOS 原生沙箱（新后端）

参考 Codex 在 macOS 上的做法，新增 `sandbox.SeatbeltEnvironment`（`internal/sandbox/seatbelt.go`），实现既有的 `Environment` 接口：

- 每次 `RunBash` 以 `sandbox-exec -p <profile> bash -c <cmd>` 拉起子进程，profile 为 **deny-by-default**：`(deny default)` + `(import "system.sb")` + 全盘可读 + 进程执行/fork；
- **workspace-write** 档追加可写根（workDir / TMPDIR / ~/.harness9，全部经符号链接解析——macOS 的 /tmp、/var 是指向 /private 的软链，不解析会静默失效）与网络放行；
- **read-only** 档禁写禁网，适合只读探索任务；
- 文件工具（read_file/write_file）仍走宿主 IO + `safePath` 路径沙箱，与 Docker 后端的 bind-mount 语义一致。

与 Docker 后端的关系：Seatbelt 无容器生命周期（不经过 Manager、零守护进程依赖、秒启），是桌面端的一等默认后端；Docker 提供更强的容器级硬隔离，二者通过设置页或 `SANDBOX_BACKEND` 环境变量切换（`seatbelt` / `docker` / `off`）。定位是行为护栏而非对抗性安全边界（与项目 NetworkBlockedHosts 的威胁模型口径一致）。

桌面端设置持久化在 `~/.harness9/desktop/settings.json`（模型 / 权限模式 / 沙箱后端 / 最近工作目录）。生效时机：权限模式下次打开会话生效（会话栈重建时读取），模型与沙箱后端重启生效。API Key 仍由 workDir 的 `.env` 提供，不进设置文件。

---

## 会话管理

| 能力 | 实现 |
|------|------|
| 列表 | `memory.Manager.ListSessions`（updated_at 降序，含标题/消息数） |
| 新建 | 每次启动与「＋新建任务」都创建新会话（对齐 TUI 语义） |
| 恢复 | `OpenSession` 重建会话栈 + 返回 `HistoryItem` 时间线 |
| 标题 | `sessions` 表新增 `title` 列（PRAGMA 探测 + ALTER TABLE 幂等迁移，旧库无损）；首条消息自动取首行截断 30 字符，双击侧栏可重命名 |
| 删除 | `DeleteSession` 级联清理消息/计划/offload 目录/压缩记录；运行中的会话禁止删除 |

---

## 构建与运行

```bash
# 依赖：Go 1.25+、Node 18+、Xcode Command Line Tools、wails CLI
go install github.com/wailsapp/wails/v2/cmd/wails@latest

cd desktop
wails build            # 产物：desktop/build/bin/harness9.app（arm64，ad-hoc 签名）
open build/bin/harness9.app
```

开发模式（前端热更新）：`cd desktop && wails dev`。

运行时日志写入 `~/.harness9/desktop/logs/app.log`（stdlib log 全量重定向，规避 GUI 进程 stderr 不可见的问题）。

---

## 测试

- `internal/appservice` 单元测试：会话生命周期、事件泵（含审批并发场景：允许+白名单持久化、拒绝+反馈回传）、`historyToItems` 归并、git diff 采集、设置读写、权限模式互转；
- `internal/sandbox/seatbelt_test.go`：真实 `sandbox-exec` 冒烟（workspace-write 可写 workDir / 越界写入被拒 / read-only 禁写禁网 / 网络分档），非 darwin 自动跳过；
- 既有引擎与 eval 黄金数据集零回退（服务层不改变任何 Agent 行为语义）。

---

## 实现参考

| 框架 | 借鉴点 |
|------|--------|
| OpenAI Codex | app-server 分层（core → service → 多客户端）、审批策略与沙箱策略分离、macOS Seatbelt 沙箱、diff review 闭环、Thread 标题/持久化/resume |
| Claude Code | 五选项审批交互（允许/总是允许/拒绝/拒绝并反馈）、工具卡片信息密度 |
| Wails v2 | Go 单语言桌面壳、进程内绑定、WKWebView 打包 |
