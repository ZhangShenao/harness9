---
title: "Benchmarking Beyond Scores: How harness9 Iterates from Execution Traces"
date: 2026-07-24
tags: [harness9, agent, golang, benchmark, swe-bench, terminal-bench]
summary: "harness9 treats SWE-bench and Terminal-Bench runs as engineering evidence: it aligns trajectories, patches, environments, and verifier output to improve the runtime rather than merely chase a score."
cover: /blog/benchmark-driven-iteration/images/cover.png
---

# Benchmarking Beyond Scores: How harness9 Iterates from Execution Traces

## About harness9

harness9 is a Local-First, lightweight, feature-complete, production-ready Agent framework for Go.

- **Website**: [https://zhangshenao.github.io/harness9/](https://zhangshenao.github.io/harness9/)
- **GitHub**: [https://github.com/ZhangShenao/harness9](https://github.com/ZhangShenao/harness9)

## TL;DR

- SWE-bench evaluates whether an Agent can turn an issue and a repository revision into a patch that passes hidden tests.
- Terminal-Bench evaluates the final state of a real terminal environment through Harbor and a task verifier.
- A score alone cannot distinguish an engine defect, a broken environment, an adapter constraint, and a model decision.
- harness9 uses trajectory analysis to turn those distinctions into narrowly scoped changes: bootstrap steps, verification gates, retries, timeouts, and stall prompts.
- M1 is the shipped v1.0.0 Agent Harness baseline. M2, a local multi-Agent OS, is a roadmap rather than a benchmark result.

![The benchmark-to-iteration feedback loop](/blog/benchmark-driven-iteration/images/benchmark-feedback-loop-01.png)

## SWE-bench: patch quality without test leakage

A SWE-bench instance supplies a repository, a base commit, and an issue description. The runner creates an isolated work directory and Sandbox, runs the Agent with normal file and shell tools, then collects the resulting `git diff`. The official evaluator applies its private `test_patch` only after the Agent has finished.

This boundary matters. `problem_statement` is valid Agent input; `test_patch`, `FAIL_TO_PASS`, and `PASS_TO_PASS` are evaluator-only evidence. Exposing them to the prompt would turn an evaluation into test leakage.

![The SWE-bench execution and evaluation boundary](/blog/benchmark-driven-iteration/images/swebench-execution-pipeline-02.png)

The runner also preserves new files with `git add -A -N` before collecting the diff. Benchmark execution uses bounded turns, retry policy, an expanded shell timeout, and project bootstrap commands so a recoverable dependency failure does not look like a model failure.

## A verification gate, not an infinite loop

Early trajectories showed a basic failure mode: Agents often stopped without running a real test. `runWithVerificationGate` observes bash tool events. If a run ends without a detected test command, the benchmark runner injects one verification reminder and resumes with the same `MemorySession`.

The continuation is deliberately capped at one attempt and remains bounded by the instance timeout and `MaxTurns`; it is a measurement guardrail, not a generic change to the Agent loop.

![The one-shot verification gate](/blog/benchmark-driven-iteration/images/verification-gate-state-flow-03.png)

## Terminal-Bench: final environment state

Terminal-Bench 2.0 is integrated through `benchmarks/terminal_bench/harness9_agent.py`. `Harness9Agent`, a Harbor `BaseInstalledAgent`, installs the static harness9 binary into the task container, uploads the instruction file, and runs the binary non-interactively. Harbor owns lifecycle and verifier execution; `reward.txt` records the outcome.

This exposed different classes of engineering work. Some task images lacked `ca-certificates`, so HTTPS failed deterministically for static Go binaries; retrying an unavailable trust store is not resilience. The adapter now installs the certificate package. Likewise, the adapter keeps a wide absolute fallback while Harbor respects task-specific timeout budgets.

![The Harbor and Terminal-Bench adapter lifecycle](/blog/benchmark-driven-iteration/images/terminal-bench-harbor-adapter-05.png)

## Use trajectories as evidence

Every conclusion needs three aligned records: `agent/harness9.log` for turns and tool calls, the patch or terminal state for actual behavior, and verifier output for an external judgment. Only then can an apparent regression be classified as a runtime issue, an environment issue, an evaluation-adapter issue, or model behavior.

![Trajectory analysis turns evidence into focused changes](/blog/benchmark-driven-iteration/images/trajectory-analysis-loop-04.png)

On a controlled SWE-bench Lite comparison of 24 instances, resolved tasks rose from 16 to 19 and tasks that ran a real test rose from 1 to 18. The important result is not a single percentage point: it is restoring a test-feedback loop that makes subsequent changes explainable.

## From M1 to M2

M1, harness9 v1.0.0, established the production-ready Agent Harness baseline: the ReAct engine, providers, tools, Sandbox, permissions, MCP, planning, Skills, Sub-Agents, AutoDev, memory, evaluation, benchmarks, observability, TUI, CLI, and bilingual documentation.

M2 is the roadmap for a local multi-Agent OS. It covers role and permission boundaries, task graphs and scheduling, worktree-session-sandbox lifecycle, resumable state, conflict-aware file ownership, provenance-aware memory, benchmark regression dashboards, and operator controls. It is explicitly not a claimed delivery or an extrapolation from the benchmark score.

![Delivered M1 versus the planned M2 boundary](/blog/benchmark-driven-iteration/images/m1-m2-roadmap-06.png)

## Conclusion

Benchmarks become useful when they are treated as a chain of evidence, not a leaderboard number. That discipline is what lets harness9 improve an Agent runtime without confusing accidental wins for product progress.
