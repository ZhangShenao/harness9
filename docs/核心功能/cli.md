# harness9 CLI 使用指南

harness9 默认以**交互式终端 Agent（CLI REPL）**模式运行，直接在终端与 Agent 对话。

---

## 安装

### 一键安装（推荐）

```bash
curl -fsSL https://raw.githubusercontent.com/ZhangShenao/harness9/master/scripts/install.sh | bash
```

安装完成后验证：

```bash
harness9 --version
# harness9 v0.1.0
```

---

## 升级

已安装 harness9 的用户可直接通过内置 `upgrade` 命令升级到最新版本：

```bash
harness9 upgrade
```

升级过程：

1. 查询 GitHub Releases API，获取最新版本号
2. 与当前版本比较，若已是最新则退出
3. 下载适用于当前平台（OS / 架构）的压缩包
4. SHA256 校验（确保完整性）
5. 解压并原子替换当前可执行文件

示例输出：

```
正在检查最新版本...
发现新版本：v0.1.1（当前：0.1.0）
正在下载 harness9_0.1.1_darwin_arm64.tar.gz...
正在校验 SHA256...
正在解压...
升级成功：harness9 v0.1.1
```

> **注意**：若 harness9 安装在需要 root 权限的目录（如 `/usr/local/bin`），升级时可能提示权限错误，请使用 `sudo harness9 upgrade`。

### 从源码构建（开发者）

```bash
git clone https://github.com/ZhangShenao/harness9
cd harness9
go run ./cmd/harness9
```

---

## 快速启动

```bash
# 设置 API Key（推荐写入 ~/.zshrc 或 ~/.bashrc）
export OPENAI_API_KEY="sk-..."

# 进入你的项目目录（harness9 以此目录为工作沙箱）
cd /your/project

# 启动
harness9
```

启动后看到提示符即表示 Agent 就绪：

```
harness9 │ 输入 "exit" 或按 Ctrl-C 退出

harness9>
```

---

## 环境变量

| 变量 | 必填 | 默认值 | 说明 |
|------|:----:|--------|------|
| `OPENAI_API_KEY` | ✅ | — | LLM Provider API Key |
| `LLM_MODEL` | ❌ | `openai/gpt-4o-mini` | 模型名称，支持任意 OpenAI 兼容模型 |
| `OPENAI_BASE_URL` | ❌ | OpenAI 官方地址 | 自定义 API 地址，可接入 OpenRouter / Azure / 本地模型 |

### 加载规则

harness9 启动时从**工作目录（即执行 `harness9` 命令时所在的目录）**读取 `.env` 文件。  
手动通过 `export` 或 shell 配置文件设置的变量**始终优先**于 `.env` 文件中的同名变量。

**优先级：** `export VAR=value` > `work_dir/.env` 文件

### 方式一：手动 export（推荐，全局生效）

写入 `~/.zshrc` 或 `~/.bashrc`，对所有项目生效，无需在每个目录放置 `.env`：

```bash
export OPENAI_API_KEY="sk-..."
export LLM_MODEL="openai/gpt-4o-mini"
# export OPENAI_BASE_URL="https://openrouter.ai/api/v1"
```

### 方式二：.env 文件（项目级配置）

在项目根目录放置 `.env` 文件，仅对当前项目生效：

```env
OPENAI_API_KEY=sk-...

# 可选：切换模型
LLM_MODEL=openai/gpt-4o-mini

# 可选：使用 OpenRouter 或其他兼容 API
# OPENAI_BASE_URL=https://openrouter.ai/api/v1
```

> **注意：** `.env` 文件包含 API Key 等敏感信息，建议将其加入 `.gitignore`，避免提交到代码仓库。

---

## 对话与操作

### 基本对话

在 `harness9>` 提示符后输入任何问题或指令，Agent 将在启动目录下执行任务：

```
harness9> 列出当前目录下的所有 Go 文件
harness9> 帮我分析 internal/engine/agent_loop.go 的结构
harness9> 在 main.go 里添加一个 --version 标志
```

### 退出

| 方式 | 说明 |
|------|------|
| 输入 `exit` | 正常退出 |
| 输入 `quit` | 正常退出 |
| `Ctrl-C` | 发送取消信号，Agent 完成当前操作后退出 |
| `Ctrl-D`（EOF）| 正常退出 |

### 空行

直接按 Enter 跳过，不触发 Agent。

---

## 工作目录

harness9 以**启动时的进程工作目录（cwd）**作为 Agent 的沙箱边界，无需任何配置：

```bash
cd /your/project
harness9   # 沙箱根目录 = /your/project
```

所有文件读写、命令执行均被限制在此目录内：

- `read_file`、`write_file`、`edit_file` 工具会拒绝访问启动目录以外的路径
- `bash` 工具的工作目录也被设定为启动目录
- 路径穿越攻击（`../../etc/passwd`）被自动拦截

---

## Project Guidelines (AGENTS.md)

在项目根目录放置 `AGENTS.md` 文件，Agent 启动时会自动将其内容注入 System Prompt，作为项目级规范和上下文指南。

**典型用途：**
- 描述项目架构、技术栈、编码规范
- 指定禁止操作（如"禁止修改 go.mod"）
- 提供领域背景知识

**格式：** 标准 Markdown，无格式限制。

```markdown
# 我的项目指南

## 技术栈
- Go 1.25
- PostgreSQL 16

## 规范
- 所有函数必须有注释
- 禁止直接操作数据库，必须通过 Repository 层
```

---

## Agent Skills

Skills 是可按需加载的领域知识文档。在项目根目录的 `skills/` 子目录下放置 `.md` 文件即可：

```
your-project/
├── skills/
│   ├── refactor-guide.md      # 重构规范
│   ├── testing-standards.md   # 测试标准
│   └── api-design.md          # API 设计规范
└── AGENTS.md
```

**Skill 文件格式（frontmatter + 内容）：**

```markdown
---
name: refactor-guide
description: Use when refactoring Go code — explains team conventions
trigger: refactor, clean up, restructure
---

# 重构规范

重构 Go 代码时，请遵循以下原则：
1. 先运行 `go vet`，修复所有 warning
2. 保持函数不超过 50 行
...
```

**frontmatter 字段：**

| 字段 | 必填 | 说明 |
|------|:----:|------|
| `name` | ✅ | Skill 唯一名称，供 `use_skill` 工具调用 |
| `description` | ✅ | 简短描述，注入 System Prompt 供 Agent 感知 |
| `trigger` | ❌ | 触发关键词（仅文档说明，不做自动匹配） |

**工作机制（Progressive Disclosure）：**

1. 启动时，所有 Skill 的 `name` 和 `description` 注入 System Prompt 形成索引
2. Agent 需要时调用 `use_skill` 工具，按需加载指定 Skill 的完整内容
3. 全文内容不预先注入，节省 Token，保持 System Prompt 精简

Agent 在 System Prompt 中会看到类似：

```
## Available Skills

Use the `use_skill` tool to load the full content of any skill when needed.

- refactor-guide: Use when refactoring Go code — explains team conventions
- testing-standards: Use when writing or reviewing tests
```

详见 [Agent Skills 设计原理](agent-skills.md)。

---

## 使用示例

### 代码分析

```
harness9> 帮我分析 internal/engine/agent_loop.go，说明它的主循环设计
```

### 代码修改

```
harness9> 在 internal/tools/ 下新建一个 list_dir 工具，列出目录内容
```

### 使用 Skill

```
harness9> 帮我重构 internal/provider/openai.go（Agent 会自动加载 go-coding-standards Skill）
```

### 运行测试

```
harness9> 帮我运行 go test ./... 并分析失败原因
```

---

## 常见问题

**Q: 启动报错 `创建 Provider 失败`**

检查 `OPENAI_API_KEY` 是否正确设置，以及 `.env` 文件是否在运行命令的目录下。

**Q: Agent 无法读取某个文件**

确认文件路径在启动目录内。Agent 使用相对路径，绝对路径或 `../` 路径穿越会被拦截。

**Q: 想使用其他模型（如 Claude、OpenRouter）**

设置 `OPENAI_BASE_URL` 指向兼容 OpenAI Chat Completions API 的端点，并将 `LLM_MODEL` 改为对应模型名称：

```env
OPENAI_BASE_URL=https://openrouter.ai/api/v1
LLM_MODEL=anthropic/claude-sonnet-4-5
```

**Q: 每次对话是否有记忆？**

有。harness9 在同一进程会话内维护完整的对话历史（通过 `memory.Session` + SQLite 持久化），每次 `harness9>` 输入都追加到当前会话上下文，Agent 可引用之前所有轮次的对话和工具结果。进程重启后需通过 TUI 的 `/resume` 命令或在代码中显式调用 `mgr.OpenSession(id)` 恢复历史会话；CLI REPL 启动时始终创建新会话。
