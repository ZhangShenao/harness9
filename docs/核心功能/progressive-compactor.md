# ProgressiveCompactor 分层渐进式上下文压缩

## 1. 概述

ProgressiveCompactor 是 harness9 的默认上下文压缩策略，解决长程 Agent 任务中上下文窗口不断膨胀的核心挑战。与传统的"单阈值全量截断"不同，它采用**分层渐进**架构：根据 token 占用比例分为四个档位（Warn / Soft / Full / Emergency），每个档位执行不同强度的压缩动作，从轻量 offload 到 LLM 摘要再到紧急截断，平滑过渡而非突变。

### 1.1 设计目标

| 目标 | 机制 |
|------|------|
| **渐进式触发** | 四层阈值（60% / 70% / 80% / 95%），避免"要么不压要么全压" |
| **信息不丢失** | 大 tool_result offload 到文件 + 结构化 Anchor 锚点保底 |
| **关键信息保留** | 五类锚点（用户意图/进度/决策/尝试/下一步）程序化可校验 |
| **全链路可追踪** | CompactionRecord 记录每次压缩的完整细节，持久化到 JSONL |
| **跨轮增量更新** | 内部状态追踪上次摘要，增量合并避免信息叠加丢失 |
| **向后兼容** | 实现 Compactor + ForceCompactor + RecordedCompactor 三接口 |

### 1.2 核心架构

```
                    CompactWithRecord(msgs)
                            │
                    ┌───────▼────────┐
                    │ determineTier() │ ── ratio = EstimateTokens(msgs) / ContextWindow
                    └───────┬────────┘
                            │
          ┌─────────┬───────┼───────┬──────────┐
          │         │       │       │          │
      TierNone   TierWarn  TierSoft  TierFull  TierEmergency
      (<60%)     (60-70%)  (70-80%)  (80-95%)  (≥95%)
          │         │       │       │          │
       原样返回   offload   offload  offload   强制截断
                  only      +摘要½   +摘要全   (Fallback)
                  head      head     head
                            +锚点    +锚点
```

---

## 2. 分层压缩机制

### 2.1 四层 Tier 定义

```go
type CompactionTier int

const (
    TierNone      CompactionTier = iota // <60%: 无需压缩
    TierWarn                            // 60-70%: 仅 offload 大结果
    TierSoft                            // 70-80%: offload + 摘要最旧 1/2 head
    TierFull                            // 80-95%: offload + 摘要全 head + anchor
    TierEmergency                       // ≥95%: 强制截断回退
)
```

### 2.2 触发时机判定

每个 Turn 在 LLM 调用前，`CompactWithRecord` 被调用。它首先通过 `determineTier` 判定当前应触发哪个档位：

```go
func (c *ProgressiveCompactor) determineTier(msgs []schema.Message) CompactionTier {
    if c.ContextWindow <= 0 {
        return TierNone
    }
    ratio := float64(EstimateTokens(msgs)) / float64(c.ContextWindow)
    switch {
    case ratio >= c.EmergencyThreshold: return TierEmergency
    case ratio >= c.FullThreshold:      return TierFull
    case ratio >= c.SoftThreshold:      return TierSoft
    case ratio >= c.WarnThreshold:      return TierWarn
    default:                            return TierNone
    }
}
```

**token 估算**：使用 `EstimateTokens(msgs)` = 字符总数 ÷ 4（业界标准近似值，误差 ±10%）。实际 token 用量在 LLM 调用后从 API 响应 `usage` 字段提取，用于 TUI 展示校正。

**阈值默认值与可配置性**：

| 阈值 | 默认值 | 含义 |
|------|--------|------|
| `WarnThreshold` | 0.60 | 开始 offload 大结果 |
| `SoftThreshold` | 0.70 | 开始 LLM 摘要 |
| `FullThreshold` | 0.80 | 全量摘要 + anchor |
| `EmergencyThreshold` | 0.95 | 紧急截断保命 |
| `OffloadThreshold` | 4000 字符 | head 中 tool_result 超此值则 offload |
| `MinTailMessages` | 6 | 尾部强制保留条数 |

### 2.3 各 Tier 数据流

#### TierNone（< 60%）

无需压缩，原样返回 msgs。record.Tier = TierNone，不触发任何副作用。

#### TierWarn（60% - 70%）

```
输入: [system, m1, m2, ..., mN]

1. 分割: head = msgs[1 : N-MinTail], tail = msgs[N-MinTail :]
2. offload head 中大 tool_result:
   for each msg in head:
     if msg.ToolCallID != "" && len(msg.Content) > OffloadThreshold:
       写入文件, msg.Content = 占位符
3. 返回 [system, ...offloaded-head, ...tail]
4. 无 LLM 调用，无锚点提取

record: { Offloaded: [...], Summarized: 0, PreservedTail: len(tail) }
```

TierWarn 是最轻量的压缩：仅将 head 中的大 tool_result 移到文件，消息结构不变，tool_call/tool_result 配对完整保留。这是一个**预防性**操作——在真正需要摘要之前先释放空间。

#### TierSoft（70% - 80%）

```
输入: [system, m1, m2, ..., mN]

1. 分割: head = msgs[1 : N-MinTail], tail = msgs[N-MinTail :]
2. head 再分: headOldest = head[:len/2], headRecent = head[len/2:]
3. LTM 提取 (fail-open): extractor.Extract(headOldest)
4. offload headOldest 中大 tool_result
5. LLM 摘要 headOldest + 锚点提取 (增量合并)
6. 返回 [system, compactionMsg, ...headRecent, ...tail]

record: { Anchors: [...], Offloaded: [...], Summarized: len(headOldest),
          PreservedTail: len(tail) + len(headRecent), SummaryText: "..." }
```

TierSoft 只摘要 head 的前半部分，保留后半部分原文。这在压缩率和上下文连续性之间取得平衡——较新的 head 消息仍以原文形式留在 context 中。

#### TierFull（80% - 95%）

```
输入: [system, m1, m2, ..., mN]

1. 分割: head = msgs[1 : N-MinTail], tail = msgs[N-MinTail :]
2. LTM 提取 (fail-open): extractor.Extract(head)
3. offload head 中全部大 tool_result
4. LLM 摘要整个 head + 锚点提取 (增量合并)
5. 返回 [system, compactionMsg, ...tail]

record: { Anchors: [...], Offloaded: [...], Summarized: len(head),
          PreservedTail: len(tail), SummaryText: "..." }
```

TierFull 是主要的压缩档位：整个 head 被摘要为一条 `[Context Compaction]` 消息，仅保留尾部 MinTailMessages 条最近消息。

#### TierEmergency（≥ 95%）

```
跳过 LLM 摘要（上下文已接近硬上限，无法承受摘要调用的输入）
直接委托 Fallback.CompactForce(msgs) [TokenBudgetCompactor]

record: { Error: "emergency fallback: forced truncation", PreservedTail: N }
```

紧急档位是最后防线：不调用 LLM，直接截断到最小尾部。record 中标记 Error 字段供排查。

---

## 3. Tool-Call Offload 设计

### 3.1 问题

Agent 执行长任务时，工具调用（如 `bash` 执行 `ls -la /usr`）可能产生大量输出。这些 tool_result 消息进入 `contextHistory` 后持续占用 token。传统方案在压缩时直接将 head 中的 tool_result 交给 LLM 摘要——但 LLM 摘要会丢失原始数据，Agent 后续无法检索完整结果。

### 3.2 方案：压缩时 Offload

ProgressiveCompactor 在摘要 head 之前，先扫描其中的 tool_result 消息。超过 `OffloadThreshold`（默认 4000 字符）的，通过 `CompactionOffloader` 写入文件系统，在 context 中替换为带预览的占位符。

```
原始 head 消息:
  [user, tool_call_id=tc_001]: "total 128\ndrwxr-xr-x ...(5000 行)..."

offload 后:
  [user, tool_call_id=tc_001]: "[offloaded: .harness9/tool_results/sess1/tc_001.txt | 5000 行 / 128KB]
                                预览（前 10 行）：
                                total 128
                                drwxr-xr-x  8 user  staff   256 ...
                                ...（完整结果已保存至文件，可通过 read_file 检索）"
```

### 3.3 CompactionOffloader 实现

```go
type CompactionOffloader struct {
    workDir      string
    sessionID    string
    threshold    int           // 默认 4000
    previewLines int           // 默认 10
    cache        map[string]bool // 已 offload 的 ToolCallID
}
```

**文件路径**：`{workDir}/.harness9/tool_results/{sessionID}/{toolCallID}.txt`（与现有 OffloadHook 目录结构统一）

**幂等性缓存**：`cache map[string]bool` 记录已 offload 的 ToolCallID。首次 offload 写文件并记入 cache；后续 turn 遇到同一 ID 跳过写文件直接返回占位符。这避免了每轮压缩重复写同一文件的浪费。

**跳过条件**（返回 error 表示不 offload）：
- `ToolCallID` 为空（无法生成稳定文件名）
- 内容长度 ≤ threshold（不够大，不值得 offload）
- 内容以 `[输出已保存至` 开头（已被 OffloadHook 在工具执行时 offload 过）

**fail-open**：写入失败时返回 error，调用方保留原文不 offload，不影响整体压缩流程。

### 3.4 与 OffloadHook 的协作

harness9 有两个独立的 offload 机制：

| 机制 | 触发时机 | 阈值 | 职责 |
|------|---------|------|------|
| **OffloadHook** | 工具执行后、消息进入 contextHistory 前 | 10000 字符 | 防止超大输出进入历史 |
| **CompactionOffloader** | 压缩时、head 被摘要前 | 4000 字符 | 压缩时释放 head 中的大结果 |

两者互补：OffloadHook 在源头拦截超大输出，CompactionOffloader 在压缩时对已进入历史的中等大小结果（4000-10000 字符）进行 retroactive offload。压缩阈值更低是因为压缩时更积极地省空间。

---

## 4. Anchors 锚点设计

### 4.1 问题

传统 LLM 摘要将对话压缩为自由文本，关键信息的保留完全依赖 LLM 的判断。无法程序化校验"用户意图是否被保留"、"关键决策是否被记录"。在多轮压缩后，信息叠加丢失不可避免。

### 4.2 方案：结构化 Anchor + 散文摘要

ProgressiveCompactor 的 LLM 调用同时产出两种输出：
1. **结构化 Anchors**：五类锚点，程序化可校验，保证关键信息不丢失
2. **散文 Summary**：补充上下文，anchors 未覆盖的细节

### 4.3 五类锚点

```go
type AnchorType string

const (
    AnchorUserIntent        AnchorType = "user_intent"         // 用户真实意图
    AnchorExecutionProgress AnchorType = "execution_progress"  // 当前执行进度
    AnchorKeyDecision       AnchorType = "key_decision"        // 已做出的关键决策
    AnchorTriedSolution     AnchorType = "tried_solution"      // 已尝试的解决方案
    AnchorNextStep          AnchorType = "next_step"           // 下一步需执行的工作
)
```

每类锚点对应 Agent 执行过程中最关键的信息维度：
- **UserIntent**：防止压缩后 Agent "忘记"用户最初想做什么
- **ExecutionProgress**：已完成的里程碑，避免重复执行
- **KeyDecision**：架构/技术选型等决策及其理由
- **TriedSolution**：已尝试但失败的方案及原因，避免重蹈覆辙
- **NextStep**：待执行的后续工作，保证任务连续性

### 4.4 LLM Prompt 设计

首次压缩使用 `compactionFirstTemplate`：

```
Produce a structured compaction of the following conversation in this exact format:

## Anchors

### User Intent
<one concise sentence>

### Execution Progress
- <key milestone>

### Key Decisions
- <decision: rationale>

### Tried Solutions
- <approach: outcome>

### Next Steps
- <pending task>

## Summary
<supplementary context not captured in anchors>

Rules:
- Each anchor section MUST be present (use "- N/A" if nothing applies)
- Be concise: each item one line
- [offloaded: ...] entries indicate large outputs saved to files

Conversation:
{conversation text}
```

增量更新使用 `compactionIncrementalTemplate`：

```
Update the existing compaction by merging in new conversation content.

<previous-compaction>
{previous anchors + summary}
</previous-compaction>

New conversation to merge:
{new conversation text}
```

### 4.5 解析与容错

`ParseAnchorsAndSummary` 从 LLM 输出中提取结构化数据：

1. 按 `### ` header 分割，匹配 `anchorHeaderMap` 映射到 AnchorType
2. 每个 section 的内容（去掉 header 行）作为 Content
3. **缺失的 AnchorType 填充 `Content="N/A"`**，保证返回的 `[]Anchor` 始终包含全部五类
4. `## Summary` 之后的内容提取为 SummaryText，未找到则返回空字符串

这保证了即使 LLM 输出不完整或格式异常，程序化校验时结构始终完整。

### 4.6 增量合并

`MergeAnchors(old, new []Anchor) []Anchor` 在增量更新时合并新旧锚点：

- 新 anchors 中非 "N/A" 的值覆盖旧的同类型值
- 旧 anchors 中未被新 anchors 覆盖的非 "N/A" 值保留（取并集）
- 始终返回按 `allAnchorTypes` 顺序排列、长度为 5 的锚点切片

**为什么需要并集**：LLM 在增量更新时可能遗漏旧的锚点（如上次记录的 KeyDecision 在新输出中未提及）。并集合并保证旧的关键信息不被丢弃。

### 4.7 跨轮增量更新机制

ProgressiveCompactor 通过**内部状态**而非 contextHistory 中的 marker 实现增量更新：

```go
type ProgressiveCompactor struct {
    // ...
    lastSummary string    // 上次压缩的摘要文本
    lastAnchors []Anchor  // 上次压缩的锚点列表
}
```

**为什么不用 contextHistory 中的 marker 检测**：contextHistory 是非破坏性的（完整历史持久化到 DB），compactionMsg 仅存在于 compactedHistory（压缩视图）中。下一轮 head 从 contextHistory 派生，不含上次 compactionMsg。因此无法通过 marker 检测实现增量更新，改用 Compactor 内部状态追踪。

压缩成功后调用 `updateLastState(summary, anchors)` 更新内部状态。下次压缩时 `summarizeAndExtract` 检测 `lastSummary` 非空则走增量模板。

### 4.8 压缩消息格式

压缩后注入 context 的消息格式：

```
[Context Compaction]
## Anchors

### User Intent
Build a REST API server with chi router

### Execution Progress
- Set up project structure
- Implemented routing

### Key Decisions
- Using chi router for performance

### Tried Solutions
- Tried gin: too heavy

### Next Steps
- Add authentication
- Write tests

## Summary
The project uses Go 1.25 with chi router. Main file is cmd/server/main.go.

## Offloaded References
- .harness9/tool_results/sess1/tc_001.txt (5000行) - offloaded tool result

## Active Tasks
[>] Set up middleware
[ ] Add health endpoint
```

---

## 5. 可观测性实现

### 5.1 CompactionRecord

每次压缩生成完整的 `CompactionRecord`，记录保留/压缩/offload 的细粒度信息：

```go
type CompactionRecord struct {
    ID               string         `json:"id"`               // UUID
    SessionID        string         `json:"session_id"`       // 会话 ID
    Timestamp        time.Time      `json:"timestamp"`        // 压缩时间
    Tier             CompactionTier `json:"tier"`             // 触发档位

    TokensBefore     int            `json:"tokens_before"`    // 压缩前 token 数
    TokensAfter      int            `json:"tokens_after"`     // 压缩后 token 数
    MsgsBefore       int            `json:"msgs_before"`      // 压缩前消息数
    MsgsAfter        int            `json:"msgs_after"`       // 压缩后消息数

    Anchors          []Anchor       `json:"anchors"`          // 保留的锚点
    Offloaded        []OffloadEntry `json:"offloaded"`        // offload 的消息
    Summarized       int            `json:"summarized"`       // 被摘要的消息条数
    PreservedTail    int            `json:"preserved_tail"`   // 原样保留的尾部条数
    SummaryText      string         `json:"summary_text"`     // LLM 摘要正文

    CompressionRatio float64        `json:"compression_ratio"` // TokensAfter / TokensBefore
    Duration         time.Duration  `json:"duration"`          // 压缩耗时
    Error            string         `json:"error,omitempty"`   // 非空表示有问题
}
```

### 5.2 RecordStore 持久化

```go
type RecordStore interface {
    Append(record CompactionRecord) error
    List(sessionID string) ([]CompactionRecord, error)
}
```

`FileRecordStore` 将记录以 **JSONL**（JSON Lines）格式追加写入文件：

- 路径：`~/.harness9/compaction_records/{sessionID}.jsonl`
- 每行一条 JSON 记录，追加写入（append-only）
- `List` 按写入顺序读取，支持 1MB 行缓冲

**fail-open**：持久化失败仅 log warning，不影响压缩结果返回。

### 5.3 事件系统

`EventCompaction` 事件携带完整的 `CompactionRecord`：

```go
// engine/stream.go
compaction: func(record memory.CompactionRecord) {
    sendEvent(ctx, ch, Event{Type: EventCompaction, Data: record})
},
```

引擎通过 `applyCompactionWith` 的类型断言检测 `RecordedCompactor`：

```go
func (e *AgentEngine) applyCompactionWith(comp memory.Compactor, msgs []schema.Message) ([]schema.Message, *memory.CompactionRecord) {
    if comp == nil {
        return msgs, nil
    }
    if rc, ok := comp.(memory.RecordedCompactor); ok {
        result, record := rc.CompactWithRecord(msgs)
        return result, &record
    }
    return comp.Compact(msgs), nil
}
```

### 5.4 TUI 展示

TUI 收到 `EventCompaction` 后展示双行富信息通知，Tier 用文字标签区分：

```
TierFull:
⚡ 上下文压缩 [Full] 45.2K→8.1K tokens（82% 压缩率）
   5 锚点 | 3 offload(42KB) | 28 条摘要 | 保留尾部 6 条

TierWarn:
⚡ 上下文压缩 [Warn] 76.8K→72.3K tokens（6% 压缩率）
   2 offload(18KB) | 无摘要

TierEmergency:
⚡ 上下文压缩 [Emergency] 98.5K→12.0K tokens（88% 压缩率）
   ⚠ 紧急截断回退 | 保留尾部 1 条
```

### 5.5 级联 GC

`Manager.DeleteSession` 删除会话时级联清理：
- `tool_results/{sessionID}/` 目录（已有）
- `compaction_records/{sessionID}.jsonl` 文件（新增）

通过 `WithCompactionRecordsDir` ManagerOption 配置。

---

## 6. 错误处理与回退

### 6.1 分层 fail-open 策略

每个环节独立失败，不中断整体压缩：

| 环节 | 失败行为 | 记录 |
|------|---------|------|
| **Offload 写文件** | 保留原文不 offload，继续摘要 | `record.Error` 追加 |
| **LTM 提取** | 静默跳过（已有 fail-open） | 无 |
| **LLM 摘要调用** | 回退到 `Fallback.Compact(msgs)` | `record.Error` 记录原因 |
| **Anchor 解析** | 提取已识别锚点，缺失类型填 "N/A" | `record.Error` 追加 |
| **RecordStore 持久化** | 静默跳过，仅 log warning | 不影响 record 返回 |

### 6.2 LLM 失败回退链

```
TierSoft/TierFull LLM 调用失败
  → fallbackCompact(msgs, reason)
    → c.fallback().Compact(msgs)  [TokenBudgetCompactor]
      → 截断至 MaxTokens 预算内
      → repairOrphanedToolPairs 修复
  → record.Error = "LLM summary failed in TierXxx"
```

### 6.3 Emergency 回退

TierEmergency 不调用 LLM，直接委托 `Fallback.CompactForce(msgs)`（TokenBudgetCompactor 的强制截断模式），record 标记 `Error="emergency fallback: forced truncation"`。

---

## 7. 接口设计

### 7.1 三接口实现

ProgressiveCompactor 同时实现三个接口：

```go
// 基础接口（所有 Compactor 必须实现）
type Compactor interface {
    Compact(msgs []schema.Message) []schema.Message
}

// 强制压缩（手动 /compact 命令）
type ForceCompactor interface {
    Compactor
    CompactForce(msgs []schema.Message) []schema.Message
}

// 带记录的压缩（引擎类型断言检测）
type RecordedCompactor interface {
    Compactor
    CompactWithRecord(msgs []schema.Message) ([]schema.Message, CompactionRecord)
}
```

- `Compact()` → 调用 `CompactWithRecord()` 并丢弃 record（向后兼容）
- `CompactForce()` → 直接执行 TierEmergency（手动 /compact 命令）
- `CompactWithRecord()` → 主入口，检测 tier 并委托对应方法

### 7.2 构造选项

```go
func NewProgressiveCompactor(p Summarizer, contextWindow int, opts ...ProgressiveOption) *ProgressiveCompactor

// 可注入的协作组件
WithProgressiveTodoInjector(ti TodoInjector)        // 活跃任务注入
WithProgressiveMemoryExtractor(ex MemoryExtractor)  // LTM 提取
WithProgressiveRecordStore(rs RecordStore)          // 压缩记录持久化
WithProgressiveOffloader(o *CompactionOffloader)    // 工具结果 offload
WithProgressiveSessionID(id string)                 // 会话 ID
```

---

## 8. 与引擎的集成

### 8.1 runLoop 中的压缩时序

```
每个 Turn:
  1. availableTools = registry.GetAvailableTools()
  2. toolTokens = EstimateToolTokens(availableTools)
  3. msgTokensBefore = EstimateTokens(contextHistory)
  4. compactedHistory, record = applyCompactionWith(comp, contextHistory)
  5. msgTokensAfter = EstimateTokens(compactedHistory)
  6. if record != nil && record.Tier != TierNone:
       em.compaction(*record)  → 发出 EventCompaction
  7. em.tokenUpdate(totalTokens, contextWindow)  → TUI 展示
  8. responseMsg = em.generate(compactedHistory, availableTools)  → LLM 调用
  9. if usage != nil: em.tokenUpdate(usage.InputTokens)  → 实际值校正
  10. contextHistory = append(contextHistory, responseMsg)  → 完整历史
```

### 8.2 非破坏性压缩设计

- `contextHistory`：完整历史，持续追加，持久化到 DB
- `compactedHistory`：每轮从 contextHistory 派生的压缩视图，只传给 LLM
- `saveHistoryWith` 保存 contextHistory（非压缩版），确保历史不丢失
- offload 修改的是 compactedHistory 中的消息副本，不影响 contextHistory
- offloader cache 保证不重复写同一文件

### 8.3 手动 /compact 命令

`engine.Compact()` 方法支持手动触发强制压缩：

```go
func (e *AgentEngine) Compact(ctx context.Context) (memory.CompactionRecord, error) {
    // 加载历史 → 注入 system prompt → 调用 CompactForce → 剥离 system → 写回 session
    if rc, ok := comp.(memory.RecordedCompactor); ok {
        compactedWithSystem, record = rc.CompactWithRecord(withSystem)
    } else if fc, ok := comp.(memory.ForceCompactor); ok {
        compactedWithSystem = fc.CompactForce(withSystem)
    } else {
        compactedWithSystem = comp.Compact(withSystem)
    }
    // 清空 session → 写回压缩后历史 → 返回 record
}
```

---

## 9. 涉及文件

| 文件 | 职责 |
|------|------|
| `internal/memory/progressive_compactor.go` | ProgressiveCompactor 主实现 |
| `internal/memory/anchor.go` | Anchor 类型 + ParseAnchorsAndSummary + MergeAnchors |
| `internal/memory/compaction_offloader.go` | CompactionOffloader + OffloadEntry |
| `internal/memory/record_store.go` | CompactionRecord + CompactionTier + RecordStore + FileRecordStore |
| `internal/memory/compaction.go` | RecordedCompactor 接口 + repairOrphanedToolPairs |
| `internal/engine/history.go` | applyCompactionWith 类型断言（压缩适配） |
| `internal/engine/loop_phases.go` | prepareTurnInput 中的 runLoop 压缩集成 |
| `internal/engine/stream.go` | EventCompaction 携带 CompactionRecord |
| `internal/engine/compact.go` | 手动 /compact 适配 RecordedCompactor |
| `internal/memory/manager.go` | WithCompactionRecordsDir + 级联 GC |
| `cmd/harness9/main.go` | 默认使用 ProgressiveCompactor + 初始化 RecordStore/Offloader |
| `cmd/harness9/tui_update.go` | 双行压缩通知展示 |

---

## 10. 设计决策总结

| 决策 | 原因 |
|------|------|
| **四层渐进阈值** | 避免"要么不压要么全压"的突变，60% 开始预防性 offload |
| **内部状态增量更新** | contextHistory 非破坏性，compactionMsg 不进入 contextHistory，无法用 marker 检测 |
| **Anchor + 散文双层** | Anchor 是程序化可校验的保底信息，散文是补充上下文 |
| **缺失 Anchor 填 N/A** | 保证 []Anchor 始终 5 条，程序化校验结构完整 |
| **增量合并取并集** | 防止 LLM 增量更新时遗漏旧锚点 |
| **offload 阈值 4000 < OffloadHook 10000** | 压缩时更积极省空间，retroactive offload 中等大小结果 |
| **offloader cache 幂等** | 避免每轮压缩重复写同一文件 |
| **TierEmergency 跳过 LLM** | 95% 时上下文接近硬上限，无法承受摘要调用的输入 |
| **CompactionRecord JSONL 持久化** | 追加写入高效，每行一条记录易解析，fail-open 不影响压缩 |
| **三接口实现** | 向后兼容 Compact/ForceCompactor，新增 RecordedCompactor 供引擎类型断言 |
