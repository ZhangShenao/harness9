# ProgressiveCompactor: Tiered Progressive Context Compression

## 1. Overview

ProgressiveCompactor is the default context compression strategy for harness9, addressing the core challenge of context window bloat during long-running Agent tasks. Unlike traditional "single-threshold full truncation," it employs a **tiered progressive** architecture: based on token usage ratio, it divides compression into four tiers (Warn / Soft / Full / Emergency), each executing different intensity actions—from lightweight offload to LLM summarization to emergency truncation—ensuring smooth transitions rather than abrupt changes.

### 1.1 Design Goals

| Goal | Mechanism |
|------|-----------|
| **Progressive triggering** | Four-tier thresholds (60% / 70% / 80% / 95%), avoiding "all-or-nothing" compression |
| **No information loss** | Large tool_results offloaded to files + structured Anchor points as guarantee |
| **Critical info preservation** | Five anchor types (intent/progress/decisions/attempts/next-steps), programmatically verifiable |
| **Full-chain traceability** | CompactionRecord logs every compression detail, persisted to JSONL |
| **Cross-turn incremental update** | Internal state tracks last summary, incremental merge prevents information stacking loss |
| **Backward compatibility** | Implements Compactor + ForceCompactor + RecordedCompactor interfaces |

### 1.2 Core Architecture

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
       Return    offload   offload  offload   Force trunc
                only      +summarize +summarize (Fallback)
                head      ½ head    full head
                          +anchors  +anchors
```

---

## 2. Tiered Compression Mechanism

### 2.1 Four Tier Definitions

```go
type CompactionTier int

const (
    TierNone      CompactionTier = iota // <60%: no compression needed
    TierWarn                            // 60-70%: offload large results only
    TierSoft                            // 70-80%: offload + summarize oldest 1/2 head
    TierFull                            // 80-95%: offload + summarize full head + anchors
    TierEmergency                       // ≥95%: forced truncation fallback
)
```

### 2.2 Trigger Timing

Each Turn, before the LLM call, `CompactWithRecord` is invoked. It first determines the appropriate tier via `determineTier`:

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

**Token estimation**: Uses `EstimateTokens(msgs)` = total characters ÷ 4 (industry-standard approximation, ±10% error). Actual token usage is extracted from the API response `usage` field after LLM calls, used for TUI display correction.

**Default thresholds and configurability**:

| Threshold | Default | Meaning |
|-----------|---------|---------|
| `WarnThreshold` | 0.60 | Start offloading large results |
| `SoftThreshold` | 0.70 | Start LLM summarization |
| `FullThreshold` | 0.80 | Full summarization + anchors |
| `EmergencyThreshold` | 0.95 | Emergency truncation |
| `OffloadThreshold` | 4000 chars | tool_results in head exceeding this are offloaded |
| `MinTailMessages` | 6 | Minimum tail messages to always preserve |

### 2.3 Tier Data Flows

#### TierNone (< 60%)

No compression needed; return msgs as-is. record.Tier = TierNone, no side effects.

#### TierWarn (60% - 70%)

```
Input: [system, m1, m2, ..., mN]

1. Split: head = msgs[1 : N-MinTail], tail = msgs[N-MinTail :]
2. Offload large tool_results in head:
   for each msg in head:
     if msg.ToolCallID != "" && len(msg.Content) > OffloadThreshold:
       write to file, msg.Content = placeholder
3. Return [system, ...offloaded-head, ...tail]
4. No LLM call, no anchor extraction

record: { Offloaded: [...], Summarized: 0, PreservedTail: len(tail) }
```

TierWarn is the lightest compression: only moves large tool_results in head to files, message structure unchanged, tool_call/tool_result pairs fully preserved. This is a **preventive** operation—releasing space before summarization is actually needed.

#### TierSoft (70% - 80%)

```
Input: [system, m1, m2, ..., mN]

1. Split: head = msgs[1 : N-MinTail], tail = msgs[N-MinTail :]
2. Split head: headOldest = head[:len/2], headRecent = head[len/2:]
3. LTM extraction (fail-open): extractor.Extract(headOldest)
4. Offload large tool_results in headOldest
5. LLM summarize headOldest + anchor extraction (incremental merge)
6. Return [system, compactionMsg, ...headRecent, ...tail]

record: { Anchors: [...], Offloaded: [...], Summarized: len(headOldest),
          PreservedTail: len(tail) + len(headRecent), SummaryText: "..." }
```

TierSoft only summarizes the first half of head, keeping the second half as original text. This balances compression ratio and context continuity—newer head messages remain in context as original text.

#### TierFull (80% - 95%)

```
Input: [system, m1, m2, ..., mN]

1. Split: head = msgs[1 : N-MinTail], tail = msgs[N-MinTail :]
2. LTM extraction (fail-open): extractor.Extract(head)
3. Offload all large tool_results in head
4. LLM summarize entire head + anchor extraction (incremental merge)
5. Return [system, compactionMsg, ...tail]

record: { Anchors: [...], Offloaded: [...], Summarized: len(head),
          PreservedTail: len(tail), SummaryText: "..." }
```

TierFull is the primary compression tier: the entire head is summarized into a single `[Context Compaction]` message, preserving only the tail's MinTailMessages most recent messages.

#### TierEmergency (≥ 95%)

```
Skip LLM summarization (context is near hard limit, cannot afford summarization input)
Delegate directly to Fallback.CompactForce(msgs) [TokenBudgetCompactor]

record: { Error: "emergency fallback: forced truncation", PreservedTail: N }
```

Emergency tier is the last line of defense: no LLM call, direct truncation to minimal tail. The Error field is set in the record for troubleshooting.

---

## 3. Tool-Call Offload Design

### 3.1 Problem

During long-running tasks, tool calls may produce large outputs. These tool_result messages enter `contextHistory` and continuously consume tokens. Traditional approaches hand head's tool_results to LLM summarization-but LLM summarization loses original data, and the Agent cannot retrieve complete results afterward.

### 3.2 Solution: Compaction-Time Offload

ProgressiveCompactor scans head's tool_result messages before summarization. Those exceeding `OffloadThreshold` (default 4000 chars) are written to the filesystem via `CompactionOffloader`, replaced in context with a preview placeholder.

### 3.3 CompactionOffloader Implementation

```go
type CompactionOffloader struct {
    workDir      string
    sessionID    string
    threshold    int           // default 4000
    previewLines int           // default 10
    cache        map[string]bool // offloaded ToolCallIDs
}
```

**File path**: `{workDir}/.harness9/tool_results/{sessionID}/{toolCallID}.txt` (unified with OffloadHook directory structure)

**Idempotency cache**: records offloaded ToolCallIDs. First offload writes file and caches; subsequent turns skip file writing, return placeholder directly.

**Skip conditions**: empty ToolCallID, content below threshold, or already offloaded by OffloadHook.

**fail-open**: write failure returns error; caller retains original text, not affecting overall compression.

### 3.4 Coordination with OffloadHook

| Mechanism | Trigger Timing | Threshold | Responsibility |
|-----------|---------------|-----------|----------------|
| **OffloadHook** | After tool execution, before entering contextHistory | 10000 chars | Prevent oversized outputs from entering history |
| **CompactionOffloader** | During compression, before head summarization | 4000 chars | Release large results in head during compression |

OffloadHook intercepts at source; CompactionOffloader performs retroactive offload on medium-sized results (4000-10000 chars) already in history.

---

## 4. Anchors Design

### 4.1 Problem

Traditional LLM summarization compresses conversation into free text, with critical information retention entirely dependent on LLM judgment. No programmatic verification of "whether user intent was preserved" or "whether key decisions were recorded."

### 4.2 Solution: Structured Anchors + Prose Summary

ProgressiveCompactor's LLM call produces two outputs:
1. **Structured Anchors**: Five types, programmatically verifiable
2. **Prose Summary**: Supplementary context

### 4.3 Five Anchor Types

```go
type AnchorType string

const (
    AnchorUserIntent        AnchorType = "user_intent"
    AnchorExecutionProgress AnchorType = "execution_progress"
    AnchorKeyDecision       AnchorType = "key_decision"
    AnchorTriedSolution     AnchorType = "tried_solution"
    AnchorNextStep          AnchorType = "next_step"
)
```

- **UserIntent**: Prevents Agent from forgetting user's original goal
- **ExecutionProgress**: Completed milestones, avoiding redundant work
- **KeyDecision**: Architecture/technology choices and rationale
- **TriedSolution**: Failed approaches and reasons, avoiding repeated mistakes
- **NextStep**: Pending follow-up work, ensuring task continuity

### 4.4 Parsing and Fault Tolerance

`ParseAnchorsAndSummary` extracts structured data from LLM output. Missing AnchorTypes are filled with `Content="N/A"`, ensuring `[]Anchor` always contains all five types.

### 4.5 Incremental Merge

`MergeAnchors(old, new []Anchor)` preserves old anchors not overwritten by new non-"N/A" values (union). This prevents LLM from omitting old critical information during incremental updates.

### 4.6 Cross-Turn Incremental Update

ProgressiveCompactor uses **internal state** (`lastSummary`, `lastAnchors`) for incremental updates. In the era of per-turn views this was the only option — compactionMsg never entered the next turn's input. With write-back compaction (§8.2) compactionMsg does enter the history, yet internal state remains more robust: degraded-truncation turns produce no compactionMsg, marker attribution across multiple compactions gets confusing, and state tracking does not depend on parsing history text.

---

## 5. Observability Implementation

### 5.1 CompactionRecord

Each compression generates a complete `CompactionRecord` with fine-grained info: ID, SessionID, Timestamp, Tier, TokensBefore/After, MsgsBefore/After, Anchors, Offloaded, Summarized, PreservedTail, SummaryText, CompressionRatio, Duration, Error.

### 5.2 RecordStore Persistence

`FileRecordStore` persists records in JSONL format to `~/.harness9/compaction_records/{sessionID}.jsonl`. Append-only, one JSON record per line. fail-open: persistence failure logs warning, does not affect compression.

### 5.3 Event System

`EventCompaction` carries the complete `CompactionRecord`. Engine detects `RecordedCompactor` via type assertion in `applyCompactionWith`.

### 5.4 TUI Display

Two-line rich notification with tier labels:

```
TierFull:
⚡ Context compressed [Full] 45.2K->8.1K tokens (82% compression ratio)
   5 anchors | 3 offload(42KB) | 28 summarized | tail: 6 preserved
```

### 5.5 Cascade GC

`Manager.DeleteSession` cascades cleanup: `tool_results/{sessionID}/` directory (existing) + `compaction_records/{sessionID}.jsonl` file (new). Configured via `WithCompactionRecordsDir`.

---

## 6. Error Handling and Fallback

### 6.1 Layered Fail-Open Strategy

| Component | Failure Behavior | Record |
|-----------|------------------|--------|
| **Offload file write** | Retain original, continue summarization | `record.Error` appended |
| **LTM extraction** | Silent skip | None |
| **LLM summary call** | Fallback to `Fallback.Compact(msgs)` | `record.Error` records cause |
| **Anchor parsing** | Extract identified, fill missing with "N/A" | `record.Error` appended |
| **RecordStore persistence** | Silent skip, log warning | No effect on record |

### 6.2 Emergency Fallback

TierEmergency skips LLM, delegates to `Fallback.CompactForce(msgs)`, record marks `Error="emergency fallback: forced truncation"`. The truncated view **always keeps the first task message** (task anchor), then greedily includes the newest messages within the remaining budget, skipping any single message that does not fit — the issue #117 E2E run showed that an emergency view without the task anchor leaves the model amnesiac and spinning (143 consecutive Emergency turns, task failed). The Emergency truncated view is also written back and persisted (unlike transient summarization-failure fallbacks), so the next turn starts from a converged history.

---

## 7. Interface Design

ProgressiveCompactor implements three interfaces:
- `Compactor.Compact()` -> delegates to `CompactWithRecord()`, discards record (backward compat)
- `ForceCompactor.CompactForce()` -> executes TierEmergency (manual /compact)
- `RecordedCompactor.CompactWithRecord()` -> main entry, detects tier and delegates

Constructor options: `WithProgressiveTodoInjector`, `WithProgressiveMemoryExtractor`, `WithProgressiveRecordStore`, `WithProgressiveOffloader`, `WithProgressiveSessionID`.

---

## 8. Engine Integration

### 8.1 Compression Timing in runLoop

Each Turn: `applyCompactionWith` -> if record != nil, `writeBackCompaction` (on real effect: replace contextHistory with the compacted product and persist to session, rollback on failure) -> emit `EventCompaction` if tier != TierNone -> LLM call with compactedHistory -> append response to the written-back contextHistory.

### 8.2 Write-Back Design (issue #117)

- `contextHistory`: the engine-local history. When a Recorded compactor takes real effect, it is replaced by the compacted product and persisted to the session — one compaction stays effective for many turns, and tiers progress as designed.
- `compactedHistory`: the per-turn view sent to the LLM; on a real compaction it becomes the new contextHistory.
- **Write-back gating**: Recorded compactors only, no degraded `Error` in the record, and an actual reduction (fewer messages or offload happened). Non-Recorded compactors keep the pure view semantics.
- **Degraded write-back is tier-aware**: a TierEmergency truncated view (true capacity overflow — the full history can no longer be sent through the API) **is written back** and persisted; it is the only recovery path, letting the next turn start from a converged history where Soft/Full summaries work again — otherwise every turn re-triggers Emergency (a 200-turn truncation loop was observed in E2E). The forced truncation caused by transient LLM summarization failures (Soft/Full + Error) is **never persisted**; it only applies to the current turn's view so the next turn can retry a real summary.
- **Rollback on failure**: if the session write-back fails after Clear, an independent 5s context restores exactly the persisted prefix (`history[:startLen]`); the engine keeps its original history and still feeds the compacted view to the LLM this turn.
- **Defensive copy**: offload placeholders are written into a copy of head, never in-place into the caller's slice. Previously `offloadHead` permanently rewrote `contextHistory` through a shared backing array, collapsing the token ratio and preventing higher tiers from ever triggering (issue #117 alias bug; covered by a regression test).
- The offloader cache guarantees each file is written only once.

---

## 9. Files

| File | Responsibility |
|------|----------------|
| `internal/memory/progressive_compactor.go` | ProgressiveCompactor main implementation |
| `internal/memory/anchor.go` | Anchor types + ParseAnchorsAndSummary + MergeAnchors |
| `internal/memory/compaction_offloader.go` | CompactionOffloader + OffloadEntry |
| `internal/memory/record_store.go` | CompactionRecord + CompactionTier + RecordStore + FileRecordStore |
| `internal/memory/compaction.go` | RecordedCompactor interface + repairOrphanedToolPairs |
| `internal/engine/history.go` | applyCompactionWith + writeBackCompaction/persistCompacted (compaction adapter and write-back) |
| `internal/engine/loop_phases.go` | runLoop compaction integration inside prepareTurnInput |
| `internal/engine/stream.go` | EventCompaction carrying CompactionRecord |
| `internal/engine/compact.go` | Manual /compact adapts to RecordedCompactor |
| `internal/memory/manager.go` | WithCompactionRecordsDir + cascade GC |
| `cmd/harness9/main.go` | Default ProgressiveCompactor + init RecordStore/Offloader |
| `cmd/harness9/tui_update.go` | Two-line compression notification display |

---

## 10. Design Decisions Summary

| Decision | Rationale |
|----------|-----------|
| **Four-tier progressive thresholds** | Avoids abrupt "all-or-nothing", 60% starts preventive offload |
| **Write-back compaction** | The per-turn view re-ran compression every turn once over threshold (Soft/Full paid an extra LLM summary per turn, with the "incremental" template re-feeding the full head); write-back makes one compaction effective for many turns and lets tiers progress (issue #117) |
| **Internal state incremental update** | Degraded turns produce no compactionMsg and marker attribution is error-prone; internal state does not rely on parsing history text |
| **Anchor + prose dual layer** | Anchors are verifiable guarantee, prose is supplementary |
| **Missing anchors filled with N/A** | Ensures []Anchor always 5 items, structure complete |
| **Incremental merge takes union** | Prevents LLM from omitting old anchors |
| **offload threshold 4000 < OffloadHook 10000** | More aggressive space saving during compression |
| **offloader cache idempotency** | Avoids redundant file writes across turns |
| **TierEmergency skips LLM** | 95% context near hard limit, cannot afford summarization input |
| **Emergency keeps task anchor** | Dropping the first task message in emergency truncation leaves the model spinning without a goal (issue #117 E2E); anchor guarantee + budget-greedy tail keeps a minimal workable context |
| **JSONL persistence** | Append-only efficient, one record per line, fail-open |
| **Three interface implementation** | Backward compat Compact/ForceCompactor + new RecordedCompactor |
