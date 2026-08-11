---
title: "Context Compaction Is Not Deleting History: How harness9 Keeps an Agent on Track"
date: 2026-08-11
tags: [harness9, agent, golang, context, compaction, memory]
summary: "ProgressiveCompactor gives harness9 four escalating context-compaction tiers: offload first, then summarize part of the history, then summarize the whole head, and only then truncate. Structured Anchors, two-stage tool-result offloading, tool-pair repair, and JSONL records preserve continuity in long-running work."
---

# Context Compaction Is Not Deleting History: How harness9 Keeps an Agent on Track

## About harness9

harness9 is a Local-First, lightweight, feature-complete, production-ready general-purpose Go Agent framework.

- **Website**: [https://zhangshenao.github.io/harness9/](https://zhangshenao.github.io/harness9/)
- **GitHub**: [https://github.com/ZhangShenao/harness9](https://github.com/ZhangShenao/harness9)

Starring the repo is the most direct way to support this open-source work — issues and PRs are very welcome.

---

## TL;DR

- `ProgressiveCompactor` does not wait until the context window is nearly full. It steps up at 60%, 70%, 80%, and 95% utilization.
- At 60%, it only offloads old large `tool_result` messages. This costs no LLM call and preserves the conversation structure.
- Soft compaction summarizes only the oldest half of the compactable history; full compaction summarizes the whole head and preserves the recent tail.
- Five structured Anchors keep the user intent, progress, decisions, failed attempts, and next steps from drifting across repeated summaries.
- `OffloadHook` and `CompactionOffloader` catch large tool output at two different moments in the lifecycle.
- When the context is critical or a summary fails, `TokenBudgetCompactor` keeps the Agent moving; each meaningful compaction can still be audited in JSONL.

## What you will learn

- Why compaction runs before every LLM request, rather than after history has already been stored.
- How four tiers replace a sudden “do nothing, then delete everything” transition with gradual pressure relief.
- How two-stage tool-result offloading retains raw evidence without keeping it in the model context.
- Why Anchors merge incrementally and refuse to overwrite useful context with `N/A`.
- How tool-call pairing, fallback behavior, and audit records keep a long-running task coherent.

## Where does compaction happen?

`contextHistory` is the Agent's complete working record. `compactedHistory` is only the temporary view sent to the model for the current turn. That separation matters: compaction does not overwrite the SQLite session history with a summary. The next turn can derive a new compacted view from the complete history again.

Inside `runLoop`, the engine obtains the available tool schemas, runs `applyCompactionWith`, and only then sends the compacted messages and tools to the LLM. Compaction is therefore a preflight gate for reasoning, not an after-the-fact cleanup job.

```go
// internal/engine/agent_loop.go
compactedHistory, compactionRecord := e.applyCompactionWith(comp, contextHistory)
msgTokensAfter := memory.EstimateTokens(compactedHistory)
totalTokens := msgTokensAfter + toolTokens

if compactionRecord != nil && compactionRecord.Tier != memory.TierNone {
    em.compaction(*compactionRecord)
}
em.tokenUpdate(totalTokens, e.contextWindow)
msg, usage, err := e.generateWithRetry(ctx, em, turnCount,
    compactedHistory, availableTools)
```

`EstimateTokens` uses character count divided by four as a fast estimate. It does not pretend to be exact, but it is inexpensive enough to run before every turn. Tool schemas have their own token cost too, which is why it is not safe to compact the message history right up to the window boundary.

![Compaction before the LLM request](/blog/progressive-context-compaction/images/preflight-flow-02.png)

## When does it compact?

`ProgressiveCompactor` turns context-window utilization into five explicit outcomes. `TierNone` is not an omission; it is the deliberate choice to leave the history untouched. Every higher tier does a little more than the last.

| Tier | Utilization | Action | LLM summary |
|---|---:|---|---|
| `TierNone` | < 60% | Pass through unchanged | No |
| `TierWarn` | 60%–70% | Offload only old large tool results | No |
| `TierSoft` | 70%–80% | Summarize the oldest half, retain newer head and tail | Yes |
| `TierFull` | 80%–95% | Summarize the complete head, retain the recent tail | Yes |
| `TierEmergency` | >= 95% | Force truncation directly | No |

The decision function contains no hidden magic. It divides estimated tokens by the model's `ContextWindow` and matches from the most urgent tier downward:

```go
// internal/memory/progressive_compactor.go
func (c *ProgressiveCompactor) determineTier(msgs []schema.Message) CompactionTier {
    if c.ContextWindow <= 0 {
        return TierNone
    }
    ratio := float64(EstimateTokens(msgs)) / float64(c.ContextWindow)
    switch {
    case ratio >= c.EmergencyThreshold:
        return TierEmergency
    case ratio >= c.FullThreshold:
        return TierFull
    case ratio >= c.SoftThreshold:
        return TierSoft
    case ratio >= c.WarnThreshold:
        return TierWarn
    default:
        return TierNone
    }
}
```

The defaults are `0.60 / 0.70 / 0.80 / 0.95`, with the most recent six messages retained by default. The trade-off is intentionally plain: early tiers avoid touching semantics, while later tiers increasingly prioritize keeping the task alive. An Agent should not experience no change at 79% and sudden amnesia at 80%.

![Five ProgressiveCompactor waterlines](/blog/progressive-context-compaction/images/tier-ladder-01.png)

## How can large results stay available without staying in context?

In a long task, the biggest danger is often not a conversation message but a single `bash` output containing tens of thousands of characters. harness9 does not reduce that to an opaque ellipsis. It stores the original output in a local file and leaves a searchable reference with a preview in the context.

There are two gates:

- `OffloadHook` handles tool output over 10,000 characters immediately after execution, before it can bloat the history.
- `CompactionOffloader` handles medium-sized output already present in an old head when compaction begins, at a 4,000-byte threshold. This fills the gap that the first gate intentionally leaves open.

The file path is isolated by session and tool-call ID: `.harness9/tool_results/<sessionID>/<toolCallID>.txt`. Files use `0600` permissions. A cache keyed by `ToolCallID` means a later compaction rebuilds the placeholder instead of repeatedly overwriting the stored result.

The compactor replaces only recognizable, large `tool_result` messages:

```go
// internal/memory/progressive_compactor.go
for i, msg := range head {
    if msg.ToolCallID == "" || len(msg.Content) <= c.OffloadThreshold {
        continue
    }
    entry, placeholder, err := c.offloader.OffloadToolResult(msg)
    if err != nil {
        continue
    }
    head[i].Content = placeholder
    entries = append(entries, entry)
}
```

On failure it simply continues and keeps the original message. This is fail-open: an Agent should not stop working merely because an attempt to save tokens could not write a file. If the model later needs the details, it can retrieve them through paginated `read_file` calls.

![Two-stage tool-result offloading](/blog/progressive-context-compaction/images/two-stage-offload-03.png)

## Why add Anchors?

A free-form summary can look as though it remembered the task while still losing the task's skeleton. `ProgressiveCompactor` therefore asks the summary LLM to produce five structured Anchors alongside its prose summary: User Intent, Execution Progress, Key Decisions, Tried Solutions, and Next Steps.

`ParseAnchorsAndSummary` always returns all five. Missing values become `N/A`. At the next compaction, `MergeAnchors` accepts new values but does not allow `N/A` to overwrite an older value that was actually known:

```go
// internal/memory/anchor.go
for _, a := range new {
    if a.Content != "N/A" {
        m[a.Type] = a.Content
    }
}
for _, at := range allAnchorTypes {
    content := m[at]
    if content == "" {
        content = "N/A"
    }
    result = append(result, Anchor{Type: at, Content: content})
}
```

This is not an attempt to make a summary look more like a report. It leaves five durable pins in a long task. The model may forget wording halfway through a project, but it should not casually lose what the user asked for or which dead end has already been tried. The prose summary carries detail; Anchors keep the skeleton stable.

![Incremental Anchor merging](/blog/progressive-context-compaction/images/anchor-merge-04.png)

## Tool-call pairs must not break

Compacted messages also face a less obvious API constraint. The Anthropic Messages API requires `tool_call` and `tool_result` messages to remain paired. Trimming old history can leave a result without its call, or a call without its result. Either case is more than “a little less context”: it can turn into a 400 response.

`repairOrphanedToolPairs` repairs both directions after every compaction result: it removes a result without a matching call, and inserts a placeholder result when a call has no result. Every `ProgressiveCompactor` tier that changes the message list passes through this repair.

![Bidirectional repair for tool-call pairs](/blog/progressive-context-compaction/images/tool-pair-repair-05.png)

## Can the Agent still proceed when compaction fails?

Yes. `TierSoft` and `TierFull` give their summary request a dedicated 60-second timeout. If the summarizer fails, the engine falls back to `TokenBudgetCompactor`. `TierEmergency` is more direct: it does not call an LLM at all and uses `CompactForce` to truncate immediately.

That path gives up some historical nuance in exchange for a more important property: an Agent does not stall because the service intended to compress its context is unavailable. `CompactionRecord.Error` preserves the reason for a fallback. Whenever the tier is not `TierNone`, the record is eligible to be appended to a session-scoped JSONL file; the TUI also receives `EventCompaction` with before/after token counts, the selected tier, offload count, and preserved-tail count.

One implementation detail is worth stating precisely. Although the comment for `/compact` describes it as “forced compaction,” `AgentEngine.Compact` first detects `RecordedCompactor`, which `ProgressiveCompactor` implements. In the current implementation, `/compact` calls `CompactWithRecord` and still chooses a tier from the current utilization; it does not unconditionally enter emergency truncation. That boundary only becomes clear when reading the code rather than the comment alone.

![The compaction audit trail](/blog/progressive-context-compaction/images/compaction-audit-06.png)

## Closing thought

harness9's approach is not “throw away the old things.” At every waterline, it makes a more careful trade: first preserve a way to recover the raw evidence, then preserve the task skeleton, and only at the end sacrifice history to protect the next step.

## Cover

![Context compaction cover](/blog/progressive-context-compaction/images/cover.png)
