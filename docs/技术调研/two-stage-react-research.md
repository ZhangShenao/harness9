# Two-Stage ReAct（两阶段推理-行动）实现机制调研报告

## 深度调研报告

> 调研日期：2026-04-22
> 调研范围：7 个主流 Agent Harness 框架的 Thinking/Planning → Action 两阶段分离机制
> 目标：为 harness9 项目的 Two-Stage ReAct 设计提供横向对比与设计参考

---

## 目录

1. [调研概述](#1-调研概述)
2. [harness9 的 Two-Stage ReAct 机制](#2-harness9-的-two-stage-react-机制)
3. [各框架实现分析](#3-各框架实现分析)
4. [横向对比总结](#4-横向对比总结)
5. [设计模式提炼](#5-设计模式提炼)
6. [对 harness9 的设计建议](#6-对-harness9-的设计建议)

---

## 1. 调研概述

### 1.1 什么是 Two-Stage ReAct

Two-Stage ReAct 是一种在每个 Agent Turn 中将推理（Thinking）和行动（Action）显式分离的机制：

```
┌─────────────────────────────────────────────────────────────┐
│                    传统 ReAct Loop                          │
├─────────────────────────────────────────────────────────────┤
│  Turn N:                                                     │
│    LLM(messages, tools) → response + tool_calls             │
│    execute(tool_calls) → observations                       │
│    → Turn N+1                                               │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│              Two-Stage ReAct Loop (harness9)                │
├─────────────────────────────────────────────────────────────┤
│  Turn N:                                                     │
│    Phase 1 (Thinking):                                       │
│      LLM(messages, tools=nil) → 纯推理与规划                  │
│      思考结果作为 assistant 消息注入上下文                      │
│    Phase 2 (Action):                                         │
│      LLM(messages, tools=all) → 基于 Phase 1 的行动           │
│      execute(tool_calls) → observations                     │
│    → Turn N+1                                               │
└─────────────────────────────────────────────────────────────┘
```

**核心设计思想**：通过在 Thinking 阶段剥夺 LLM 的工具访问权，强制模型进行纯推理和规划，避免模型在行动前缺乏充分思考就贸然调用工具。

### 1.2 调研框架一览

| 框架 | 来源 | 语言 | 是否有类似两阶段分离 |
|------|------|------|---------------------|
| **OpenAI Agent SDK** | OpenAI | Python/TS | ❌ 无 |
| **Claude Agent SDK** | Anthropic | Python/TS | ❌ 无（有 Extended Thinking） |
| **HermesAgent** | NousResearch | Python | ❌ 无（有 reasoning callback） |
| **OpenHarness** | HKUDS | Python | ❌ 无 |
| **DeepAgents** | LangChain | Python | ⚠️ 部分（Planning 作为工具） |
| **OpenCode** | Anomaly | TypeScript | ⚠️ 部分（Plan Agent 模式） |
| **OpenClaw** | OpenClaw | TypeScript | ❌ 无 |

### 1.3 核心发现

**harness9 的 Two-Stage ReAct 机制在 7 个调研框架中是独一无二的。** 没有任何其他框架实现了在同一 Turn 内显式分离 Thinking 和 Action 两个阶段，且通过剥夺/恢复工具列表来强制模型行为分离的机制。

各框架的"思考"能力主要通过以下替代方案实现：

| 替代方案 | 框架 | 说明 |
|----------|------|------|
| 模型内部推理（Extended Thinking） | Claude Agent SDK | 单次 API 调用内的内部推理，不是框架层面的两阶段 |
| 推理模型（Reasoning Models） | OpenAI Agent SDK | o1/o3 系列模型的内部 Chain-of-Thought |
| Reasoning Callback | HermesAgent | 通过回调捕获模型的推理内容 |
| Planning 工具 | DeepAgents | `write_todos` 是一个工具而非独立阶段 |
| Plan Agent 模式 | OpenCode | `plan` 是独立 Agent 配置而非同一 Turn 内的阶段 |
| Think 命令 | OpenClaw | `/think <level>` 设置模型推理深度 |

---

## 2. harness9 的 Two-Stage ReAct 机制

### 2.1 实现代码

```go
// internal/engine/agent_loop.go

for {
    turnCount++
    availableTools := e.registry.GetAvailableTools()

    // ================= Phase 1: Thinking =================
    if e.EnableThinking {
        log.Println("[harness9][Phase 1] 剥夺工具访问权，强制进入慢思考与规划阶段...")
        thinkResp, err := e.provider.Generate(ctx, contextHistory, nil) // 传入 nil 剥夺工具
        if err != nil {
            return fmt.Errorf("Thinking 阶段生成失败: %w", err)
        }
        if thinkResp.Content != "" {
            fmt.Printf("🧠 [内部思考 Trace]: %s\n", thinkResp.Content)
            contextHistory = append(contextHistory, *thinkResp)
        }
    }

    // ================= Phase 2: Action =================
    log.Println("[harness][Phase 2] 恢复工具挂载，等待模型采取行动...")
    responseMsg, err := e.provider.Generate(ctx, contextHistory, availableTools)
    if err != nil {
        return fmt.Errorf("模型生成失败: %w", err)
    }

    contextHistory = append(contextHistory, *responseMsg)

    // 终止条件检测
    if len(responseMsg.ToolCalls) == 0 {
        break
    }

    // ToolCall 并发执行...
}
```

### 2.2 设计特点

| 维度 | harness9 实现 |
|------|-------------|
| **阶段分离** | 同一 Turn 内两次独立的 `Generate` 调用 |
| **工具剥夺** | Phase 1 传入 `nil` 作为 tools 参数 |
| **上下文传递** | Phase 1 结果作为 `assistant` 消息追加到 `contextHistory` |
| **可配置性** | 通过 `EnableThinking` 布尔开关控制 |
| **额外成本** | 每个 Turn 多一次 LLM API 调用 |
| **可见性** | 思考内容通过 `🧠 [内部思考 Trace]` 打印到终端 |

### 2.3 上下文流

```
Turn N 的上下文历史:
  [system] → [user] → [assistant(Thinking)] → [assistant(Action + ToolCalls)] → [user(Observation)]

Turn N+1 的上下文历史:
  ... → [assistant(Thinking_N)] → [assistant(Action_N)] → [user(Obs_N)] → [assistant(Thinking_N+1)] → ...
```

---

## 3. 各框架实现分析

### 3.1 OpenAI Agent SDK — 单阶段循环 + 推理模型

**核心文件**：`src/agents/run.py` → `AgentRunner.run()`

**循环结构**：

```python
# src/agents/run.py (简化)
while True:
    current_turn += 1
    if current_turn > max_turns:
        raise MaxTurnsExceeded(...)

    # 单次 LLM 调用，tools 始终可用
    turn_result = await run_single_turn(current_agent, original_input, ...)

    if isinstance(turn_result.next_step, NextStepFinalOutput):
        return result
    elif isinstance(turn_result.next_step, NextStepHandoff):
        current_agent = turn_result.next_step.new_agent
        continue
    elif isinstance(turn_result.next_step, NextStepRunAgain):
        continue
```

**关于"思考"的实现**：

OpenAI Agent SDK 不在框架层面实现两阶段分离。"思考"能力通过模型端的 **Reasoning Models** (o1/o3/o4-mini 系列) 提供：

```python
# 推理通过模型参数配置，不是框架层面的阶段分离
reasoning_effort = "high"  # 控制推理深度

# ReasoningItemIdPolicy 控制如何处理模型返回的 reasoning items
class ReasoningItemIdPolicy:
    # 保留/截断/移除推理内容的策略
    pass
```

**关键发现**：

| 问题 | 答案 |
|------|------|
| 是否有 Thinking/Planning → Action 的两阶段分离？ | ❌ 没有。所有工具在同一调用中可用 |
| 思考阶段是否对 LLM 隐藏工具？ | ❌ 没有。tools 始终传入 |
| 思考结果如何注入到上下文中？ | 模型端内部处理（reasoning items），SDK 通过 `ReasoningItemIdPolicy` 管理保留策略 |
| 是否有 Extended Thinking / CoT / Planning 概念？ | ✅ 有推理模型（o1/o3），但是单次 API 调用内的内部机制 |

### 3.2 Claude Agent SDK — 黑盒循环 + Extended Thinking

**核心文件**：内嵌 Claude Code 二进制，通过异步迭代器暴露

**循环结构**：

```python
# Claude Agent SDK 的 API 设计 — 黑盒循环
async for message in query(
    prompt="Find and fix the bug",
    options=ClaudeAgentOptions(allowed_tools=["Read", "Edit", "Bash"]),
):
    if isinstance(message, AssistantMessage):
        # 处理助手消息（包含内部 thinking）
    elif isinstance(message, ResultMessage):
        # 循环结束
```

**关于"思考"的实现**：

Claude Agent SDK 使用 Anthropic 的 **Extended Thinking**（扩展思考）功能。这是 Anthropic API 层面的特性，不是框架层面的两阶段分离：

```python
# Extended Thinking 在模型 API 层面配置
# Claude Opus 4.7 使用 adaptive thinking
thinking = {
    "type": "adaptive",           # 自适应思考模式
    "budget_tokens": 10000        # 思考 token 预算
}
output_config = {
    "effort": "high"              # 推理努力程度
}
```

关键区别：
- Extended Thinking 发生在 **单次 API 调用内部** — 模型先在内部生成思考链，然后生成可见响应
- 框架层面没有两阶段调用 — 只有一次 `query()` 调用
- Thinking 内容通过 `thinking` 类型的 content block 返回，不是单独的 assistant 消息

**关键发现**：

| 问题 | 答案 |
|------|------|
| 是否有 Thinking/Planning → Action 的两阶段分离？ | ❌ 没有。框架层面是黑盒，无法控制内部循环 |
| 思考阶段是否对 LLM 隐藏工具？ | ❌ 没有。工具始终在 API 请求中 |
| 思考结果如何注入到上下文中？ | 通过 Anthropic API 的 `thinking` content block，模型内部处理 |
| 是否有 Extended Thinking / CoT / Planning 概念？ | ✅ 有 Extended Thinking（adaptive thinking），API 层面 |

### 3.3 HermesAgent — 显式循环 + Reasoning Callback

**核心文件**：`run_agent.py` → `AIAgent.run_conversation()`

**循环结构**：

```python
# run_agent.py (简化)
def run_conversation(self, user_input, ...):
    messages = [{"role": "user", "content": user_input}]

    while self.iteration_budget.consume():
        # 单次 LLM 调用，工具始终可用
        response = self._call_api(messages, tools, ...)

        tool_calls = response.choices[0].message.tool_calls
        if not tool_calls:
            return response.choices[0].message.content

        # 执行工具
        results = self._execute_tool_calls(tool_calls)
        messages.append(response.choices[0].message)
        for result in results:
            messages.append({"role": "tool", ...})
```

**关于"思考"的实现**：

HermesAgent 通过多种机制处理推理，但都不是框架层面的两阶段分离：

1. **Reasoning Callback** — 捕获模型的推理内容：

```python
# 构造函数中的回调
self.thinking_callback = thinking_callback       # 思考内容回调
self.reasoning_callback = reasoning_callback     # 推理内容回调

# 在 _call_api 中处理
if reasoning_content:
    if self.reasoning_callback:
        self.reasoning_callback(reasoning_content)
```

2. **Reasoning Config** — 配置推理参数：

```python
self.reasoning_config = reasoning_config
# 例如 {"effort": "medium"} 用于 OpenRouter 的推理控制
```

3. **Scratchpad 机制** — 将推理内容转换为 think blocks：

```python
from agent.trajectory import convert_scratchpad_to_think
# 将模型的内部推理转换为可追踪的思考块
```

4. **多 API 模式支持** — 支持四种不同的 API 协议：

```python
# 支持 chat_completions, codex_responses, anthropic_messages, bedrock_converse
# anthropic_messages 模式可以利用 Anthropic 的 Extended Thinking
```

**关键发现**：

| 问题 | 答案 |
|------|------|
| 是否有 Thinking/Planning → Action 的两阶段分离？ | ❌ 没有。单次 API 调用，工具始终可用 |
| 思考阶段是否对 LLM 隐藏工具？ | ❌ 没有 |
| 思考结果如何注入到上下文中？ | 通过 callback 输出，不作为独立 assistant 消息注入上下文 |
| 是否有 Extended Thinking / CoT / Planning 概念？ | ✅ 有 reasoning_callback + scratchpad 转换，但都是后处理 |

### 3.4 OpenHarness — 显式循环 + 无思考分离

**核心文件**：`src/openharness/engine/query.py` → `run_query()`

**循环结构**：

```python
# src/openharness/engine/query.py (简化)
async def run_query(context, messages):
    turn_count = 0
    while context.max_turns is None or turn_count < context.max_turns:
        turn_count += 1

        # Auto-compact check（不是思考阶段，是上下文管理）
        async for event, usage in _stream_compaction(trigger="auto"):
            yield event, usage

        # 单次 LLM 流式调用，工具始终可用
        async for event in context.api_client.stream_message(
            ApiMessageRequest(
                model=context.model,
                messages=messages,
                tools=context.tool_registry.to_api_schema(),  # 工具始终传入
            )
        ):
            # 处理流式事件...

        # 终止条件
        if not final_message.tool_uses:
            return

        # 并行工具执行
        raw_results = await asyncio.gather(
            *[_run(tc) for tc in tool_calls], return_exceptions=True
        )

        messages.append(ConversationMessage(role="user", content=tool_results))
```

**关键发现**：

| 问题 | 答案 |
|------|------|
| 是否有 Thinking/Planning → Action 的两阶段分离？ | ❌ 没有。标准的单阶段 ReAct 循环 |
| 思考阶段是否对 LLM 隐藏工具？ | ❌ 没有。`tools=context.tool_registry.to_api_schema()` 始终传入 |
| 思考结果如何注入到上下文中？ | 不适用 |
| 是否有 Extended Thinking / CoT / Planning 概念？ | ⚠️ 有 Plan Mode（通过 `enter_plan_mode`/`exit_plan_mode` 工具），但是工具级别的模式切换，不是 Turn 内的阶段分离 |

**Plan Mode 详解**：

OpenHarness 有一个 `enter_plan_mode`/`exit_plan_mode` 工具对，但它与 harness9 的两阶段机制有本质区别：

```
OpenHarness Plan Mode:
  Turn 1: LLM calls enter_plan_mode → 权限切换到 Plan 模式（禁止写操作）
  Turn 2: LLM 在 Plan 模式下分析代码（只读工具可用）
  Turn 3: LLM calls exit_plan_mode → 恢复正常模式
  Turn 4: LLM 执行实际的修改操作

harness9 Two-Stage ReAct:
  Turn N Phase 1: LLM 被调用时 tools=nil（无任何工具可用，纯思考）
  Turn N Phase 2: LLM 被调用时 tools=all（所有工具可用，基于思考行动）
  → 两个 Phase 在同一个 Turn 内完成
```

### 3.5 DeepAgents — 图编排 + Planning 工具

**核心文件**：`libs/deepagents/graph.py` → `create_deep_agent()`

**循环结构**：

```python
# DeepAgents 不直接实现循环，而是通过 LangGraph 图编排
def create_deep_agent(model, tools, *, system_prompt, middleware, ...):
    # 组装 middleware 栈
    deepagent_middleware = [
        TodoListMiddleware(),       # ← Planning 中间件
        SkillsMiddleware(...),
        FilesystemMiddleware(...),
        SubAgentMiddleware(...),
        create_summarization_middleware(model, backend),
        PatchToolCallsMiddleware(),
    ]

    return create_agent(
        model,
        system_prompt=final_system_prompt,
        tools=_tools,
        middleware=deepagent_middleware,
    ).with_config({"recursion_limit": 9_999})
```

**关于"思考"的实现**：

DeepAgents 的 Planning 机制通过 `write_todos` 工具实现：

```python
# Planning 不是框架层面的独立阶段，而是模型可以选择调用的工具
# write_todos: 创建和更新任务列表，追踪进度
# 这与 harness9 的"剥夺工具强制思考"有本质区别
```

模型在需要规划时主动调用 `write_todos` 工具：

```
LLM 思考 → "我需要先规划一下" → 调用 write_todos 工具 → 继续推理
```

与 harness9 的区别：
- DeepAgents: Planning 是**可选的工具**，由模型决定是否使用
- harness9: Thinking 是**强制的阶段**，由框架在每个 Turn 强制执行

**关键发现**：

| 问题 | 答案 |
|------|------|
| 是否有 Thinking/Planning → Action 的两阶段分离？ | ⚠️ 部分。有 `write_todos` Planning 工具，但不是框架级别的阶段分离 |
| 思考阶段是否对 LLM 隐藏工具？ | ❌ 没有。Planning 是工具之一，其他工具仍然可用 |
| 思考结果如何注入到上下文中？ | 通过工具调用/返回的消息机制（标准的 tool_result） |
| 是否有 Extended Thinking / CoT / Planning 概念？ | ✅ 有 TodoListMiddleware + write_todos 工具，但与 harness9 的机制不同 |

### 3.6 OpenCode — Vercel AI SDK + Agent 模式切换

**核心文件**：`packages/opencode/src/session/session.ts` + Vercel AI SDK

**循环结构**：

```typescript
// OpenCode 使用 Vercel AI SDK 的 streamText/generateText
// 循环本身由 AI SDK 的 maxSteps 参数控制
const result = await streamText({
    model: model,
    messages: messages,
    tools: tools,
    maxSteps: agent.steps,  // Agent 配置的最大步数
    onStepFinish: async (step) => { /* 更新 session */ },
})
```

**关于"思考"的实现**：

OpenCode 通过 **Agent 模式**实现类似"规划-执行"的分离，但不是同一 Turn 内的两阶段：

```typescript
// 两种内置 Agent 模式
export const Info = z.object({
    name: z.string(),
    mode: z.enum(["subagent", "primary", "all"]),
    permission: Permission.Ruleset.zod,
    steps: z.number().int().positive().optional(),  // max turns
})

// build Agent: 全权限，用于实际开发工作
// plan Agent: 只读模式，用于分析和探索
//   - 禁止文件编辑
//   - Bash 命令需要权限确认
//   - 适合探索陌生代码库或规划变更
```

用户通过 `Tab` 键在 `build` 和 `plan` 模式之间切换。这是**人工介入的模式选择**，而非自动化的两阶段机制。

**关键发现**：

| 问题 | 答案 |
|------|------|
| 是否有 Thinking/Planning → Action 的两阶段分离？ | ⚠️ 部分。有 `plan` Agent 模式（只读），但是不同的 Agent 配置，不是同一 Turn 内的阶段分离 |
| 思考阶段是否对 LLM 隐藏工具？ | ⚠️ 部分。`plan` 模式限制了工具权限（无写操作），但不是剥夺全部工具 |
| 思考结果如何注入到上下文中？ | 不适用（跨 Agent 模式的上下文共享通过 session 管理） |
| 是否有 Extended Thinking / CoT / Planning 概念？ | ✅ 有 `plan` Agent 模式和 `compaction` Agent（用于上下文压缩） |

### 3.7 OpenClaw — Vercel AI SDK + Think 命令

**核心文件**：基于 Vercel AI SDK 的 Gateway 架构

**循环结构**：

```typescript
// OpenClaw 使用 Vercel AI SDK 的 streamText
const result = await streamText({
    model: model,
    messages: messages,
    tools: tools,
    maxSteps: agent.steps,
    onStepFinish: async (step) => { /* 更新 session */ },
})
```

**关于"思考"的实现**：

OpenClaw 通过 `/think` 命令控制模型的推理深度：

```
/think <level>    — 设置思考级别（如 low/medium/high）
```

这是对模型推理参数的配置，不是框架层面的两阶段分离。类似于调整 `reasoning_effort` 参数。

**关键发现**：

| 问题 | 答案 |
|------|------|
| 是否有 Thinking/Planning → Action 的两阶段分离？ | ❌ 没有。标准的单阶段 ReAct 循环 |
| 思考阶段是否对 LLM 隐藏工具？ | ❌ 没有 |
| 思考结果如何注入到上下文中？ | 不适用 |
| 是否有 Extended Thinking / CoT / Planning 概念？ | ⚠️ 有 `/think` 命令设置推理级别，但只是模型参数配置 |

---

## 4. 横向对比总结

### 4.1 核心对比表

| 维度 | harness9 | OpenAI SDK | Claude SDK | HermesAgent | OpenHarness | DeepAgents | OpenCode | OpenClaw |
|------|----------|-----------|-----------|-------------|-------------|-----------|---------|---------|
| **两阶段分离** | ✅ 显式 | ❌ | ❌ | ❌ | ❌ | ⚠️ 工具级 | ⚠️ Agent 级 | ❌ |
| **实现方式** | nil tools 参数 | — | — | — | — | write_todos 工具 | plan Agent 模式 | — |
| **工具剥夺** | ✅ 完全剥夺 | ❌ | ❌ | ❌ | ❌ | ❌ | ⚠️ 部分限制 | ❌ |
| **思考结果传递** | assistant 消息 | — | — | callback | — | tool_result | session | — |
| **额外 API 调用** | ✅ 每Turn +1 | — | — | — | — | — | — | — |
| **模型端推理** | 可选 | ✅ o1/o3 | ✅ Extended Thinking | ✅ reasoning_config | ❌ | ❌ | ❌ | ⚠️ /think 命令 |
| **用户可控** | EnableThinking | — | — | — | — | — | Tab 切换 | /think |

### 4.2 思考机制分类

```
┌─────────────────────────────────────────────────────────────────┐
│                  Agent "思考"机制分类谱系                         │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  框架级强制 ←————————————————————→ 模型级可选                    │
│                                                                 │
│  harness9          DeepAgents        OpenCode     Claude SDK    │
│  (Two-Stage        (Planning         (Plan Mode   (Extended     │
│   ReAct)            Tool)             切换)        Thinking)    │
│                                                                 │
│  ● 剥夺全部工具    ● 保留全部工具    ● 限制写工具  ● 模型内部     │
│  ● 强制纯推理      ● 模型可选调用    ● 用户手动    ● 透明处理     │
│  ● 思考可见        ● 结果可见        ● 跨模式      ● API 层面     │
│  ● 双 API 调用     ● 无额外调用      ● 无额外调用  ● 无额外调用   │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### 4.3 与 harness9 的异同点分析

#### 与 OpenAI Agent SDK 的对比

| 维度 | harness9 | OpenAI SDK |
|------|----------|-----------|
| 架构 | 显式两阶段循环 | 单阶段循环 |
| 思考来源 | 框架强制（剥夺工具） | 模型内置（reasoning models） |
| 工具可用性 | Phase 1: 无; Phase 2: 有 | 始终可用 |
| 成本 | 2x API 调用/Turn | 1x API 调用/Turn |
| 可控性 | 框架完全控制 | 依赖模型行为 |
| 普适性 | 适用于任何模型 | 仅适用于推理模型 |

**优势**：harness9 不依赖特定模型能力，任何支持 function calling 的模型都能使用两阶段机制。OpenAI SDK 的推理能力仅限于 o1/o3 系列模型。

**劣势**：harness9 的双 API 调用成本更高（每个 Turn 翻倍），且增加了延迟。

#### 与 Claude Agent SDK 的对比

| 维度 | harness9 | Claude SDK |
|------|----------|-----------|
| 思考位置 | 框架层面（两次 API 调用） | API 层面（单次调用内的 thinking block） |
| 可见性 | 完全可见（打印到终端） | 部分可见（thinking content block） |
| 循环控制 | 完全可控 | 黑盒 |
| 工具剥夺 | ✅ nil tools | ❌ 不支持 |

**优势**：harness9 的思考结果作为独立的 assistant 消息注入上下文，Phase 2 可以明确看到 Phase 1 的规划内容。Claude 的 Extended Thinking 内容不一定总是被模型的行动阶段显式参考。

**劣势**：Claude 的 Extended Thinking 不需要额外 API 调用，且思考 token 通常以折扣价格计费。harness9 则需要完整的一次额外 API 调用。

#### 与 DeepAgents 的对比

| 维度 | harness9 | DeepAgents |
|------|----------|-----------|
| Planning 机制 | 框架强制阶段 | 可选工具 |
| 执行保证 | 每个 Turn 必经 Thinking | 模型可能跳过规划 |
| 工具剥夺 | 完全剥夺 | 无 |
| 灵活性 | 低（固定两阶段） | 高（模型自主决策） |

**优势**：harness9 保证了每个 Turn 都有思考阶段，避免模型在复杂任务中"跳过规划直接行动"。DeepAgents 的 `write_todos` 工具可能被模型忽略。

**劣势**：DeepAgents 的方式更灵活 — 模型可以根据任务复杂度自行决定是否需要规划，简单任务不会浪费时间和成本在思考阶段。

#### 与 OpenCode 的对比

| 维度 | harness9 | OpenCode |
|------|----------|---------|
| 分离粒度 | Turn 内（同一上下文） | Agent 级（不同配置） |
| 自动化 | 自动每 Turn 执行 | 用户手动切换（Tab） |
| 工具剥夺 | 完全剥夺 | 部分限制（禁止写操作） |
| 适用场景 | 所有任务 | 人工辅助开发 |

**优势**：harness9 的自动化机制不需要用户干预。OpenCode 的 Plan 模式需要用户主动切换。

**劣势**：OpenCode 的模式切换给了用户更多控制权 — 用户可以在不需要规划时使用 Build 模式提高效率。

### 4.4 优劣势综合分析

#### harness9 Two-Stage ReAct 的核心优势

1. **模型无关性**：不依赖特定模型的推理能力，任何支持 function calling 的模型都可以使用
2. **强制规划**：通过剥夺工具确保模型必须先思考再行动，避免冲动式工具调用
3. **完全可观测**：思考内容作为独立消息注入上下文，开发者可以完全看到模型的推理过程
4. **明确分离**：Thinking 和 Action 的职责清晰，便于调试和优化
5. **上下文连贯**：Phase 1 的思考结果作为 assistant 消息注入，Phase 2 可以显式参考

#### harness9 Two-Stage ReAct 的已知局限

1. **成本翻倍**：每个 Turn 多一次完整的 LLM API 调用
2. **延迟增加**：Phase 1 的 API 调用增加了每 Turn 的响应时间
3. **上下文消耗**：Phase 1 的思考内容作为 assistant 消息占用上下文窗口
4. **缺乏灵活性**：所有 Turn 都执行两阶段，无法根据任务复杂度动态调整
5. **模型适应性**：某些模型可能在无工具时产生低质量或不相关的思考内容

---

## 5. 设计模式提炼

### 5.1 模式一：框架级强制两阶段（harness9 独有）

**适用场景**：需要确保模型在每个 Turn 都进行充分思考的场景

```
每 Turn:
  Phase 1: Generate(ctx, history, tools=nil)     → 纯推理
  Phase 2: Generate(ctx, history + thinking, tools=all) → 行动
```

**Trade-off**：
- ✅ 保证思考质量
- ❌ 2x API 调用成本

### 5.2 模式二：模型级内置推理（OpenAI/Claude）

**适用场景**：使用支持内置推理的模型（o1/o3/Claude with Extended Thinking）

```
每 Turn:
  Generate(ctx, history, tools=all, reasoning_config) → 模型内部先推理再行动
```

**Trade-off**：
- ✅ 无额外 API 调用
- ❌ 依赖特定模型能力

### 5.3 模式三：工具级可选规划（DeepAgents）

**适用场景**：模型自主决定是否需要规划

```
每 Turn:
  Generate(ctx, history, tools=[..., write_todos, ...]) → 模型选择是否调用规划工具
```

**Trade-off**：
- ✅ 灵活、低成本
- ❌ 模型可能跳过规划

### 5.4 模式四：Agent 模式切换（OpenCode）

**适用场景**：需要人工介入决定是否进入规划模式

```
Plan Mode: Generate(ctx, history, tools=read_only)
Build Mode: Generate(ctx, history, tools=all)
用户通过 Tab 键手动切换
```

**Trade-off**：
- ✅ 用户完全控制
- ❌ 需要人工介入

### 5.5 模式五：自适应思考（推荐增强方向）

**结合 harness9 现有机制的增强方向**：

```
每 Turn:
  if task_complexity > threshold:
    Phase 1: Generate(ctx, history, tools=nil)     → 深度思考
    Phase 2: Generate(ctx, history + thinking, tools=all) → 行动
  else:
    Single: Generate(ctx, history, tools=all)      → 直接行动
```

**实现建议**：

```go
// 自适应思考策略
type ThinkingStrategy int

const (
    ThinkingAlways    ThinkingStrategy = iota  // 每个 Turn 都思考
    ThinkingNever                               // 从不思考
    ThinkingAdaptive                            // 根据复杂度自适应
)

func (e *AgentEngine) shouldThink(history []schema.Message) bool {
    switch e.thinkingStrategy {
    case ThinkingAlways:
        return true
    case ThinkingNever:
        return false
    case ThinkingAdaptive:
        return e.estimateComplexity(history) > e.thinkingThreshold
    }
    return false
}
```

---

## 6. 对 harness9 的设计建议

### 6.1 短期优化建议

#### 6.1.1 思考内容标签化

当前 Phase 1 的思考结果作为普通 `assistant` 消息注入，建议增加标签以区分：

```go
// Phase 1 结果使用特殊 Role 或元数据标记
if thinkResp.Content != "" {
    thinkMsg := schema.Message{
        Role:    schema.RoleAssistant,
        Content: fmt.Sprintf("<thinking>\n%s\n</thinking>", thinkResp.Content),
        // 或使用元数据标记
        Meta:    map[string]string{"phase": "thinking"},
    }
    contextHistory = append(contextHistory, thinkMsg)
}
```

#### 6.1.2 思考阶段 System Prompt 增强

为 Phase 1 提供专门的 System Prompt，引导模型进行结构化思考：

```go
thinkingSystemPrompt := `You are in THINKING mode. You have NO tools available.
Your job is to:
1. Analyze the current situation
2. Identify what information you need
3. Plan your next steps
4. Consider potential pitfalls

Output a structured plan in markdown format.`

// Phase 1 使用专用 system prompt
thinkResp, err := e.provider.Generate(ctx, thinkingMessages, nil)
```

#### 6.1.3 思考内容长度控制

避免 Phase 1 消耗过多上下文窗口：

```go
const maxThinkingTokens = 500

// 通过 Provider 的 max_tokens 参数控制 Phase 1 的输出长度
// 或在注入前进行截断
if len(thinkResp.Content) > maxThinkingChars {
    thinkResp.Content = thinkResp.Content[:maxThinkingChars] + "\n[...truncated]"
}
```

### 6.2 中期架构增强

#### 6.2.1 自适应思考策略

参考 DeepAgents 和 OpenCode 的设计，增加灵活性：

```go
type ThinkingStrategy interface {
    ShouldThink(ctx context.Context, history []schema.Message, turnCount int) bool
}

// 实现
type AlwaysThink struct{}
func (s *AlwaysThink) ShouldThink(...) bool { return true }

type NeverThink struct{}
func (s *NeverThink) ShouldThink(...) bool { return false }

type AdaptiveThink struct {
    Threshold float64
}
func (s *AdaptiveThink) ShouldThink(..., history []schema.Message) bool {
    // 基于上下文长度、Turn 数量、工具调用频率等判断
    return estimateComplexity(history) > s.Threshold
}
```

#### 6.2.2 思考内容压缩

参考 OpenHarness 的 compaction 机制，对 Phase 1 的长思考内容进行压缩：

```go
func (e *AgentEngine) compressThinking(content string) string {
    if len(content) <= maxThinkingChars {
        return content
    }
    // 选项1: 简单截断
    // 选项2: 提取关键信息（如使用正则提取计划步骤）
    // 选项3: 使用 LLM 进行摘要（成本更高但质量更好）
    return content[:maxThinkingChars]
}
```

#### 6.2.3 思考结果传递方式优化

当前思考结果作为完整 `assistant` 消息注入，建议支持多种传递方式：

```go
type ThinkingInjectionMode int

const (
    // 作为 assistant 消息注入（当前方式）
    InjectAsAssistantMessage ThinkingInjectionMode = iota
    // 作为 system 消息注入（不占用 assistant 轮次）
    InjectAsSystemHint
    // 不注入上下文，仅用于日志（Phase 2 完全自主）
    InjectAsLogOnly
)
```

### 6.3 长期演进方向

#### 6.3.1 与模型端推理能力协同

当使用的模型支持 Extended Thinking（如 Claude）或 Reasoning Models（如 o3）时，可以自动降级框架级两阶段，利用模型端的推理能力：

```go
func (e *AgentEngine) Run(ctx context.Context, userPrompt string) error {
    // 检测模型是否支持内置推理
    if e.provider.SupportsBuiltInReasoning() {
        // 使用模型端推理，跳过框架级两阶段
        return e.runWithModelReasoning(ctx, userPrompt)
    }
    // 降级到框架级两阶段
    return e.runWithTwoStageReAct(ctx, userPrompt)
}
```

#### 6.3.2 思考缓存

对于相似的历史上下文，缓存 Phase 1 的思考结果：

```go
func (e *AgentEngine) getThinking(ctx context.Context, history []schema.Message) (*schema.Message, error) {
    // 计算上下文指纹
    fingerprint := hashHistory(history)

    // 检查缓存
    if cached, ok := e.thinkingCache.Get(fingerprint); ok {
        return cached, nil
    }

    // 执行 Phase 1
    thinkResp, err := e.provider.Generate(ctx, history, nil)
    if err != nil {
        return nil, err
    }

    // 写入缓存
    e.thinkingCache.Set(fingerprint, thinkResp)
    return thinkResp, nil
}
```

---

## 附录 A：框架源码路径索引

| 框架 | Agent Loop 核心文件 | 思考/规划相关 |
|------|---------------------|---------------|
| harness9 | `internal/engine/agent_loop.go` | Phase 1/Phase 2 两阶段 |
| OpenAI SDK | `src/agents/run.py` → `AgentRunner.run()` | `ReasoningItemIdPolicy` |
| Claude SDK | 内嵌二进制 | Extended Thinking（API 层面） |
| HermesAgent | `run_agent.py` → `run_conversation()` | `reasoning_callback` + `scratchpad` |
| OpenHarness | `src/openharness/engine/query.py` | `enter_plan_mode`/`exit_plan_mode` 工具 |
| DeepAgents | `libs/deepagents/graph.py` | `TodoListMiddleware` + `write_todos` 工具 |
| OpenCode | `packages/opencode/src/session/session.ts` | `plan` Agent 模式 |
| OpenClaw | `src/` Gateway + Vercel AI SDK | `/think` 命令 |

## 附录 B：关键术语对照

| harness9 术语 | 通用术语 | 说明 |
|--------------|---------|------|
| Phase 1 (Thinking) | Pre-planning / Inner Monologue | 无工具的纯推理阶段 |
| Phase 2 (Action) | Tool Calling / Action | 带工具的行动阶段 |
| nil tools | Tool Stripping / Tool Deprivation | 传入空工具列表 |
| EnableThinking | Thinking Budget / Reasoning Toggle | 控制是否启用思考 |
| contextHistory | Conversation History / Message Log | 共享的对话上下文 |

## 附录 C：推荐阅读顺序

1. **本文第 2 节** — 理解 harness9 的 Two-Stage ReAct 实现
2. **本文第 3.2 节**（Claude Agent SDK）— 理解 Extended Thinking 作为替代方案
3. **本文第 3.5 节**（DeepAgents）— 理解工具级规划作为替代方案
4. **本文第 5 节** — 理解五种设计模式的 Trade-off
5. **本文第 6 节** — 对 harness9 的具体设计建议
