# Claude Code 到 Codex 项目配置迁移设计

## 背景

harness9 当前同时存在项目级 Claude Code 配置与用户级自维护配置。目标是在不修改原 Claude 配置的前提下，为 Codex 建立一套独立、原生、可验证的项目级配置，使两套 Harness 配置可以并行维护。

本设计以当前扫描结果和 Codex 官方项目配置机制为准：

- `AGENTS.md` 承载仓库级长期指令。
- `.agents/skills/` 承载仓库级 Skills。
- `.codex/agents/` 承载自定义 Sub-Agent。
- `.codex/config.toml` 承载项目级 MCP、权限和 Agent 注册。
- `.codex/hooks.json` 与 `.codex/hooks/` 承载生命周期 Hook。
- `.codex/rules/` 承载项目命令策略。

## 目标

1. 迁移项目级 4 个 Sub-Agent：
   - `harness-blog-writer`
   - `harness-enhancer`
   - `harness-researcher`
   - `test-runner`
2. 迁移用户级 3 个 Sub-Agent：
   - `collector`
   - `analyzer`
   - `organizer`
3. 迁移 4 个 Skills：
   - `release-cli`
   - `commit`
   - `cr`
   - `pr`
4. 迁移 Obsidian 同步 Hook，并适配 Codex 的 `PostToolUse` 输入。
5. 将项目 Context7 MCP 转换为 Codex 项目配置。
6. 为外部知识库目录配置最小必要权限。
7. 让 `harness-blog-writer` 直接生成正文图片和封面图，而不再只输出图片 Prompt。
8. 保留现有 Claude 配置，支持 Claude Code 与 Codex 双栈并行维护。

## 非目标

- 不修改或删除 `.claude/`。
- 不修改或删除 `.mcp.json`。
- 不修改 `AGENTS.md` 内容。
- 不修改 `CLAUDE.md -> AGENTS.md` 符号链接。
- 不迁移 Claude 会话、任务、历史记录、文件历史、缓存或统计数据。
- 不迁移 Claude 累积的全局 permission allowlist。
- 不复制 Claude 插件缓存、市场目录或已安装插件副本。
- 不在仓库中保存 Token、密码、连接串或其他凭据。
- 不重新打包 Superpowers、Figma 或 Notion；继续使用当前 Codex 环境中已安装的原生插件。

## 已扫描资产

### 项目级

| 类型 | 数量 | 来源 |
|---|---:|---|
| Sub-Agent | 4 | `.claude/agents/*.md` |
| Skill | 1 | `.claude/skills/release-cli/skill.md` |
| Hook | 1 | `.claude/settings.json` |
| MCP | 1 | `.mcp.json` |
| 项目指令 | 1 | `AGENTS.md` |

### 用户级自维护资产

| 类型 | 数量 | 来源 |
|---|---:|---|
| Sub-Agent | 3 | `~/.claude/agents/*.md` |
| Skill | 3 | `~/.claude/skills/*/SKILL.md` |

### 复用的现有 Codex 能力

- Superpowers 插件
- Figma 插件
- Notion 插件
- 内置 `imagegen` Skill 和 `image_gen` 工具

## 方案选择

采用 Codex 原生分层迁移：

```text
harness9/
├── AGENTS.md
├── CLAUDE.md -> AGENTS.md
├── .claude/
├── .mcp.json
├── .agents/
│   └── skills/
│       ├── release-cli/
│       │   ├── SKILL.md
│       │   └── agents/openai.yaml
│       ├── commit/
│       │   ├── SKILL.md
│       │   └── agents/openai.yaml
│       ├── cr/
│       │   ├── SKILL.md
│       │   └── agents/openai.yaml
│       └── pr/
│           ├── SKILL.md
│           └── agents/openai.yaml
└── .codex/
    ├── config.toml
    ├── hooks.json
    ├── agents/
    │   ├── harness-blog-writer.toml
    │   ├── harness-enhancer.toml
    │   ├── harness-researcher.toml
    │   ├── test-runner.toml
    │   ├── collector.toml
    │   ├── analyzer.toml
    │   └── organizer.toml
    ├── hooks/
    │   └── sync-to-obsidian.sh
    ├── rules/
    │   └── harness9.rules
    ├── scripts/
    │   └── cleanup-knowledge-day.sh
    └── tests/
        ├── test-sync-to-obsidian.sh
        └── test-cleanup-knowledge-day.sh
```

原 Claude 文件与新增 Codex 文件没有符号链接、生成依赖或反向写入关系。

## Sub-Agent 迁移

每个 Codex Agent 使用独立 TOML 文件，必须包含：

- `name`
- `description`
- `developer_instructions`

Claude 的 `tools` 字段不机械复制。能力边界由 Codex sandbox、MCP 配置、外部 writable roots、项目 rules 和 `developer_instructions` 共同表达。

### 模型策略

- `test-runner` 固定使用 `gpt-5.6-luna`，保留原 Haiku Agent 的轻量、低成本定位。
- 其余 6 个 Agent 不写 `model`，继承主任务当前模型，避免项目锁死未来默认模型。

### Agent 职责

| Agent | Codex 行为 |
|---|---|
| `harness-blog-writer` | 读取项目文档和代码，写入 `website/zh/blog/<slug>/`，更新 VitePress 配置，生成并校验实际图片 |
| `harness-enhancer` | 对全仓执行 Review、修复、注释补充和文档同步 |
| `harness-researcher` | 使用 Web 与 Context7 调研指定框架，只写 `docs/技术调研/` |
| `test-runner` | 运行测试并输出结构化报告，不修改项目文件 |
| `collector` | 采集外部信息，只向知识库 `raw/` 写入 |
| `analyzer` | 读取 `raw/`，按需回源，只向 `analysis/` 写入 |
| `organizer` | 读取 `analysis/` 与历史文章，只向 `articles/` 写入；成功后调用受保护清理脚本 |

## Blog Writer 图片生成

`harness-blog-writer` 的 Codex 版本把图片生成纳入必需交付物：

1. 每篇文章至少生成 6 张正文技术图和 1 张封面图。
2. 每张不同图片使用一次内置 `image_gen` 调用，不用单次调用的变体数量替代不同资产。
3. 正文图使用现有的吉卜力简约技术图规格；封面使用 16:9 场景叙事式插画规格。
4. 生成结果先由 Codex 保存到默认图片目录，再复制到：
   - `website/zh/blog/<slug>/images/<filename>.png`
5. 每张图片落盘后必须检查：
   - 主题是否匹配
   - 构图和宽高方向是否正确
   - 技术节点是否遗漏
   - 是否出现不需要的文字、水印或明显伪影
6. 验证失败时只做一次针对性重试；仍失败则报告具体图片，不伪造成功。
7. Markdown 同时保留图片引用和最终 Prompt，支持复现与后续重生成。
8. 项目引用的图片不得只留在 Codex 默认生成目录。
9. 如果当前运行环境没有 `image_gen` 能力，Agent 必须明确报告阻塞，不自动降级为只生成 Prompt，也不自动切换到需要 API Key 的 CLI 路径。

## Skills 迁移

4 个 Skills 使用 `.agents/skills/<name>/SKILL.md`，并遵循以下规则：

- 文件名统一为大写 `SKILL.md`。
- frontmatter 只保留 Codex 所需字段。
- `name` 使用小写字母、数字和连字符。
- `description` 明确描述触发条件与边界。
- 每个 Skill 生成匹配的 `agents/openai.yaml`。
- Claude Slash Command 心智模型转换为 Codex Skill 调用：
  - `/release-cli` → `$release-cli`
  - `/commit` → `$commit`
  - `/cr` → `$cr`
  - `/pr` → `$pr`
- `commit`、`cr`、`pr` 维持 review → commit → draft PR 的门禁关系。
- `release-cli` 保留版本确认、分支检查、tag、push、Actions、Release Note 和失败恢复流程。
- Codex 副本可以修复源文件中明显的 Markdown 格式错误，但不修改 Claude 源文件。

每个 Skill 独立完成基线场景、格式验证和迁移后行为测试，再迁移下一个 Skill。

## MCP 配置

项目 Context7 配置迁移到 `.codex/config.toml`：

- 传输方式：stdio
- 命令：`npx`
- 参数：`-y @upstash/context7-mcp`
- 不写入凭据
- 无 Context7 依赖的 Agent 不应主动使用该 MCP
- `harness-researcher` 在 Context7 不可用时报告依赖失败，不使用虚构结果补位

## 外部知识库权限

保留现有外部目录：

`/Users/zsa/Desktop/workspace/harness9/知识库日报/`

项目配置为知识库 Agent 增加该目录的访问能力。各 Agent 的写入边界仍在 `developer_instructions` 中明确：

- `collector`：`raw/`
- `analyzer`：`analysis/`
- `organizer`：`articles/`

不把外部路径开放为主 Agent 的无条件全局写入默认值。

## 知识库清理

`organizer` 不直接执行开放式 `rm -rf`。它调用 `.codex/scripts/cleanup-knowledge-day.sh`，脚本必须：

1. 只接受 `YYYYMMDD` 日期。
2. 拒绝空值、斜杠、点路径、路径穿越和非日期输入。
3. 将目标限制在知识库根目录下的：
   - `raw/<YYYYMMDD>`
   - `analysis/<YYYYMMDD>`
4. 清理前确认 `articles/<YYYYMMDD>-daily*.md` 至少存在一份。
5. 明确解析并比对规范化路径。
6. 不接受任意目标目录参数。
7. 删除完成后报告实际删除路径。

## Hook 迁移

Codex Hook 使用 `.codex/hooks.json` 注册 `PostToolUse`：

- matcher 覆盖 `apply_patch`，并兼容 `Edit`、`Write` 别名。
- Hook 从 stdin 读取 Codex JSON。
- 对 `apply_patch` 从 `tool_input.command` 解析所有受影响文件。
- 只同步 Markdown 文件。
- 同步范围：
  - `website/zh/blog/<slug>/index.md` → `技术博客/<slug>.md`
  - `docs/**` → 对应知识库目录
  - 项目内知识文章目录 → `知识库日报/`
- 不相关工具、不相关路径、缺失文件和无法识别的输入安静跳过。
- 同步失败输出诊断，但不阻断已经完成的编辑。
- 使用 git root 解析脚本位置，不依赖 Codex 从仓库根目录启动。

Claude 原 Hook 与 `scripts/sync-to-obsidian.sh` 保持不变。

## Rules

`.codex/rules/harness9.rules` 只维护少量项目相关规则：

- 对 Go 构建、测试、检查、格式化和只读 Git 查询提供明确规则。
- 对 push、tag、Release 修改和其他外部写操作保留审查。
- 对开放式危险删除保持提示或禁止。
- 为规则提供 `match` 与 `not_match` 示例。
- 不复制 Claude 历史 permission allowlist。
- 不为其他项目的命令或绝对路径建立规则。

## 错误处理

- Hook 对无关输入 fail-open；同步错误可见但不阻断编辑。
- 图片生成失败时报告具体资产；不创建空图片或假成功引用。
- Context7 不可用时，依赖它的 Agent 报告阻塞。
- 清理脚本任一前置条件失败时不删除任何内容。
- 发布 Skill 在分支、工作区、tag 或 GitHub Release 状态不满足条件时停止。
- 配置解析失败视为迁移失败，不能以“文件已生成”作为完成标准。

## 验证策略

### 源配置不变性

迁移前后对以下内容计算并比较校验和：

- `.claude/`
- `.mcp.json`
- `AGENTS.md`
- `CLAUDE.md` 符号链接目标

### 格式验证

- 使用 Python `tomllib` 解析 `.codex/config.toml` 和全部 Agent TOML。
- 使用 JSON 解析器验证 `.codex/hooks.json`。
- 使用 Codex Skill validator 验证每个 Skill。
- 使用 `bash -n` 验证 Hook 与清理脚本。
- 验证每个 Agent 的必需字段。
- 验证只有 `test-runner` 固定模型。

### 行为验证

- 用模拟 `PostToolUse` JSON 验证：
  - 单文件 patch
  - 多文件 patch
  - 相对路径和绝对路径
  - 不相关文件
  - 非法 JSON
- 在临时目录测试清理脚本：
  - 合法日期
  - 非法日期
  - 路径穿越
  - 文章不存在
  - 只删除目标日期
- 使用 `codex execpolicy check` 验证 rules。
- 对 4 个 Skills 逐个进行迁移前基线和迁移后行为测试。
- 对 `harness-blog-writer` 做能力发现测试，确认子代理能看到并调用 `imagegen`；测试资产使用临时或专用验证目录，不污染正式博客。

### 回归与安全

- 运行 `go test ./...`。
- 扫描新增文件中的常见凭据模式。
- 检查 `git diff`，确认 Claude 配置无变化。
- 检查所有新路径均在设计范围内。

## 验收标准

迁移完成必须同时满足：

1. 7 个 Codex Sub-Agent 可被发现，职责和权限边界完整。
2. 4 个 Codex Skills 可被发现、显式调用并通过验证。
3. `test-runner` 使用 `gpt-5.6-luna`，其他 Agent 继承父模型。
4. Context7 以项目级 stdio MCP 方式配置。
5. 知识库 Agent 可以访问既有外部目录。
6. `organizer` 只能通过受保护脚本清理目标日期中间数据。
7. Obsidian Hook 能处理 Codex `apply_patch` 输入。
8. Blog Writer 能生成、校验并落盘至少 7 张实际图片，同时保留 Prompt。
9. 新增文件不包含凭据。
10. Claude 配置、MCP 和项目指令的校验和保持不变。
11. 所有配置测试、脚本测试、Skill 测试和 Go 回归测试通过。

## 官方依据

- [Codex 自定义 Sub-Agent](https://learn.chatgpt.com/docs/agent-configuration/subagents)
- [Codex Skills](https://learn.chatgpt.com/docs/build-skills)
- [Codex Hooks](https://learn.chatgpt.com/docs/hooks)
- [Codex 项目配置](https://learn.chatgpt.com/docs/config-file/config-basic)
- [Codex Rules](https://learn.chatgpt.com/docs/agent-configuration/rules)
