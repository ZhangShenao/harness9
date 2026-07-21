---
layout: home

hero:
  name: "harness9"
  text: "轻量级 Go Agent Harness 框架"
  tagline: 功能完备、生产可用。极简抽象，代码直白。
  image:
    src: ''
    alt: 'harness9 terminal demo'
  actions:
    - theme: brand
      text: 快速开始 →
      link: /zh/docs/quick-start
    - theme: alt
      text: 查看文档
      link: /zh/docs/tui

features:
  - icon: 🎯
    title: 简洁
    details: 最小化抽象层，代码直白易读，极少的直接依赖。引入 harness9 不意味着引入一套复杂的概念体系。
  - icon: ✅
    title: 完备
    details: 覆盖 Agent 运行所需的全部核心模块——Engine、Provider、Schema、Tools、Env，开箱即用。
  - icon: 🚀
    title: 生产可用
    details: 错误恢复、上下文管理、超时控制、并发工具执行、Path Traversal 防护，不只是 demo。
  - icon: 💻
    title: 全屏 TUI
    details: 流式输出、Spinner 进度、Token 用量实时展示、Tab 补全。AltScreen 全屏渲染，非 TTY 自动退回 CLI。
  - icon: ⚡
    title: Shell 执行
    details: 输入框 ! 前缀直接运行 Bash 命令，输出追加到对话流并自动注入 LLM 上下文，无需切换终端。
  - icon: 🧠
    title: Context Engineering
    details: LLM 摘要压缩、SQLite 会话持久化、80% 阈值自动触发，长对话不丢语义。
  - icon: 🗃️
    title: Long-Term Memory
    details: 跨会话长期记忆，SQLite + FTS5 持久化，MEMORY.md 物化视图实时注入，显式工具 / 压缩前提取 / nudge 三路触发，零新增依赖。
  - icon: 📋
    title: Planning 模块
    details: Plan Mode 先规划后执行，TodoStore 状态机校验，工具层权限过滤，自动续跑 + 停滞检测。
  - icon: 🔀
    title: 并发工具执行
    details: 同 Turn 内多工具并发执行，每工具独立超时控制。执行失败时错误原样回传 LLM，触发自动重试。
  - icon: 💡
    title: 推理内容展示
    details: Anthropic extended thinking 和 OpenRouter reasoning_content 均路由为 EventThinkingDelta，TUI 流式渲染深灰色思维块。
---

## 架构总览

![harness9 整体架构图](/harness9_architecture.png)

---

## 快速开始

### 安装

```bash
curl -fsSL https://raw.githubusercontent.com/ZhangShenao/harness9/master/scripts/install.sh | bash
```

### 配置 API Key

```bash
# OpenAI / OpenRouter
export OPENAI_API_KEY="sk-..."

# 或 Anthropic
export ANTHROPIC_API_KEY="sk-ant-..."
```

### 启动

```bash
cd /your/project && harness9
```

> 在 TTY 环境中自动进入全屏 TUI；管道/CI 环境退回 CLI REPL 模式。
>
> 更多配置选项见 [快速启动指南](/zh/docs/quick-start)。
