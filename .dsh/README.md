# `.dsh/` — DeepSeek Harness 项目级配置

本目录是 harness9 面向 **DeepSeek Harness（DSH）** 的 project scope 配置，随仓库版本化。
它把原先分散在 `.claude/`、`.codex/`、`.opencode/`、`.zcode/` 的配置合并、去重，并翻译成 DSH 真正会读取的形态。

## 为什么只有 `skills/`

DSH 的 project scope 只有两个自动发现入口，其余都是 profile（用户）级：

| 入口 | 位置 | DSH 行为 |
|---|---|---|
| 项目技能 | `<repo>/.dsh/skills/`（优先级 100） | 自动发现、按需加载、监听目录变化 |
| 工作区指令 | 仓库根 `AGENTS.md` / `CLAUDE.md` | 首次请求注入指令链，二者内容相同时自动去重 |

项目级**没有**子代理定义文件、没有预设（preset）目录、没有 MCP 配置、没有 hook。因此：

- 子代理 → 写成 `agent-*` 技能，由主 Agent 用 `subagent` 委派（见下）；
- 斜杠命令 → 写成技能，输入 `/名字` 即注入全文；
- MCP / 权限 → 属于 profile 级，见「平台差异」。

## 目录结构

```
.dsh/
├── README.md                  # 本文件：来源映射、用法、平台差异、维护约定
├── scripts/validate.py        # 技能规范校验（frontmatter / 重名 / 旧平台残留）
└── skills/
    ├── cr/                    # /cr            工作区只读代码审查
    ├── commit/                # /commit        精确暂存 + 复查 + 提交
    ├── pr/                    # /pr            推送 feature 分支 + Draft PR
    ├── release-cli/           # /release-cli   CLI 发布流水线
    ├── sync-docs/             # /sync-docs     中文文档 → 英文镜像
    ├── autodev/               # /autodev       需求澄清 → spec → 委派实现
    ├── go-coding-standards/   # 知识：Go 编码规范
    ├── architecture-overview/ # 知识：架构总览
    ├── debugging-guide/       # 知识：调试指南
    └── agent-*/               # 10 个子代理角色
        ├── agent-harness-blog-writer/
        ├── agent-harness-enhancer/
        ├── agent-harness-researcher/
        ├── agent-test-runner/
        ├── agent-collector/
        ├── agent-analyzer/
        ├── agent-organizer/
        ├── agent-dev/
        ├── agent-doc-writer/
        └── agent-explorer/
```

## 用法

### 1. 斜杠命令

输入 `/cr`、`/commit`、`/pr`、`/release-cli v1.2.3`、`/sync-docs HEAD~3`、`/autodev <功能描述>`。
`/名字` 之后的文本会作为参数一起进入本回合，技能正文由宿主直接注入，不需要模型再调用加载工具。

### 2. 自动触发

技能目录的 `description` 就是路由条件：例如「调试 Go 编译错误、测试失败、panic」会自动命中 `debugging-guide`。也可以显式说「用 go-coding-standards 检查」。

### 3. 子代理委派

每个 `agent-*` 技能的正文就是该角色的完整工作手册。委派时让子代理自己去加载，不要在主会话里全文粘贴：

```
subagent(
  description="跑全量测试",
  prompt="调用 skill 工具加载 `agent-test-runner`，并严格按该技能执行：\
运行 harness9 全量单元测试并返回结构化报告。"
)
```

要点：

- `subagent` 只返回「已启动」，结果通过完成通知异步返回；需要多个独立任务时，在同一条消息里并发发起多个 `subagent` 调用，然后先做别的工作。
- 子代理不继承主会话上下文。prompt 里要写清任务、目标路径、验收标准；角色细节交给技能。
- 默认委派深度上限为 1（子代理不能再派子代理），可在 Settings → Subagent 调整。
- 需要指定模型时用 `subagent` 的 `provider` / `model` / `reasoning_effort` 参数；角色技能里不再写死模型。
- 子代理与主 Agent 共享同一个 preset 与工作目录，所以能发现同一份 `.dsh/skills`。

### 4. 文件与路径

| 角色 | 写入范围 |
|---|---|
| `agent-harness-enhancer` | 全仓库源码 + 文档 |
| `agent-harness-blog-writer` | `website/zh/blog/<slug>/`、`website/public/blog/<slug>/images/`、VitePress 侧边栏 |
| `agent-harness-researcher` | `docs/技术调研/` |
| `agent-dev` | 委派指定的 git worktree |
| `agent-collector` / `agent-analyzer` / `agent-organizer` | 知识库 `<KNOWLEDGE_ROOT>` 下的 `raw/`、`analysis/`、`articles/` |
| `agent-test-runner` / `agent-explorer` / `agent-doc-writer` | 只读（`doc-writer` 只写文档） |

知识库三个技能的 `<KNOWLEDGE_ROOT>` 是占位符：执行前先确认本机知识库根目录的实际绝对路径（原配置写死为 `/Users/zsa/Desktop/workspace/harness9/知识库日报`），再替换。目录不存在时不要擅自建目录。

## 来源映射与去重

### 工作流（5）

| DSH 技能 | 母本 | 说明 |
|---|---|---|
| `cr` | `.opencode/commands/cr.md` | 与 `.zcode/commands/cr.md` 同源；英文加固版，比 `skills/cr` 的中文初版更严格 |
| `commit` | `.opencode/commands/commit.md` | 精确暂存、静态复查、提交后父/树/路径集校验 |
| `pr` | `.opencode/commands/pr.md` | 冻结 OID 推送、禁止 force、Draft PR |
| `release-cli` | `.opencode/commands/release-cli.md` | 比 `.claude/skills/release-cli` 更新；平台措辞改为 DSH |
| `sync-docs` | `.opencode/commands/sync-docs.md` | 三阶段：代码 diff → 中文 → 英文镜像 |

### 知识技能（4）

| DSH 技能 | 母本 |
|---|---|
| `autodev` | `skills/autodev/`（`.zcode` 已适配版） |
| `architecture-overview` | `skills/architecture-overview/` |
| `debugging-guide` | `skills/debugging-guide/` |
| `go-coding-standards` | `skills/go-coding-standards/` |

### 子代理角色（10）

| DSH 技能 | 母本 | 说明 |
|---|---|---|
| `agent-harness-blog-writer` | `.claude/agents/` | 以公众号读者画像版为母本（`.zcode` 已核对同源） |
| `agent-harness-enhancer` | `.claude/agents/` | 全仓库质量提升 |
| `agent-harness-researcher` | `.claude/agents/` | context7 改为可选增强，未接入时走 web_fetch |
| `agent-test-runner` | `.claude/agents/` | 绝对项目路径改为仓库根，模型字段移除 |
| `agent-collector` / `agent-analyzer` / `agent-organizer` | `.opencode/agents/` | 知识库日报流水线 |
| `agent-dev` / `agent-doc-writer` / `agent-explorer` | `.zcode/agents/` | 原 `.harness9/agents/`（未跟踪）迁移而来 |

**去重规则**：同一角色在多套 harness 中互为副本（`.zcode/README.md` 记录了母本选择），DSH 只保留一份。`skills/` 目录仍是 harness9 二进制自身技能系统的信息源；`.dsh/skills` 是 DSH 工具名适配版，两者关系与 `.zcode/skills` 和 `skills/` 相同。

## 平台差异

### 工具名

| 旧配置写法 | DSH |
|---|---|
| Read / read_file | `read` |
| Write / write_file | `write` |
| Edit / edit_file | `edit` |
| Bash | `bash` |
| Glob / list_dir | `glob` |
| Grep | `grep` |
| WebFetch | `web_fetch` |
| WebSearch | `web_search` |
| Task / `@dev` | `subagent` + 技能加载 |
| TodoWrite | `todo_write` |
| use_skill | `skill` |

### 模型

不再保留 `model: sonnet` / `model: claude-haiku-*` / `model: inherit`。DSH 的子代理模型由 `subagent` 调用参数或 Session 路由决定，默认继承当前会话。

### MCP（context7）

DSH 不读取 `.mcp.json`，MCP 服务器是 profile 级配置。在 **Settings → MCP** 添加 stdio 条目：

```json
{
  "context7": {
    "transport": "stdio",
    "command": "npx",
    "args": ["-y", "@upstash/context7-mcp"]
  }
}
```

接入后 `agent-harness-researcher` 会自动多用两个 `mcp__context7__*` 工具；未接入时按技能内说明降级为 `web_fetch`，不影响使用。

### 权限与沙箱

`.claude/settings.local.json` 的权限白名单、`.codex/rules/harness9.rules` 的 execpolicy 都没有 DSH 对应物：DSH 用**会话级权限预设**（`read-only` / `workspace-write` / `danger-full-access`）加工具审批兜底。`autodev` 里原 harness9 的 `SANDBOX_IMAGE` 检查也已移除，DSH 沙箱由自身配置管理。

### 指令文件

仓库根的 `AGENTS.md` 就是 DSH 的项目指令源；`CLAUDE.md` 是指向它的符号链接，DSH 按内容去重，不会重复注入。本目录不再复制一份指令文件，避免两份漂移。

也刻意没有把 DSH 说明追加进 `AGENTS.md`：该文件已超过 64 KiB 的指令预算，继续追加只会挤掉末尾内容；技能目录本身可被发现，说明放在这里即可。

## 已验证

在当前 Desktop 安装（profile `desktop`）上确认：

- `.dsh/skills` 下 19 个技能全部被宿主发现，名称与描述正确进入技能目录；
- `skill` 工具能加载角色技能，返回的 `<skill_content>` 指向项目内绝对路径；
- `subagent`（spawn）委派的子代理能发现并加载同一份 `.dsh/skills`；
- `python3 .dsh/scripts/validate.py` 通过。

未覆盖：斜杠菜单的交互高亮、`/release-cli` 的完整发布链路（需要真实 tag 与 GitHub Actions）、知识库流水线（依赖本机知识库目录）。

## 维护

1. **只改 `.dsh/skills/<name>/SKILL.md`**：不要再往 `.dsh/` 添加 `.claude` / `.codex` 式的 agents、commands 副本。
2. 新增技能：建目录 + `SKILL.md`，frontmatter 必填 `name`（kebab-case，与目录同名）和 `description`（一句话路由条件）；可选 `whenToUse`、`disable-model-invocation`、`user-invocable`。
3. 新增子代理角色：命名 `agent-<role>`，正文开头保留「DSH 子代理角色」说明块（委派方式、建议工具集、写入边界），并在本 README 的映射表登记母本。
4. 修改后校验：

   ```bash
   python3 .dsh/scripts/validate.py
   ```

5. 已打开的会话不会收到磁盘新增 `SKILL.md` 的通知；新建会话或重连后可见。正文修改对下一次加载立即生效。

## 有意未迁移

| 旧配置 | 原因 |
|---|---|
| `.codex/rules/harness9.rules` | Codex execpolicy 专有格式，DSH 无对应机制 |
| `.claude/settings.local.json` | Claude Code 权限白名单，DSH 用权限预设 + 审批 |
| `.mcp.json` | 保留给其他 harness；DSH 在 Settings → MCP 配置 |
| `opencode.json` 的 `superpowers` 外部插件 | Git 远程插件（obra/superpowers），不随仓库版本化；需要时再评估 DSH 侧等价的技能包 |
| `.codex/scripts/cleanup-knowledge-day.sh` | Codex execpolicy 的受控删除封装；DSH 侧由 `agent-organizer` 的清理步骤直接执行 |
| `.zcode/`、`.claude/`、`.codex/`、`.opencode/` | 各自客户端的配置，保持原样；本目录不引用、不复制 |

`.dsh/` 与它们并存，互不读取。若以后不再使用某个 harness，直接删除对应目录即可，`.dsh/` 不受影响。
