# Agent Harness 框架网页读取能力技术调研报告

> 调研日期：2026-06-11
> 调研范围：DeepAgents、OpenHarness、OpenCode、OpenClaw、HermesAgent、Claude Agent SDK
> 调研方法：直接访问各框架 GitHub 仓库源码、官方文档及 Context7 API 文档

---

## 1. 调研背景与范围

网页读取（Web Fetching/Browsing）能力是现代 Agent Harness 框架中争议最大、实现差异最显著的能力之一。与文件系统工具或 Shell 执行不同，网页能力涉及多个技术层的协同：HTTP 请求层、HTML 解析层、内容转换层、搜索引擎后端层，以及由此引发的安全、上下文溢出等工程问题。

本报告系统梳理上述六个框架在网页读取领域的完整实现，聚焦以下五个维度：
1. Tool-Calling 设计（工具定义、HTML 处理、截断与错误处理）
2. Sub-Agent 调度（是否引入专门的 Web Sub-Agent）
3. 深度搜索（Deep Search）vs 广度搜索（Breadth Search）
4. 搜索引擎后端集成（支持的后端、API Key 管理、Fallback 策略）
5. 上下文管理（大量网页内容的溢出处理）

---

## 2. 各框架实现概述

### 2.1 DeepAgents (LangChain)

**项目定位**：Python + TypeScript，"batteries-included" Agent Harness，以 LangGraph 为编排基础。Stars：24,444（截至调研日）。

**网页能力定位**：无内置网页工具，完全采用 MCP 生态外接。

**实现分析**：

DeepAgents 的内置工具集仅包含文件操作（`ls`/`read_file`/`write_file`/`edit_file`/`glob`/`grep`）、任务管理（`write_todos`）、Shell 执行（`execute`）和子代理委派（`task`）。源码文件 `graph.py` 和 `_tools.py` 中均无任何 URL 获取、HTML 解析或搜索引擎调用的痕迹。

网页能力的提供方式是外部 MCP 服务器。官方文档（Talon 运行时的 README）中仅有一处提到 Tavily：
> "Tavily or other search tools receive query strings chosen by the model and may include conversation-derived values."

这意味着 DeepAgents 不对网页工具的具体实现做任何约束，用户通过 `tools.json` 配置 HTTP MCP 服务器（如 Tavily MCP、Brave MCP 等）来获得网页能力。

**Tools 定义方式**：外接 MCP，不内置。

**上下文管理**：`middleware/summarization.py` 实现了对话历史的 LLM 压缩（`ContextOverflowError` 触发自动压缩 + 参数截断），但不对网页内容做专门处理。

**设计决策分析**：DeepAgents 的哲学是"极简内核 + MCP 生态"，将网页能力委托给已有的 MCP 服务器生态，避免在框架层硬编码特定搜索引擎，保持了极高的灵活性，但也意味着开箱即用的网页能力为零。

---

### 2.2 OpenHarness (HKUDS)

**项目定位**：Python，"Open Agent Harness with a Built-in Personal Agent (Ohmo)"。Stars：13,757。

**网页能力定位**：内置两个独立工具 `web_search` 和 `web_fetch`，设计简洁，直接内置 DuckDuckGo 作为搜索后端。

**工具定义**

**`web_search` 工具：**

```python
# Tool Name: web_search
# Purpose: Search the web and return compact top results with titles, URLs, and snippets

class WebSearchToolInput(BaseModel):
    query: str                          # required: search terms
    max_results: int = Field(default=5, ge=1, le=10)  # 1-10 results
    search_url: Optional[str] = None    # override backend URL
```

**`web_fetch` 工具：**

```python
# Tool Name: web_fetch
# Purpose: Fetch one web page and return compact readable text

class WebFetchToolInput(BaseModel):
    url: str           # HTTP/HTTPS URL to retrieve
    max_chars: int = Field(default=12000, ge=500, le=50000)  # character limit
```

**HTML 解析实现**：

自实现 `_HTMLTextExtractor`（继承 `HTMLParser`），策略如下：
- 跳过 `<script>` 和 `<style>` 标签（使用深度计数器追踪嵌套）
- 提取非空白文本内容
- 不使用正则处理标签（注释中明确说明"避免 pathological regex behavior"）

**内容截断**：超出 `max_chars` 时直接截断并追加截断标记。默认 12,000 字符，最大 50,000 字符。

**搜索后端**：
- 默认：DuckDuckGo HTML 端点（`https://html.duckduckgo.com/html/`）
- 可通过环境变量 `OPENHARNESS_WEB_SEARCH_URL` 自定义后端
- 单次请求可传入 `search_url` 参数覆盖

**URL 解析**：`_normalize_result_url()` 处理 DuckDuckGo 的重定向链接，从 `uddg` 参数提取真实目标 URL。

**HTTP 请求配置**：
- `web_search`：超时 20 秒，自定义 User-Agent `OpenHarness/0.1.7`
- `web_fetch`：超时 15 秒，最多 5 次重定向，自定义 User-Agent

**安全机制**：网络守卫（NetworkGuard）预校验 URL，所有外部内容返回时附带"不可信来源"警告横幅。

**Sub-Agent 支持**：有 `agent_tool.py`，但网页能力不通过 Sub-Agent 委派——`web_search` 和 `web_fetch` 作为顶层工具直接暴露给主 Agent。

**深度搜索**：不支持，仅单次搜索 + 单页抓取，由 LLM 决策是否继续翻页或跟踪链接。

**上下文管理**：无专门机制，依赖 `max_chars` 截断控制单次内容量。

---

### 2.3 OpenCode (Anomaly)

**项目定位**：TypeScript，Monorepo，"The open source coding agent"，主攻编码场景。Stars：173,001。

**网页能力定位**：暂未在 `packages/core` 或主包中发现内置网页工具。`packages/` 目录中有 `http-recorder` 包，但主要用于记录 HTTP 交互而非提供搜索能力。

**实现分析**：

仓库结构以编码功能（LSP 集成、代码编辑、终端执行）为核心，当前版本不包含独立的 `web_search` 或 `web_fetch` 工具。网页能力可通过 MCP 服务器接入（OpenCode 支持 MCP 协议）。

`packages/plugin` 目录提供插件机制，理论上可以扩展网页工具，但官方未提供开箱即用的实现。

**设计决策分析**：OpenCode 定位为编码专用 Agent，与 DeepAgents 类似采用"核心极简，通过插件或 MCP 扩展"的思路。网页能力不在其核心关注点内。

---

### 2.4 OpenClaw (OpenClaw)

**项目定位**：TypeScript，"Your own personal AI assistant"，全能个人助手，跨平台。Stars：378,109，是调研框架中 Stars 最多的。

**网页能力定位**：设计最为完整，采用插件化架构，`web_fetch` 和 `web_search` 各自独立，支持 200+ 插件扩展，支持多搜索后端的级联 Fallback。

**整体架构**：

```
src/
├── web-fetch/          # web_fetch 运行时（provider 解析 + 工具定义）
│   ├── runtime.ts      # provider 选择、credential 管理、工具创建
│   └── content-extractors.runtime.ts  # 内容提取器注册表（插件式）
├── web-search/         # web_search 运行时（multi-provider cascading fallback）
│   ├── runtime.ts      # provider 级联 fallback 逻辑
│   └── runtime-types.ts  # TypeScript 类型定义

extensions/             # 各搜索/抓取后端的独立插件
├── brave/              # Brave Search API
├── tavily/             # Tavily Search + Extract
├── exa/                # Exa Search API
├── firecrawl/          # Firecrawl（搜索 + 网页抓取）
├── web-readability/    # Mozilla Readability（客户端 HTML 提取）
└── ...（200+ 其他插件）
```

**HTML 解析实现（web-readability 插件）**：

技术栈：`linkedom`（DOM 解析）+ `@mozilla/readability`（主内容提取）+ `htmlToMarkdown`（Markdown 转换）。

```typescript
// web-content-extractor.ts
import { parseHTML } from "linkedom";
import { Readability } from "@mozilla/readability";

// Two extraction modes
// - text mode: stripInvisibleUnicode() + normalizeWhitespace() on parsed text
// - markdown mode: htmlToMarkdown() then stripInvisibleUnicode()

// Size guard: reject HTML > 1,000,000 chars
// Depth guard: reject documents with nesting depth > 3,000
```

**搜索后端 Fallback 策略**：

```
Resolution order for web_search:
1. Explicit providerId parameter
2. search.provider from OpenClawConfig
3. Auto-detect: credential-backed providers (sorted by priority)
4. Keyless providers (last resort, e.g. free tier)
5. Throw error if all candidates exhausted

Structured error handling:
- Providers returning /^missing_[a-z0-9_]*api_key$/i are treated
  as "unavailable" during auto-detected fallback, not as failures
```

**支持的搜索/抓取后端**：

| 插件 | 类型 | 特性 |
|------|------|------|
| Brave | search-only | 结构化结果 + 国家/语言/时间过滤 |
| Tavily | search + fetch | AI 答案摘要，domain 过滤 |
| Exa | search | 语义搜索优化 |
| Firecrawl | search + fetch | 深度网页抓取，支持 markdown/html/text |
| web-readability | fetch only | 本地 Mozilla Readability，无 API 依赖 |
| DuckDuckGo | search-only | 免费，无需 key |

**Firecrawl 抓取参数**：

```typescript
interface FetchFirecrawlContentParams {
    url: string;
    extractMode: "markdown" | "text";
    apiKey: string;
    baseUrl: string;
    onlyMainContent: boolean;
    maxAgeMs: number;           // cache duration
    proxy: "auto" | "basic" | "stealth";  // anti-detection proxy mode
    storeInCache: boolean;
    timeoutSeconds: number;
    maxChars?: number;
}
```

**API Key 管理**：多路径查找，优先级从高到低：
1. OpenClawConfig 中的显式配置值
2. 配置 fallback 值
3. 环境变量（如 `BRAVE_API_KEY`、`TAVILY_API_KEY`）
4. Agent 目录中的 auth profiles

**Sub-Agent 调度**：无专用 Web Sub-Agent，工具直接暴露给主 Agent。

**深度搜索**：无内置实现，由 LLM 通过多次工具调用实现搜索树遍历。

**上下文管理**：依赖 `maxChars` 参数控制单次内容量，无专门压缩策略。

---

### 2.5 HermesAgent (NousResearch)

**项目定位**：Python，"The agent that grows with you"，全能个人助手，Stars：190,498，工具集最为丰富。

**网页能力定位**：设计最复杂。分为三个层级：
1. **轻量级 HTTP 工具**：`web_search` + `web_extract`（`tools/web_tools.py`）
2. **重量级浏览器自动化**：基于 CDP 的 `browser_*` 工具集（`tools/browser_tool.py`，164KB）
3. **隐身浏览**：Camoufox Firefox 包装器（`tools/browser_camofox.py`）

#### 工具集定义

**Toolset 分组**（来自 `toolsets.py`）：

| Toolset 名 | 包含工具 | 适用场景 |
|-----------|---------|---------|
| `web` | `web_search` + `web_extract` | Web 研究和内容提取 |
| `search` | `web_search` | 仅搜索，不抓取 |
| `browser` | `browser_navigate` + `browser_snapshot` + `browser_click` + `browser_type` + `browser_scroll` + `browser_back` + `browser_press` + `browser_get_images` + `browser_vision` + `browser_console` + `browser_cdp` + `browser_dialog` + `web_search` | 完整浏览器自动化 |

**`web_search` 工具参数**：
```python
web_search(query: str, limit: int = 5)  # returns JSON: title, URL, description, position
```

**`web_extract` 工具参数**：
```python
web_extract(
    urls: List[str],           # up to 5 URLs
    format: str = "markdown",  # markdown/html/text
    use_llm_processing: bool = True,
    model: Optional[str] = None  # defaults to Gemini 3 Flash Preview via OpenRouter
)
```

#### 后端系统

**支持的搜索/抓取后端**（`plugins/web/` 目录）：

| 后端 | 类型 | 备注 |
|------|------|------|
| Exa | search + extract | 语义搜索 |
| Firecrawl | search + extract | 通过 Nous Tool Gateway 代理 |
| Parallel | search + extract | 免费 MCP 层（始终可用，作为 keyless fallback） |
| Tavily | search + extract | 标准 REST API，max 20 结果 |
| SearXNG | search-only | 自托管元搜索 |
| Brave | search-only | 免费版限制较多 |
| DuckDuckGo | search-only | 免费 |
| xAI | search | X.AI 搜索 |

**后端选择优先级**：
```
1. 显式配置：config.yaml 中的 web.backend / web.search_backend / web.extract_backend
2. 环境变量：TAVILY_API_KEY / EXA_API_KEY / FIRECRAWL_API_KEY 等
3. Nous 工具网关（订阅用户）
4. Parallel 免费 MCP（keyless fallback，始终可用）
```

注意："explicit user credentials beat the managed-tool-gateway probe"，防止配置错误。

#### LLM 内容摘要管线（核心差异化能力）

`web_extract` 提取内容后，若内容超过阈值，触发 LLM 压缩：

```
内容 < 5,000 chars  → 直接返回，不压缩
内容 5K~500K chars → 单次 LLM 摘要（Gemini 3 Flash Preview via OpenRouter）
内容 500K~2M chars → 分块：每 100K 字符一个 chunk，asyncio.gather() 并行摘要 → 合成
内容 > 2M chars    → 拒绝处理，返回大小警告

最终输出：上限约 5,000 字符的摘要
降级策略：LLM 超时 → 返回原始内容前 5,000 字符
```

**多 URL 并行处理**：

```python
# web_extract 接受最多 5 个 URL
# 每个 URL 独立进行安全检验（SSRF + embedded secret 检测）
# asyncio.gather() 并行抓取和压缩
```

#### 浏览器自动化系统（重量级）

**三种浏览器后端**：
- **Local Chromium**（默认，零成本）：通过 `agent-browser` CLI 调用
- **Browserbase**（云端）：CDP over WebSocket
- **Browser Use**（云端）：托管 API

**核心技术**：Accessibility Tree（ARIA Snapshot）而非 DOM 截图。

```
browser_snapshot() 返回 ARIA 可访问性树文本表示（而非像素）
当 snapshot > 8,000 字符时：
  - 触发 LLM 内容提取（保留元素 ref ID，如 @e1、@e2）
  - LLM 失败时回退：按行边界截断，显示省略行数
```

**超时配置**：
- 命令超时：30 秒（`config["browser"]["command_timeout"]`）
- 会话不活跃超时：300 秒

**CDP 工具**（`browser_cdp_tool.py`）：

原始 CDP 协议通道，作为"逃生舱"（escape hatch）。支持 `Target.*`、`Page.*`、`Runtime.*`、`DOM.*`、`Network.*` 等域，通过 WebSocket 多路复用。

**Camoufox 隐身浏览器**（`browser_camofox.py`）：

基于 Firefox 的 C++ 指纹伪造浏览器，通过本地 REST API 提供自动化能力。适用于需要规避反爬检测的场景。

**SSRF 防护**（`tools/url_safety.py`）：

```python
# is_safe_url() 实现：
# 1. 验证 HTTP/HTTPS scheme
# 2. DNS 解析，错误时 fail-closed
# 3. 检查 resolved IP 是否在禁止范围内

# 永远禁止（不可配置）：
# - AWS metadata（169.254.169.254）
# - Azure IMDS、GCP metadata
# - link-local 段 169.254.0.0/16

# 默认禁止（可通过 security.allow_private_urls=true 关闭）：
# - RFC1918 私网地址
# - loopback（127.x.x.x）
# - CGNAT（100.64.0.0/10）
```

#### Sub-Agent 支持

通过 `delegate_tool.py` 支持任务委派，但网页能力本身不强制走 Sub-Agent 路径。Toolset 设计允许为 Sub-Agent 分配专门的 Web Toolset：

```yaml
# 示例：主 Agent 分配 web toolset，Sub-Agent 专注 browser toolset
main_agent:
  toolset: web + browser
sub_agent_research:
  toolset: web
sub_agent_interaction:
  toolset: browser
```

---

### 2.6 Claude Agent SDK (Anthropic)

**项目定位**：Python + TypeScript，"Build production AI agents with Claude Code as a library"。使用 Claude Code 的完整能力作为库。

**网页能力定位**：内置两个官方工具 `WebSearch` 和 `WebFetch`，与 Claude Code 共用相同底层实现，通过 `allowed_tools` 显式激活。

**工具定义**（来自官方文档）：

| 工具名 | 功能描述 |
|--------|---------|
| `WebSearch` | 搜索 Web 获取当前信息 |
| `WebFetch` | 抓取并解析网页内容 |

**激活方式**：

```python
# Python
from claude_agent_sdk import query, ClaudeAgentOptions

async for message in query(
    prompt="Search for the latest Go 1.25 release notes",
    options=ClaudeAgentOptions(
        allowed_tools=["WebSearch", "WebFetch"]
    ),
):
    print(message)
```

```typescript
// TypeScript
import { query } from "@anthropic-ai/claude-agent-sdk";

for await (const message of query({
  prompt: "Search for the latest Go 1.25 release notes",
  options: { allowedTools: ["WebSearch", "WebFetch"] }
})) {
  console.log(message);
}
```

**实现细节**：工具底层实现与 Claude Code CLI 完全共享，具体实现为 Anthropic 内部封闭逻辑（搜索后端未公开披露）。工具设计遵循最小接口原则——调用方只需指定工具名称，参数（URL、查询词）由 Claude 决策传入。

**Sub-Agent 与 Web 工具的组合**：

官方提供了 `research-agent` 示例（`anthropics/claude-agent-sdk-demos`），展示了多 Sub-Agent 并行研究模式：

```python
# research_agent/agent.py
options=ClaudeAgentOptions(
    agents={
        "researcher": AgentDefinition(
            description="Use for gathering research information on any topic",
            prompt=researcher_prompt,    # 从 researcher.txt 加载
            tools=["WebSearch", "Bash", "Read", "Write", "Glob", "Grep"],
            model="claude-haiku-4-5",   # 低成本模型执行搜索
        ),
        "data-analyst": AgentDefinition(
            description="Use for processing numerical findings",
            tools=["Bash", "Read", "Write"],
        ),
        "report-writer": AgentDefinition(
            description="Use for synthesizing findings into PDF reports",
            tools=["Bash", "Read", "Write"],
        ),
    }
)
```

**架构模式**：主 Agent（协调者，使用 Claude Haiku 降低成本）→ 研究 Sub-Agent（`WebSearch` 权限）→ 分析 Sub-Agent → 报告 Sub-Agent。研究结果写入 `files/research_notes/`，通过文件系统在 Sub-Agent 间传递。

**MCP 扩展**：Claude Agent SDK 可通过 MCP 接入 Playwright 等浏览器自动化服务：

```python
options=ClaudeAgentOptions(
    mcp_servers={
        "playwright": {"command": "npx", "args": ["@playwright/mcp@latest"]}
    }
)
```

**上下文管理**：Claude Agent SDK 自动处理 context 压缩（与 Claude Code 相同机制），大量网页内容会触发内置 summarization，开发者无需手动管理。

---

## 3. 各框架实现对比表

### 3.1 核心能力矩阵

| 维度 | DeepAgents | OpenHarness | OpenCode | OpenClaw | HermesAgent | Claude Agent SDK |
|------|-----------|-------------|----------|---------|-------------|-----------------|
| **内置 web_search** | 否（MCP） | 是 | 否（MCP） | 是（插件式） | 是 | 是 |
| **内置 web_fetch** | 否（MCP） | 是 | 否（MCP） | 是（插件式） | 是 | 是 |
| **浏览器自动化** | 否 | 否 | 否 | 是（插件） | 是（CDP + Playwright） | 是（MCP） |
| **HTML → Markdown** | N/A | 否（纯文本） | N/A | 是（Mozilla Readability + htmlToMarkdown） | 是（format 参数） | 未公开 |
| **SSRF 防护** | N/A | 是（NetworkGuard） | N/A | 是（credential 检测） | 是（async_is_safe_url） | 是（内置） |
| **多后端 Fallback** | N/A | 部分（环境变量切换） | N/A | 是（自动级联 fallback） | 是（4 级 fallback） | N/A（单一实现） |
| **LLM 内容压缩** | 否 | 否 | 否 | 否（maxChars 截断） | 是（Gemini 3 Flash via OpenRouter） | 是（内置，自动触发） |
| **深度搜索模式** | 否 | 否 | 否 | 否 | 否（工具层面无，Agent 层面可实现） | 否（工具层面无） |
| **Sub-Agent Web 专用** | 否 | 否 | 否 | 否 | 否（但支持 Toolset 分组） | 是（research-agent 示例） |
| **搜索引擎数量** | 0（全 MCP） | 1（DuckDuckGo） | 0（全 MCP） | 5+（插件） | 8（Exa/Firecrawl/Tavily/Parallel/SearXNG/Brave/DDG/xAI） | 未公开（Anthropic 内部） |
| **开箱即用** | 否 | 是 | 否 | 是 | 是 | 是 |
| **主要实现语言** | Python | Python | TypeScript | TypeScript | Python | Python + TypeScript |

### 3.2 工具参数设计对比

| 框架 | 搜索工具参数 | 抓取工具参数 | 返回格式 |
|------|------------|------------|---------|
| OpenHarness | `query`, `max_results`(1-10), `search_url` | `url`, `max_chars`(500-50000, 默认 12000) | 纯文本（数字编号列表 / 截断文本） |
| OpenClaw | 依后端而定（由 provider 定义） | `url`, `extractMode`(markdown/text), provider-specific 参数 | Markdown 或纯文本 |
| HermesAgent | `query`, `limit`(max 20, 默认 5) | `urls`(max 5), `format`(markdown/html/text), `use_llm_processing`, `model` | JSON 结构化结果 / Markdown |
| Claude Agent SDK | 由 Claude 决策 | 由 Claude 决策 | 由底层实现决定，开发者透明 |

### 3.3 搜索后端支持对比

| 后端 | OpenHarness | OpenClaw | HermesAgent | Claude Agent SDK |
|------|------------|---------|-------------|-----------------|
| DuckDuckGo | 是（默认） | 是（插件） | 是（插件） | 未公开 |
| Brave | 否 | 是（插件） | 是（插件） | 未公开 |
| Tavily | 否 | 是（插件） | 是（插件） | 未公开 |
| Exa | 否 | 是（插件） | 是（插件） | 未公开 |
| Firecrawl | 否 | 是（插件） | 是（插件） | 未公开 |
| SearXNG | 否 | 否（未见插件） | 是（插件） | 未公开 |
| Google/Bing | 否 | 否 | 否 | 未公开 |
| Keyless Fallback | 否 | 是（自动降级） | 是（Parallel MCP） | N/A |

---

## 4. 核心设计决策分析

### 4.1 内置 vs MCP 外接

**两种极端策略**：

- **内置派**（OpenHarness、HermesAgent）：将 web_search 和 web_fetch 直接实现为框架核心工具，开箱即用，但与特定后端强耦合（OpenHarness → DuckDuckGo）。
- **MCP 外接派**（DeepAgents、OpenCode）：框架核心不包含网页工具，通过 MCP 协议接入外部搜索服务，极度灵活但缺乏开箱即用体验。
- **插件化混合派**（OpenClaw、HermesAgent）：内置工具接口定义和 Fallback 逻辑，具体后端通过插件扩展，兼顾开箱即用与灵活性。

**权衡分析**：

| 策略 | 优势 | 劣势 |
|------|------|------|
| 内置 | 零配置，简单直接 | 强耦合特定后端，维护成本高 |
| MCP 外接 | 极度灵活，与框架核心解耦 | 用户需自行配置，学习曲线陡 |
| 插件化混合 | 兼顾两者 | 架构复杂度最高，代码量大 |

### 4.2 HTML 解析策略

**三种主流方法**：

1. **纯文本提取**（OpenHarness）：自实现 HTML Parser，跳过 script/style，提取可见文本。优点：无第三方依赖，行为可预期。缺点：丢失文档结构。

2. **Mozilla Readability + Markdown 转换**（OpenClaw）：业界最佳实践，`linkedom` 解析 DOM，Readability 提取主内容，`htmlToMarkdown` 转 Markdown。优点：保留语义结构，Markdown 对 LLM 友好。缺点：依赖链较重。

3. **外部服务抓取**（HermesAgent via Firecrawl）：将 HTML 处理外包给专业服务（Firecrawl），框架本身不做 HTML 解析。优点：质量高，支持 JS 渲染页面。缺点：依赖外部 API，有成本。

**结论**：Markdown 格式的内容对 LLM 最友好（保留标题层级、链接、代码块等语义），建议优先选择 Readability + Markdown 路径。

### 4.3 内容截断 vs LLM 压缩

**字符截断**（OpenHarness、OpenClaw）：
- 实现简单，无额外 API 调用
- 可能截断关键信息
- 适合轻量场景

**LLM 压缩**（HermesAgent）：
- 保留语义，压缩率更高
- 引入额外 LLM 调用成本（HermesAgent 默认使用 Gemini Flash，成本低廉）
- 支持超大文档（chunked pipeline 处理 2M 字符内的内容）
- 输出有边界（约 5,000 字符），可控性好

**HermesAgent 的压缩参数设计值得参考**：

```
5K chars 阈值：低于此值不压缩（避免小内容的压缩损耗）
5,000 chars 输出上限：与普通对话消息体量相当，不会引发 context 压力
500K 分块阈值：超出此值进入并行分块处理
2M 字符拒绝阈值：防止异常大文档导致无限处理
```

### 4.4 深度搜索的实现路径

调研的六个框架均**没有在工具层面实现深度搜索**（递归链接追踪、搜索树构建、结果聚合去重）。这是一个明确的行业共识：

**深度搜索 = Agent 行为，不是工具行为**

工具层只提供原子能力：
- `web_search(query)` → 一批结果链接
- `web_fetch(url)` → 单页内容

深度研究的决策逻辑（"是否跟踪这个链接"、"是否需要扩展搜索词"、"是否已获得足够信息"）由 LLM 在 Agent Loop 中完成。

Claude Agent SDK 的 `research-agent` 示例展示了标准模式：
- 主 Agent（协调者）负责规划研究路径
- 研究 Sub-Agent 执行 WebSearch + WebFetch
- 结果写入文件系统，作为 Sub-Agent 间的通信媒介
- 分析 Sub-Agent 处理数值数据
- 报告 Sub-Agent 生成最终输出

**Deep Research 模式（Depth-First）**：LLM 决定深挖某一页面，对该页面内的链接递归调用 `web_fetch`。

**Breadth Research 模式（Breadth-First）**：LLM 决定扩展搜索词，多次调用 `web_search`，收集更多候选 URL，再统一抓取。

两种模式的切换完全在 LLM 的 System Prompt 层面控制，无需工具层支持。

### 4.5 SSRF 与安全防护

HermesAgent 实现了最完整的 SSRF 防护（`tools/url_safety.py`），体现了以下最佳实践：

1. **硬编码永久禁止**：云厂商 metadata 端点（AWS/Azure/GCP），即使用户配置 `allow_private_urls=true` 也不放行
2. **DNS 解析后检查**：先 DNS 解析 hostname 到 IP，再检查 IP 段，防止 DNS rebinding 攻击的首次绕过
3. **Fail-closed**：DNS 解析失败时，拒绝访问（保守策略）
4. **重定向重检**：跟踪重定向后对最终 URL 重新校验
5. **Embedded Secret 检测**：URL 中包含 API key 特征字符串时拒绝请求（防止日志泄露 key）

OpenHarness 的 `NetworkGuard` 提供了类似机制，但文档未详细披露实现细节。

---

## 5. 对 harness9 实现网页读取功能的设计建议

基于上述调研，以下是针对 harness9（Go 语言，轻量级，生产可用）的具体设计建议：

### 5.1 整体架构建议

推荐采用**插件化混合策略**，参考 HermesAgent 的 `web_tools.py` 架构：

```
internal/tools/
├── web_search.go          # web_search 工具（接口定义 + backend 调度）
├── web_fetch.go           # web_fetch 工具（接口定义 + HTML 提取）
├── web_backends/          # 可插拔后端
│   ├── duckduckgo.go      # DuckDuckGo（无 key，默认 fallback）
│   ├── brave.go           # Brave Search API
│   ├── tavily.go          # Tavily Search + Extract
│   └── interface.go       # WebSearchBackend 接口
└── web_content.go         # HTML → Markdown 转换（共享工具函数）
```

**理由**：
- Go 生态无 Mozilla Readability，需自行实现 HTML → Markdown，建议封装为独立模块
- 内置 DuckDuckGo 作为零配置 Fallback，保证开箱即用
- 通过接口支持 Brave/Tavily 等付费后端，满足生产需求

### 5.2 工具接口设计

```go
// web_search 工具
// Name: web_search
// 参数 Schema：
type WebSearchArgs struct {
    Query      string `json:"query"`
    MaxResults int    `json:"max_results,omitempty"` // 默认 5，最大 10
}

// web_fetch 工具
// Name: web_fetch
// 参数 Schema：
type WebFetchArgs struct {
    URL      string `json:"url"`
    MaxChars int    `json:"max_chars,omitempty"` // 默认 8000，最大 32000
    Format   string `json:"format,omitempty"`    // "markdown" | "text"，默认 "markdown"
}
```

**返回格式**：

```go
// web_search 返回：结构化 JSON 文本（LLM 可直接理解）
// [1] 标题：...
// URL: ...
// 摘要：...
//
// [2] 标题：...
// ...

// web_fetch 返回：Markdown 格式文本
// # 页面标题
// > 来源：https://...
//
// ... 主内容（Markdown）...
//
// [内容已截断，原始长度 X 字符，显示前 8000 字符]
```

### 5.3 HTML 解析实现建议

Go 生态中推荐的组合：

| 组件 | Go 库 | 功能 |
|------|-------|------|
| HTML 解析 | `golang.org/x/net/html` | 标准库级别的 HTML tokenizer |
| 可读性提取 | `github.com/go-shiori/go-readability` | Mozilla Readability 的 Go 移植 |
| Markdown 转换 | `github.com/JohannesKaufmann/html-to-markdown` | HTML → Markdown |

这三个库合并可实现等同于 OpenClaw `linkedom + @mozilla/readability + htmlToMarkdown` 的效果。

**内容大小限制策略**（参考 OpenClaw + HermesAgent 的经验值）：

```go
const (
    maxHTMLSize   = 1_000_000  // 1MB HTML，超出拒绝处理
    maxNestDepth  = 3_000      // 最大 DOM 嵌套深度
    defaultMaxChars = 8_000    // 默认输出字符上限（约 2000 token）
    hardMaxChars   = 32_000    // 最大输出字符上限（约 8000 token）
)
```

### 5.4 搜索后端 Fallback 策略

```go
// 后端选择优先级（参考 HermesAgent 四级 fallback）
// 1. 环境变量显式指定：HARNESS9_WEB_BACKEND=tavily
// 2. 环境变量 API Key 探测：存在 TAVILY_API_KEY → 使用 Tavily
// 3. 存在 BRAVE_API_KEY → 使用 Brave
// 4. 降级到 DuckDuckGo（无需 key，始终可用）

type WebSearchBackend interface {
    Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error)
    Name() string
    RequiresKey() bool
}
```

### 5.5 SSRF 防护

必须实现，参考 HermesAgent `url_safety.py` 的设计：

```go
// internal/tools/web_safety.go
func isSafeURL(rawURL string) error {
    u, err := url.Parse(rawURL)
    if err != nil {
        return fmt.Errorf("invalid url: %w", err)
    }
    // 1. scheme 必须是 http/https
    // 2. 永久禁止：169.254.x.x（链路本地 / 云 metadata）
    // 3. 禁止私网地址（RFC1918）：192.168.x.x / 10.x.x.x / 172.16-31.x.x
    // 4. 禁止 loopback：127.x.x.x / ::1
    // 5. DNS 解析后二次检查 resolved IP
}
```

**注意**：harness9 当前已有 `safe_path.go` 处理文件路径沙箱，网页 SSRF 防护应独立实现（`web_safety.go`），不复用路径逻辑。

### 5.6 上下文溢出处理

**阶段一（简单）**：`MaxChars` 参数截断，当前 `OffloadHook` 机制（超大工具输出写文件）已可复用。

**阶段二（进阶）**：针对网页内容的 LLM 摘要压缩，参考 HermesAgent 的分级压缩策略：
- 小内容（< 5K chars）：直接返回
- 中等内容（5K~50K chars）：单次 LLM 摘要（使用低成本模型如 claude-haiku）
- 大内容（> 50K chars）：分块并行摘要 → 合成

此功能可复用 `memory/summarization.go` 中的 `Summarizer` 接口，保持设计一致性。

### 5.7 Sub-Agent 与 Deep Research

harness9 已有完整的 Sub-Agent 系统（`internal/subagent/`）。对于深度研究场景，推荐内置一个 `research` 预定义 Sub-Agent：

```markdown
<!-- .harness9/agents/research.md -->
---
name: research
description: Deep web research specialist. Use when the task requires searching multiple sources or gathering comprehensive information from the web.
tools:
  - web_search
  - web_fetch
  - write_file
  - read_file
---

You are a web research specialist. Your goal is to gather comprehensive, accurate information by:

1. Starting with targeted web_search queries
2. Fetching the most relevant pages with web_fetch
3. Cross-referencing multiple sources
4. Writing organized research notes to files
5. Returning a structured summary with source citations
```

这样主 Agent 可以通过 `task` 工具委派给 `research` Sub-Agent，无需复杂的工具权限配置。

### 5.8 实现优先级建议

| 优先级 | 功能 | 实现建议 |
|--------|------|---------|
| P0 | `web_fetch` 工具 | `golang.org/x/net/html` + go-readability，DuckDuckGo fallback，SSRF 防护 |
| P0 | `web_search` 工具（DuckDuckGo） | 参考 OpenHarness 实现，简单 HTTP + 正则解析 |
| P1 | HTML → Markdown 转换 | `html-to-markdown` 库 |
| P1 | SSRF 防护完整实现 | `web_safety.go`，参考 HermesAgent |
| P2 | 多后端支持（Brave/Tavily） | `WebSearchBackend` 接口 + 各后端实现 |
| P2 | 内容 LLM 压缩 | 复用 `memory.Summarizer` 接口 |
| P3 | `research` 预定义 Sub-Agent | `.harness9/agents/research.md` 文件 |
| P3 | 浏览器自动化 | MCP（Playwright MCP 服务器），不内置 |

---

## 6. 权威参考资料

以下资料均经 WebFetch 确认可正常访问，且内容与本调研主题直接相关：

| 标题 | 来源 | URL | 摘要 |
|------|------|-----|------|
| Building Effective AI Agents | Anthropic Research | https://www.anthropic.com/research/building-effective-agents | Anthropic 官方 Agent 设计指导，涵盖工具使用、Orchestrator-Workers 模式，提及搜索任务的多轮评估场景 |
| Introducing the Model Context Protocol | Anthropic News | https://www.anthropic.com/news/model-context-protocol | MCP 协议发布公告，是 DeepAgents 和 Claude Agent SDK 网页工具外接方案的技术基础 |
| Claude Agent SDK Overview | Anthropic Docs | https://code.claude.com/docs/en/agent-sdk/overview | Claude Agent SDK 官方文档，WebSearch/WebFetch 工具的权威定义来源 |
| Claude Agent SDK Subagents | Anthropic Docs | https://code.claude.com/docs/en/agent-sdk/subagents | Sub-Agent 系统详细文档，包含 research-agent 示例的设计模式说明 |
| OpenHarness GitHub | HKUDS | https://github.com/HKUDS/OpenHarness | OpenHarness 框架源码，包含 web_search_tool.py 和 web_fetch_tool.py 的完整实现 |
| HermesAgent GitHub | NousResearch | https://github.com/NousResearch/hermes-agent | HermesAgent 源码，包含 web_tools.py、browser_tool.py、url_safety.py 等网页能力完整实现 |
| OpenClaw GitHub | OpenClaw | https://github.com/openclaw/openclaw | OpenClaw 源码，包含 src/web-search/、src/web-fetch/、extensions/ 等插件化网页架构 |

---

## 7. 总结

| 维度 | 最佳实践来源 | 核心做法 |
|------|------------|---------|
| 工具接口设计 | Claude Agent SDK | 最简参数（query/url），由 LLM 决策具体参数值 |
| HTML 解析 | OpenClaw | Mozilla Readability + Markdown 转换，保留语义结构 |
| 多后端 Fallback | HermesAgent + OpenClaw | 4 级优先级：显式配置 → API Key 探测 → 托管网关 → 免费 Fallback |
| LLM 内容压缩 | HermesAgent | 分级压缩：5K/500K/2M 三个阈值，低成本模型执行 |
| SSRF 防护 | HermesAgent | 硬禁 metadata 端点 + DNS 解析后二次检查 + Fail-closed |
| Sub-Agent 研究模式 | Claude Agent SDK | 研究 Sub-Agent 持有 WebSearch 权限，文件系统作为 Sub-Agent 间通信媒介 |
| 深度搜索决策 | 行业共识 | 工具层提供原子能力，搜索树遍历决策完全在 LLM System Prompt 层面实现 |

对于 harness9 而言，实现网页读取能力的最关键路径是：**P0 阶段完成 `web_fetch` + `web_search`（DuckDuckGo fallback）+ SSRF 防护**，已足以支撑大多数 Agent 场景。LLM 内容压缩和多后端支持可在 P1/P2 阶段根据实际用量决策是否引入。
