---
layout: home

hero:
  name: "harness9"
  text: "A lightweight Go Agent Harness framework"
  tagline: Feature-complete and production-ready, with minimal abstractions and code that stays out of your way.
  image:
    src: ''
    alt: 'harness9 terminal demo'
  actions:
    - theme: brand
      text: Quick Start →
      link: /docs/quick-start
    - theme: alt
      text: Read the Docs
      link: /docs/tui

features:
  - icon: 🎯
    title: Simple
    details: Minimal abstraction layers, straightforward and readable code, and very few direct dependencies. Adopting harness9 doesn't mean buying into a complex conceptual system.
  - icon: ✅
    title: Complete
    details: Covers every core module an Agent needs to run — Engine, Provider, Schema, Tools, Env — ready to use out of the box.
  - icon: 🚀
    title: Production-Ready
    details: Error recovery, context management, timeout control, concurrent tool execution, and Path Traversal protection — not just a demo.
  - icon: 💻
    title: Full-Screen TUI
    details: Streaming output, spinner progress indicators, real-time token usage display, and Tab completion. Full-screen AltScreen rendering, with automatic fallback to CLI for non-TTY environments.
  - icon: ⚡
    title: Shell Execution
    details: Prefix the input box with ! to run Bash commands directly. Output is appended to the conversation stream and automatically injected into the LLM context — no need to switch terminals.
  - icon: 🧠
    title: Context Engineering
    details: LLM-based summarization compaction, SQLite session persistence, and automatic triggering at an 80% threshold — long conversations never lose their semantics.
  - icon: 🗃️
    title: Long-Term Memory
    details: Cross-session Long-Term Memory backed by SQLite + FTS5, with a MEMORY.md materialized view injected in real time. Triggered three ways — explicit tools, pre-compaction extraction, and nudges — with zero new dependencies.
  - icon: 📋
    title: Planning Module
    details: Plan Mode for plan-then-execute workflows, TodoStore state-machine validation, tool-layer permission filtering, automatic continuation, and stagnation detection.
  - icon: 🔀
    title: Concurrent Tool Execution
    details: Multiple tools run concurrently within a single turn, each with independent timeout control. On failure, the error is passed straight back to the LLM to trigger automatic retries.
  - icon: 💡
    title: Reasoning Display
    details: Both Anthropic extended thinking and OpenRouter's reasoning_content are routed through EventThinkingDelta, with the TUI streaming them as dark gray thinking blocks.
---

## Architecture Overview

![harness9 overall architecture diagram](/harness9_architecture.png)

---

## Quick Start

### Install

```bash
curl -fsSL https://raw.githubusercontent.com/ZhangShenao/harness9/master/scripts/install.sh | bash
```

### Configure API Keys

```bash
# OpenAI / OpenRouter
export OPENAI_API_KEY="sk-..."

# or Anthropic
export ANTHROPIC_API_KEY="sk-ant-..."
```

### Run

```bash
cd /your/project && harness9
```

> Automatically launches the full-screen TUI in a TTY environment; falls back to the CLI REPL mode in pipes/CI environments.
>
> For more configuration options, see the [Quick Start Guide](/docs/quick-start).
