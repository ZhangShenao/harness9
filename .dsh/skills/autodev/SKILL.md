---
name: autodev
description: "功能自动开发：澄清需求 → 生成 spec → 委派 agent-dev 在 git worktree 实现并合并回当前分支。/autodev <功能> 调用。"
---

# /autodev — Feature Auto-Development Workflow

当用户输入 `/autodev <功能描述>` 时，你按以下三个阶段工作。

---

## Phase 1: 需求澄清

首先用 `read` 读取 `AGENTS.md`，了解项目当前模块、架构和规范（重点：第 4 节项目结构、第 5 节开发流程）。

然后向用户提 **2-3 个关键澄清问题，每次只问一个**：

1. **功能边界**：明确实现什么、不实现什么
2. **验收标准**：什么样的测试证明功能完成（单元测试？eval 用例？）
3. **影响范围**：新增文件为主还是修改已有模块

2-3 轮澄清后直接进入 Phase 2，不要过度追问。

---

## Phase 2: Spec 生成与确认

以如下格式生成并展示 spec：

```
## Feature Spec: <标题>

### 功能描述
<简洁描述该功能做什么>

### 实现范围
新增文件：
- internal/<pkg>/<file>.go
- internal/<pkg>/<file>_test.go

修改文件：
- cmd/harness9/main.go（如需注册新工具）

### 验收标准
- [ ] go build ./... 通过
- [ ] go test ./... 通过
- [ ] （如涉及 Agent 行为）internal/evals/dataset/ 新增 eval 用例

### 不在范围内
- <明确列出不做的事>
```

展示完成后，输出以下提示，**不要调用任何工具，直接结束本轮回复**（DSH 会在本轮结束后等待用户输入）：

> **请确认 spec（输入「确认」继续实现），或告知需要修改的地方。**

如果用户要求修改，更新 spec 后重新展示，再次等待确认。
只有用户明确输入「确认」后才进入 Phase 3。

---

## Phase 3: 前置检查 + 委派实现

### 3.1 前置检查

依次用 bash 执行以下检查，任一失败则停下来告知用户并等待修复：

```bash
# 检查 Go 已安装（项目要求 1.25+）
go version
```

```bash
# 检查 git 可用
git --version
```

DSH 侧无需额外沙箱配置：子代理与主 Agent 使用同一会话权限预设。若会话处于 `read-only` 预设，先切到 `workspace-write`（权限不足时向用户说明，不要绕过审批）。

### 3.2 创建 git worktree

从 spec 标题生成 slug（全小写，空格替换为 `-`，去掉非 ASCII 字母数字字符），然后执行：
示例：`"Add WebSocket 支持"` → `"add-websocket-"`（中文字符去掉）→ 可简化为 `"add-websocket-support"`

```bash
git worktree add .autodev/<slug> -b feature/autodev-<slug>
```

用 bash 获取绝对路径（macOS 的 `readlink` 不支持 `-f`，用下面的写法）：
```bash
cd .autodev/<slug> && pwd
```

### 3.3 委派 dev sub-agent

DSH 用 `subagent` 工具委派，prompt 必须让子代理自己加载角色技能（子代理不继承本会话上下文）：

```
subagent(
  description="autodev 实现",
  prompt="调用 skill 工具加载 `agent-dev`，并严格按该技能执行。\n\nFeature Spec：\n<spec 全文>\n\n工作目录（git worktree 绝对路径）：<worktreePath>"
)
```

`subagent` 只返回「已启动」，不会阻塞等待。**本轮到此结束**，等子代理的完成通知再继续：通知里应包含 `AUTODEV_RESULT: SUCCESS/FAILED` 与 `BRANCH: <branch-name>`。

### 3.4 处理结果

**成功（完成通知里 `AUTODEV_RESULT: SUCCESS`）：**

从 sub-agent 返回结果中提取 `BRANCH: <branch-name>`，然后执行合并：

```bash
# 将 feature 分支合并到当前分支
git merge feature/autodev-<slug>
```

若合并成功：
1. 展示：「✓ 已合并到当前分支：feature/autodev-<slug>」
2. 清理 worktree：`git worktree remove .autodev/<slug>`

若合并产生冲突：
1. 展示冲突文件列表
2. 保留 worktree：「存在合并冲突，worktree 保留在 .autodev/<slug>，请手动解决冲突」
3. 告知用户解决后执行：`git worktree remove .autodev/<slug> --force`

**失败（完成通知里 `AUTODEV_RESULT: FAILED`）：**
1. 展示错误摘要
2. 保留 worktree 供排查：「worktree 保留在 .autodev/<slug>，可用 `cd .autodev/<slug>` 进入排查」
3. 告知用户可手动删除：`git worktree remove .autodev/<slug> --force`
