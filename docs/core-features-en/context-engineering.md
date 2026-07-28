# Context Engineering Implementation

## 1. Background and Design Goals

### 1.1 Problem Background

harness9's `runLoop` originally declared `contextHistory` as a local variable, freshly initialized on every `Run()` call, so history could not carry over between sessions. As conversation history keeps growing, the design also faces the following challenges:

- **Statelessness**: session history is entirely lost after a process restart, and users cannot recover previous work
- **Context overflow**: history messages grow without bound; once the LLM context window is exceeded, the API errors out or truncates
- **Crude compaction**: the early SlidingWindowCompactor only truncated by message count, ignoring actual token usage, so compaction timing was imprecise
- **Opacity**: users cannot see current context usage or know when compaction was triggered

### 1.2 Design Goals

The Context Engineering module covers the following capabilities:

| Goal | Implementation Mechanism |
|------|---------|
| **Session persistence** | SQLite WAL mode, recoverable after a process restart |
| **Precise compaction timing** | Token Budget aware of the LLM context window, triggered at an 80% threshold |
| **Orphaned tool-pair repair** | Bidirectional repair, guaranteeing API compatibility |
| **Actual token usage** | Extracted from the API response's usage field, updating the display after the fact |
| **User visibility** | TUI shows real-time token usage with color-coded alerts, and issues a notification when compaction occurs |

---

## 2. Overall Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                          AgentEngine                              │
│                                                                  │
│   WithSession(sess)       →  session   memory.Session            │
│   WithCompactor(comp)     →  compactor memory.Compactor          │
│   WithContextWindow(tok)  →  contextWindow int                   │
│                                                                  │
│   runLoop() (each Turn):                                         │
│     1. loadHistoryWith()    ← Session load + system prompt inject │
│     2. EstimateTokens()     ← Preflight: estimate token usage     │
│     3. applyCompactionWith()← Compaction (SummarizationCompactor)  │
│     4. tokenUpdate(est)     ← Emit estimated value to TUI          │
│     5. em.generate()        ← LLM call, obtain *Usage             │
│     6. tokenUpdate(actual)  ← Update TUI with actual value         │
│     7. saveHistoryWith()    ← Write new messages back to Session  │
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
│  SQLiteSession (primary impl)  MemorySession (for testing)        │
│  ├── WAL-mode SQLite            └── sync.Mutex + []Message        │
│  ├── Transactional AddMessages                                   │
│  └── tool_calls JSON serialization                               │
│                                                                  │
│  Compactor (interface)                                           │
│  ├── SummarizationCompactor  ← LLM summarization compaction (default, with fallback) │
│  ├── TokenBudgetCompactor    ← Token Budget aware truncation (fallback strategy)      │
│  └── SlidingWindowCompactor  ← Trim by message count (simple fallback)                │
│                                                                  │
│  token.go                    model_limits.go                     │
│  ├── EstimateTokens()        ├── GetModelLimits(name)            │
│  ├── EstimateToolTokens()    └── ModelLimits{ContextTokens, ...} │
│  └── FormatTokenCount()                                          │
└──────────────────────────────────────────────────────────────────┘
               │
               ▼
     ~/.harness9/sessions.db  (SQLite persistence file)
```

---

## 3. Package Structure

```
internal/memory/
├── session.go               # Session interface + SessionInfo type definitions
├── manager.go               # Manager: SQLite connection owner + session CRUD
├── sqlite_session.go        # SQLiteSession: WAL-mode SQLite persistent implementation
├── mem_session.go           # MemorySession: pure in-memory implementation (for testing)
├── compaction.go            # Compactor interface + TokenBudgetCompactor + SlidingWindowCompactor
├── summarization.go         # SummarizationCompactor: LLM summarization compaction (default strategy)
├── token.go                 # Token estimation utility functions
├── sqlite_session_test.go
├── mem_session_test.go
├── manager_test.go
├── compaction_test.go
└── summarization_test.go

internal/provider/
├── model_limits.go          # Model context window registry
└── model_limits_test.go
```

---

## 4. Core Interfaces

### 4.1 Session Interface

```go
// Session manages the message history and planning state of a single session.
type Session interface {
    SessionID() string
    // GetMessages returns historical messages; limit=0 returns all, limit>0 returns the most recent limit entries.
    GetMessages(ctx context.Context, limit int) ([]schema.Message, error)
    // AddMessages appends new messages to the session history.
    AddMessages(ctx context.Context, msgs []schema.Message) error
    // PopMessage deletes and returns the most recent message (used for undo); returns nil, nil when there are no messages.
    PopMessage(ctx context.Context) (*schema.Message, error)
    // Clear clears the session history.
    Clear(ctx context.Context) error
    // GetTodos returns the todo list persisted for this session. Returns nil, nil when there are no todos.
    GetTodos(ctx context.Context) ([]planning.TodoItem, error)
    // SaveTodos atomically saves the todo list (write-replace semantics).
    SaveTodos(ctx context.Context, items []planning.TodoItem) error
}
```

### 4.2 Compactor Interface

```go
// Compactor trims historical messages before they are injected into the LLM context, preventing the context window from being exceeded.
type Compactor interface {
    Compact(msgs []schema.Message) []schema.Message
}
```

### 4.3 Manager

```go
type Manager struct{ db *sql.DB; toolResultsDir string }

// NewManager opens (or creates) the SQLite database, initializes the schema, and supports optional configuration.
func NewManager(dbPath string, opts ...ManagerOption) (*Manager, error)
// WithToolResultsDir sets the offload file root directory; DeleteSession cascades cleanup to the corresponding subdirectory.
func WithToolResultsDir(dir string) ManagerOption
func (m *Manager) NewSession(ctx context.Context) (Session, error)
func (m *Manager) OpenSession(ctx context.Context, id string) (Session, error)
func (m *Manager) ListSessions(ctx context.Context) ([]SessionInfo, error)
func (m *Manager) DeleteSession(ctx context.Context, id string) error
func (m *Manager) Close() error
```

`Manager` is the single source of truth holding the process-wide SQLite connection; all `SQLiteSession` instances share the same `*sql.DB`.

---

## 5. SQLite Schema

Persistence path: `~/.harness9/sessions.db`

```sql
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT    PRIMARY KEY,   -- UUID v4
    created_at INTEGER NOT NULL,      -- Unix timestamp (seconds)
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   TEXT    NOT NULL,
    role         TEXT    NOT NULL,    -- 'system'|'user'|'assistant'
    content      TEXT    NOT NULL,
    tool_calls   TEXT,                -- JSON, only present for assistant messages
    tool_call_id TEXT,                -- only present for Observation (user) messages
    created_at   INTEGER NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, id);
```

**Key design decisions:**

- **WAL mode**: `journal_mode=WAL` allows concurrent reads and writes, suited to simultaneous TUI + engine access
- **The `tool_calls` column**: stores the JSON serialization of `[]schema.ToolCall`, deserialized on read, fully aligned with the existing type
- **System prompt is not persisted**: the system message is re-injected by `loadHistoryWith` every time and is never written to the DB, so the prompt can change as configuration is updated
- **ON DELETE CASCADE**: deleting a session automatically cleans up all associated messages

### 5.1 SQLiteSession Implementation Highlights

**GetMessages:**

```go
// limit=0: full ascending query
SELECT role, content, tool_calls, tool_call_id
FROM messages WHERE session_id = ?
ORDER BY id ASC

// limit>0: DESC LIMIT first, then reverse in memory, yielding "most recent N entries in ascending order"
SELECT ... ORDER BY id DESC LIMIT ?
// → reverse in memory → ascending order
```

**AddMessages (transactional):**

```
BEGIN TX
  INSERT INTO messages ... (multiple rows)
  UPDATE sessions SET updated_at = ? WHERE id = ?
COMMIT
```

**PopMessage (atomic delete):**

```
BEGIN TX
  SELECT ... ORDER BY id DESC LIMIT 1
  Deserialize tool_calls (ROLLBACK on failure, message is not lost)
  DELETE FROM messages WHERE id = ?
COMMIT
```

---

## 6. Compaction Strategies

harness9 provides three compaction strategies, ranked from highest to lowest priority:

| Strategy | File | Default | Applicable Scenario |
|------|------|:----:|---------|
| `SummarizationCompactor` | `summarization.go` | ✅ | Long-running tasks, information-dense conversations, best semantic retention |
| `TokenBudgetCompactor` | `compaction.go` | — | Automatic fallback strategy when the Provider is unavailable |
| `SlidingWindowCompactor` | `compaction.go` | — | Quick prototyping, extremely cost-sensitive scenarios |

### 6.1 SummarizationCompactor (LLM Summarization Compaction, Default)

`SummarizationCompactor` calls the LLM to compress old messages into a structured summary, significantly outperforming truncation strategies in semantic retention.

**Interface design:**

```go
// Summarizer is defined in the memory package (the consumer side); any provider.LLMProvider satisfies this interface.
type Summarizer interface {
    Generate(ctx context.Context, messages []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error)
}
```

**Configuration:**
- `Provider Summarizer` — the LLM that performs summarization
- `MaxTokens int` — the token budget that triggers compaction (typically `contextWindow × 80%`)
- `MinTailMessages int` — the minimum number of tail messages that must be retained (default 6)
- `Fallback Compactor` — the fallback strategy used when the Provider call fails (default `TokenBudgetCompactor`)

**Compaction algorithm:**

```
Input: msgs = [system, msg1, ..., msgN]

1. EstimateTokens(msgs) ≤ MaxTokens → return directly (no compaction needed)
2. msgs[0].Role ≠ RoleSystem → return directly (defensive check)
3. Split: head = msgs[1 : N-minTail], tail = msgs[N-minTail:]
4. len(head) == 0 → return directly (nothing to summarize)
5. Call summarize(head) → summary string
6. On success: return [system, {user: "[Conversation Summary]\n"+summary}, ...tail]
           + repairOrphanedToolPairs()
7. On failure: fall back to Fallback.Compact(msgs)
```

**Incremental update mechanism:**

When head already contains a previous summary message (starting with `[Conversation Summary]`), `summarize` extracts the old summary and constructs an incremental update prompt:

```
<previous-summary>
{previous summary content}
</previous-summary>

New conversation to merge:
{new conversation text}
```

This avoids the problem of information being lost through stacking across multiple rounds of compaction.

**Summary output format (summaryTemplate):**

```
**Goal:** What the user is trying to accomplish.
**Progress:** Key actions taken and their results.
**Key Decisions:** Important choices and rationale.
**Next Steps:** What was planned or pending.
**Critical Context:** Facts, file paths, variable names, or constraints the agent must remember.
```

**Comparison with TokenBudgetCompactor:**

| Dimension | TokenBudgetCompactor | SummarizationCompactor |
|------|---------------------|------------------------|
| Semantic retention | Truncation (old messages are completely lost) | LLM summary (key information retained) |
| Speed | Extremely fast (no LLM call) | Additional LLM call latency |
| Cost | Zero | Summarization API cost |
| Availability | Always available | Depends on Provider availability |
| Recommended scenario | Quick prototyping, cost-sensitive | Long-running tasks, information-dense conversations |

### 6.2 TokenBudgetCompactor (Token Budget Aware, Fallback Strategy)

Configuration:
- `MaxTokens int` — the maximum allowed token count (typically `contextWindow × 80%`)
- `MinTailMessages int` — the minimum number of tail messages that must be retained (default 6, ensuring conversational continuity)

```go
func NewTokenBudgetCompactor(contextWindow int) *TokenBudgetCompactor {
    return &TokenBudgetCompactor{
        MaxTokens:       contextWindow * 80 / 100,
        MinTailMessages: 6,
    }
}
```

**Compaction algorithm:**

```
Input: msgs = [system, msg1, ..., msgN]
Non-system message count = nonSystemCount = len(msgs) - 1 (skipping msgs[0])

1. If nonSystemCount ≤ MinTailMessages, return directly (protects the minimum tail)
2. Estimate total token count = EstimateTokens(msgs)
3. If totalTokens ≤ MaxTokens, return directly (within budget)
4. Binary search: find the largest tailLen ∈ [MinTailMessages, nonSystemCount-1]
   such that EstimateTokens([system] + msgs[N-tailLen:]) ≤ MaxTokens
5. Take the final tail = msgs[len-tailLen:]
6. Repair orphaned tool pairs: repairOrphanedToolPairs([system] + tail)
7. Return the repaired message list
```

**Why 80% instead of 100%?**

- Reserves 20% headroom for tool definitions (bash/read_file tool descriptions can consume 10-30K tokens)
- Avoids exceeding the API limit due to estimation error (char÷4 is an approximation)
- Leaves room for the LLM to generate output

### 6.3 SlidingWindowCompactor (by Message Count, Simple Fallback)

Configuration: `MaxMessages int` (default 100, including the system prompt)

```
Input: msgs = [system, msg1, msg2, ..., msgN]

1. If len(msgs) ≤ MaxMessages, return the original slice directly (no compaction needed)
2. Compute the window start point: startIdx = len(msgs) - MaxMessages + 1
3. [Boundary fix A] Walk backward over orphaned Observations:
   while startIdx > 1 AND msgs[startIdx].ToolCallID != "" {
       startIdx--
   }
4. Combine: candidate = [msgs[0]] + msgs[startIdx:]
5. [Boundary fix B] Call repairOrphanedToolPairs for bidirectional repair (see 6.4)
6. Return the repaired result
```

`SlidingWindowCompactor` is not aware of token usage, and is suited to quick prototyping scenarios; `SummarizationCompactor` is recommended for production environments.

> **Note:** Step 3 (walking backward) can only handle the case where "the window start point happens to cut in the middle of an Observation" (Type A orphan). If the window retains an assistant message with ToolCalls but its corresponding tool_result has been trimmed away (Type B orphan), the bidirectional repair in step 5 is needed as a backstop.

### 6.4 Orphaned Tool-Pair Repair (repairOrphanedToolPairs)

Both `TokenBudgetCompactor` and `SlidingWindowCompactor` call this function after truncation to handle two types of orphaned messages:

**Type A: orphaned tool_result** (has a `ToolCallID` but no corresponding `ToolCalls` message)

```
Before truncation: [system][assistant:tool_call_id=x,tool_calls=[bash]][user:tool_call_id=x][assistant:result]
After truncation:                                                     [user:tool_call_id=x][assistant:result]
                                                                       ↑ orphaned tool_result → delete
```

**Type B: orphaned tool_call** (has `ToolCalls` but no corresponding tool_result)

```
After truncation: [system][assistant:tool_calls=[bash]][assistant:result]
                          ↑ orphaned tool_call → insert a stub user message as tool_result
```

Repair logic (bidirectional scan):

```go
func repairOrphanedToolPairs(msgs []schema.Message) []schema.Message {
    // Pass 1: collect existing tool_call IDs
    existingIDs := map[string]bool{}
    for _, msg := range msgs {
        for _, tc := range msg.ToolCalls {
            existingIDs[tc.ID] = true
        }
    }

    // Pass 2: delete orphaned tool_result; insert a stub for orphaned tool_call
    var result []schema.Message
    for _, msg := range msgs {
        if msg.ToolCallID != "" && !existingIDs[msg.ToolCallID] {
            continue // delete orphaned tool_result
        }
        result = append(result, msg)
        if len(msg.ToolCalls) > 0 {
            for _, tc := range msg.ToolCalls {
                // check whether there is a corresponding tool_result
                hasResult := false
                for _, m2 := range msgs {
                    if m2.ToolCallID == tc.ID {
                        hasResult = true
                        break
                    }
                }
                if !hasResult {
                    // insert a stub tool_result
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

**Why is bidirectional repair needed?**

LLM APIs (especially the Anthropic Messages API) require tool_call / tool_result to always appear in pairs. If orphaned messages appear after truncation, the API call will error with a 400. Both `TokenBudgetCompactor` and `SlidingWindowCompactor` call `repairOrphanedToolPairs`, providing complete bidirectional repair as a guardrail.

---

## 7. Token Estimation and Model Awareness

### 7.1 Token Estimation (internal/memory/token.go)

```go
const charsPerToken = 4  // industry-standard approximation (used by DeepAgents, HermesAgent, and OpenCode alike)

// EstimateTokens estimates the token usage of a message list.
func EstimateTokens(msgs []schema.Message) int

// EstimateToolTokens estimates the token usage of a tool definition list.
// Tool definitions (JSON Schema) often consume a large number of tokens (10-30K) and must be included in the preflight calculation.
func EstimateToolTokens(tools []schema.ToolDefinition) int

// FormatTokenCount formats a token count into a human-readable string.
// Example: 500 → "500", 45200 → "45.2K", 1200000 → "1.2M"
func FormatTokenCount(n int) string
```

**Why char÷4 instead of a precise tokenizer?**

- Avoids introducing an external dependency such as tiktoken, preserving the zero-dependency principle
- The error on models such as GPT/Claude is typically within ±10%
- The compaction decision already reserves a 20% buffer, so estimation error is within tolerance
- Actual token usage is corrected after the LLM call via the `usage` field in the API response

### 7.2 Model Context Window Registry (internal/provider/model_limits.go)

```go
type ModelLimits struct {
    ContextTokens int  // input context window size (tokens)
    OutputTokens  int  // maximum output token count
}

// GetModelLimits returns the context window limits based on the model name.
// Automatically strips routing prefixes such as "openai/" (e.g., the OpenRouter format "openai/gpt-4o").
// Returns a conservative default of 256K for unknown models.
func GetModelLimits(modelName string) ModelLimits
```

Coverage (as of 2026-05):

| Family | Representative Model | Context Window |
|------|---------|---------------|
| Claude 4.x | claude-opus-4, claude-sonnet-4 | 200K |
| Claude 3.x | claude-3-5-sonnet, claude-3-opus | 200K |
| GPT-4o | gpt-4o, gpt-4o-mini | 128K |
| GPT-4.5 | gpt-4.5-preview | 128K |
| o-series | o3, o4-mini | 200K |
| Gemini 2.0+ | gemini-2.0-flash | 1M |
| DeepSeek | deepseek-chat, deepseek-r1 | 64K |
| Qwen | qwen-plus, qwen-max | 128K |
| Unknown model | — | 256K (conservative default) |

### 7.3 Actual Token Usage (API Response Usage)

The API response contains the actual token usage, which is more accurate than the character-based estimate. After the LLM call completes, the engine updates the TUI display with the actual value.

**schema.Usage type:**

```go
// Usage records the token usage of a single LLM API call, extracted by the Provider from the API response.
type Usage struct {
    InputTokens  int `json:"input_tokens"`
    OutputTokens int `json:"output_tokens"`
}
```

**LLMProvider interface update:**

```go
// Generate returns (*schema.Message, *schema.Usage, error).
// Usage contains the actual token usage for this call (may be nil).
Generate(ctx context.Context, messages []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error)
```

**Provider implementations:**

- **OpenAI (non-streaming)**: extracted from `resp.Usage.PromptTokens` / `resp.Usage.CompletionTokens`
- **OpenAI (streaming)**: sets `StreamOptions.IncludeUsage = true` on the request, extracted from `Usage.PromptTokens` in the final chunk
- **Anthropic (non-streaming)**: extracted from `resp.Usage.InputTokens` / `resp.Usage.OutputTokens`
- **Anthropic (streaming)**: extracted from `Message.Usage.InputTokens` in the `message_start` event
- **Mock Provider**: returns `nil` (test stubs do not simulate the API call)

**Update timing in the engine:**

```
Turn N:
  1. tokenUpdate(estimated, window)   ← Before the LLM call: send the estimated value (used for compaction decisions and initial display)
  2. em.generate() → returns Usage    ← LLM call
  3. tokenUpdate(actual, window)      ← After the LLM call: overwrite with the actual value (if usage != nil)
```

The TUI user first sees the estimated value, then it refreshes to the actual value once the LLM response returns, giving more accurate information.

---

## 8. AgentEngine Integration

### 8.1 New Options and Fields

```go
type AgentEngine struct {
    // ...existing fields
    contextWindow int          // model context window (tokens), used for TUI display, 0 means unknown
    mu            sync.RWMutex // protects session and compactor, preventing races with the TUI goroutine
    session       memory.Session
    compactor     memory.Compactor
}

func WithSession(s memory.Session) Option
func WithCompactor(c memory.Compactor) Option
func WithContextWindow(tokens int) Option

// SetSession replaces the current session, for the TUI to call when switching via /new or /resume. Thread-safe.
func (e *AgentEngine) SetSession(s memory.Session)
```

### 8.2 runLoop Preflight (Preflight Token Check)

```go
func (e *AgentEngine) runLoop(ctx context.Context, userPrompt string, ...) error {
    // Snapshot session/compactor to avoid races with the TUI goroutine's SetSession
    e.mu.RLock()
    sess, comp := e.session, e.compactor
    e.mu.RUnlock()

    contextHistory, startLen := e.loadHistoryWith(ctx, userPrompt, sess)

    for {
        availableTools := e.registry.GetAvailableTools()
        toolTokens := memory.EstimateToolTokens(availableTools)

        // Preflight: estimate token usage before and after compaction
        msgTokensBefore := memory.EstimateTokens(contextHistory)
        compactedHistory := e.applyCompactionWith(comp, contextHistory)
        msgTokensAfter := memory.EstimateTokens(compactedHistory)
        totalTokens := msgTokensAfter + toolTokens

        // If compaction reduced tokens by > 5%, emit EventCompaction
        if comp != nil && msgTokensAfter < int(float64(msgTokensBefore)*0.95) {
            em.compaction(CompactionData{
                TokensBefore: msgTokensBefore + toolTokens,
                TokensAfter:  totalTokens,
                MsgsBefore:   len(contextHistory),
                MsgsAfter:    len(compactedHistory),
            })
        }

        // Emit the estimated token count (the preflight estimate before the LLM call)
        em.tokenUpdate(totalTokens, e.contextWindow)

        // LLM call
        responseMsg, usage, err := em.generate(ctx, turnCount, compactedHistory, availableTools)

        // Update the display with the actual token usage (replacing the estimate)
        if usage != nil && usage.InputTokens > 0 {
            em.tokenUpdate(usage.InputTokens, e.contextWindow)
        }

        // Note: contextHistory keeps accumulating the full history (uncompacted)
        // compactedHistory is only the view passed to the LLM
        contextHistory = append(contextHistory, *responseMsg)
        // ...tool execution, observation injection...
    }

    // Only save contextHistory[startLen:] (the full history, not the compacted version)
    e.saveHistoryWith(ctx, sess, contextHistory, startLen)
}
```

**Non-destructive compaction design:**

- `contextHistory`: the full history, continuously appended (containing all messages), serving as the long-term record
- `compactedHistory`: a compacted view derived from `contextHistory` each turn, passed to the LLM only
- `saveHistoryWith` saves `contextHistory` (the uncompacted version), ensuring no history is lost

### 8.3 Helper Method Semantics

| Method | When sess/comp=nil | Description |
|------|----------------|------|
| `loadHistoryWith` | Creates a fresh `[system, user]` | Degrades to the original stateless behavior |
| `applyCompactionWith` | Returns msgs as-is | No compaction |
| `saveHistoryWith` | no-op | Failure only logs a warning, does not interrupt the main flow |

**Design rationale for not persisting the system prompt:**

`startLen` is recorded after the system message is injected and before the user input is appended. `saveHistoryWith` saves `msgs[startLen:]`, i.e. `[user_prompt, assistant_response, observations...]`, so the system message is always skipped. This way the system prompt can change as the PromptBuilder / AGENTS.md is updated.

### 8.4 Concurrency Safety

`SetSession` is called by the TUI goroutine, and `runLoop` is called by the engine goroutine; isolation is achieved via a `sync.RWMutex` snapshot:

```go
// TUI goroutine (write)
func (e *AgentEngine) SetSession(s memory.Session) {
    e.mu.Lock()
    e.session = s
    e.mu.Unlock()
}

// Snapshot at the start of runLoop (read)
e.mu.RLock()
sess := e.session
e.mu.RUnlock()
// runLoop internally only operates on sess, and no longer reads e.session
```

---

## 9. Streaming Event System

### 9.1 Event Types

```go
// EventTokenUpdate is emitted once before each LLM call (estimated value) and once after (actual value).
EventTokenUpdate EventType = "token_update"

// EventCompaction is emitted when the context undergoes effective compaction (token count reduced by > 5%).
EventCompaction EventType = "compaction"
```

### 9.2 TokenUpdateData

```go
type TokenUpdateData struct {
    // EstimatedTokens is the token count of the current context (either an estimate or actual API usage).
    EstimatedTokens int `json:"estimated_tokens"`
    // ContextWindow is the maximum context window (tokens) of the current model. 0 means unknown.
    ContextWindow int `json:"context_window"`
}
```

### 9.3 CompactionData

```go
type CompactionData struct {
    TokensBefore int `json:"tokens_before"`  // token count before compaction
    TokensAfter  int `json:"tokens_after"`   // token count after compaction
    MsgsBefore   int `json:"msgs_before"`    // message count before compaction
    MsgsAfter    int `json:"msgs_after"`     // message count after compaction
}
```

### 9.4 StreamChunk.Usage

```go
type StreamChunk struct {
    Type    StreamChunkType `json:"type"`
    Delta   string          `json:"delta,omitempty"`
    Message *Message        `json:"message,omitempty"`
    Error   string          `json:"error,omitempty"`
    // Usage is populated by the Provider in StreamChunkDone, containing the actual token usage for this call.
    Usage *Usage `json:"usage,omitempty"`
}
```

---

## 10. TUI Integration

### 10.1 Token Usage Display

The TUI status bar replaces the original `msgs: N` with a real-time token usage display:

```
[harness9] gpt-4o-mini  workdir: /your/project  │  session: f3a2c1b0...  ctx: 45.2K/128K (35%)
```

Color coding (based on the `contextTokens / contextWindow` utilization rate):

| Utilization | Color | Meaning |
|--------|------|------|
| < 50% | Green (color "10") | Normal |
| 50–80% | Yellow (color "11") | Warning |
| ≥ 80% | Red (color "9") | High pressure, compaction imminent |

**New tuiModel fields:**

```go
type tuiModel struct {
    // ...existing fields
    contextTokens int  // current context token usage (updated by EventTokenUpdate)
    contextWindow int  // model context window (set by the first EventTokenUpdate)
    tokenOKStyle   lipgloss.Style  // green
    tokenWarnStyle lipgloss.Style  // yellow
    tokenHighStyle lipgloss.Style  // red
}
```

### 10.2 Compaction Notification

Upon receiving `EventCompaction`, a system notification line is inserted into the conversation area:

```
⚡ Context compacted — 12.5K → 6.2K tokens (45 → 22 messages)
```

```go
case engine.EventCompaction:
    data := msg.Event.Data.(engine.CompactionData)
    line := fmt.Sprintf(
        "⚡ Context compacted — %s → %s tokens (%d → %d messages)",
        memory.FormatTokenCount(data.TokensBefore),
        memory.FormatTokenCount(data.TokensAfter),
        data.MsgsBefore, data.MsgsAfter,
    )
    m.conversationLines = append(m.conversationLines, line)
```

### 10.3 Session Management Commands

Three built-in commands are registered uniformly via `builtinCmds`, with Tab-key completion:

| Command | Behavior |
|------|------|
| `/new` | `manager.NewSession()`, replaces `session`, calls `eng.SetSession()`, refreshes the status bar |
| `/resume` | `manager.ListSessions()`, displays the most recent 10 sessions, enters index-selection mode |
| `/exit` | `tea.Quit` exits the TUI |

`/resume` interaction flow:

```
Available sessions (3):
  [1] f3a2c1b0-4d7e-4c3a-9f12-ab8d1e2c3f01  2026-05-17 14:30  23 messages
  [2] 9c1b77a2-8e5f-4b2d-a301-cd4e5f6a7b02  2026-05-16 09:15  41 messages
  [3] 8d4f2e01-1c3b-4a5d-b210-ef7a8b9c0d03  2026-05-15 21:00  7 messages
Enter an index to select (press Enter with a non-numeric value to cancel):
```

---

## 11. main.go Initialization

```go
// Get the model context window, build a SummarizationCompactor (the default compaction strategy)
modelName := os.Getenv("LLM_MODEL")
if modelName == "" {
    modelName = "openai/gpt-4o-mini"
}

modelLimits := provider.GetModelLimits(modelName)
// SummarizationCompactor uses the same LLM to generate summaries, with a built-in TokenBudgetCompactor as an error fallback.
compactor := memory.NewSummarizationCompactor(llm, modelLimits.ContextTokens)

eng := engine.NewAgentEngine(llm, registry, workDir,
    engine.WithPromptBuilder(promptBuilder),
    engine.WithSession(sess),
    engine.WithCompactor(compactor),
    engine.WithContextWindow(modelLimits.ContextTokens),  // passed to the TUI for usage display
)
```

---

## 12. Design Decision Summary

| Decision | Rationale |
|------|------|
| **SQLite WAL mode** | Recoverable after a process restart; the `/resume` feature depends on persistence; WAL supports concurrent reads and writes |
| **Pure-Go SQLite (modernc.org/sqlite)** | No CGo dependency, consistent with harness9's zero-CGo goal, friendly to cross-compilation |
| **System prompt not persisted** | The prompt can change as the PromptBuilder / AGENTS.md is updated, and should not be locked in by historical data |
| **SummarizationCompactor as the default** | LLM summarization preserves semantics, superior to truncation; the built-in TokenBudgetCompactor serves as an error fallback, ensuring availability |
| **80% trigger threshold** | Reserves 20% for tool definition tokens (which can reach 20-30K) and an estimation error buffer |
| **Bidirectional orphaned tool-pair repair** | The Anthropic API strictly requires tool_call / tool_result pairing; one-directional backtracking is insufficient |
| **char÷4 estimation + API actual-value correction** | Dependency-free estimation is used for compaction decisions; the actual value is used for TUI display precision |
| **Two-phase tokenUpdate** | Emits the estimated value before the call (immediate feedback), and the actual value after the call (precise display) |
| **contextWindow set only once, not overwritten** | Prevents TUI flicker from being updated every turn; 0→N is set only once |
| **Non-destructive compaction (compactedHistory)** | contextHistory remains complete; saveHistoryWith persists the full history |
| **sync.RWMutex snapshot** | runLoop takes a one-time snapshot of session/compactor at the start, eliminating races with the TUI goroutine |
| **Failure logs a warning without interrupting** | A saveHistoryWith failure does not affect the main flow; persistence is an enhancement, not a core dependency |

---

## 13. Future Roadmap

| Feature | Priority | Description |
|------|--------|------|
| FTS5 full-text session search | P3 | `/search` command, search historical conversation content |
| TTL automatic expiry cleanup | P3 | Periodically purge old sessions to control disk usage |
| CLI mode session support | P3 | The CLI is currently a stateless REPL |
| Precise Token Budget counting | P2 | Integrate an official tokenizer to eliminate char÷4 error (currently corrected via the actual value from the API response) |

---

## 14. Session Full-Text Search (FTS5)

### 14.1 Background and Positioning

harness9 already provides FTS5 full-text search for **long-term memory entries** via the `internal/ltm/` package (the `memory_search` tool). However, long-term memory stores knowledge fragments that have been extracted and structured by the LLM — not the raw conversation messages from sessions.

Session full-text search solves a different problem: **searching freely within the raw messages of historical sessions themselves**. Typical use cases include:

- A user wants to find a specific code snippet or set of steps that the Agent provided in a previous session
- A developer wants to confirm which sessions a particular keyword has appeared in
- An Agent wants to proactively recall "have I dealt with a similar problem before?" during the current conversation

These scenarios share a common characteristic: the search target is **uncompressed raw message text** — fine-grained, comprehensive, and complementary to long-term memory rather than a replacement for it.

### 14.2 Core Components

#### messages_fts Virtual Table

A standalone FTS5 virtual table is added to the SQLite schema in the `internal/memory/` package, mirroring the core fields of the `messages` table:

```sql
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts
USING fts5(session_id UNINDEXED, role UNINDEXED, content, content='messages', content_rowid='id');
```

- `session_id` and `role` are marked `UNINDEXED` — they are returned as part of search results but do not participate in the full-text index
- The `content` field enters the FTS5 index, supporting full-text keyword matching
- `content='messages'` and `content_rowid='id'` associate the virtual table with the `messages` primary table (content table mode), saving disk space

When new messages are written to the `messages` table, the index is synchronously updated via `INSERT INTO messages_fts(...)`, ensuring messages are immediately searchable.

#### SearchMessages Method

The `Manager` gains the following new method:

```go
// MessageSearchResult represents a single full-text search result,
// containing the source session, role, and content of the message.
type MessageSearchResult struct {
    SessionID string
    Role      string
    Content   string
}

// SearchMessages performs an FTS5 full-text search across all historical session messages.
// query is the search keyword (supports FTS5 query syntax); limit controls the maximum number of results returned.
// Results are sorted by FTS5 relevance (bm25), with the most relevant appearing first.
func (m *Manager) SearchMessages(ctx context.Context, query string, limit int) ([]MessageSearchResult, error)
```

Example underlying query:

```sql
SELECT session_id, role, content
FROM messages_fts
WHERE messages_fts MATCH ?
ORDER BY rank
LIMIT ?;
```

#### session_search Tool

The `internal/tools/` package adds a `session_search` tool (a `BaseTool` implementation), enabling the Agent to invoke retrieval directly during a conversation:

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `query` | string | ✅ | FTS5 search keyword |
| `limit` | integer | ❌ | Maximum number of results to return, default 5 |

The tool calls `Manager.SearchMessages()` internally, formats the results, and returns them as tool output to the Agent.

### 14.3 How It Works

```
User inputs a search term (or Agent autonomously invokes the session_search tool)
         │
         ▼
  session_search tool (internal/tools/)
         │   query + limit
         ▼
  Manager.SearchMessages() (internal/memory/)
         │   SQL: SELECT … FROM messages_fts WHERE messages_fts MATCH ? ORDER BY rank LIMIT ?
         ▼
  SQLite FTS5 engine executes BM25-ranked full-text search
         │
         ▼
  []MessageSearchResult{SessionID, Role, Content}
         │
         ▼
  Formatted as text → injected into conversation context as tool output → Agent continues reasoning
```

**Synchronous indexing on write**: `SQLiteSession.AddMessages()` writes to the `messages` table and immediately inserts the corresponding record into `messages_fts` within the same transaction, ensuring the index remains consistent with the primary table at all times — no delays or asynchronous gaps.

### 14.4 Comparison with LTM memory_search

| Dimension | Session FTS (messages_fts) | LTM Memory Search (memories_fts) |
|-----------|---------------------------|----------------------------------|
| **Search target** | Raw messages from historical sessions | Structured knowledge entries extracted by LTM |
| **Coverage** | All historical conversations, without filtering | Only content explicitly recorded by the Extractor or `memory_write` |
| **Granularity** | Individual messages (role + content) | Individual memory entries (category + content + importance) |
| **Update timing** | Synchronously indexed on message write | Extracted before compaction by Extractor / explicit `memory_write` call |
| **Use cases** | Recalling exact wording, finding specific code snippets | Cross-session semantic understanding, user preferences, project context |
| **Storage location** | `sessions.db` messages_fts | `sessions.db` memories_fts |
