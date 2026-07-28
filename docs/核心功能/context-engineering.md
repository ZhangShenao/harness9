# Context Engineering 上下文管理实现原理

## 1. 背景与设计目标

### 1.1 问题背景

harness9 的 `runLoop` 原本将 `contextHistory` 声明为局部变量，每次 `Run()` 调用全新初始化，会话间无法延续。随着对话历史不断增长，还面临以下挑战：

- **无状态**：进程重启后会话历史全部丢失，用户无法恢复之前的工作
- **上下文溢出**：历史消息无限增长，超出 LLM 上下文窗口后 API 报错或截断
- **粗暴压缩**：早期 SlidingWindowCompactor 仅按消息条数截断，忽略 token 实际用量，压缩时机不精准
- **不透明**：用户无法感知当前上下文用量，不知道何时触发了压缩

### 1.2 设计目标

Context Engineering 模块覆盖以下能力：

| 目标 | 实现机制 |
|------|---------|
| **会话持久化** | SQLite WAL 模式，进程重启可恢复 |
| **精准压缩时机** | Token Budget 感知 LLM context window，80% 阈值触发 |
| **孤立工具对修复** | 双向修复，保证 API 兼容性 |
| **实际 Token 用量** | 从 API 响应的 usage 字段提取，事后更新显示 |
| **用户可见** | TUI 实时展示 token 用量和颜色告警，压缩时发出通知 |

---

## 2. 整体架构

```
┌──────────────────────────────────────────────────────────────────┐
│                          AgentEngine                              │
│                                                                  │
│   WithSession(sess)       →  session   memory.Session            │
│   WithCompactor(comp)     →  compactor memory.Compactor          │
│   WithContextWindow(tok)  →  contextWindow int                   │
│                                                                  │
│   runLoop()（每个 Turn）：                                         │
│     1. loadHistoryWith()    ← Session 加载 + system prompt 注入   │
│     2. EstimateTokens()     ← 预检：预估 token 用量              │
│     3. applyCompactionWith()← 压缩（SummarizationCompactor）       │
│     4. tokenUpdate(est)     ← 发出估算值给 TUI                    │
│     5. em.generate()        ← LLM 调用，获取 *Usage              │
│     6. tokenUpdate(actual)  ← 用实际值更新 TUI                   │
│     7. saveHistoryWith()    ← 新增消息写回 Session               │
└──────────────┬───────────────────────────────────────────────────┘
               │
               ▼
┌──────────────────────────────────────────────────────────────────┐
│                       internal/memory/                            │
│                                                                  │
│  Session (interface)          Manager                            │
│  ├── GetMessages(limit)       ├── NewSession()   → SQLiteSession │
│  ├── AddMessages(msgs)        ├── OpenSession(id)→ SQLiteSession │
│  ├── PopMessage()             ├── ListSessions() → []SessionInfo │
│  └── Clear()                 └── DeleteSession()                │
│                                                                  │
│  SQLiteSession (主实现)        MemorySession (测试用)             │
│  ├── WAL 模式 SQLite           └── sync.Mutex + []Message        │
│  ├── 事务性 AddMessages                                           │
│  └── tool_calls JSON 序列化                                      │
│                                                                  │
│  Compactor (interface)                                           │
│  ├── SummarizationCompactor  ← LLM 摘要压缩（默认，含回退）        │
│  ├── TokenBudgetCompactor    ← Token Budget 感知截断（回退策略）   │
│  └── SlidingWindowCompactor  ← 按消息条数裁剪（简单回退方案）       │
│                                                                  │
│  token.go                    model_limits.go                     │
│  ├── EstimateTokens()        ├── GetModelLimits(name)            │
│  ├── EstimateToolTokens()    └── ModelLimits{ContextTokens, ...} │
│  └── FormatTokenCount()                                          │
└──────────────────────────────────────────────────────────────────┘
               │
               ▼
     ~/.harness9/sessions.db  (SQLite 持久化文件)
```

---

## 3. 包结构

```
internal/memory/
├── session.go               # Session 接口 + SessionInfo 类型定义
├── manager.go               # Manager：SQLite 连接持有者 + 会话 CRUD
├── sqlite_session.go        # SQLiteSession：WAL 模式 SQLite 持久化实现
├── mem_session.go           # MemorySession：纯内存实现（测试用）
├── compaction.go            # Compactor 接口 + TokenBudgetCompactor + SlidingWindowCompactor
├── summarization.go         # SummarizationCompactor：LLM 摘要压缩（默认策略）
├── token.go                 # Token 估算工具函数
├── sqlite_session_test.go
├── mem_session_test.go
├── manager_test.go
├── compaction_test.go
└── summarization_test.go

internal/provider/
├── model_limits.go          # 模型 context window 注册表
└── model_limits_test.go
```

---

## 4. 核心接口

### 4.1 Session 接口

```go
// Session 管理单个会话的消息历史与规划状态。
type Session interface {
    SessionID() string
    // GetMessages 返回历史消息；limit=0 返回全部，limit>0 返回最近 limit 条。
    GetMessages(ctx context.Context, limit int) ([]schema.Message, error)
    // AddMessages 追加新消息到会话历史。
    AddMessages(ctx context.Context, msgs []schema.Message) error
    // PopMessage 删除并返回最新一条消息（undo 用）；无消息时返回 nil, nil。
    PopMessage(ctx context.Context) (*schema.Message, error)
    // Clear 清空会话历史。
    Clear(ctx context.Context) error
    // GetTodos 返回该会话已持久化的任务列表。无任务时返回 nil, nil。
    GetTodos(ctx context.Context) ([]planning.TodoItem, error)
    // SaveTodos 原子性保存任务列表（write-replace 语义）。
    SaveTodos(ctx context.Context, items []planning.TodoItem) error
}
```

### 4.2 Compactor 接口

```go
// Compactor 在将历史消息注入 LLM 上下文前进行裁剪，防止超出上下文窗口。
type Compactor interface {
    Compact(msgs []schema.Message) []schema.Message
}
```

### 4.3 Manager

```go
type Manager struct{ db *sql.DB; toolResultsDir string }

// NewManager 打开（或创建）SQLite 数据库，初始化 Schema，支持可选配置。
func NewManager(dbPath string, opts ...ManagerOption) (*Manager, error)
// WithToolResultsDir 设置 offload 文件根目录；DeleteSession 会级联清理对应子目录。
func WithToolResultsDir(dir string) ManagerOption
func (m *Manager) NewSession(ctx context.Context) (Session, error)
func (m *Manager) OpenSession(ctx context.Context, id string) (Session, error)
func (m *Manager) ListSessions(ctx context.Context) ([]SessionInfo, error)
func (m *Manager) DeleteSession(ctx context.Context, id string) error
func (m *Manager) Close() error
```

`Manager` 是整个进程的单一 SQLite 连接持有者，所有 `SQLiteSession` 共享同一个 `*sql.DB`。

---

## 5. SQLite Schema

持久化路径：`~/.harness9/sessions.db`

```sql
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT    PRIMARY KEY,   -- UUID v4
    created_at INTEGER NOT NULL,      -- Unix timestamp（秒）
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   TEXT    NOT NULL,
    role         TEXT    NOT NULL,    -- 'system'|'user'|'assistant'
    content      TEXT    NOT NULL,
    tool_calls   TEXT,                -- JSON，仅 assistant 消息有值
    tool_call_id TEXT,                -- 仅 Observation（user）消息有值
    created_at   INTEGER NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, id);
```

**关键设计决策：**

- **WAL 模式**：`journal_mode=WAL` 允许并发读写，适合 TUI + 引擎同时访问
- **`tool_calls` 列**：存储 `[]schema.ToolCall` 的 JSON 序列化，读取时反序列化，与现有类型完全对齐
- **System Prompt 不持久化**：system 消息每次由 `loadHistoryWith` 重新注入，不写入 DB，保持 prompt 可随配置更新而变化
- **ON DELETE CASCADE**：删除会话时自动清理所有关联消息

### 5.1 SQLiteSession 实现要点

**GetMessages：**

```go
// limit=0：全量升序查询
SELECT role, content, tool_calls, tool_call_id
FROM messages WHERE session_id = ?
ORDER BY id ASC

// limit>0：先 DESC LIMIT，再内存反转，获得"最近 N 条按时间升序"
SELECT ... ORDER BY id DESC LIMIT ?
// → 内存反转 → 升序
```

**AddMessages（事务）：**

```
BEGIN TX
  INSERT INTO messages ...（多条）
  UPDATE sessions SET updated_at = ? WHERE id = ?
COMMIT
```

**PopMessage（原子删除）：**

```
BEGIN TX
  SELECT ... ORDER BY id DESC LIMIT 1
  反序列化 tool_calls（失败则 ROLLBACK，消息不丢失）
  DELETE FROM messages WHERE id = ?
COMMIT
```

---

## 6. 压缩策略

harness9 提供三种压缩策略，按优先级从高到低排列：

| 策略 | 文件 | 默认 | 适用场景 |
|------|------|:----:|---------|
| `SummarizationCompactor` | `summarization.go` | ✅ | 长任务、信息密集型对话，语义保留最佳 |
| `TokenBudgetCompactor` | `compaction.go` | — | Provider 不可用时的自动回退策略 |
| `SlidingWindowCompactor` | `compaction.go` | — | 快速原型、成本极度敏感场景 |

### 6.1 SummarizationCompactor（LLM 摘要压缩，默认）

`SummarizationCompactor` 调用 LLM 将旧消息压缩为结构化摘要，在语义保留方面显著优于截断策略。

**接口设计：**

```go
// Summarizer 定义在 memory 包（使用者侧），任何 provider.LLMProvider 均满足此接口。
type Summarizer interface {
    Generate(ctx context.Context, messages []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error)
}
```

**配置：**
- `Provider Summarizer` — 执行摘要的 LLM
- `MaxTokens int` — 触发压缩的 token 预算（通常 `contextWindow × 80%`）
- `MinTailMessages int` — 尾部强制保留的最少消息条数（默认 6）
- `Fallback Compactor` — Provider 调用失败时的回退策略（默认 `TokenBudgetCompactor`）

**压缩算法：**

```
输入：msgs = [system, msg1, ..., msgN]

1. EstimateTokens(msgs) ≤ MaxTokens → 直接返回（无需压缩）
2. msgs[0].Role ≠ RoleSystem → 直接返回（防御）
3. 分割：head = msgs[1 : N-minTail]，tail = msgs[N-minTail:]
4. len(head) == 0 → 直接返回（没有可摘要的消息）
5. 调用 summarize(head) → summary string
6. 成功：返回 [system, {user: "[Conversation Summary]\n"+summary}, ...tail]
           + repairOrphanedToolPairs()
7. 失败：回退 Fallback.Compact(msgs)
```

**增量更新机制：**

当 head 中已含有上次摘要消息（以 `[Conversation Summary]` 开头），`summarize` 会提取旧摘要并构造增量更新 prompt：

```
<previous-summary>
{上次摘要内容}
</previous-summary>

New conversation to merge:
{新对话文本}
```

这避免了多轮压缩后信息叠加丢失的问题。

**摘要输出格式（summaryTemplate）：**

```
**Goal:** What the user is trying to accomplish.
**Progress:** Key actions taken and their results.
**Key Decisions:** Important choices and rationale.
**Next Steps:** What was planned or pending.
**Critical Context:** Facts, file paths, variable names, or constraints the agent must remember.
```

**与 TokenBudgetCompactor 的对比：**

| 维度 | TokenBudgetCompactor | SummarizationCompactor |
|------|---------------------|------------------------|
| 语义保留 | 截断（旧消息完全丢失） | LLM 摘要（关键信息保留） |
| 速度 | 极快（无 LLM 调用） | 有额外 LLM 调用延迟 |
| 成本 | 零 | 摘要 API 费用 |
| 可用性 | 始终可用 | 依赖 Provider 可用性 |
| 推荐场景 | 快速原型、成本敏感 | 长任务、信息密集型对话 |

### 6.2 TokenBudgetCompactor（Token Budget 感知，回退策略）

配置：
- `MaxTokens int` — 最大允许 token 数（通常为 `contextWindow × 80%`）
- `MinTailMessages int` — 尾部强制保留的最少消息条数（默认 6，保证对话连贯性）

```go
func NewTokenBudgetCompactor(contextWindow int) *TokenBudgetCompactor {
    return &TokenBudgetCompactor{
        MaxTokens:       contextWindow * 80 / 100,
        MinTailMessages: 6,
    }
}
```

**压缩算法：**

```
输入：msgs = [system, msg1, ..., msgN]
非 system 消息数 = nonSystemCount = len(msgs) - 1（跳过 msgs[0]）

1. 若 nonSystemCount ≤ MinTailMessages，直接返回（保护最小尾部）
2. 估算总 token 数 = EstimateTokens(msgs)
3. 若 totalTokens ≤ MaxTokens，直接返回（未超预算）
4. 二分搜索：找最大的 tailLen ∈ [MinTailMessages, nonSystemCount-1]，
   使得 EstimateTokens([system] + msgs[N-tailLen:]) ≤ MaxTokens
5. 取最终 tail = msgs[len-tailLen:]
6. 修复孤立工具对：repairOrphanedToolPairs([system] + tail)
7. 返回修复后的消息列表
```

**为何用 80% 而非 100%？**

- 保留 20% 余量给工具定义（bash/read_file 工具描述可消耗 10-30K tokens）
- 避免因估算误差（char÷4 是近似值）导致 API 超限
- 给 LLM 生成输出保留空间

### 6.3 SlidingWindowCompactor（按消息条数，简单回退）

配置：`MaxMessages int`（默认 100，含 system prompt）

```
输入：msgs = [system, msg1, msg2, ..., msgN]

1. 若 len(msgs) ≤ MaxMessages，直接返回原切片（无需压缩）
2. 计算窗口起点：startIdx = len(msgs) - MaxMessages + 1
3. 【边界修正 A】向前回溯孤立的 Observation：
   while startIdx > 1 AND msgs[startIdx].ToolCallID != "" {
       startIdx--
   }
4. 组合：candidate = [msgs[0]] + msgs[startIdx:]
5. 【边界修正 B】调用 repairOrphanedToolPairs 双向修复（见 6.4）
6. 返回修复结果
```

`SlidingWindowCompactor` 不感知 token 用量，适合快速原型场景，生产环境推荐使用 `SummarizationCompactor`。

> **注意：** 步骤 3（向前回溯）只能处理"窗口起点恰好切在 Observation 中间"的情况（A 类孤立）。若窗口保留了一个带 ToolCalls 的 assistant 消息但其对应 tool_result 被截掉（B 类孤立），则需步骤 5 的双向修复兜底。

### 6.4 孤立工具对修复（repairOrphanedToolPairs）

`TokenBudgetCompactor` 和 `SlidingWindowCompactor` 在截断后均调用此函数，处理两类孤立消息：

**类型 A：孤立 tool_result**（有 `ToolCallID` 但无对应 `ToolCalls` 消息）

```
截断前：[system][assistant:tool_call_id=x,tool_calls=[bash]][user:tool_call_id=x][assistant:结果]
截断后：                                                   [user:tool_call_id=x][assistant:结果]
                                                           ↑ 孤立 tool_result → 删除
```

**类型 B：孤立 tool_call**（有 `ToolCalls` 但无对应 tool_result）

```
截断后：[system][assistant:tool_calls=[bash]][assistant:结果]
                ↑ 孤立 tool_call → 插入 stub user 消息作为 tool_result
```

修复逻辑（双向扫描）：

```go
func repairOrphanedToolPairs(msgs []schema.Message) []schema.Message {
    // Pass 1：收集存在的 tool_call IDs
    existingIDs := map[string]bool{}
    for _, msg := range msgs {
        for _, tc := range msg.ToolCalls {
            existingIDs[tc.ID] = true
        }
    }

    // Pass 2：删除孤立 tool_result；为孤立 tool_call 插入 stub
    var result []schema.Message
    for _, msg := range msgs {
        if msg.ToolCallID != "" && !existingIDs[msg.ToolCallID] {
            continue // 删除孤立 tool_result
        }
        result = append(result, msg)
        if len(msg.ToolCalls) > 0 {
            for _, tc := range msg.ToolCalls {
                // 检查是否有对应 tool_result
                hasResult := false
                for _, m2 := range msgs {
                    if m2.ToolCallID == tc.ID {
                        hasResult = true
                        break
                    }
                }
                if !hasResult {
                    // 插入 stub tool_result
                    result = append(result, schema.Message{
                        Role:       schema.RoleUser,
                        Content:    "[context truncated]",
                        ToolCallID: tc.ID,
                    })
                }
            }
        }
    }
    return result
}
```

**为什么需要双向修复？**

LLM API（尤其是 Anthropic Messages API）要求 tool_call / tool_result 必须配对出现。若截断后出现孤立消息，API 调用会报 400 错误。`TokenBudgetCompactor` 和 `SlidingWindowCompactor` 均调用 `repairOrphanedToolPairs`，提供完整双向修复保障。

---

## 7. Token 估算与模型感知

### 7.1 Token 估算（internal/memory/token.go）

```go
const charsPerToken = 4  // 业界标准近似值（DeepAgents、HermesAgent、OpenCode 均采用此值）

// EstimateTokens 估算消息列表的 token 用量。
func EstimateTokens(msgs []schema.Message) int

// EstimateToolTokens 估算工具定义列表的 token 用量。
// 工具定义（JSON Schema）往往占用大量 token（10-30K），必须纳入预检计算。
func EstimateToolTokens(tools []schema.ToolDefinition) int

// FormatTokenCount 将 token 数格式化为人类可读字符串。
// 示例：500 → "500"，45200 → "45.2K"，1200000 → "1.2M"
func FormatTokenCount(n int) string
```

**为什么用 char÷4 而非精确 tokenizer？**

- 无需引入 tiktoken 等外部依赖，保持零依赖原则
- 在 GPT/Claude 等模型上误差通常在 ±10% 以内
- 压缩决策已预留 20% 缓冲，估算误差在容忍范围内
- 实际 token 用量在 LLM 调用后通过 API 响应的 `usage` 字段校正

### 7.2 模型 Context Window 注册表（internal/provider/model_limits.go）

```go
type ModelLimits struct {
    ContextTokens int  // 输入上下文窗口大小（tokens）
    OutputTokens  int  // 最大输出 token 数
}

// GetModelLimits 根据模型名称返回 context window 限制。
// 自动剥除 "openai/" 等路由前缀（如 OpenRouter 格式 "openai/gpt-4o"）。
// 未知模型返回 256K 保守默认值。
func GetModelLimits(modelName string) ModelLimits
```

覆盖范围（截至 2026-05）：

| 系列 | 代表模型 | Context Window |
|------|---------|---------------|
| Claude 4.x | claude-opus-4, claude-sonnet-4 | 200K |
| Claude 3.x | claude-3-5-sonnet, claude-3-opus | 200K |
| GPT-4o | gpt-4o, gpt-4o-mini | 128K |
| GPT-4.5 | gpt-4.5-preview | 128K |
| o-series | o3, o4-mini | 200K |
| Gemini 2.0+ | gemini-2.0-flash | 1M |
| DeepSeek | deepseek-chat, deepseek-r1 | 64K |
| Qwen | qwen-plus, qwen-max | 128K |
| 未知模型 | — | 256K（保守默认）|

### 7.3 实际 Token 用量（API Response Usage）

API 响应中包含实际 token 用量，比字符估算更准确。在 LLM 调用完成后，引擎用实际值更新 TUI 显示。

**schema.Usage 类型：**

```go
// Usage 记录单次 LLM API 调用的 token 用量，由 Provider 从 API 响应中提取。
type Usage struct {
    InputTokens  int `json:"input_tokens"`
    OutputTokens int `json:"output_tokens"`
}
```

**LLMProvider 接口更新：**

```go
// Generate 返回 (*schema.Message, *schema.Usage, error)。
// Usage 包含本次调用的实际 token 用量（可能为 nil）。
Generate(ctx context.Context, messages []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error)
```

**各 Provider 实现：**

- **OpenAI（非流式）**：从 `resp.Usage.PromptTokens` / `resp.Usage.CompletionTokens` 提取
- **OpenAI（流式）**：请求时设置 `StreamOptions.IncludeUsage = true`，从末尾 chunk 的 `Usage.PromptTokens` 提取
- **Anthropic（非流式）**：从 `resp.Usage.InputTokens` / `resp.Usage.OutputTokens` 提取
- **Anthropic（流式）**：从 `message_start` 事件的 `Message.Usage.InputTokens` 提取
- **Mock Provider**：返回 `nil`（测试桩不模拟 API 调用）

**引擎中的更新时序：**

```
Turn N:
  1. tokenUpdate(estimated, window)   ← LLM 调用前：发送估算值（用于压缩决策和初始显示）
  2. em.generate() → returns Usage    ← LLM 调用
  3. tokenUpdate(actual, window)      ← LLM 调用后：用实际值覆盖（若 usage != nil）
```

TUI 用户会看到先显示估算值，LLM 响应返回后刷新为实际值，信息更准确。

---

## 8. AgentEngine 集成

### 8.1 新增 Option 与字段

```go
type AgentEngine struct {
    // ...现有字段
    contextWindow int          // 模型 context window（tokens），用于 TUI 展示，0 表示未知
    mu            sync.RWMutex // 保护 session 和 compactor，防止与 TUI goroutine 竞争
    session       memory.Session
    compactor     memory.Compactor
}

func WithSession(s memory.Session) Option
func WithCompactor(c memory.Compactor) Option
func WithContextWindow(tokens int) Option

// SetSession 替换当前会话，供 TUI /new、/resume 切换时调用。线程安全。
func (e *AgentEngine) SetSession(s memory.Session)
```

### 8.2 runLoop 预检（Preflight Token Check）

```go
func (e *AgentEngine) runLoop(ctx context.Context, userPrompt string, ...) error {
    // 快照 session/compactor，避免与 TUI goroutine 的 SetSession 产生竞争
    e.mu.RLock()
    sess, comp := e.session, e.compactor
    e.mu.RUnlock()

    contextHistory, startLen := e.loadHistoryWith(ctx, userPrompt, sess)

    for {
        availableTools := e.registry.GetAvailableTools()
        toolTokens := memory.EstimateToolTokens(availableTools)

        // Preflight：估算压缩前后的 token 用量
        msgTokensBefore := memory.EstimateTokens(contextHistory)
        compactedHistory := e.applyCompactionWith(comp, contextHistory)
        msgTokensAfter := memory.EstimateTokens(compactedHistory)
        totalTokens := msgTokensAfter + toolTokens

        // 若压缩减少了 > 5% 的 token，发出 EventCompaction
        if comp != nil && msgTokensAfter < int(float64(msgTokensBefore)*0.95) {
            em.compaction(CompactionData{
                TokensBefore: msgTokensBefore + toolTokens,
                TokensAfter:  totalTokens,
                MsgsBefore:   len(contextHistory),
                MsgsAfter:    len(compactedHistory),
            })
        }

        // 发出估算 token 数（LLM 调用前的预估）
        em.tokenUpdate(totalTokens, e.contextWindow)

        // LLM 调用
        responseMsg, usage, err := em.generate(ctx, turnCount, compactedHistory, availableTools)

        // 用实际 token 用量更新显示（替代估算值）
        if usage != nil && usage.InputTokens > 0 {
            em.tokenUpdate(usage.InputTokens, e.contextWindow)
        }

        // 注意：contextHistory 持续累积完整历史（非压缩版）
        // compactedHistory 只是传给 LLM 的视图
        contextHistory = append(contextHistory, *responseMsg)
        // ...工具执行、观察注入...
    }

    // 只保存 contextHistory[startLen:]（全量历史，非压缩版）
    e.saveHistoryWith(ctx, sess, contextHistory, startLen)
}
```

**非破坏性压缩设计：**

- `contextHistory`：完整历史，持续追加（含所有消息），作为长期记忆
- `compactedHistory`：每轮从 `contextHistory` 派生的压缩视图，只传给 LLM
- `saveHistoryWith` 保存 `contextHistory`（非压缩版），确保历史不丢失

### 8.3 辅助方法语义

| 方法 | sess/comp=nil 时 | 说明 |
|------|----------------|------|
| `loadHistoryWith` | 创建全新 `[system, user]` | 退化为原有无状态行为 |
| `applyCompactionWith` | 原样返回 msgs | 不压缩 |
| `saveHistoryWith` | no-op | 失败仅打 warning，不中断主流程 |

**System Prompt 不持久化的设计原因：**

`startLen` 在注入 system 消息之后、追加用户输入之前记录。`saveHistoryWith` 保存 `msgs[startLen:]`，即 `[user_prompt, assistant_response, observations...]`，system 消息始终被跳过。这样 system prompt 可随 PromptBuilder / AGENTS.md 更新而变化。

### 8.4 并发安全

`SetSession` 由 TUI goroutine 调用，`runLoop` 由引擎 goroutine 调用，通过 `sync.RWMutex` 快照隔离：

```go
// TUI goroutine（写）
func (e *AgentEngine) SetSession(s memory.Session) {
    e.mu.Lock()
    e.session = s
    e.mu.Unlock()
}

// runLoop 开始时快照（读）
e.mu.RLock()
sess := e.session
e.mu.RUnlock()
// runLoop 内部只操作 sess，不再读取 e.session
```

---

## 9. 流式事件系统

### 9.1 事件类型

```go
// EventTokenUpdate 在每次 LLM 调用前（估算值）和调用后（实际值）各发出一次。
EventTokenUpdate EventType = "token_update"

// EventCompaction 在上下文发生有效压缩时发出（token 数减少 > 5%）。
EventCompaction EventType = "compaction"
```

### 9.2 TokenUpdateData

```go
type TokenUpdateData struct {
    // EstimatedTokens 当前上下文的 token 数（估算值或实际 API 用量）。
    EstimatedTokens int `json:"estimated_tokens"`
    // ContextWindow 当前模型的最大 context window（tokens）。0 表示未知。
    ContextWindow int `json:"context_window"`
}
```

### 9.3 CompactionData

```go
type CompactionData struct {
    TokensBefore int `json:"tokens_before"`  // 压缩前的 token 数
    TokensAfter  int `json:"tokens_after"`   // 压缩后的 token 数
    MsgsBefore   int `json:"msgs_before"`    // 压缩前的消息条数
    MsgsAfter    int `json:"msgs_after"`     // 压缩后的消息条数
}
```

### 9.4 StreamChunk.Usage

```go
type StreamChunk struct {
    Type    StreamChunkType `json:"type"`
    Delta   string          `json:"delta,omitempty"`
    Message *Message        `json:"message,omitempty"`
    Error   string          `json:"error,omitempty"`
    // Usage 在 StreamChunkDone 中由 Provider 填充，包含本次调用的实际 token 用量。
    Usage *Usage `json:"usage,omitempty"`
}
```

---

## 10. TUI 集成

### 10.1 Token 用量展示

TUI 状态栏将原先的 `msgs: N` 替换为实时 token 用量展示：

```
[harness9] gpt-4o-mini  workdir: /your/project  │  session: f3a2c1b0...  ctx: 45.2K/128K (35%)
```

颜色编码（基于 `contextTokens / contextWindow` 使用率）：

| 使用率 | 颜色 | 含义 |
|--------|------|------|
| < 50% | 绿色（color "10"）| 正常 |
| 50–80% | 黄色（color "11"）| 警告 |
| ≥ 80% | 红色（color "9"）| 高压，压缩即将触发 |

**tuiModel 新增字段：**

```go
type tuiModel struct {
    // ...现有字段
    contextTokens int  // 当前 context 的 token 用量（由 EventTokenUpdate 更新）
    contextWindow int  // 模型 context window（由首次 EventTokenUpdate 设置）
    tokenOKStyle   lipgloss.Style  // 绿色
    tokenWarnStyle lipgloss.Style  // 黄色
    tokenHighStyle lipgloss.Style  // 红色
}
```

### 10.2 压缩通知

收到 `EventCompaction` 时，在对话区插入一条系统通知行：

```
⚡ 上下文已压缩 — 12.5K → 6.2K tokens（45 → 22 条消息）
```

```go
case engine.EventCompaction:
    data := msg.Event.Data.(engine.CompactionData)
    line := fmt.Sprintf(
        "⚡ 上下文已压缩 — %s → %s tokens（%d → %d 条消息）",
        memory.FormatTokenCount(data.TokensBefore),
        memory.FormatTokenCount(data.TokensAfter),
        data.MsgsBefore, data.MsgsAfter,
    )
    m.conversationLines = append(m.conversationLines, line)
```

### 10.3 会话管理命令

三条内置命令通过 `builtinCmds` 统一注册，Tab 键可补全：

| 命令 | 行为 |
|------|------|
| `/new` | `manager.NewSession()`，替换 `session`，调用 `eng.SetSession()`，状态栏刷新 |
| `/resume` | `manager.ListSessions()`，展示最近 10 条会话，进入序号选择模式 |
| `/exit` | `tea.Quit` 退出 TUI |

`/resume` 交互流：

```
可用会话（3 条）：
  [1] f3a2c1b0-4d7e-4c3a-9f12-ab8d1e2c3f01  2026-05-17 14:30  23 条消息
  [2] 9c1b77a2-8e5f-4b2d-a301-cd4e5f6a7b02  2026-05-16 09:15  41 条消息
  [3] 8d4f2e01-1c3b-4a5d-b210-ef7a8b9c0d03  2026-05-15 21:00  7 条消息
输入序号选择（非数字 Enter 取消）：
```

---

## 11. main.go 初始化

```go
// 获取模型 context window，构建 SummarizationCompactor（默认压缩策略）
modelName := os.Getenv("LLM_MODEL")
if modelName == "" {
    modelName = "openai/gpt-4o-mini"
}

modelLimits := provider.GetModelLimits(modelName)
// SummarizationCompactor 使用同一 LLM 生成摘要，内置 TokenBudgetCompactor 作为错误回退。
compactor := memory.NewSummarizationCompactor(llm, modelLimits.ContextTokens)

eng := engine.NewAgentEngine(llm, registry, workDir,
    engine.WithPromptBuilder(promptBuilder),
    engine.WithSession(sess),
    engine.WithCompactor(compactor),
    engine.WithContextWindow(modelLimits.ContextTokens),  // 传给 TUI 做用量展示
)
```

---

## 12. 设计决策总结

| 决策 | 原因 |
|------|------|
| **SQLite WAL 模式** | 进程重启可恢复，`/resume` 功能依赖持久化；WAL 支持并发读写 |
| **纯 Go SQLite（modernc.org/sqlite）** | 无 CGo 依赖，与 harness9 零 CGo 目标一致，交叉编译友好 |
| **System Prompt 不持久化** | Prompt 可随 PromptBuilder / AGENTS.md 更新而变化，不应被历史数据锁定 |
| **SummarizationCompactor 为默认** | LLM 摘要保留语义，优于截断；内置 TokenBudgetCompactor 作为错误回退，保证可用性 |
| **80% 触发阈值** | 预留 20% 给工具定义 token（可达 20-30K）和估算误差缓冲 |
| **双向孤立工具对修复** | Anthropic API 严格要求 tool_call / tool_result 配对，单向回溯不够 |
| **char÷4 估算 + API 实际值校正** | 无依赖估算用于压缩决策；实际值用于 TUI 展示精度 |
| **两阶段 tokenUpdate** | 调用前发估算值（即时响应），调用后发实际值（精确展示） |
| **contextWindow 首次设置不覆盖** | 防止每轮更新导致 TUI 闪烁；0→N 只设置一次 |
| **非破坏性压缩（compactedHistory）** | contextHistory 保持完整，saveHistoryWith 持久化全量历史 |
| **sync.RWMutex 快照** | runLoop 开始时一次性快照 session/compactor，消除与 TUI goroutine 的竞争 |
| **失败 warning 不中断** | saveHistoryWith 失败不影响主流程；持久化是增强功能，不是核心依赖 |

---

## 13. 后续 Roadmap

| 功能 | 优先级 | 说明 |
|------|--------|------|
| FTS5 全文会话搜索 | P3 | `/search` 命令，搜索历史对话内容 |
| TTL 自动过期清理 | P3 | 定期清除旧会话，控制磁盘占用 |
| CLI 模式 session 支持 | P3 | CLI 当前为无状态 REPL |
| Token Budget 精确计数 | P2 | 接入官方 tokenizer，消除 char÷4 误差（当前已通过 API 响应实际值校正） |

---

## 14. 会话全文检索（FTS5）

### 14.1 背景与定位

harness9 已通过 `internal/ltm/` 包为**长期记忆条目**提供了 FTS5 全文检索能力（`memory_search` 工具）。但长期记忆存储的是经过 LLM 提取、结构化整理的知识片段，并非会话的原始对话内容。

会话全文检索解决的是另一个问题：**在历史会话的原始消息本身中自由检索**。典型场景：

- 用户想找出某次历史会话里 Agent 给出的具体代码片段或操作步骤
- 开发者想确认某个关键词在哪些会话里被提及过
- Agent 在当前对话中想主动回忆"我之前有没有处理过类似的问题"

这类需求的特点是：检索目标是**未经压缩的原始消息文本**，粒度细、覆盖全，与长期记忆互为补充而非替代关系。

### 14.2 核心组件

#### messages_fts 虚表

在 `internal/memory/` 包的 SQLite Schema 中新增一张 standalone FTS5 虚表，镜像 `messages` 表的核心字段：

```sql
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts
USING fts5(session_id UNINDEXED, role UNINDEXED, content, content='messages', content_rowid='id');
```

- `session_id`、`role` 标记为 `UNINDEXED`，仅作为检索结果的返回字段，不参与全文索引
- `content` 字段进入 FTS5 索引，支持关键词全文匹配
- `content=''messages''`、`content_rowid=''id''` 将虚表与 `messages` 主表关联（content table 模式），节省磁盘空间

新消息写入 `messages` 表时，同步通过 `INSERT INTO messages_fts(...)` 更新索引，保证实时可检索。

#### SearchMessages 方法

`Manager` 新增以下方法：

```go
// MessageSearchResult 表示一条全文检索结果，包含消息的来源会话、角色和内容。
type MessageSearchResult struct {
    SessionID string
    Role      string
    Content   string
}

// SearchMessages 在所有历史会话消息中执行 FTS5 全文检索。
// query 为检索关键词（支持 FTS5 查询语法）；limit 控制返回条数上限。
// 结果按 FTS5 相关性排序（bm25），最相关的排在最前。
func (m *Manager) SearchMessages(ctx context.Context, query string, limit int) ([]MessageSearchResult, error)
```

底层查询示例：

```sql
SELECT session_id, role, content
FROM messages_fts
WHERE messages_fts MATCH ?
ORDER BY rank
LIMIT ?;
```

#### session_search 工具

`internal/tools/` 包新增 `session_search` 工具（`BaseTool` 实现），让 Agent 在对话中可以直接调用检索：

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `query` | string | ✅ | FTS5 检索关键词 |
| `limit` | integer | ❌ | 返回条数上限，默认 5 |

工具内部调用 `Manager.SearchMessages()`，将结果格式化后作为工具输出返回给 Agent。

### 14.3 工作原理

```
用户输入检索词（或 Agent 自主调用 session_search 工具）
         │
         ▼
  session_search 工具（internal/tools/）
         │   query + limit
         ▼
  Manager.SearchMessages()（internal/memory/）
         │   SQL: SELECT … FROM messages_fts WHERE messages_fts MATCH ? ORDER BY rank LIMIT ?
         ▼
  SQLite FTS5 引擎执行 BM25 排序全文检索
         │
         ▼
  []MessageSearchResult{SessionID, Role, Content}
         │
         ▼
  格式化为文本 → 作为工具输出注入对话上下文 → Agent 继续推理
```

**写入时同步索引**：`SQLiteSession.AddMessages()` 每次写入 `messages` 表后，立即在同一事务中向 `messages_fts` 插入对应记录，确保索引与主表始终保持一致，不存在延迟或异步 gap。

### 14.4 与 LTM memory_search 的区别

| 维度 | 会话全文检索（messages_fts） | LTM 记忆检索（memories_fts） |
|------|----------------------------|-----------------------------|
| **检索目标** | 历史会话原始消息 | LTM 提取的结构化知识条目 |
| **覆盖范围** | 所有历史对话，不经筛选 | 仅被 Extractor / memory_write 显式记录的内容 |
| **粒度** | 单条消息（role + content） | 单条记忆（category + content + importance） |
| **更新时机** | 写入消息时同步索引 | Extractor 压缩前提取 / 显式调用 memory_write |
| **使用场景** | 回忆"原话"、查找具体代码片段 | 跨会话语义理解、用户偏好、项目背景 |
| **存储位置** | `sessions.db` messages_fts | `sessions.db` memories_fts |
