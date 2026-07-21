# Long-Term Memory: Cross-Session Long-Term Memory Implementation Principles

## 1. Background and Design Goals

### 1.1 Problem Background

harness9's existing short-term memory (`internal/memory/`) covers history persistence and context compaction within a single session, but cannot retain information across sessions. Every time a new session starts, the Agent knows nothing about user preferences, project background, or historical decisions, and the user has to repeat themselves.

### 1.2 Design Goals

The Long-Term Memory (LTM) system covers the following capabilities:

| Goal | Implementation Mechanism |
|------|---------|
| **Cross-session persistence** | SQLite `long_term_memories` table, reusing the existing `state.db` connection |
| **Bounded token injection** | MEMORY.md materialized view (≤5KB), avoiding token bomb risk |
| **On-demand deep retrieval** | FTS5 full-text search (`memory_search` tool), JIT loading of long-tail memories |
| **Three-way automatic triggering** | Explicit tool call / pre-compaction extraction (Extractor) / Turn-granularity nudge |
| **Forgetting and deduplication** | SHA256 content signature deduplication + TTL expiration + soft delete + staleness detection |
| **Zero new dependencies** | Reuses `modernc.org/sqlite` (already verified to support FTS5) |

---

## 2. Architecture and Package Boundaries

A new self-contained package `internal/ltm/` is added, kept isolated from the short-term memory package `internal/memory/`. The two packages share the same underlying `*sql.DB` connection — the `Manager.DB()` accessor exposes the connection to `ltm.NewStore`, guaranteeing WAL single-writer semantics.

```
┌──────────────────────────────────────────────────────────────────┐
│                          cmd/harness9/main.go                     │
│                                                                  │
│   ltm.NewStore(mgr.DB())      → ltmStore                        │
│   ltm.NewPrecis(ltmStore, path, 5120) → ltmPrecis               │
│   ltm.NewExtractor(llm, ltmStore)     → extractor               │
│   memory.WithMemoryExtractor(extractor)  → inject into Compactor │
│   promptBuilder.WithLongTermMemory(reader)  → System Prompt     │
│   engine.WithMemoryNudge(10, text)    → inject prompt every 10 turns │
└─────────────────────┬───────────────────────────────────────────┘
                      │
          ┌───────────▼──────────┐
          │   internal/ltm/      │
          │                      │
          │  Store               │
          │  ├── Add (signature dedup) │
          │  ├── Get             │
          │  ├── Search (FTS5)    │
          │  ├── Update (rebuild FTS) │
          │  ├── SoftDelete      │
          │  ├── List (top-N)    │
          │  ├── PurgeExpired    │
          │  └── StaleCandidates │
          │                      │
          │  Precis              │
          │  ├── Regenerate      │
          │  └── Read            │
          │                      │
          │  Extractor           │
          │  └── Extract (LLM)   │
          │                      │
          │  Provider/Embedder/  │
          │  Consolidator (seam) │
          └───────────┬──────────┘
                      │
          ┌───────────▼──────────┐
          │ ~/.harness9/         │
          │  sessions.db         │   ← long_term_memories + memories_fts
          │  memories/MEMORY.md  │   ← Precis materialized view
          └──────────────────────┘
```

**Connection sharing mechanism**: `Manager` adds a `DB() *sql.DB` accessor; `ltm.NewStore(db)` performs an idempotent `CREATE TABLE IF NOT EXISTS` migration at construction time. Ownership of the LTM schema stays within the `ltm` package, consistent with the project convention of "data ownership resides on the consumer side."

---

## 3. Package Structure

```
internal/ltm/
├── entry.go         # Entry struct, Category type, Signature (SHA256 dedup), Expired
├── store.go         # Store: schema migration + Add/Get/Search/Update/SoftDelete/List/PurgeExpired/StaleCandidates; var ErrNotFound
├── precis.go        # Precis: Regenerate/Read (MEMORY.md materialized view) + truncateUTF8 (UTF-8-safe truncation)
├── extractor.go     # Extractor (implements memory.MemoryExtractor): LLM pre-compaction fact extraction + Generator interface
├── provider.go      # Phase 3 seam: Provider/Embedder/Consolidator interfaces + noopProvider
├── entry_test.go
├── store_test.go
├── precis_test.go
├── extractor_test.go
└── provider_test.go

internal/tools/
├── memory_write.go  # MemoryWriteTool: add/update (merge)/remove three actions + Precis rebuild
└── memory_search.go # MemorySearchTool: FTS5 retrieval + reinforcement side effect
```

---

## 4. Storage Schema

Persistence path: `~/.harness9/sessions.db` (shared with the same file as short-term memory)

```sql
CREATE TABLE IF NOT EXISTS long_term_memories (
    id           TEXT PRIMARY KEY,
    title        TEXT NOT NULL,
    content      TEXT NOT NULL,
    category     TEXT,                 -- knowledge | preference | task | skill
    importance   INTEGER NOT NULL DEFAULT 0,  -- 0-10, determines precis ranking + staleness detection
    signature    TEXT UNIQUE,          -- SHA256(normalize(content)), dedup fingerprint; set to NULL on soft delete to free the slot
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    use_count    INTEGER NOT NULL DEFAULT 0,
    ttl_days     INTEGER,              -- NULL = never expires
    disabled     INTEGER NOT NULL DEFAULT 0,  -- soft delete flag
    tags         TEXT                  -- JSON array
);

CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(id UNINDEXED, title, content);
```

`memories_fts` uses **standalone mode** (not an external content table), manually synchronized by code: inserted on `Add`, deleted-then-reinserted on `Update`, deleted on `SoftDelete`/`PurgeExpired`. This avoids trigger dependence on the SQLite version and also keeps the control logic explicitly visible.

### 4.1 Go Data Structures

```go
// Category affects precis rendering and retrieval semantics.
type Category string  // "knowledge" | "preference" | "task" | "skill"

type Entry struct {
    ID         string
    Title      string
    Content    string
    Category   Category
    Importance int       // 0-10, determines precis ranking and staleness detection
    Signature  string    // SHA256(normalize(content)), json:"-"
    CreatedAt  time.Time
    UpdatedAt  time.Time
    LastUsedAt time.Time
    UseCount   int
    TTLDays    int       // 0 = never expires
    Disabled   bool      // json:"-"
    Tags       []string
}
```

### 4.2 Core Store Methods

| Method | Semantics |
|------|------|
| `Add(ctx, *Entry) (*Entry, error)` | Writes a new entry; if the same `signature` exists and is not disabled, it's treated as a dedup hit — `updated_at` is refreshed and `use_count` incremented, without inserting a new row |
| `Get(ctx, id) (*Entry, error)` | Returns an entry by ID (including soft-deleted ones, for auditing); returns `ErrNotFound` if it doesn't exist |
| `Search(ctx, query, limit) ([]*Entry, error)` | FTS5 full-text search, ranked by relevance; reinforces on hit (`use_count+1` / writes `last_used_at`) |
| `Update(ctx, *Entry) error` | Updates fields by `ID`, recomputes `signature`, rebuilds the FTS index within the transaction |
| `SoftDelete(ctx, id) error` | Sets `disabled=1`, `signature=NULL` (freeing the UNIQUE slot), removes from FTS |
| `List(ctx, limit) ([]*Entry, error)` | Returns non-deleted, non-expired entries, sorted by `importance DESC, updated_at DESC`, for Precis rendering |
| `PurgeExpired(ctx) (int, error)` | Batch soft-deletes entries past their TTL (sets `disabled=1`, `signature=NULL`), synchronously cleans up FTS, returns the reclaimed count |
| `StaleCandidates(ctx) ([]*Entry, error)` | Identifies cleanup candidates: `importance<=1 AND use_count=0 AND not updated in 60 days` |

---

## 5. MEMORY.md Materialized View

### 5.1 Design Principles

The SQLite `long_term_memories` table is the **single source of truth**. `MEMORY.md` is a bounded file automatically rendered from the top-N highest-value entries — it is not independent storage, must not be manually edited, and is rebuilt by `Precis.Regenerate` after every memory write.

This design avoids the "two sources of truth drifting apart" problem, and also naturally avoids token bomb risk: the precis file has a hard byte cap (5120 bytes by default) and does not grow linearly with the total volume of memories.

### 5.2 Precis Implementation

```go
// Precis maintains the MEMORY.md materialized view.
type Precis struct {
    store    *Store
    path     string  // absolute path, defaults to ~/.harness9/memories/MEMORY.md
    maxBytes int     // injection budget cap, defaults to 5120
}

func NewPrecis(store *Store, path string, maxBytes int) *Precis
func (p *Precis) Regenerate(ctx context.Context) error  // fetch top-30 entries → render → write file
func (p *Precis) Read() (string, error)                 // read the file; returns empty string if it doesn't exist (no error)
```

**Render format** (`renderPrecis`): each entry is rendered as `## {title} \`{category}\`` plus content, separated by `\n\n`. When it exceeds `maxBytes`, `truncateUTF8` safely truncates at a UTF-8 byte boundary and appends a `\n…(truncated)` marker.

**Trigger timing**: `MemoryWriteTool.Execute` calls `Precis.Regenerate` after every successful write (fail-soft: failures are only logged, not blocking the tool's return). `main.go` also calls it once at startup to ensure the file stays in sync with the database.

---

## 6. Three-Way Triggering

### 6.1 Explicit Tool Calls

Actively invoked by the LLM, available at any time.

**`memory_write` (`MemoryWriteTool`)**:

| Parameter `action` | Behavior |
|--------------|------|
| `add` | Adds a new memory (`content` required; content signature auto-deduplicates) |
| `update` | Partial merge update (first `Get` the original value, only overwrite fields explicitly provided by the caller) |
| `remove` | Soft-deletes by `id` |

MEMORY.md is rebuilt after every successful write.

**`memory_search` (`MemorySearchTool`)**:

Accepts `query` (required) and `limit` (optional, defaults to 5), retrieves non-disabled, non-expired memories via FTS5, ranked by relevance, and returns a JSON array. Hit entries are automatically reinforced (`use_count+1`).

### 6.2 Pre-Compaction Extractor

`SummarizationCompactor.Compact` calls `MemoryExtractor.Extract(head)` to extract durable facts **before** the `head` messages are wiped into a summary.

The interface is defined on the consumer side (the `memory` package) and implemented by `ltm.Extractor`, avoiding `memory` depending on `ltm`:

```go
// memory package (consumer side)
type MemoryExtractor interface {
    Extract(msgs []schema.Message)
}

// WithMemoryExtractor injects the extractor, extracting durable facts from head messages before every compaction summary.
func WithMemoryExtractor(ex MemoryExtractor) CompactorOption
```

`Extractor`'s behavior:

1. Flatten the `head` messages into conversation text (`renderConversation`)
2. Build a prompt from `extractSystemPrompt` plus the conversation text, and call the LLM (60s timeout)
3. Parse a JSON array (tolerating ` ```json ``` ` code fences), with each entry as `{title, content, category, importance}`
4. `store.Add` each entry one by one (signature dedup)

**Fail-open principle**: any error along the way is only logged and never blocks the compaction flow. The `Extract` method does not return an `error`.

```go
// ltm package
type Generator interface {
    Generate(ctx context.Context, messages []schema.Message, tools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error)
}

type Extractor struct { gen Generator; store *Store }

func NewExtractor(gen Generator, store *Store) *Extractor
func (e *Extractor) Extract(msgs []schema.Message)  // implements memory.MemoryExtractor
```

### 6.3 Turn-Granularity Nudge

`engine.WithMemoryNudge(interval, text)` configures nudge behavior. Every `interval` turns (`turnCount % interval == 0`), the engine appends `text` to a **defensive copy** of the history sent to the LLM — it is not written into `contextHistory`, not persisted, and does not accumulate.

```go
func WithMemoryNudge(interval int, text string) Option
```

Default configuration in main.go:

```go
engine.WithMemoryNudge(10,
    "If this turn's conversation contains information worth retaining across sessions (user preferences, stable " +
    "project knowledge, key decisions, reusable skills), call the memory_write tool to record it; otherwise ignore this prompt.")
```

The nudge is disabled when interval=0 or text="" (disabled by default, requires explicit configuration).

---

## 7. Context Injection

### 7.1 Real-Time System Prompt Injection (Re-Read Every Turn)

`DefaultPromptBuilder.WithLongTermMemory(reader func() string)` accepts a read closure, which is invoked every time `Build()` assembles the System Prompt to read the **latest** MEMORY.md content, injected into section 6 ("## Long-Term Memory"):

```
## Long-Term Memory

Below is the distilled long-term memory accumulated across sessions. When more historical detail is needed,
use the `memory_search` tool to retrieve it; when new information worth retaining long-term is found,
use the `memory_write` tool to record it.

{MEMORY.md content}
```

When the reader returns an empty string, the entire section is skipped and not injected. **The injected content is re-read every turn in real time** (rather than a snapshot fixed at process startup) — so memories written by the Agent via `memory_write` during the session, once persisted through `Precis.Regenerate`, become immediately visible in the System Prompt precis on the very next turn, with no process restart needed.

**main.go wiring** (passing a closure that reads the Precis, rather than a one-time string):

```go
promptBuilder = promptBuilder.WithLongTermMemory(func() string {
    content, _ := ltmPrecis.Read()
    return content
})
```

### 7.2 On-Demand Retrieval (FTS5 JIT)

The `memory_search` tool provides on-demand full-text retrieval, injecting the detailed content of long-tail memories into the current Turn's Observation context via the tool's return value, without consuming the fixed System Prompt budget.

---

## 8. Conflict / Forgetting / Reinforcement Mechanisms

| Mechanism | Implementation |
|------|------|
| **SHA256 deduplication** | `Signature(content) = SHA256(normalize(content))`; `normalize` collapses whitespace + lowercases + trims leading/trailing whitespace; when `Add` hits an existing signature, `updated_at` is refreshed and `use_count` incremented, without inserting a new row |
| **TTL expiration** | `ttl_days` field; `List`/`Search` filter on read (`updated_at + ttl_days * 86400 < now`); `PurgeExpired` batch soft-deletes; `main.go` calls it once at startup for cleanup |
| **Soft delete** | `disabled=1`, never physically deleted (preserves audit history); `signature` is simultaneously set to `NULL` to free the UNIQUE constraint slot, allowing the same content to be re-added in the future |
| **Reinforcement** | Executed on every `Search` hit: `use_count+1`, `last_used_at=now`; feeds back into the importance weighting, keeping frequently used memories ranked high in `List` |
| **Staleness detection** | `StaleCandidates`: `importance<=1 AND use_count=0 AND updated_at < now-60 days`; the result can be used by the LLM or background logic to decide whether to delete |
| **Contradiction resolution** | Resolved by the LLM via `memory_write update/remove` (intent-driven); the system does not perform automatic arbitration |

---

## 9. Phase 3 Seam

`internal/ltm/provider.go` defines the following interfaces (seam only; currently no real implementation other than `noopProvider`):

```go
// Provider is the extension interface for external memory providers (Phase 3).
// Modeled on HermesAgent's provider plugin system; can later integrate Mem0 / Honcho / vector stores as external backends.
type Provider interface {
    Prefetch(ctx context.Context, query string) ([]*Entry, error)       // pre-turn prefetch
    Sync(ctx context.Context, userContent, assistantContent string) error // post-turn sync
    OnPreCompress(ctx context.Context, msgs []schema.Message) error      // pre-compaction hook
    OnSessionEnd(ctx context.Context) error                              // session-end hook
}

// Embedder is the vector embedding interface (Phase 3), can later connect to Ollama / OpenAI Embeddings.
type Embedder interface {
    Embed(ctx context.Context, text string) ([]float32, error)
}

// Consolidator is the Dreaming consolidation interface (Phase 3), can later be batch-promoted from short-term signals to long-term memory via cron.
type Consolidator interface {
    Consolidate(ctx context.Context) (promoted int, err error)
}

// NewNoopProvider returns a Provider whose hooks are all no-ops.
func NewNoopProvider() Provider
```

These interfaces compile, are covered by tests in noop form, and provide a stable seam for future expansion without introducing any runtime cost.

---

## 10. main.go Initialization Sequence

```go
// 1. Obtain the shared DB connection from the Manager, initialize the LTM Store (idempotent migration)
ltmStore, err := ltm.NewStore(mgr.DB())

// 2. Create the Precis (materialized view)
memoryFilePath := filepath.Join(harness9Dir, "memories", "MEMORY.md")
ltmPrecis := ltm.NewPrecis(ltmStore, memoryFilePath, 5120)

// 3. Clean up expired memories at startup + rebuild the precis file
ltmStore.PurgeExpired(ctx)
ltmPrecis.Regenerate(ctx)

// 4. Inject the precis read closure into the System Prompt (re-read every turn on Build, written content visible on the very next turn)
promptBuilder = promptBuilder.WithLongTermMemory(func() string {
    content, _ := ltmPrecis.Read()
    return content
})

// 5. Register LTM tools
registry.Register(tools.NewMemoryWriteTool(ltmStore, ltmPrecis))
registry.Register(tools.NewMemorySearchTool(ltmStore))

// 6. Inject the Extractor into the compactor
compactor := memory.NewSummarizationCompactor(llm, modelLimits.ContextTokens,
    memory.WithMemoryExtractor(ltm.NewExtractor(llm, ltmStore)),
    // ...other options
)

// 7. Configure the Turn nudge
eng := engine.NewAgentEngine(llm, registry, workDir,
    engine.WithMemoryNudge(10, nudgeText),
    // ...other options
)
```

---

## 11. Design Decision Summary

| Decision | Rationale |
|------|------|
| **Standalone `ltm` package, not merged into `memory`** | `memory` is explicitly defined in the project as short-term memory; mixing in long-term memory would blur module boundaries and hinder future independent expansion |
| **Reuse `state.db`, don't open a new connection** | WAL mode requires a single writer; a new connection would break transaction isolation and introduce races |
| **Materialized view (MEMORY.md) instead of real-time rendering** | Single source of truth (SQLite) + bounded injection (≤5KB), avoiding token bomb; the cost of re-rendering on every write is negligible |
| **Precis injection uses a read closure (re-read every turn)** | `WithLongTermMemory` accepts a `func() string` rather than a static string; `Build()` re-reads MEMORY.md every turn; memories newly written within a session become visible in the System Prompt on the very next turn without a restart; the cost of reading a ≤5KB file is negligible |
| **Standalone FTS5, manually synchronized** | Explicit control over insert/delete/update timing, no triggers needed, no additional requirements on the SQLite version |
| **`signature=NULL` on soft delete** | Frees the UNIQUE slot, allowing the same content to be re-added in the future, avoiding permanent blocking |
| **`MemoryExtractor` interface defined in the `memory` package** | Consumer-side definition principle; the `memory` package need not import `ltm`, avoiding circular dependencies |
| **Extractor fail-open** | Extraction is an enhancement, not a core flow; failure should not block compaction or interrupt Agent operation |
| **Nudge injects into the defensive copy** | The nudge is a one-time prompt and should not be persisted or injected into the summary, avoiding context pollution |
| **Phase 3 is interfaces only** | Vector embeddings, external providers, and Dreaming consolidation are P3 features (YAGNI); interface placeholders allow future zero-breakage expansion |

---

## 12. Future Roadmap

| Feature | Priority | Description |
|------|--------|------|
| Vector embedding semantic retrieval | P3 | Integrate Ollama / OpenAI Embeddings, implement the `Embedder` interface, add a semantic recall path to `Search` |
| Dreaming consolidation | P3 | Implement the `Consolidator` interface, background cron batch-promotes high-value signals from short-term conversation |
| External memory providers | P3 | Implement the `Provider` interface, integrate external memory services such as Mem0 / Honcho |
| Automatic stale memory cleanup | P3 | Periodic reclamation based on `StaleCandidates`, controlling storage growth |
