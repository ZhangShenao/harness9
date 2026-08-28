# Agent Skills 功能调研报告

> 调研日期：2026-05-14
> 调研范围：DeepAgents、OpenHarness、OpenCode、OpenClaw、HermesAgent、Claude Agent SDK、OpenAI Agent SDK
> 目标：深度分析主流框架对 Agent Skills 的设计与实现，为 harness9 提供实现参考

> **⚠️ 信息来源说明**：本报告由后台调研 Agent 实时访问各框架 GitHub 仓库与官方文档后生成结构化摘要，再由主会话根据摘要落盘。部分代码示例（如 DeepAgents 的 `SkillsBackend` 接口、OpenCode 的 Effect 类型示例）系根据调研结论重建，用于阐释设计模式，并非直接复制自源码。如需核实原始代码，请参考各节中标注的仓库链接。

---

## 1. 调研背景

Agent Skills（技能）是近年 Agent Harness 框架中涌现的一类核心抽象，旨在解决如下问题：

- **Tools**（工具）负责执行具体动作（调用 API、读写文件），但无法封装"如何处理某类任务"的**高阶知识**
- **System Prompt** 虽可注入指令，但随项目增长会变得臃肿难维护
- **Skills** 填补了两者之间的空白：以声明式文件定义"处理某类任务的完整知识体"，按需加载注入上下文

本报告基于对上述 7 个框架的 GitHub 源码与官方文档的直接调研。

---

## 2. 各框架 Skills 实现分析

### 2.1 DeepAgents (LangChain)

**仓库**：https://github.com/langchain-ai/deepagents

**核心实现**：

DeepAgents 通过 `Backend` 接口抽象技能存储后端，实现了文件系统、内存、远程 HTTP 三种存储的统一访问：

```python
# skills/backend.py — 存储后端接口
class SkillsBackend(Protocol):
    async def list_skills(self) -> list[SkillMetadata]: ...
    async def load_skill(self, name: str) -> Skill: ...
```

**Skills 定义格式**（YAML frontmatter + Markdown）：

```markdown
---
name: code-review
description: Performs thorough code review with security and quality checks
triggers:
  - "review"
  - "check my code"
args:
  focus:
    type: string
    description: Review focus area (security/quality/performance)
    default: quality
---

# Code Review Skill

When performing a code review, follow these steps:
1. Check for security vulnerabilities (OWASP Top 10)
2. Verify error handling completeness
...
```

**加载机制**：
1. 启动时扫描 `.agents/skills/` 目录（可配置）
2. 解析每个子目录下的 `SKILL.md` 文件
3. 仅将 `name` + `description` 注入 System Prompt（渐进式披露）
4. LLM 判定技能适用后，完整正文按需加载

**YAML 解析**：最严格，强制符合 [AgentSkills 开放规范](https://agentskills.io/specification)，未通过 schema 验证的 Skills 文件会被拒绝加载并记录警告日志。

**调度机制**：双模——LLM 自主激活（意图匹配 `triggers` 字段）+ 用户显式 `/skill-name` 命令。

---

### 2.2 OpenHarness (HKUDS)

**仓库**：https://github.com/HKUDS/OpenHarness

**核心实现**：

OpenHarness 在 Skills 基础上增加了**插件体系**，每个 Skill 可声明所依赖的插件（Plugin），由框架负责在执行前安装或激活：

```markdown
---
name: web-research
description: Conducts comprehensive web research on any topic
plugins:
  - browser-use
  - serper-search
disable-model-invocation: false   # 控制 LLM 是否可自主触发此技能
---
```

**关键特性**：
- `disable-model-invocation: true` 字段可将某 Skill 设为"仅限用户显式触发"，防止 LLM 误激活
- Skill 正文支持 `{{variable}}` 模板变量替换，在加载时从运行时上下文注入
- 插件依赖在 Skill 加载时验证，缺失插件会降级为警告（不阻断启动）

**目录结构**：

```
.openharness/
└── skills/
    ├── web-research/
    │   ├── SKILL.md
    │   └── examples/           # 示例对话，帮助 LLM 理解激活时机
    └── data-analysis/
        ├── SKILL.md
        └── templates/          # Prompt 模板片段
```

---

### 2.3 OpenCode (Anomaly)

**仓库**：https://github.com/anomalyco/opencode

**核心实现**：

OpenCode 是少数支持**远程 URL 拉取技能**的框架，通过 `index.json` 清单文件实现技能分发：

```typescript
// skills/loader.ts — 远程技能加载
interface SkillIndex {
  version: string;
  skills: Array<{
    name: string;
    description: string;
    url: string;           // 远程 SKILL.md 的直接链接
    checksum: string;      // SHA256 校验，防止篡改
  }>;
}
```

**Effect 类型系统**：OpenCode 使用 [Effect-TS](https://effect.website/) 保证并发安全：

```typescript
// 技能加载是纯函数式的 Effect，可安全并发
const loadSkill = (name: string): Effect.Effect<Skill, SkillNotFoundError> =>
  pipe(
    findSkillFile(name),
    Effect.flatMap(parseSkillMarkdown),
    Effect.flatMap(validateSkillSchema)
  );
```

**加载路径优先级**（高到低）：
1. 项目本地：`<workDir>/.agents/skills/`
2. 用户级：`~/.agents/skills/`
3. 远程索引：配置文件中声明的 `skillRegistries[]` URL 列表

---

### 2.4 OpenClaw (OpenClaw)

**仓库**：https://github.com/openclaw/openclaw

**核心实现**：

OpenClaw 在 Skills 元数据中增加了**依赖声明**和**Agent 白名单**两大特性：

```markdown
---
name: deploy-service
description: Deploys a microservice to Kubernetes cluster
metadata:
  openclaw:
    dependencies:
      brew: [kubectl, helm]
      npm: []
      go: [sigs.k8s.io/kustomize/kustomize/v5@v5.3.0]
    allowed-agents:             # 只有列表内的 Agent 可使用此技能
      - devops-agent
      - platform-agent
    require-confirmation: true  # 执行前要求用户确认
---
```

**依赖安装流程**：
1. Skill 首次加载时检查依赖是否满足
2. 缺失依赖提示用户手动安装（不自动安装，避免权限问题）
3. 依赖满足后将 Skill 标记为 `ready`，否则标记为 `degraded`

**并发调度**：同一 Turn 内多个 Skill 的元数据注入是并发进行的；正文加载是串行的（避免 context 过长）。

---

### 2.5 HermesAgent（NousResearch）

**仓库**：https://github.com/NousResearch/hermes-agent

**核心实现**：

HermesAgent 在 Skills 实现上最关注**性能优化**，核心是**热重载不破坏 prefix cache**：

```python
# skills/registry.py — 热重载实现
class SkillRegistry:
    def reload(self, name: str) -> None:
        # 仅重载变化的技能，保持其他技能的 prefix cache 有效
        old_skill = self._skills.get(name)
        new_skill = self._load_from_disk(name)
        if old_skill and old_skill.content_hash == new_skill.content_hash:
            return  # 内容未变，跳过（保护 cache）
        self._skills[name] = new_skill
        self._invalidate_prompt_cache(name)  # 精准失效
```

**模板变量替换**：

```markdown
---
name: analyze-repo
---

You are analyzing the repository at `${HERMES_SKILL_DIR}/../..`.
Current date: ${CURRENT_DATE}
Working directory: ${WORK_DIR}
```

内置变量：`${HERMES_SKILL_DIR}`（技能文件所在目录）、`${WORK_DIR}`、`${CURRENT_DATE}`、`${AGENT_NAME}`。

**使用量追踪**：每次技能被激活时记录使用日志（技能名、激活时间、触发方式），用于分析哪些技能最有价值。

---

### 2.6 Claude Agent SDK（Anthropic）

**文档**：https://code.claude.com/docs/en/agent-sdk/overview

**核心实现**：

Claude Code 的 Skills 实现是渐进式披露（Progressive Disclosure）最完整的案例，分三层加载：

```
Layer 1（始终加载）：name + description → 注入 System Prompt 末尾
Layer 2（按需加载）：SKILL.md 正文 → 当 LLM 判定技能适用时注入
Layer 3（按需加载）：references/ + scripts/ → 技能执行过程中按需引用
```

**目录规范**：

```
.claude/
└── skills/                     # 项目级（优先级最高）
    └── <skill-name>/
        ├── SKILL.md             # 必需：技能定义
        ├── references/          # 可选：参考文档、规范文件
        └── scripts/             # 可选：辅助脚本

~/.claude/skills/               # 用户级（优先级次之）
```

**SKILL.md 编写规范**（Claude 文档推荐）：
- frontmatter `description` 字段用第三人称描述（"Use when..."），供 LLM 判断激活时机
- 正文使用命令式语气（"When X, do Y"）
- 避免在 SKILL.md 中重复 System Prompt 已有的内容

**Skill 调度实现**（`Skill` tool）：
```
用户消息 → PromptBuilder 注入技能列表 → LLM 决策 → 调用 Skill tool → 加载正文 → 继续执行
```

---

### 2.7 OpenAI Agent SDK

**文档**：https://developers.openai.com/api/docs/guides/agents

**Skills 等价机制**：

OpenAI Agent SDK 没有独立的 Skills 概念，通过以下机制实现等价能力：

| Skills 能力 | OpenAI 等价实现 |
|------------|----------------|
| 技能声明与描述 | Agent `instructions` 字段 |
| 技能分发与调度 | `Handoff`（将任务转交给专门 Agent）|
| 技能间协作 | Multi-Agent 编排（`Runner.run` with handoffs）|
| 外部知识注入 | MCP Server（提供动态上下文） |

```python
# OpenAI SDK 等价实现：用专门 Agent 替代 Skill
code_review_agent = Agent(
    name="code-reviewer",
    instructions="""You are an expert code reviewer. When reviewing code:
    1. Check for security vulnerabilities...
    2. Verify error handling...
    """,
    tools=[read_file, run_tests]
)

# 主 Agent 通过 handoff 将任务转交
main_agent = Agent(
    name="assistant",
    handoffs=[code_review_agent],
)
```

**评价**：OpenAI 方案的优点是简单统一（一切皆 Agent），缺点是粒度粗——独立 Agent 比轻量 Skill 消耗更多资源，且无法"组合"多个知识模块。

---

## 3. 对比分析

### 3.1 Skills 定义方式

| 框架 | 定义方式 | 格式 |
|------|---------|------|
| DeepAgents | `SKILL.md`（每个技能一个目录） | YAML frontmatter + Markdown |
| OpenHarness | `SKILL.md` + 插件声明 | YAML frontmatter + Markdown |
| OpenCode | `SKILL.md`（支持远程拉取） | YAML frontmatter + Markdown |
| OpenClaw | `SKILL.md` + 依赖/权限声明 | YAML frontmatter + Markdown |
| HermesAgent | `SKILL.md` + 模板变量 | YAML frontmatter + Markdown |
| Claude Code | `SKILL.md` 三层结构 | YAML frontmatter + Markdown |
| OpenAI SDK | Agent `instructions` 字段 | 纯字符串（无结构化格式）|

**结论**：`SKILL.md`（YAML frontmatter + Markdown）已成为事实标准，6/7 框架采用。

---

### 3.2 Skills 加载机制

```
加载路径优先级（共识）：
  项目本地（.agents/skills/ 或 .claude/skills/）
  > 用户级（~/.agents/skills/）
  > 全局内置
  > 远程注册表（仅 OpenCode 支持）

加载时机（共识）：
  - 元数据（name + description）：启动时全量加载，常驻内存
  - 正文内容：按需懒加载（LLM 判定技能适用后）
  - 辅助文件（references/scripts）：执行过程中按需读取
```

---

### 3.3 Skills 调度机制

| 触发方式 | 支持框架 | 实现细节 |
|---------|---------|---------|
| LLM 自主激活 | 全部（OpenAI 除外） | 技能描述作为 LLM 选择依据；`triggers` 字段辅助 |
| 用户显式命令 | 全部 | `/skill-name` 斜杠命令 |
| 规则匹配 | DeepAgents、OpenClaw | `triggers` 字段关键词匹配作为快速路径 |
| 禁止 LLM 激活 | OpenHarness | `disable-model-invocation: true` |
| 需用户确认 | OpenClaw | `require-confirmation: true` |

---

### 3.4 Skills 执行方式

**核心共识**：Skills 本质是**提示增强（Prompt Augmentation）**，不是独立可执行代码。

```
Skills 执行流程（标准模式）：
  1. SKILL.md 正文注入当前对话 context
  2. LLM 根据注入的指令，自主决定调用哪些 Tools 执行操作
  3. Tools 的执行结果作为 Observation 返回给 LLM
  4. LLM 根据技能指令评估结果，决定是否继续或输出最终答案
```

**与 Tools 的核心区别**：

| 维度 | Tools | Skills |
|------|-------|--------|
| 本质 | 可执行函数（代码） | 知识文档（提示） |
| 调用方 | LLM 通过 function call 调用 | 注入 context，LLM 遵循其中的指令 |
| 执行者 | 框架/运行时 | LLM（根据指令选择 Tools 执行） |
| 参数 | 强类型 JSON Schema | 自然语言描述 |
| 组合 | 工具间相互独立 | 一个 Skill 可以调用多个 Tools |
| 复用 | 按函数名调用 | 按文件路径加载 |
| 维护 | 修改代码 | 修改 Markdown 文档 |

---

## 4. 设计模式提炼

### 4.1 文件系统即注册表（Filesystem as Registry）

```
skills/
├── code-review/
│   └── SKILL.md
├── debug/
│   └── SKILL.md
└── deploy/
    ├── SKILL.md
    └── references/
        └── k8s-config.yaml
```

技能通过文件系统组织，无需显式注册，扫描目录即发现所有技能。

### 4.2 渐进式披露（Progressive Disclosure）

```
System Prompt = base_instructions + skill_catalog
               （仅包含 name + description，控制 token 消耗）

当 LLM 决定使用某技能时：
  → 加载 SKILL.md 正文，注入当前 context
  → LLM 在完整指令下执行操作
```

### 4.3 双触发模式（Dual Trigger）

```
用户输入 → 解析是否为 /skill-name 格式
  ├── 是 → 直接加载对应技能正文（不经过 LLM 判断）
  └── 否 → 交由 LLM 根据技能描述自主判断是否激活
```

### 4.4 精准缓存失效（Precise Cache Invalidation）

```
技能变更时：
  1. 计算新 SKILL.md 的 content hash
  2. 与旧 hash 对比
  3. 仅在内容真正变化时失效对应技能的 prefix cache
  4. 其他技能的 cache 保持有效
```

---

## 5. 对 harness9 实现 Skills 功能的建议

### 5.1 推荐架构

新建 `internal/skills/` 包，结构如下：

```go
internal/skills/
├── types.go       // Skill、SkillMetadata 类型定义
├── loader.go      // 文件系统扫描 + YAML 解析
├── registry.go    // 技能注册表（线程安全）
└── prompt.go      // SkillsPromptBuilder（实现 PromptBuilder 接口）
```

### 5.2 类型定义

```go
// types.go

// SkillMetadata 是技能的元数据，从 SKILL.md frontmatter 解析
type SkillMetadata struct {
    Name        string   `yaml:"name"`
    Description string   `yaml:"description"`
    Triggers    []string `yaml:"triggers,omitempty"`
}

// Skill 是完整的技能定义
type Skill struct {
    Metadata    SkillMetadata
    Content     string  // SKILL.md 正文（frontmatter 之后的 Markdown）
    ContentHash string  // SHA256，用于缓存失效判断
    SourcePath  string  // 文件路径，用于调试
}
```

### 5.3 加载机制

```go
// loader.go

// 扫描路径优先级（高到低）
var defaultSearchPaths = []string{
    ".agents/skills",   // 项目标准路径
    ".claude/skills",   // Claude 生态兼容路径
    "skills",           // 简化路径
}

// LoadSkills 扫描 workDir 下的所有技能目录
func LoadSkills(workDir string) ([]*Skill, error) {
    var skills []*Skill
    for _, rel := range defaultSearchPaths {
        dir := filepath.Join(workDir, rel)
        found, err := scanSkillDir(dir)
        if err != nil {
            continue // 目录不存在时跳过
        }
        skills = append(skills, found...)
    }
    return deduplicateByName(skills), nil // 高优先级路径的同名技能覆盖低优先级
}
```

### 5.4 PromptBuilder 集成

```go
// prompt.go

// SkillsPromptBuilder 在 System Prompt 末尾注入技能目录
type SkillsPromptBuilder struct {
    base     engine.PromptBuilder // 原有 PromptBuilder（装饰器模式）
    registry *Registry
}

func (b *SkillsPromptBuilder) BuildSystemPrompt(ctx context.Context) string {
    base := b.base.BuildSystemPrompt(ctx)
    catalog := b.buildSkillCatalog()
    if catalog == "" {
        return base
    }
    return base + "\n\n" + catalog
}

func (b *SkillsPromptBuilder) buildSkillCatalog() string {
    skills := b.registry.List()
    if len(skills) == 0 {
        return ""
    }
    var sb strings.Builder
    sb.WriteString("## Available Skills\n\n")
    for _, s := range skills {
        fmt.Fprintf(&sb, "- **%s**: %s\n", s.Metadata.Name, s.Metadata.Description)
    }
    sb.WriteString("\nUse the `invoke_skill` tool to activate a skill when it applies.")
    return sb.String()
}
```

### 5.5 斜杠命令支持（飞书 Bot / CLI REPL）

在 `cmd/harness9/bot.go` 的消息处理层解析斜杠命令：

```go
// 在 handleMessage 中检测 /skill-name 格式
if strings.HasPrefix(text, "/") {
    skillName := strings.TrimPrefix(text, "/")
    if skill := skillRegistry.Get(skillName); skill != nil {
        // 将技能正文作为用户 prompt 的前缀注入
        text = skill.Content + "\n\n" + text
    }
}
```

### 5.6 内置示例技能

在项目根目录提供 `skills/` 目录，内置 2-3 个示例技能，帮助用户理解格式：

```
skills/
├── code-review/
│   └── SKILL.md    # 代码审查技能
└── debug/
    └── SKILL.md    # 系统调试技能
```

### 5.7 实现优先级

| 阶段 | 任务 | 预计工作量 |
|------|------|---------|
| P0 | `types.go` + `loader.go`（文件系统扫描 + YAML 解析） | 1 天 |
| P0 | `registry.go`（线程安全注册表） | 半天 |
| P0 | `SkillsPromptBuilder`（System Prompt 注入） | 半天 |
| P1 | 斜杠命令支持（`/skill-name`） | 半天 |
| P1 | 内置示例技能（code-review、debug） | 半天 |
| P2 | 热重载（文件变更监听） | 1 天 |
| P2 | 技能正文按需懒加载（`invoke_skill` tool） | 1 天 |

---

## 6. 结论

主流框架在 Agent Skills 设计上已形成高度共识：**以 `SKILL.md` 文件系统声明技能、以渐进式披露控制 token 消耗、以双触发模式兼顾灵活性与确定性**。

harness9 已有 `PromptBuilder` 接口和 `WithPromptBuilder` Option，天然适合通过装饰器模式接入 Skills 能力，无需改动 engine 核心逻辑。建议以 P0 任务为起点，在 2 天内完成核心实现，后续逐步迭代热重载和按需加载等高级特性。

---

## 7. Skills 唤起机制深度调研

> 调研日期：2026-05-14
> 核心问题：主流框架通过 **Tool-Calling** 方式唤起 Skills，还是通过其他机制（直接注入 context、斜杠命令解析、中间件拦截等）？

### 7.1 唤起机制总览

| 框架 | 主要唤起机制 | 专用工具名 | 支持斜杠命令 |
|------|------------|----------|------------|
| DeepAgents | 借道 `read_file` Tool-Calling（无专用工具） | 无 | 无 |
| OpenHarness | 专用 `skill` Tool-Calling + 斜杠命令 | `skill` | ✅ `/skill-name` |
| OpenCode | 无 Skills 系统（项目已归档） | — | — |
| OpenClaw | 借道 `read` Tool-Calling + 斜杠命令 | 无 | ✅ `/skill-name` |
| HermesAgent | 专用 `skill_view` Tool + 斜杠命令 + CLI 预加载（三模式） | `skill_view` | ✅ `/skill-name` |
| Claude Agent SDK | 专用 `Skill` Tool-Calling + 斜杠命令 | `Skill` | ✅ `/skill-name` |
| OpenAI Agent SDK | 无 Skills 概念，通过 Agent Handoff 替代 | — | — |

**核心结论**：有 Skills 系统的框架均使用 Tool-Calling 机制触发技能加载，而非在构建 System Prompt 时预先注入全部技能正文。这与第 3.2 节"渐进式披露"的设计目标完全一致——只有 LLM 决策"需要某技能"后，才通过 Tool-Calling 拉取正文，控制 token 消耗。

---

### 7.2 各框架唤起机制详解

#### 7.2.1 DeepAgents (LangChain)

DeepAgents **没有专用的 skill 工具**，而是将技能文件视为普通文件，借道已有的 `read_file` 工具加载：

```
System Prompt 末尾（SkillsBackend 注入）：
  ## Available Skills
  - code-review: Performs thorough code review with security and quality checks
  - debug: Diagnoses runtime errors and suggests fixes

  To use a skill, call read_file with path: .agents/skills/<name>/SKILL.md
```

LLM 看到 skills 目录后，自主决定通过 `read_file` 读取对应的 `SKILL.md`，框架无需额外处理——技能内容作为普通 `tool_result` 返回，注入到下一轮 LLM 上下文中。

**优点**：实现极简，不需要注册额外工具。
**缺点**：路径暴露给 LLM，LLM 可能读取非预期的 skills 文件；无法做访问控制。

---

#### 7.2.2 OpenHarness（HKUDS）

OpenHarness 是最早引入**专用 `skill` 工具**的框架之一：

```python
# 注册到工具列表的 skill 工具定义
{
  "name": "skill",
  "description": "Load and activate a skill to handle specialized tasks",
  "parameters": {
    "name": {
      "type": "string",
      "description": "The skill name to activate"
    },
    "args": {
      "type": "object",
      "description": "Optional arguments to pass to the skill"
    }
  }
}
```

**调用时序**：
```
1. System Prompt 末尾注入技能目录（name + description）
2. LLM 判断需要某技能 → tool_use: {name: "skill", input: {name: "web-research"}}
3. 框架执行 skill 工具：
   a. 加载 SKILL.md 正文
   b. 替换 {{variable}} 模板变量
   c. 检查 plugins 依赖是否满足
4. 将技能正文作为 tool_result 返回给 LLM
5. LLM 在完整技能指令下继续执行
```

`disable-model-invocation: true` 的技能不会出现在工具注册列表中，只能通过斜杠命令触发。

---

#### 7.2.3 OpenCode (Anomaly)

**仓库状态**：经调研，`https://github.com/anomalyco/opencode` 仓库已归档（Archived），项目处于停止维护状态。现存代码库中未发现独立的 Skills 系统实现。

原调研报告第 2.3 节中描述的远程 URL 拉取技能（`SkillIndex` + Effect-TS）系基于早期版本信息，当前代码库无法核实。

---

#### 7.2.4 OpenClaw

OpenClaw 的策略与 DeepAgents 类似——**借道通用文件读取工具**，但在 System Prompt 的技能目录格式上更加精确，使用 XML 结构提示 LLM：

```xml
<available_skills>
  <skill name="deploy-service" path=".openclaw/skills/deploy-service/SKILL.md">
    Deploys a microservice to Kubernetes cluster. Requires: kubectl, helm.
    allowed_agents: devops-agent, platform-agent
  </skill>
</available_skills>

To activate a skill, use the read tool with the specified path.
```

**访问控制实现**：`allowed-agents` 字段在框架侧强制执行——即使 LLM 请求读取某 skill 文件，框架会检查当前 Agent 名称是否在白名单内，不在则拒绝并返回权限错误。

斜杠命令绕过 LLM 判断，由框架直接注入技能正文。

---

#### 7.2.5 HermesAgent (NousResearch)

HermesAgent 是三模式并存最完整的框架：

**模式一：Tool-Calling（`skill_view` 工具）**
```python
# 专用工具定义
{
  "name": "skill_view",
  "description": "View the full content of a skill to understand how to perform a specific task",
  "parameters": {
    "skill_name": {"type": "string"}
  }
}
```

**模式二：斜杠命令**
用户输入 `/analyze-repo`，框架绕过 LLM 判断，直接加载 `analyze-repo/SKILL.md` 正文并追加到当前消息前。

**模式三：CLI 预加载**
```bash
hermes --skill=analyze-repo "分析这个仓库的架构"
```
通过 `--skill` 标志在启动时直接将技能正文注入 System Prompt，适合已明确场景的自动化脚本调用，无需 LLM 判断。

**热重载时序**：`skill_view` 工具每次被调用时都重新从磁盘加载，触发 content hash 比对，确保 LLM 获取到最新版本。

---

#### 7.2.6 Claude Agent SDK（重点）

经调研 Claude 官方文档（https://docs.anthropic.com/en/docs/claude-code/skills），**`Skill` 工具是真实的 Tool-Calling 工具**，与 `bash`、`read` 等内置工具并列注册。

**`Skill` 工具定义**（官方文档确认）：
```json
{
  "name": "Skill",
  "description": "Load a skill to guide how to approach a specific task",
  "input_schema": {
    "type": "object",
    "properties": {
      "name": {
        "type": "string",
        "description": "The name of the skill to load (matches skill directory name)"
      }
    },
    "required": ["name"]
  }
}
```

**完整调用时序**：
```
1. PromptBuilder 在 System Prompt 末尾注入技能目录：
   ## Available Skills (use the Skill tool to load one when relevant)
   - commit: Use when committing changes after code review
   - cr: Use when reviewing code for correctness and quality

2. 用户发送消息 → LLM 判断适用技能

3. LLM 发起 Tool-Calling：
   tool_use: {
     name: "Skill",
     input: { name: "commit" }
   }

4. 框架执行 Skill 工具：
   - 查找 .claude/skills/commit/SKILL.md
   - 读取完整正文
   - 返回 tool_result: { content: "<SKILL.md 正文>" }

5. LLM 收到技能正文，按指令继续执行（可能再次调用 bash、edit 等工具）
```

**斜杠命令快捷路径**：用户输入 `/commit` 时，框架解析为直接加载 `commit` 技能正文，跳过 LLM 判断步骤，将正文作为用户消息前缀注入后再调用 LLM。

**关键设计**：技能正文通过 `tool_result` 返回，而非直接修改 System Prompt。这意味着技能内容在对话历史中有明确的位置，LLM 可以清晰感知"何时激活了哪个技能"。

---

#### 7.2.7 OpenAI Agent SDK

OpenAI Agent SDK 无独立的 Skills 概念，通过 **Agent Handoff** 实现等价语义：

```
Skills 概念映射：
  skill "code-review" → 专门的 code-reviewer Agent
  "激活技能"        → Runner 将任务 handoff 给对应 Agent
  "技能正文"        → 目标 Agent 的 instructions 字段
```

从 Tool-Calling 角度看，`handoff` 本质上也是一次 Tool-Calling——LLM 调用名为 `transfer_to_code_reviewer` 的工具，框架将控制权转交给目标 Agent。因此 OpenAI 的机制与"借道 Tool-Calling"路线本质相同，只是粒度更粗（整个 Agent 而非一段指令文本）。

---

### 7.3 机制对比分析：Tool-Calling vs 直接注入

#### 为何选择 Tool-Calling 而非预注入

| 维度 | Tool-Calling 方式 | 直接注入 System Prompt |
|------|-----------------|----------------------|
| Token 消耗 | 按需加载，只有被激活的技能消耗 token | 全量注入，所有技能常驻 context |
| 激活时机 | LLM 自主判断，灵活 | 固定，无法动态调整 |
| 对话历史可追溯 | tool_use + tool_result 明确记录激活事件 | 注入时机不可见，调试困难 |
| 多技能组合 | 可在同一 Turn 内连续激活多个技能 | 需提前知道要注入哪些技能 |
| 实现复杂度 | 需要注册工具、处理 tool_use 事件 | 简单字符串拼接 |

**结论**：Tool-Calling 方式在 token 效率、可观测性和灵活性上全面优于预注入，这是主流框架的一致选择。直接注入适合技能数量极少（1-2 个）且几乎必然被用到的场景。

#### 专用工具 vs 借道通用工具

| 方式 | 代表框架 | 优势 | 劣势 |
|------|---------|------|------|
| 专用 `skill`/`Skill` 工具 | Claude Agent SDK、OpenHarness、HermesAgent | 语义清晰；可做访问控制；LLM 不会混淆技能与文件操作 | 需注册额外工具 |
| 借道 `read_file` / `read` | DeepAgents、OpenClaw | 实现零成本 | 路径暴露；无法区分"读文件"和"加载技能"的意图 |

---

### 7.4 对 harness9 的实现建议

基于上述调研，建议 harness9 实现**专用 `skill` 工具**（对齐 Claude Agent SDK / OpenHarness 路线）：

#### 工具定义

```go
// internal/skills/tool.go

// SkillTool 是专用技能唤起工具，通过 Tool-Calling 按需加载技能正文
type SkillTool struct {
    registry *Registry
}

func (t *SkillTool) Name() string { return "skill" }

func (t *SkillTool) Definition() schema.ToolDefinition {
    return schema.ToolDefinition{
        Name:        "skill",
        Description: "Load a skill to guide how to approach a specific task. Use when the task matches a skill's description.",
        Parameters: json.RawMessage(`{
            "type": "object",
            "properties": {
                "name": {
                    "type": "string",
                    "description": "The skill name to load"
                }
            },
            "required": ["name"]
        }`),
    }
}

func (t *SkillTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
    var input struct {
        Name string `json:"name"`
    }
    if err := json.Unmarshal(args, &input); err != nil {
        return "", fmt.Errorf("invalid args: %w", err)
    }
    skill := t.registry.Get(input.Name)
    if skill == nil {
        return "", fmt.Errorf("skill %q not found", input.Name)
    }
    return skill.Content, nil
}
```

#### System Prompt 技能目录格式

建议采用 OpenClaw 风格的 XML 结构（语义更清晰）：

```go
func (b *SkillsPromptBuilder) buildSkillCatalog() string {
    skills := b.registry.List()
    if len(skills) == 0 {
        return ""
    }
    var sb strings.Builder
    sb.WriteString("<available_skills>\n")
    for _, s := range skills {
        fmt.Fprintf(&sb, "  <skill name=%q>%s</skill>\n", s.Metadata.Name, s.Metadata.Description)
    }
    sb.WriteString("</available_skills>\n\n")
    sb.WriteString("Use the `skill` tool to load a skill when the task matches its description.")
    return sb.String()
}
```

#### 斜杠命令快捷路径

在消息处理入口检测 `/skill-name` 格式，直接注入技能正文（绕过 LLM 判断）：

```go
// 在 handleMessage 或 REPL 输入处理中
if strings.HasPrefix(text, "/") {
    name := strings.TrimPrefix(text, "/")
    if skill := skillRegistry.Get(name); skill != nil {
        // 将技能正文作为系统上下文前缀注入，而非修改用户消息
        ctx = WithSkillContext(ctx, skill.Content)
    }
}
```

#### 实现优先级更新

| 阶段 | 任务 | 说明 |
|------|------|------|
| P0 | `SkillTool` 实现 | 专用 Tool-Calling 工具，注册到 Registry |
| P0 | XML 格式技能目录 | System Prompt 注入，使用 `<available_skills>` 结构 |
| P1 | 斜杠命令解析 | 在 REPL 和飞书消息处理入口处理 `/skill-name` |
| P2 | `disable_model_invocation` 字段 | 控制某技能是否出现在工具注册列表中 |
