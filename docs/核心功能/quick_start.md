# harness9 快速启动指南

本文引导你在 5 分钟内完成安装、配置，并运行第一个 harness9 Agent 会话。

---

## 1. 安装

### 方式一：一键安装脚本（推荐）

```bash
curl -fsSL https://raw.githubusercontent.com/ZhangShenao/harness9/master/scripts/install.sh | bash
```

安装完成后验证：

```bash
harness9 --version
# harness9 v0.1.0
```

### 方式二：从源码构建（开发者）

需要 Go 1.25+：

```bash
git clone https://github.com/ZhangShenao/harness9
cd harness9
go build -o harness9 ./cmd/harness9
```

---

## 2. 配置 API Key

harness9 使用 OpenAI 兼容接口（也支持 Anthropic、OpenRouter 等）。

```bash
export OPENAI_API_KEY="sk-..."
```

建议将上述命令写入 `~/.zshrc` 或 `~/.bashrc`，避免每次重新设置。

### 可选：切换模型

```bash
export LLM_MODEL="openai/gpt-4o"          # 默认：openai/gpt-4o-mini
```

### 可选：使用 OpenRouter 或其他兼容 API

```bash
export OPENAI_BASE_URL="https://openrouter.ai/api/v1"
export OPENAI_API_KEY="<your-openrouter-key>"
export LLM_MODEL="openai/gpt-4o"
```

### 使用 Anthropic

```bash
export ANTHROPIC_API_KEY="sk-ant-..."
export LLM_MODEL="claude-sonnet-4-6"
```

### 使用项目级 `.env` 文件

在项目目录放置 `.env`（**不要提交到 Git**）：

```env
OPENAI_API_KEY=sk-...
LLM_MODEL=openai/gpt-4o-mini
```

> 优先级：`export` 环境变量 > `.env` 文件

---

## 3. 首次运行

```bash
cd /your/project   # 进入你的项目目录
harness9           # 启动（自动将当前目录设为 Agent 工作沙箱）
```

在交互式终端中自动进入全屏 TUI 模式：

```
         ╦ ╦  ╔╦╗  ╔═╗  ╔╗╦  ╔══  ╔══  ╔══  ╔═╗
         ╠═╣  ╠╩╣  ╠╦╝  ║╚╗  ╠═   ╚═╗  ╚═╗  ╚═╣
         ╩ ╩  ╚ ╝  ╩╗   ╩ ╩  ╚══  ══╝  ══╝    ╝

  harness9  ·  An AI-powered coding agent
  model: gpt-4o-mini  │  workdir: /your/project
  › 输入任务后按 Enter 发送
```

输入任务并按 Enter，Agent 将自动读取代码、执行命令、分析结果：

```
  ▶ You: 帮我分析 main.go 里的 bug

  ◆ harness9:
    好的，我先读取文件...
    ✓ read_file(main.go) — 234ms
    发现第 42 行存在空指针解引用问题...
```

---

## 4. 基本命令

| 命令 | 说明 |
|------|------|
| `/new` | 开启全新会话（清除当前对话历史） |
| `/resume` | 列出历史会话并选择恢复 |
| `/exit` | 退出 TUI |
| `Tab` | 补全命令或 Skill 名称 |
| `↑ / ↓` | 滚动对话历史 |
| `Ctrl-C` | 中断正在运行的 Agent；再按一次退出 |

---

## 5. 配置项目规范（可选）

在项目根目录放置 `AGENTS.md`，启动时自动注入 System Prompt：

```markdown
# 我的项目规范

## 技术栈
- Go 1.25, PostgreSQL 16

## 编码规范
- 所有函数必须有注释
- 禁止直接操作数据库，必须通过 Repository 层
```

---

## 6. 添加 Skills（可选）

在 `skills/<name>/SKILL.md` 下放置 Agent 可按需加载的领域知识：

```bash
mkdir -p skills/refactor-guide
```

```markdown
---
name: refactor-guide
description: Use when refactoring Go code — explains team conventions
---

# 重构规范
1. 先运行 go vet，修复所有 warning
2. 保持函数不超过 50 行
```

---

## 7. 上下文管理

会话历史自动持久化到 `~/.harness9/sessions.db`，进程重启后可通过 `/resume` 恢复。

状态栏实时显示 token 用量：

```
ctx: 45.2K/128K (35%)   ← 绿色：正常
ctx: 92.1K/128K (72%)   ← 黄色：警告
ctx: 108K/128K (84%)    ← 红色：即将触发压缩
```

当上下文接近模型限制时，harness9 自动调用 LLM 生成对话摘要（SummarizationCompactor），保留关键信息后继续会话：

```
⚡ 上下文已压缩 — 12.5K → 6.2K tokens（45 → 22 条消息）
```

---

## 8. 非 TTY / CI 模式

通过管道或 CI 调用时自动退回 CLI REPL 模式：

```bash
$ echo "列出目录下所有 Go 文件" | harness9
```

或交互式 CLI：

```
harness9> 帮我分析 internal/engine/agent_loop.go 的结构
harness9> exit
```

---

## 常见问题

**Q: API Key 不生效？**
确认 `export` 已在当前 shell 中执行，或检查 `.env` 文件是否在正确目录。

**Q: 如何使用 Anthropic Claude 模型？**
设置 `ANTHROPIC_API_KEY` 并将 `LLM_MODEL` 设为 `claude-sonnet-4-6` 等 Claude 模型名称。

**Q: 会话数据存在哪里？**
`~/.harness9/sessions.db`（SQLite），删除该文件将清空所有历史会话。

**Q: 如何完全清空当前会话？**
在 TUI 中输入 `/new` 创建新会话，旧会话数据仍保留，可通过 `/resume` 恢复。
