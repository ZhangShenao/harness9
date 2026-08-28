# MCP 能力深度调研：主流 Agent Harness 框架的 MCP 集成设计

> 调研日期：2026-06-17
> 调研范围：DeepAgents / OpenHarness / OpenCode / OpenClaw / HermesAgent / Claude Agent SDK
> 调研背景：harness9 feat/mcp 分支正在开发 MCP 集成能力，本报告为该实现提供架构参考

---

## 1. 摘要与横向对比表

### 1.1 MCP 支持能力矩阵

| 维度 | DeepAgents | OpenHarness | OpenCode | OpenClaw | HermesAgent | Claude Agent SDK |
|------|-----------|-------------|----------|----------|-------------|-----------------|
| **MCP Client（消费 MCP 工具）** | 间接（通过 LangChain tools） | 是（原生实现） | 是（深度实现） | 否（自有插件系统） | 部分（仅消费） | 是（最完整） |
| **MCP Server（暴露自身能力）** | 否 | 否 | 否 | 是（核心架构） | 是（messaging bridge） | 是（in-process SDK server） |
| **传输方式支持** | HTTP（间接） | stdio + HTTP + WebSocket | stdio + HTTP + SSE | stdio | stdio | stdio + HTTP + SSE + in-process |
| **工具与内置工具统一管理** | 是（additive，同一 tools 参数） | 是（McpToolAdapter 注入 Registry） | 是（合并到 tools() 返回值） | 不适用 | 部分（独立工具集） | 是（allowedTools 统一管控） |
| **Resources 支持** | 否 | 是（ListMcpResources + ReadMcpResource 工具） | 是（collectFromConnected） | 否 | 否 | 是（resource 类型返回值） |
| **Prompts 支持** | 否 | 否 | 是（getPrompt API） | 否 | 否 | 部分（文档未明确） |
| **Sampling 支持** | 否 | 否 | 否 | 否 | 否 | 否 |
| **OAuth / 认证** | 否 | 是（Bearer/Header/Env 三模式） | 是（完整 OAuth 2.1 + PKCE + token refresh） | 否 | 否 | 是（env 变量 + HTTP headers） |
| **多 Server 并发** | 是（LangChain tools 并发） | 是（connect_all 并发） | 是（Effect 并发连接） | 是（多插件并发） | 否 | 是（多 mcpServers 并发） |
| **Server 生命周期管理** | 无（由 LangChain 层托管） | connect_all / reconnect_all / close | Effect.acquireUseRelease | 进程级 stdio 管理 | EventBridge + FastMCP | query 级别自动生命周期 |
| **listChanged 动态更新** | 否 | 否（reconnect_all 手动重连） | 是（ToolListChangedNotificationSchema） | 否 | 否 | 否（文档未提及） |
| **工具名命名规范** | 原名（LangChain tool name） | `mcp__server__tool` | `{serverName}_{toolName}`（下划线分隔） | 无（插件工具原名） | 原名 | `mcp__{server-name}__{tool-name}` |
| **大工具集优化** | 否 | 否 | 是（pagination，1000 页上限） | 否 | 否 | 是（Tool Search，按需加载，支持 10000 工具） |
| **实现语言** | Python | Python | TypeScript | TypeScript | Python | Python + TypeScript |
| **GitHub Stars** | 24,752 | 13,932 | 175,405 | 379,088 | 195,609 | N/A（官方 SDK） |

---

## 2. 逐框架深度分析

### 2.1 DeepAgents (LangChain)

**GitHub**: https://github.com/langchain-ai/deepagents
**语言**: Python | **Stars**: 24,752（未 archived）

#### 2.1.1 MCP 定位

DeepAgents 对 MCP 的态度是"借道 LangChain 工具体系"，而非独立实现 MCP 客户端协议栈。官方 README 明确列出"Tools — bring your own functions or any MCP server"，但源码中（经多文件扫描确认）**不存在任何原生 MCP Client 实现**。

其集成路径如下：

```
LangChain MCP 适配器（langchain_mcp）
     ↓ 将 MCP tools 转换为 BaseTool 实例
create_deep_agent(tools=[...langchain_mcp_tools...])
     ↓ additive merge
内置工具（bash/filesystem/task 等）+ 用户工具
```

`create_deep_agent()` 函数签名中**不存在 `mcp_servers` 参数**，MCP 集成完全由调用者在外部完成（通过 `langchain-mcp-adapters` 等社区包把 MCP server 工具转化为 `BaseTool`，再通过 `tools=` 参数传入）。

#### 2.1.2 工具管理架构

内置工具通过 `FilesystemMiddleware`、`SubAgentMiddleware` 等 Middleware 动态注入到 agent graph，用户自定义工具（包括 MCP 衍生工具）通过 `tools=` 参数 additive 添加。工具描述可通过 `HarnessProfile` 的 `tool_description_overrides` 覆盖，通过 `excluded_tools` 移除。

MCP 工具经外部 adapter 转换后，名称沿用 LangChain 侧的工具名（不做特殊前缀处理），与内置工具在同一工具列表中一视同仁。

#### 2.1.3 设计亮点

- **MCP Docs Agent 示例**：`examples/deploy-mcp-docs-agent/` 展示了通过 deepagents CLI 注册 HTTP MCP server（`deepagents mcp-servers add --url https://docs.langchain.com/mcp`），然后在 `tools.json` 引用的配置式集成方法，说明框架层面有 workspace-scoped MCP server 注册表的概念。
- **Additive 工具合并**：用户传入工具永远叠加在内置工具之上，从不替换，这使 MCP 工具可以无缝增强内置能力而不引发冲突。
- **Profiles 系统**：HarnessProfile 可按模型定制工具行为，为 MCP 工具的 description override 提供了钩子。

#### 2.1.4 局限

- 没有原生 MCP Client 实现，须依赖外部 `langchain-mcp-adapters`；
- 不支持 MCP Resources 和 Prompts（工具以外的 MCP 能力）；
- 没有 Server 生命周期管理（连接/断线重连由外部库负责）；
- 没有认证管理机制。

---

### 2.2 OpenHarness (HKUDS)

**GitHub**: https://github.com/HKUDS/OpenHarness
**语言**: Python | **Stars**: 13,932（未 archived）

#### 2.2.1 MCP 实现架构

OpenHarness 有自研的 MCP 客户端实现，位于 `src/openharness/mcp/` 包，由四个核心文件构成：

```
src/openharness/mcp/
├── __init__.py      # 懒加载公共 API
├── client.py        # McpClientManager（核心）
├── config.py        # load_mcp_server_configs（配置合并）
└── types.py         # McpStdioServerConfig / McpHttpServerConfig / McpWebSocketServerConfig
```

**McpClientManager** 实现了完整的多 Server 生命周期管理：

```python
# 连接层：两类传输均通过 AsyncExitStack 管理资源
async def _connect_stdio(self, name, config):   # 启动子进程，建立 stdin/stdout 双向流
async def _connect_http(self, name, config):    # httpx.AsyncClient + streamable HTTP session

# 生命周期接口
async def connect_all()      # 并发初始化所有 server 连接
async def reconnect_all()    # 先关闭所有连接，重置为 pending，再重新连接
async def close()            # 优雅关闭，suppress RuntimeError/CancelledError

# 工具/资源发现
async def list_tools()       # 返回 McpToolInfo 列表
async def list_resources()   # 返回 McpResourceInfo 列表
async def call_tool(server_name, tool_name, arguments)  # 执行工具
```

连接状态机：`pending → connected | failed`，失败时记录错误信息，不中断其他 server 的初始化。

#### 2.2.2 工具统一管理：McpToolAdapter 模式

OpenHarness 通过 **Adapter 模式**把 MCP 工具注入到框架的统一工具 Registry：

```python
# tools/__init__.py 中的 create_default_tool_registry()
if mcp_manager:
    # 1. 注入 MCP 元信息工具
    registry.register(ListMcpResourcesTool(mcp_manager))
    registry.register(ReadMcpResourceTool(mcp_manager))
    # 2. 为每个 MCP tool 创建 Adapter 并注册
    for tool_info in mcp_manager.list_tools():
        registry.register(McpToolAdapter(mcp_manager, tool_info))
```

**McpToolAdapter** 核心逻辑：
- 工具名格式：`mcp__<server>__<tool>`（双下划线分隔）
- 参数转换：JSON Schema → Pydantic model（通过 `_input_model_from_schema` 动态生成）
- 执行路径：`execute(args)` → `model_dump(mode='json', exclude_none=True)` → `manager.call_tool()`
- 错误处理：捕获 `McpServerNotConnectedError`，返回 `ToolResult(is_error=True)`

#### 2.2.3 Resources 支持（三个专用工具）

OpenHarness 专门为 MCP Resources 设计了三个工具：

| 工具名 | 功能 |
|--------|------|
| `list_mcp_resources` | 列出所有连接 server 的可用 Resources |
| `read_mcp_resource` | 按 server_name + uri 读取具体 Resource 内容 |
| `mcp_auth` | 配置 MCP server 的认证信息（Bearer/Header/Env） |

Resources 工具作为一等公民注入 Registry，与普通内置工具无差别，LLM 可以像调用任何工具一样发现并使用 MCP Resources。

#### 2.2.4 认证管理

`McpAuthTool` 支持三种认证模式：
- **stdio 服务器**：`env`（注入环境变量）或 `bearer`（`MCP_AUTH_TOKEN`）
- **HTTP/WebSocket 服务器**：`header`（自定义 header 名）或 `bearer`（`Authorization: Bearer xxx`）

认证配置持久化到 Settings，并在配置变更后自动触发 reconnect。

#### 2.2.5 配置来源合并

`load_mcp_server_configs()` 实现了多来源配置合并：settings 中的配置优先，plugin 中的 MCP server 配置用 `plugin-name:config-name` 命名空间防冲突，通过 `setdefault` 确保 settings 的配置不被覆盖。

#### 2.2.6 局限

- 没有 `listChanged` 动态更新支持（需手动调用 `reconnect_all`）；
- 没有 MCP Prompts 支持；
- 没有大工具集优化（如分批加载）；
- 重连后工具 Registry 不自动更新（需要重新调用 `create_default_tool_registry`）。

---

### 2.3 OpenCode (Anomaly)

**GitHub**: https://github.com/anomalyco/opencode
**语言**: TypeScript | **Stars**: 175,405（未 archived）

#### 2.3.1 MCP 实现架构：Effect 函数式服务

OpenCode 的 MCP 实现是所有调研框架中**最完整、最工程化**的实现，位于 `packages/opencode/src/mcp/`（五个文件，35KB 核心逻辑）。

整体采用 **Effect.ts 函数式框架**管理副作用与资源生命周期：

```
src/mcp/
├── index.ts         # MCP Service（核心，35KB）
├── catalog.ts       # McpCatalog（工具/Prompt/Resource 转换与分页）
├── auth.ts          # OAuth 2.1 + PKCE + token 持久化（文件锁）
├── oauth-provider.ts # OAuth Provider 实现
└── oauth-callback.ts # OAuth callback server
```

#### 2.3.2 连接管理：双模式

**Remote 服务器**（HTTP/SSE）：

```typescript
// StreamableHTTPClientTransport（主）→ SSEClientTransport（降级）
// Effect.acquireUseRelease 确保传输层故障时资源释放
connectRemote(name, config, state) {
  // 尝试 StreamableHTTP，失败则 fallback SSE
  // 支持 OAuth headers + 30s 默认超时
}
```

**Local 服务器**（stdio）：

```typescript
connectLocal(name, config, state) {
  // StdioClientTransport：指定 command/args/env/cwd
  // Effect.acquireUseRelease 管理子进程生命周期
}
```

#### 2.3.3 工具统一管理

工具通过 `MCP.tools()` 方法返回，命名规则为 `sanitize(clientName) + "_" + sanitize(mcpTool.name)`（单下划线分隔，与 Claude SDK 的 `mcp__` 不同）：

```typescript
const tools = Effect.fn("MCP.tools")(function* () {
  const result: Record<string, Tool> = {}
  for (const [clientName, client] of Object.entries(s.clients)) {
    if (s.status[clientName]?.status !== "connected") continue
    for (const mcpTool of s.defs[clientName]) {
      const key = sanitize(clientName) + "_" + sanitize(mcpTool.name)
      result[key] = McpCatalog.convertTool(mcpTool, client, timeout)
    }
  }
  return result
})
```

工具 Registry（`packages/opencode/src/tool/registry.ts`）在构建工具列表时调用 `MCP.tools()`，与 built-in 工具合并，统一交给模型使用。

#### 2.3.4 Catalog 层（McpCatalog）

`catalog.ts` 实现了以下关键能力：

- **工具转换**：MCP `Tool` → ai SDK `Tool`，强制 `additionalProperties: false`，防止 schema 注入；schema 验证失败时降级为 tolerant schema（剥除 `outputSchema`）
- **分页**：`paginate()` 函数处理 cursor-based 分页，最多 1000 页，检测 cursor 循环避免死循环
- **名称净化**：`sanitize()` 将非字母数字字符替换为下划线

#### 2.3.5 Resources 与 Prompts 支持

两者均通过 `collectFromConnected` 通用模式实现：

```typescript
// 遍历所有 connected clients，按能力检查（capabilities.prompts/resources）
// 通过 McpCatalog.prompts/resources 分页获取，error-tolerant
const prompts = yield* collectFromConnected(
  (client, name) => McpCatalog.prompts(client, name, timeout)
)
const resources = yield* collectFromConnected(
  (client, name) => McpCatalog.resources(client, name, timeout)
)
```

Resource 单独读取：`readResource(serverName, uri)` 通过 `withClient` 路由到对应 client。
Prompt 获取：`getPrompt(name, args)` 支持动态参数注入。

#### 2.3.6 动态工具更新（listChanged）

OpenCode 是唯一支持 `tools/list_changed` 通知的框架：

```typescript
// 注册 ToolListChangedNotificationSchema handler
client.setNotificationHandler(
  ToolListChangedNotificationSchema,
  async () => {
    const newDefs = await client.listTools()
    state.defs[name] = newDefs.tools
    // 发布 ToolsChanged 事件，触发 Registry 刷新
    bus.publish(ToolsChanged, ...)
  }
)
```

#### 2.3.7 OAuth 2.1 + PKCE 完整实现

`auth.ts` 实现了 MCP spec 2025-03-26 定义的 OAuth 2.1 授权流程：
- PKCE code_verifier/code_challenge
- state 参数防 CSRF
- token 自动刷新（`isTokenExpired` 检查）
- 凭证持久化到 `mcp-auth.json`（0o600 权限，文件锁并发安全）
- 动态客户端注册（无需预先注册 client_id 的服务端）的 fallback 支持

#### 2.3.8 连接状态机

五种状态：`connected | disabled | failed | needs_auth | needs_client_registration`

其中 `needs_auth` 和 `needs_client_registration` 是 OAuth flow 中的中间状态，触发 UI 侧的引导流程。

#### 2.3.9 局限

- Tool Search 优化不内置（依赖外部 AI SDK 层处理 context window 问题）；
- Effect.ts 学习成本高，源码理解门槛较高；
- 工具名使用单下划线 `_` 而非双下划线 `__`，与 Claude SDK 规范不同，存在潜在冲突。

---

### 2.4 OpenClaw

**GitHub**: https://github.com/openclaw/openclaw
**语言**: TypeScript | **Stars**: 379,088（未 archived）

#### 2.4.1 双重角色：MCP Server 而非 MCP Client

OpenClaw 的 MCP 集成方向与其他框架完全相反：它**把自身能力暴露为 MCP Server**，供外部 MCP Client（如 Claude Code、Cursor 等）调用，而非作为 MCP Client 去消费外部工具。

MCP 相关代码位于 `src/mcp/`（14 个文件）：

```
src/mcp/
├── channel-bridge.ts        # MCP Gateway bridge（核心，21KB）
├── channel-server.ts        # Channel MCP Server 创建
├── channel-shared.ts        # 共享协议类型
├── channel-tools.ts         # 注册 channel 相关工具到 MCP Server
├── plugin-tools-serve.ts    # 插件工具 MCP Server（启动入口）
├── plugin-tools-handlers.ts # 插件工具执行 handler
├── openclaw-tools-serve.ts  # OpenClaw 内置工具 MCP Server
└── tools-stdio-server.ts    # stdio 传输通用工具
```

#### 2.4.2 架构设计：Gateway Bridge 模式

```
外部 MCP Client（Claude Code/Cursor 等）
    ↓ stdio
[Channel Bridge MCP Server]
    ↓ Gateway Protocol
[OpenClaw Gateway]
    ↓ 内部路由
[Plugin Registry / Channel System]
```

`OpenClawChannelBridge` 充当 MCP 工具调用 → Gateway 操作的翻译层：

- **连接管理**：与 Gateway 建立持久连接，等待 "hello ok" 握手后订阅事件；内置重试机制区分可恢复（transient）和永久（permanent）故障
- **事件队列**：Gateway 事件进入内部队列（上限 1000 条），cursor-based 分页，支持 polling 和 long-poll 两种消费模式
- **审批路由**：工具调用中的 exec/plugin 审批请求通过 TTL map 跟踪，响应路由回 Gateway

#### 2.4.3 工具暴露模式

OpenClaw 通过三种 MCP Server 暴露不同类型的工具：

| Server 名称 | 工具来源 | 传输方式 |
|------------|---------|---------|
| `openclaw-tools` | OpenClaw 内置工具（如 cron） | stdio |
| `openclaw-plugin-tools` | 用户安装的插件工具 | stdio |
| Channel Server | 会话/消息/审批管理工具 | stdio |

每个 Server 都通过 `createToolsMcpServer()` + `connectToolsMcpServerToStdio()` 组合启动，日志强制重定向到 stderr（保持 stdout 干净供 MCP 协议使用）。

#### 2.4.4 自身的工具系统

OpenClaw 有独立的插件工具系统（`packages/plugin-sdk/src/provider-tools.ts`），工具通过 `defineToolDescriptor()` 声明式定义，`toToolProtocolDescriptor()` 在运行时转换为 MCP 协议格式。

这套系统与 MCP 工具体系并行存在：内部工具通过 Plugin SDK 管理，对外通过 `plugin-tools-serve.ts` 以 MCP Server 形式暴露。

#### 2.4.5 局限

- **不消费外部 MCP 工具**，这是其他框架 MCP Client 角色的缺失；
- 400+ 插件扩展系统与 MCP 整合松散，大量扩展仍通过私有 Plugin Protocol 而非 MCP 协议对接；
- 没有 Resources/Prompts/Sampling 的 Server 侧支持（仅暴露 Tools）。

---

### 2.5 HermesAgent (NousResearch)

**GitHub**: https://github.com/NousResearch/hermes-agent
**语言**: Python | **Stars**: 195,609（未 archived）

#### 2.5.1 双重 MCP 角色

HermesAgent 同时具有：
- **MCP Server**（`mcp_serve.py` + `agent/transports/hermes_tools_mcp_server.py`）：把 Hermes 的 messaging bridge 能力暴露为 MCP Server
- **可选 MCP 服务集成**（`optional-mcps/` 目录）：Linear 和 n8n 的 MCP 集成配置示例

但 HermesAgent **没有实现原生 MCP Client 协议栈**，对外部 MCP Server 的消费是通过运行时的 FastMCP 框架处理，Agent 主循环（`agent/conversation_loop.py`）不包含 MCP Client 逻辑。

#### 2.5.2 Messaging Bridge MCP Server

`mcp_serve.py` 通过 **FastMCP** 暴露 10 个工具，作为 Hermes messaging 状态的 MCP 接口：

```python
@mcp.tool()
async def conversations_list(platform: str = "", keyword: str = "", limit: int = 20):
    """Filter and paginate active sessions"""
    ...

@mcp.tool()
async def messages_send(session_id: str, text: str, channel_id: str = ""):
    """Transmit outbound messages"""
    ...
```

配置方式（任何 MCP Client 均可直接对接）：

```json
{
  "mcpServers": {
    "hermes": {"command": "hermes", "args": ["mcp", "serve"]}
  }
}
```

#### 2.5.3 Hermes Tools MCP Server（Codex 集成专用）

`agent/transports/hermes_tools_mcp_server.py` 是专为 Codex App Server 集成设计的 MCP Server：

- 从 `get_tool_definitions()` 获取权威工具 schema
- 为 28 个 Hermes 工具（web_search/browser/vision/skill 等）创建闭包 handler
- 每个 handler 代理到 `model_tools.handle_function_call()`

**刻意排除**的工具（设计决策）：
- 终端/文件操作：由 Codex 自身内置工具覆盖，避免重复
- `delegate_task`、`memory`、`todo` 等：这些工具依赖"running AIAgent context"的中间状态，无法在无状态 MCP 回调中驱动

这一排除策略揭示了 MCP 协议固有局限：**有状态的 Agent 内部机制难以通过无状态 MCP 工具接口暴露**。

#### 2.5.4 EventBridge：状态同步机制

Messaging MCP Server 通过后台 `EventBridge` 线程保持状态同步：
- 每 200ms 轮询 `state.db`（SQLite）和 `sessions.json`
- mtime 缓存避免空转 I/O
- 最多保留 1000 条事件，cursor-based 索引

这种方式本质上是把有状态的 Hermes 运行时包装为无状态 MCP 工具，通过 polling bridge 实现近实时同步。

#### 2.5.5 局限

- 没有原生 MCP Client（无法消费外部 MCP Server 工具）；
- FastMCP stdio 传输，不支持 HTTP/SSE；
- 没有认证管理；
- EventBridge polling 模式存在 200ms 延迟，非 push 模式。

---

### 2.6 Claude Agent SDK (Anthropic)

**文档**: https://code.claude.com/docs/en/agent-sdk/mcp
**语言**: Python + TypeScript | **来源**: Anthropic 官方

#### 2.6.1 MCP 定位：一等公民能力

Claude Agent SDK 将 MCP 支持作为核心能力之一（与 Hooks、Subagents 并列），在官方文档中有独立章节，是调研框架中文档最完整、API 最规范的实现。

#### 2.6.2 三种传输类型

```python
# 1. stdio（本地进程）
mcp_servers={
    "github": {
        "command": "npx",
        "args": ["-y", "@modelcontextprotocol/server-github"],
        "env": {"GITHUB_TOKEN": os.environ["GITHUB_TOKEN"]}
    }
}

# 2. HTTP（Streamable HTTP，新规范）
mcp_servers={
    "remote-api": {
        "type": "http",
        "url": "https://api.example.com/mcp",
        "headers": {"Authorization": "Bearer token"}
    }
}

# 3. SSE（Server-Sent Events，旧规范）
mcp_servers={
    "legacy-api": {
        "type": "sse",
        "url": "https://api.example.com/mcp/sse"
    }
}

# 4. In-process SDK MCP Server（无需外部进程）
from claude_agent_sdk import tool, create_sdk_mcp_server

@tool("my_func", "Do something", {"x": int})
async def my_func(args): ...

sdk_server = create_sdk_mcp_server(name="mytools", version="1.0.0", tools=[my_func])
mcp_servers={"mytools": sdk_server}
```

#### 2.6.3 工具命名与权限控制

MCP 工具统一命名规范：`mcp__{server-name}__{tool-name}`（双下划线，破折号保留）

权限控制通过 `allowedTools` 显式声明：
```python
allowed_tools=[
    "mcp__github__*",           # wildcard: server 下所有工具
    "mcp__db__query",           # 精确到某个工具
    "mcp__slack__send_message"  # 跨 server 选择
]
```

这与内置工具的权限管控使用同一个列表，统一了 MCP 工具和内置工具的权限模型。

#### 2.6.4 Tool Search：大工具集优化

这是调研框架中**唯一原生实现大工具集优化**的方案：

- 默认开启（Vertex AI 和非官方代理除外）
- 工作机制：Tool 定义不随 context 传送，Agent 在需要某类工具时先触发一次搜索，再加载 3-5 个最相关工具
- 5 种配置值：`true` / `false` / `auto` / `auto:N` / unset
- 支持最多 10,000 个工具（远超其他框架）
- 适用于 MCP 工具和内置工具

```python
env={"ENABLE_TOOL_SEARCH": "auto:5"}  # 工具定义超过 context 5% 时启动
```

#### 2.6.5 生命周期管理

MCP Server 生命周期与 `query()` 调用绑定：每次 `query()` 自动启动配置中的 MCP Server，调用结束后自动清理。通过 init 事件监测连接状态：

```python
async for message in query(prompt=..., options=options):
    if isinstance(message, SystemMessage) and message.subtype == "init":
        failed = [s for s in message.data["mcp_servers"] if s["status"] != "connected"]
```

#### 2.6.6 In-process SDK MCP Server

`create_sdk_mcp_server` / `createSdkMcpServer` 允许在进程内定义 MCP 工具，不需要启动外部子进程：

```python
@tool(
    "get_temperature",
    "Get the current temperature at a location",
    {"latitude": float, "longitude": float},
    annotations=ToolAnnotations(readOnlyHint=True)  # 标注为只读，可并行调用
)
async def get_temperature(args): ...

weather_server = create_sdk_mcp_server(
    name="weather", version="1.0.0", tools=[get_temperature]
)
```

工具注解（`readOnlyHint`/`destructiveHint`/`idempotentHint`/`openWorldHint`）直接映射到 MCP spec 中的 Tool Annotations。

#### 2.6.7 Resources 与 Structured Content

工具返回值支持 `resource` 类型（嵌入式资源）和 `structuredContent`（机器可读 JSON），完整遵循 MCP 2025-06-18 规范：

```python
return {
    "content": [{"type": "resource", "resource": {"uri": "file:///tmp/report.md", "text": "..."}}],
    "structuredContent": {"temperature": 22.5, "humidity": 65}
}
```

#### 2.6.8 局限

- OAuth 2.1 不自动处理，需调用者在 token 获取后通过 headers 传入；
- `listChanged` 动态工具更新未在文档中提及；
- Sampling 功能未实现；
- in-process SDK server 的 Python 侧不支持 `structuredContent`（需用外部 MCP server）。

---

## 3. 横向对比深度分析

### 3.1 工具统一管理：四种架构范式

| 范式 | 代表框架 | 核心思路 | 优缺点 |
|------|---------|---------|--------|
| **Adapter 注入 Registry** | OpenHarness | McpToolAdapter 将 MCP tool 包装为 BaseTool，注入统一 Registry | 架构统一，LLM 无感知；重连后需重建 Registry |
| **合并返回（Lazy Merge）** | OpenCode | tools() 方法按需合并 built-in + MCP 工具 | 动态，支持 listChanged；命名规范独立（单下划线） |
| **外部 Adapter（框架不感知）** | DeepAgents | tools= 参数接受任意 BaseTool，MCP adapter 在框架外完成 | 最大灵活性；框架无 MCP 生命周期概念 |
| **SDK 级统一管控** | Claude Agent SDK | allowedTools 统一管理，query 级生命周期 | 最简洁；生命周期与 query 耦合，无法跨 query 复用连接 |

### 3.2 生命周期管理：三种策略

**策略一：进程生命周期（OpenHarness）**
```
应用启动 → connect_all() → 工具注入 Registry → 工具持续可用
连接断开 → reconnect_all() → Registry 重建
应用关闭 → close()
```
优点：连接复用，开销低。缺点：重连后需手动刷新工具列表。

**策略二：Effect 资源管理（OpenCode）**
```
query 执行 → Effect.acquireUseRelease 分配连接 → 使用 → 自动释放
listChanged → 自动更新工具缓存，无需重连
```
优点：资源安全，支持动态更新。缺点：Effect 框架学习成本高。

**策略三：query 级生命周期（Claude Agent SDK）**
```
每次 query() → 启动配置的 MCP servers → 执行 → 清理
```
优点：最简单，不需要显式管理。缺点：每次 query 重启 server 有冷启动开销（尤其对 stdio server）。

### 3.3 命名规范对比

| 框架 | 规范 | 示例 |
|------|------|------|
| Claude Agent SDK | `mcp__{server-name}__{tool-name}` | `mcp__github__list_issues` |
| OpenHarness | `mcp__{server}__{tool}` | `mcp__github__list_issues` |
| OpenCode | `{server}_{tool}` | `github_list_issues` |
| DeepAgents | 原工具名（由外部 adapter 决定） | `list_issues` |
| OpenClaw | 不适用（自身为 Server） | N/A |
| HermesAgent | 原工具名（FastMCP 默认） | `web_search` |

Claude Agent SDK 与 OpenHarness 采用相同的双下划线规范（`mcp__`），这是更好的实践：明确区分 MCP 工具与内置工具，避免命名冲突。

### 3.4 协议支持深度

```
MCP 协议能力层级（从基础到高级）：
Level 1: Tools/list + Tools/call                    全部框架支持
Level 2: stdio transport                             全部框架支持（除 DeepAgents）
Level 3: HTTP/SSE transport                          OpenCode + Claude Agent SDK
Level 4: Resources/list + Resources/read             OpenHarness + OpenCode + Claude Agent SDK
Level 5: Prompts/list + Prompts/get                  OpenCode（部分）
Level 6: listChanged 动态通知                         OpenCode（唯一）
Level 7: OAuth 2.1 + PKCE                           OpenCode（完整）
Level 8: Tool Annotations                            Claude Agent SDK
Level 9: structuredContent（outputSchema）           Claude Agent SDK
Level 10: Tool Search（大规模工具集）                 Claude Agent SDK（唯一）
Level 11: Sampling（LLM 反向调用）                   无框架实现
```

---

## 4. 对 harness9 的启示与建议

harness9 当前架构：
- 工具通过 `internal/tools/Registry` 统一管理（`BaseTool` 接口：`Name() / Definition() / Execute()`）
- 标准 ReAct 循环，同 Turn 并发工具执行
- 当前在 `feat/mcp` 分支开发 MCP 集成

### 4.1 核心设计建议

#### 建议一：采用 Adapter 注入 Registry 模式（对齐 OpenHarness）

harness9 的 `BaseTool` 接口天然适合 Adapter 模式：

```go
// internal/tools/mcp_adapter.go
type MCPToolAdapter struct {
    client     MCPClient        // MCP client 接口
    serverName string
    toolDef    MCPToolInfo      // 从 tools/list 获取的工具元数据
}

func (a *MCPToolAdapter) Name() string {
    return "mcp__" + sanitize(a.serverName) + "__" + sanitize(a.toolDef.Name)
}

func (a *MCPToolAdapter) Definition() schema.ToolDefinition {
    return schema.ToolDefinition{
        Name:        a.Name(),
        Description: a.toolDef.Description,
        InputSchema: a.toolDef.InputSchema, // JSON Schema 直接透传
    }
}

func (a *MCPToolAdapter) Execute(ctx context.Context, args json.RawMessage) (string, error) {
    result, err := a.client.CallTool(ctx, a.toolDef.Name, args)
    if err != nil {
        return "", fmt.Errorf("mcp tool %s: %w", a.Name(), err)
    }
    return result, nil
}
```

这样 MCP 工具对 Engine 完全透明，所有现有的并发执行、超时控制、错误回传机制自动生效。

#### 建议二：采用双下划线命名规范

遵循 Claude Agent SDK 和 OpenHarness 的 `mcp__{server}__{tool}` 规范，理由：
- 明确区分 MCP 工具与内置工具（内置工具无前缀：`bash`、`read_file`）
- 双下划线降低与正常工具名冲突的概率（单下划线如 OpenCode 的 `server_tool` 存在歧义）
- 与业界事实标准（Claude）保持一致

#### 建议三：MCPManager 独立包，生命周期与 Session 绑定

```go
// internal/mcp/manager.go
type Manager struct {
    mu      sync.RWMutex
    servers map[string]*ServerState  // server name → 状态
    config  []ServerConfig
}

type ServerState struct {
    status  Status     // pending/connected/failed
    client  MCPClient
    tools   []MCPToolInfo
    stack   io.Closer  // 资源清理
}

// 生命周期
func (m *Manager) Start(ctx context.Context) error  // connect_all 并发
func (m *Manager) Stop() error                       // 清理所有连接
func (m *Manager) Reconnect(name string) error       // 单个 Server 重连

// 工具注入（对接 Registry）
func (m *Manager) InjectTools(registry *tools.Registry) error
```

生命周期绑定到 Session 而非 query：Session 创建时 `Start()`，Session 删除时 `Stop()`，避免每次交互的重启开销（尤其对 stdio server 冷启动）。

#### 建议四：同时支持 stdio 和 HTTP 两种传输

```go
// internal/mcp/transport.go
type StdioTransport struct {
    cmd  *exec.Cmd
    pipe io.ReadWriteCloser
}

type HTTPTransport struct {
    client  *http.Client
    baseURL string
    headers map[string]string
}
```

HTTP 支持优先级高于 SSE（前者是 MCP 新规范），两者均依赖标准 `net/http`，无额外外部依赖。

#### 建议五：Resources 作为工具暴露（对齐 OpenHarness）

参考 OpenHarness 的实践，将 Resources 通过两个内置工具对 LLM 暴露：

```go
// list_mcp_resources：LLM 可发现并列举所有连接 Server 的 Resources
// read_mcp_resource(server_name, uri)：LLM 可读取具体 Resource 内容
```

这比让 LLM 直接调用底层 MCP 协议更符合 ReAct 范式，也与 harness9 的 YOLO 工具哲学一致。

#### 建议六：参考 OpenCode 实现 listChanged 通知

当 MCP Server 工具列表变更时，通过 JSON-RPC notification 通知重新调用 `tools/list`，并更新 Registry：

```go
// 订阅 tools/list_changed 通知
client.OnNotification("notifications/tools/list_changed", func() {
    if err := m.refreshTools(serverName); err != nil {
        log.Print(logfmt.FormatMsg("mcp", "failed to refresh tools: "+err.Error()))
    }
})
```

#### 建议七：工具注解（Tool Annotations）在 Definition 层支持

MCP 工具注解（`readOnlyHint` 等）可映射到 harness9 的工具 Definition 扩展字段，为未来的并发优化（只读工具可以更激进地并行）和权限控制（DangerHook 可利用 `destructiveHint`）提供语义。

### 4.2 不建议实现的能力

- **Sampling（反向 LLM 调用）**：协议复杂，实际需求低，所有调研框架均未实现
- **OAuth 2.1 完整实现**：初版可简化为 env 变量 + HTTP headers，OAuth 流程可后续按需添加
- **Tool Search**：依赖 Anthropic 私有 API beta（`tool_reference` blocks），非通用能力

### 4.3 实施路径建议

**Phase 1（最小可用）**：
1. `internal/mcp/` 包：MCPClient 接口 + StdioTransport 实现
2. `internal/mcp/manager.go`：Manager（Start/Stop，单 Server）
3. `internal/tools/mcp_adapter.go`：MCPToolAdapter（Adapter 注入 Registry）
4. `cmd/harness9/main.go`：解析 `.mcp.json` 或环境变量，初始化 MCPManager

**Phase 2（生产完备）**：
1. HTTPTransport 支持（含 auth headers）
2. 多 Server 并发 + 单独 Reconnect
3. `list_mcp_resources` / `read_mcp_resource` 工具
4. `listChanged` 动态刷新

**Phase 3（进阶）**：
1. Tool Annotations 映射到 DangerHook 语义
2. OAuth token 传递（通过 HTTP headers）
3. Prompts 支持（通过专用工具暴露）

---

## 5. 参考资料

以下资料均经 WebFetch 验证可访问，内容与本调研主题直接相关。

### 5.1 MCP 官方规范

- MCP 工具规范（2025-06-18）：[https://modelcontextprotocol.io/specification/2025-06-18/server/tools](https://modelcontextprotocol.io/specification/2025-06-18/server/tools)
  - 摘要：完整定义工具格式（inputSchema / outputSchema / annotations）、调用协议、listChanged 通知、错误处理两层机制

- MCP 工具概念文档：[https://modelcontextprotocol.io/docs/concepts/tools](https://modelcontextprotocol.io/docs/concepts/tools)
  - 摘要：工具设计原则、消息流示意图、结构化内容返回规范

### 5.2 Claude Agent SDK 官方文档

- Agent SDK 总览：[https://code.claude.com/docs/en/agent-sdk/overview](https://code.claude.com/docs/en/agent-sdk/overview)
  - 摘要：SDK 核心能力介绍，MCP 作为四大能力之一，包含完整示例

- MCP 集成指南：[https://code.claude.com/docs/en/agent-sdk/mcp](https://code.claude.com/docs/en/agent-sdk/mcp)
  - 摘要：三种传输类型、allowedTools 规范、认证配置、错误处理、OAuth 说明

- 自定义工具（in-process MCP server）：[https://code.claude.com/docs/en/agent-sdk/custom-tools](https://code.claude.com/docs/en/agent-sdk/custom-tools)
  - 摘要：@tool 装饰器、create_sdk_mcp_server、Tool Annotations、structured content 返回

- Tool Search 文档：[https://code.claude.com/docs/en/agent-sdk/tool-search](https://code.claude.com/docs/en/agent-sdk/tool-search)
  - 摘要：大工具集优化原理、5 种配置值、10000 工具上限、与 MCP 工具的交互

### 5.3 框架源码（经 WebFetch 确认可访问）

- OpenCode MCP 实现（index.ts，35KB）：[https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/opencode/src/mcp/index.ts](https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/opencode/src/mcp/index.ts)
  - 摘要：Effect-based MCP Service，五状态连接机器，OAuth 2.1 + PKCE，listChanged 支持

- OpenHarness MCP 客户端：[https://raw.githubusercontent.com/HKUDS/OpenHarness/main/src/openharness/mcp/client.py](https://raw.githubusercontent.com/HKUDS/OpenHarness/main/src/openharness/mcp/client.py)
  - 摘要：McpClientManager，stdio/HTTP 双传输，AsyncExitStack 资源管理

- OpenHarness McpToolAdapter：[https://raw.githubusercontent.com/HKUDS/OpenHarness/main/src/openharness/tools/mcp_tool.py](https://raw.githubusercontent.com/HKUDS/OpenHarness/main/src/openharness/tools/mcp_tool.py)
  - 摘要：Adapter 模式将 MCP 工具注入 Registry，Pydantic 动态参数模型，`mcp__server__tool` 命名

- HermesAgent Hermes Tools MCP Server：[https://raw.githubusercontent.com/NousResearch/hermes-agent/main/agent/transports/hermes_tools_mcp_server.py](https://raw.githubusercontent.com/NousResearch/hermes-agent/main/agent/transports/hermes_tools_mcp_server.py)
  - 摘要：FastMCP stdio server，28 个工具代理，刻意排除有状态工具的设计决策

- OpenClaw Plugin Tools MCP Server：[https://raw.githubusercontent.com/openclaw/openclaw/main/src/mcp/plugin-tools-serve.ts](https://raw.githubusercontent.com/openclaw/openclaw/main/src/mcp/plugin-tools-serve.ts)
  - 摘要：插件工具 MCP Server，策略合并（profile + allowlist + sandbox），stdio 传输
