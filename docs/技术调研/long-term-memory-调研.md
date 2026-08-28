# Agent Harness 框架 Long-Term Memory 技术调研报告

> 调研日期：2026-06-02
> 调研范围：DeepAgents、OpenHarness、OpenCode、OpenClaw、HermesAgent、Claude Agent SDK
> 调研方法：WebFetch 直接访问 GitHub 源码 + 官方文档 + Context7 API 文档

---

## 目录

1. [调研背景与方法](#1-调研背景与方法)
2. [各框架逐一分析](#2-各框架逐一分析)
   - 2.1 DeepAgents（LangChain）
   - 2.2 OpenHarness（HKUDS）
   - 2.3 OpenCode（Anomaly）
   - 2.4 OpenClaw
   - 2.5 HermesAgent（NousResearch）
   - 2.6 Claude Agent SDK（Anthropic）
3. [横向对比：五大维度矩阵](#3-横向对比五大维度矩阵)
4. [设计模式提炼](#4-设计模式提炼)
5. [对 harness9 的设计启示与建议](#5-对-harness9-的设计启示与建议)
6. [参考资料](#6-参考资料)

---

## 1. 调研背景与方法

### 1.1 背景

Long-Term Memory（长期记忆）是 Agent Harness 框架的核心差异化能力之一。短期记忆（Short-Term Memory / Context Window）只能在单次会话内保持，而长期记忆必须跨会话持久化，使 Agent 能够积累知识、记住用户偏好、复用过去的经验。

本调研针对 6 个主流 Agent 框架，系统分析其 Long-Term Memory 的技术方案，聚焦 5 个核心维度：

1. **触发时机**：何时写入/更新长期记忆
2. **生成与更新方式**：LLM 摘要还是规则提取，增量还是全量，是否有 reflection 机制
3. **存储方式与结构**：介质、数据结构、Schema 设计
4. **注入 Context 的方式**：检索机制、注入位置、与压缩协同
5. **冲突、遗忘、衰减等机制**

### 1.2 仓库存活性验证（截至 2026-06-02）

| 框架 | Stars | 语言 | 状态 |
|------|-------|------|------|
| DeepAgents | 23,722 | Python | 活跃，最后更新 2026-06-02 |
| OpenHarness | 13,421 | Python | 活跃 |
| OpenCode | 168,613 | TypeScript | 活跃，最后更新 2026-06-02 |
| OpenClaw | 376,159 | TypeScript | 活跃，最后更新 2026-06-02 |
| HermesAgent | 176,579 | Python | 活跃，最后更新 2026-06-02 |
| Claude Agent SDK | —— | Python/多语言 | 活跃（Managed Agents 公测，2026-04） |

---

## 2. 各框架逐一分析

---

### 2.1 DeepAgents (LangChain)

**仓库**：https://github.com/langchain-ai/deepagents

**定位**：基于 LangGraph 构建的"batteries-included"Agent Harness，强调生产可用的持久化与上下文管理。

#### 2.1.1 Long-Term Memory 触发时机

DeepAgents 的长期记忆触发有两种路径：

**路径 A：Agent 主动写入（会话内实时触发）**

`MemoryMiddleware` 在 Agent 初始化时加载内存文件并注入 System Prompt，同时在 System Prompt 中显式指示 LLM：

> "call the `edit_file` tool to persist learning, with recommendations to update promptly—usually in the same turn once you have enough context."

即 LLM 在当前 Turn 内判断有新知识时，主动调用 `edit_file` 工具写入 `/memories/` 路径下的文件。

**路径 B：后台定期巩固（Background Consolidation，适用于高吞吐场景）**

通过 cron 调度运行独立的 consolidation agent，该 agent 调用 `search_recent_conversations()` 查询时间窗口内的历史 thread，提取关键事实后合并写入持久化存储。

```python
# consolidation agent 伪代码示意
recent_threads = search_recent_conversations(lookback_window=timedelta(days=1))
for thread in recent_threads:
    facts = extract_facts(thread.messages)
    store.put(namespace, "/memories/facts.md", facts)
```

#### 2.1.2 生成与更新方式

- **生成方式**：LLM 自主判断（没有规则化提取管道）。`MemoryMiddleware` 仅提供工具调用能力和 prompt 指引，具体何时写、写什么由模型自主决策。
- **更新方式**：增量更新。Agent 使用 `edit_file` 工具对已有文件做字符串替换或追加，而非全量重写。
- **Reflection 机制**：后台 consolidation 模式中存在类 reflection 机制——consolidation agent 作为独立角色对历史会话进行反思与提炼。在线（in-session）模式下无显式 reflection。
- **Summarization（上下文压缩时）**：触发上下文压缩后，旧消息被 LLM 摘要并以 markdown 格式追加写入 `/conversation_history/{thread_id}.md`，但该文件**不是**自动被下一个 session 加载的长期记忆，需要 Agent 显式读取才能利用。

#### 2.1.3 存储方式与结构

DeepAgents 通过 `CompositeBackend` 将不同路径路由到不同后端：

```python
backend = CompositeBackend(
    default=StateBackend(),   # 临时状态（会话内有效）
    routes={
        "/memories/": StoreBackend(namespace=lambda ctx: (ctx.user_id, "memories")),
        "/skills/":   StoreBackend(namespace=lambda ctx: (ctx.user_id, "skills")),
    }
)
```

| 组件 | 介质 | 范围 | 说明 |
|------|------|------|------|
| `StateBackend` | LangGraph thread state（内存+checkpoint） | 单 thread 内 | 临时文件/草稿 |
| `StoreBackend` | LangGraph `BaseStore`（默认 InMemoryStore；生产推荐 PostgresStore / MongoDB） | 跨 thread 持久化 | 真正的长期记忆 |

**数据结构**：

- 存储单元为键值对：`(namespace_tuple, file_path, store_value)`
- `store_value` 包含：`content`（UTF-8 文本或 base64 binary）、`encoding`、`created_at`、`modified_at`
- 支持 v1（`list[str]`）和 v2（plain `str`）格式，向后兼容
- **无 embedding / 向量索引**：检索仅支持精确 grep 和 glob，无语义搜索

**Namespace 隔离**：

- Agent 级别：`(assistant_id,)` ——跨所有用户共享同一 Agent 的知识
- 用户级别：`(user_id,)` ——每个用户独立存储，不互相泄漏偏好
- 组织级别：只读挂载，防止 prompt injection

#### 2.1.4 注入 Context 的方式

1. **启动加载**：`MemoryMiddleware.before_agent()` / `abefore_agent()` 调用 `backend.download_files()` 一次性下载所有配置的 `memory` 路径文件，存入 `state["memory_contents"]`。
2. **System Prompt 注入**：`modify_request()` 将内容格式化为 `<agent_memory>` 块附加到 system message 末尾。
3. **Anthropic Prompt Cache**：当模型为 Anthropic 系列时，自动加 `cache_control: ephemeral` breakpoint，减少重复加载的 token 成本。
4. **与压缩协同**：摘要（Summarization Middleware）触发后旧内容被 offload 到 backend 文件，active context 保留摘要引用。agent 需通过 `read_file` 工具显式读取历史，无自动跨 session 恢复。

#### 2.1.5 冲突、遗忘与衰减机制

- **冲突解决**：最后写入胜出（Last-Write-Wins）。文档建议通过将记忆拆分为多个 topic 文件来减少写入冲突。
- **遗忘/TTL**：无内置 TTL。生命周期由 LangGraph Store 后端决定（PostgresStore 可通过 SQL 自行实现 TTL）。
- **衰减**：无记忆衰减机制。
- **去重**：无自动去重，依赖 LLM 在写入时的自我判断或 consolidation agent 的批量处理。

---

### 2.2 OpenHarness (HKUDS)

**仓库**：https://github.com/HKUDS/OpenHarness

**定位**：开放式 Agent Harness，内置 Ohmo 个人 Agent，设计上高度参考 Claude Code，强调持久化记忆与多范围隔离。

#### 2.2.1 Long-Term Memory 触发时机

OpenHarness 的长期记忆采用 **Agent 主动工具调用触发** 模式。LLM 通过调用 `add_memory_entry()` / `remove_memory_entry()` 接口写入记忆，没有周期性后台触发。

关键函数签名（来自 `src/openharness/memory/manager.py`）：

```python
def add_memory_entry(
    title: str,
    content: str,
    memory_type: MemoryType = "project",
    scope: MemoryScope = "private",
    category: str | None = None,
    importance: int = 0,
    tags: list[str] | None = None,
    ttl_days: int | None = None,
) -> MemoryHeader
```

此外，上下文压缩（Auto-Compact）时有保留机制：`auto-compaction` 在 v0.1.6 引入，"preserves task state and channel logs across context compression"，但具体实现为状态持久化而非 LLM 摘要提炼。

#### 2.2.2 生成与更新方式

- **生成方式**：LLM 自主决策何时调用 `add_memory_entry`，无规则化提取管道。
- **去重检测**：写入时通过 `compute_memory_signature()`（SHA256 of normalized content）检测重复内容：若已存在相同签名的条目，则仅刷新时间戳而不创建新条目。
- **更新方式**：支持两种模式：
  - 新建（相同 slug 不存在时）
  - 刷新（内容相同时，仅更新 `updated_at`）
  - 软删除（`remove_memory_entry()` 标记为 disabled，不物理删除）
- **Reflection/Consolidation**：无内置 reflection 机制。
- **迁移工具**：`migrate.py` 提供 schema 版本迁移，带 dry-run 模式和自动备份，支持从旧格式升级到 schema-v1。

#### 2.2.3 存储方式与结构

**存储介质**：本地文件系统（Markdown 文件 + YAML Frontmatter）

**目录结构**：

```
.openharness/
├── agent/
│   ├── MEMORY.md          # 项目级记忆索引
│   ├── <slug>.md          # 单条记忆文件
│   └── ...
├── agent-memory-local/    # 本地私有记忆（不加入版本控制）
└── agent-memory-snapshots/# 快照（用于初始化新项目的预置记忆）

~/.openharness/data/       # 用户级全局记忆
```

**单条记忆文件 Schema**（YAML Frontmatter + Markdown body）：

```yaml
---
schema_version: "1"
id: "uuid-string"
name: "记忆标题"
description: "一句话摘要"
type: "project"          # user | feedback | project | reference
scope: "private"         # private | project | team
category: "knowledge"
importance: 5            # 整数，影响检索权重
source: "manual"
signature: "sha256hex"   # 内容去重指纹
created_at: "2026-01-01T00:00:00Z"
updated_at: "2026-01-01T00:00:00Z"
ttl_days: 30             # 可选，到期自动失效
disabled: false          # 软删除标志
supersedes: ["old-id"]   # 替代关系，用于记忆演化
tags: ["tag1", "tag2"]
---
正文内容（Markdown）
```

**团队记忆**：独立的 `team/` 子目录，写入前扫描敏感凭证（API key、私钥等），检出即拦截。

#### 2.2.4 注入 Context 的方式

**检索机制**：启发式加权评分（`search.py`），无向量嵌入，采用 token-level 关键词匹配：

| 匹配位置 | 权重 |
|---------|------|
| 标题/描述（frontmatter） | 2x |
| 正文内容 | 1x |
| importance 字段 | +0.4x |
| 使用频次（上限 5 次） | +0.5x |
| 近 14 天更新 | +0.3 |
| 近 30 天更新 | +0.1 |

- 默认检索最多 5 条（`max_results=5`），扫描上限 100 条文件
- 支持自定义 `MemorySelector` 回调，允许外部实现重排序
- 结果截断至 8000 字符注入 prompt

**注入位置**：System Prompt（通过 `load_memory_prompt()` 构建，由 `prompts/` 模块组装）

**使用跟踪**：`usage.py` 维护一个 JSON 索引，记录每条记忆的 `use_count` 和 `last_used_at`，用于影响检索权重和识别陈旧记忆。

#### 2.2.5 冲突、遗忘与衰减机制

- **冲突解决**：SHA256 签名去重（内容相同不创建新条目）；无两条记忆内容冲突时的自动仲裁
- **TTL**：frontmatter 中的 `ttl_days` 字段 + `is_memory_expired()` 检查（以 `updated_at` 或 `created_at` 为基准），扫描时自动过滤已过期条目
- **陈旧记忆识别**：`find_stale_memory_candidates()` 识别 importance ≤ 1、60 天未更新、use_count=0 的条目作为清理候选
- **软删除**：`disabled: true` 标记，不物理删除，便于审计
- **遗忘/衰减**：无自动遗忘曲线；依赖 TTL 和人工/LLM 主动清理

---

### 2.3 OpenCode (Anomaly)

**仓库**：https://github.com/anomalyco/opencode

**定位**：开源编程 Agent（TypeScript），168K+ Stars，专注代码任务。

**调研结论：资料不足**

经过多渠道调研（README、源码目录结构、官方文档），OpenCode 的 Long-Term Memory 实现细节**不可公开获取**：

- `src/` 目录下有 `memory/root-memory-files.ts` 文件，但仅实现 `MEMORY.md` 文件路径解析工具函数（路径规范化、遗留路径检测），不涉及记忆内容的生成与管理逻辑
- `packages/memory-host-sdk` 目录的 `src/` 为空
- README 未包含任何 Long-Term Memory 架构说明
- 官方文档站（opencode.ai/docs）无 Memory 专题章节

**已确认的有限信息**：

- 存在 `MEMORY.md` 文件作为持久化记忆载体（从 `root-memory-files.ts` 中 `resolveCanonicalRootMemoryPath()` 可推断）
- TypeScript 生态，代码库规模庞大（~280MB），但内存模块实现未公开

该框架的记忆实现最大可能与 Claude Code（其分叉来源）相似，但无法依据现有可访问代码做出确定性判断，故标注为**资料不足**。

---

### 2.4 OpenClaw

**仓库**：https://github.com/openclaw/openclaw

**定位**：376K+ Stars 的跨平台个人 AI 助手，强调"own-your-data"，拥有最成熟的分层记忆架构。

#### 2.4.1 Long-Term Memory 触发时机

OpenClaw 是本次调研中触发机制**最丰富**的框架，支持以下路径：

1. **Agent 实时写入**：LLM 在会话中随时通过工具写入 `MEMORY.md` 或日记文件（`memory/YYYY-MM-DD.md`）
2. **压缩前记忆刷新（Memory Flush before Compaction）**：上下文压缩前触发一个静默后台 turn，提示 Agent 将重要上下文写入磁盘，防止信息丢失
3. **Dreaming 系统（可选，定时批量触发）**：后台 cron 任务从 `memory/.dreams/` 短期缓存中筛选候选记忆，经评分后晋升到 `MEMORY.md`
4. **Grounded Backfill**：读取历史 `YYYY-MM-DD.md` 日记文件，回溯提取结构化条目写入 `DREAMS.md` 供人工审查
5. **Commitments（承诺系统）**：对话中推断的短期跟进任务（如"明天面试后回来告诉我结果"），作为独立的 commitment 条目存储，通过 heartbeat 在到期时触发

#### 2.4.2 生成与更新方式

- **生成方式**：LLM 自主判断写入时机与内容；Dreaming 系统引入评分门控（recall frequency + query diversity gates），具有轻度结构化提取特征
- **更新方式**：增量追加（日记文件）+ 人工/LLM 主动整理合并（MEMORY.md）
- **Reflection/Consolidation 机制**：**Dreaming 系统**是框架内最接近 reflection 的机制：
  - 短期记忆信号积累在 `memory/.dreams/` 中
  - Dreaming sweep 对候选条目打分（recall 频率 + query 多样性）
  - 通过质量门控的条目晋升为长期记忆
  - 相当于"睡眠巩固"（Sleep Consolidation）的计算模拟
- **DREAMS.md 人工审查**：提升内容会同步写入 DREAMS.md 供人工确认，保留人类决策权

#### 2.4.3 存储方式与结构

**三层文件系统架构**：

```
~/.openclaw/workspace/
├── MEMORY.md                  # 第3层：长期记忆（每次会话加载，compact精华）
├── DREAMS.md                  # Dreaming 审查日记（人工可读）
└── memory/
    ├── YYYY-MM-DD.md          # 第2层：每日工作记忆（近2天自动加载）
    ├── YYYY-MM-DD-<slug>.md   # 日记变体（hooks 注入）
    └── .dreams/               # 短期记忆缓存（dreaming 待处理队列）
```

第 1 层为 Session Context（JSONL 对话记录，存于运行时 context window）。

**向量存储**：OpenClaw 内置 SQLite + 向量索引（多 embedding 后端可选）：

| 后端 | 特性 |
|------|------|
| Builtin（默认） | SQLite + keyword + vector，无外部依赖 |
| QMD | 本地优先，带 reranking 和 query expansion |
| Honcho | 跨 session 用户建模 + 多 Agent 感知 |
| LanceDB | OpenAI compatible embeddings + 本地 Ollama |

**混合检索算法**（memory_search 工具）：

```
finalScore = vectorWeight × vectorScore + textWeight × textScore
```

向量相似度（BM25 语义）+ 精确 token 匹配（适用于 ID、代码符号、错误字符串）。

#### 2.4.4 注入 Context 的方式

- **Bootstrap 注入**：`MEMORY.md` 在每次会话启动时注入 system prompt（全量加载）
- **自动日记加载**：近 2 天的 `memory/YYYY-MM-DD.md` 自动加载
- **按需检索**：历史日记**不**自动注入，仅通过 `memory_search` 和 `memory_get` 工具按需检索
- **截断保护**：MEMORY.md 超出 bootstrap 预算时，磁盘文件保留完整，仅截断注入 context 的副本
- **DM 会话安全边界**：MEMORY.md 仅在私信（DM）会话中注入，在群组上下文中不暴露（防止信息泄漏）
- **与压缩协同**：压缩前执行 Memory Flush，确保重要信息写盘后再压缩，两者协同保证零信息损失

#### 2.4.5 冲突、遗忘与衰减机制

- **冲突解决**：无自动仲裁。文档明确说明"Memory 记录批准上下文，但不强制执行策略"——记忆内容冲突由 LLM 自行判断，强制约束由外部审批设置/沙箱保证
- **遗忘/TTL**：文档明确注明**无 TTL/遗忘机制**。长期记忆完全由 LLM 和人工主动整理，无自动过期
- **Dreaming 门控**：通过质量评分（recall frequency + query diversity）防止低价值内容晋升为长期记忆，起到间接"遗忘"作用
- **Commitments 到期**：短期承诺（commitment）有隐式时效，通过 heartbeat 到期交付后自然消亡

**已知架构局限（来自 GitHub Issue #50096）**：

MEMORY.md 全量注入是"token bomb"——文件增大后每个 turn 都要携带完整历史。社区提议引入 RAG 层、外部向量库（Qdrant、LanceDB）、graph memory（Cognee）和 context decay 协议。

---

### 2.5 HermesAgent (NousResearch)

**仓库**：https://github.com/NousResearch/hermes-agent

**定位**：176K+ Stars，自称"the agent that grows with you"，具备本次调研中**最完整的长期记忆闭环**，包括 FTS5 全文检索、Honcho 用户建模、8 个外部记忆提供者插件。

#### 2.5.1 Long-Term Memory 触发时机

Hermes 采用**多路并发触发**机制：

1. **周期性 nudge（主要路径）**：每 N 个用户 turn 触发一次记忆审查（`nudge_interval` 可配置），LLM 被提示检查当前会话是否有值得保存的新知识，自主决策写入 MEMORY.md / USER.md

   ```python
   # 来自 conversation_loop.py
   if (agent._memory_nudge_interval > 0
           and "memory" in agent.valid_tool_names
           and agent._memory_store):
       agent._turns_since_memory += 1
       if agent._turns_since_memory >= agent._memory_nudge_interval:
           _should_review_memory = True
           agent._turns_since_memory = 0
   ```

2. **压缩前记忆提取**：`on_pre_compress()` 在上下文压缩前被调用，外部提供者可从即将被压缩的消息中提取记忆（`agent.commit_memory_session(messages)`），**先保存后压缩**。

3. **会话结束**：`on_session_end()` 通知提供者，可执行 end-of-session 事实提取与汇总。

4. **委托完成**：`on_delegation()` 在子代理任务完成时通知，追踪跨代理的记忆更新。

5. **Gateway 模式强制刷新**：Gateway 创建新 Agent 实例前主动 flush 记忆，防止空闲超时时丢失未保存内容。

6. **外部提供者 sync**：每个 turn 结束后，`sync_all(user_content, assistant_content, messages, session_id)` 将对话数据同步给所有注册的提供者。

#### 2.5.2 生成与更新方式

**内置记忆（MEMORY.md / USER.md）**：

- **生成**：LLM 自主决定写入内容与时机（由 nudge 机制定期提示）
- **更新**：通过 `memory` 工具的 `replace`（子字符串替换）和 `remove`（子字符串删除）操作增量更新，无需精确匹配全文
- **容量管理**：MEMORY.md 约 2200 字符上限（~800 tokens），USER.md 约 1375 字符（~500 tokens），上限可配置

  ```yaml
  # ~/.hermes/config.yaml
  memory:
    memory_enabled: true
    user_profile_enabled: true
    memory_char_limit: 2200
    user_char_limit: 1375
  ```

- **80% 触发整理**：当内存超过 80% 容量时，LLM 被提示合并相关条目或删除过时内容

**SQLite 长期记忆表（`long_term_memory`）**：

```sql
-- 推断 schema（来自源码分析）
CREATE TABLE long_term_memory (
    id         INTEGER PRIMARY KEY,
    fact       TEXT NOT NULL,
    confidence REAL,
    source_session TEXT,
    created_at DATETIME,
    last_reinforced DATETIME
);

-- FTS5 全文搜索虚拟表
CREATE VIRTUAL TABLE conversation_fts USING fts5(
    content,
    session_id,
    ...
);
```

**外部提供者（8 个）**：

| 提供者 | 存储方式 | 检索方式 | 特色 |
|-------|---------|---------|------|
| Honcho | 云端，session-scoped | 语义搜索 | Dialectic 用户建模，per-session 摘要 |
| OpenViking | 本地文件系统层级 | 分层检索（abstract/overview/full） | 6 类自动提取 |
| Mem0 | 云端 | 服务端 LLM 事实提取 + 自动去重 | 近乎零维护 |
| Hindsight | 知识图谱（可本地/云端） | 多策略（实体解析 + 跨记忆合成） | 图谱推理 |
| Holographic | 本地 SQLite | FTS + HRR 代数查询 | 无依赖 |
| RetainDB | 云端 | Vector + BM25 + Reranking 混合 | 7 种记忆类型 |
| ByteRover | 本地层级树（可选云端同步） | 层级检索 | 压缩前提取（pre-compression extraction） |
| Supermemory | 云端语义 | 语义检索 | Context fencing 防递归污染 |

#### 2.5.3 存储方式与结构

三层存储体系：

| 层级 | 介质 | 生命周期 | 内容 |
|------|------|---------|------|
| MEMORY.md | 本地文件 `~/.hermes/memories/` | 永久（手动清理）| 精华知识，§分隔符条目 |
| USER.md | 本地文件 `~/.hermes/memories/` | 永久 | 用户画像、偏好、技能水平 |
| SQLite FTS5 | `~/.hermes/state.db` | 永久，all sessions | 全量对话记录，支持跨 session 搜索 |

MEMORY.md 内容格式使用 `§` 作为条目分隔符，每条记忆可多行。

#### 2.5.4 注入 Context 的方式

- **Snapshot 注入**：会话开始时将 MEMORY.md 和 USER.md 内容作为 frozen snapshot 注入 System Prompt（不是实时从文件读取，而是在 session 初始化时固定）
- **Prefetch（非阻塞）**：每个 turn 前，外部提供者以后台方式 prefetch 相关记忆，结果通过 `build_memory_context_block()` 构建并注入用户消息（ephemeral，不写入 session DB）
- **StreamingContextScrubber**：记忆上下文块以 `<memory-context>...</memory-context>` 标记包裹，流式输出时自动剥离该段内容（不显示给用户），防止记忆内容泄漏到 UI
- **与压缩协同**：压缩前 `on_pre_compress()` 允许提供者从待压缩消息提取记忆，`commit_memory_session()` 在 session 轮换前固化记忆，形成完整的信息守护链

#### 2.5.5 冲突、遗忘与衰减机制

- **冲突解决（内置）**：子字符串替换机制通过定位旧文本来更新，不允许重复插入相同 old_text；80% 容量触发时 LLM 主动合并冲突条目
- **冲突解决（外部提供者）**：
  - Mem0：服务端自动去重
  - Hindsight：实体解析（entity resolution），合并指向同一实体的不同描述
  - Supermemory：context fencing 防止已召回的记忆被再次捕获（防递归污染）
- **遗忘/TTL**：无内置 TTL。`confidence` 字段 + `last_reinforced` 支持外部实现衰减逻辑，但框架本身不自动衰减
- **陈旧识别**：`/insights [--days N]` 命令显示历史洞察，隐含支持按时间范围筛选
- **FTS5 会话搜索**：SQLite FTS5 作为长期"外挂记忆"，Agent 主动查询过去的对话，弥补 MEMORY.md 容量受限的不足

---

### 2.6 Claude Agent SDK (Anthropic)

**文档**：https://platform.claude.com/docs/en/managed-agents/memory

**定位**：Anthropic 官方 Managed Agents 平台的记忆服务（2026-04 公测），提供服务端托管的版本化记忆存储，配合 `memory_20250818` 客户端工具（2025-09 发布）构成完整的双层记忆体系。

#### 2.6.1 Long-Term Memory 触发时机

Claude Agent SDK 提供两种并列的记忆触发机制：

**机制 A：`memory_20250818` 工具（客户端主动触发，2025-09 发布）**

Claude 在每次对话开始时自动检查 `/memories` 目录（由 System Prompt 中自动注入的 MEMORY PROTOCOL 指令驱动），然后在任务过程中随时调用工具写入。System Prompt 指令：

```text
IMPORTANT: ALWAYS VIEW YOUR MEMORY DIRECTORY BEFORE DOING ANYTHING ELSE.
MEMORY PROTOCOL:
1. Use the `view` command of your `memory` tool to check for earlier progress.
2. ... (work on the task) ...
   - As you make progress, record status / progress / thoughts etc in your memory.
ASSUME INTERRUPTION: Your context window might be reset at any moment.
```

**机制 B：Managed Agents Memory Stores（服务端托管，2026-04 公测）**

- 会话启动时 attach memory store 到 session，记忆以 filesystem 形式 mount 到沙箱 `/mnt/memory/`
- Agent 使用标准文件工具（Read/Write）操作 memory，每次写入即触发不可变的 memory version 创建
- 无自动压缩前提取机制，记忆由 Agent 自主决策写入时机

**与 Dreaming/Consolidation 类机制的对应**：官方文档提供了"consolidation via dreaming session"的指导（`/docs/en/managed-agents/dreams`），通过独立的 consolidation session 将碎片化记忆合并到新的 output store，但该能力为独立可选模块。

#### 2.6.2 生成与更新方式

**`memory_20250818` 工具**：

- **生成**：LLM 自主判断，无规则化提取
- **更新命令**（6 个）：`view`、`create`、`str_replace`（子字符串精确替换）、`insert`（行号插入）、`delete`、`rename`
- `str_replace` 要求 old_str 在文件中唯一；多处匹配时拒绝执行，防止错误覆写

**Managed Agents Memory Stores**：

- **更新方式**：`memories.update` 接受 `content_sha256` 前置条件（乐观并发控制），仅当存储内容 hash 与读取时一致才允许更新，防止并发写入冲突
- **创建 vs 更新**：`memories.create` 不覆写；修改必须用 `memories.update`
- **预填充**：支持在会话开始前通过 API 预加载参考材料（如 GAAP 格式标准）

#### 2.6.3 存储方式与结构

**`memory_20250818` 工具（客户端）**：

- 存储介质：应用方自控（可以是本地文件系统、Redis、数据库、云存储等）
- 工具调用由应用层处理，SDK 提供 `BetaAbstractMemoryTool`（Python）/ `betaMemoryTool`（TypeScript）作为抽象基类
- 数据结构：自由文件系统（以 `/memories` 为根目录）

**Managed Agents Memory Stores（服务端）**：

| 属性 | 规格 |
|------|------|
| 存储单元 | 文本文档（memory），以 path 为键 |
| 单条上限 | 100 kB（约 25k tokens） |
| 每 store 上限 | 2,000 条记忆 |
| 每 session 上限 | 8 个 store |
| 隔离级别 | workspace 级别（按 store ID 隔离） |
| 版本化 | 每次修改创建不可变 memory version（`memver_...`） |
| 版本保留 | 30 天（近期版本不受此限制） |
| 访问控制 | `read_write`（默认）或 `read_only` |
| 归档 | 单向 archive（变只读，不可逆） |

AgentDefinition 的 memory scope（`memory: Literal["user", "project", "local"]`）控制哪类记忆被挂载给子代理：

```python
AgentDefinition(
    description="...",
    prompt="...",
    memory="project",   # "user" | "project" | "local"
)
```

#### 2.6.4 注入 Context 的方式

**`memory_20250818` 工具**：

- Claude 主动调用 `view` 命令读取 `/memories` 目录，**按需加载**（Just-in-Time），不自动全量注入 system prompt
- 读取结果作为 `tool_result` 出现在 context window 中，属于 ephemeral 注入
- System Prompt 中的 MEMORY PROTOCOL 指令确保每次任务开始时主动检查

**Managed Agents Memory Stores**：

- Store 以 filesystem 形式 mount 到 `/mnt/memory/`，自动向 System Prompt 追加每个 mount 点的描述（path、access mode、store description、instructions）
- Agent 使用 Read 工具读取具体文件，写入通过 Write 工具
- **无全量预加载**：Agent 需主动读取所需文件，而非全量加载到 context

**与压缩（Compaction）协同**：

> "memory persists important information across compaction boundaries so that nothing critical is lost in the summary"

Memory 和 Compaction 协同形成"不可压缩的持久层"：Compaction 压缩活跃对话，Memory 保存跨 compaction 边界的关键信息。

#### 2.6.5 冲突、遗忘与衰减机制

- **并发冲突**：`content_sha256` 乐观锁，hash 不匹配时写入失败，调用方需重新读取后重试
- **版本审计**：完整的不可变版本链，支持 point-in-time 回滚（手动重读旧 version 并 update）
- **版本 Redact**：合规场景可 redact 历史版本内容（清除 PII/敏感数据），保留审计元数据
- **TTL/遗忘**：无内置 TTL。官方建议：定期执行 dreaming consolidation session 合并碎片；2000 条上限达到时，通过 `memories.delete` 主动清理或 attach 新 store
- **安全隔离**：`read_only` mount 防止不可信输入（用户 prompt、爬取的 web 内容）写入 prompt injection 到记忆中
- **归档**：`memory_stores.archive` 实现 store 级别的软删除（变只读，无法附加到新 session）

---

## 3. 横向对比：五大维度矩阵

### 3.1 触发时机对比

| 框架 | Agent 主动写入 | Token 阈值触发 | 压缩前提取 | 周期后台触发 | 会话结束触发 | 显式工具调用 |
|------|:-----------:|:-----------:|:--------:|:---------:|:---------:|:---------:|
| DeepAgents | ✅ | ✗ | 部分（offload，不自动跨 session）| ✅（cron consolidation）| ✗ | ✅（edit_file） |
| OpenHarness | ✅ | ✗ | ✗ | ✗ | ✗ | ✅（add_memory_entry） |
| OpenCode | 未确认 | 未确认 | 未确认 | 未确认 | 未确认 | 未确认 |
| OpenClaw | ✅ | ✗ | ✅（Memory Flush） | ✅（Dreaming cron） | ✗ | ✅（memory_search/get） |
| HermesAgent | ✅ | ✗ | ✅（on_pre_compress） | ✅（nudge interval） | ✅（on_session_end）| ✅（memory tool） |
| Claude Agent SDK | ✅ | ✗ | ✗（需外部实现）| ✗（Dreaming 可选）| ✗ | ✅（memory_20250818 / file tools） |

### 3.2 生成与更新方式对比

| 框架 | 生成方式 | 更新粒度 | Reflection/Consolidation | 去重机制 |
|------|---------|---------|--------------------------|---------|
| DeepAgents | LLM 自主 | 增量（edit_file 字符串替换）| 后台 consolidation agent（可选）| 无 |
| OpenHarness | LLM 自主 | 新建/刷新/软删除 | 无 | SHA256 内容签名 |
| OpenCode | 未确认 | 未确认 | 未确认 | 未确认 |
| OpenClaw | LLM 自主 + Dreaming 评分门控 | 增量追加 + 人工整理 | Dreaming 系统（睡眠巩固模拟）| 评分门控（间接）|
| HermesAgent | LLM 自主（nudge 提示）| 子字符串替换（精准或模糊）| 外部提供者（Hindsight 知识图谱）| Mem0 自动去重、Hindsight 实体解析 |
| Claude Agent SDK | LLM 自主 | str_replace + 乐观并发锁 | Dreaming consolidation session（可选）| 乐观锁防并发覆写 |

### 3.3 存储方式对比

| 框架 | 主要介质 | 数据结构 | 向量检索 | Schema 复杂度 |
|------|---------|---------|---------|-------------|
| DeepAgents | LangGraph BaseStore（PostgreSQL/MongoDB/内存）| KV 文件（命名空间树）| 无（仅 grep/glob）| 低 |
| OpenHarness | 本地 Markdown 文件 | YAML Frontmatter + Markdown body | 无（关键词启发式）| 中（schema-v1 含 14 字段）|
| OpenCode | 本地 Markdown（MEMORY.md）| 推测为 flat Markdown | 未确认 | 未确认 |
| OpenClaw | 本地 Markdown + SQLite 向量索引 | 分层 Markdown + Embedding | ✅（Vector + BM25 混合）| 中 |
| HermesAgent | 本地文件 + SQLite（FTS5）+ 外部提供者（8 种）| §分隔条目 + SQL + 各提供者自定义 | ✅（外部提供者，如 Mem0/Honcho/RetainDB）| 高（多层混合）|
| Claude Agent SDK | 服务端托管存储（Managed Stores）+ 客户端自控（memory tool）| 路径寻址文件系统 | 无内置（可外部实现）| 低（简洁路径 + 内容）|

### 3.4 注入 Context 方式对比

| 框架 | 注入时机 | 注入位置 | 检索方式 | 与压缩协同 |
|------|---------|---------|---------|----------|
| DeepAgents | 启动时全量加载 | System Prompt 末尾（`<agent_memory>` 块）| 全量（启动时）+ grep/glob（按需）| offload 到文件；无自动跨 session |
| OpenHarness | 启动时 load_memory_prompt | System Prompt | 启发式加权评分（关键词）| Auto-compact 保留状态 |
| OpenCode | 未确认 | 未确认 | 未确认 | 未确认 |
| OpenClaw | 启动时（MEMORY.md）+ 按需（工具）| System Prompt + 工具返回 | 全量（MEMORY.md）+ 向量+BM25 混合（工具）| 压缩前 Memory Flush |
| HermesAgent | 启动时（MEMORY.md/USER.md 快照）+ 每 turn 预取 | System Prompt（快照）+ 用户消息（ephemeral 提供者块）| 快照（MEMORY.md）+ 语义（外部提供者）+ FTS5（session 搜索）| on_pre_compress 提取；commit_memory_session |
| Claude Agent SDK | 按需（JIT 工具读取）| 工具返回（memory tool）/ System Prompt 描述（Managed Store mount 点）| 按需文件读取（无向量）| 与 Compaction 互补，memory 守护跨 compaction 边界 |

### 3.5 冲突与遗忘机制对比

| 框架 | 冲突解决 | TTL/遗忘 | 衰减机制 | 审计/版本 |
|------|---------|---------|---------|---------|
| DeepAgents | Last-Write-Wins | 无内置 | 无 | 无 |
| OpenHarness | SHA256 去重；软删除 | ✅（frontmatter ttl_days）| 使用频次权重 + 陈旧识别（60 天）| 版本迁移（schema-v1）|
| OpenCode | 未确认 | 未确认 | 未确认 | 未确认 |
| OpenClaw | 无自动仲裁；人工管理 | 无明确 TTL | Dreaming 门控（间接）| DREAMS.md 人工审查 |
| HermesAgent | 子字符串替换；Mem0/Hindsight 自动去重 | 无内置 TTL（confidence + last_reinforced 可外部实现）| 无自动衰减 | SQLite 对话历史永久保留 |
| Claude Agent SDK | 乐观并发锁（SHA256 precondition）| 无内置 TTL；版本 30 天保留 | 无 | ✅ 不可变版本链 + Redact 合规 |

---

## 4. 设计模式提炼

### 模式一：Markdown 文件作为通用记忆载体

**采用框架**：所有 6 个框架（不同形式）

**核心理念**：Markdown 文件是 LLM 最擅长读写的格式，具备人类可读性、版本控制友好、工具调用直接。

**共同规律**：
- 文件命名为 `MEMORY.md`（或 `CLAUDE.md`、`AGENTS.md`）作为持久化入口
- 文件大小通常受限（100 行/25KB/2200 字符），超出则截断或归档
- LLM 直接用文本编辑工具（str_replace、edit_file）操作，无需额外 API

### 模式二：三层记忆架构（短期→工作→长期）

**代表框架**：OpenClaw（最清晰）、HermesAgent（多层混合）

```
Layer 3: Long-Term Memory (MEMORY.md) — 精华，每次会话全量注入
Layer 2: Working Memory (YYYY-MM-DD.md / 近期 session) — 近期上下文，按需加载
Layer 1: Session Context (Context Window) — 当前会话，自动管理
```

**启示**：三层架构在信息密度和 token 成本之间取得最优平衡——最高价值信息全量注入，中间层按需检索，当前 session 自动管理。

### 模式三：压缩前记忆守护（Pre-Compaction Memory Preservation）

**采用框架**：OpenClaw（Memory Flush）、HermesAgent（on_pre_compress + commit_memory_session）

**核心逻辑**：

```
context 压缩前 → 触发记忆提取 → 写入长期记忆 → 执行压缩
```

这形成了"无损压缩"——上下文窗口被压缩，但关键信息已被持久化，不随压缩而丢失。

### 模式四：全量注入 vs 按需检索的权衡

| 策略 | 优点 | 缺点 | 代表 |
|------|------|------|------|
| 全量注入（Bootstrap）| 零延迟，无检索错误 | Token 成本随记忆增长线性增加 | DeepAgents（启动），OpenClaw（MEMORY.md）|
| 按需 JIT 检索 | Token 高效，记忆可无限扩展 | 检索延迟，可能遗漏 | Claude memory_20250818，OpenClaw（工具）|
| 混合（精华全量 + 历史按需）| 两者兼得 | 工程复杂度较高 | HermesAgent，OpenClaw |

**最佳实践**：精华记忆（≤1000 tokens）全量注入 System Prompt，历史详情通过工具按需检索。

### 模式五：记忆提供者插件化（Memory Provider Plugin System）

**代表框架**：HermesAgent（8 个提供者）、DeepAgents（CompositeBackend）、OpenClaw（4 个后端）

**架构特征**：
- 定义抽象 `MemoryProvider` / `BackendProtocol` 接口
- 每次 turn 的生命周期钩子：`prefetch` → `sync` → `on_pre_compress` → `on_session_end`
- 强制单外部提供者约束（HermesAgent）防止工具 schema 膨胀

### 模式六：Dreaming 系统（Sleep Consolidation Simulation）

**代表框架**：OpenClaw、Claude Agent SDK（Dreaming session 可选）

**仿生设计**：模拟人类睡眠记忆巩固——白天积累短期信号，晚上批量筛选晋升为长期记忆。

**质量门控**：recall frequency + query diversity，防止低价值信息污染长期记忆。

---

## 5. 对 harness9 的设计启示与建议

### 5.1 当前状态分析

harness9 目前具备完善的**短期记忆**（`internal/memory/`）：

- SQLiteSession：WAL + 事务，会话内完整对话历史
- SummarizationCompactor：LLM 摘要压缩 + 增量更新（80% 阈值触发）
- TokenBudgetCompactor：字符截断回退方案

但**长期记忆（跨会话持久化）尚未实现**。以下是基于本次调研的具体设计建议。

### 5.2 触发时机建议

推荐采用 **三路触发** 机制：

**路径 A：压缩前自动提取（最高优先级）**

参考 HermesAgent 的 `on_pre_compress` 模式，在 `SummarizationCompactor` 触发之前，先调用记忆提取：

```go
// 在 memory/summarization.go 的 Compact() 中增加 pre-compaction hook
func (c *SummarizationCompactor) Compact(ctx context.Context, msgs []schema.Message) ([]schema.Message, error) {
    // Step 1: 压缩前记忆提取（如 LTM store 已配置）
    if c.ltmStore != nil {
        c.ltmStore.Extract(ctx, msgs) // 提取关键事实到长期记忆
    }
    // Step 2: 执行现有压缩逻辑
    return c.summarize(ctx, msgs)
}
```

**路径 B：Turn 粒度周期性 nudge（可选）**

参考 HermesAgent 的 `nudge_interval`，每 N turns 在 System Prompt 中注入一段"请检查是否有值得记忆的新知识"提示，由 LLM 自主决策是否调用 `memory_write` 工具。

**路径 C：显式工具调用**

通过新增 `memory_write` 和 `memory_search` 内置工具，允许 LLM 在任意 turn 主动写入/检索长期记忆。

### 5.3 存储结构建议

推荐参考 OpenHarness 的 **Markdown + YAML Frontmatter** 方案，利用已有的 SQLite 基础设施扩展：

**方案 A：文件系统（轻量，首选）**

```
~/.harness9/
├── memories/
│   ├── MEMORY.md          # 精华摘要（全量注入）
│   └── entries/
│       ├── <id>.md        # 单条详细记忆
│       └── ...
└── state.db               # 已有：短期记忆 + 新增：FTS 全文搜索
```

单条记忆 Schema（Go struct）：

```go
// internal/memory/ltm.go

type LTMEntry struct {
    ID          string    `json:"id"`
    Title       string    `json:"title"`
    Content     string    `json:"content"`
    Category    string    `json:"category"`    // "knowledge" | "preference" | "task" | "skill"
    Importance  int       `json:"importance"`  // 0-10
    CreatedAt   time.Time `json:"created_at"`
    UpdatedAt   time.Time `json:"updated_at"`
    TTLDays     int       `json:"ttl_days,omitempty"` // 0 = 永不过期
    Signature   string    `json:"signature"`           // SHA256(normalized content)
    Tags        []string  `json:"tags,omitempty"`
}
```

**方案 B：SQLite 扩展（推荐长期，可搜索）**

在 `state.db` 中新增 `long_term_memories` 表 + FTS5 虚拟表：

```sql
CREATE TABLE long_term_memories (
    id          TEXT PRIMARY KEY,
    title       TEXT NOT NULL,
    content     TEXT NOT NULL,
    category    TEXT,
    importance  INTEGER DEFAULT 0,
    signature   TEXT UNIQUE,          -- SHA256 去重
    created_at  DATETIME,
    updated_at  DATETIME,
    ttl_days    INTEGER,              -- NULL = 永不过期
    disabled    INTEGER DEFAULT 0,   -- 软删除
    tags        TEXT                  -- JSON 数组
);

CREATE VIRTUAL TABLE memories_fts USING fts5(
    title,
    content,
    content=long_term_memories,
    content_rowid=rowid
);
```

### 5.4 注入 Context 建议

推荐 **混合注入策略**（参考 HermesAgent + OpenClaw）：

1. **MEMORY.md 全量注入**：在 `DefaultPromptBuilder` 中，将 `~/.harness9/memories/MEMORY.md` 的内容（≤200 行 / ≤5000 字节）追加到 System Prompt 的固定位置，并利用 Anthropic 的 `cache_control` breakpoint 降低重复 token 成本。

2. **按需 FTS 检索（工具）**：通过新增 `memory_search(query)` 工具，利用 SQLite FTS5 进行全文检索，搜索结果以工具返回值形式注入当前 turn。

3. **注入位置**：MEMORY.md 精华部分注入 System Prompt；详细记忆通过工具 JIT 加载，避免"token bomb"问题。

### 5.5 冲突与遗忘建议

| 机制 | 推荐设计 |
|------|---------|
| 去重 | SHA256(normalize(content)) 签名，写入前检查，内容相同时刷新 updated_at |
| TTL | frontmatter `ttl_days` 字段，扫描时自动过滤已过期条目（`updated_at + ttl_days < now`）|
| 容量管理 | MEMORY.md 内容超出 N tokens 时，在下次 nudge 提示 LLM 整理合并 |
| 软删除 | `disabled = true` 标记，不物理删除（保留审计历史）|
| 冲突仲裁 | 通过 LLM 的 `str_replace` 操作（精确 old_text 匹配）实现有意图的更新，减少意外覆写 |

### 5.6 最小可行实现路径（MVP）

建议分三阶段实现：

**Phase 1（1-2 周）：MEMORY.md 读写**

- 新增 `memory_write` 工具（向 `~/.harness9/memories/MEMORY.md` 追加或替换条目）
- 在 `DefaultPromptBuilder` 中注入 MEMORY.md 内容
- 在 `SummarizationCompactor` 中增加 pre-compaction nudge 提示

**Phase 2（2-3 周）：结构化存储 + FTS**

- 在 `state.db` 中新增 `long_term_memories` + FTS5 表
- 新增 `memory_search(query)` 工具
- SHA256 去重、TTL 过滤、陈旧条目识别

**Phase 3（后续）：高级特性**

- Dreaming consolidation 后台任务（cron 触发，批量筛选晋升）
- 外部提供者插件接口（参考 HermesAgent 的 `MemoryProvider` 接口设计）
- 向量嵌入（可接入本地 Ollama 或 OpenAI Embeddings）

---

## 6. 参考资料

以下所有 URL 已通过 WebFetch 确认可正常访问，内容与调研主题相关。

### 官方文档与源码

- [DeepAgents Long-Term Memory 官方文档](https://docs.langchain.com/oss/python/deepagents/long-term-memory) — DeepAgents 官方 LTM 配置与 CompositeBackend 路由说明
- [DeepAgents GitHub 仓库](https://github.com/langchain-ai/deepagents) — 源码，包含 `backends/store.py`、`middleware/memory.py`、`middleware/summarization.py`
- [OpenHarness GitHub 仓库](https://github.com/HKUDS/OpenHarness) — 源码，`src/openharness/memory/` 目录完整实现
- [OpenClaw 官方文档 - Memory 概念](https://docs.openclaw.ai/concepts/memory) — 三层架构、Dreaming 系统、Commitment 系统完整说明（经 WebSearch 验证）
- [OpenClaw docs/concepts/memory.md 源码](https://github.com/openclaw/openclaw/blob/main/docs/concepts/memory.md) — 原始文档
- [HermesAgent 持久化记忆官方文档](https://hermes-agent.nousresearch.com/docs/user-guide/features/memory) — MEMORY.md/USER.md 格式、nudge 机制、容量管理
- [HermesAgent 记忆提供者文档](https://hermes-agent.nousresearch.com/docs/user-guide/features/memory-providers) — 8 个外部提供者详细说明
- [Anthropic Managed Agents Memory 官方文档](https://platform.claude.com/docs/en/managed-agents/memory) — Memory Stores 完整 API、版本化、并发控制、最佳实践
- [Anthropic Memory Tool 官方文档](https://platform.claude.com/docs/en/agents-and-tools/tool-use/memory-tool) — memory_20250818 工具的 6 个命令、安全考量、与 Compaction 协同

### 深度分析文章

- [LangChain Blog：Launching Long-Term Memory Support in LangGraph](https://www.langchain.com/blog/launching-long-term-memory-support-in-langgraph) — LangGraph BaseStore 设计理念，namespace 隔离，跨 thread 持久化
- [OpenClaw GitHub Issue #50096：Long-Term Memory & Knowledge Management](https://github.com/openclaw/openclaw/issues/50096) — 社区对 MEMORY.md "token bomb" 问题的深度讨论与解决方案提案
- [OpenClaw GitHub Issue #9386：Working Memory for Long-Horizon Agent Tasks](https://github.com/openclaw/openclaw/issues/9386) — WORKING.md 工作记忆提案，多天跨 session 任务场景

### 相关实现参考

- [NousResearch/hermes-agent memory.md 源码](https://github.com/NousResearch/hermes-agent/blob/main/website/docs/user-guide/features/memory.md) — 官方文档原始 Markdown
- [Claude Agent SDK Python 文档 - 会话存储](https://github.com/anthropics/claude-agent-sdk-python/blob/main/_autodocs/configuration.md) — SessionStore 接口、S3/Redis 配置示例
- [Anthropic Engineering Blog - Effective Context Engineering](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents) — 官方上下文工程最佳实践（memory tool 的设计背景）
