# Shell 执行功能技术方案

## 概述

harness9 的 Shell 执行功能允许用户在 TUI 对话框中直接运行 Bash 命令，无需切换到独立终端。命令输出实时追加到对话流，并在下一次向 LLM 发送消息时自动注入上下文，使 Agent 能够引用命令结果进行推理。

触发方式：输入框以 `!` 开头即进入 Shell 模式，按 `Enter` 执行，`Esc` 取消。

---

## 设计原则

| 原则 | 实现方式 |
|------|---------|
| **不打断对话流** | 命令输出 inline 追加到 Scrollback，不弹出新页面 |
| **LLM 感知结果** | 输出缓冲到 `pendingShellOutput`，下次 dispatch 时前置注入 |
| **不阻塞 TUI** | 通过 `tea.Cmd` 异步执行，主 goroutine 不等待 |
| **安全拦截** | 已知交互式命令（vim/ssh 等）直接拒绝，提示在独立终端运行 |
| **内存有界** | 存储侧按字节截断，防止大输出长期占用内存 |

---

## 交互流程

```
用户输入 "!git status"
    │
    ▼
tea.KeyEnter 触发 Update()
    │
    ▼
strings.HasPrefix(raw, "!") → true
    │
    ▼
dispatchShellCommand("git status")
    ├── 空命令 → 直接返回，无操作
    ├── isInteractiveCmd → 拒绝，显示错误提示
    └── 正常命令
            │
            ▼
        lines 追加 "$ git status"（shellCmdStyle）
            │
            ▼
        返回 runShellCmd(workDir, cmd) 作为 tea.Cmd
            │
            ▼  [异步 goroutine，30s 超时]
        exec.CommandContext("bash", "-c", "git status")
            │
            ▼
        shellResultMsg{cmd, output, isErr, dur}
            │
            ▼
    case shellResultMsg: 在 Update() 中处理
        ├── 展示侧：truncateUTF8(output, 4096) 逐行追加（shellOutputStyle）
        ├── 状态行：✓ 完成 / ✗ 非零退出 + 耗时
        └── 存储侧：truncateUTF8(output, 2048) 追加到 pendingShellOutput
```

---

## 视觉状态切换

输入框实时检测是否以 `!` 开头，切换 Shell 模式视觉标识：

```
普通模式                        Shell 模式
┌──────────────────────┐        ┌──────────────────────────────────┐
│  ›  输入任务...       │        │  [SHELL]  $  !git status█        │
└──────────────────────┘        └──────────────────────────────────┘

状态栏：深灰底 #235              状态栏：深绿底 #22（shellStatusBarStyle）
Footer：正常快捷键               Footer：enter 执行 / esc 取消 / ctrl+c 退出
```

### 涉及的样式变量（`tui.go`）

| 变量 | 用途 |
|------|------|
| `shellStatusBarStyle` | 深绿底（#22）+ 浅绿文字（#120）状态栏，与默认灰底状态栏明确区分 |
| `shellModeTagStyle` | 输入区 `[SHELL]` 徽章：深橄榄背景（#58）+ 亮黄文字（#226） |
| `shellModeAccentStyle` | 亮绿色（#83）accent，替换默认青色 |
| `shellModePromptStyle` | `$` 提示符样式，预计算避免每帧 `.Bold(true)` 分配 |
| `shellModeLabelInBarStyle` | 状态栏内 `SHELL` 标签，亮绿粗体 |
| `shellCmdStyle` | 命令行 `$ cmd`，黄色粗体（#33） |
| `shellOutputStyle` | 输出行，浅灰（#250） |
| `shellOKStyle` | `✓ 完成` 行，绿色（#34） |
| `shellErrStyle` | `✗ 非零退出` 行，红色（#160） |

颜色切换逻辑集中在 `tui_view.go` 的两个方法中，View 层无散落的 if 判断：

```go
func (m tuiModel) accentStyle() lipgloss.Style         // 返回当前模式的强调色
func (m tuiModel) activeStatusBarStyle() lipgloss.Style // 返回当前模式的状态栏容器样式
```

优先级（高→低）：

```
shellMode=true  →  深绿底 #22 + 亮绿 accent #83
Plan/AutoEdit   →  深橙底 #94 + 琥珀黄 accent #220
Default         →  深灰底 #235 + 青色 accent #81
```

---

## 核心数据流

### `tuiModel` 字段

```go
pendingShellOutput []string  // 本轮积累的 Shell 命令记录，下次 dispatch 清空
shellMode          bool      // 输入框以 "!" 开头时为 true，驱动 View 层切换样式
```

### `shellResultMsg` 类型

```go
type shellResultMsg struct {
    cmd    string        // 原始命令字符串
    output string        // stdout + stderr 合并输出（CombinedOutput）
    isErr  bool          // exit code != 0
    dur    time.Duration // 实际执行耗时
}
```

### LLM 上下文注入

用户下一次按 `Enter` 发送消息时，`dispatch()` 在 prompt 前置注入缓冲的命令记录：

```
[用户执行的 Shell 命令记录]
$ git status
On branch main...

---
$ go build ./...
# github.com/harness9/cmd/harness9
...

[用户的实际问题]
```

注入后 `pendingShellOutput` 清空，避免重复注入。每条记录独立截断至 `maxShellContextLen`（2048 字节），多条之间以 `---` 分隔。

---

## 截断策略

Shell 功能涉及两个截断边界，都使用 `truncateUTF8` 保证字节截断不破坏多字节字符：

| 场景 | 常量 | 截断时机 | 截断标记 |
|------|------|---------|---------|
| TUI 展示 | `maxShellDisplayLen = 4096` | `case shellResultMsg:` 展示前 | `...[输出过长，已截断，建议用 head -n N 重新执行]...` |
| LLM 上下文 | `maxShellContextLen = 2048` | `case shellResultMsg:` 存储时 | 无（直接截断，LLM 可感知内容不完整） |

### `truncateUTF8` 实现

```go
func truncateUTF8(s string, maxBytes int) string {
    if len(s) <= maxBytes {
        return s
    }
    s = s[:maxBytes]
    for len(s) > 0 {
        r, size := utf8.DecodeLastRuneInString(s)
        if r != utf8.RuneError || size > 1 {
            break
        }
        s = s[:len(s)-1]
    }
    return s
}
```

`utf8.DecodeLastRuneInString` 对末尾不完整序列返回 `(RuneError, 1)`，逐字节后退直到末尾为合法 rune。对有效的 `RuneError`（U+FFFD，`size > 1`）不回退。

---

## 异步执行机制

Shell 命令通过 Bubbletea 的 `tea.Cmd` 异步模式执行，TUI 主循环不阻塞：

```
Update() 返回 (m, runShellCmd(workDir, cmd))
    │
    ▼  Bubbletea runtime 在独立 goroutine 执行该 Cmd
exec.CommandContext(ctx, "bash", "-c", cmd)  // 30s 超时
    │
    ▼  Cmd 返回一个 tea.Msg
shellResultMsg → 发送到主消息队列
    │
    ▼
Update() case shellResultMsg: 处理结果
```

工作目录固定为 `tuiModel.workDir`（程序启动目录），通过 `c.Dir = workDir` 注入。`stdout` 和 `stderr` 通过 `CombinedOutput()` 合并，确保错误信息对用户可见。

---

## 交互式命令拦截

Bubbletea 以 AltScreen 模式运行，独占终端输入输出，PTY 依赖类程序无法正常工作：

```go
var interactiveCmds = map[string]bool{
    "vim": true, "vi": true, "nano": true, "emacs": true,
    "ssh": true, "top": true, "htop": true, "less": true,
    "man": true, "more": true, "watch": true, "tmux": true,
    "screen": true,
}
```

`isInteractiveCmd` 提取命令行第一个 token 的 `filepath.Base`（处理 `/usr/bin/vim` 等绝对路径），与拦截列表匹配。命中时输出 `✗ 该命令需要交互式终端，请在独立终端窗口中运行`，不执行命令。

---

## 键盘行为

| 按键 | 行为 |
|------|------|
| `!` (首字符) | 触发 Shell 模式视觉切换（实时，无需 Enter） |
| `Enter` | 执行 `!` 后的命令；命令执行完成前输入框不可用 |
| `Esc` | 清空输入框，退出 Shell 模式（仅在非执行中状态） |
| `Ctrl-C` | 若命令运行中：取消（注：当前实现 30s 超时到期才取消）；否则退出程序 |
| `Backspace` (删除 `!`) | 实时退出 Shell 模式，恢复普通输入提示符 |

---

## 代码位置索引

| 内容 | 文件 | 位置 |
|------|------|------|
| Shell 样式变量（`shellCmdStyle` 等） | `cmd/harness9/tui.go` | `var (...)` 块，Shell 模式样式分组 |
| `shellMode` / `pendingShellOutput` 字段 | `cmd/harness9/tui.go` | `tuiModel` struct |
| 常量 `maxShellDisplayLen` / `maxShellContextLen` | `cmd/harness9/tui_update.go` | `const (...)` 块 |
| `shellResultMsg` 类型 | `cmd/harness9/tui_update.go` | `shellResultMsg` struct 定义处 |
| Esc 退出 Shell 模式 | `cmd/harness9/tui_update.go` | `case tea.KeyEsc:` |
| Enter 分发 Shell 命令 | `cmd/harness9/tui_update.go` | `case tea.KeyEnter:`，`strings.HasPrefix(raw, "!")` 分支 |
| `case shellResultMsg:` 结果处理 | `cmd/harness9/tui_update.go` | `Update()` 中 `case shellResultMsg:` |
| Shell 模式实时检测 | `cmd/harness9/tui_update.go` | `Update()` 末尾 textinput fallthrough 区块 |
| `dispatch()` 上下文注入 | `cmd/harness9/tui_update.go` | `dispatch()` 函数 `pendingShellOutput` 处理块 |
| `truncateUTF8` | `cmd/harness9/tui_update.go` | `truncateUTF8` 函数 |
| `interactiveCmds` / `isInteractiveCmd` | `cmd/harness9/tui_update.go` | `interactiveCmds` var + `isInteractiveCmd` 函数 |
| `runShellCmd` | `cmd/harness9/tui_update.go` | `runShellCmd` 函数 |
| `dispatchShellCommand` | `cmd/harness9/tui_update.go` | `dispatchShellCommand` 函数 |
| `accentStyle()` / `activeStatusBarStyle()` | `cmd/harness9/tui_view.go` | 文件开头两个方法 |
| `renderStatusBar()` SHELL 标签 | `cmd/harness9/tui_view.go` | `renderStatusBar`，`modePart` 赋值分支 |
| `renderInput()` Shell 模式 | `cmd/harness9/tui_view.go` | `renderInput` 首 `if m.shellMode` 分支 |
| `renderFooter()` Shell 模式 | `cmd/harness9/tui_view.go` | `renderFooter` 首 `if m.shellMode` 分支 |
| 单元测试 | `cmd/harness9/tui_test.go` | `TestShell*`、`TestTruncateUTF8*` |
