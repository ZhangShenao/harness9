---
title: "The Agent Skills System: Extending LLM Capabilities via Progressive Disclosure"
date: 2026-06-15
tags: [harness9, agent, golang, skills, progressive-disclosure, prompt-engineering]
summary: "harness9's Agent Skills system is built around Progressive Disclosure — it packages domain expertise for the LLM into independent modules that load on demand. At startup only an index of capabilities is injected; at runtime the LLM decides for itself and pulls the full content via the use_skill tool. This post breaks down the protocol design, loading mechanism, and injection strategy, along with how this design fundamentally differs from RAG and traditional System Prompt extension."
---

# The Agent Skills System: Extending LLM Capabilities via Progressive Disclosure

## TL;DR

harness9's Agent Skills system solves a core tension: **an Agent needs a large amount of domain knowledge to handle complex tasks, but stuffing all of that knowledge into the System Prompt causes the LLM to lose focus and blows up token usage.**

The solution is **Progressive Disclosure** — splitting a Skill into two layers:

| Layer | Content | Injection timing | Token cost |
|------|------|---------|-----------|
| **Index layer** (frontmatter) | name + description, one line | Every conversation, third section of the System Prompt | Fixed, very low (~20 tokens per entry) |
| **Content layer** (body) | Full domain-knowledge body | Only when the LLM actively calls `use_skill` | On demand, stays in the ToolResult once used |

**The key decision**: recall is delegated to the LLM instead of RAG vector search. After reading the index, the LLM autonomously decides which Skill to invoke based on the semantics of the current task — which means the quality of the `description` field is the single dial controlling Skill recall accuracy.

**Execution path** (one Skill usage):
```
User input → LLM reads index → tool_call: use_skill("go-refactor")
  → GetFullContent reads from disk → Skill body returned as ToolResult
  → Next Turn, LLM executes the task with the Skill body as background knowledge
```

**Fundamental differences from mainstream approaches**:
- vs. full injection: the System Prompt always contains only the index — the body never pollutes the System Prompt
- vs. RAG retrieval: no embedding model is needed, recall is a semantic judgment rather than a similarity computation
- vs. LangChain Tool: a Skill has no side effects and doesn't execute — it's a declarative knowledge container

**Suited for**: team coding conventions, deployment SOPs, debugging guides, API usage manuals — any structured knowledge that you "check when needed and shelve otherwise."

---

## About harness9

harness9 is a Local-First, lightweight, feature-complete, production-ready general-purpose Go Agent framework.

- **Website**: [https://zhangshenao.github.io/harness9/](https://zhangshenao.github.io/harness9/)
- **GitHub**: [https://github.com/ZhangShenao/harness9](https://github.com/ZhangShenao/harness9)

A star is the most direct way to support this open-source work — issues and PRs are welcome.

---

## What you'll learn from this post

- Why harness9 chose an "index-first, full-content-on-demand" two-stage Skill design instead of embedding domain knowledge directly in the System Prompt
- What Progressive Disclosure concretely means in LLM engineering, and how it fundamentally differs from RAG retrieval in its recall strategy
- How the three components `Index`, `UseSkillTool`, and `DefaultPromptBuilder` divide labor to form a complete "discover — pull — inject" protocol chain
- How the frontmatter-driven Skill file format balances "developer readability" against "machine parseability"
- How CLI slash commands and TUI Tab completion bypass the LLM's decision layer as a fast path for manual triggering

---

## What happens when you stuff knowledge into the System Prompt?

Cramming team coding conventions, deployment SOPs, and debugging guides all into the System Prompt is the most intuitive approach. It's also the fastest way to exhaust the context window.

For a mid-sized project, writing three to five reasonably thorough spec documents will quickly balloon the System Prompt to 8,000–15,000 tokens. GPT-4o's 128K window looks generous, but in practice the context window is shared with conversation history, tool results, and multi-turn reasoning. Knowledge in the System Prompt that's "irrelevant today" still consumes attention resources — worse, an LLM's attention allocation isn't uniform, and an overly long System Prompt causes the model to lose focus on the critical information.

harness9's Agent Skills system offers a different answer: **don't stuff capabilities in — tell the LLM where the capability lives and when to go fetch it.**

![Diagram: System Prompt bloat vs. Skill index](/blog/agent-skills/images/prompt-inflation-vs-skill-index-01.png)


---

## What does a Skill file look like?

harness9 organizes Skills by directory — each Skill is an independent subdirectory that must contain a `SKILL.md` file:

```
{workdir}/skills/
├── go-refactor/
│   └── SKILL.md
├── deploy-prod/
│   └── SKILL.md
└── debugging-guide/
    └── SKILL.md
```

The format of `SKILL.md` is standard Markdown with a YAML frontmatter block:

```markdown
---
name: go-refactor
description: Use when refactoring Go code — team conventions and patterns
trigger: "refactor, clean up, restructure, simplify"
---

# Go Refactoring Guide

## Before you refactor

1. Run `go vet ./...` to confirm there are no static-analysis errors
2. Run `go test ./...` to confirm all tests pass
3. Review the git diff to confirm the scope of the change
```

The frontmatter has only three fields: `name` (unique identifier), `description` (the index description shown to the LLM), and `trigger` (an optional trigger-word hint — documentation only, not used for automatic matching).

The key to this design is **separating frontmatter from body**. The frontmatter is what gets injected into the System Prompt — just name and description; the body is loaded on demand — the full domain-knowledge text. The two live in the same physical file but belong to different loading stages logically.

The implementation of `parseFrontmatter` is deliberately minimal, zero-dependency, hand-rolled parsing:

```go
func parseFrontmatter(content string) (name, description, trigger, body string) {
    const delim = "---\n"
    if !strings.HasPrefix(content, delim) {
        return "", "", "", content
    }
    rest := content[len(delim):]
    idx := strings.Index(rest, "\n---\n")
    if idx == -1 {
        return "", "", "", content
    }
    fm := rest[:idx]
    body = strings.TrimPrefix(rest[idx+len("\n---\n"):], "\n")
    // ...line-by-line key:value parsing
}
```

No YAML parsing library was introduced. This is a deliberate trade-off: the frontmatter's field set is fixed and tiny (three fields), so hand-rolled parsing fully covers the need while avoiding an extra indirect dependency. One of harness9's design philosophies is "keep the number of direct dependencies minimal," and Skill format parsing is a concrete embodiment of that philosophy.

![Diagram: Skill file format parsing flow](/blog/agent-skills/images/skill-file-parsing-02.png)


---

## Why not RAG?

Progressive Disclosure is a term from the UX design world, meaning "only reveal complex information when the user needs it." harness9 carries this idea into the context-management layer of LLM engineering.

To understand its value, first compare two common alternatives:

**Option A: Full injection into the System Prompt.** All Skill bodies are written into the System Prompt at startup. Simple, but token cost is fixed, the LLM's attention gets diluted by irrelevant content, and every conversation carries content that "may never be used."

**Option B: RAG retrieval.** Use embedding vectors to retrieve relevant Skills and inject the most relevant snippets into the context. Token-efficient, but it introduces the engineering complexity of a vector database dependency, embedding-model calls, and similarity-threshold tuning. More importantly, recall is passive — the timing and strategy of retrieval are decided by the framework, not the LLM.

**harness9's approach**: the LLM actively pulls. The System Prompt only injects the Skill index (one line per entry, name + description); the LLM decides for itself which Skill is needed while executing a task, calls the `use_skill` tool to pull the full content, and the tool result is injected as an Observation into the current turn's context.

There's a key architectural decision here: **the judgment call on recall is handed to the LLM, not to a retrieval algorithm at the framework layer**.

This means:
- No embedding model or vector database is required
- The LLM can judge which Skill is needed based on the full semantics of the task, rather than token similarity
- The Skill's description text (the `description` field) becomes the core variable affecting recall quality — a piece of text developers can control precisely, not a fluctuating similarity score

The downside is equally obvious: the LLM must see the task first before it can judge which Skill it needs, which costs one extra LLM turn compared to RAG. This is a trade-off harness9 explicitly chose to accept.

### Four engineering principles behind Progressive Disclosure

Progressive Disclosure isn't just "lazy loading" — there are three deeper engineering principles behind it.

**Principle one: LLM attention allocation isn't uniform**

A Transformer's attention mechanism doesn't treat every token in the context equally. Empirical observations (as well as research such as [Lost in the Middle](https://arxiv.org/abs/2307.03172)) show that content near the beginning and end of the context window is more likely to be "attended to," while content in the middle tends to get suppressed.

This means that if you stack the full text of 10 Skills (say, 1,000 tokens each, 10,000 tokens total) into the System Prompt, most of that content ends up in an "attention trough." The LLM will know something is there but won't necessarily reference it precisely.

Conversely, when the LLM pulls a Skill's body via `use_skill`, the content appears as a ToolResult at **the end of the current turn** — the position of highest attention. The same text, purely because of where it appears, is more likely to be effectively used by the model.

Progressive Disclosure doesn't just save tokens — more importantly, it **places knowledge at the position in the context that's most effective for the LLM's attention**.

**Principle two: the index is a metacognitive layer, the body is an execution layer**

This is the most elegant part of harness9's design. The Skill index in the System Prompt (name + description) isn't content for the LLM to "read" — it's a signal for the LLM to "decide" with.

```
## Available Skills
- go-refactor: Use when refactoring Go code — team conventions and patterns
- deploy-prod: Use when deploying to production environment
```

When the LLM reads these two lines, it doesn't try to "understand the Go refactoring conventions" — it stores them as a piece of metacognition: **"I have these two capabilities, and I can invoke them when a task falls into these categories."** This is an extremely low-cognitive-load operation — it's merely remembering "capability boundaries," not digesting knowledge.

When a user asks "help me clean up this handler, it's a mess," the LLM doesn't need to search — it fires directly from the metacognitive layer: `go-refactor`'s description contains "refactoring," a match. So it issues a `use_skill("go-refactor")` call.

**This is semantic routing, not similarity matching**. The LLM understands the semantics of the task and then actively looks up the appropriate knowledge — rather than embedding the task and computing vector distances against every Skill. The two behave completely differently on accuracy curves: RAG is prone to false recall when wording is close but meaning differs, while an LLM's semantic judgment is more robust to wording variation.

**Principle three: the description field is the only recall control lever**

In a RAG system, recall quality is influenced by multiple variables: choice of embedding model, chunking strategy (chunk size / overlap), similarity threshold, document preprocessing method — most of these parameters are opaque and tuning them requires experimentation.

Progressive Disclosure compresses all these variables into one: **the text quality of the `description` field**.

This is a fully transparent, precisely controllable text variable. Developers can optimize the description the way they'd optimize a prompt:

```yaml
# A poor description: vague, unclear trigger conditions
description: "Conventions related to Go code"

# A good description: explicit behavioral trigger words, concrete task scenario
description: "Use when refactoring Go code — covers naming, error handling, interface design, and test coverage conventions for this team"
```

The more precise the description, the more accurate the LLM's recall. This is text engineers can directly intervene in and tune — with no black-box component involved.

Going further: harness9 allows Skill files to be modified at runtime (`GetFullContent` doesn't cache — it reads from disk every time), which means the description can also be hot-updated without restarting the Agent. This gives developers a fast feedback loop for iterating on Skill description copy.

**Principle four: the difference in context lifecycle between ToolResult position and System Prompt position**

Injecting Skill content into a ToolResult (rather than the System Prompt) carries another important architectural implication: **its lifecycle is managed by the Compactor**.

harness9's SummarizationCompactor triggers when the context reaches the 80% threshold, compressing historical messages (including ToolResults) into a summary. This means the full text of a Skill will eventually be summarized — it won't permanently occupy the context.

By contrast, the System Prompt is a fixed cost that never participates in compaction — once it's in there, it stays forever, even after that knowledge has long since been used up. Putting the Skill body in a ToolResult brings it into the context's normal "consume — compact — release" lifecycle, rather than locking it permanently into the System Prompt.

The following sequence diagram shows the complete Progressive Disclosure workflow:

```
At startup
  ┌─────────────────────────────────────────────────┐
  │  System Prompt (fixed cost)                      │
  │  ...                                             │
  │  ## Available Skills                             │  ← Index layer: metacognitive signal
  │  - go-refactor: Use when refactoring Go code...  │    ~20 tokens per entry, n Skills
  │  - deploy-prod: Use when deploying to prod...    │    Fixed cost, independent of Skill body size
  └─────────────────────────────────────────────────┘

Turn N (LLM decides a Skill is needed)
  LLM output: tool_call → use_skill("go-refactor")
  Framework: GetFullContent → read from disk → return the Skill body

  ┌─────────────────────────────────────────────────┐
  │  ToolResult (dynamic, part of session history)   │
  │  [go-refactor full body, 1,200 tokens]           │  ← Content layer: peak attention at execution time
  │  # Go Refactoring Guide                          │    Appears only when the task needs it
  │  ## Before you refactor                          │    Eventually summarized by the Compactor
  │  1. Run go vet...                                │
  └─────────────────────────────────────────────────┘

On the next compaction round
  SummarizationCompactor compresses the ToolResult into
  "[Loaded go-refactor conventions, key points: go vet first, interfaces defined by the caller...]"
  → The Skill body has served its purpose, tokens are released
```

**Summary comparison of the three approaches**:

| Dimension | Full injection | RAG retrieval | Progressive Disclosure |
|------|---------|---------|----------------------|
| Skill-count scalability | Poor (linear growth) | Good | Good (index scales linearly, body on demand) |
| Engineering complexity | Very low | High (vector DB + embedding model) | Low (zero external dependencies) |
| Recall-accuracy control | N/A | Tuning (opaque) | Optimize description (transparent) |
| LLM attention efficiency | Low (knowledge in the middle, attention trough) | Medium (snippet injection position is uncertain) | High (ToolResult at the end of the current turn) |
| Context lifecycle | Permanently occupies the System Prompt | Enters session history | Enters session history, managed by the Compactor |
| Extra LLM turns | 0 | 0 (framework-triggered) | +1 (LLM decision call) |

The extra +1 turn is the only cost Progressive Disclosure pays. harness9 considers it worthwhile — in exchange it gets transparent recall control, better attention efficiency, and zero external dependencies.

![Diagram: comparison of three Skill recall strategies](/blog/agent-skills/images/skill-retrieval-comparison-03.png)


---

## Three layers of lazy loading

harness9's Skill loading is split into three clearly defined layers, with no overlapping responsibilities.

**Layer one: `LoadSkills` (at startup, directory scan)**

```go
func LoadSkills(skillsDir string) (*Index, error) {
    entries, err := os.ReadDir(skillsDir)
    if errors.Is(err, fs.ErrNotExist) {
        return &Index{}, nil  // Directory doesn't exist — silently return an empty Index
    }
    // ...
    for _, entry := range entries {
        if !entry.IsDir() {
            continue  // Only process subdirectories; loose top-level files are ignored
        }
        filePath := filepath.Join(skillsDir, entry.Name(), "SKILL.md")
        data, err := os.ReadFile(filePath)
        // ...
        name, desc, trigger, _ := parseFrontmatter(string(data))
        if name == "" || desc == "" {
            // Skip invalid frontmatter, print a warn log
            continue
        }
        loaded = append(loaded, Skill{
            Name: name, Description: desc, Trigger: trigger,
            filePath: filePath,  // Only the path is recorded, not the body
        })
    }
    return &Index{skills: loaded}, nil
}
```

Note that `Skill.filePath` is an unexported field, and at this point only the file path is recorded — **the file body is not read**. This is the physical evidence of lazy loading: startup-time I/O only touches the frontmatter, and the body never enters memory.

**Layer two: `Index` (at runtime, indexing plus on-demand reads)**

```go
func (idx *Index) GetFullContent(name string) (string, error) {
    for _, s := range idx.skills {
        if s.Name == name {
            data, err := os.ReadFile(s.filePath)  // Read from disk on every call
            if err != nil {
                return "", fmt.Errorf("failed to read skill %q: %w", name, err)
            }
            _, _, _, body := parseFrontmatter(string(data))
            return strings.TrimSpace(body), nil
        }
    }
    return "", fmt.Errorf("skill %q does not exist, available skills: %s", name, idx.availableNames())
}
```

`GetFullContent` re-reads from disk on every call. There's no in-memory cache. This decision is worth noting: Skill files are typically content a developer might modify while the Agent is running (adjusting conventions, updating an SOP), and not caching means edits take effect immediately — the next `use_skill` call picks up the new version. For a development-time tool, this trade-off makes sense.

**Layer three: `UseSkillTool` (the tool layer, executing the LLM's decision)**

```go
func (t *UseSkillTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
    var a useSkillArgs
    if err := json.Unmarshal(args, &a); err != nil {
        return "", fmt.Errorf("failed to parse arguments: %w", err)
    }
    if a.SkillName == "" {
        return "", fmt.Errorf("skill_name must not be empty")
    }
    return t.index.GetFullContent(a.SkillName)
}
```

`UseSkillTool` is an ordinary tool in the tool registry, satisfying the `tools.BaseTool` interface implicitly via Go's structural typing — note the code comment explicitly states "no need to import the tools package," which avoids a circular import.

![Diagram: three-layer loading mechanism sequence](/blog/agent-skills/images/skill-loading-sequence-04.png)


---

## Where does the Skill index get injected?

`DefaultPromptBuilder.Build()` assembles the System Prompt in a fixed order, with the Skills index as the third section:

```
1. Base prompt (role definition + working directory + current date + operating principles)
2. AGENTS.md (user project conventions, silently skipped if absent)
3. Available Skills (index summary, the whole block is skipped when the Index is empty)
4. Task management (todo_write guidance, injected only when the tool is registered)
5. Large-output file retrieval (OffloadHook guidance)
6. Long-Term Memory (MEMORY.md materialized view)
```

The actual text form of the Skills section:

```
## Available Skills

Use the `use_skill` tool to load the full content of any Skill when needed.

- go-refactor: Use when refactoring Go code — team conventions and patterns
- deploy-prod: Use when deploying to production environment
- debugging-guide: Use when debugging runtime errors or performance issues
```

`Index.Summary()` generates a plain-text list in the form `- name: description\n` per line, with very low token cost. The index for three Skills costs roughly 60–90 tokens — no matter how long the Skill bodies are, the token cost of this section in the System Prompt is fixed.

There's a test case here worth citing as a normative reference:

```go
// Progressive Disclosure: the skill's full body must not appear in the System Prompt
if strings.Contains(prompt, "Always run go vet first.") {
    t.Error("prompt must NOT contain skill body content (progressive disclosure violated)")
}
```

This assertion isn't just a test — it's a declaration of the protocol's **invariant**: the body must never leak into the System Prompt.

![Diagram: System Prompt section structure and the Skill index position](/blog/agent-skills/images/system-prompt-anatomy-05.png)


---

## How does the LLM use a Skill?

Here's what one complete Skill usage looks like within the ReAct loop:

```
Turn N (user submits a task):
  The LLM receives the System Prompt (with the Skills index) + the user prompt
  LLM output: tool_call → use_skill(skill_name="go-refactor")

  The framework executes use_skill:
    Index.GetFullContent("go-refactor")
    → reads skills/go-refactor/SKILL.md
    → parseFrontmatter → returns the body

  The tool result is injected as an Observation into the Turn N context

Turn N+1:
  The LLM receives the full go-refactor guide content (in the ToolResult)
  The LLM executes the actual refactoring task guided by this guide
  LLM output: subsequent tool calls (read_file, edit_file, etc.)
```

The full text of a Skill appears in the `ToolResult` (the tool observation), not in the System Prompt. This is the key difference in context injection position: a ToolResult is part of session history and can be summarized or trimmed across compaction rounds; the System Prompt is a permanent cost that never participates in compaction.

![Diagram: where a Skill call sits in the ReAct loop](/blog/agent-skills/images/skill-in-react-loop-06.png)


---

## Two trigger paths

harness9's Skills system is designed with two trigger paths, corresponding to different usage scenarios.

**Path one: tool calling (the main path)**

The LLM decides on its own, triggering via a `use_skill` tool call. This is the mainstream path in production, where the LLM decides whether it needs a Skill — and which one — based on the semantics of the task.

**Path two: slash command (a manual fast path)**

![Diagram: executing a Skill via a slash command](/blog/agent-skills/images/slash-command-skill.png)

In CLI REPL mode, a user can type `/skill-name` directly to bypass the LLM's decision:

```go
func resolvePrompt(input string, idx *skills.Index) (prompt string, ok bool) {
    if !strings.HasPrefix(input, "/") || idx == nil {
        return input, true
    }
    rest := strings.TrimPrefix(input, "/")
    name, extra, _ := strings.Cut(rest, " ")
    extra = strings.TrimSpace(extra)

    body, err := idx.GetFullContent(name)
    if err != nil {
        log.Print(logfmt.FormatMsg("skills", fmt.Sprintf("activation failed: %v", err)))
        return "", false
    }
    if extra == "" {
        return body, true
    }
    return body + "\n\n" + extra, true
}
```

`/go-refactor clean up main.go` concatenates the Skill body with the extra instruction and sends it directly to the LLM as the prompt. This effectively promotes the Skill content from the ToolResult layer to the User Message layer — the LLM sees the full Skill content in the very first turn, with no extra tool-call round needed.

In TUI mode, Tab completion merges Skill names with built-in commands into a single completion list, triggered by the `/` prefix, and highlighted in cyan (`Color("14")`) to distinguish them. This is a small but telling detail: `skillStyle` uses a different color from ordinary commands — the visual distinction mirrors a semantic one: a Skill is user-defined domain capability, while a built-in command is a framework control command.

![Diagram: comparison of the two Skill trigger paths](/blog/agent-skills/images/skill-trigger-paths-07.png)


---

## What happens when a Skill call fails?

The error message `GetFullContent` produces when a Skill doesn't exist is deliberately designed:

```go
return "", fmt.Errorf("skill %q does not exist, available skills: %s", name, idx.availableNames())
```

The error message includes all available Skill names. Since this error is returned to the LLM as a `ToolResult` with `IsError: true`, the LLM can read "the Skill I called doesn't exist, but here are the ones available," and then correct its call based on that list, choose a different Skill, or simply tell the user directly that no corresponding domain knowledge exists. This is harness9's general pattern for tool-failure design — an error doesn't terminate the loop; it becomes an Observation that lets the LLM self-heal.

When the Skills directory doesn't exist, an empty `Index` is returned, silently, with no error. When `Index.IsEmpty()` is true, `DefaultPromptBuilder` skips the entire Skills section. This follows the framework's zero-configuration principle: with no Skill files, the Agent works as usual — it just lacks this capability-extension mechanism.

---

## A Skill is neither a Tool nor Memory

LangChain's `Tool` is an executable function with inputs and outputs, and its execution produces side effects.

The OpenAI Agents SDK's `file_search` is RAG retrieval — it injects relevant document snippets into the context, with recall authority residing in the embedding model.

harness9's Skill is a **declarative domain-knowledge container** — it doesn't execute, produces no side effects, and its content is static Markdown text that, once loaded, becomes part of the LLM's cognitive context rather than a tool-call result.

It's closer to a "protocol convention": a Skill's `description` field is input to the LLM's decision layer, while its `body` is input to the LLM's execution layer — the two belong to different points in time and different positions in the context.

CrewAI's Agent Role is fixed when the Agent is initialized and can't be extended at runtime. harness9's Skill is pulled on demand at runtime — within a single Interaction, the LLM can load multiple Skills in sequence, each supplying the domain knowledge for one sub-task; once the task is done, that content stays in session history awaiting processing by the compactor.

---

## Closing thoughts

At its core, harness9's Agent Skills system rests on a single judgment: **an LLM is better than a retrieval algorithm at judging "what knowledge is needed right now."**

That judgment holds as long as the semantics of the task are transparent to the LLM and the Skill's description text is clear enough. It shifts an engineer's focus from "tuning embedding similarity thresholds" to "writing a good Skill description" — a task much closer to human intuition.

One thing worth pondering: as the number of Skills grows into the dozens, the index itself will start consuming a noticeable number of tokens. harness9 currently does no further layering or grouping of the index — that's an interesting design problem for the next iteration.

---
