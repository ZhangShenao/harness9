# Agent Harness 框架文件系统使用方式深度调研

**调研日期**：2026-05-22
**关注焦点**：Context Offload 机制、执行计划持久化、文件与 Session 绑定、Tool-Calling 与文件系统交互

---

## 各框架详细分析

### DeepAgents（LangChain）

**基础信息**：Python，23,181 stars，MIT 协议

#### Context Offload 机制——最完备

DeepAgents 实现了调研框架中最系统化的 Context Offload，分两层：

**第一层：FilesystemMiddleware（工具结果自动 offload）**

源文件 `libs/deepagents/deepagents/middleware/filesystem.py`（91.6 KB）。当工具调用结果超过阈值（默认 **20,000 tokens**）时，完整内容写入文件，context 中替换为截断预览 + 文件路径引用：

- 写入路径：`/large_tool_results/<tool_call_id>`
- context 提示文本：`"Message content too large and was saved to the filesystem at: {file_path}"`，附带 head/tail 预览
- 排除工具：`ls`、`glob`、`grep`、`read_file`、`edit_file`、`write_file`（原因：这些工具要么已自截断，要么重复 offload 会形成循环）
- System prompt 动态注入使用指引，告知 agent 可通过 `read_file` 配合分页参数检索 `/large_tool_results/` 目录

**用户消息 Eviction**：超过 **50,000 tokens** 的用户消息 offload 至 `/conversation_history/{uuid}.md`，消息对象打 `lc_evicted_to` 标记。

**第二层：SummarizationMiddleware（对话历史 offload）**

源文件 `libs/deepagents/deepagents/middleware/summarization.py`（67.4 KB）：

- 发生 `ContextOverflowError` 时，被淘汰消息追加写入 `{artifacts_root}/conversation_history/{thread_id}.md`（按 ISO 时间戳分节，格式为 markdown）
- Summary 消息中嵌入文件路径引用，agent 后续可通过 `read_file` 反查完整历史
- 发生 overflow 时进一步将大型保留 tail offload 到 `/large_tool_results/` 下各工具调用的独立文件
- `TruncateArgsSettings` 可在正式摘要前对老消息工具调用参数轻量截断（`write_file` 内容、`edit_file` patch、`execute` 输出），默认上限 2000 字符
- offload 与摘要生成并发执行以降低延迟

**后端抽象**

| 后端 | 特点 |
|------|------|
| `StateBackend` | 文件存于 LangGraph 状态内存，ephemeral，session 结束消失 |
| `FilesystemBackend` | 直接操作本地文件系统，`root_dir` 为 base，无 session 级目录隔离 |
| `SandboxBackend` | 沙箱环境 |
| `CompositeBackend` | 组合多个 backend |

#### 执行计划持久化

无独立 Plan 文件机制。通过 **AGENTS.md** 实现跨 session 持久化知识：
- 加载路径：`["~/.deepagents/AGENTS.md", "./.deepagents/AGENTS.md"]`
- Agent 通过调用 `edit_file` 工具更新 AGENTS.md，建议在同一 turn 内立即写入
- HTML 注释（`<!-- ... -->`）注入 system prompt 前自动剥除

#### 文件与 Session 绑定

- `conversation_history/{thread_id}.md`：以 LangGraph `thread_id` 为 session 标识
- `large_tool_results/<tool_call_id>`：以工具调用 ID 为文件名，天然唯一
- `StateBackend` 模式下文件存于内存，session 结束即消失；`FilesystemBackend` 持久化但无自动 GC
- 无 session 级目录结构，所有文件共享同一 root_dir

---

### OpenHarness（HKUDS）

**基础信息**：Python，12,920 stars，MIT 协议

#### Context Offload 机制

无"写入文件 + 路径引用"式 offload。Context 管理通过两种机制：
- **Auto-Compaction**：在 `engine/` 子模块中实现，保留 task state 和 channel logs，支持多天长跑，压缩结果保留在内存/session 状态，不 offload 到文件
- **CLAUDE.md 动态注入**：自动检测项目目录下的 CLAUDE.md 并注入 context

#### 执行计划持久化

`tasks/manager.py` 实现了**部分持久化**：
- 每个任务启动时创建输出日志：`{tasks_dir}/{task_id}.log`，以 4KB chunk 持续追加 stdout
- 任务重启时追加重启通知（`_TASK_RESTART_NOTICE`）
- 任务元数据（`TaskRecord`：状态、命令、创建时间等）仅保存在内存中，**进程重启后丢失**

#### 文件与 Session 绑定

记忆目录路径计算（`memory/paths.py`）：

```python
# {data_dir}/memory/{project_name}-{sha1_hash_12chars}/
# 主入口文件：{memory_dir}/MEMORY.md
# SHA1 hash 取工作目录绝对路径前 12 位，确保同名项目唯一性
```

每个 memory entry 命名为 `{slug}.md`，MEMORY.md 作为 markdown 索引维护所有条目链接。Memory entry metadata 含 `scope`（project/team）、`ttl_days`（过期天数）、`signature`（去重 hash）。

任务输出文件通过 `get_tasks_dir()` 获取路径，以 `task_id` 为文件名，无 session 级隔离。

---

### OpenCode（Anomaly）

**基础信息**：TypeScript，163,777 stars，MIT 协议

#### Context Offload 机制

`packages/opencode/src/session/compaction.ts` 实现 context 管理，**不使用文件系统 offload**：
- 保留最近 N 轮（默认 2）+ `PRUNE_PROTECT` tokens 的工具调用历史，对老的工具调用输出标记 `pruned`
- Summary 模板结构化 markdown，覆盖：Goal、Constraints、Progress、Key Decisions、Next Steps、Critical Context、Relevant Files 七个维度
- Summary 作为 assistant message 部分存于数据库（`msg.info.summary = true`），全程 SQLite（Drizzle ORM）

#### 执行计划持久化——独特亮点

Plan 文件持久化到 markdown（`packages/opencode/src/session/session.ts`）：

```typescript
export function plan(input: { slug: string; time: { created: number } }, instance: InstanceContext) {
  const base = instance.project.vcs
    ? path.join(instance.worktree, ".opencode", "plans")
    : path.join(Global.Path.data, "plans")
  return path.join(base, [input.time.created, input.slug].join("-") + ".md")
}
```

- **Git 项目**：`<worktree>/.opencode/plans/<timestamp>-<slug>.md`（可随 git 提交，天然版本化）
- **非 Git 项目**：`<data_directory>/plans/<timestamp>-<slug>.md`

Todo 项通过 Drizzle ORM 持久化到 SQLite 的 `TodoTable`，以 session ID 绑定，支持跨进程恢复：

```typescript
// 删除该 session 的全部 todos，重新插入（全量替换）
db.delete(TodoTable).where(eq(TodoTable.session_id, input.sessionID))
// 插入新 todos，含 position 字段用于排序
```

#### 文件与 Session 绑定——双轨制存储

**SQLite 主存储**（`~/.opencode/db` 或 `Global.Path.data`）：

| 表/实体 | 存储内容 |
|---------|---------|
| `sessions` | 会话元数据（ID、slug、projectID、token 用量、成本统计） |
| `messages` | 完整消息历史 |
| `parts` | 消息细粒度片段（step-start、step-finish、工具调用等） |
| `TodoTable` | 待办事项，以 sessionID 绑定 |

**文件系统辅助存储**：

```
{storage_root}/
├── project/{projectID}.json
├── session/{projectID}/{sessionID}.json        # 聚合统计（从 session_diff 迁移后）
├── session_diff/{sessionID}.json               # 详细 diff 数据（Migration 2 拆分出）
└── migration                                   # 迁移版本标记
```

Migration 2 将 session 内的 diffs 数组提取到独立的 `session_diff/{sessionID}.json`，session 文件仅保留 additions/deletions/files 聚合统计，优化元数据查询性能。

Plan 文件与 session 通过 slug（session slug）和 timestamp 命名绑定，无独立关联表。

---

### OpenClaw

**基础信息**：TypeScript，373,837 stars，MIT 协议

#### Context Offload 机制

通过 Context Engine 框架（`src/context-engine/`）实现可插拔 context 管理：
- 接口定义三个核心方法：`ingest()`（摄入消息）、`assemble()`（组装 context，含 token budget）、`compact()`（压缩）
- `compact()` 接受 `sessionFile` 参数，返回值可含更新后的 `sessionFile`，支持"safe branch-and-reappend transcript rewrites"
- 具体压缩实现委托给内置 runtime（`compact.runtime.js`），源码未见直接文件写入逻辑

记忆系统（`src/memory/root-memory-files.ts`）管理 `MEMORY.md`（规范名）和 `memory.md`（遗留名），`.openclaw-repair/root-memory` 为修复目录，有严格的符号链接过滤。

#### 执行计划持久化

`src/tools/planner.ts` 的 `buildToolPlan` 实现工具级 plan（排序 + 可用性分类为 visible/hidden），是运行时结构，不写入文件。无独立任务执行计划持久化到文件系统的机制。

#### 文件与 Session 绑定

Session key 体系支持多层级嵌套（`agent:{agentId}:{rest}`、`agent:{agentId}:cron:{...}:run:{...}`、`subagent:...`、`acp:{...}`、`{base}:thread:{threadId}`），但 key 到存储路径的映射依赖具体存储插件，核心代码未见明确文件目录结构定义。`memory-host-sdk` 和 `plugin-sdk` 是可扩展的外部接入点。

---

### HermesAgent（NousResearch）

**基础信息**：Python，162,127 stars

#### Context Offload 机制——三层防御

`tools/tool_result_storage.py` 实现调研框架中最精细的工具输出 offload：

**第一层**：工具自截断（per-tool output cap）

**第二层**：单结果持久化（per-result persistence）
- 超过工具阈值的输出写入磁盘，context 保留预览 + 文件路径引用

**第三层**：轮次总预算 offload（per-turn aggregate budget）
- 收集完一轮所有工具结果后，若总大小 > **200,000 字符**，将最大的未持久化结果依次 spill 到磁盘

存储路径：
```python
# 优先：env.get_temp_dir() + "/hermes-results"
# 备用：/tmp/hermes-results（Linux）或 $TMPDIR/hermes-results（Termux）
# 文件命名：{storage_dir}/{tool_use_id}.txt
```

写入通过 `env.execute()` 以 stdin 管道方式，规避 Linux `MAX_ARG_STRLEN`（~128 KB）限制：

```bash
mkdir -p {storage_dir} && cat > {remote_path}
```

此设计支持所有后端（local、Docker、SSH、Modal、Daytona），agent 通过 `read_file` 配合 offset/limit 分页检索。

**轨迹压缩**（`trajectory_compressor.py`，针对训练数据）：保留首几轮（system、human、first GPT、first tool）和最后 N 轮，中间轮次替换为单条 `[CONTEXT SUMMARY]: ...` 人类摘要消息，保留后续工具调用完整。

#### 执行计划持久化——最复杂

`tools/checkpoint_manager.py` 实现了 git-based shadow store，位于 `~/.hermes/checkpoints/`：

```
~/.hermes/checkpoints/
├── store/                    # 共享 bare git 仓库（跨所有项目）
├── refs/hermes/<hash16>      # 每项目分支 tip（16 位工作目录 hash）
├── indexes/<hash16>          # 每项目 git index 文件（防并发冲突）
└── projects/<hash16>.json    # 每项目元数据：{"workdir, created_at, last_touch"}
```

- 触发时机：文件变更类操作（`write_file`、`patch`、`terminal` 带破坏性标志）完成后，每 turn 自动触发一次（turn 内去重）
- 维护清理：孤儿检测、`retention_days` 过期淘汰、`max_total_size_mb` 大小限制、`git gc --prune=now`，旧版 shadow repo 迁移为 `legacy-<timestamp>/` 归档而非删除

HermesAgent 的 TodoStore 是**纯内存**实现，`format_for_injection()` 在 context 压缩后将 todo 重注入 context，但 session 重启后丢失。

#### 文件与 Session 绑定

**SQLite 主存储**（`~/.hermes/state.db`，WAL 模式，FTS5 + trigram tokenizer 支持 CJK）：
- `sessions` 表：完整元数据 + token 计数 + 成本数据
- `messages` 表：完整对话历史，multimodal 内容序列化为 `\x00json:...`
- `parent_session_id`：session 压缩后创建子 session，链式追踪

**完整目录结构**：
```
~/.hermes/
├── config.yaml
├── state.db                  # SQLite（FTS5 + WAL）
├── logs/{agent,errors,gateway}.log
├── skills/{name}/            # agent 创建的 skill 文件
│   ├── .archive/             # 归档（永不删除）
│   └── .usage.json           # 使用统计
├── cron/.tick.lock           # 防重入文件锁
└── checkpoints/              # git-based 文件快照
```

**FileStateRegistry**（`tools/file_state.py`）：进程级单例，跟踪多子 agent 间文件读写时序：
- 每 agent 的读取记录（path → mtime + read_ts + partial）
- 全局 last-writer 记录
- 路径级读写锁（序列化 read→modify→write）
- 最大容量：每 agent 4096 paths，全局 4096 writers
- `HERMES_DISABLE_FILE_STATE_GUARD=1` 可关闭

**写入保护**（`file_operations.py`）：
- `WRITE_DENIED_PATHS`：绝对路径黑名单（`/etc/shadow`、`~/.ssh/id_rsa` 等）
- `WRITE_DENIED_PREFIXES`：目录前缀黑名单（`/sys`、`/proc` 等）
- `HERMES_WRITE_SAFE_ROOT`：可选沙箱根目录限制

---

### Claude Agent SDK (Anthropic)

#### Context Offload 机制

SDK 层本身不提供工具输出 offload 到文件的框架机制。Context 压缩由底层 Claude Code 进程内核处理，对 SDK 调用方透明（session-message schema 定义了 `Compaction` 消息类型）。

#### 执行计划持久化

通过 **Session JSONL 文件**实现跨进程 context 恢复（间接等效于 plan 状态恢复）：

```
~/.claude/projects/<encoded-cwd>/<session-id>.jsonl
```

`<encoded-cwd>` 为工作目录绝对路径编码：所有非字母数字字符替换为 `-`（如 `/Users/me/proj` → `-Users-me-proj`）。每个 session 对应一个 JSONL 文件，包含完整对话历史。

Session Fork 创建新的独立 JSONL 文件，原 session 文件不变，两个文件独立演化。

#### 文件与 Session 绑定

Session ID 与 JSONL 文件名直接对应（`{session-id}.jsonl`），`cwd` 编码后作为目录名实现多项目隔离：

| API（Python / TypeScript） | 功能 |
|--------------------------|------|
| `list_sessions()` / `listSessions()` | 枚举磁盘 session 文件 |
| `get_session_messages()` / `getSessionMessages()` | 读取 session 消息历史 |
| `get_session_info()` / `getSessionInfo()` | 获取 session 元信息 |
| `rename_session()` / `renameSession()` | 重命名 session |
| `tag_session()` / `tagSession()` | 打标签 |

`persistSession: false`（TypeScript only）关闭磁盘写入，session 仅存内存。跨主机 resume 需将 JSONL 文件复制到新主机同路径，且 `cwd` 必须匹配。

#### Tool-Calling 与文件系统——File Checkpointing

SDK 最具特色的文件系统功能：
- 跟踪 `Write`、`Edit`、`NotebookEdit` 工具对文件的变更（Bash 命令不被跟踪）
- 每个 user message 带 UUID，作为 checkpoint 标识（需配置 `replay-user-messages: null`）
- `rewind_files(checkpoint_id)` / `rewindFiles(checkpointId)` 将文件回滚到任意历史状态
- 回滚仅恢复文件内容，不回滚 session 对话历史
- 支持多个 restore point（存储所有 user message UUID 数组）

**CLAUDE.md / Memory 文件**：SDK 默认从 `.claude/` 和 `~/.claude/` 加载 Skills（`.claude/skills/*/SKILL.md`）、Slash commands（`.claude/commands/*.md`）、Memory（`CLAUDE.md` 或 `.claude/CLAUDE.md`）。

---

## 横向对比表格

### 核心机制对比

| 特性 | DeepAgents | OpenHarness | OpenCode | OpenClaw | HermesAgent | Claude Agent SDK |
|------|-----------|-------------|---------|---------|------------|-----------------|
| **工具输出 Offload** | 有，阈值 20K tokens | 无 | 无（内联截断） | 无 | 有，三层防御，200K 字符 | 无（SDK 层） |
| **用户消息 Offload** | 有，阈值 50K tokens | 无 | 无 | 无 | 无 | 无 |
| **对话历史 Offload** | 有，追加写 markdown | 无 | 无 | 无 | 无（SQLite 链式 session） | 无（JSONL 全量保留） |
| **read_file 反查机制** | 有，system prompt 注入指引 | 无 | 无 | 无 | 有，offset/limit 分页 | 无（框架层） |
| **Plan 文件持久化** | 无 | 部分（任务日志） | 有，`.opencode/plans/*.md` | 无 | 无（内存 todo） | 无（JSONL session） |
| **Checkpoint/快照** | 无 | 无 | SQLite 事务 | sessionFile 机制 | 有，git-based shadow store | 有，file checkpointing |
| **记忆文件** | AGENTS.md | MEMORY.md（sha1+hash 路径） | 无专属记忆文件 | MEMORY.md（规范 + 遗留名） | 无 | CLAUDE.md |
| **Session 存储** | LangGraph thread state | 内存（部分日志文件） | SQLite（Drizzle ORM） | 插件化（可扩展） | SQLite（FTS5 + WAL） | JSONL 文件 |
| **文件 GC/生命周期管理** | 无自动 GC | memory 有 ttl_days | 无自动 GC | 未知 | 有，retention_days + size cap + git gc | 无自动 GC |
| **路径安全/沙箱** | virtual_mode root_dir 约束 | permissions 模块 | contains/overlaps 路径检测 | boundary 检测 | WRITE_DENIED_PATHS + WRITE_SAFE_ROOT | 工作目录约束 |
| **多 agent 文件冲突防护** | 无 | 无 | 无 | 无 | 有，FileStateRegistry（进程级 singleton） | 无 |

### 文件路径结构对比

| 框架 | 工具输出 offload 路径 | Session 存储路径 | Plan/记忆路径 |
|------|---------------------|-----------------|--------------|
| DeepAgents | `/large_tool_results/<tool_call_id>` | LangGraph thread | `~/.deepagents/AGENTS.md` |
| OpenHarness | 无 | 内存 | `{data_dir}/memory/{project}-{hash12}/MEMORY.md` |
| OpenCode | 无 | `~/.opencode/db`（SQLite） | `<worktree>/.opencode/plans/<ts>-<slug>.md` |
| OpenClaw | 无 | 插件化 | `MEMORY.md`（workspace 根） |
| HermesAgent | `/tmp/hermes-results/<tool_use_id>.txt` | `~/.hermes/state.db`（SQLite + FTS5） | `~/.hermes/skills/`，`~/.hermes/checkpoints/` |
| Claude Agent SDK | 无（SDK 层） | `~/.claude/projects/<cwd-encoded>/<session-id>.jsonl` | `CLAUDE.md`，`.claude/skills/*/SKILL.md` |

---

## 设计模式提炼

### Context Offload 的两种流派

**流派一：主动 Offload（Push-to-Disk）**——DeepAgents、HermesAgent

超过阈值立即写磁盘，context 替换为路径引用 + 摘要预览，system prompt 告知 agent 如何检索。

- 优点：context 不因单次大输出爆炸；完整数据可随时全量检索
- 缺点：依赖可靠文件系统；文件生命周期管理复杂（/tmp 可能被清理）；agent 需理解"路径引用"模式，增加推理成本

**流派二：就地压缩（In-Place Compaction）**——OpenCode、Claude Agent SDK（内核）

通过摘要/截断减小 context 体积，summary 直接作为消息保留在 context，不落盘为中间文件。

- 优点：无文件系统依赖；context 结构简洁；对 agent 透明
- 缺点：完整历史不可恢复；摘要质量依赖 LLM 能力

### Plan 持久化的三种模式

| 模式 | 代表框架 | 特点 |
|------|---------|------|
| **内存模式** | HermesAgent TodoStore | 简单，session 重启后丢失 |
| **数据库模式** | OpenCode TodoTable（SQLite） | 跨进程可靠，支持复杂查询，sessionID 绑定 |
| **文件模式** | OpenCode Plans（.md） | 人类可读，可随 git 提交，天然版本化 |

---

## 对 harness9 项目的参考意义与实施建议

### 现状评估

| 维度 | harness9 现状 | 对标框架 | 差距 |
|------|-------------|---------|------|
| Session 持久化 | SQLite WAL | HermesAgent | 已对齐，无需改动 |
| Context 压缩 | SummarizationCompactor | DeepAgents SummarizationMiddleware | 已对齐，无需改动 |
| 工具输出 | 无 offload，直接进入 context | HermesAgent 三层防御 | **主要差距** |
| Plan 持久化 | TodoStore 仅内存 | OpenCode SQLite + .md 文件 | 有改进空间 |
| 文件系统 offload | **无** | DeepAgents / HermesAgent | **功能缺失** |

### 建议 A：工具输出 Context Offload（高优先级）

当前 read_file 截断至 4096 字节，bash 工具无上限截断，超大输出（长 bash 日志、大文件内容）会直接占满 context 并触发 SummarizationCompactor，在长任务场景下造成有效信息被压缩丢失。

建议参考 HermesAgent 三层防御，在 `internal/engine/agent_loop.go` 工具执行后、结果注入 context 前执行 offload 逻辑：

```
层级 1：工具自截断（已有）
  - read_file：4096 字节
  - bash：建议增加输出上限（如 8192 字节）

层级 2：单工具结果 offload（新增）
  - 阈值：单次工具输出 > 10,000 字符
  - 写入路径：~/.harness9/tool_results/{session_id}/{tool_call_id}.txt
  - context 替换为：
    "[输出已保存至 {path}，共 {n} 行。可通过 read_file 工具分页读取。
     预览（前 20 行）：\n{head}\n...（已截断）]"

层级 3：轮次总预算 offload（新增，可选）
  - 阈值：一轮所有工具输出总计 > 50,000 字符
  - 将最大的未 offload 结果依次 spill
```

实施要点：
- Session ID 作为子目录，保证文件隔离；tool_call_id 作为文件名
- Session 删除时（`internal/memory/manager.go` 的 `DeleteSession`）级联清理 offload 目录
- 可扩展现有 `read_file` 工具支持 offset/limit 参数，无需新增工具

### 建议 B：Plan 持久化增强（中优先级）

TodoStore 目前仅保存在内存中，进程重启后丢失。建议将 TodoStore 持久化到现有 SQLite：

在 `internal/memory/` 下增加 todo 表（`sqlite_session.go` 扩展），以 session_id 绑定，支持跨进程恢复 plan 状态。

可选：额外生成人类可读的 Plan markdown 文件（参考 OpenCode），路径为：
- Git 项目：`{workdir}/.harness9/plans/{timestamp}-{session_id_prefix}.md`
- 非 Git 项目：`~/.harness9/plans/{timestamp}-{session_id_prefix}.md`

### 建议 C：Session 目录结构标准化（低优先级）

引入工具输出 offload 后，建议规划清晰的目录结构：

```
~/.harness9/
├── sessions.db                              # SQLite 主存储（现有）
├── tool_results/                            # 工具输出 offload（建议 A 新增）
│   └── {session_id}/
│       └── {tool_call_id}.txt
├── plans/                                   # 非 git 项目 plan 文件（建议 B 可选）
│   └── {timestamp}-{session_slug}.md
└── logs/
    └── harness9.log
```

### 建议 D：Offload 文件 GC（低优先级）

DeepAgents 和 HermesAgent（除 checkpoint manager 外）均无完善的文件 GC，是已知技术债。建议：
- Session 删除时级联清理对应 tool_results 子目录
- 提供 CLI 子命令（如 `harness9 gc`）清理孤立文件
- 参考 HermesAgent 的 `retention_days` 机制，支持按天数过期清理

### 总结

| 维度 | 最佳实践来源 | 对 harness9 的适用性 |
|------|------------|-------------------|
| 工具输出 Offload | HermesAgent（三层防御）+ DeepAgents（路径引用模式） | **强烈建议实施**，Go 实现难度低 |
| Plan 持久化 | OpenCode（SQLite + 可选 markdown） | **建议实施** SQLite 方案（最小改动） |
| Session 存储 | harness9 现有 SQLite WAL 已与 HermesAgent 对齐 | 无需改动 |
| Context 压缩 | harness9 现有 SummarizationCompactor 与各框架对齐 | 无需改动 |
| File Checkpointing | Claude Agent SDK（最完整） | 可选，适合高安全性需求场景 |
| 记忆文件（MEMORY.md） | OpenHarness（hash 路径唯一化最精细） | 可考虑支持 AGENTS.md 加载 |
| 多 agent 文件冲突防护 | HermesAgent FileStateRegistry | 当前单 agent，暂不需要 |

**核心结论**：harness9 在 context 压缩（SummarizationCompactor）和 session 存储（SQLite WAL）上已达到主流框架水平。主要差距是**工具输出 Offload 机制的缺失**——在长任务场景（bash 大输出、长文件读取）下 context 会被非关键内容占满，建议优先补齐这一能力。实现成本在 Go 中相对较低：在 `runLoop` 中拦截工具结果，超阈值写文件，替换为摘要引用即可。
