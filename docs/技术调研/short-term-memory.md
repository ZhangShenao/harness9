# Short-Term Memory 技术调研报告

> 调研时间：2026-05-17  
> 调研范围：DeepAgents、OpenHarness、OpenCode (sst/opencode)、OpenClaw、HermesAgent、Claude Agent SDK、OpenAI Agent SDK

---

## 一、概述与核心概念

Short-Term Memory（短期记忆）是 Agent Harness 框架中负责在单次会话（Session/Thread）生命周期内维持对话连贯性的核心模块。与长期记忆（跨会话持久化知识库）不同，短期记忆聚焦于以下三个问题：

1. **"我们聊了什么"** — 消息历史（Message History）的存储与检索
2. **"上下文塞不下了怎么办"** — 上下文窗口管理（Context Window Management）
3. **"下次打开还在吗"** — 会话持久化（Session Persistence）

各框架在这三个维度上的实现差异巨大，反映了不同的工程哲学：

| 框架 | 核心哲学 | 持久化方案 | 压缩策略 |
|------|----------|------------|----------|
| OpenAI Agent SDK | 插件式后端，开发者选择 | SQLite/Redis/MongoDB/PG 等 8 种 | 滑动窗口 + Token Budget |
| DeepAgents | LangGraph 图状态 + Checkpointer | 插件化 Checkpointer（内存/文件/DB） | Delta 存储 + O(N²)→O(N) 优化 |
| OpenHarness | Markdown 文件 + 进程内消息列表 | MEMORY.md 文件 | 双层 LLM 压缩（微压缩 + 全量） |
| HermesAgent | SQLite WAL + FTS5 | 单文件 SQLite | 会话分叉链（Compression Chain） |
| OpenCode (sst/opencode) | Drizzle ORM + Effect 运行时 | SQLite/PostgreSQL | 存档（compacting 标志） |
| OpenClaw | 本地 JSON/文件 + Gateway | 工作区文件 | /compact 命令 |
| Claude Agent SDK | 托管在 Anthropic 服务端 | 服务端存储（Conversations API） | 服务端 Context Compaction |

---

## 二、Thread/Session 绑定机制

### 2.1 OpenAI Agent SDK — String ID 直接绑定

OpenAI Agent SDK 采用最简洁的设计：Session 对象由一个字符串 `session_id` 标识，调用方自行管理 ID 的生成与分配。

**核心抽象 — SessionABC 协议：**

```python
# 来源：openai/openai-agents-python — src/agents/memory/session.py
from typing import Protocol, runtime_checkable, List

@runtime_checkable
class Session(Protocol):
    session_id: str
    session_settings: SessionSettings | None

    async def get_items(self, limit: int | None = None) -> List[TResponseInputItem]:
        """Retrieve the conversation history for this session."""
        ...

    async def add_items(self, items: List[TResponseInputItem]) -> None:
        """Add new items to the conversation history."""
        ...

    async def pop_item(self) -> TResponseInputItem | None:
        """Remove and return the most recent item from the session."""
        ...

    async def clear_session(self) -> None:
        """Clear all items for this session."""
        ...
```

**Session 绑定到 Runner 的方式：**

```python
# Session ID 即为会话的唯一标识，由调用方生成
session = SQLiteSession("user_12345_conversation")

# 每次 Runner.run() 传入同一 session 对象，即保持上下文连续性
result = await Runner.run(agent, "第一条消息", session=session)
result = await Runner.run(agent, "第二条消息", session=session)
# agent 自动记得第一条消息的内容
```

**Runner 内部的 Session 绑定流程：**

```
Runner.run(agent, input, session=session)
  ├── 1. session.get_items() → 取出历史消息
  ├── 2. 将历史消息 prepend 到 input_items 前
  ├── 3. 调用 LLM（历史 + 新输入）
  └── 4. session.add_items(新产生的所有消息)
       ├── 用户输入
       ├── 助手回复
       └── 工具调用/结果
```

**多会话并发隔离**：每个 `SQLiteSession` 实例对应一个 `session_id`，SQLite 表中以 `session_id` 作为分区键，多会话天然隔离。并发安全通过两套机制保障：
- 内存数据库：`threading.RLock` 共享连接锁
- 文件数据库：Thread-Local 连接 + 进程级引用计数文件锁

### 2.2 DeepAgents — LangGraph Thread ID

DeepAgents 基于 LangGraph，Session 概念对应 LangGraph 的 `thread_id`（即 `config.configurable["thread_id"]`）。Checkpointer 负责将每个 thread 的状态快照序列化到存储后端。

```python
# 来源：langchain-ai/deepagents
from deepagents import create_deep_agent
from langgraph.checkpoint.sqlite import SqliteSaver

checkpointer = SqliteSaver.from_conn_string(":memory:")
agent = create_deep_agent(checkpointer=checkpointer)

# Thread ID 在调用时指定，LangGraph 自动恢复该 thread 的状态
config = {"configurable": {"thread_id": "session_abc"}}
result = await agent.ainvoke({"messages": [HumanMessage("你好")]}, config=config)

# 下次调用同一 thread_id，自动加载历史
result = await agent.ainvoke({"messages": [HumanMessage("刚才说了什么")]}, config=config)
```

**与 OpenAI SDK 的关键区别**：LangGraph 的 Checkpointer 存储的是完整的**图状态（Graph State）**，不仅是消息列表，还包括当前节点位置、中间变量等，支持从任意断点恢复。

### 2.3 OpenHarness — 进程内消息列表 + 命名会话文件

OpenHarness 的短期记忆是两层结构：
- **进程内**：`QueryEngine` 维护 `self._messages: list[ConversationMessage]` 作为当前会话的消息列表
- **跨进程持久化**：通过 Session 命名（`--name` 标志）将会话存储为磁盘文件，通过 `/resume` 命令恢复

```python
# 来源：HKUDS/OpenHarness — src/openharness/engine/query_engine.py

class QueryEngine:
    def __init__(self, ...):
        self._messages: list[ConversationMessage] = []
        self._tool_metadata: ToolMetadata = ToolMetadata()

    def messages(self) -> list[ConversationMessage]:
        return self._messages

    def load_messages(self, messages: list[ConversationMessage]) -> None:
        """Replace conversation history (used when resuming a session)."""
        self._messages = messages

    def clear(self) -> None:
        self._messages = []
        self._cost_tracker.reset()
```

### 2.4 HermesAgent — SQLite Session ID + 分叉链

HermesAgent 使用 SQLite 存储 Session，每个 Session 有一个唯一 ID（UUID），通过 `parent_session_id` 字段支持会话分叉（用于压缩链）：

```python
# 来源：NousResearch/hermes-agent — hermes_state.py
SCHEMA_SQL = """
CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    source TEXT NOT NULL DEFAULT 'cli',
    user_id TEXT,
    model TEXT NOT NULL,
    parent_session_id TEXT,  -- 支持压缩链：指向被压缩的父 session
    title TEXT,
    started_at REAL NOT NULL,
    ended_at REAL,
    end_reason TEXT,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (parent_session_id) REFERENCES sessions(id)
);

CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    role TEXT NOT NULL,  -- 'user'|'assistant'|'tool'
    content TEXT NOT NULL,  -- 普通文本或 JSON-encoded 多模态内容
    FOREIGN KEY (session_id) REFERENCES sessions(id)
);
"""
```

**压缩链查找**：

```python
def get_compression_tip(self, session_id: str) -> str:
    """Walk the compression chain forward to find the live continuation session."""
    current_id = session_id
    while True:
        child = self._find_child_session(current_id)
        if child is None:
            return current_id
        current_id = child["id"]
```

### 2.5 OpenCode (sst/opencode) — Drizzle ORM + Effect 运行时

OpenCode 使用 TypeScript + Effect 运行时构建，Session 是一等公民类型，通过 Drizzle ORM 持久化到 SQLite（本地）或 PostgreSQL（云端）：

```typescript
// 来源：sst/opencode — packages/opencode/src/session/index.ts

const Info = z.object({
  id: z.string(),              // Session UUID
  slug: z.string(),            // 人类可读标识
  projectID: z.string(),       // 所属项目
  title: z.string().optional(),
  directory: z.string(),
  model: z.string(),
  agent: z.string(),
  tokens: z.object({
    input: z.number(),
    output: z.number(),
    reasoning: z.number(),
    cache: z.object({ read: z.number(), write: z.number() }),
  }),
  cost: z.number(),
  compacting: z.boolean(),     // 是否正在压缩
  archived: z.boolean(),
  parentID: z.string().optional(),  // 支持 Fork
  created: z.number(),
  updated: z.number(),
})
```

---

## 三、持久化方案对比

### 3.1 SQLite 文件持久化

**代表框架**：OpenAI Agent SDK (`SQLiteSession`)、HermesAgent、OpenCode

**OpenAI Agent SDK SQLiteSession 的完整数据库 Schema：**

```sql
-- 来源：openai/openai-agents-python — src/agents/memory/sqlite_session.py

CREATE TABLE IF NOT EXISTS agent_sessions (
    session_id TEXT PRIMARY KEY,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS agent_messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL,
    message_data TEXT NOT NULL,     -- JSON-serialized TResponseInputItem
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (session_id) REFERENCES agent_sessions (session_id)
    ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_messages_session_id
    ON agent_messages (session_id, id);
```

**HermesAgent 的增强 Schema（WAL + FTS5）：**

```sql
-- 来源：NousResearch/hermes-agent — hermes_state.py

-- WAL 模式：并发读取不阻塞写入
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;

-- FTS5 全文搜索（标准 unicode61 tokenizer）
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts
USING fts5(
    id UNINDEXED,
    session_id UNINDEXED,
    content,
    tokenize='unicode61'
);

-- FTS5 CJK 支持（trigram tokenizer）
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts_trigram
USING fts5(
    id UNINDEXED,
    session_id UNINDEXED,
    content,
    tokenize='trigram'
);

-- 自动维护 FTS 索引的触发器
CREATE TRIGGER IF NOT EXISTS messages_ai
AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(id, session_id, content)
    VALUES (new.id, new.session_id, new.content_text);
END;
```

| 维度 | 优势 | 劣势 |
|------|------|------|
| 部署复杂度 | 零依赖，单文件 | 不适合分布式部署 |
| 并发性能 | WAL 模式支持多读单写 | 写入竞争会降级到串行 |
| 可移植性 | 文件直接拷贝即可迁移 | 跨机器同步困难 |
| 搜索能力 | FTS5 支持全文检索 | 不支持语义检索 |
| 适用场景 | 单机、本地开发、CLI 工具 | 不适合多用户 SaaS |

### 3.2 Redis 持久化

**代表框架**：OpenAI Agent SDK (`RedisSession`)

```python
# 来源：openai/openai-agents-python — src/agents/extensions/memory/redis_session.py
from agents.extensions.memory import RedisSession

session = RedisSession.from_url(
    "user_123",
    url="redis://localhost:6379/0",
)
result = await Runner.run(agent, "Hello", session=session)
```

Redis 存储结构：以 `session_id` 为 Key，每条消息 JSON 序列化后存入 List，支持 `EXPIRE` 设置 TTL 自动过期。

**适用场景**：多进程 Worker 共享同一会话、需要毫秒级读写延迟。

### 3.3 进程内存（In-Memory）

**代表框架**：OpenHarness、harness9（当前实现）

```python
# 来源：HKUDS/OpenHarness
class QueryEngine:
    def __init__(self):
        self._messages: list[ConversationMessage] = []  # 进程内存
```

```go
// 来源：harness9 — internal/engine/agent_loop.go
contextHistory := []schema.Message{
    {Role: schema.RoleSystem, Content: e.buildSystemPrompt()},
    {Role: schema.RoleUser, Content: userPrompt},
}
// 每个 Turn 追加到 contextHistory 切片，进程退出即消失
```

| 维度 | 优势 | 劣势 |
|------|------|------|
| 实现复杂度 | 零代码复杂度 | 进程重启丢失全部上下文 |
| 读写性能 | 纳秒级访问 | 受进程内存限制 |
| 适用场景 | 无状态 API、单次任务 | 不适合长期对话助手 |

### 3.4 Markdown 文件持久化

**代表框架**：OpenHarness（MEMORY.md）

```python
# 来源：HKUDS/OpenHarness — src/openharness/memory/manager.py

def add_memory_entry(title: str, content: str, memory_type: str = "") -> MemoryHeader:
    """
    Create a new memory markdown file and append its index entry to MEMORY.md.
    Uses exclusive file locking to prevent concurrent modification conflicts.
    """
    slug = _slugify(title)
    memory_file = memory_dir() / f"{slug}.md"

    _atomic_write(memory_file, _format_memory_content(title, content, memory_type))

    with _memory_lock():
        _append_to_index(title, slug, memory_type)

    return MemoryHeader(
        path=memory_file,
        title=title,
        description=content[:200],
        modified_at=time.time(),
        memory_type=memory_type,
    )
```

---

## 四、存储结构设计

### 4.1 OpenAI Agent SDK — TResponseInputItem 格式

消息以 OpenAI Responses API 的原生格式存储，JSON 序列化后写入 SQLite：

```json
{"role": "user", "content": "What city is the Golden Gate Bridge in?"}

{"role": "assistant", "content": "San Francisco"}

{
  "type": "function_call",
  "id": "call_abc123",
  "function": {
    "name": "search_web",
    "arguments": "{\"query\": \"Golden Gate Bridge location\"}"
  }
}

{
  "type": "function_call_output",
  "call_id": "call_abc123",
  "output": "The Golden Gate Bridge is located in San Francisco, California."
}
```

**关键设计**：消息格式与 OpenAI API 完全一致，`add_items` 写入什么，`get_items` 原样取出，Runner 可直接作为 API 的 `input` 参数使用。

### 4.2 OpenHarness — Pydantic 模型 + ContentBlock 多态

```python
# 来源：HKUDS/OpenHarness — src/openharness/engine/messages.py

class TextBlock(BaseModel):
    type: Literal["text"] = "text"
    text: str

class ToolUseBlock(BaseModel):
    type: Literal["tool_use"] = "tool_use"
    id: str
    name: str
    input: dict[str, Any]

class ToolResultBlock(BaseModel):
    type: Literal["tool_result"] = "tool_result"
    tool_use_id: str
    content: str | list[TextBlock]
    is_error: bool = False

class ImageBlock(BaseModel):
    type: Literal["image"] = "image"
    source: dict  # {"type": "base64", "media_type": "image/png", "data": "..."}

ContentBlock = TextBlock | ImageBlock | ToolUseBlock | ToolResultBlock

class ConversationMessage(BaseModel):
    role: Literal["user", "assistant"]
    content: list[ContentBlock] | str

    @property
    def text(self) -> str:
        if isinstance(self.content, str):
            return self.content
        return "".join(b.text for b in self.content if isinstance(b, TextBlock))
```

**Sanitization（对话历史清洗）**：OpenHarness 提供了 `sanitize_conversation_messages()` 函数，在每次 LLM 调用前清洗历史，删除末尾不完整的 `tool_use` 块（会话中断时产生），避免 API 拒绝请求。

### 4.3 HermesAgent — SQLite 多模态内容编码

HermesAgent 使用特殊编码区分纯文本和多模态内容：

```python
# 来源：NousResearch/hermes-agent — hermes_state.py

def _encode_content(content: str | list) -> str:
    """List content (multimodal) is JSON-encoded with NUL-byte prefix."""
    if isinstance(content, list):
        return "\x00" + json.dumps(content)  # NUL 前缀标记为 JSON 编码
    return content

def _decode_content(raw: str) -> str | list:
    if raw.startswith("\x00"):
        return json.loads(raw[1:])
    return raw
```

消息表 Schema 包含完整的工具调用元数据：

```sql
CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    role TEXT NOT NULL,          -- 'user'|'assistant'|'tool'
    content TEXT NOT NULL,       -- 纯文本或 NUL+JSON 多模态
    tool_name TEXT,              -- 工具名称（tool 角色消息）
    tool_calls TEXT,             -- JSON-encoded 工具调用列表
    tool_call_id TEXT,           -- 工具调用 ID（关联请求与结果）
    reasoning TEXT,              -- 思维链推理文本
    created_at REAL NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions(id)
);
```

### 4.4 OpenCode — TypeScript Zod Schema + 多种 Part 类型

```typescript
// 来源：sst/opencode — packages/opencode/src/session/message.ts

export const TextPart = z.object({
  type: z.literal("text"),
  text: z.string(),
})

export const ToolInvocationPart = z.object({
  type: z.literal("tool-invocation"),
  toolInvocationId: z.string(),
  toolName: z.string(),
  state: z.union([
    z.object({ type: z.literal("call"), input: z.any() }),
    z.object({ type: z.literal("result"), output: z.any() }),
    z.object({ type: z.literal("error"), error: z.any() }),
  ]),
})

export const Info = z.object({
  id: z.string(),
  role: z.enum(["user", "assistant"]),
  parts: z.array(MessagePart),
  metadata: z.object({
    time: z.object({ created: z.number(), completed: z.number().optional() }),
    sessionID: z.string(),
    assistant: z.object({
      model: z.string(),
      provider: z.string(),
      cost: z.number(),
      tokens: z.object({
        input: z.number(),
        output: z.number(),
        cache: z.object({ read: z.number(), write: z.number() }),
      }),
    }).optional(),
  }),
})
```

### 4.5 DeepAgents — LangGraph Messages Channel + Delta 存储

DeepAgents 利用 LangGraph 的 `DeltaChannel` 做消息状态优化：

```python
# 来源：langchain-ai/deepagents — libs/deepagents/deepagents/graph.py

class _DeepAgentState(AgentState):
    """Agent state with optimized message storage using delta checkpointing."""
    messages: Annotated[
        list[BaseMessage],
        DeltaChannel(
            _messages_delta_reducer,
            snapshot_frequency=50,  # 每 50 步保存一次完整快照
            # 中间步骤只记录 delta，将 Checkpoint 大小从 O(N²) 降低到 O(N)
        )
    ]
```

---

## 五、生命周期管理

### 5.1 OpenAI Agent SDK — 明确的 CRUD API

```python
from agents import SQLiteSession

session = SQLiteSession("user_123", "conversations.db")

# CREATE — 首次 add_items 时自动创建 session 记录
await session.add_items([{"role": "user", "content": "Hello"}])

# READ — 获取历史消息，支持 limit
items = await session.get_items()          # 全部历史
items = await session.get_items(limit=20)  # 最近 20 条

# UPDATE — 追加新消息
await session.add_items([{"role": "assistant", "content": "Hi!"}])

# DELETE (partial) — 撤销最后一条消息
last = await session.pop_item()

# DELETE (full) — 清空整个会话
await session.clear_session()
```

**限制历史检索量**：

```python
from agents import RunConfig, SessionSettings

# 仅加载最近 50 条消息作为上下文
result = await Runner.run(
    agent,
    "Summarize our recent discussion.",
    session=session,
    run_config=RunConfig(session_settings=SessionSettings(limit=50)),
)
```

**无内置 TTL 机制**：OpenAI Agent SDK 的内置实现不包含自动过期功能，开发者需要在应用层实现定期清理。

### 5.2 HermesAgent — 完整的会话状态机

```python
# 来源：NousResearch/hermes-agent — hermes_state.py

def create_session(self, model: str, source: str = "cli") -> dict:
    session_id = str(uuid.uuid4())
    now = time.time()
    self.conn.execute(
        "INSERT INTO sessions (id, model, source, started_at) VALUES (?, ?, ?, ?)",
        (session_id, model, source, now)
    )
    return {"id": session_id, "model": model, "started_at": now}

def end_session(self, session_id: str, end_reason: str = "normal") -> None:
    self.conn.execute(
        "UPDATE sessions SET ended_at=?, end_reason=? WHERE id=?",
        (time.time(), end_reason, session_id)
    )

def reopen_session(self, session_id: str) -> None:
    """Reverse a session close (for undo operations)."""
    self.conn.execute(
        "UPDATE sessions SET ended_at=NULL, end_reason=NULL WHERE id=?",
        (session_id,)
    )

def replace_messages(self, session_id: str, messages: list[dict]) -> None:
    """Atomically rewrite the entire transcript for retry/undo flows."""
    with self.conn:  # BEGIN IMMEDIATE transaction
        self.conn.execute("DELETE FROM messages WHERE session_id=?", (session_id,))
        for msg in messages:
            self._insert_message(session_id, msg)
```

### 5.3 DeepAgents — Checkpointer 自动快照

LangGraph Checkpointer 在每个 Graph 步骤后自动保存状态，不需要应用层手动管理生命周期：

```python
# 配置 SQLite Checkpointer（持久化）
from langgraph.checkpoint.sqlite.aio import AsyncSqliteSaver

async with AsyncSqliteSaver.from_conn_string("conversations.db") as checkpointer:
    agent = create_deep_agent(checkpointer=checkpointer)

    # 每次 invoke 后，状态自动写入 SQLite
    # snapshot_frequency=50 意味着每 50 步写一次完整快照，中间步骤只写 delta
    result = await agent.ainvoke(
        {"messages": [HumanMessage("你好")]},
        config={"configurable": {"thread_id": "session_001"}}
    )
```

---

## 六、上下文注入与压缩机制

### 6.1 滑动窗口（Sliding Window）

**OpenAI Agent SDK 的 `session_input_callback`**：

```python
# 来源：openai/openai-agents-python

def keep_recent_history(history: list, new_input: list) -> list:
    """Keep only the last 10 history items, implements a sliding window."""
    return history[-10:] + new_input

result = await Runner.run(
    agent,
    "Continue from the latest updates only.",
    session=session,
    run_config=RunConfig(session_input_callback=keep_recent_history),
)
```

**优点**：实现简单，延迟可预期。**缺点**：丢失早期重要上下文（如用户姓名、初始需求）。

### 6.2 双层 LLM 压缩（Summarization Compaction）

**OpenHarness 的微压缩 + 全量压缩**：

```python
# 来源：HKUDS/OpenHarness — src/openharness/engine/query.py

async def auto_compact_if_needed(
    messages: list[ConversationMessage],
    context: QueryContext,
) -> list[ConversationMessage]:
    """
    Two-tier compaction strategy:
    1. Micro-compaction: cheaply clear old tool result content
    2. Full compaction: LLM-based summarization when micro-compaction is insufficient
    """
    estimated_tokens = estimate_tokens(messages)

    if estimated_tokens < context.auto_compact_threshold:
        return messages  # 不需要压缩

    # 第一层：微压缩 — 清除旧工具结果的详细内容，保留摘要
    compacted = micro_compact(messages)

    if estimate_tokens(compacted) < context.auto_compact_threshold:
        return compacted  # 微压缩够用

    # 第二层：全量 LLM 压缩
    summary = await llm_summarize(
        messages=messages,
        system_prompt="Summarize the key information from this conversation...",
        api_client=context.api_client,
        model=context.model,
    )

    # 返回：摘要消息 + 最近几条消息
    return [
        ConversationMessage(role="user", content=[
            TextBlock(text=f"[Previous conversation summary]\n{summary}")
        ])
    ] + messages[-context.keep_recent_n:]


def micro_compact(messages: list[ConversationMessage]) -> list[ConversationMessage]:
    """Clear old tool result content (keep only a short snippet)."""
    result = []
    for i, msg in enumerate(messages):
        if i < len(messages) - context.keep_recent_n:
            msg = _truncate_tool_results(msg, max_chars=200)
        result.append(msg)
    return result
```

**反应式压缩**（API 报 `prompt_too_long` 时触发）：

```python
async def run_query(context: QueryContext, ...) -> AsyncIterator:
    for turn in range(context.max_turns):
        try:
            context.messages = await auto_compact_if_needed(context.messages, context)
            async for event in context.api_client.stream_message(...):
                yield event
        except APIError as e:
            if is_context_overflow_error(e):
                context.messages = await force_compact(context.messages, context)
                continue  # 重试当前 Turn
            raise
```

### 6.3 Token Budget 控制

**DeepAgents 的 O(N) Delta 存储 + Snapshot**：

```python
# 来源：langchain-ai/deepagents

class _DeepAgentState(AgentState):
    messages: Annotated[
        list[BaseMessage],
        DeltaChannel(
            reducer=_messages_delta_reducer,
            snapshot_frequency=50,
            # 中间步骤只记录 delta (added/removed messages)
            # 将 Checkpoint 大小从 O(N²) 降低到 O(N)
        )
    ]
```

**LangGraph 的 `REMOVE_ALL_MESSAGES` Sentinel**：

```python
# 来源：langchain-ai/deepagents — libs/deepagents/deepagents/_messages_reducer.py

from langgraph.graph.message import REMOVE_ALL_MESSAGES, RemoveMessage

def _messages_delta_reducer(
    existing: list[BaseMessage],
    updates: list[BaseMessage | RemoveMessage],
) -> list[BaseMessage]:
    """
    Handles three write types:
    1. New messages: append (deduplicate by ID)
    2. RemoveMessage(id=X): remove specific message by ID
    3. REMOVE_ALL_MESSAGES: reset everything (used for full compaction)
    """
    if any(u is REMOVE_ALL_MESSAGES for u in updates):
        last_idx = max(i for i, u in enumerate(updates) if u is REMOVE_ALL_MESSAGES)
        remaining_writes = updates[last_idx + 1:]
        return _apply_updates([], remaining_writes)

    return _apply_updates(existing, updates)
```

### 6.4 HermesAgent 的会话分叉压缩（Compression Chain）

```python
# 来源：NousResearch/hermes-agent — hermes_state.py

def compress_session(
    self,
    original_session_id: str,
    summary: str,
    recent_messages: list[dict],
) -> str:
    """
    Compress a session by:
    1. Creating a new child session with parent_session_id pointing to original
    2. Writing the summary as the first message in the child session
    3. Appending recent messages to preserve immediate context

    The original session remains intact (for auditing/rollback).
    Returns the new child session_id.
    """
    child_id = str(uuid.uuid4())
    self.conn.execute(
        """INSERT INTO sessions (id, model, source, parent_session_id, started_at)
           SELECT model, source, ?, ? FROM sessions WHERE id=?""",
        (child_id, original_session_id, time.time(), original_session_id)
    )

    # 摘要作为第一条消息
    self._insert_message(child_id, {
        "role": "user",
        "content": f"[Conversation summary]\n{summary}"
    })

    # 追加最近的原始消息
    for msg in recent_messages:
        self._insert_message(child_id, msg)

    return child_id
```

### 6.5 Claude API 的 Context Compaction

Claude Agent SDK 的 context compaction 在**服务端**执行：

- **被动 compaction**：对话历史超过上下文窗口时，Claude 自动执行摘要并将压缩后的内容作为新的上下文开头
- **主动 compaction**：通过 `extended_thinking` 允许 Claude 在长对话中自动维护上下文连贯性
- **Prompt Caching**：配合 `cache_control` 参数，对稳定的历史前缀进行缓存，降低长对话的 token 成本

**OpenCode 的 compacting 标志**：

```typescript
// 来源：sst/opencode
const Info = z.object({
  compacting: z.boolean(),    // 标记正在压缩中，UI 显示压缩状态
  parentID: z.string().optional(),  // 压缩后 fork 出的新 session 指向原始 session
})

// 压缩流程：
// 1. 设置 session.compacting = true
// 2. 调用 LLM 生成摘要
// 3. 创建新 session（parentID = 当前 session.id）
// 4. 将摘要 + 最近消息写入新 session
// 5. 切换到新 session 继续工作
```

---

## 七、各框架横向对比

| 对比维度 | OpenAI Agent SDK | DeepAgents | OpenHarness | HermesAgent | OpenCode | Claude Agent SDK |
|---------|-----------------|------------|-------------|-------------|----------|-----------------|
| **Session 绑定** | String session_id | LangGraph thread_id | 进程内列表 + 命名文件 | SQLite UUID | Drizzle ORM UUID | 服务端托管 |
| **持久化后端** | 8 种（SQLite/Redis 等） | 插件化 Checkpointer | 进程内/文件 | SQLite WAL | SQLite/PG | 服务端存储 |
| **消息格式** | OpenAI Responses API 原生 | LangGraph BaseMessage | Pydantic ContentBlock | JSON+NUL 编码 | Zod Part 类型 | Anthropic Messages |
| **Context 压缩** | 滑动窗口 callback | REMOVE_ALL_MESSAGES | 双层 LLM 压缩 | Session 分叉链 | compacting 标志 | 服务端自动 |
| **Token 控制** | SessionSettings limit | DeltaChannel O(N) | 阈值触发 | 无明确限制 | 无明确限制 | 服务端管理 |
| **全文搜索** | 无 | 无 | 无 | FTS5 unicode+trigram | 无 | 无 |
| **并发安全** | RLock/Thread-Local | LangGraph 内部保证 | 文件锁 | WAL+jitter 重试 | Effect 运行时 | 服务端保证 |
| **会话恢复** | 通过 session_id | 通过 thread_id | /resume 命令 | reopen_session | 列表选择 | conversation_id |
| **TTL/自动过期** | 无（需应用层实现） | 无 | 无 | 无 | 无 | 服务端策略 |
| **会话分叉/回滚** | pop_item | RemoveMessage | 无 | replace_messages | Fork session | 无 |

---

## 八、设计建议（针对 harness9 项目）

### 8.1 当前状态分析

harness9 当前的 `engine/agent_loop.go` 中，`contextHistory` 是一个局部变量，在 `Run()`/`RunStream()` 的单次调用内存在，调用结束即销毁：

```go
// 当前实现：每次 Run 都是全新的上下文，没有跨调用持久化
contextHistory := []schema.Message{
    {Role: schema.RoleSystem, Content: e.buildSystemPrompt()},
    {Role: schema.RoleUser, Content: userPrompt},
}
```

这意味着：用户在 TUI 中每次输入都是一次全新的 Run，**之前的对话内容完全丢失**。这对于 AI 编程助手来说是严重的功能缺陷。

### 8.2 推荐方案：SQLite 分层架构

参考 HermesAgent 和 OpenAI Agent SDK 的实现，为 harness9 设计如下短期记忆模块：

**目录结构建议：**

```
internal/
└── memory/
    ├── session.go          # Session 接口（使用者侧定义）
    ├── sqlite_session.go   # SQLite 实现
    ├── memory_session.go   # 内存实现（用于测试）
    └── compaction.go       # 上下文压缩策略
```

**Session 接口设计（Go 风格）：**

```go
// internal/memory/session.go

// Session 管理单个对话会话的消息历史。
// 接口定义在 memory 包（使用者侧），由 sqlite_session.go 等实现。
type Session interface {
    SessionID() string
    GetMessages(ctx context.Context, limit int) ([]schema.Message, error)
    AddMessages(ctx context.Context, msgs []schema.Message) error
    PopMessage(ctx context.Context) (*schema.Message, error)
    Clear(ctx context.Context) error
    Close() error
}
```

**SQLite 实现的 Schema：**

```go
// internal/memory/sqlite_session.go

const schemaSQL = `
CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL,  -- Unix timestamp
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   TEXT    NOT NULL,
    role         TEXT    NOT NULL,  -- 'system'|'user'|'assistant'
    content      TEXT    NOT NULL,
    tool_calls   TEXT,              -- JSON-encoded []schema.ToolCall
    tool_call_id TEXT,              -- 关联 Observation 消息
    created_at   INTEGER NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_messages_session
    ON messages (session_id, id);
`
```

**与 AgentEngine 的集成（最小改动原则）：**

```go
// 修改 AgentEngine 接受可选 Session
type AgentEngine struct {
    // ...现有字段
    session memory.Session  // 可选，nil 表示不持久化
}

func WithSession(s memory.Session) Option {
    return func(e *AgentEngine) { e.session = s }
}

// runLoop 中集成 Session
func (e *AgentEngine) runLoop(ctx context.Context, userPrompt string, ...) error {
    var contextHistory []schema.Message

    if e.session != nil {
        history, err := e.session.GetMessages(ctx, 0)
        if err != nil {
            return fmt.Errorf("加载会话历史失败：%w", err)
        }
        contextHistory = history
    }

    // System prompt 只在历史为空时注入
    if len(contextHistory) == 0 || contextHistory[0].Role != schema.RoleSystem {
        contextHistory = append([]schema.Message{
            {Role: schema.RoleSystem, Content: e.buildSystemPrompt()},
        }, contextHistory...)
    }

    contextHistory = append(contextHistory, schema.Message{
        Role:    schema.RoleUser,
        Content: userPrompt,
    })

    startLen := len(contextHistory)

    // ... 主循环（现有逻辑不变）

    if e.session != nil {
        newMsgs := contextHistory[startLen:]
        if err := e.session.AddMessages(ctx, newMsgs); err != nil {
            log.Print(logfmt.FormatMsg("engine", "警告：保存会话历史失败："+err.Error()))
        }
    }

    return nil
}
```

### 8.3 上下文压缩策略

**第一层：消息数量限制（Sliding Window）**

```go
// internal/memory/compaction.go

// SlidingWindowCompact 保留最近 N 条消息（system prompt 始终保留）。
func SlidingWindowCompact(msgs []schema.Message, maxMessages int) []schema.Message {
    if len(msgs) <= maxMessages {
        return msgs
    }
    system := msgs[0]
    recent := msgs[len(msgs)-maxMessages+1:]
    return append([]schema.Message{system}, recent...)
}
```

**第二层：Token 预算控制（Token Budget）**

```go
// 基于 token 估算的裁剪（粗略按字节/4 估算 token 数）
func TokenBudgetCompact(msgs []schema.Message, maxTokens int) []schema.Message {
    total := estimateTokens(msgs)
    if total <= maxTokens {
        return msgs
    }
    result := []schema.Message{msgs[0]} // system
    for i := len(msgs) - 1; i >= 1; i-- {
        if estimateTokens(msgs[i:]) <= maxTokens-estimateTokens(result) {
            result = append(result, msgs[i:]...)
            break
        }
    }
    return result
}

func estimateTokens(msgs []schema.Message) int {
    total := 0
    for _, m := range msgs {
        total += len(m.Content) / 4  // 粗略：4 字节≈1token
    }
    return total
}
```

### 8.4 持久化路径规范

```
~/.harness9/
├── sessions.db          # SQLite 数据库（所有会话共享一个文件）
└── config.json          # 用户配置
```

Session ID 生成策略：
- **自动模式**：`time.Now().Format("20060102-150405") + "-" + randHex(4)`，如 `20260517-143022-a3f1`
- **命名模式**：用户通过 `--name my-project` 指定，`my-project` 直接作为 session_id

### 8.5 TUI 集成建议

1. 在 StatusBar 显示当前 Session ID（短格式）和消息数量
2. 支持 `/new` 命令新建会话、`/resume` 命令列出历史会话
3. 上下文压缩时在 Spinner 区域显示"压缩上下文中..."
4. Tab 补全支持 session_id 补全

### 8.6 实现优先级

| 优先级 | 功能 | 难度 | 工作量估计 |
|--------|------|------|------------|
| P0 | Session 接口定义 + MemorySession（测试用） | 低 | 1 天 |
| P0 | SQLiteSession 基本实现（CRUD） | 中 | 2 天 |
| P0 | AgentEngine 集成 Session 接口 | 低 | 0.5 天 |
| P1 | TUI /new、/resume 命令 | 中 | 1 天 |
| P1 | SlidingWindow 压缩策略 | 低 | 0.5 天 |
| P2 | Token Budget 压缩 | 中 | 1 天 |
| P2 | TUI Session 状态显示 | 低 | 0.5 天 |
| P3 | LLM-based 摘要压缩 | 高 | 3 天 |
| P3 | FTS5 会话搜索 | 中 | 1 天 |

---

## 九、关键结论

主流框架的短期记忆实现高度收敛于以下三件套：

1. **SQLite 单文件持久化** — 零依赖、可移植、WAL 模式支持并发
2. **消息 JSON 序列化** — 消息格式与 LLM API 保持一致，减少转换层
3. **应用层压缩** — 框架本身不强制压缩策略，由调用方选择（滑动窗口/LLM 摘要/Token Budget）

**最佳参考**：
- **接口设计**：OpenAI Agent SDK 的 `SessionABC`（四个方法：get/add/pop/clear）是最简洁、可扩展的抽象
- **Schema 设计**：HermesAgent 的 WAL + 声明式 Schema 协调（startup 自动补列，而非版本化 migration）适合 Go+SQLite 方案
- **压缩策略**：OpenHarness 的双层压缩（微压缩 + LLM 全量压缩 + 反应式触发）是目前最完善的开源实现
