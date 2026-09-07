# .zcode/ — ZCode 工作区配置

本目录是 harness9 项目面向 **ZCode** 的统一配置（workspace 级，随仓库版本化）。它由原先分散在 `.claude/`、`.codex/`、`.opencode/`、`.harness9/` 四套 Harness 配置整理合并而来；四套旧配置仍保留供各自客户端使用，**ZCode 只读本目录**。

## 目录结构

```
.zcode/
├── config.json        # MCP 服务器（context7）+ Hooks（Obsidian 同步）
├── README.md          # 本文件：来源映射与维护约定
├── agents/            # 10 个子代理（ZCode 自动加载，@名字 或 task 委派调用）
├── commands/          # 5 个斜杠命令（/commit /cr /pr /release-cli /sync-docs）
└── skills/            # 4 个技能（按 description 自动触发，或 /名字 调用）
```

## 统一来源映射

### Agents（`.zcode/agents/`）

选取各系统中**内容最新**的版本作为母本，frontmatter 统一为 ZCode 格式（`name` + `description` + `model: inherit` + `tools` 列表），正文保持原样：

| ZCode 文件 | 母本来源 | 说明 |
|---|---|---|
| `harness-blog-writer.md` | `.claude/agents/`（2026-09-01 最新版） | 公众号读者画像版 |
| `harness-enhancer.md` | `.claude/agents/` | 全仓库质量提升 |
| `harness-researcher.md` | `.claude/agents/` | Context7 工具名已修正为 `get-library-docs` |
| `test-runner.md` | `.claude/agents/` | `model: claude-haiku-*` 改为 `inherit` |
| `analyzer.md` / `collector.md` / `organizer.md` | `.opencode/agents/`（仅此系统有） | 知识库日报流水线；`mode`/`tools` 布尔表转 ZCode 工具列表 |
| `dev.md` / `doc-writer.md` / `explorer.md` | `.harness9/agents/`（**未被 git 跟踪**，本次迁移顺便纳入版本化） | harness9 工具名（`read_file` 等）已转换为 ZCode 内置工具名 |

统一约定：所有子代理 `model: inherit`（跟随主 Agent 当前模型）；dev 的 `skills: go-coding-standards` 字段 ZCode 不支持，规范来源改为 AGENTS.md 第 3 节（子代理默认注入 AGENTS.md）。

### Commands（`.zcode/commands/`）

直接复用 `.opencode/commands/` 的最新加固版提示词（英文安全加固版比 `skills/` 下的中文初版更新、更严格）：

| 命令 | 来源 |
|---|---|
| `/commit` `/cr` `/pr` `/release-cli` `/sync-docs` | `.opencode/commands/` 复制并去平台化（Codex→ZCode 措辞、`apply_patch`→通用说法），给 `/release-cli`、`/sync-docs` 追加 `argument-hint` 与 `$ARGUMENTS` 占位 |

### Skills（`.zcode/skills/`）

| 技能 | 来源 | 说明 |
|---|---|---|
| `architecture-overview` `debugging-guide` `go-coding-standards` | `skills/`（项目级，原样复制） | 知识型技能，无工具依赖 |
| `autodev` | `skills/autodev/` | 正文 `read_file`→`Read`、task 委派措辞已适配 ZCode |

`skills/`（仓库根目录）仍是 **harness9 二进制自身技能系统**的唯一信息源，两者内容可能有平台化差异，属预期行为。`commit`/`cr`/`pr`/`release-cli` 四个工作流在 ZCode 侧由 `commands/` 承担，不再复制为技能，避免双份漂移。

### MCP + Hooks（`config.json`）

| 配置 | 来源 | 说明 |
|---|---|---|
| `mcp.servers.context7` | `.mcp.json`（npx stdio 版） | AGENTS.md §6.7 要求第三方 API/SDK 文档优先走 context7 |
| `hooks.events.PostToolUse`（matcher `Write|Edit`） | `.claude/settings.json` + `.codex/hooks.json` | 调用仓库根 `scripts/sync-to-obsidian.sh`（stdin 协议与 ZCode 兼容） |

**Hooks 注意事项**：部分 ZCode 版本对工作区级 hooks 需要信任确认或暂不执行；若发现 Obsidian 同步未生效，把 `config.json` 中的 `hooks` 段复制到用户级 `~/.zcode/cli/config.json`（同样需要 `hooks.enabled: true`）。

## 未迁移项（有意跳过）

| 旧配置 | 未迁移原因 |
|---|---|
| `.claude/settings.local.json`（权限白名单）、`.harness9/settings.json`（permissions） | Claude Code / harness9 专有权限模型；ZCode 用交互式审批（"总是允许"）+ `PermissionRequest` hook，不读 JSON 白名单 |
| `.codex/rules/harness9.rules`、`.codex/scripts/cleanup-knowledge-day.sh` | Codex execpolicy 沙箱专有格式，ZCode 无对应机制 |
| `.opencode/plugins/sync-to-obsidian.js` | OpenCode 插件形态，ZCode 由 `config.json` hooks 承担同一职责 |
| `.superpowers/` | 运行时产物（brainstorm/sdd 工作目录），非配置 |
| `.opencode/node_modules/` | 依赖产物，非配置 |

## 日常维护

- **新增/修改子代理**：直接编辑本目录 `agents/*.md`，新建会话后生效（`name`、`description` 必填；`model: inherit` 跟随主模型；MCP 工具需写全名 `mcp__<server>__<tool>` 且在 `mcpServers` 声明依赖）。
- **知识库日报流水线**（collector → analyzer → organizer）依赖 Obsidian vault 路径 `/Users/zsa/Desktop/workspace/harness9/`，属本机个人配置，换机器需同步调整。
- **harness-researcher 依赖 context7**：若 context7 MCP 未连接会导致该子代理调用失败；此时删除其 frontmatter 中 `mcpServers` 段和两行 `mcp__context7__*` 工具即可降级为纯 WebFetch 检索。
