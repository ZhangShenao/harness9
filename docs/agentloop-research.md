# AgentLoop (ReAct Loop / MainLoop) 核心实现原理与设计模式

## 深度调研报告

> 调研日期：2026-04-20  
> 调研范围：7 个主流 Agent Harness 框架的 AgentLoop 实现  
> 目标：为 harness9 项目的 `internal/engine/engine.go` 设计提供参考

---

## 目录

1. [调研概述](#1-调研概述)
2. [各框架 AgentLoop 实现分析](#2-各框架-agentloop-实现分析)
3. [横向对比总结](#3-横向对比总结)
4. [设计模式提炼](#4-设计模式提炼)
5. [对 harness9 的设计建议](#5-对-harness9-的设计建议)

---

## 1. 调研概述

### 1.1 什么是 AgentLoop

AgentLoop 是 Agent Harness 框架的核心运行时引擎。它实现了一个反复迭代的推理-行动循环（ReAct Loop），让 LLM 能够：

1. **接收输入** → 理解用户意图和当前上下文
2. **推理（Reason）** → 调用 LLM 生成响应
3. **行动（Act）** → 解析工具调用请求并执行工具
4. **观察（Observe）** → 将工具执行结果注入上下文
5. **迭代** → 回到步骤 2，直到任务完成

### 1.2 调研框架一览

| 框架 | 来源 | 语言 | AgentLoop 核心文件 | 循环风格 |
|------|------|------|---------------------|----------|
| **DeepAgents** | LangChain | Python | `libs/deepagents/graph.py` → 委托 LangGraph | 图编排（StateGraph） |
| **OpenHarness** | HKUDS | Python | `src/openharness/engine/query.py` | 显式 while 循环 |
| **OpenCode** | Anomaly | TypeScript | `packages/opencode/src/session/session.ts` + Vercel AI SDK | 委托 AI SDK streamText |
| **OpenClaw** | OpenClaw | TypeScript | `src/` 下 Gateway 路由 + Vercel AI SDK | 委托 AI SDK streamText |
| **HermesAgent** | NousResearch | Python | `run_agent.py` → `run_conversation()` | 显式 while 循环 |
| **Claude Agent SDK** | Anthropic | Python/TS | 内嵌 Claude Code 二进制，通过 `query()` 异步迭代器暴露 | 黑盒循环 + 流式异步迭代器 |
| **OpenAI Agent SDK** | OpenAI | Python/TS | `src/agents/run.py` → `AgentRunner.run()` | 显式 while True 循环 |

### 1.3 核心发现

所有框架的 AgentLoop 遵循相同的基本模式，但在以下维度存在显著差异：

- **循环控制权**：显式 while 循环 vs 委托图引擎 vs 黑盒
- **终止条件**：stop_reason 检测、max_turns 限制、用户中断
- **并行工具执行**：仅 OpenHarness 和 HermesAgent 实现了真正的并发工具调用
- **上下文管理**：auto-compaction（OpenHarness/HermesAgent）vs 依赖模型 API（OpenAI Agent SDK）
- **错误恢复**：从简单的 try/catch 到 reactive compaction + 断点续跑

---

## 2. 各框架 AgentLoop 实现分析

### 2.1 OpenHarness — 最清晰的显式循环实现

**核心文件**：`src/openharness/engine/query.py`（`run_query` 函数）

**循环结构**：

```python
# src/openharness/engine/query.py - run_query()

async def run_query(context, messages):
    turn_count = 0
    while context.max_turns is None or turn_count < context.max_turns:
        turn_count += 1
        
        # 1. Auto-compact check before calling the model
        async for event, usage in _stream_compaction(trigger="auto"):
            yield event, usage
        
        # 2. Stream LLM response
        async for event in context.api_client.stream_message(request):
            if isinstance(event, ApiTextDeltaEvent):
                yield AssistantTextDelta(text=event.text), None
            elif isinstance(event, ApiMessageCompleteEvent):
                final_message = event.message
        
        # 3. Append assistant message to history
        messages.append(final_message)
        yield AssistantTurnComplete(message=final_message), usage
        
        # 4. Check termination: no tool calls → done
        if not final_message.tool_uses:
            return
        
        # 5. Execute tools (single or parallel)
        if len(tool_calls) == 1:
            # Sequential for single tool
            result = await _execute_tool_call(context, tc.name, tc.id, tc.input)
        else:
            # Parallel for multiple tools
            raw_results = await asyncio.gather(
                *[_run(tc) for tc in tool_calls], 
                return_exceptions=True
            )
        
        # 6. Append tool results as user message
        messages.append(ConversationMessage(role="user", content=tool_results))
    
    raise MaxTurnsExceeded(context.max_turns)
```

**关键设计特点**：

| 维度 | 实现 |
|------|------|
| **循环控制** | `while max_turns` 显式循环 |
| **终止条件** | `final_message.tool_uses` 为空时返回；超过 `max_turns` 抛异常 |
| **流式输出** | 通过 `AsyncIterator[tuple[StreamEvent, UsageSnapshot]]` 逐事件 yield |
| **并行工具** | `asyncio.gather` + `return_exceptions=True`（确保单个失败不影响其他工具） |
| **上下文管理** | 每个 turn 开始前检查 auto-compact；prompt-too-long 时触发 reactive compact |
| **错误处理** | API 调用异常 → 检测是否 context-too-long → reactive compact → 重试 |
| **权限控制** | `_execute_tool_call` 中调用 `permission_checker.evaluate()`，支持交互式确认 |
| **Hook 系统** | PreToolUse / PostToolUse / Stop / Notification 事件钩子 |

**上下文压缩**（Compaction）：

```python
# 在每个 turn 开始前
async for event, usage in _stream_compaction(trigger="auto"):
    yield event, usage

# API 调用失败时（reactive compaction）
if not reactive_compact_attempted and _is_prompt_too_long_error(exc):
    reactive_compact_attempted = True
    async for event, usage in _stream_compaction(trigger="reactive", force=True):
        yield event, usage
    if was_compacted:
        continue  # 重试当前 turn
```

### 2.2 HermesAgent — 最完整的 Python 实现

**核心文件**：`run_agent.py` → `AIAgent.run_conversation()`

**循环结构**（简化版）：

```python
# run_agent.py - AIAgent.run_conversation() 核心循环

def run_conversation(self, user_input, ...):
    messages = [{"role": "user", "content": user_input}]
    
    while self.iteration_budget.consume():
        # 1. Call LLM (supports multiple API modes)
        response = self._call_api(messages, tools, ...)
        
        # 2. Extract tool calls from response
        tool_calls = response.choices[0].message.tool_calls
        
        # 3. Check termination
        if not tool_calls:
            return response.choices[0].message.content
        
        # 4. Execute tools (parallel or sequential)
        if _should_parallelize_tool_batch(tool_calls):
            # Concurrent execution in ThreadPoolExecutor
            results = self._execute_tool_calls_concurrent(tool_calls)
        else:
            results = self._execute_tool_calls_sequential(tool_calls)
        
        # 5. Append assistant message + tool results
        messages.append(response.choices[0].message)
        for result in results:
            messages.append({
                "role": "tool",
                "tool_call_id": result.id,
                "content": result.output
            })
        
        # 6. Context compression if needed
        if estimated_tokens > context_limit * threshold:
            messages = self._compress_context(messages)
```

**关键设计特点**：

| 维度 | 实现 |
|------|------|
| **循环控制** | `IterationBudget` 线程安全计数器（parent+subagent 共享预算） |
| **API 模式** | 支持 `chat_completions`、`codex_responses`、`anthropic_messages`、`bedrock_converse` 四种 API 模式 |
| **并行工具** | `ThreadPoolExecutor` + 路径冲突检测（`_should_parallelize_tool_batch`） |
| **上下文压缩** | `ContextCompressor` 支持 micro-compact（清除旧 tool 输出）和 LLM-based summarize |
| **中断机制** | `_interrupt_requested` + `_pending_steer`（注入式引导，不中断循环） |
| **子代理** | `delegate` 工具创建独立 `AIAgent` 实例，共享 `IterationBudget` |
| **错误恢复** | `jittered_backoff` 指数退避 + 自动 failover 到备用模型 |
| **Token 计费** | 精确的 token 计数和成本追踪 |

**并行工具调用的智能调度**：

```python
# run_agent.py - _should_parallelize_tool_batch()

def _should_parallelize_tool_batch(tool_calls) -> bool:
    """判断一批工具调用是否可以安全并行执行"""
    if len(tool_calls) <= 1:
        return False
    # 交互式工具必须串行
    if any(name in _NEVER_PARALLEL_TOOLS for name in tool_names):
        return False
    # 路径冲突检测（同文件的读写不能并行）
    for tool_call in tool_calls:
        if tool_name in _PATH_SCOPED_TOOLS:
            if any(_paths_overlap(scoped_path, existing) for existing in reserved_paths):
                return False
    # 只读工具 + 无冲突的路径工具可以并行
    return all(name in _PARALLEL_SAFE_TOOLS or name in _PATH_SCOPED_TOOLS 
               for name in tool_names)
```

### 2.3 OpenAI Agent SDK — 最企业级的实现

**核心文件**：`src/agents/run.py` → `AgentRunner.run()`

**循环结构**：

```python
# src/agents/run.py - AgentRunner.run()

async def run(self, starting_agent, input, *, max_turns=DEFAULT_MAX_TURNS, ...):
    current_agent = starting_agent
    current_turn = 0
    
    while True:
        current_turn += 1
        
        # 1. Check max_turns
        if current_turn > max_turns:
            raise MaxTurnsExceeded(f"Max turns ({max_turns}) exceeded")
        
        # 2. Run input guardrails (first turn only)
        if current_turn == 1:
            await run_input_guardrails(...)
        
        # 3. Call model
        turn_result = await run_single_turn(
            current_agent, original_input, ...
        )
        
        # 4. Process result based on next step type
        if isinstance(turn_result.next_step, NextStepFinalOutput):
            # Final answer → run output guardrails → return
            await run_output_guardrails(...)
            return RunResult(final_output=turn_result.next_step.output)
            
        elif isinstance(turn_result.next_step, NextStepHandoff):
            # Switch to new agent → continue loop
            current_agent = turn_result.next_step.new_agent
            continue
            
        elif isinstance(turn_result.next_step, NextStepInterruption):
            # Human approval needed → pause & return state
            return build_interruption_result(run_state=...)
            
        elif isinstance(turn_result.next_step, NextStepRunAgain):
            # Tool calls executed → continue loop
            continue
```

**关键设计特点**：

| 维度 | 实现 |
|------|------|
| **循环控制** | `while True` + 显式 `NextStep*` 类型区分 |
| **终止条件** | `NextStepFinalOutput`（模型生成最终答案）、`MaxTurnsExceeded`（超过限制） |
| **Handoff** | `NextStepHandoff` 切换 agent 但不终止循环 |
| **中断恢复** | `RunState` 可序列化，支持从 interruption 处恢复（`RunState` 作为 input 传入） |
| **Guardrail** | Input guardrails（首轮执行）+ Output guardrails（最终输出前执行）+ Tool guardrails |
| **会话管理** | 支持 `session`（本地持久化）、`conversationId`（OpenAI Conversations API）、`previousResponseId`（Responses API）四种策略 |
| **Sandbox** | 内置 `SandboxRuntime` 支持 Docker 容器化执行 |
| **Tracing** | 完整的 OpenTelemetry span 追踪（agent_span / turn_span / task_span） |

**RunState 断点续跑**：

```python
# 支持从断点恢复
result = await Runner.run(agent, RunState(...))  # 从中断处继续
```

### 2.4 DeepAgents (LangChain) — 图编排模式

**核心文件**：`libs/deepagents/graph.py` → `create_deep_agent()`

**设计理念**：不直接实现循环，而是通过 LangGraph 的 `CompiledStateGraph` 进行图编排。

```python
# DeepAgents 不直接写 while 循环，而是组装 LangGraph 图

def create_deep_agent(model, tools, *, system_prompt, middleware, ...):
    # 组装 middleware 栈
    deepagent_middleware = [
        TodoListMiddleware(),
        SkillsMiddleware(...),
        FilesystemMiddleware(...),
        SubAgentMiddleware(...),
        create_summarization_middleware(model, backend),
        PatchToolCallsMiddleware(),
        # ... 更多 middleware
    ]
    
    # 委托 LangGraph create_agent 创建图
    return create_agent(
        model,
        system_prompt=final_system_prompt,
        tools=_tools,
        middleware=deepagent_middleware,
        ...
    ).with_config({"recursion_limit": 9_999})
```

**关键设计特点**：

| 维度 | 实现 |
|------|------|
| **循环控制** | 委托 LangGraph 的图引擎，`recursion_limit: 9_999` |
| **Middleware 栈** | 洋葱模型：请求从外到内穿过每层 middleware |
| **SubAgent** | `SubAgentMiddleware` + `AsyncSubAgentMiddleware` 管理子代理 |
| **Summarization** | `create_summarization_middleware` 自动压缩长上下文 |
| **权限** | `_PermissionMiddleware` 必须是最后一层（看到所有工具） |
| **Prompt 缓存** | `AnthropicPromptCachingMiddleware` 对非 Anthropic 模型静默跳过 |

### 2.5 OpenCode — TypeScript + Effect 生态

**核心文件**：`packages/opencode/src/session/session.ts` + Vercel AI SDK

**设计理念**：使用 Effect.ts 函数式编程框架 + Vercel AI SDK 的 `streamText` 函数。

```typescript
// OpenCode 的 Agent 定义（不是 loop 本身）
export const Info = z.object({
    name: z.string(),
    mode: z.enum(["subagent", "primary", "all"]),
    permission: Permission.Ruleset.zod,
    steps: z.number().int().positive().optional(),  // max turns
    // ...
})
```

**关键设计特点**：

| 维度 | 实现 |
|------|------|
| **循环控制** | 委托 Vercel AI SDK 的 `streamText` / `generateText`，通过 `maxSteps` 控制循环次数 |
| **Agent 模式** | `build`（主代理）、`plan`（只读）、`general`（子代理）、`explore`（搜索） |
| **Effect 生态** | 全面使用 `Effect`、`Layer`、`Context` 进行依赖注入和状态管理 |
| **权限系统** | 细粒度的 `Permission.Ruleset`，支持 `allow/deny/ask` 三种动作 |
| **工具定义** | `Tool.Def` 接口：`id` + `parameters` (Zod schema) + `execute` (Effect) |
| **输出截断** | 内置 `Truncate` 服务，自动截断过长的工具输出 |

### 2.6 OpenClaw — 全平台个人助手

**核心文件**：TypeScript，基于 Vercel AI SDK

**设计理念**：Gateway 架构，Agent Loop 在 Gateway 进程中运行。

```typescript
// OpenClaw 使用 Vercel AI SDK 的 streamText
// Loop 本身由 AI SDK 的 maxSteps 参数控制
const result = await streamText({
    model: model,
    messages: messages,
    tools: tools,
    maxSteps: agent.steps,
    onStepFinish: async (step) => { /* 更新 session */ },
})
```

**关键设计特点**：

| 维度 | 实现 |
|------|------|
| **循环控制** | Vercel AI SDK `maxSteps` 参数 |
| **多渠道** | WhatsApp、Telegram、Slack、Discord 等 20+ 渠道 |
| **多代理路由** | 支持根据渠道/账户路由到不同的 agent |
| **Compaction** | `/compact` 命令 + 自动上下文压缩 |
| **沙箱** | Docker 容器化执行非 main session |

### 2.7 Claude Agent SDK — 黑盒循环

**核心文件**：内嵌 Claude Code 二进制，通过异步迭代器暴露

```python
# Claude Agent SDK 的 API 设计
async for message in query(
    prompt="Find and fix the bug",
    options=ClaudeAgentOptions(allowed_tools=["Read", "Edit", "Bash"]),
):
    if isinstance(message, AssistantMessage):
        # 处理助手消息
    elif isinstance(message, ResultMessage):
        # 循环结束，获取最终结果
```

**关键设计特点**：

| 维度 | 实现 |
|------|------|
| **循环控制** | 黑盒（内嵌 Claude Code 二进制），用户无法控制循环细节 |
| **终止条件** | 模型不再调用工具或达到内部限制 |
| **流式输出** | `async for message in query()` 异步迭代器 |
| **权限模式** | `acceptEdits` / `dontAsk` / `auto` / `bypassPermissions` |
| **Hook** | `PreToolUse` / `PostToolUse` / `Stop` / `SessionStart` 等回调 |
| **子代理** | `AgentDefinition` 定义子代理，通过 `Agent` 工具调用 |
| **会话** | `session_id` 支持跨调用上下文保持 |

---

## 3. 横向对比总结

### 3.1 AgentLoop 核心流程对比

```
┌─────────────────────────────────────────────────────────────────────┐
│                    通用 AgentLoop 流程                               │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  ┌──────────┐    ┌──────────┐    ┌──────────┐    ┌──────────┐      │
│  │ 接收输入  │───▶│ 调用 LLM  │───▶│ 检查输出  │───▶│ 有工具调用?│     │
│  └──────────┘    └──────────┘    └──────────┘    └────┬─┬───┘      │
│       ▲                                              │ │            │
│       │                                         否 ──┘ │ ── 是      │
│       │                                              ▼   ▼          │
│       │                                        ┌──────────┐        │
│       │                                        │ 返回结果  │        │
│       │                                        └──────────┘        │
│       │                                              │              │
│       ▼                                              │              │
│  ┌──────────┐    ┌──────────┐                        │              │
│  │ 注入结果  │◀───│ 执行工具  │◀───────────────────────┘              │
│  └──────────┘    └──────────┘                                       │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

### 3.2 关键维度对比表

| 维度 | OpenHarness | HermesAgent | OpenAI SDK | DeepAgents | OpenCode | OpenClaw | Claude SDK |
|------|:-----------:|:-----------:|:----------:|:----------:|:--------:|:--------:|:----------:|
| **循环实现** | 显式 while | 显式 while | 显式 while True | 图编排 | AI SDK 委托 | AI SDK 委托 | 黑盒 |
| **默认 max_turns** | 8 | 90 | 25 | 9,999 | 可配置 | 可配置 | 内部 |
| **并行工具** | ✅ asyncio.gather | ✅ ThreadPool | ❌ 顺序 | ❌ 图限制 | ✅ AI SDK | ✅ AI SDK | ❌ 黑盒 |
| **流式输出** | ✅ AsyncIterator | ✅ callback | ✅ StreamEvents | ✅ LangGraph | ✅ AI SDK | ✅ AI SDK | ✅ AsyncIterator |
| **上下文压缩** | ✅ auto+reactive | ✅ 三级压缩 | ✅ Compaction API | ✅ summarization | ✅ 专用 agent | ✅ /compact | ✅ 黑盒 |
| **错误恢复** | ✅ reactive compact | ✅ backoff+failover | ✅ RunState 恢复 | ✅ checkpoint | ⚠️ 基础 | ⚠️ 基础 | ❌ 黑盒 |
| **Handoff** | ❌ | ✅ delegate 工具 | ✅ NextStepHandoff | ✅ SubAgent | ✅ subagent | ✅ 多代理路由 | ✅ AgentDefinition |
| **权限系统** | ✅ 多级+路径规则 | ✅ allowlist | ✅ guardrails+approval | ✅ Permission MW | ✅ Ruleset | ✅ 沙箱模式 | ✅ 多种模式 |
| **Hook 系统** | ✅ Pre/Post Tool | ✅ callback 体系 | ✅ RunHooks | ✅ Middleware | ✅ Plugin | ✅ Plugin | ✅ HookMatcher |
| **中断/恢复** | ✅ continue_pending | ✅ interrupt+steer | ✅ RunState | ✅ checkpoint | ⚠️ 基础 | ⚠️ 基础 | ✅ session_id |

### 3.3 终止条件对比

| 框架 | 主要终止条件 | 次要终止条件 |
|------|-------------|-------------|
| **OpenHarness** | `stop_reason != "tool_use"` | `MaxTurnsExceeded` 异常 |
| **HermesAgent** | 无 tool_calls 或 finish_reason != tool_calls | `IterationBudget` 耗尽；用户 `/stop` |
| **OpenAI SDK** | `NextStepFinalOutput` | `MaxTurnsExceeded` 异常；`NextStepInterruption`（暂停） |
| **DeepAgents** | LangGraph 图到达 END 节点 | `recursion_limit`（9,999） |
| **OpenCode** | AI SDK `maxSteps` 耗尽 | 模型无工具调用 |
| **OpenClaw** | AI SDK `maxSteps` 耗尽 | 模型无工具调用 |
| **Claude SDK** | `ResultMessage` 事件 | 内部限制 |

### 3.4 上下文窗口管理对比

| 策略 | 框架 | 实现方式 |
|------|------|----------|
| **Auto-Compact** | OpenHarness | 每个 turn 前检查 token 估算，超阈值自动触发 LLM summarize |
| **Reactive Compact** | OpenHarness | API 返回 prompt-too-long 错误时强制压缩后重试 |
| **Micro-Compact** | HermesAgent | 清除旧工具输出内容（保留结构） |
| **LLM Summarize** | HermesAgent | 用 LLM 生成历史摘要替换旧消息 |
| **Compaction API** | OpenAI SDK | 使用 OpenAI 的 `/compaction` 服务端 API |
| **Summarization MW** | DeepAgents | `SummarizationMiddleware` 中间件 |
| **专用 Agent** | OpenCode | 用 `compaction` agent 专门做上下文压缩 |

---

## 4. 设计模式提炼

### 4.1 模式一：显式 While 循环（推荐）

**适用场景**：需要完全控制循环逻辑的框架（如 harness9）

```
while max_turns not exceeded:
    1. [可选] 上下文压缩检查
    2. 调用 LLM（流式/非流式）
    3. 检查终止条件
    4. 解析工具调用
    5. 执行工具（串行/并行）
    6. 注入工具结果到消息历史
```

**代表框架**：OpenHarness、HermesAgent、OpenAI Agent SDK

### 4.2 模式二：洋葱 Middleware 栈

**适用场景**：需要在循环的各个阶段插入横切关注点

```
Request → MW1 → MW2 → MW3 → [Core Loop] → MW3 → MW2 → MW1 → Response
```

**关键规则**：
- 权限中间件必须在最外层（最后执行，看到所有工具）
- 缓存中间件在权限之前（避免缓存未授权的结果）
- 摘要中间件在工具中间件之后（能看到完整的工具输出）

**代表框架**：DeepAgents

### 4.3 模式三：NextStep 状态机

**适用场景**：需要在循环中处理复杂的分支逻辑（handoff、中断、恢复）

```python
match next_step:
    case NextStepFinalOutput(output):
        return result
    case NextStepHandoff(new_agent):
        current_agent = new_agent
        continue
    case NextStepInterruption(approvals):
        return pause_with_state()
    case NextStepRunAgain():
        continue
```

**代表框架**：OpenAI Agent SDK

### 4.4 模式四：异步迭代器流式输出

**适用场景**：需要在循环执行过程中实时向前端推送事件

```python
async def run_query(context, messages) -> AsyncIterator[tuple[StreamEvent, Usage]]:
    while ...:
        # LLM 推理事件
        yield AssistantTextDelta(text=chunk), None
        # 工具执行事件
        yield ToolExecutionStarted(tool_name=name), None
        yield ToolExecutionCompleted(tool_name=name, output=result), None
        # Turn 完成事件
        yield AssistantTurnComplete(message=msg, usage=usage), usage
```

**代表框架**：OpenHarness、Claude Agent SDK

### 4.5 模式五：并行工具执行 + 异常隔离

**适用场景**：模型返回多个工具调用时需要高效执行

```python
if len(tool_calls) > 1:
    # 并行执行，单个失败不影响其他工具
    raw_results = await asyncio.gather(
        *[_run(tc) for tc in tool_calls],
        return_exceptions=True  # 关键：异常不传播
    )
    # 将异常转换为 ToolResultBlock(is_error=True)
    for tc, result in zip(tool_calls, raw_results):
        if isinstance(result, BaseException):
            tool_results.append(ToolResultBlock(
                content=f"Tool {tc.name} failed: {result}",
                is_error=True
            ))
```

**关键约束**（来自 HermesAgent）：
- 交互式工具（如 `clarify`）必须串行
- 同一路径的文件操作不能并行（路径冲突检测）
- 只读工具 + 无冲突的文件工具可以并行

**代表框架**：OpenHarness、HermesAgent

### 4.6 模式六：双层上下文压缩

**适用场景**：长期运行的 Agent 会话

```
第一层：Micro-Compact（低成本）
    → 清除旧的工具输出内容，保留消息结构
    → 适用于略微超限的情况

第二层：LLM Summarize（高成本）
    → 用 LLM 生成历史摘要，替换多条旧消息
    → 适用于严重超限的情况

第三层：Reactive Compact（紧急恢复）
    → API 报错 prompt-too-long 时强制触发
    → 保证不会因上下文过长而完全失败
```

**代表框架**：OpenHarness、HermesAgent

---

## 5. 对 harness9 的设计建议

基于以上调研，为 harness9 的 `internal/engine/engine.go` 提出以下设计建议：

### 5.1 推荐 Loop 结构

```go
// internal/engine/engine.go

func (e *Engine) RunLoop(ctx context.Context, input Message) (<-chan Event, error) {
    eventCh := make(chan Event)
    
    go func() {
        defer close(eventCh)
        
        turnCount := 0
        
        for {
            select {
            case <-ctx.Done():
                eventCh <- ErrorEvent{Err: ctx.Err()}
                return
            default:
            }
            
            // 检查 max turns
            turnCount++
            if e.maxTurns > 0 && turnCount > e.maxTurns {
                eventCh <- ErrorEvent{Err: ErrMaxTurnsExceeded}
                return
            }
            
            // 1. 上下文压缩检查
            if e.shouldCompact() {
                if err := e.compact(ctx); err != nil {
                    eventCh <- StatusEvent{Message: "compaction failed"}
                }
            }
            
            // 2. 调用 LLM（流式）
            response, err := e.provider.Stream(ctx, e.buildRequest())
            if err != nil {
                // 检测 context-too-long → reactive compact → 重试
                if isContextTooLong(err) {
                    e.compact(ctx)  // 强制压缩
                    continue        // 重试当前 turn
                }
                eventCh <- ErrorEvent{Err: err}
                return
            }
            
            // 3. 流式输出 LLM 响应
            var finalMsg Message
            for chunk := range response.Stream() {
                eventCh <- TextDeltaEvent{Text: chunk.Text}
                finalMsg = chunk.Accumulate()
            }
            
            // 4. 追加助手消息到历史
            e.history.Append(finalMsg)
            eventCh <- TurnCompleteEvent{Message: finalMsg}
            
            // 5. 检查终止条件
            if len(finalMsg.ToolCalls) == 0 {
                return  // 模型没有更多工具调用，循环结束
            }
            
            // 6. 执行工具（并行或串行）
            results := e.executeTools(ctx, finalMsg.ToolCalls)
            
            // 7. 注入工具结果
            toolResultMsg := Message{Role: "user", Content: results}
            e.history.Append(toolResultMsg)
            
            // 8. 发送工具执行事件
            for _, r := range results {
                eventCh <- ToolResultEvent{Name: r.ToolName, Output: r.Output}
            }
        }
    }()
    
    return eventCh, nil
}
```

### 5.2 核心接口定义建议

```go
// internal/engine/engine.go

// Provider 抽象 LLM 调用
type Provider interface {
    Stream(ctx context.Context, req Request) (Response, error)
}

// ToolRegistry 工具注册中心
type ToolRegistry interface {
    Get(name string) (Tool, bool)
    List() []Tool
    ToSchema() []ToolSchema
}

// Tool 单个工具
type Tool interface {
    Name() string
    Description() string
    Execute(ctx context.Context, input json.RawMessage) (ToolResult, error)
    IsReadOnly(input json.RawMessage) bool
}

// PermissionChecker 权限检查
type PermissionChecker interface {
    Evaluate(toolName string, isReadOnly bool, filePath string) PermissionDecision
}

// HookExecutor 生命周期钩子
type HookExecutor interface {
    Execute(ctx context.Context, event HookEvent, data map[string]any) error
}

// ContextManager 上下文窗口管理
type ContextManager interface {
    ShouldCompact(history []Message) bool
    Compact(ctx context.Context, history []Message) ([]Message, error)
}
```

### 5.3 关键设计决策建议

| 决策点 | 建议 | 理由 |
|--------|------|------|
| **循环风格** | 显式 for 循环 + goroutine | Go 的惯用模式，清晰可控 |
| **输出方式** | `<-chan Event` 通道 | 天然支持流式输出和取消 |
| **并行工具** | `errgroup.Group` | Go 标准库，支持错误传播和取消 |
| **上下文管理** | 双层压缩（micro + LLM summarize） | 参考 OpenHarness 的 auto + reactive 模式 |
| **错误恢复** | reactive compact + 重试 | 防止 context-too-long 导致完全失败 |
| **权限控制** | 工具执行前统一检查 | 参考 OpenHarness 的 `PermissionChecker` |
| **终止条件** | 无工具调用 + max_turns + context 取消 | 三重保障 |
| **Hook** | PreToolUse / PostToolUse / Stop / TurnComplete | 覆盖主要生命周期阶段 |

### 5.4 并行工具执行建议

```go
// internal/engine/engine.go

func (e *Engine) executeTools(ctx context.Context, calls []ToolCall) []ToolResult {
    if len(calls) == 1 {
        // 单工具：直接执行
        result := e.executeSingleTool(ctx, calls[0])
        return []ToolResult{result}
    }
    
    // 多工具：并行执行
    g, gctx := errgroup.WithContext(ctx)
    results := make([]ToolResult, len(calls))
    
    for i, tc := range calls {
        i, tc := i, tc  // capture
        g.Go(func() error {
            results[i] = e.executeSingleTool(gctx, tc)
            return nil  // 永远不返回 error，避免取消其他工具
        })
    }
    
    g.Wait()  // 等待所有完成
    return results
}
```

### 5.5 上下文压缩建议

```go
// internal/context/manager.go

type Manager struct {
    provider       Provider
    maxTokens      int
    compactRatio   float64  // 超过此比例触发压缩（如 0.8）
}

func (m *Manager) ShouldCompact(messages []Message) bool {
    estimated := estimateTokens(messages)
    return float64(estimated) > float64(m.maxTokens) * m.compactRatio
}

func (m *Manager) Compact(ctx context.Context, messages []Message) ([]Message, error) {
    // 第一层：Micro-Compact（清除旧工具输出）
    compacted := m.microCompact(messages)
    if m.estimateTokens(compacted) < int(float64(m.maxTokens)*0.7) {
        return compacted, nil
    }
    
    // 第二层：LLM Summarize（生成摘要替换旧消息）
    return m.llmSummarize(ctx, compacted)
}
```

### 5.6 事件类型定义建议

```go
// internal/engine/events.go

type Event interface{ eventTag() }

type TextDeltaEvent struct { Text string }
type ToolStartedEvent struct { Name string; Input json.RawMessage }
type ToolCompletedEvent struct { Name string; Output string; IsError bool }
type TurnCompleteEvent struct { Message Message; Usage Usage }
type StatusEvent struct { Message string }
type ErrorEvent struct { Err error }
```

---

## 附录 A：框架源码路径索引

| 框架 | Loop 核心文件 | 工具系统 | 上下文管理 |
|------|--------------|----------|-----------|
| OpenHarness | `src/openharness/engine/query.py` | `src/openharness/tools/base.py` | `src/openharness/services/compact.py` |
| HermesAgent | `run_agent.py` → `run_conversation()` | `model_tools.py` | `agent/context_compressor.py` |
| OpenAI SDK | `src/agents/run.py` → `AgentRunner.run()` | `src/agents/tool.py` | `src/agents/memory/` |
| DeepAgents | `libs/deepagents/graph.py` → LangGraph | Middleware 栈 | `middleware/summarization.py` |
| OpenCode | `packages/opencode/src/session/` | `packages/opencode/src/tool/tool.ts` | 专用 compaction agent |
| OpenClaw | `src/` Gateway 路由 | AI SDK tools | `/compact` 命令 |
| Claude SDK | 内嵌二进制 | 内置工具 | 内置 |

## 附录 B：推荐阅读顺序

1. **OpenHarness** `query.py` — 最清晰的显式循环实现，建议作为首选参考
2. **OpenAI SDK** `run.py` — 企业级的状态管理和断点恢复机制
3. **HermesAgent** `run_agent.py` — 最完整的 Python 实现，特别是并行工具和上下文压缩
4. **DeepAgents** `graph.py` — Middleware 栈的设计思路
