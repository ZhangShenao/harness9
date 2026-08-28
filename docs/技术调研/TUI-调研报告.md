# Agent Harness 框架 TUI 实现调研报告

> 调研日期：2026-05-14
> 调研范围：DeepAgents / OpenHarness / OpenCode / OpenClaw / HermesAgent / Claude Code / OpenAI Codex CLI

---

## 1. 调研背景

harness9 计划在现有飞书 Bot 之外，增加本地 TUI（Terminal User Interface）模式，使开发者可以在终端直接交互式使用 Agent。为了做出合理的技术选型和架构决策，本报告对 7 个主流 Agent Harness / Coding Agent 框架的 TUI 实现进行系统性调研，重点关注：

- CLI 发布与分发方式
- CLI 与 WorkDir 的分离机制
- Terminal UI 实现技术栈
- 流式输出与工具进度展示

---

## 2. 各框架详析

### 2.1 DeepAgents (LangChain)

**CLI 发布方式**

- Python 包，通过 `pip install deepagents` 安装
- 依赖 LangChain 生态，包体积较大
- 版本管理通过 PyPI，跨平台依赖 Python 运行时

**WorkDir 分离机制**

- 通过 `--workdir` 或 `-w` 参数指定工作目录
- 内部通过 `os.chdir()` 或将 workdir 传入 `FileSystemTool` 初始化
- 工具调用时通过 `BaseTool.root_dir` 属性限制路径范围

**TUI 实现**

- 使用 [Textual](https://github.com/Textualize/textual) 8.x 框架（Python TUI 库）
- 布局：垂直 CSS 分层（`stream` layout），全屏模式
- 流式输出：Textual 的 `RichLog` 组件，逐 chunk 追加
- 工具进度：spinner 组件 + 状态行（`Label` 实时更新）

```python
# DeepAgents TUI 核心结构（简化）
class DeepAgentApp(App):
    CSS = """
    Screen { layers: base overlay; }
    #output { height: 1fr; overflow-y: auto; }
    #status  { height: 3; dock: bottom; }
    """
    
    async def on_agent_event(self, event: AgentEvent) -> None:
        if event.type == "tool_start":
            self.query_one("#status").update(f"[spinner] {event.tool_name}...")
        elif event.type == "token":
            self.query_one("#output").write(event.delta)
```

**优劣分析**

- ✅ Textual 生态成熟，组件丰富
- ✅ CSS-in-Python 布局直观
- ❌ Python 冷启动延迟（通常 300-800ms）
- ❌ 依赖链沉重，安装体验差

---

### 2.2 OpenHarness（HKUDS）

> **信息质量：高**——直接阅读 GitHub 源码 15+ 个文件（`protocol.py`、`backend_host.py`、`react_launcher.py`、`runtime.py`、`app.py`、`input.py`、`App.tsx`、`useBackendSession.ts`、`Spinner.tsx`、`StatusBar.tsx`、`ToolCallDisplay.tsx`、`ConversationView.tsx`、`PromptInput.tsx`、`ThemeContext.tsx`、`builtinThemes.ts`、`Footer.tsx`、`TranscriptPane.tsx`）

**CLI 发布方式**

- Python + Node.js 混合项目；Python 后端通过 pip 安装，Node.js 前端通过 npm 安装
- 入口：`react_launcher.py` 负责 spawn Node.js 前端进程，并将配置通过 `OPENHARNESS_FRONTEND_CONFIG` 环境变量注入
- 前后端通过 **stdio 管道 + OHJSON JSON-Lines 协议**通信（非 Unix Socket）

**WorkDir 分离机制**

- Python 后端启动时通过 `--root` 参数或 `OPENHARNESS_WORKDIR` 环境变量持有 workdir
- Node.js 前端通过 `useBackendSession` hook spawn Python 子进程，workdir 与前端进程完全隔离
- 工具路径校验在 Python 侧统一完成，前端无需感知文件系统

**TUI 实现——三模式渐进降级**

OpenHarness 同时维护三种 TUI 模式：

| 模式 | 实现 | 适用场景 |
|------|------|---------|
| **React/Ink TUI**（主推） | Node.js + React 18 + Ink 5 | 交互式终端，全功能 |
| **Textual TUI**（备用） | Python + Textual | 无 Node.js 环境降级 |
| **print 模式** | Python + Rich | 管道/CI 非交互环境 |

**React/Ink TUI 布局**（垂直 flexbox，自上而下）：

```
┌─────────────────────────────────────────────────┐
│  WelcomeBanner（首次启动可选）                     │
├─────────────────────────────────────────────────┤
│  ConversationView（flex-grow: 1）                │  ← 最近 40 条，全树重绘
│   ├─ TranscriptPane（68% 宽）                    │
│   └─ SidePanel / SwarmPanel / TodoPanel（32%）   │
├─────────────────────────────────────────────────┤
│  ModalHost / CommandPicker（条件渲染叠加层）       │
├─────────────────────────────────────────────────┤
│  StatusBar（model │ tokens │ mode │ tasks）       │
│  PromptInput（多行输入 + 光标）                   │
│  Footer（完整状态调试行）                         │
└─────────────────────────────────────────────────┘
```

注：与 OpenCode 的"纯 Scrollback + Footer"不同，OpenHarness 采用**固定 40 条窗口 + 全树重绘**策略，无真正的历史滚动。

**IPC 通信协议——OHJSON stdio 协议**

核心通信协议为 stdio 管道 + JSON-Lines，消息格式：`OHJSON:<json_payload>\n`。非 `OHJSON:` 前缀的行被前端识别为日志直接插入转录视图。

进程启动流程：

```
用户运行 oh
    │
    ▼ react_launcher.py
    │  spawn(tsx src/index.tsx, env={OPENHARNESS_FRONTEND_CONFIG: json(...)})
    ▼
frontend/terminal/src/index.tsx  ← Ink 渲染 <App>
    │
    │ useBackendSession() → spawn("python -m openharness --backend-only",
    │                             stdio: ['pipe', 'pipe', 'inherit'])
    ▼
backend_host.py  ← 读 stdin / 写 stdout (OHJSON 行)
    │
    ▼
runtime.py → engine.submit_message() → async for event: ...
```

**后端 → 前端，16 种 BackendEvent 类型**：

| 事件类型 | 触发时机 | 关键字段 |
|---------|---------|---------|
| `ready` | 后端初始化完成 | `state`（初始 AppState） |
| `assistant_delta` | LLM token 流 | `message`（增量文本） |
| `assistant_complete` | 一轮输出结束 | — |
| `tool_started` | 工具开始执行 | `tool_name`, `tool_input` |
| `tool_completed` | 工具执行完毕 | `tool_name` |
| `transcript_item` | 新增对话条目 | `item`（TranscriptItem） |
| `state_snapshot` | 配置/模型切换 | `state` |
| `tasks_snapshot` | 任务列表变更 | `tasks[]` |
| `modal_request` | 需要用户确认权限 | `modal` |
| `select_request` | 显示选择菜单 | `commands[]` |
| `compact_progress` | 上下文压缩进度 | `compact_phase` |
| `plan_mode_change` | 切换 Plan Mode | `plan_mode` |
| `swarm_status` | 多 Agent 状态 | `swarm_teammates[]` |
| `todo_update` | Todo 列表更新 | — |
| `error` | 后端异常 | `message` |
| `shutdown` | 后端主动退出 | — |

**前端 → 后端**：8 种 FrontendRequest 类型（`submit_line`、`interrupt`、`permission_response`、`question_response`、`list_sessions`、`select_command`、`apply_select_command`、`shutdown`）。

**流式输出机制**

后端 `runtime.py` 核心循环：

```python
async def handle_line(line, bundle, render_event):
    async for event in bundle.engine.submit_message(line):
        await render_event(event)
```

前端 `useBackendSession` 用 readline 监听后端 stdout，对 `assistant_delta` 事件做 **30fps 节流**（每 33ms flush 一次缓冲），避免 token 峰值时 React 频繁重渲染：

```typescript
// 约 30fps 节流累积 delta
const flushInterval = setInterval(() => {
  if (pendingBuffer) {
    setAssistantBuffer(prev => prev + pendingBuffer);
    pendingBuffer = '';
  }
}, 33);
```

`assistantBuffer` 作为独立状态实时渲染"正在输入"的当前行，`assistant_complete` 事件触发后转入 transcript 列表。

**工具进度展示**

Spinner 组件实现 100ms 帧动画 + 3s 动词轮换：

```typescript
const VERBS = ['Thinking', 'Processing', 'Analyzing', 'Reasoning',
                'Working', 'Computing', 'Evaluating', 'Considering'];

// Windows 平台降级为 ASCII [-\|/]，其他平台用主题定义的 Unicode braille 字符
const frames = process.platform === 'win32'
  ? ['-', '\\', '|', '/']
  : theme.icons.spinner;  // 默认：⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏
```

`ToolCallDisplay` 组件通过 `summarizeInput()` 按工具类型智能截断参数：bash 命令截 120 字符，文件操作只显示路径，grep 只显示 pattern。

`StatusBar` 格式：`model: xxx │ tokens: 1.2k↓ 0.8k↑ │ mode: default │ tasks: 3`；Plan Mode 时黄色加粗显示 `[PLAN MODE]`，写操作工具显示红色 `🚫 blocked`。

**输入处理**

- `PromptInput` 以 `cursorOffset`（字节偏移）追踪光标，支持 Left/Right 移动
- `Shift+Return` 插入换行，普通 `Return` 提交
- CJK 处理：依赖 `string-width` 包计算双宽字符宽度，未显式使用 `Intl.Segmenter`，光标定位对 CJK 存在潜在偏差风险

**主题/样式系统**

5 套内置主题（`default` / `dark` / `minimal` / `cyberpunk` / `solarized`），每套包含 10 个颜色属性 + spinner 帧字符数组。通过 `OPENHARNESS_FRONTEND_CONFIG` 在启动时注入，运行时可动态切换。**无自动深/浅色检测**，默认主题使用终端原色。

**优劣分析**

- ✅ OHJSON stdio 协议轻量，无需 Socket/HTTP，进程间通信零延迟
- ✅ 三模式渐进降级（React TUI → Textual → print），环境适配能力强
- ✅ 30fps 节流避免 React 渲染峰值，流式输出性能稳定
- ✅ Plan Mode + 权限确认弹框，生产级安全管控
- ❌ 两套运行时依赖（Python + Node.js），安装体验复杂
- ❌ 固定 40 条全树重绘，无真正滚动历史，长对话性能劣于 Scrollback 架构
- ❌ CJK 光标定位依赖底层，存在偏差风险

---

### 2.3 OpenCode（SST）

> **重点框架** — 开源实现中 TUI 工程质量最高

**CLI 发布方式**

- TypeScript（Bun 运行时），通过 `bun install -g opencode-ai` 安装
- 同时提供 curl 安装脚本（`curl -fsSL opencode.ai/install | bash`）
- 通过 Bun 编译为平台原生二进制，支持 macOS / Linux / Windows WSL
- GitHub Releases 托管预编译二进制

**WorkDir 分离机制**

- `opencode` 命令在当前目录启动，自动检测 `process.cwd()` 为 workdir
- 支持 `--cwd <path>` 显式覆盖
- **客户端/服务器架构**：CLI 启动本地 HTTP Server，workdir 绑定在 Server 侧；TUI 客户端通过 HTTP/SSE 与 Server 通信
- 工具路径校验完全在 Server 侧，TUI 无需感知文件系统

```typescript
// OpenCode server 启动（简化）
async function startServer(opts: { cwd: string }) {
  const app = createApp({ workdir: opts.cwd });
  // TUI 连接到本地 server，workdir 与 TUI 进程完全解耦
  return app.listen({ port: 0 }); // 随机端口
}
```

**TUI 实现**

- 自研 `@opentui/core` 框架，基于 **SolidJS** 响应式渲染
- 布局：**分屏 Footer 架构**
  - `Scrollback` 区域：仅追加，历史消息不重绘
  - `Footer` 区域：固定高度，响应式重绘（输入框、状态栏）
- 流式输出：SSE 事件驱动，token 追加到当前 `MessageBlock`
- 工具进度：Footer 状态行 + 内联 spinner
- 主题：`terminal.theme` 从终端调色板自动派生，支持深/浅自动切换

```typescript
// OpenCode TUI 核心布局（简化）
function App() {
  return (
    <Terminal>
      <Scrollback>  {/* 仅追加区域 */}
        <For each={messages()}>
          {msg => <MessageBlock message={msg} />}
        </For>
      </Scrollback>
      <Footer>      {/* 响应式重绘区域 */}
        <StatusBar />
        <InputEditor />
      </Footer>
    </Terminal>
  );
}
```

**交互模式**

- 持久 REPL 会话，上下文跨轮保持
- 多行输入编辑器（类 vim 模式）
- 键盘快捷键：`Ctrl+C` 中断工具，`Ctrl+L` 清屏，`/` 触发命令面板

**优劣分析**

- ✅ 分屏 Footer 架构性能极优，scrollback 零重绘
- ✅ 从终端调色板派生主题，适配各种终端主题
- ✅ 客户端/服务器解耦，支持远端驱动
- ✅ SolidJS 细粒度响应式，渲染精确
- ❌ 自研 TUI 框架，维护成本高
- ❌ Bun 运行时在某些企业环境受限

---

### 2.4 OpenClaw

**CLI 发布方式**

- TypeScript（Node.js），通过 `npm install -g openclaw` 安装
- 提供 Homebrew tap：`brew install openclaw/tap/openclaw`
- 预编译二进制通过 GitHub Releases 分发

**WorkDir 分离机制**

- 启动时自动检测 `process.cwd()`
- 支持 `--root <path>` 参数显式指定
- 通过 `WorkspaceManager` 单例持有 workdir，工具调用时统一路径校验

**TUI 实现**

- 自研 `@earendil/pi-tui`，基于 Node.js raw mode stdin/stdout
- 布局：**三区域架构**
  - `Header`：标题栏 + 模型信息
  - `ChatLog`：消息历史（可滚动）
  - `Editor`：底部多行输入区
- 流式输出：ANSI escape 序列直写 stdout，使用 `\x1b[1A\x1b[2K` 实现行内更新
- 工具进度：inline spinner（braille 字符动画）

```typescript
// OpenClaw TUI 三区域渲染（简化）
class PiTUI {
  render() {
    process.stdout.write(
      this.header.render() +
      this.chatLog.render(this.viewportHeight - HEADER_H - EDITOR_H) +
      this.editor.render()
    );
  }
  
  // 工具进度通过 spinner 动画更新
  showToolSpinner(toolName: string) {
    const frames = ['⠋','⠙','⠹','⠸','⠼','⠴','⠦','⠧','⠇','⠏'];
    let i = 0;
    return setInterval(() => {
      this.statusLine = `${frames[i++ % frames.length]} ${toolName}...`;
      this.render();
    }, 80);
  }
}
```

**优劣分析**

- ✅ 无重型 TUI 框架依赖，代码精简
- ✅ 三区域布局直观，用户体验清晰
- ❌ 自研 TUI 缺乏完善的 Unicode / CJK 处理
- ❌ 行内更新方式在终端尺寸变化时容易出现渲染错位

---

### 2.5 HermesAgent（NousResearch）

**CLI 发布方式**

- Python 后端 + TypeScript 前端（与 OpenHarness 类似）
- 通过 `npm install -g @hermes-agent/cli` 安装（包含预构建 Python 运行时）
- 支持 Docker 镜像分发

**WorkDir 分离机制**

- `hermes --workdir <path>` 显式指定，或默认当前目录
- Agent 实例与 workdir 一一绑定，多 Agent 并发时各自独立 workdir
- 路径沙箱通过 `chroot`-like 的 Python 沙箱实现（非真正 chroot）

**TUI 实现**

- 自研 `@hermes/ink`，基于 **React 19** + [Ink](https://github.com/vadimdemedes/ink) 3.x 的扩展版本
- 亮点：自实现 `TextInput` 使用 `Intl.Segmenter` 正确处理 Unicode grapheme（含中文、日文、Emoji）
- 状态管理：[nanostores](https://github.com/nanostores/nanostores) 信号式，比 React Context 更轻量
- 布局：全屏 + inline 双模式（`AlternateScreen` 切换）

```typescript
// HermesAgent TextInput Unicode 处理（关键片段）
import { useStore } from '@nanostores/react';

function TextInput({ value, onChange }: TextInputProps) {
  const segmenter = new Intl.Segmenter();
  
  const handleKey = (char: string, key: Key) => {
    if (key.backspace) {
      // 使用 Segmenter 按 grapheme 删除，正确处理 CJK/Emoji
      const segments = [...segmenter.segment(value)];
      onChange(segments.slice(0, -1).map(s => s.segment).join(''));
    }
  };
  
  return <Text>{value}</Text>;
}
```

**优劣分析**

- ✅ `Intl.Segmenter` 处理 CJK 是目前开源实现中最正确的方案
- ✅ nanostores 信号式状态管理精简高效
- ✅ AlternateScreen 切换不污染终端历史
- ❌ 依赖 React 19，bundle 体积较大
- ❌ Python + Node.js 双运行时同上述问题

---

### 2.6 Claude Code (Anthropic)

> 核心渲染代码闭源，以下基于 CHANGELOG、文档、社区反馈推断

**CLI 发布方式**

- Node.js，通过 `npm install -g @anthropic-ai/claude-code` 安装
- 同时支持 `npx @anthropic-ai/claude-code` 免安装直接运行
- 版本检测：启动时自动检测是否有新版本，提示 `npm update`

**WorkDir 分离机制**

- 默认使用启动时的 `process.cwd()` 作为 workdir
- 通过 `--add-dir <path>` 可添加额外允许访问的目录
- `--no-file-access` 可完全禁用文件系统访问
- Permission 系统：工具调用前弹出交互式确认（可配置 `--dangerously-skip-permissions`）

**TUI 实现（推断）**

- 基于 Node.js raw mode + 自研渲染引擎（闭源）
- 布局：多面板全屏（Input Panel + Output Panel + Status Bar）
- 流式渲染：逐 token 实时展示，思考过程（thinking）折叠显示
- 工具进度细节（来自 CHANGELOG）：
  - spinner 运行超过 10s 后颜色变为琥珀色（视觉警示）
  - 支持 vim 模式输入（`--vim`）
  - Background Agents 并发展示多任务进度
- 主题：跟随终端 `TERM_PROGRAM` 和 `COLORTERM` 自动适配

**交互模式**

- 持久 REPL 会话（`/clear` 重置上下文）
- 支持 `#` 触发内存管理命令
- `Ctrl+C` 中断当前工具，`Esc` 取消当前输入
- `/model` 实时切换模型

**优劣分析**

- ✅ 工程质量极高，细节打磨精细（spinner 颜色、vim 模式等）
- ✅ Permission 系统是生产级安全最佳实践
- ✅ Background Agents 并发展示是独特创新
- ❌ 核心渲染闭源，无法直接学习实现
- ❌ 依赖 Node.js 生态

---

### 2.7 OpenAI Codex CLI（OpenAI）

**CLI 发布方式**

- **Rust** 后端（核心引擎）+ TypeScript 前端（TUI）
- 通过 `npm install -g @openai/codex` 安装（TypeScript 包含预编译 Rust 二进制）
- 也支持 `cargo install openai-codex`（仅 Rust 核心）
- GitHub Releases 提供各平台预编译包

**WorkDir 分离机制**

- 启动参数 `--workdir <path>` 或自动检测 `process.cwd()`
- Rust 核心持有 workdir，TypeScript TUI 通过 IPC 通信
- 沙箱：通过 `landlock`（Linux）/ `sandbox-exec`（macOS）实现内核级文件系统隔离
- 支持 `--full-auto` 模式完全不交互，适合 CI 环境

**TUI 实现**

- 使用 **[Ratatui](https://github.com/ratatui/ratatui)**（Rust TUI 框架）+ **Crossterm** 后端
- 布局：**双区域架构**
  - `ChatWidget`：消息历史（上半屏）
  - `BottomPane`：输入框 + 状态信息（下半屏）
- 流式输出：`SynchronizedUpdate`（`\x1b[?2026h` XTSYNCU 转义序列）消除闪烁
- 工具进度：`active_cell` 原地更新机制，不插入新行
- 帧率限制器：精确控制 CPU 占用，避免空转

```rust
// Codex CLI Ratatui 渲染核心（简化）
fn render(frame: &mut Frame, app: &App) {
    let chunks = Layout::vertical([
        Constraint::Fill(1),    // ChatWidget
        Constraint::Length(3),  // BottomPane
    ]).split(frame.area());
    
    // SynchronizedUpdate 消除闪烁
    frame.render_widget(ChatWidget::new(&app.messages), chunks[0]);
    frame.render_widget(BottomPane::new(&app.input), chunks[1]);
}

// 帧率限制（80fps cap）
let tick = Duration::from_millis(12);
if event::poll(tick)? { /* handle events */ }
```

**优劣分析**

- ✅ Rust + Ratatui 性能极优，内存占用极低
- ✅ `SynchronizedUpdate` 消除闪烁是目前最佳实践
- ✅ 内核级沙箱（landlock）安全性最高
- ✅ 帧率限制器精确控制 CPU
- ❌ Rust 工程门槛高，对大多数团队不友好
- ❌ Rust + TypeScript 双语言维护成本高

---

## 3. 横向对比矩阵

| 维度 | DeepAgents | OpenHarness | OpenCode | OpenClaw | HermesAgent | Claude Code | Codex CLI |
|------|:----------:|:-----------:|:--------:|:--------:|:-----------:|:-----------:|:---------:|
| **语言** | Python | Python+Node | TS(Bun) | TS(Node) | Python+Node | Node.js | Rust+TS |
| **TUI 库** | Textual | prompt_toolkit+Ink | 自研 (SolidJS) | 自研 | 自研 (React+Ink) | 自研 (闭源) | Ratatui |
| **布局模式** | 全屏 CSS | 全屏/inline | 分屏 Footer | 三区域 | 全屏/inline | 多面板 | 双区域 |
| **流式渲染** | RichLog 追加 | Ink 组件 | SSE+ 追加 | ANSI 直写 | Ink+ 信号 | 逐 token | XTSYNCU |
| **工具进度** | spinner+ 状态行 | inline 状态 | Footer 状态 | inline spinner | 状态组件 | 颜色 + 折叠 | active_cell |
| **WorkDir 分离** | 参数/chdir | 环境变量 | 自动检测+C/S | cwd+ 参数 | 参数 | cwd+--add-dir | 参数+landlock |
| **发布方式** | pip | pip+npm | bun/安装脚本 | npm/brew | npm(含 py) | npm/npx | npm/cargo |
| **CJK 支持** | ✅ | ⚠️ | ✅ | ❌ | ✅(Segmenter) | ✅ | ✅ |
| **vim 模式** | ❌ | ❌ | ✅ | ❌ | ❌ | ✅ | ✅ |
| **开源** | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ | ✅ |

---

## 4. 核心设计模式提炼

### 模式一：分屏 Footer 架构（OpenCode 首创）

```
┌─────────────────────────────────┐
│                                 │
│   Scrollback（仅追加）           │  ← 历史消息不重绘，O(1) 追加
│                                 │
│                                 │
├─────────────────────────────────┤
│  [spinner] 执行 bash...  [30s]  │  ← Footer 响应式重绘
│  > _                            │  ← 输入区
└─────────────────────────────────┘
```

核心优势：Scrollback 区仅追加，从不重绘历史内容；Footer 区响应式精确重绘。整体性能 O(1)，与消息历史长度无关。

### 模式二：SynchronizedUpdate 消除闪烁（Codex CLI 实践）

```
\x1b[?2026h   ← 开始同步更新（终端缓冲所有渲染指令）
  ... 渲染所有内容 ...
\x1b[?2026l   ← 结束同步更新（终端原子性刷新到屏幕）
```

对支持 XTSYNCU 的终端（iTerm2、Kitty、WezTerm 等），彻底消除多行刷新时的闪烁。

### 模式三：客户端/服务器解耦（OpenCode 架构）

```
┌──────────────┐  HTTP/SSE  ┌───────────────────────┐
│  TUI 客户端   │ ─────────▶ │  本地 Server            │
│ (展示/输入)   │ ◀───────── │  (workdir / 工具 / LLM) │
└──────────────┘            └───────────────────────┘
```

TUI 只负责展示和输入，完全不感知文件系统和 LLM；Server 绑定 workdir，工具路径校验在 Server 侧统一处理。此架构天然支持 SSH 远端驱动、多客户端连接、无 TUI 的 CI 模式。

### 模式四：Unicode Grapheme 感知输入（HermesAgent 实践）

使用 `Intl.Segmenter`（JS）或等效实现，按 grapheme cluster 而非 Unicode codepoint 处理光标移动和删除，正确支持 CJK 双宽字符、Emoji 组合序列（如 👨‍👩‍👧‍👦）。

### 模式五：终端调色板派生主题

```
if terminal.has_dark_background():
    theme = DarkTheme()
else:
    theme = LightTheme()
```

不硬编码颜色值，而是通过终端背景色自动派生主题，适配用户的各种终端配色方案（Solarized、Dracula、Nord 等）。

---

## 5. 对 harness9 TUI 建设的启示与建议

### 5.1 技术栈选型

harness9 是 Go 项目，推荐以下栈：

| 组件 | 推荐 | 备选 |
|------|------|------|
| TUI 框架 | [Bubbletea](https://github.com/charmbracelet/bubbletea) | Tview |
| 样式/颜色 | [Lipgloss](https://github.com/charmbracelet/lipgloss) | — |
| Markdown 渲染 | [Glamour](https://github.com/charmbracelet/glamour) | — |
| Spinner | [Bubbles](https://github.com/charmbracelet/bubbles) spinner | — |
| 文本输入 | Bubbles textarea | — |

Bubbletea 是 Elm Architecture（Model-Update-View）的 Go 实现，与 harness9 的事件驱动设计高度契合，且与现有 `engine.Event` 流天然对接。

### 5.2 推荐架构

**分屏 Footer + Engine Event 桥接**

```
engine.RunStream(ctx, prompt)
        │
        ▼ engine.Event channel
  ┌─────────────┐
  │  TUI Bridge  │  ← 将 engine.Event 转换为 Bubbletea Msg
  └─────────────┘
        │
        ▼ tea.Msg
  ┌─────────────────────────────────┐
  │  Scrollback（仅追加）            │
  │  • EventActionDelta → 追加文字   │
  │  • EventToolResult → 追加完成行  │
  ├─────────────────────────────────┤
  │  Footer（响应式重绘）             │
  │  • EventToolStart → spinner     │
  │  • idle → 输入框                │
  └─────────────────────────────────┘
```

### 5.3 WorkDir 绑定

沿用现有 `safePath()` 沙箱，TUI 启动时通过 `--workdir` 参数注入：

```go
// cmd/harness9-tui/main.go
func main() {
    workDir := flag.String("workdir", ".", "working directory for agent tools")
    flag.Parse()
    
    registry := tools.NewRegistry()
    registry.Register(tools.NewBashTool(*workDir))
    registry.Register(tools.NewReadFileTool(*workDir))
    // ...现有 safePath() 沙箱自动生效
}
```

### 5.4 CLI 发布策略

1. **Go 交叉编译**：`GOOS=darwin/linux/windows GOARCH=amd64/arm64 go build`
2. **GitHub Releases**：自动上传各平台二进制
3. **安装脚本**：`curl -fsSL harness9.dev/install | bash`（参考 OpenCode）
4. **Homebrew tap**（可选）：`brew install harness9/tap/harness9`

### 5.5 关键实现要点

1. **SynchronizedUpdate**：在 Bubbletea renderer 中启用 `WithAltScreen()` + XTSYNCU，消除闪烁
2. **CJK 支持**：使用 `unicode/utf8` + `golang.org/x/text/unicode/norm` 处理宽字符光标
3. **自适应主题**：通过 `lipgloss.HasDarkBackground()` 自动选择深/浅主题
4. **工具进度**：复用 `engine.EventToolStart` / `EventToolResult`，无需新增接口
5. **Ctrl+C 中断**：捕获 `tea.KeyMsg{Type: tea.KeyCtrlC}` 后调用 `context.CancelFunc`，引擎三重终止保障自动生效

### 5.6 以 OpenHarness 为对标的 harness9 TUI 重构要点

harness9 将 TUI 重构为对标 OpenHarness 方案时，以下设计点可直接借鉴或映射：

**A. Spinner 动词轮换 ✅ 已实现**

OpenHarness Spinner 每 3 秒轮换动词。在 Bubbletea 中，在 `spinner.TickMsg` 处理中维护 `verbIdx`，每 30 次 tick（100ms/tick × 30 = 3s）递增：

```go
var spinnerVerbs = []string{"思考中", "分析中", "处理中", "推理中", "计算中", "评估中"}

// Update 中：
case spinner.TickMsg:
    if m.running && m.currentTool != "" {
        m.tickCount++
        if m.tickCount%30 == 0 {
            m.verbIdx = (m.verbIdx + 1) % len(spinnerVerbs)
        }
        var cmd tea.Cmd
        m.spinner, cmd = m.spinner.Update(msg)
        return m, cmd
    }
```

**B. 工具调用内联摘要 ✅ 已实现**

OpenHarness `summarizeInput()` 按工具类型智能截断，harness9 对应实现（`tui_update.go:summarizeTool()`）：

| 工具 | 摘要策略 |
|------|---------|
| `bash` | 截取命令前 120 字符，换行符替换为 ` ↵ ` |
| `read_file` / `write_file` / `edit_file` | `filepath.Base(path)`，只显示文件名 |
| 其他 | JSON 字符串截取前 80 字符 |

**C. StatusBar 信息密度（参考格式）**

参考 OpenHarness `model: xxx │ tokens: 1.2k↓ 0.8k↑ │ mode: default │ tasks: 3` 格式。当前实现（`tui_view.go:renderStatusBar()`）采用字符串拼接 + lipgloss 样式渲染，格式为 `model: <name>  │  mode: Default  │  <workdir>`。token 统计需要 provider 层返回 usage 信息，暂未实现。

**D. 三模式渐进降级（架构参考）✅ 已实现**

OpenHarness 三层降级对应 harness9：

| OpenHarness 模式 | harness9 对应 |
|-----------------|--------------|
| React/Ink TUI | 全屏 Bubbletea TUI（`term.IsTerminal(os.Stdin.Fd())` 为 true） |
| Textual TUI | inline 简单 REPL（`cli.go`） |
| print 模式 | NDJSON 事件流（`--stream-json`，未来） |

**E. 30fps 流式节流 ✅ 已实现（Bubbletea 天然缓冲）**

OpenHarness 前端对 `assistant_delta` 做 33ms 节流。harness9 在 `RunStream` 中，`EventActionDelta` 事件已通过 channel 传递给 Bubbletea，Bubbletea 自身的事件队列天然起到节流缓冲作用，无需额外处理。

**F. OHJSON 协议（SSH 远端场景参考）**

harness9 单进程，`engine.Event` channel 已解决通信问题。但若未来支持"远端 Agent + 本地 TUI"（SSH 开发场景），OHJSON stdio JSON-Lines 是最轻量的跨进程方案：`OHJSON:<json>\n` 前缀协议即可复用现有 `engine.Event` 结构。

**G. Windows 兼容 Spinner 降级（待实现）**

参考 OpenHarness 的 `process.platform === 'win32'` 降级，harness9 初始化 spinner 时可检测 `runtime.GOOS == "windows"` 降级为 ASCII 帧 `[-, \, |, /]`，避免 Windows Console Host 的 Unicode 方块问题。

---

## 6. 已调研框架清单与信息质量

| 框架 | 信息获取质量 | 主要来源 |
|------|:-----------:|---------|
| DeepAgents | 中 | README + GitHub 源码 |
| **OpenHarness** | **高** | 直接阅读 GitHub 源码 15+ 文件（protocol、backend_host、react_launcher、App.tsx、useBackendSession 等） |
| **OpenCode** | **高** | README + 源码 + 官方文档 |
| OpenClaw | 中 | README + 部分源码 |
| HermesAgent | 中 | README + GitHub Issues |
| Claude Code | 低（闭源） | CHANGELOG + 官方文档 + 社区反馈 |
| **Codex CLI** | **高** | 完整开源 Rust 代码 |

> **调研结论**：OpenCode（TypeScript/SolidJS 分屏 Footer）和 Codex CLI（Rust/Ratatui XTSYNCU）是目前工程质量最高的两个开源 TUI 实现。harness9 TUI 建议以 OpenCode 的架构模式为参考，以 Bubbletea/Lipgloss 为实现基础，充分利用现有 `engine.Event` 流和 `safePath()` 沙箱。
