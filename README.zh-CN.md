# harness9

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.25.3-00ADD8?logo=go)](go.mod)
[![Release](https://img.shields.io/github/v/release/ZhangShenao/harness9)](https://github.com/ZhangShenao/harness9/releases)
[![Docs](https://img.shields.io/badge/docs-website-blue)](https://zhangshenao.github.io/harness9/zh/)

**Local-First · 轻量级 · 功能完备 · 生产可用的通用 Agent 框架**

[English](README.md) | 简体中文

---

![harness9 欢迎界面](welcome.png)

![harness9 对话界面](quickstart.png)

---

## 为什么选择 harness9？

大多数 Agent 框架要么过于臃肿（满屏抽象层、数百个依赖），要么过于简陋（仅能跑个 demo）。harness9 走中间路线：

| 原则 | 说明 |
|---|---|
| **Local-First** | 数据全部存储在本机（SQLite、tool_results、plans），工具在本地 Docker 容器内执行，无云端依赖，代码不离机 |
| **轻量级** | 最小化抽象层，代码直白易读，极少的直接依赖 |
| **功能完备** | 覆盖 Agent 运行所需的全部核心模块 |
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

> 完整安装选项、Anthropic/OpenRouter 配置、AGENTS.md 设置和常见问题，见[快速启动指南](https://zhangshenao.github.io/harness9/zh/docs/quick-start)。

---

## 核心特性

以下每项特性均链接到[官网](https://zhangshenao.github.io/harness9/zh/)上的完整技术方案。

- **[全屏 TUI](https://zhangshenao.github.io/harness9/zh/docs/tui)** —— 基于 Bubbletea，欢迎页/对话页双 Phase、流式输出、实时工具 Spinner、Tab 补全。
- **[Shell 执行（`!` 前缀）](https://zhangshenao.github.io/harness9/zh/docs/shell-execution)** —— 在输入框直接运行 Bash 命令，输出自动注入 LLM 上下文。
- **[Context Engineering](https://zhangshenao.github.io/harness9/zh/docs/context-engineering)** —— SQLite 会话持久化，80% 阈值自动触发 LLM 摘要压缩。
- **[Long-Term Memory](https://zhangshenao.github.io/harness9/zh/docs/long-term-memory)** —— 跨会话记忆持久化到 SQLite + FTS5，MEMORY.md 物化视图实时注入 System Prompt。
- **[Agent Skills](https://zhangshenao.github.io/harness9/zh/docs/agent-skills)** —— Progressive Disclosure：领域知识按需加载，System Prompt 始终精简。
- **[Human-in-the-Loop 权限控制](https://zhangshenao.github.io/harness9/zh/docs/human-in-the-loop)** —— 规则引擎自动评估风险，只有真正需要人类判断的操作才会暂停审批。
- **[Planning 模块](https://zhangshenao.github.io/harness9/zh/docs/planning)** —— Plan Mode 在工具层强制先规划后执行，配合停滞检测。
- **[文件系统能力](https://zhangshenao.github.io/harness9/zh/docs/file-system)** —— OffloadHook 把超大工具输出转存到磁盘，FilePlanWriter 把计划持久化为 markdown。
- **[Sub-Agent 子代理委派](https://zhangshenao.github.io/harness9/zh/docs/sub-agent)** —— 把边界清晰的子任务委派给受限工具集的独立子代理。
- **[Observability](https://zhangshenao.github.io/harness9/zh/docs/eval)** —— OpenTelemetry Span + Metrics 贯穿引擎、LLM 调用与工具执行，开箱支持接入 Langfuse/Grafana/Jaeger。
- **[Test & Eval](https://zhangshenao.github.io/harness9/zh/docs/eval)** —— 确定性 `ScriptedProvider` + 断言体系 + 22 用例黄金数据集，CI 质量门禁。
- **[Sandbox](https://zhangshenao.github.io/harness9/zh/docs/sandbox)** —— 默认在加固过的 Docker 容器内执行所有工具调用，Docker 不可用时自动降级为本地执行。
- **[AutoDev（`/autodev`）](https://zhangshenao.github.io/harness9/zh/docs/autodev)** —— 自举开发闭环：需求澄清 → Spec 确认 → 委派 dev sub-agent 编码、测试、创建 PR。
- **[MCP 工具集成](https://zhangshenao.github.io/harness9/zh/docs/mcp)** —— 通过 `.mcp.json` 接入任意 Model Context Protocol Server，工具透明注入注册表。
- **[网页搜索与抓取](https://zhangshenao.github.io/harness9/zh/docs/web-search)** —— `web_search`/`web_fetch` 工具内置 SSRF 防护，无需任何 API Key。
- **标准 ReAct 循环** —— 每个 Turn 携带完整工具列表调用一次 LLM；并发工具执行 + 自愈能力（工具错误原样回传给 LLM，触发自动重试）。
- **双运行模式** —— 阻塞式 `Run` 与流式 `RunStream` 共享同一引擎实例。

---

## 架构总览

![harness9 整体架构图](harness9_architecture.png)

---

## 核心模块

| 模块 | 说明 |
|---|---|
| **TUI** | 全屏 Bubbletea TUI：双 Phase、流式输出、Spinner + 精确耗时、Tab 补全、Token 用量实时展示、Shell 模式 |
| **Engine** | 标准 ReAct 主循环，阻塞 + 流式双模式，事件流（Token 更新、压缩、工具结果、思维增量） |
| **Hooks** | 工具拦截器：HookRegistry（洋葱模型）+ OffloadHook + FilePlanWriter + DangerHook |
| **Permission** | Human-in-the-Loop：PermissionHook（JSON 规则）+ 五选项审批对话框 + 动态白名单 + 敏感路径硬保护 |
| **Sub-Agent** | 子代理委派：内置 general-purpose 子代理、文件式定义（`.harness9/agents/*.md`）、前台/后台 `task` 工具、`@agent` 直跑 |
| **Planning** | Plan Mode、TodoStore、`todo_write` 工具、工具层权限过滤、自动续跑 + 停滞检测 |
| **Memory** | 会话持久化（SQLite WAL）、SummarizationCompactor（默认）+ TokenBudgetCompactor（回退） |
| **LTM** | 长期记忆存储（SQLite + FTS5）、MEMORY.md 物化视图、Extractor、Phase 3 接缝（Provider/Embedder/Consolidator） |
| **Context** | System Prompt 组装：基础 + AGENTS.md + Skills 索引 + todo/offload/sandbox/LTM 段落 |
| **Skills** | Skills 解析、索引、按需加载（`use_skill` 工具） |
| **Provider** | LLM 统一接口，OpenAI/Anthropic 适配器，实际 Token 用量提取 |
| **Schema** | 跨组件共享的核心数据类型（Message、ToolCall、Usage 等） |
| **Tools** | 工具注册表 + 内置工具（bash、read_file、write_file、edit_file、todo_write、memory_write/search、web_search/web_fetch） |
| **Sandbox** | Docker 容器级隔离：进程沙箱、Agent 级独立容器、孤儿回收；默认开启 |
| **Observability** | OpenTelemetry 链路追踪：贯穿引擎、LLM 调用与工具执行；默认 noop |
| **Evals** | 自动化评估框架、黄金数据集、CI 质量门禁 |
| **MCP** | Model Context Protocol 客户端集成，工具透明注入 |
| **AutoDev** | 自举开发闭环（`/autodev` Skill + dev sub-agent） |
| **Env** | 零依赖 `.env` 加载器 |

---

## 对标框架

| 框架 | 来源 | 与 harness9 的差异 |
|---|---|---|
| DeepAgents | LangChain | Python，图编排（LangGraph StateGraph）；harness9 显式 ReAct 循环，无图引擎依赖，Go 原生 |
| OpenHarness | HKUDS | Python，asyncio 并发；harness9 goroutine 并发模型，Go 原生 |
| OpenCode | Anomaly | TypeScript，委托 Vercel AI SDK streamText，放弃循环控制权；harness9 自持显式循环 |
| OpenClaw | OpenClaw | TypeScript，多代理路由，委托 AI SDK；harness9 Go 原生单 Agent |
| HermesAgent | NousResearch | Python，ThreadPool 并发工具，三级上下文压缩；harness9 goroutine 并发，更轻量 |
| Claude Agent SDK | Anthropic | 官方 SDK，仅支持 Anthropic，黑盒循环；harness9 多 Provider，透明可控 |
| OpenAI Agent SDK | OpenAI | Python，Handoffs 多 Agent，依赖 OpenAI Compaction API；harness9 Go 原生，自持压缩 |

---

## Star History

[![Star History Chart](star-history.svg)](https://github.com/ZhangShenao/harness9/stargazers)

> GitHub 于 2026-06-30 起限制了 stargazers API 的访问权限（仅 owner/collaborator 可读），star-history.com 等第三方实时徽章因此普遍失效。上图由 [`.github/workflows/star-history.yml`](.github/workflows/star-history.yml) 每日用仓库自身 `GITHUB_TOKEN` 离线生成并提交为静态文件，不依赖任何第三方服务的实时可用性。

---

## SWE-bench Benchmark

harness9 在 [SWE-bench Lite](https://github.com/princeton-nlp/SWE-bench) 上评估 Agent 能力，完整方法论与运行步骤见[基准测试技术方案](https://zhangshenao.github.io/harness9/zh/docs/benchmark)。

---

## 文档

完整文档、架构解析与技术博客见[官网](https://zhangshenao.github.io/harness9/zh/)，包括驱动本仓库自身 Agent 的 [AGENTS.md](AGENTS.md) 项目规范。

---

## License

[MIT](LICENSE)
