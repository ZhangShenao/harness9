# Long-Term Memory 跨会话长期记忆实现原理

## 1. 背景与设计目标

### 1.1 问题背景

harness9 原有的短期记忆（`internal/memory/`）覆盖了单次会话内的历史持久化与上下文压缩，但无法跨会话保留信息。每次新会话启动时，Agent 对用户偏好、项目背景、历史决策一无所知，需要用户反复说明。

### 1.2 设计目标

Long-Term Memory（LTM）系统覆盖以下能力：

| 目标 | 实现机制 |
|------|---------|
| **跨会话持久化** | SQLite `long_term_memories` 表，复用现有 `state.db` 连接 |
| **有界 Token 注入** | MEMORY.md 物化视图（≤5KB），规避 token bomb 风险 |
| **按需深度检索** | FTS5 全文检索（`memory_search` 工具），JIT 加载长尾记忆 |
| **三路自动触发** | 显式工具 / 压缩前提取（Extractor）/ Turn 粒度 nudge |
| **遗忘与去重** | SHA256 内容签名去重 + TTL 过期 + 软删除 + 陈旧识别 |
| **零新增依赖** | 复用 `modernc.org/sqlite`（已验证支持 FTS5） |

---

## 2. 架构与包边界

新增自包含包 `internal/ltm/`，与短期记忆包 `internal/memory/` 保持隔离。两个包共享同一个底层 `*sql.DB` 连接——`Manager.DB()` 访问器向 `ltm.NewStore` 暴露连接，保证 WAL 单写者语义。

```
┌──────────────────────────────────────────────────────────────────┐
│                          cmd/harness9/main.go                     │
│                                                                  │
│   ltm.NewStore(mgr.DB())      → ltmStore                        │
│   ltm.NewPrecis(ltmStore, path, 5120) → ltmPrecis               │
│   ltm.NewExtractor(llm, ltmStore)     → extractor               │
│   memory.WithMemoryExtractor(extractor)  → 注入 Compactor        │
│   promptBuilder.WithLongTermMemory(reader)  → System Prompt     │
│   engine.WithMemoryReminder(10, text)  → 每 10 轮注入提示         │
└─────────────────────┬───────────────────────────────────────────┘
                      │
          ┌───────────▼──────────┐
          │   internal/ltm/      │
          │                      │
          │  Store               │
          │  ├── Add（签名去重）   │
          │  ├── Get             │
          │  ├── Search（FTS5）   │
          │  ├── Update（重建FTS）│
          │  ├── SoftDelete      │
          │  ├── List（top-N）    │
          │  ├── PurgeExpired    │
          │  └── StaleCandidates │
          │                      │
          │  Precis              │
          │  ├── Regenerate      │
          │  └── Read            │
          │                      │
          │  Extractor           │
          │  └── Extract（LLM）  │
          │                      │
          │  Provider/Embedder/  │
          │  Consolidator（接缝） │
          └───────────┬──────────┘
                      │
          ┌───────────▼──────────┐
          │ ~/.harness9/         │
          │  sessions.db         │   ← long_term_memories + memories_fts
          │  memories/MEMORY.md  │   ← Precis 物化视图
          └──────────────────────┘
```

**连接共享机制**：`Manager` 新增 `DB() *sql.DB` 访问器，`ltm.NewStore(db)` 在构造时执行 `CREATE TABLE IF NOT EXISTS` 幂等迁移。LTM schema 的所有权留在 `ltm` 包内，符合"数据归属在使用者侧"的项目惯例。

---

## 3. 包结构

```
internal/ltm/
├── entry.go         # Entry 结构体、Category 类型、Signature（SHA256 去重）、Expired
├── store.go         # Store：schema 迁移 + Add/Get/Search/Update/SoftDelete/List/PurgeExpired/StaleCandidates；var ErrNotFound
├── precis.go        # Precis：Regenerate/Read（MEMORY.md 物化视图）+ truncateUTF8（UTF-8 安全截断）
├── extractor.go     # Extractor（实现 memory.MemoryExtractor）：LLM 压缩前事实提取 + Generator 接口
├── provider.go      # Phase 3 接缝：Provider/Embedder/Consolidator 接口 + noopProvider
├── entry_test.go
├── store_test.go
├── precis_test.go
├── extractor_test.go
└── provider_test.go

internal/tools/
├── memory_write.go  # MemoryWriteTool：add/update（merge）/remove 三动作 + Precis 重建
└── memory_search.go # MemorySearchTool：FTS5 检索 + 强化副作用
```

---

## 4. 存储 Schema

持久化路径：`~/.harness9/sessions.db`（与短期记忆共用同一文件）

```sql
CREATE TABLE IF NOT EXISTS long_term_memories (
    id           TEXT PRIMARY KEY,
    title        TEXT NOT NULL,
    content      TEXT NOT NULL,
    category     TEXT,                 -- knowledge | preference | task | skill
    importance   INTEGER NOT NULL DEFAULT 0,  -- 0-10，决定精华排序 + 陈旧识别
    signature    TEXT UNIQUE,          -- SHA256(normalize(content))，去重指纹；软删除时置 NULL 释放槽位
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    use_count    INTEGER NOT NULL DEFAULT 0,
    ttl_days     INTEGER,              -- NULL = 永不过期
    disabled     INTEGER NOT NULL DEFAULT 0,  -- 软删除标志
    tags         TEXT                  -- JSON 数组
);

CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(id UNINDEXED, title, content);
```

`memories_fts` 采用 **standalone 模式**（非外部内容表），由代码手动同步：`Add` 时插入，`Update` 时删除再插入，`SoftDelete`/`PurgeExpired` 时删除。这避免了触发器对 SQLite 版本的依赖，也使控制逻辑显式可见。

### 4.1 Go 数据结构

```go
// Category 影响精华渲染与检索语义。
type Category string  // "knowledge" | "preference" | "task" | "skill"

type Entry struct {
    ID         string
    Title      string
    Content    string
    Category   Category
    Importance int       // 0-10，决定精华排序与陈旧识别
    Signature  string    // SHA256(normalize(content))，json:"-"
    CreatedAt  time.Time
    UpdatedAt  time.Time
    LastUsedAt time.Time
    UseCount   int
    TTLDays    int       // 0 = 永不过期
    Disabled   bool      // json:"-"
    Tags       []string
}
```

### 4.2 Store 核心方法

| 方法 | 语义 |
|------|------|
| `Add(ctx, *Entry) (*Entry, error)` | 写入新条目；相同 `signature` 且未禁用时视为去重命中，刷新 `updated_at` 并自增 `use_count`，不插入新行 |
| `Get(ctx, id) (*Entry, error)` | 按 ID 返回条目（含软删除，便于审计）；不存在时返回 `ErrNotFound` |
| `Search(ctx, query, limit) ([]*Entry, error)` | FTS5 全文检索，按相关度排序；命中后执行强化（`use_count+1` / 写 `last_used_at`） |
| `Update(ctx, *Entry) error` | 按 `ID` 更新字段，重算 `signature`，事务内重建 FTS 索引 |
| `SoftDelete(ctx, id) error` | 置 `disabled=1`、`signature=NULL`（释放 UNIQUE 槽位），移出 FTS |
| `List(ctx, limit) ([]*Entry, error)` | 返回未删除、未过期条目，按 `importance DESC, updated_at DESC` 排序，供 Precis 渲染 |
| `PurgeExpired(ctx) (int, error)` | 批量软删除已超 TTL 的条目（置 `disabled=1`、`signature=NULL`），同步清理 FTS，返回回收数 |
| `StaleCandidates(ctx) ([]*Entry, error)` | 识别清理候选：`importance<=1 AND use_count=0 AND 60 天未更新` |

---

## 5. MEMORY.md 物化视图

### 5.1 设计原则

SQLite `long_term_memories` 表是**唯一事实源**。`MEMORY.md` 是由 top-N 高价值条目自动渲染出的有界文件——不是独立存储，不允许手工编辑，每次写入记忆后由 `Precis.Regenerate` 重建。

这一设计规避了"两个事实源漂移"问题，也天然规避了 token bomb 风险：精华文件有硬字节上限（默认 5120 字节），不随记忆总量线性膨胀。

### 5.2 Precis 实现

```go
// Precis 维护 MEMORY.md 物化视图。
type Precis struct {
    store    *Store
    path     string  // 绝对路径，默认 ~/.harness9/memories/MEMORY.md
    maxBytes int     // 注入预算上限，默认 5120
}

func NewPrecis(store *Store, path string, maxBytes int) *Precis
func (p *Precis) Regenerate(ctx context.Context) error  // 拉取 top-30 条目 → 渲染 → 写文件
func (p *Precis) Read() (string, error)                 // 读文件；不存在时返回空串（不报错）
```

**渲染格式**（`renderPrecis`）：每条条目渲染为 `## {title} \`{category}\`` + 内容，以 `\n\n` 分隔。超出 `maxBytes` 时 `truncateUTF8` 在 UTF-8 字节边界安全截断并追加 `\n…（已截断）` 标记。

**触发时机**：`MemoryWriteTool.Execute` 在每次成功写入后调用 `Precis.Regenerate`（fail-soft：失败仅记日志，不阻断工具返回）。启动时 `main.go` 也调用一次以确保文件与数据库同步。

---

## 6. 三路触发

### 6.1 显式工具调用

LLM 主动调用，随时可用。

**`memory_write`（`MemoryWriteTool`）**：

| 参数 `action` | 行为 |
|--------------|------|
| `add` | 新增记忆（`content` 必填；内容签名自动去重） |
| `update` | 部分 merge 更新（先 `Get` 取原值，仅覆盖调用方显式提供的字段） |
| `remove` | 按 `id` 软删除 |

每次成功写入后重建 MEMORY.md。

**`memory_search`（`MemorySearchTool`）**：

接受 `query`（必填）和 `limit`（可选，默认 5），通过 FTS5 检索未禁用、未过期的记忆，按相关度排序返回 JSON 数组。命中条目自动强化（`use_count+1`）。

### 6.2 压缩前 Extractor

`SummarizationCompactor.Compact` 在将 `head` 消息摘要抹除**之前**，调用 `MemoryExtractor.Extract(head)` 提取持久事实。

接口定义在使用者侧（`memory` 包），由 `ltm.Extractor` 实现，避免 `memory` 依赖 `ltm`：

```go
// memory 包（使用者侧）
type MemoryExtractor interface {
    Extract(msgs []schema.Message)
}

// WithMemoryExtractor 注入提取器，在每次压缩摘要前从 head 消息提取持久事实。
func WithMemoryExtractor(ex MemoryExtractor) CompactorOption
```

`Extractor` 的行为：

1. 将 `head` 消息扁平化为对话文本（`renderConversation`）
2. 以 `extractSystemPrompt` + 对话文本构造 prompt，调用 LLM（60s 超时）
3. 解析 JSON 数组（容忍 ` ```json ``` ` 代码围栏），每条 `{title, content, category, importance}`
4. 逐条 `store.Add`（签名去重）

**Fail-open 原则**：任何环节出错仅记日志，绝不阻断压缩流程。`Extract` 方法不返回 `error`。

```go
// ltm 包
type Generator interface {
    Generate(ctx context.Context, messages []schema.Message, tools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error)
}

type Extractor struct { gen Generator; store *Store }

func NewExtractor(gen Generator, store *Store) *Extractor
func (e *Extractor) Extract(msgs []schema.Message)  // 实现 memory.MemoryExtractor
```

### 6.3 Turn 粒度 Reminder

`engine.WithMemoryReminder(interval, text)` 配置记忆提醒行为。每隔 `interval` 轮（`turnCount % interval == 0`），引擎将 `text` 追加到发送给 LLM 的历史**防御性副本**中——不写入 `contextHistory`，不持久化，不累积。

> 命名说明：此机制原名为 `WithMemoryNudge`，已随 loopGuard 护栏体系统一更名为 `WithMemoryReminder`（语义不变），与重复提醒 / 停滞提醒共享三源仲裁通道（每轮至多一条干预消息）。

```go
func WithMemoryReminder(interval int, text string) Option
```

main.go 默认配置：

```go
engine.WithMemoryReminder(10,
    "如果本轮对话中出现了值得跨会话长期保留的信息（用户偏好、稳定的项目知识、" +
    "关键决策、可复用技能），请调用 memory_write 工具记录；否则忽略此提示。")
```

interval=0 或 text="" 时关闭 reminder（默认关闭，需显式配置）。

---

## 7. Context 注入

### 7.1 System Prompt 实时注入（每轮重读）

`DefaultPromptBuilder.WithLongTermMemory(reader func() string)` 接收一个读取闭包，在每次 `Build()` 组装 System Prompt 时调用它读取**最新**的 MEMORY.md 内容，注入第 6 段（"## 长期记忆"）：

```
## 长期记忆

以下是跨会话积累的长期记忆精华。需要更多历史细节时，使用 `memory_search` 工具检索；
发现值得长期保留的新信息时，使用 `memory_write` 工具记录。

{MEMORY.md 内容}
```

reader 返回空串时整段跳过，不注入。**注入内容每轮实时读取**（而非进程启动时快照固定）——因此 Agent 在会话中通过 `memory_write` 写入的记忆经 `Precis.Regenerate` 落盘后，会在下一轮对话的 System Prompt 精华中立即可见，无需重启进程。

**main.go 接线**（传入读取 Precis 的闭包，而非一次性字符串）：

```go
promptBuilder = promptBuilder.WithLongTermMemory(func() string {
    content, _ := ltmPrecis.Read()
    return content
})
```

### 7.2 按需检索（FTS5 JIT）

`memory_search` 工具提供按需全文检索，将长尾记忆的详细内容以工具返回值注入当前 Turn 的 Observation 上下文，不占用固定 System Prompt 预算。

---

## 8. 冲突 / 遗忘 / 强化机制

| 机制 | 实现 |
|------|------|
| **SHA256 去重** | `Signature(content) = SHA256(normalize(content))`；`normalize` 折叠空白 + 小写化 + 去首尾空白；`Add` 命中签名时刷新 `updated_at` + 自增 `use_count`，不插入新行 |
| **TTL 过期** | `ttl_days` 字段；`List`/`Search` 读取时过滤（`updated_at + ttl_days * 86400 < now`）；`PurgeExpired` 批量软删除；`main.go` 启动时调用一次清理 |
| **软删除** | `disabled=1`，绝不物理删除（保留审计历史）；`signature` 同时置 `NULL` 以释放 UNIQUE 约束槽位，使相同内容可在未来重新添加 |
| **强化** | `Search` 命中即执行：`use_count+1`、`last_used_at=now`；反哺 importance 权重，使常用记忆在 `List` 中维持高位 |
| **陈旧识别** | `StaleCandidates`：`importance<=1 AND use_count=0 AND updated_at < now-60天`；结果可供 LLM 或后台逻辑决定是否删除 |
| **矛盾冲突** | 由 LLM 通过 `memory_write update/remove`（意图驱动）解决；系统不做自动仲裁 |

---

## 9. Phase 3 接缝

`internal/ltm/provider.go` 定义以下接口（仅接缝，当前除 `noopProvider` 外无真实实现）：

```go
// Provider 是外部记忆提供者的扩展接口（Phase 3）。
// 参考 HermesAgent 提供者插件系统，后续可接入 Mem0 / Honcho / 向量库等外部后端。
type Provider interface {
    Prefetch(ctx context.Context, query string) ([]*Entry, error)       // Turn 前预取
    Sync(ctx context.Context, userContent, assistantContent string) error // Turn 后同步
    OnPreCompress(ctx context.Context, msgs []schema.Message) error      // 压缩前钩子
    OnSessionEnd(ctx context.Context) error                              // 会话结束钩子
}

// Embedder 向量嵌入接口（Phase 3），后续可接 Ollama / OpenAI Embeddings。
type Embedder interface {
    Embed(ctx context.Context, text string) ([]float32, error)
}

// Consolidator Dreaming 巩固接口（Phase 3），后续可由 cron 批量晋升短期信号为长期记忆。
type Consolidator interface {
    Consolidate(ctx context.Context) (promoted int, err error)
}

// NewNoopProvider 返回所有钩子均为无操作的 Provider。
func NewNoopProvider() Provider
```

这些接口可编译，以 noop 形式被测试覆盖，为未来扩展提供稳定接缝而不引入任何运行期成本。

---

## 10. main.go 初始化序列

```go
// 1. 从 Manager 获取共享 DB 连接，初始化 LTM Store（幂等迁移）
ltmStore, err := ltm.NewStore(mgr.DB())

// 2. 创建 Precis（物化视图）
memoryFilePath := filepath.Join(harness9Dir, "memories", "MEMORY.md")
ltmPrecis := ltm.NewPrecis(ltmStore, memoryFilePath, 5120)

// 3. 启动时清理过期记忆 + 重建精华文件
ltmStore.PurgeExpired(ctx)
ltmPrecis.Regenerate(ctx)

// 4. 注入精华读取闭包到 System Prompt（每轮 Build 时实时重读，写入即下一轮可见）
promptBuilder = promptBuilder.WithLongTermMemory(func() string {
    content, _ := ltmPrecis.Read()
    return content
})

// 5. 注册 LTM 工具
registry.Register(tools.NewMemoryWriteTool(ltmStore, ltmPrecis))
registry.Register(tools.NewMemorySearchTool(ltmStore))

// 6. 注入 Extractor 到压缩器
compactor := memory.NewSummarizationCompactor(llm, modelLimits.ContextTokens,
    memory.WithMemoryExtractor(ltm.NewExtractor(llm, ltmStore)),
    // ...其他选项
)

// 7. 配置 Turn reminder
eng := engine.NewAgentEngine(llm, registry, workDir,
    engine.WithMemoryReminder(10, nudgeText),
    // ...其他选项
)
```

---

## 11. 设计决策总结

| 决策 | 原因 |
|------|------|
| **独立 `ltm` 包，不并入 `memory`** | `memory` 在项目中明确定义为短期记忆；混入长期记忆会模糊模块边界，阻碍后续独立扩展 |
| **复用 `state.db`，不新开连接** | WAL 模式要求单写者；新连接会破坏事务隔离，引入竞态 |
| **物化视图（MEMORY.md）而非实时渲染** | 单一事实源（SQLite）+ 有界注入（≤5KB），规避 token bomb；每次写入重渲成本可忽略 |
| **精华注入用读取闭包（每轮重读）** | `WithLongTermMemory` 接收 `func() string` 而非静态字符串，`Build()` 每轮重读 MEMORY.md；会话内新写入的记忆下一轮即在 System Prompt 可见，无需重启；读取 ≤5KB 文件成本可忽略 |
| **standalone FTS5，手动同步** | 显式控制插入/删除/更新时机，无需触发器，对 SQLite 版本无额外要求 |
| **`signature=NULL` 于软删除** | 释放 UNIQUE 槽位，使相同内容可在未来重新被添加，不造成永久封锁 |
| **`MemoryExtractor` 接口定义在 `memory` 包** | 使用者侧定义原则；`memory` 包无需 import `ltm`，避免循环依赖 |
| **Extractor fail-open** | 提取是增强功能，不是核心流程；失败不应阻断压缩或中断 Agent 运行 |
| **Nudge 注入防御性副本** | nudge 是一次性提示，不应被持久化或注入摘要，避免上下文污染 |
| **Phase 3 仅接口** | 向量嵌入、外部提供者、Dreaming 巩固属于 P3 功能（YAGNI）；接口占位允许未来零破坏性扩展 |

---

## 12. 后续 Roadmap

| 功能 | 优先级 | 说明 |
|------|--------|------|
| 向量嵌入语义检索 | P3 | 接入 Ollama / OpenAI Embeddings，实现 `Embedder` 接口，为 `Search` 增加语义召回路径 |
| Dreaming 巩固 | P3 | 实现 `Consolidator` 接口，后台 cron 批量晋升短期对话中的高价值信号 |
| 外部记忆提供者 | P3 | 实现 `Provider` 接口，接入 Mem0 / Honcho 等外部记忆服务 |
| 陈旧记忆自动清理 | P3 | 基于 `StaleCandidates` 定期回收，控制存储增长 |
