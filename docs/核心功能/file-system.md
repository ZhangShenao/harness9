# 文件系统能力技术方案

## 1. 概述

文件系统能力是 harness9 在 Planning 模块之上叠加的一层**基础设施增强**，解决三个核心问题：

| 问题 | 解决方案 |
|------|---------|
| 工具输出过大导致 context 窗口爆炸 | OffloadHook — 自动将超阈值输出写入文件，context 中仅保留摘要引用 |
| Agent 执行计划仅存在于内存，进程重启后丢失 | FilePlanWriter — todo_write 每次写入后同步输出 markdown 计划文件 |
| 删除会话时 offload 文件残留磁盘 | Manager.DeleteSession 级联清理 `~/.harness9/tool_results/{sessionID}/` |

这三个问题通过**钩子机制**（Hooks）、**接口注入**（PlanWriter）和**选项模式**（ManagerOption）解决，各自可以独立启用或禁用，不改变引擎核心逻辑。

---

## 2. 架构：Hooks 拦截层

### 2.1 ToolHook 接口

```go
// internal/hooks/hook.go
type ToolHook interface {
    BeforeExecute(ctx context.Context, tc schema.ToolCall) (context.Context, error)
    AfterExecute(ctx context.Context, tc schema.ToolCall, result schema.ToolResult) schema.ToolResult
}
```

- `BeforeExecute` 返回 `error` 时**短路**整个调用链，返回 `IsError: true` 的 ToolResult，不执行内层工具
- `AfterExecute` 可修改 `result`，逆向执行（洋葱模型）

### 2.2 HookRegistry

`HookRegistry` 实现 `tools.Registry` 接口，包装原始 Registry：

```
用户输入 → HookRegistry.Execute
           ├─ BeforeExecute（正向）: hook[0] → hook[1] → …
           ├─ inner.Execute（原始工具）
           └─ AfterExecute（逆向）: … → hook[1] → hook[0]
```

**零 hook 时行为与原始 Registry 完全一致**，引擎不感知是否存在 Hook 层。

```go
// 组装示例（main.go）
// OffloadHook 第一个参数是 workDir，文件写入 workDir/.harness9/tool_results/{sessionID}/
offloadHook := hooks.NewOffloadHook(workDir, sess.SessionID())
hookReg := hooks.NewHookRegistry(registry, offloadHook)
eng := engine.NewAgentEngine(llm, hookReg, workDir, ...)
```

---

## 3. Context Offload（超大输出卸载）

### 3.1 设计动机

工具输出无上限时（如 `bash` 执行 `grep -r` 遍历大型代码库），单次输出可能消耗数万 token，导致：
- context 窗口在一两轮内耗尽
- LLM 无法处理后续指令
- 压缩压力骤增，摘要语义损失

Offload 的核心思路：**将数据移出 context，在 context 中留一个"指针"**。LLM 可通过 `read_file + offset/limit` 按需检索。

### 3.2 实现：OffloadHook

```go
// internal/hooks/offload.go
type OffloadHook struct {
    workDir      string  // Agent 工作区根目录，offload 文件写入其 .harness9 子目录
    sessionID    string
    threshold    int     // 默认 10000 字符
    previewLines int     // 默认 20 行
}
```

**触发条件：**
- `len(result.Output) > threshold`（默认 10,000 字符）
- 工具不在排除列表 `{read_file, write_file, edit_file}`（避免读写循环）

**执行流程：**
1. `os.MkdirAll({workDir}/.harness9/tool_results/{sessionID}/, 0700)`
2. `os.WriteFile({dir}/{toolCallID}.txt, 完整输出, 0600)`
3. 将 `result.Output` 替换为摘要引用（使用相对于 `workDir` 的相对路径，LLM 可直接传给 `read_file`）：

```
[输出已保存至 .harness9/tool_results/{sessionID}/{id}.txt，共 847 行 / 32416 字节。
可通过 read_file 工具配合 offset/limit 参数分页读取。

预览（前 20 行）：
...（前 20 行内容）...
...（已截断）]
```

4. **Fail-open**：`os.MkdirAll` 或 `os.WriteFile` 失败时，原样返回原始结果，不中断 agent loop

### 3.3 文件命名规则

```
{workDir}/.harness9/
└── tool_results/
    └── {sessionID}/
        ├── {toolCallID-1}.txt   # 第一次超阈值输出
        ├── {toolCallID-2}.txt   # 第二次
        └── ...
```

`toolCallID` 由引擎在每次工具调用时生成（UUID），与 context 中的引用路径一一对应。

### 3.4 read_file 分页扩展

为支持 LLM 分段检索 offload 文件，`read_file` 工具增加了 `offset` / `limit` 参数：

```json
{
  "path": "相对或绝对路径",
  "offset": 8192,   // 起始字节（可选，默认 0）
  "limit": 8192     // 读取字节数（可选，默认 8192，即 read_file 的 maxReadLen）
}
```

**边界处理：**
- `offset >= totalSize`：返回 `[offset=N 超出文件大小（T 字节），无内容可读。]`，不报错
- 读取量超过 `limit`（多读 1 字节检测）：追加截断提示 `"如需继续读取请使用 offset=N"`，LLM 可自动续读

**沙箱限制**：偏移读取仍通过 `safePath()` 校验，路径穿越攻击无效。

### 3.5 System Prompt 集成

`PromptBuilder.WithOffloadEnabled(true)` 在 System Prompt 中注入检索指引：

```
## 大输出文件检索

当工具输出超过阈值时，完整内容会自动保存到文件系统，context 中仅显示路径引用和预览。
如需查看完整输出，使用 read_file 工具并指定 offset/limit 参数分页读取：
- offset：起始字节位置（默认 0）
- limit：读取字节数（默认 8192）
示例：read_file({"path": "/path/to/offload/file.txt", "offset": 8192, "limit": 8192})
```

---

## 4. Plan 持久化（FilePlanWriter）

### 4.1 设计动机

Planning 模块的 `TodoStore` 将任务列表保存在内存（每次会话启动时从 SQLite 恢复）。但用户往往希望将计划以**人类可读的格式**保存到项目目录，方便：
- 在 IDE 或文本编辑器中查看当前执行进度
- 在 git 仓库中追踪 AI 执行的任务历史

### 4.2 PlanWriter 接口（解耦设计）

```go
// internal/planning/plan_writer.go
type PlanWriter interface {
    Write(todos []TodoItem) error
}
```

接口定义在 `planning` 包（使用者侧），避免 `tools → hooks → tools` 的循环导入：

```
tools.TodoWriteTool
  └─ planning.PlanWriter（接口）
       └─ hooks.FilePlanWriter（实现）
```

### 4.3 FilePlanWriter 实现

```go
// internal/hooks/plan_writer.go
type FilePlanWriter struct {
    path      string  // 计划文件绝对路径，构造时确定，后续不变
    sessionID string
}
```

**路径选择策略：**
- 检测 `workDir/.git` 是否存在
  - **Git 项目**：写入 `{workDir}/.harness9/plans/{timestamp}-{sessionID[:8]}.md`
  - **非 Git 目录**：写入 `{homeDir}/.harness9/plans/{timestamp}-{sessionID[:8]}.md`

**构造时快速失败：** `os.MkdirAll` 失败则立即返回 error（而非懒创建），确保启动时即发现权限问题。

**文件内容格式：**
```markdown
# 执行计划

session: abc12345-...
updated: 2026-05-22T15:30:00+08:00

## 任务列表

- [ ] 创建目录结构
- [>] 初始化 go.mod
- [x] 实现 main.go
- [-] 删除旧文件（已取消）
```

状态标记映射：

| TodoStatus | 标记 |
|------------|------|
| pending    | `[ ]` |
| in_progress | `[>]` |
| completed  | `[x]` |
| cancelled  | `[-]` |

**Fail-open：** `Write` 失败时，`todo_write` 工具仅记录日志，不中断 agent loop：

```go
// tools/todo_write.go
if err := t.planWriter.Write(current); err != nil {
    log.Print(logfmt.FormatMsg("todo_write", fmt.Sprintf("写入计划文件失败: %v", err)))
}
```

### 4.4 注入方式

通过选项模式注入，`nil` 时跳过（无操作）：

```go
tools.NewTodoWriteTool(todoStore, tools.WithPlanWriter(planWriter))
```

---

## 5. Session GC（Offload 文件级联清理）

### 5.1 问题

用户通过 `/new` 或其他机制删除会话时，对应的 offload 文件（`~/.harness9/tool_results/{sessionID}/`）需同步清理，避免长期积累磁盘占用。

### 5.2 实现

`memory.Manager` 通过 `WithToolResultsDir` 选项接收 offload 根目录：

```go
mgr, err := memory.NewManager(
    filepath.Join(homeDir, ".harness9", "sessions.db"),
    memory.WithToolResultsDir(toolResultsDir),
)
```

`DeleteSession` 在删除 SQLite 记录后，级联清理对应目录：

```go
func (m *Manager) DeleteSession(ctx context.Context, id string) error {
    _, err := m.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
    if err != nil {
        return fmt.Errorf("删除会话: %w", err)
    }
    if m.toolResultsDir != "" {
        _ = os.RemoveAll(filepath.Join(m.toolResultsDir, id))
    }
    return nil
}
```

`os.RemoveAll` 的错误被静默忽略（文件系统 GC 失败不应影响会话删除语义）。

---

## 6. 数据流总览

```
用户输入 → engine.runLoop
         └─ hookReg.Execute(toolCall)
              ├─ OffloadHook.BeforeExecute（空操作）
              ├─ inner.Registry.Execute（工具实际执行）
              │    └─ (bash / read_file / write_file / ...)
              └─ OffloadHook.AfterExecute
                   ├─ len(output) ≤ threshold → 原样返回
                   └─ len(output) > threshold
                        ├─ os.WriteFile(~/.harness9/tool_results/{sid}/{id}.txt)
                        └─ result.Output = 摘要引用 + 预览

LLM 需要完整内容时：
    read_file({path, offset, limit}) → 分页返回文件内容

todo_write 每次写入时：
    TodoStore.Write → planWriter.Write
                      └─ os.WriteFile({workDir}/.harness9/plans/{ts}-{sid}.md)

会话删除时：
    Manager.DeleteSession
        ├─ SQL DELETE（级联删除 messages、todos）
        └─ os.RemoveAll(~/.harness9/tool_results/{sessionID}/)
```

---

## 7. 文件系统目录结构

```
~/.harness9/
├── sessions.db                          # SQLite 会话数据库
├── tool_results/
│   ├── {sessionID-1}/
│   │   ├── {toolCallID-a}.txt           # 某次 bash 的超大输出
│   │   └── {toolCallID-b}.txt
│   └── {sessionID-2}/
│       └── ...
└── plans/                               # 非 git 目录时的计划存储位置
    └── {timestamp}-{sessionID[:8]}.md

{workDir}/.harness9/
└── plans/                               # git 项目时的计划存储位置
    └── {timestamp}-{sessionID[:8]}.md
```

---

## 8. 配置参数

| 参数 | 位置 | 默认值 | 说明 |
|------|------|--------|------|
| `threshold` | `OffloadHook` | 10,000 字符 | 超过此长度触发 offload |
| `previewLines` | `OffloadHook` | 20 行 | context 中保留的预览行数 |
| `maxReadLen` | `read_file` | 4,096 字节 | 不指定 `limit` 时的单次读取上限 |
| `toolResultsDir` | `Manager` | `~/.harness9/tool_results` | offload 根目录，空字符串时禁用 GC |

---

## 9. 扩展：自定义 Hook

实现 `ToolHook` 接口即可注入新行为，例如添加审计日志：

```go
type AuditHook struct{ log *slog.Logger }

func (h *AuditHook) BeforeExecute(ctx context.Context, tc schema.ToolCall) (context.Context, error) {
    h.log.Info("tool start", "name", tc.Name, "id", tc.ID)
    return ctx, nil
}

func (h *AuditHook) AfterExecute(ctx context.Context, tc schema.ToolCall, result schema.ToolResult) schema.ToolResult {
    h.log.Info("tool done", "name", tc.Name, "is_error", result.IsError)
    return result
}

// 组装时插入 HookRegistry
hookReg := hooks.NewHookRegistry(registry, offloadHook, &AuditHook{log: logger})
```

多个 Hook 的执行顺序遵循洋葱模型：BeforeExecute 正向、AfterExecute 逆向。
