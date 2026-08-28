# Context Engineering 策略深度调研报告

> 调研时间：2026-05-19  
> 调研框架：DeepAgents / OpenHarness / OpenCode(sst) / OpenClaw / HermesAgent / Claude Agent SDK / OpenAI Agent SDK  
> 目标：为 harness9 的 Context Engineering 演进提供可操作的技术依据

---

## 1. 执行摘要

本次调研覆盖 7 个主流 Agent Harness 框架的 Context Engineering 实现，核心发现如下：

**最关键发现**：所有成熟框架都已从"消息条数"滑动窗口演进到"token 感知"压缩，并形成了两条清晰的技术路线：
- **静默裁剪路线**（OpenCode、DeepAgents）：在发送前检测 overflow，先 prune 工具输出，再触发 LLM 摘要，最后自动继续。
- **API 委托路线**（OpenAI Agent SDK）：将历史压缩委托给 Responses API 的 `responses.compact` 端点，框架层只管触发时机。

**最有价值的单一实践**：HermesAgent 的"工具调用对完整性修复"——压缩后主动扫描并修复 `tool_call / tool_result` 孤立对，这是保证 Anthropic API 消息合法性的关键。

**对 harness9 的核心建议**：以 token 估算（字符数 ÷ 4）替代消息条数，在 LLM 调用前执行预飞检测（preflight check），80% context window 时触发压缩。这可以在不引入新依赖的前提下实现，完全契合轻量级原则。

---

## 2. 框架对比矩阵

| 维度 | DeepAgents | OpenHarness | OpenCode(sst) | OpenClaw | HermesAgent | OpenAI Agent SDK | Claude Agent SDK |
|------|-----------|------------|--------------|---------|------------|-----------------|-----------------|
| **Token Budget 机制** | 基于 max_input_tokens 分数或绝对 token 数 | auto_compact_threshold_tokens 参数 | 动态计算 usable = context - reserved(20K) | 插件化：tokenBudget 参数传入 compact() | threshold_percent × context_length（默认 75%） | 10+ 候选项触发，或自定义 lambda | 服务端 compact_threshold 参数 |
| **Summarize 实现** | LLM 调用，可换模型，支持 LangChain DEFAULT_SUMMARY_PROMPT | 调用 LLM 生成摘要 | 结构化 SUMMARY_TEMPLATE，支持增量更新 | 委托给 runtime，pluggable | 结构化提示词，支持焦点主题，增量摘要 | 委托给 OpenAI Responses API | 委托给 Claude API |
| **Token 计算方式** | count_tokens_approximately（LangChain 方法） | 估算 | Token.estimate()（字符估算） | 调用方提供 currentTokenCount | 字符数 ÷ 4，图片固定 1600 tokens | API 返回的 usage 字段 | API 返回 |
| **Compaction 触发时机** | 预飞检测 + ContextOverflowError 响应式 | 每次调用前检查阈值 | 流式处理中检测 overflow，中断流重触发 | 预飞（assemble 前）+ budget/threshold 两种目标 | 预飞多 pass（最多 3 次），API 报错后响应式 | 每 turn 结束后（可自定义 lambda） | 服务端实时检测 |
| **动态感知 context window** | model.profile["max_input_tokens"] | context_window_tokens 配置参数 | model.limit.context（来自 models.dev） | tokenBudget 由 runner 注入 | 10 步骤查询链（OpenRouter→硬编码→fallback 256K） | API 返回的 usage 数据 | 模型元数据 |
| **Tool-Call 裁剪** | read_file 截断 4K 字符，其余 offload 到文件 | 工具结果单独处理 | 2000 字符上限，prune 标记旧工具输出 | 支持 tokensBefore/tokensAfter 跟踪 | 3 轮预处理（去重→1 行摘要→截断参数） | 通过 compacted 候选项跟踪 | 服务端处理 |
| **孤立 tool_result 修复** | 过滤孤立 ToolMessage | sanitize_conversation_messages() | prune 停在 summary 边界，保留完整对 | 委托给 runtime | 压缩后主动插入 stub result / 删除孤立 use | responses.compact 内部处理 | API 内部处理 |
| **File System Offload** | FilesystemMiddleware：工具结果超 20K tokens 写入 /large_tool_results/ | 无明确证据 | 无（压缩 + 截断策略） | 无明确证据 | @file:/@folder: 引用，50%/25% context 配额 | 无 | 无 |
| **MCP 支持** | 有 | 有 | 有 | 有 | 有 | 有 | 有 |
| **语言** | Python | Python | TypeScript | TypeScript | Python | Python | — |

---

## 3. 各维度深度分析

### 3.1 Token Budget 机制

#### DeepAgents（LangChain）

DeepAgents 的 `SummarizationMiddleware` 支持三种触发格式：

```python
# 触发格式 1：绝对 token 数
trigger = ("tokens", 170000)

# 触发格式 2：消息条数
trigger = ("messages", N)

# 触发格式 3：模型 max_input_tokens 的分数
trigger = ("fraction", 0.85)  # 达到 85% 时触发
```

分配策略由 `compute_summarization_defaults()` 动态选择：有模型 profile（包含 `max_input_tokens`）时使用分数策略，无 profile 时退化为保守固定值（170K tokens 触发，保留最后 6 条消息）。

```python
def compute_summarization_defaults(model: BaseChatModel) -> SummarizationDefaults:
    has_profile = (
        model.profile is not None
        and "max_input_tokens" in model.profile
    )
    if has_profile:
        return {
            "trigger": ("fraction", 0.85),  # 85% 触发
            "keep": ("fraction", 0.10),     # 保留 10% 的最近内容
        }
    return {
        "trigger": ("tokens", 170000),      # 固定阈值兜底
        "keep": ("messages", 6),
    }
```

**Tool 参数截断预处理**：在正式压缩前有一个独立的 `truncate_args_settings` 检查，同样支持三种格式，对 `write_file`/`edit_file` 等工具的参数字符串执行廉价截断，避免不必要的全量 LLM 摘要调用。

#### OpenHarness（HKUDS）

`QueryEngine` 通过两个参数管理 token budget：

```python
class QueryContext:
    context_window_tokens: int         # 总窗口大小
    auto_compact_threshold_tokens: int  # 触发自动压缩的阈值
    max_tokens: int = 4096             # 单次输出上限（保守兜底）
```

`MAX_SAFE_COMPLETION_TOKENS = 128_000`：对用户配置的 max_tokens 做上限保护，防止过大的 completion token 请求导致 API 拒绝。

`run_query()` 在每次 API 调用前检查 token 估算，并在收到"prompt too long"错误时响应式触发压缩后重试。

#### OpenCode（sst/opencode）

OpenCode 的 token budget 计算最为精细：

```typescript
// COMPACTION_BUFFER = 20_000 tokens（为输出预留空间）
export function usable(input: { cfg: Config.Info; model: Provider.Model }) {
  const context = input.model.limit.context
  if (context === 0) return 0

  // 优先使用配置的 reserved，否则取 min(20K, maxOutputTokens)
  const reserved = input.cfg.compaction?.reserved ??
    Math.min(COMPACTION_BUFFER, maxOutputTokens(input.model))

  // 若 model 有独立 input limit，使用 input - reserved；否则用 context - output
  return input.model.limit.input
    ? Math.max(0, input.model.limit.input - reserved)
    : Math.max(0, context - maxOutputTokens(input.model))
}
```

最近上下文预留策略：

```typescript
// 将 usable 的 25% 分配给最近消息，最少 2K，最多 8K tokens
const preserveRecentBudget = Math.min(
  MAX_PRESERVE_RECENT_TOKENS,   // 8000
  Math.max(MIN_PRESERVE_RECENT_TOKENS, Math.floor(usable(input) * 0.25))
  // 2000
)
```

#### HermesAgent（NousResearch）

基于百分比的阈值机制：

```python
class ContextEngine:
    threshold_percent: float = 0.75  # 默认 75% 触发
    context_length: int              # 当前模型最大 context
    threshold_tokens: int            # = context_length * threshold_percent

    def update_model(self, model: str, context_length: int):
        self.context_length = context_length
        self.threshold_tokens = int(context_length * self.threshold_percent)
```

budget 分配：
- `_SUMMARY_RATIO = 0.20`：摘要 token 上限为可压缩内容的 20%
- `_SUMMARY_TOKENS_CEILING = 12_000`：摘要 token 硬上限
- `tail_token_budget = summary_target_ratio * threshold_tokens`：尾部保护区预算

#### OpenAI Agent SDK

两种互补机制：

**客户端压缩**：`OpenAIResponsesCompactionSession` 默认在每 turn 结束后检查"10+ 候选项"，可通过自定义 lambda 覆盖：

```python
session = OpenAIResponsesCompactionSession(
    session_id="conv_123",
    underlying_session=SQLiteSession("conv_123"),
    should_trigger_compaction=lambda ctx: len(ctx["candidates"]) >= 5,
)
```

**服务端压缩**（`ModelSettings.context_management`）：通过 Responses API 的 `context_management` 字段触发服务端实时压缩：

```python
# 服务端在 200K tokens 时触发压缩，与框架层互补
ModelSettings(context_management=[{"type": "compaction", "compact_threshold": 200000}])
```

### 3.2 Summarize 上下文摘要

#### DeepAgents：摘要 + 文件系统 offload

摘要生成调用 LLM（可配置独立摘要模型），使用 LangChain `DEFAULT_SUMMARY_PROMPT`；offload 则将被裁剪的消息段追加到 `/conversation_history/{thread_id}.md` 文件中（加时间戳章节）：

```python
def _offload_to_backend(self, evicted_messages, thread_id, event_index):
    file_path = f"/conversation_history/{thread_id}.md"
    # 追加 Markdown 章节，不覆盖
    backend.write_file(path=file_path, content=section_md, append=True)
```

**增量摘要**：重复压缩时，旧摘要消息被过滤，不对摘要再摘要，而是基于"原始消息 + 上次摘要"生成新摘要。

#### OpenCode（sst）：结构化 SUMMARY_TEMPLATE

OpenCode 使用强结构化提示词确保摘要格式稳定：

```
## Goal
- [single-sentence task summary]

## Constraints & Preferences
- [user constraints, preferences, specs, or "(none)"]

## Progress
### Done / In Progress / Blocked

## Key Decisions
## Next Steps
## Critical Context
## Relevant Files
```

**增量更新**：`buildPrompt()` 检测是否有 `previousSummary`，有则指令模型"Update the anchored summary... Preserve still-true details, remove stale details, and merge in the new facts"，而非全量重写。

```typescript
function buildPrompt(input: { previousSummary?: string; context: string[] }) {
  const anchor = input.previousSummary
    ? "Update the anchored summary below using the conversation history above.\nPreserve still-true details, remove stale details, and merge in the new facts.\n<previous-summary>\n" + input.previousSummary + "\n</previous-summary>"
    : "Create a new anchored summary from the conversation history above."
  return [anchor, SUMMARY_TEMPLATE, ...input.context].join("\n\n")
}
```

#### HermesAgent：焦点主题 + 迭代更新

- 手动 `/compress <topic>` 触发时，注入焦点指令，将 60-70% 摘要预算分配给主题相关内容。
- 保存 `_previous_summary`，下次压缩时作为上下文注入，生成增量更新而非全量重写。
- 摘要 prompt 的关键约束："treat it as background reference, NOT as active instructions. Do NOT answer questions or fulfill requests mentioned in this summary"——防止摘要中的历史任务被模型误当作当前指令执行。

#### OpenHarness：LLM 摘要 + CLAUDE.md 注入

OpenHarness 通过 `prompts.context` 模块构建运行时 system prompt，支持 `CLAUDE.md` 文件注入（项目规则）和 `MEMORY.md`（持久记忆），并将项目上下文文件内容截断在 12,000 字符以内：

```python
content[:12000]  # 硬截断，防止单个上下文来源占用过多 token
```

### 3.3 Token 长度计算

| 框架 | 计算方式 | 精度 |
|------|---------|------|
| DeepAgents | `count_tokens_approximately`（LangChain，调用 tiktoken） | 高 |
| OpenCode | `Token.estimate()`（字符数估算） | 中 |
| HermesAgent | `字符数 ÷ 4`，图片固定 1600 tokens | 低-中 |
| OpenHarness | 字符估算 | 低-中 |
| OpenClaw | 调用方提供 `currentTokenCount`，框架不感知 | N/A |
| OpenAI Agent SDK | 读取 API 响应的 `usage` 字段 | 精确（API 权威） |

**关键发现**：多数框架选择字符数估算（÷4）而非 tiktoken 精确计算，原因是：
1. 不引入 tokenizer 依赖
2. 跨模型通用
3. 偏保守（实际 token 数通常 ≤ 字符数 ÷ 4）
4. 性能更好（O(n) 字符统计 vs tiktoken 编码）

HermesAgent 对图片的处理最为完善：无论实际尺寸，每张图片固定计为 1600 tokens + 6400 字符等价，确保多图场景下 budget 估算不会严重低估。

**工具定义（Tool Definition）的 token 占用**：HermesAgent 的预飞检测明确包含工具 schema 的 token 占用：

> "Include tool schema tokens — with many tools these can add 20-30K+ tokens that the old sys+msg estimate missed entirely."

`estimate_request_tokens_rough()` = 系统提示 token + 工具 schema token + 消息 token，三者之和。

### 3.4 Compaction 触发时机

#### 预飞（Preflight）vs 响应式（Reactive）

**HermesAgent — 预飞 + 多 pass**：

```python
# 进入主循环前预飞检测
while estimate_request_tokens_rough(messages) >= compressor.threshold_tokens:
    messages = await compressor.compress(messages)
    passes += 1
    if passes >= 3:
        break  # 防止无限压缩
```

检测公式包含工具 schema token，防止漏算。

**OpenCode — 流中断 + 事后处理**：

```typescript
// 流式处理中，检测到 overflow 时中断流
const isOverflow = ({tokens, model, cfg}) => {
  if (cfg.compaction?.auto === false) return false
  const total = tokens.input + tokens.output + tokens.cache.read + tokens.cache.write
  return total >= usable({model, cfg})
}

// processor 中：
Stream.takeUntil(() => ctx.needsCompaction)
// overflow 后触发 processCompaction()
```

OpenCode 的触发时机是"在 LLM 响应完成后，检测 usage 是否溢出"（响应式），而非发送前（预飞）。这意味着当次请求已经被处理，压缩在下一轮前执行。

**DeepAgents — 双触发**：

```python
async def awrap_model_call(self, messages, ...):
    # 1. 预飞：发送前检查
    if self._should_summarize(messages, total_tokens):
        await self._acompact(...)
    
    try:
        return await model.ainvoke(messages)
    except ContextOverflowError:
        # 2. 响应式：API 返回错误后
        await self._acompact(...)
        return await model.ainvoke(messages)  # 重试
```

**OpenAI Agent SDK — Turn 结束后**：

```python
# 每个 turn 结束后，检查是否触发
result = await Runner.run(agent, "Hello", session=session)
# session 内部：if should_trigger_compaction(ctx): await run_compaction()
```

**防抖机制（HermesAgent）**：`_last_compression_savings_pct` 跟踪，若连续两次压缩都节省 < 10%，则跳过，防止无效循环。

### 3.5 动态感知模型上下文窗口

#### HermesAgent — 10 步查询链

最完善的实现，`get_model_context_length()` 按优先级依次查询：

1. 用户配置覆盖（最高优先）
2. 持久化磁盘缓存（带失效控制）
3. AWS Bedrock 静态表
4. 直接查询 `/models` 端点
5. Copilot 在线 API
6. Nous Portal（权威于 OpenRouter）
7. ChatGPT Codex OAuth
8. Ollama `/api/show` 探针
9. models.dev 注册表
10. OpenRouter 社区维护目录
11. 硬编码 pattern 匹配表（最长 key 优先）
12. 本地服务器查询
13. 默认兜底：**256,000 tokens**

特殊处理：Kimi 系列（KV-cache 汇报 32K，实际 262K+）有专门 guard 防止使用 OpenRouter 的低估值。

#### OpenCode — models.dev 数据库

`fromModelsDevModel()` 从 models.dev 社区维护的模型数据库提取：

```typescript
const ProviderLimit = Schema.Struct({
  context: Schema.Finite,   // 上下文窗口
  input: optionalOmitUndefined(Schema.Finite),  // 可选输入限制
  output: Schema.Finite,    // 输出上限
})
```

支持在 `opencode.json` 中自定义覆盖：

```json
{
  "models": {
    "my-model": {
      "limit": { "context": 200000, "output": 8192 }
    }
  }
}
```

#### OpenClaw — 调用方注入

OpenClaw 的 Context Engine 接口不感知模型，token budget 由 runner 在调用 `assemble()` / `compact()` 时注入：

```typescript
// ContextEngineRuntimeContext
tokenBudget?: number          // 由 runner 注入当前模型的上下文预算
currentTokenCount?: number    // 当前估算 token 数

// compact() 参数
compact(params: {
  tokenBudget?: number
  currentTokenCount?: number
  compactionTarget?: "budget" | "threshold"
})
```

这种设计使 Context Engine 完全无状态，易于测试和替换，但将模型感知责任推给了调用层。

#### DeepAgents — Model Profile 注册表

通过 `ProviderProfile` 和 `HarnessProfile` 注册模型 profile，其中包含 `max_input_tokens`：

```python
# 框架内置 6 个 profile：
# openai, openrouter, anthropic-haiku-4.5, anthropic-sonnet-4.6, anthropic-opus-4.7, openai-codex
```

无 profile 时退化为保守固定值（170K tokens 触发）。

### 3.6 Tool-Call Message 裁剪策略

#### OpenCode — 双层保护

**Prune（廉价，非 LLM）**：在保留最近 2 轮之外，反向扫描工具输出，累积超过 `PRUNE_PROTECT = 40,000 tokens` 时将旧工具输出标记为 `compacted`（时间戳标记，不删除数据）。跳过 `PRUNE_PROTECTED_TOOLS`（如 "skill"），遇到 summary 消息停止。最小节省 `PRUNE_MINIMUM = 20,000 tokens` 才实际写入变更。

```typescript
const PRUNE_MINIMUM = 20_000   // 最小节省阈值，低于此不执行 prune
const PRUNE_PROTECT = 40_000   // 累积超过此值后开始 prune 旧输出
const TOOL_OUTPUT_MAX_CHARS = 2_000  // 发送给摘要 LLM 时的工具输出截断
```

**processCompaction（有 LLM 参与）**：工具输出在转换为模型消息时强制截断至 `TOOL_OUTPUT_MAX_CHARS = 2_000` 字符。

**孤立消息合法性**：`prune` 函数遇到之前的 summary 边界时停止，保证 summary 前的消息块是一个完整的已压缩单元。

#### HermesAgent — 三轮预处理

压缩前对工具输出执行三轮廉价预处理：

```
Pass 1 - 去重：MD5 相同的工具输出替换为 back-reference
Pass 2 - 摘要化：旧输出替换为 1 行描述，如 "[terminal] ran `npm test` → exit 0, 47 lines"
Pass 3 - 参数截断：大型工具参数截断（保持 JSON 有效性）
```

**尾部保护（token 计量而非消息计数）**：

```python
# 从尾部反向累积，直到达到 tail_token_budget
# 软上限：1.5× tail_token_budget（防止一条大消息切碎）
# 硬最小：至少 3 条消息
# 保证最近 user 消息始终在尾部（防止活跃任务丢失）
```

**压缩后完整性修复**（关键！）：

```python
# 修复 1：删除无配对 assistant tool_call 的孤立 tool_result
# 修复 2：为孤立的 assistant tool_call 插入 stub tool_result
# 修复 3：剥离旧消息中的 base64 图片，防止 MB 级 payload 复活
```

这直接对应 harness9 现有的孤立 Observation 回溯逻辑，但 HermesAgent 的处理更完整（双向修复）。

#### DeepAgents — read_file 特殊处理 + 文件系统 offload

```python
# read_file 工具结果：截断到 ~4,000 字符，附带继续读取的提示
def _slice_read_file_tm(msg, original_path) -> ToolMessage:
    truncated = content[:4000]
    notice = "...truncated. Use read_file with offset and limit parameters to retrieve specific portions."
    return msg.with_content(truncated + "\n" + notice)

# 其他工具结果：超过 20,000 token 时写到文件系统
tool_token_limit_before_evict = 20000  # ~80,000 字符
# 写入路径：/large_tool_results/{tool_call_id}
```

**重要设计**：`TOOLS_EXCLUDED_FROM_EVICTION = [ls, glob, grep, read_file, edit_file, write_file]`——这些工具要么输出本身短，要么有内置分页，不需要 offload。

#### OpenHarness — sanitize_conversation_messages()

在每次 LLM 调用前执行消息卫生清洗：

```python
def sanitize_conversation_messages(messages):
    # 1. 移除空的 assistant 消息
    # 2. 检测未收到 tool_result 的 tool_use 请求（断链）
    # 3. 移除破损的工具调用序列
    # 4. 过滤孤立的用户侧 tool_result
```

### 3.7 File System Context Offload

#### DeepAgents — 最完整的 offload 实现

`FilesystemMiddleware` 提供两种 offload 触发：

**主动 offload（工具调用完成时）**：
```python
# 工具结果 > 20,000 tokens 时立即写入文件系统
tool_token_limit_before_evict = 20000
# 写入：/large_tool_results/{tool_call_id}
# 消息替换为：预览（头 5 行 + 尾 5 行）+ 文件路径引用
```

**被动 offload（LLM 调用前）**：
```python
# HumanMessage > 50,000 tokens 时
human_message_token_limit_before_evict = 50000
# 写入：/conversation_history/{uuid}.md
```

存储格式：文件路径注入到替换消息中，Agent 可以用 `read_file` 工具按需重新加载。

`NUM_CHARS_PER_TOKEN = 4`：同样使用字符估算，保持与压缩层一致。

**offload 状态追踪**：使用 `DeltaChannel` + `_file_data_delta_reducer`，每 50 步创建快照，支持通过 `None` 值标记文件删除。

#### HermesAgent — 引用系统（非 offload）

HermesAgent 使用引用系统将外部内容按需注入：

```python
REFERENCE_PATTERN  # 匹配 @file:, @folder:, @git: 引用

# token 配额控制
HARD_LIMIT = context_length * 0.50  # 超过则 blocked=True
SOFT_LIMIT = context_length * 0.25  # 超过则警告

# 路径安全：拒绝 .ssh, .aws, .kube 等敏感目录
```

这不是真正的 offload（数据不在 context 里），而是"按需加载"模式，更节省 token。

---

## 4. harness9 改进建议

基于上述调研，按优先级给出 harness9 可实施的改进方案。

### P0：Token 感知的预飞压缩（必须做）

**问题**：当前 `SlidingWindowCompactor` 不感知 token 数量，固定保留 N 条消息，在工具输出很长时会导致实际 token 数远超预期。

**方案**：在 `applyCompactionWith` 中增加 token 预估，将裁剪决策从"消息条数"切换为"token 估算"。

```go
// internal/memory/compaction.go
// TokenAwareCompactor 基于 token 预估而非消息条数进行裁剪
type TokenAwareCompactor struct {
    // MaxTokens：目标 token 预算（e.g. 180_000 for claude-3-5-sonnet with 200K context）
    MaxTokens int
    // TailTurns：无论 token 预算如何，至少保留最近 N 个完整 turn
    TailTurns int
}

// estimateTokens 用字符数估算 token 数（4 字符 ≈ 1 token）
// 与 HermesAgent、DeepAgents、OpenCode 三个框架策略一致
func estimateTokens(msgs []schema.Message) int {
    total := 0
    for _, m := range msgs {
        total += len(m.Content) / 4  // 消息文本
        for _, tc := range m.ToolCalls {
            total += (len(tc.Name) + len(tc.Arguments)) / 4
        }
        if m.ToolCallID != "" {
            total += len(m.ToolCallID) / 4
        }
    }
    return total
}

func (c *TokenAwareCompactor) Compact(msgs []schema.Message) []schema.Message {
    if len(msgs) == 0 {
        return msgs
    }
    if estimateTokens(msgs) <= c.MaxTokens {
        return msgs  // 未超预算，直接返回
    }
    // 从尾部反向保留 TailTurns 个完整 turn
    // System 消息始终保留（msgs[0]）
    // 修复孤立 Observation（当前已有此逻辑，保留）
    // ...
}
```

**实施成本**：低。不引入新依赖，纯 Go 字符串操作。

**参考**：OpenCode 的 `usable()` 函数、HermesAgent 的 `_CHARS_PER_TOKEN = 4`。

### P0：Compactor 接口增加 maxTokens 参数

**问题**：当前 `Compactor.Compact(msgs)` 接口不携带模型信息，无法动态适应不同 context window。

**方案**：扩展接口，支持注入 token budget：

```go
// Compactor 接口扩展
type Compactor interface {
    // CompactWithBudget 在指定 token 预算内压缩消息历史。
    // budget = 0 时使用 Compactor 内置默认值。
    CompactWithBudget(msgs []schema.Message, budget int) []schema.Message
}

// 向后兼容适配器
type BudgetCompactorAdapter struct {
    inner Compactor
}
```

或者更简单地在 `AgentEngine` 中维护一个 `contextWindow int` 字段，并在创建 `TokenAwareCompactor` 时传入。

### P1：工具输出截断（Tool Output Truncation）

**问题**：工具输出（特别是 `bash` 工具）可能很长，当前仅日志截断（512 字节），但完整输出进入 context。

**方案**：在工具结果注入 context 前，对超长输出进行截断：

```go
// internal/engine/agent_loop.go — executeTools 后处理

const maxToolOutputTokens = 2000  // 对应 OpenCode 的 TOOL_OUTPUT_MAX_CHARS = 2000
const charsPerToken = 4

func truncateToolOutput(output string) string {
    maxChars := maxToolOutputTokens * charsPerToken  // 8000 chars
    if len(output) <= maxChars {
        return output
    }
    // 保留头尾各 40%，中间省略
    head := output[:maxChars*2/5]
    tail := output[len(output)-maxChars*2/5:]
    return head + "\n\n[... output truncated ...]\n\n" + tail
}
```

这对 `bash` 工具执行 `find / -name "*.go"` 等高输出命令特别重要。

**参考**：OpenCode `TOOL_OUTPUT_MAX_CHARS = 2_000`，DeepAgents `tool_token_limit_before_evict = 20_000 tokens`。

### P1：预飞 Token 检测（Preflight Check）

**问题**：当前压缩发生在每次 LLM 调用前，但压缩策略（SlidingWindowCompactor）不感知 token 数，可能仍然超窗。

**方案**：在 `runLoop` 的 LLM 调用前添加轻量级 token 预估，动态决定是否压缩：

```go
// runLoop 中，LLM 调用前

// preflightTokenCheck 预估当前 context 的 token 数，超过阈值时返回 true
func preflightTokenCheck(msgs []schema.Message, maxTokens int) bool {
    if maxTokens <= 0 {
        return false
    }
    return estimateTokens(msgs) >= maxTokens
}

// 在 em.generate 调用前
history := e.applyCompactionWith(comp, contextHistory)
if preflightTokenCheck(history, e.maxContextTokens) {
    // 触发更激进的裁剪（增加 budget 约束）
    history = e.aggressiveCompact(history)
}
responseMsg, err := em.generate(ctx, turnCount, history, availableTools)
```

### P1：孤立消息双向修复

**问题**：当前 `SlidingWindowCompactor` 只做正向修复（回溯找配对的 assistant 消息），但未检测压缩后是否存在"assistant tool_call 有、tool_result 没有"的情况（对 Anthropic API 可能导致错误）。

**方案**：压缩后执行双向完整性检查：

```go
// internal/memory/compaction.go

// repairOrphanedToolCalls 修复压缩后的孤立工具消息：
// 1. 删除无配对 assistant tool_call 的孤立 user tool_result（当前已有）
// 2. 为孤立的 assistant tool_call 插入 stub user tool_result（NEW）
func repairOrphanedToolCalls(msgs []schema.Message) []schema.Message {
    // 收集所有 assistant 发起的 tool_call ID
    calledIDs := map[string]bool{}
    for _, m := range msgs {
        if m.Role == schema.RoleAssistant {
            for _, tc := range m.ToolCalls {
                calledIDs[tc.ID] = true
            }
        }
    }
    // 收集所有已收到 tool_result 的 ID
    resultIDs := map[string]bool{}
    for _, m := range msgs {
        if m.ToolCallID != "" {
            resultIDs[m.ToolCallID] = true
        }
    }
    // 为缺失 result 的 call 插入 stub
    var result []schema.Message
    for _, m := range msgs {
        result = append(result, m)
        if m.Role == schema.RoleAssistant && len(m.ToolCalls) > 0 {
            for _, tc := range m.ToolCalls {
                if !resultIDs[tc.ID] {
                    result = append(result, schema.Message{
                        Role:       schema.RoleUser,
                        Content:    "[tool result unavailable: context was compacted]",
                        ToolCallID: tc.ID,
                    })
                }
            }
        }
    }
    return result
}
```

**参考**：HermesAgent 的"压缩后完整性修复"机制，这是保证 Anthropic API 消息合法性的关键。

### P2：LLM 摘要压缩（Summarization Compactor）

**问题**：当前只有滑动窗口截断，无法保留被裁剪消息的语义信息，长会话中任务状态会丢失。

**方案**：实现 `SummarizationCompactor`，在 token 超限时调用 LLM 生成结构化摘要：

```go
// internal/memory/summarization.go

type SummarizationCompactor struct {
    Provider      provider.LLMProvider
    MaxTokens     int
    TailTurns     int    // 保留最近 N 个 turn 不压缩
    SummaryPrompt string // 可自定义，默认使用内置结构化模板
}

// summaryTemplate 参考 OpenCode 的 SUMMARY_TEMPLATE 设计
const summaryTemplate = `
## Goal
- [single-sentence task summary]

## Progress
### Done
- [completed work or "(none)"]
### In Progress
- [current work or "(none)"]

## Key Decisions
- [decision and reason, or "(none)"]

## Next Steps
- [ordered next actions or "(none)"]

## Critical Context
- [important technical facts, errors, open questions, or "(none)"]

Rules:
- Use terse bullets, not prose paragraphs.
- Preserve exact file paths, commands, and error strings.
- Do not mention that context was compacted.
`

func (c *SummarizationCompactor) Compact(msgs []schema.Message) []schema.Message {
    if estimateTokens(msgs) <= c.MaxTokens {
        return msgs
    }
    // 1. 分割 head（待摘要）+ tail（保留最近 N 个 turn）
    head, tail := c.splitHeadTail(msgs)
    // 2. 调用 LLM 生成摘要
    summary, err := c.summarize(context.Background(), head)
    if err != nil {
        // 摘要失败时降级到 SlidingWindowCompactor
        return (&SlidingWindowCompactor{MaxMessages: c.TailTurns * 2 + 1}).Compact(msgs)
    }
    // 3. 构建新 context：system + summary + tail
    summaryMsg := schema.Message{
        Role:    schema.RoleUser,
        Content: "[Conversation Summary]\n" + summary,
    }
    result := []schema.Message{msgs[0]} // system prompt
    result = append(result, summaryMsg)
    result = append(result, tail...)
    return result
}
```

**触发时机**：`MaxTokens` 设为模型 context window 的 80%（参考 HermesAgent 的 75%、DeepAgents 的 85%）。

**实施成本**：中。需要额外的 LLM 调用（增加延迟和 API 费用）。

### P2：模型上下文窗口注册表

**问题**：harness9 目前没有模型 context window 元数据，`SlidingWindowCompactor` 的 `MaxMessages` 需要用户手动配置。

**方案**：内置轻量级模型 context window 查找表：

```go
// internal/provider/model_limits.go

// ModelLimits 存储模型的上下文窗口和输出上限（单位：tokens）
type ModelLimits struct {
    ContextTokens int
    OutputTokens  int
}

// knownModels 硬编码常见模型的 context window，参考 OpenCode 的 models.dev 数据
var knownModels = map[string]ModelLimits{
    "claude-opus-4-5":        {ContextTokens: 200_000, OutputTokens: 32_000},
    "claude-sonnet-4-5":      {ContextTokens: 200_000, OutputTokens: 64_000},
    "claude-haiku-4-5":       {ContextTokens: 200_000, OutputTokens: 8_192},
    "gpt-4o":                 {ContextTokens: 128_000, OutputTokens: 16_384},
    "gpt-4o-mini":            {ContextTokens: 128_000, OutputTokens: 16_384},
    "gpt-4.1":                {ContextTokens: 1_047_576, OutputTokens: 32_768},
    "gpt-4.1-mini":           {ContextTokens: 1_047_576, OutputTokens: 32_768},
    "deepseek-v3":            {ContextTokens: 64_000, OutputTokens: 8_000},
    "qwen2.5-72b-instruct":   {ContextTokens: 131_072, OutputTokens: 8_192},
}

// GetModelLimits 返回模型限制，未知模型返回保守默认值（128K）
func GetModelLimits(modelName string) ModelLimits {
    if limits, ok := knownModels[modelName]; ok {
        return limits
    }
    // 兜底：256K tokens（参考 HermesAgent 的 fallback）
    return ModelLimits{ContextTokens: 256_000, OutputTokens: 8_192}
}
```

`AgentEngine` 从 Provider 获取 model 名称，查表得 context window，传给 `TokenAwareCompactor`。

### P2：大型工具输出 File System Offload

**问题**：`bash` 工具执行代码分析等命令可能返回数万字符的输出，占用大量 context window。

**方案**（轻量版，不引入新依赖）：将超大工具输出保存到 harness9 工作目录，context 中只保留文件引用：

```go
// internal/tools/large_output.go

const maxToolOutputChars = 8_000  // ~2000 tokens

// MaybeSaveToFile 检查工具输出大小，超限时写入文件并返回引用
func MaybeSaveToFile(workDir, toolCallID, output string) string {
    if len(output) <= maxToolOutputChars {
        return output
    }
    // 写入 .harness9/tool_outputs/{toolCallID}.txt
    path := filepath.Join(workDir, ".harness9", "tool_outputs", toolCallID+".txt")
    os.MkdirAll(filepath.Dir(path), 0755)
    os.WriteFile(path, []byte(output), 0644)
    
    // 返回头尾预览 + 文件路径
    preview := output[:500] + "\n...\n" + output[len(output)-500:]
    return fmt.Sprintf("[Output too large (%d chars). Saved to: %s]\n\nPreview:\n%s",
        len(output), path, preview)
}
```

**参考**：DeepAgents 的 `FilesystemMiddleware`（20K tokens 阈值），OpenCode 的 `TOOL_OUTPUT_MAX_CHARS = 2_000`。

---

## 5. 参考资料

### 框架源码（实际访问并提取信息的文件）

**DeepAgents（LangChain）**
- https://github.com/langchain-ai/deepagents/blob/main/libs/deepagents/deepagents/middleware/summarization.py
- https://github.com/langchain-ai/deepagents/blob/main/libs/deepagents/deepagents/middleware/_overflow_clip.py
- https://github.com/langchain-ai/deepagents/blob/main/libs/deepagents/deepagents/middleware/_message_eviction.py
- https://github.com/langchain-ai/deepagents/blob/main/libs/deepagents/deepagents/middleware/filesystem.py
- https://github.com/langchain-ai/deepagents/blob/main/libs/deepagents/deepagents/__init__.py

**OpenHarness（HKUDS）**
- https://github.com/HKUDS/OpenHarness/blob/main/src/openharness/engine/query_engine.py
- https://github.com/HKUDS/OpenHarness/blob/main/src/openharness/engine/query.py
- https://github.com/HKUDS/OpenHarness/blob/main/src/openharness/engine/messages.py
- https://github.com/HKUDS/OpenHarness/blob/main/src/openharness/engine/cost_tracker.py
- https://github.com/HKUDS/OpenHarness/blob/main/src/openharness/prompts/context.py

**OpenCode（sst/opencode）**
- https://github.com/sst/opencode/blob/dev/packages/opencode/src/session/compaction.ts
- https://github.com/sst/opencode/blob/dev/packages/opencode/src/session/overflow.ts
- https://github.com/sst/opencode/blob/dev/packages/opencode/src/session/processor.ts
- https://github.com/sst/opencode/blob/dev/packages/opencode/src/provider/provider.ts

**OpenClaw**
- https://github.com/openclaw/openclaw/blob/main/src/context-engine/types.ts
- https://github.com/openclaw/openclaw/blob/main/src/context-engine/registry.ts
- https://github.com/openclaw/openclaw/blob/main/src/context-engine/legacy.ts
- https://github.com/openclaw/openclaw/blob/main/src/context-engine/delegate.ts

**HermesAgent（NousResearch）**
- https://github.com/NousResearch/hermes-agent/blob/main/agent/context_engine.py
- https://github.com/NousResearch/hermes-agent/blob/main/agent/context_compressor.py
- https://github.com/NousResearch/hermes-agent/blob/main/agent/conversation_compression.py
- https://github.com/NousResearch/hermes-agent/blob/main/agent/conversation_loop.py
- https://github.com/NousResearch/hermes-agent/blob/main/agent/model_metadata.py
- https://github.com/NousResearch/hermes-agent/blob/main/agent/context_references.py
- https://github.com/NousResearch/hermes-agent/blob/main/agent/chat_completion_helpers.py

**OpenAI Agent SDK**
- https://github.com/openai/openai-agents-python/blob/main/src/agents/memory/openai_responses_compaction_session.py（通过 GitHub 页面分析）
- https://github.com/openai/openai-agents-python/blob/main/src/agents/model_settings.py
- https://github.com/openai/openai-agents-python/blob/main/docs/sessions/index.md
- https://github.com/openai/openai-agents-python/blob/main/src/agents/run.py
- Context7 MCP 查询（library ID: /openai/openai-agents-python）

### 可访问性说明

- **DeepAgents**：GitHub 仓库可正常访问，核心逻辑集中在 `middleware/` 目录
- **OpenHarness**：GitHub 仓库可正常访问，context engineering 实现在 `engine/` 目录
- **OpenCode**：调研使用的是 sst/opencode 仓库（162K stars），anomalyco/opencode 仓库无法访问（404）
- **OpenClaw**：GitHub 仓库可正常访问，插件化设计，实际压缩委托给 runtime
- **HermesAgent**：部分文件可访问（context_compressor.py、context_engine.py 等）
- **Claude Agent SDK**：docs.anthropic.com 和 platform.claude.com 均重定向或返回 404，无法直接访问官方文档
- **OpenAI Agent SDK**：通过 GitHub raw 文件和 Context7 MCP 获取完整信息
