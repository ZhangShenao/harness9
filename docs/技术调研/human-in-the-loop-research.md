# Human-in-the-Loop 与 Hooks/Middleware 权限控制深度调研报告

> 调研日期：2026-05-27  
> 目标分支：`human-in-the-loop`  
> 调研框架：Claude Agent SDK、DeepAgents、OpenHarness、OpenCode、OpenClaw、HermesAgent

---

## 一、各框架调研结果

### 1. Claude Agent SDK (Anthropic)

**语言**: Python + TypeScript（双 SDK）

#### 1.1 执行权限模式（Permission Modes）

六种模式，按限制程度排列：

| 模式 | 行为 |
|------|------|
| `default` | 无自动批准；未匹配工具触发 `canUseTool` 回调 |
| `dontAsk` | 未预批准的工具直接拒绝；`canUseTool` 永不触发 |
| `acceptEdits` | 文件操作（Edit/Write 及 filesystem 命令）自动批准 |
| `bypassPermissions` | 跳过所有权限检查（极度危险，仅受控环境使用） |
| `plan` | 只读工具可运行；Claude 只探索代码库不执行修改 |
| `auto`（TypeScript 专属）| 模型分类器自动判断每次工具调用是否批准 |

**权限评估顺序（严格管道，按优先级）**：

```
1. Hooks（PreToolUse）    → 可直接 deny，但不跳过后续规则
2. Deny 规则              → 即使 bypassPermissions 模式也生效
3. Permission Mode        → bypassPermissions 批准一切到达此步的工具
4. Allow 规则（白名单）    → 预批准匹配请求
5. canUseTool 回调        → dontAsk 模式下直接拒绝
```

**关键细节**：
- 裸工具名 deny 规则（如 `Bash`）从 Claude 上下文中完全移除工具定义
- 作用域 deny 规则（如 `Bash(rm *)`）只匹配特定参数模式
- `acceptEdits` 的文件操作路径限定在 `workDir` 及 `additionalDirectories` 范围内
- 支持运行时调用 `setPermissionMode()` 切换模式

**subagent 权限继承**：父 Agent 使用 `bypassPermissions`/`acceptEdits`/`auto` 时，所有 subagent 强制继承且不可覆盖。

#### 1.2 Human-in-the-Loop 实现（canUseTool 回调）

**触发条件**：工具调用未被 Hooks、deny 规则、permission mode、allow 规则任何一层拦截时，最终到达 `canUseTool` 回调。

**数据结构**（TypeScript 版）：

```typescript
canUseTool: async (toolName: string, input: ToolInput, options: {
    signal: AbortSignal,
    suggestions?: PermissionUpdate[]  // SDK 推荐的规则建议
}) => PermissionResult

// 允许响应
{ behavior: "allow", updatedInput: input }

// 拒绝响应
{ behavior: "deny", message: "用户拒绝：原因描述" }

// 允许并记忆（写入 .claude/settings.local.json）
{ behavior: "allow", updatedInput: input, updatedPermissions: suggestions }
```

**暂停/恢复机制**：
- `canUseTool` 回调可无限期挂起，Agent 执行在回调返回前完全暂停
- 从 Hook 返回 `permissionDecision: "defer"` 使进程退出，之后通过 `resume: sessionId` 恢复执行
- 整个 session 状态持久化为 JSONL 文件，恢复时完整还原上下文

#### 1.3 Hooks/Middleware 拦截系统

**可用 Hook 事件**（15+ 种）：

| 事件 | 触发时机 | 典型用途 |
|------|----------|----------|
| `PreToolUse` | 工具调用前 | 拦截危险命令、修改参数 |
| `PostToolUse` | 工具执行后 | 审计日志、结果注入 |
| `PostToolUseFailure` | 工具执行失败后 | 错误处理 |
| `PostToolBatch` | 整批工具完成后 | 批量上下文注入 |
| `UserPromptSubmit` | 用户提交 prompt 时 | 注入额外上下文 |
| `PermissionRequest` | 权限对话框出现时 | 自定义权限处理 |
| `Stop` | Agent 执行停止时 | 保存状态 |
| `Notification` | Agent 状态消息 | 推送 Slack/PagerDuty |
| `SubagentStart/Stop` | 子 Agent 生命周期 | 追踪并行任务 |
| `PreCompact` | 上下文压缩前 | 归档完整对话 |

**Hook 决策系统**（`PreToolUse` 特有）：

```typescript
// 允许
{ hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "allow" } }

// 拒绝（即使 bypassPermissions 模式也生效）
{ hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "deny",
    permissionDecisionReason: "不允许写入 .env 文件" } }

// 请求用户确认
{ hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "ask" } }

// 挂起，稍后恢复（进程可退出）
{ hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "defer" } }

// 修改工具参数
{ hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "allow",
    updatedInput: { file_path: "/sandbox/..." } } }
```

**优先级规则**：多个 Hook 并发执行，`deny > defer > ask > allow`；任一 Hook 返回 deny 则操作被阻止。

**Matcher 设计**：正则表达式匹配工具名，`"Write|Edit"` 只匹配写操作，`"^mcp__"` 匹配所有 MCP 工具，不填 matcher 则匹配所有工具。

**异步 Hook**（日志场景）：返回 `{ async: true, asyncTimeout: 30000 }`，Agent 不等待 Hook 完成即继续执行。

#### 1.4 命令白名单机制

**配置层次**（多层合并）：

```json
// .claude/settings.json（项目级，可提交到 git）
{
    "permissions": {
        "allow": ["Bash(npm *)", "Read", "Glob"],
        "deny": ["Bash(rm -rf *)", "Write(/etc/*)"],
        "ask": ["Bash(sudo *)"]
    }
}
```

```typescript
// 程序化配置（运行时）
const options = {
    allowedTools: ["Read", "Glob", "Grep"],
    disallowedTools: ["Bash(rm *)", "Write"],
    permissionMode: "dontAsk"
}
```

**动态白名单更新**：用户选择"记住此决定"时，SDK 将规则写入 `.claude/settings.local.json`，后续匹配请求自动跳过询问。

---

### 2. DeepAgents (LangChain)

**语言**: Python，MIT，依托 LangGraph 运行时

#### 2.1 执行权限模式

DeepAgents **没有独立的权限模式系统**，安全控制通过以下机制组合实现：

**FilesystemPermission**（唯一正式权限原语）：

```python
@dataclass
class FilesystemPermission:
    operations: list[str]      # "read" | "write"
    paths: list[str]           # glob 模式，必须以 / 开头
    mode: Literal["allow", "deny"] = "allow"
```

规则评估顺序：按声明顺序，第一个匹配规则的 mode 决定结果；无规则匹配时默认 allow。禁止 `..` 穿越、`~` 扩展、非 `/` 开头路径。

**Trust 模型（THREAT_MODEL.md 文档化）**：
- 框架不验证模型选择、工具注册、system prompt、backend 配置的安全性
- `LocalShellBackend` 为 opt-in，开启后 shell 以进程所有者完整权限执行
- 文件内容注入 system prompt 不做任何清洗（verbatim 注入）

#### 2.2 Human-in-the-Loop 实现（LangGraph interrupt）

**核心机制**：通过 LangGraph 的 `interrupt()` 原语 + `checkpointer` 状态持久化实现：

```python
agent = create_deep_agent(
    model=my_model,
    tools=[...],
    interrupt_on={"edit_file": True},
    checkpointer=SqliteSaver.from_conn_string(":memory:")
)
```

**实现原理**（通过 `HumanInTheLoopMiddleware`）：

```python
if interrupt_on is not None:
    deepagent_middleware.append(
        HumanInTheLoopMiddleware(interrupt_on=interrupt_on)
    )
```

`HumanInTheLoopMiddleware` 委托给 LangGraph 图节点中断机制：在工具执行节点调用 `interrupt()` 抛出中断异常 → LangGraph 捕获并保存 checkpoint → 应用层用新 input 恢复执行 → 从 checkpoint 恢复图状态后继续。

**Subagent 继承**：声明式 SubAgent spec 默认继承顶层 `interrupt_on` 配置；显式指定的 subagent `interrupt_on` 覆盖继承值。

#### 2.3 Middleware 系统

Middleware 在 LLM 调用前（而非工具调用前）触发，可动态过滤工具、注入 system prompt：

内置 middleware：`FilesystemMiddleware`（权限）、`MemoryMiddleware`（记忆）、`SummarizationMiddleware`（压缩）、`HumanInTheLoopMiddleware`（HITL）、`TodoListMiddleware`（计划）。

---

### 3. OpenHarness (HKUDS)

**语言**: Python，MIT，活跃维护，安全优先，多层防御

#### 3.1 执行权限模式

**三层权限模式**：

```python
class PermissionMode(str, Enum):
    DEFAULT = "default"     # 变更类操作需用户确认
    PLAN = "plan"           # 只读工具可运行，变更类工具被阻止
    FULL_AUTO = "full_auto" # 允许所有非黑名单操作（需 tirith 安全扫描）
```

**PermissionChecker 评估顺序**：

```
1. 敏感路径保护（硬编码，最高优先级，不可覆盖）
   - SSH 密钥、AWS/GCP/Azure 配置、Docker/Kubernetes 凭据
2. 明确 tool 黑名单/白名单（denied_tools / allowed_tools）
3. 路径级 glob 规则（path_rules，fnmatch 模式匹配）
4. 命令 pattern 拒绝（denied_commands）
5. PermissionMode 评估
   - FULL_AUTO: 允许所有未被前四步拦截的操作
   - PLAN: 阻止变更类工具
   - DEFAULT: 变更操作需用户确认
```

**只读工具豁免**：`is_read_only=True` 的工具在任何模式下均直接通过，不经过权限检查。

#### 3.2 Human-in-the-Loop 实现

**权限回调接口**：

```python
# 类型定义
PermissionPrompt = Callable[[str, str], Awaitable[bool]]
# 参数：(tool_name, reason)
# 返回：True 允许 / False 拒绝

decision = context.permission_checker.evaluate(tool_name, ...)
if not decision.allowed and decision.requires_confirmation:
    confirmed = await context.permission_prompt(tool_name, decision.reason)
    if not confirmed:
        return ToolResultBlock(content=decision.reason, is_error=True)
```

`permission_prompt` 是 async 回调，`await` 期间 Agent 完全暂停。权限询问发生时触发 `NOTIFICATION` 事件（`type: "permission_prompt"`），允许 Slack、PagerDuty 接收通知。

#### 3.3 Hooks/Middleware 系统

**四种 Hook 类型**：

```python
class CommandHookDefinition(BaseModel):
    command: str               # shell 命令，$ARGUMENTS 被替换为 JSON payload
    matcher: str | None        # fnmatch 模式，匹配工具名
    timeout_seconds: int       # 1-600，默认 30
    block_on_failure: bool = False

class PromptHookDefinition(BaseModel):
    prompt: str               # LLM 验证 prompt（处理语义层面危险性判断）
    timeout_seconds: int
    block_on_failure: bool = True

class HttpHookDefinition(BaseModel):
    url: str
    headers: dict[str, str] | None
    timeout_seconds: int = 30
    block_on_failure: bool = False

class AgentHookDefinition(BaseModel):
    prompt: str               # 使用 Agent 模式（扩展推理）验证
    timeout_seconds: int = 60
    block_on_failure: bool = True
```

**执行结果聚合**：任一 Hook 返回 blocked 则操作被阻止（OR 语义）。

**LLM Hook 的独特价值**：`PromptHookDefinition` 用 LLM 判断工具调用的语义危险性，可处理正则规则无法覆盖的边界情况（如"这条命令的意图是否具有破坏性？"）。

---

### 4. OpenCode (Anomaly)

**语言**: TypeScript，MIT，Effect 函数式编程风格，事件驱动

#### 4.1 执行权限模式（Permission V2）

**错误类型体系**：

```typescript
class RejectedError extends Schema.TaggedErrorClass<RejectedError>("RejectedError") {}
class CorrectedError extends Schema.TaggedErrorClass<CorrectedError>("CorrectedError") {
    readonly feedback: string  // 用户的修改建议（供 LLM 调整策略）
}
class DeniedError extends Schema.TaggedErrorClass<DeniedError>("DeniedError") {}
```

**权限评估逻辑**：

```typescript
function evaluate(permission: string, input: unknown, ruleset: Ruleset) {
    for (const rule of ruleset) {
        if (matches(rule.pattern, input)) {
            if (rule.action === "deny") return yield* new DeniedError()
            if (rule.action === "allow") return "allow"
            if (rule.action === "ask") // 继续到 ask 流程
        }
    }
    return "ask"  // 无规则匹配时默认请求确认
}
```

#### 4.2 Human-in-the-Loop 实现（Deferred Effect）

**事件驱动的异步审批流**：

```typescript
// Service.ask() — 发起权限请求（挂起）
async ask(permission: string, input: unknown) {
    const deferred = Deferred.make()
    pending.set(requestId, deferred)
    Event.publish(Bus, { type: "permission.asked", requestId, permission, input })
    return yield* deferred  // 挂起等待用户决定
}

// Service.reply() — 提交用户决定（恢复）
async reply(requestId: string, decision: "once" | "always" | "reject") {
    const deferred = pending.get(requestId)
    if (decision === "once") Deferred.succeed(deferred, "allow")
    else if (decision === "always") {
        approved.push({ permission, pattern, action: "allow" })  // 写入白名单
        Deferred.succeed(deferred, "allow")
    } else if (decision === "reject") {
        Deferred.fail(deferred, new RejectedError())  // 级联拒绝同 session pending 请求
    }
}
```

**`CorrectedError`**（独有设计）：用户不只可以拒绝，还可以提供 `feedback` 字符串（如"不要删文件，请改为归档"），Agent 读取 feedback 后调整执行策略。

#### 4.3 命令白名单机制

"always" 决定实时写入 `approved` 列表，后续匹配请求自动通过，数据库持久化。

---

### 5. OpenClaw

**语言**: TypeScript，MIT，个人 AI 助手定位

#### 5.1 执行权限模式

```typescript
type ApprovalMode = "ask" | "never" | "auto" | "trusted"

interface AgentRunParams {
    approvalMode?: ApprovalMode
    approvals?: ApprovalsConfig   // 目前 SDK Gateway 不支持
}
```

**审批 API**：

```typescript
class ApprovalsNamespace {
    async list(params?: ListParams): Promise<ApprovalRequest[]>
    async respond(approvalId: string, decision: Record<string, unknown>): Promise<unknown>
}
```

OpenClaw 有独立安全审计层（`src/security/`，106 个文件），偏向"安全诊断"（识别配置风险）而非运行时工具拦截。部分审批功能仍处于 WIP 状态。

---

### 6. HermesAgent (NousResearch)

**语言**: Python，MIT，以 callback 注入替代内置 HITL，安全层外置化

#### 6.1 执行权限模式

**工具层循环守卫**：

```python
@dataclass
class ToolCallGuardrailConfig:
    exact_failure_warn_threshold: int = 2
    exact_failure_block_threshold: int = 5
    same_tool_failure_warn_threshold: int = 3
    same_tool_failure_halt_threshold: int = 8
    idempotent_noprogress_block_threshold: int = 5
```

**危险命令识别**（多层正则 + 外部 tirith 扫描器）：

```python
_DESTRUCTIVE_PATTERNS = [...]  # 正则表达式列表

def _is_destructive_command(command: str) -> bool:
    return any(re.search(pattern, command) for pattern in _DESTRUCTIVE_PATTERNS)
```

**Tirith 安全扫描**（外部二进制）：
- 检测同形字 URL 攻击、pipe-to-interpreter 攻击（`curl xxx | bash`）、终端注入
- 返回码：0=allow, 1=block, 2=warn
- SHA-256 校验 + cosign 供应链验证

**提示注入防护**（`agent/threat_patterns.py`）：
- 检测 "ignore all instructions" 及变体（词插入抗性）
- 检测角色劫持、C2 心跳模式、环境变量外泄
- 不可见字符集（17 个零宽 unicode 字符）

#### 6.2 Human-in-the-Loop 实现（Callback 注入）

HermesAgent 采用**完全 opt-in 的 callback 注入方式**：

```python
def set_approval_callback(cb):
    """注册危险命令审批回调（线程局部存储，ACP 并发会话隔离）"""
    _callback_tls.approval = cb

def execute_terminal_command(command: str, force: bool = False):
    if not force:
        approval = _check_all_guards(command, approval_callback=_get_approval_callback())
        if approval["status"] == "pending_approval":
            return { "status": "pending_approval", "command": command }
    # 批准后执行（或 force=True 时跳过检查）
```

**状态回路**：工具返回 `{"status": "pending_approval"}` → 调用方显示审批界面 → 用户批准后以 `force=True` 重新调用工具。这是**手动状态机**，而非框架层的 suspend/resume。

---

## 二、横向设计对比

### 2.1 权限模式对比

| 框架 | 权限模式 | 默认安全策略 |
|------|---------|------------|
| **Claude Agent SDK** | 6 种（明确分级，行为定义精确） | default，未匹配触发 canUseTool |
| **OpenHarness** | 3 种（DEFAULT/PLAN/FULL_AUTO） | DEFAULT，变更需确认 |
| **DeepAgents** | 无独立模式，依赖工具注册 | StateBackend，无 shell，默认安全 |
| **OpenCode** | 规则引擎（ask/allow/deny） | 无规则匹配时默认 ask |
| **OpenClaw** | 4 种（ask/never/auto/trusted） | 部分功能 WIP |
| **HermesAgent** | 无正式模式，Callback opt-in | 默认无审批门控（YOLO） |

### 2.2 Human-in-the-Loop 机制对比

| 框架 | 暂停机制 | 状态保存 | 恢复方式 | 超时处理 |
|------|----------|----------|----------|----------|
| **Claude Agent SDK** | async 回调挂起；defer 退出进程 | JSONL session 文件 | `resume: sessionId` | 可无限挂起；defer+resume 跨进程 |
| **DeepAgents** | LangGraph `interrupt()` 抛异常 | checkpointer（SQLite/Redis） | 提供新 input 重新 invoke | 依赖 checkpointer TTL |
| **OpenHarness** | `await permission_prompt()` 挂起 | 无内置跨进程持久化 | 同进程 callback 返回 | 无内置超时 |
| **OpenCode** | Effect `Deferred` 挂起，事件总线通知 | `pending` map（进程内） | `Service.reply()` | 进程重启丢失 pending |
| **OpenClaw** | 工具返回 `requiresApproval: true`；REST API poll | 外部服务 | `ApprovalsNamespace.respond()` | 未明确 |
| **HermesAgent** | 工具返回 `pending_approval`；手动状态机 | 无内置 | 以 `force=True` 重调 | 无内置 |

### 2.3 Hook 系统对比

| 框架 | Hook 触发时机 | 类型 | 决策能力 | 修改参数 |
|------|-------------|------|----------|----------|
| **Claude Agent SDK** | 15+ 事件（工具前后、会话、通知、subagent 等） | async 回调 | allow/deny/ask/defer | 支持（updatedInput） |
| **OpenHarness** | PRE/POST_TOOL_USE / NOTIFICATION | command/prompt/http/agent 四种 | block（OR 语义） | 不支持 |
| **DeepAgents** | LLM 调用前（middleware）| 类继承 | 修改工具列表、system prompt | 支持 |
| **OpenCode** | 会话/消息生命周期事件 | async 函数 | push 消息 | 不支持 |
| **HermesAgent** | 无正式 hook 系统 | callback 注入 | allow/block/pending | 不支持 |

### 2.4 命令白名单机制对比

| 框架 | 粒度 | 配置方式 | 动态更新 | 持久化 |
|------|------|----------|----------|--------|
| **Claude Agent SDK** | 工具名 + 参数 pattern（`Bash(rm *)`） | 代码 + settings.json | 运行时切换；"记住"写入 settings.local.json | 文件 |
| **OpenHarness** | 工具名 + 路径 glob + 命令 pattern | 配置类 | 重启生效 | 配置文件 |
| **DeepAgents** | 工具名（注册过滤）+ 文件路径 glob | 构造函数参数 | 无运行时更新 | 无 |
| **OpenCode** | 权限名 + pattern（expand `~/`） | 配置对象 + DB | "always" 决定实时写入 DB | 数据库 |
| **HermesAgent** | 正则 pattern + 路径边界 | 硬编码 + 路径参数 | 代码更新 | 无 |

---

## 三、关键设计模式提炼

### 模式 1：分层防御管道（Claude Agent SDK）

```
[Hooks] → [Deny Rules] → [Permission Mode] → [Allow Rules] → [canUseTool]
```

每层独立拦截，有明确的优先级顺序。即使 `bypassPermissions`，Hook 的 deny 仍然生效。这是最健壮的设计：在自动化场景下 Hook 提供硬编码的安全底线，在交互场景下 `canUseTool` 提供运行时灵活性。

### 模式 2：LLM-as-Judge Hook（OpenHarness）

```python
class PromptHookDefinition:
    prompt: str  # "评估以下工具调用是否具有破坏性：$ARGUMENTS"
```

用 LLM 自身判断工具调用的语义危险性，可处理正则规则无法覆盖的边界情况。代价是额外一次 LLM 调用的延迟。

### 模式 3：Effect Deferred 异步审批（OpenCode）

```
ask() → Deferred.make() → Event.publish("permission.asked") → yield* deferred
                                                                         ↑
reply() → Deferred.succeed/fail() ────────────────────────────────────────┘
```

完全非阻塞的异步审批流。多个并发权限请求可以并行挂起，UI 通过 `list()` 轮询待处理请求。reject 可以级联取消同 session 的所有 pending 请求。`CorrectedError` 允许用户提供反馈而非只是 allow/deny。

### 模式 4：手动状态机审批（HermesAgent）

```
工具执行 → 返回 {"status": "pending_approval"} → 调用方显示 UI
                                                              ↓
                                              用户决定 → 以 force=True 重调工具
```

最简单但最耦合，框架无需内置暂停机制，但调用方需要管理状态。

### 模式 5：LangGraph Checkpoint 暂停（DeepAgents）

```
LangGraph 图节点 → interrupt() → 持久化 checkpoint → 进程可退出
                                                              ↓
                                    新输入 + resume → 从 checkpoint 恢复图状态
```

支持跨进程、持久化的 HITL，对长时间异步任务最合适（如 multi-day 工作流）。依赖 LangGraph 运行时。

---

## 四、对 harness9 项目的设计建议

基于以上调研，针对 harness9（Go 语言，ReAct 循环，`internal/hooks/hook.go` 已有基础 Hook 机制），提出以下具体设计建议：

### 4.1 权限模式设计

**建议定义三种模式**（与 OpenHarness 对齐，适合 Go 实现的简洁性）：

```go
// internal/permission/mode.go
type PermissionMode int

const (
    // PermissionModeDefault 标准模式：变更类操作触发 Human-in-the-Loop
    PermissionModeDefault PermissionMode = iota
    // PermissionModeAutoEdit 自动批准所有文件修改（相当于 Claude SDK 的 acceptEdits）
    PermissionModeAutoEdit
    // PermissionModeYOLO 跳过所有确认（相当于 bypassPermissions，仅受控环境使用）
    PermissionModeYOLO
)
```

**只读工具豁免**：read_file、bash（只读命令）等工具应直接绕过权限检查，与 OpenHarness 的 `is_read_only` 设计一致。

### 4.2 扩展现有 ToolHook 接口

当前 `ToolHook` 接口只有 BeforeExecute/AfterExecute，不能表达人类审批的异步挂起。建议扩展为：

```go
// internal/hooks/hook.go — 扩展
type HookDecision int

const (
    HookDecisionAllow  HookDecision = iota // 继续执行
    HookDecisionDeny                        // 拒绝，返回 error 给 LLM
    HookDecisionAsk                         // 触发 Human-in-the-Loop
)

type HookResult struct {
    Decision    HookDecision
    DenyReason  string          // Decision=Deny 时的拒绝原因（LLM 可读）
    UpdatedArgs json.RawMessage // 可选：修改后的工具参数
}

// ToolHook v2：BeforeExecute 返回 HookResult 而非 error
type ToolHook interface {
    BeforeExecute(ctx context.Context, tc schema.ToolCall) (HookResult, error)
    AfterExecute(ctx context.Context, tc schema.ToolCall, result schema.ToolResult) schema.ToolResult
}
```

### 4.3 Human-in-the-Loop 核心接口

```go
// internal/approval/approval.go

type ApprovalRequest struct {
    ID        string
    ToolName  string
    ToolArgs  json.RawMessage
    Reason    string    // 为什么需要审批（来自 Hook 或权限规则）
    CreatedAt time.Time
}

type ApprovalDecision struct {
    RequestID   string
    Behavior    ApprovalBehavior
    UpdatedArgs json.RawMessage // 可选：修改工具参数
    Feedback    string          // Deny 时的修改建议（给 LLM 看）
    Remember    bool            // true → 写入白名单，后续自动批准
}

type ApprovalBehavior int
const (
    ApprovalAllow ApprovalBehavior = iota
    ApprovalDeny
)

// ApprovalHandler 是 Human-in-the-Loop 回调接口
// TUI 层实现：显示审批对话框；CLI 层实现：打印并等待用户输入
// 返回前 Agent 循环完全暂停（通过 goroutine channel 阻塞实现）
type ApprovalHandler interface {
    RequestApproval(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error)
}
```

### 4.4 审批 Hook 实现

```go
// internal/hooks/approval_hook.go
type ApprovalHook struct {
    handler     approval.ApprovalHandler
    matcher     *regexp.Regexp   // nil = 匹配所有工具
    autoApprove *WhitelistStore  // 运行时白名单
}

func (h *ApprovalHook) BeforeExecute(ctx context.Context, tc schema.ToolCall) (HookResult, error) {
    // 1. 检查自动批准白名单
    if h.autoApprove.IsApproved(tc.Name, tc.Arguments) {
        return HookResult{Decision: HookDecisionAllow}, nil
    }
    // 2. matcher 不匹配则跳过
    if h.matcher != nil && !h.matcher.MatchString(tc.Name) {
        return HookResult{Decision: HookDecisionAllow}, nil
    }
    // 3. 触发人类审批（阻塞等待）
    req := approval.ApprovalRequest{
        ID: uuid.New().String(), ToolName: tc.Name,
        ToolArgs: tc.Arguments, CreatedAt: time.Now(),
    }
    decision, err := h.handler.RequestApproval(ctx, req)
    if err != nil {
        return HookResult{Decision: HookDecisionDeny, DenyReason: err.Error()}, nil
    }
    if decision.Behavior == approval.ApprovalDeny {
        reason := "用户拒绝此操作"
        if decision.Feedback != "" {
            reason = decision.Feedback
        }
        return HookResult{Decision: HookDecisionDeny, DenyReason: reason}, nil
    }
    // 4. 用户选择"记住" → 写入白名单
    if decision.Remember {
        h.autoApprove.Add(tc.Name, tc.Arguments)
    }
    if decision.UpdatedArgs != nil {
        return HookResult{Decision: HookDecisionAllow, UpdatedArgs: decision.UpdatedArgs}, nil
    }
    return HookResult{Decision: HookDecisionAllow}, nil
}
```

### 4.5 命令白名单机制

```go
// internal/permission/whitelist.go

type WhitelistEntry struct {
    ToolName string  // "*" 表示所有工具
    Pattern  string  // 参数模式（支持 glob）；"" 匹配任意参数
    Scope    Scope   // Global / Session
}

type Scope int
const (
    ScopeGlobal  Scope = iota // 全局生效
    ScopeSession              // 当前 session 生效（进程退出丢失）
)

type WhitelistStore struct {
    mu      sync.RWMutex
    entries []WhitelistEntry
    persist func(entry WhitelistEntry) error // nil = 不持久化
}

func (s *WhitelistStore) Add(toolName string, args json.RawMessage) {
    entry := WhitelistEntry{ToolName: toolName, Scope: ScopeSession}
    s.mu.Lock()
    s.entries = append(s.entries, entry)
    s.mu.Unlock()
    if s.persist != nil {
        _ = s.persist(entry)
    }
}
```

### 4.6 TUI 审批对话框设计（关键用户体验）

```
┌─────────────────────────────────────────────────────────────┐
│ 审批请求                                           [1/2 待审批] │
├─────────────────────────────────────────────────────────────┤
│ 工具: bash                                                    │
│ 命令: rm -rf ./dist                                          │
│                                                               │
│ [y] 允许    [n] 拒绝    [e] 编辑命令    [a] 记住允许此模式  │
└─────────────────────────────────────────────────────────────┘
```

- `y` = `ApprovalAllow`，使用原始参数
- `n` = `ApprovalDeny`，提示用户输入 Feedback（供 LLM 调整策略）
- `e` = 编辑命令后以 `UpdatedArgs` 允许（参考 Claude SDK 的 `updatedInput`）
- `a` = 允许并记住（`Remember: true`），写入 session 级白名单

### 4.7 引擎层集成建议

当前 harness9 的工具并发执行模型在引入 HITL 后需要调整——审批对话框不应并发弹出（用户无法同时审批多个）。建议改为**先串行审批所有需要确认的工具，批准后并发执行**：

```
executeTools()
  ├─ 分类：需审批 vs 直接允许
  ├─ 串行审批（显示对话框，等待用户逐一决定）
  └─ 并发执行（所有已批准的工具并发运行）
```

### 4.8 高危命令识别策略

参考 HermesAgent 的 `_DESTRUCTIVE_PATTERNS` 和 OpenHarness 的 `denied_commands`，建议内置分级危险命令识别：

```go
// internal/permission/dangerous.go

// Level1Patterns 高危，总是需要审批（无论 Permission Mode）
var Level1Patterns = []*regexp.Regexp{
    regexp.MustCompile(`rm\s+-[rRf]*\s+/`),    // rm -rf /
    regexp.MustCompile(`:\(\)\{.*\}.*;:`),        // fork bomb
    regexp.MustCompile(`curl[^|]+\|\s*[bs]h`),  // curl pipe to shell
    regexp.MustCompile(`>\s*/etc/`),              // 覆盖系统文件
    regexp.MustCompile(`chmod[^+]*777`),          // 危险权限
}

// Level2Patterns 中危，Default 模式需要审批，AutoEdit 模式自动批准
var Level2Patterns = []*regexp.Regexp{
    regexp.MustCompile(`rm\s+-[rRf]`),           // 递归删除
    regexp.MustCompile(`git\s+push.*--force`),   // 强制推送
    regexp.MustCompile(`DROP\s+TABLE`),           // SQL 删表
}
```

---

## 五、总结与选型建议

| 维度 | 最佳实践来源 | harness9 应采纳的设计 |
|------|------------|----------------------|
| 权限模式分级 | Claude Agent SDK（6 种） | 简化为 3 种（Default/AutoEdit/YOLO），与 Plan Mode 正交 |
| Hook 决策系统 | Claude Agent SDK（allow/deny/ask/defer） | 支持 Allow/Deny/Ask 三种决策，Deny 附带 LLM 可读原因 |
| 人类审批挂起 | OpenCode（Deferred）/ Claude SDK（canUseTool） | channel 阻塞实现同进程挂起 |
| 参数修改能力 | Claude Agent SDK（updatedInput） | 审批时允许用户编辑工具参数 |
| 记住决定 | Claude Agent SDK + OpenCode（always） | session 级动态白名单 + 可持久化到配置文件 |
| 高危命令识别 | HermesAgent（多层正则）+ tirith（外部扫描） | 内置分级正则规则，bash 工具专属扫描 |
| 凭据保护 | OpenHarness（硬编码不可覆盖） | 硬编码保护 ~/.ssh、~/.aws 等，优先于任何权限配置 |
| LLM 反馈 | OpenCode（CorrectedError）/ Claude SDK（deny message） | 拒绝时携带 Feedback，注入为 ToolResult.Output 让 LLM 调整策略 |
| 审批 UX | Claude SDK（AskUserQuestion）+ OpenHarness（TUI 对话框） | 集成到 Bubbletea 事件系统，通过 tea.Msg 异步触发审批界面 |

**核心原则**：harness9 当前的 `ToolHook` + `HookRegistry` 洋葱模型已具备良好的扩展基础。Human-in-the-Loop 本质上是一个特殊的 `ToolHook`，在 `BeforeExecute` 中通过 channel 阻塞当前 goroutine，同时向 TUI 发送 `tea.Msg`（审批请求事件）触发对话框渲染。这与 harness9 现有的 Bubbletea 事件驱动架构完全吻合，无需引入新的并发模型。
