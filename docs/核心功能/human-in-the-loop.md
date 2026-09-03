# Human-in-the-Loop 权限控制

harness9 的 Human-in-the-Loop（HITL）模块解决一个核心问题：**如何在不破坏 Agent 流畅运行的前提下，让人类对高危操作保持实质性控制？**

大多数框架要么完全信任 Agent（YOLO 模式），要么在每次工具调用前都弹出确认框（让人精疲力竭）。harness9 走中间路线：内置规则引擎根据风险等级自动决策，只有真正需要人类判断的操作才会暂停并弹出审批对话框。

---

## 系统架构

```
internal/hooks/
├── decision.go      # HookDecision（allow/deny/ask）、ApprovalResponse、ApprovalFunc、context 注入/提取
├── hook.go          # ToolHook 接口（返回 HookDecision）+ HookRegistry（洋葱模型 Execute）
└── danger_hook.go   # 内置高危命令拦截（bash 模式匹配）

internal/permission/
├── rules.go         # Rules（glob 匹配）+ LoadRules / SaveRules（JSON 配置）
└── hook.go          # PermissionHook（按需重载配置文件）

internal/engine/
├── stream.go        # EventApprovalRequired + ApprovalRequest 载荷
├── agent_loop.go    # emitter.approval 字段（emitter 定义）
├── tools_exec.go    # executeTools 将 ApprovalFunc 注入工具执行 context
└── permission.go    # PermissionMode 枚举

internal/tools/
└── safe_path.go     # 硬编码敏感路径拦截（~/.ssh、~/.aws 等）

cmd/harness9/
├── tui.go           # 审批对话框状态字段 + 样式变量
├── tui_update.go    # handleApprovalKey / confirmApproval / writeApprovalToConfig
└── tui_view.go      # renderApprovalDialog()
```

---

## 工作流概览

```
工具调用请求
      │
      ▼
┌─────────────────────────────────────────────────────┐
│  HookRegistry.Execute（洋葱模型）                    │
│                                                     │
│  1. PermissionHook ─── 读取 settings.json            │
│     ├── allow 规则命中  → Allow（直接放行）           │
│     ├── deny  规则命中  → Deny（立即拒绝）            │
│     └── 无匹配          → Ask（进入审批流程）         │
│                                                     │
│  2. DangerHook ──────── 模式匹配内置高危命令          │
│     ├── 已被前置 hook 批准  → 跳过（防双重弹框）      │
│     ├── 高危模式命中        → Ask（高/中风险）        │
│     └── 未命中             → Allow                  │
│                                                     │
│  3. OffloadHook ─────── 大输出文件转储（AfterExecute）│
└─────────────────────────────────────────────────────┘
      │
      │ HookActionAsk + ApprovalFunc 存在
      ▼
┌─────────────────────────────────────────────────────┐
│  engine.executeTools（工具 goroutine）               │
│                                                     │
│  发送 EventApprovalRequired → ch（event channel）   │
│  ⟳ 阻塞在 ResponseCh 等待用户决策                    │
└─────────────────────────────────────────────────────┘
      │
      │ TUI 接收到 EventApprovalRequired
      ▼
┌─────────────────────────────────────────────────────┐
│  TUI 审批对话框（不恢复 readNextEvent）               │
│                                                     │
│  [1] 允许（仅本次）                                  │
│  [2] 允许（本会话不再提示）                           │
│  [3] 总是允许（写入白名单）                           │
│  [4] 拒绝                                           │
│  [5] 拒绝并提供反馈...                               │
└─────────────────────────────────────────────────────┘
      │
      │ confirmApproval → ResponseCh <- resp
      ▼
  工具 goroutine 解除阻塞，继续执行或返回 IsError=true
```

---

## HookDecision 决策类型

```go
type HookAction string

const (
    HookActionAllow HookAction = "allow" // 继续执行，传递给后续 hook
    HookActionDeny  HookAction = "deny"  // 立即拒绝，AfterExecute 不调用
    HookActionAsk   HookAction = "ask"   // 请求人类审批
)

type HookDecision struct {
    Action       HookAction
    Reason       string          // 展示给用户的原因
    RiskLevel    string          // "high" | "medium" | "low"（影响对话框配色）
    ModifiedArgs json.RawMessage // 可选：hook 修改后的工具参数
}
```

构造函数：

```go
hooks.Allow()                      // 放行
hooks.Deny("路径已被锁定")           // 拒绝，携带原因
hooks.Ask("递归删除操作", "high")   // 请求审批，指定风险级别
```

---

## ToolHook 接口

```go
type ToolHook interface {
    // BeforeExecute 在工具执行前触发，返回结构化决策。
    BeforeExecute(ctx context.Context, tc schema.ToolCall) (context.Context, HookDecision, error)
    // AfterExecute 在工具执行后触发，可修改返回结果。
    AfterExecute(ctx context.Context, tc schema.ToolCall, result schema.ToolResult) schema.ToolResult
}
```

`HookRegistry.Execute` 按洋葱模型依次调用 hook 链：

- `error` → 立即短路，返回 `IsError=true`
- `HookActionDeny` → 立即拒绝，跳过后续 hook 和所有 `AfterExecute`
- `HookActionAsk` → 查找 context 中的 `ApprovalFunc`；若已被人类审批（`withApproved`）**或**已被规则显式放行（`withExplicitlyAllowed`）则跳过重复弹框；若无 `ApprovalFunc`（非交互模式）则视为 Allow
- `HookActionAllow` → 在 context 中设置 `withExplicitlyAllowed` 标记（使后续 hook 的 Ask 跳过审批），并应用 `ModifiedArgs`（若 hook 携带了参数重写）

**两种"已放行"标记的语义区分：**
- `withApproved`（`approvedContextKey`）：用户在审批对话框中实时点击"允许"后设置，表示人类介入批准
- `withExplicitlyAllowed`（`explicitlyAllowedContextKey`）：前置 hook 根据规则静默放行（如白名单命中）后设置，无需人类介入

两个标记在 `HookActionAsk` 的检测逻辑中等价（均跳过审批），但保留独立 key 确保来源可追溯。

`AfterExecute` 仅对已完成 `BeforeExecute` 的 hook 逆序调用（`executed` 计数器保证）。

---

## 内置高危命令拦截（DangerHook）

`DangerHook` 仅对 `bash` 工具生效，通过子串匹配（大小写不敏感）检测危险模式：

| 风险级别 | 模式 | 原因 |
|---------|------|------|
| high | `rm -rf` | 强制递归删除 |
| high | `rm -r /` | 强制递归删除根目录 |
| high | `\| bash`、`\|bash` | 管道执行远程脚本（含无空格变体） |
| high | `\| sh`、`\|sh` | 管道执行远程脚本（含无空格变体） |
| high | `:(){ :|:` | Fork Bomb |
| high | `dd if=` | 直接写入块设备（可能覆盖磁盘） |
| high | `> /dev/` | 写入设备文件 |
| high | `chmod -r 777` | 递归赋予所有人全部权限 |
| high | `chown -r` | 递归修改文件所有者 |
| medium | `sudo ` | 以 root 权限执行命令 |
| medium | `chmod 777 ` | 赋予所有人全部权限 |
| medium | `chmod +x ` | 添加可执行权限 |
| medium | `pkill ` | 按名称杀死进程 |
| medium | `kill -9 ` | 强制杀死进程 |
| medium | `killall ` | 杀死所有同名进程 |
| medium | `iptables ` | 修改防火墙规则 |
| medium | `systemctl ` | 管理系统服务 |

非 bash 工具直接返回 Allow，无 bash 参数或解析失败时 fail-open（Allow）。

---

## 权限规则配置（settings.json）

配置文件位于 `{workDir}/.harness9/settings.json`，JSON 格式：

```json
{
  "permissions": {
    "allow": ["bash(git *)", "read_file", "bash(*go test*)"],
    "deny":  ["bash(rm -rf *)"],
    "ask":   ["bash(sudo *)"]
  }
}
```

**规则语法：**

| 语法 | 语义 |
|------|------|
| `"read_file"` | 匹配该工具的任意调用 |
| `"bash(git *)"` | bash 工具，命令以 `git ` 开头的任意调用 |
| `"bash(*docker*)"` | bash 工具，命令中包含 `docker` |

**匹配优先级：** 规则按声明顺序匹配，第一条命中规则生效；文件加载顺序为 deny → allow → ask；无匹配时默认 Ask。

**动态更新：** `PermissionHook` 每次工具调用时从磁盘重新读取配置文件（`NewFileHook`），用户在审批对话框中选择「总是允许」后，下次同类调用立即生效，无需重启。

**`SaveRules` 注意事项：** 序列化后重新加载时，规则顺序重置为 deny→allow→ask，与原始插入顺序无关。

---

## 敏感路径硬保护（safe_path）

`safePath()` 在所有文件工具（`read_file`、`write_file`、`edit_file`）中运行，无论任何配置都拒绝访问以下路径：

```
~/.ssh        ~/.aws         ~/.kube
~/.gnupg      ~/.netrc       ~/.config/gcloud
```

注意：`bash` 工具不经过 `safePath`，针对 bash 访问敏感文件的防护由 `DangerHook` 模式匹配负责。

---

## 引擎审批事件

`RunStream` 的 `emitter.approval` 闭包把 Hook 层的 `HookActionAsk` 转换为事件驱动流：

```go
// 工具 goroutine 发送事件并阻塞等待响应
approval: func(ctx context.Context, tc schema.ToolCall, reason, riskLevel string) hooks.ApprovalResponse {
    if e.permissionMode == PermissionModeBypassAll {
        return hooks.ApprovalResponse{Approved: true}
    }
    respCh := make(chan hooks.ApprovalResponse, 1)
    req := ApprovalRequest{ToolCall: tc, Reason: reason, RiskLevel: riskLevel, ResponseCh: respCh}
    select {
    case <-ctx.Done():
        return hooks.ApprovalResponse{Approved: false}
    case ch <- Event{Type: EventApprovalRequired, Data: req}:
    }
    select {
    case <-ctx.Done():
        return hooks.ApprovalResponse{Approved: false}
    case resp := <-respCh:
        return resp
    }
},
```

**并发安全：** `ch`（event channel）无缓冲，多个工具 goroutine 同时请求审批时，第二个 goroutine 阻塞在 `ch <- Event`，直到 TUI 处理完第一个审批并恢复消费 channel。TUI 实现中，`handleEvent` 收到 `EventApprovalRequired` 后返回 `nil`（不调用 `readNextEvent`），仅通过键盘事件驱动，因此不会在持有对话框期间阻塞 channel 消费。

**非交互模式（`Run`）：** `emitter.approval` 为 `nil`，`HookActionAsk` 在 `HookRegistry.Execute` 中无 `ApprovalFunc` 可调，自动视为 Allow，保持向后兼容。

---

## TUI 审批对话框

对话框在 `approvalPending == true` 时替换普通输入行渲染，显示在状态栏上方：

```
╭─────────────────────────────────────────────────────╮
│  ⚠  工具审批请求 [高风险]                             │
│                                                     │
│  工具：bash                                         │
│  原因：强制递归删除文件/目录                          │
│                                                     │
│  ▶ [1] 允许（仅本次）                               │
│    [2] 允许（本会话不再提示）                         │
│    [3] 总是允许（写入白名单）                         │
│    [4] 拒绝                                         │
│    [5] 拒绝并提供反馈...                             │
│                                                     │
│  ↑↓ 移动  Enter/1-5 确认  Esc 拒绝                  │
╰─────────────────────────────────────────────────────╯
```

**键盘交互：**

| 按键 | 动作 |
|------|------|
| `↑` / `↓` | 移动光标 |
| `1`-`5` | 直接选择对应选项 |
| `Enter` | 确认当前光标选项（选项 5 进入反馈输入模式） |
| `Esc` | 直接拒绝（等同选项 4） |
| `Ctrl+C` / `Ctrl+D` | 拒绝并中断 |

**风险级别配色：**
- `high` → 红色（`#160`）
- `medium` → 橙色（`#208`）
- `low` / 未知 → 黄色（`#220`）

**选项 5 反馈输入模式：** 进入后普通字符输入追加到反馈文字，`Enter` 提交，`Esc` 取消返回选项模式，反馈文字随 `ApprovalResponse.Feedback` 回传给 LLM。

---

## PermissionMode

`engine.PermissionMode` 是与 `planning.PlanMode` 正交的全局权限策略：

| 模式 | 说明 |
|------|------|
| `PermissionModeDefault` | 不在白名单内的危险操作触发审批对话框（默认） |
| `PermissionModeAutoApprove` | 白名单内操作自动通过（待实现） |
| `PermissionModeReadOnly` | 拒绝所有写操作（待实现） |
| `PermissionModeBypassAll` | 跳过所有权限检查，审批闭包直接返回 Approved=true |

```go
eng := engine.NewAgentEngine(llm, hookReg, workDir,
    engine.WithPermissionMode(engine.PermissionModeBypassAll),
)
```

当前非 Default 模式（除 BypassAll 外）显示在 TUI 状态栏中。

---

## 扩展：自定义 Hook

实现 `hooks.ToolHook` 接口即可接入 HookRegistry：

```go
type MyAuditHook struct{}

func (h *MyAuditHook) BeforeExecute(ctx context.Context, tc schema.ToolCall) (context.Context, hooks.HookDecision, error) {
    log.Printf("audit: %s %s", tc.Name, tc.Arguments)
    return ctx, hooks.Allow(), nil
}

func (h *MyAuditHook) AfterExecute(ctx context.Context, tc schema.ToolCall, result schema.ToolResult) schema.ToolResult {
    return result
}

// 注册，顺序决定执行优先级
hookReg := hooks.NewHookRegistry(registry, permHook, dangerHook, &MyAuditHook{}, offloadHook)
```

**设计要点：**
- `BeforeExecute` 返回 `Deny` 时，`AfterExecute` 不会被调用（已通过 `executed` 计数器保证）
- `Ask` 时若 context 已被标记为"本次已批准"（`withApproved`），直接视为 Allow，避免多个 hook 重复弹框
- `fail-open` 原则：安全防线应设置合理的默认值，避免误拦截正常操作导致 Agent 卡死

---

## 已知限制

- **「本会话不再提示」** 选项目前与「仅本次」行为等同，会话级内存白名单待实现
- `PermissionModeAutoApprove` 和 `PermissionModeReadOnly` 枚举已定义，具体行为待实现
- 白名单写入时取命令第一个单词生成模式（如 `bash(*mkdir*)`），可能比预期宽松
- `bash` 工具不经过 `safePath`，复杂 shell 脚本可能绕过 `DangerHook` 的字符串匹配
- `settings.json` 中 `ask` 列表触发的审批对话框风险级别固定为"中等风险"（橙色），无法在配置中按规则指定 `high`/`low`；`DangerHook` 自身可区分高/中风险级别

## Bug 修复记录

**2026-06-08 白名单写入后仍重复弹出审批**（`self-dev` 分支）

**根因：** `PermissionHook` 返回 `HookActionAllow`（白名单命中）时，原实现未在 context 中记录"已放行"标记。后续 `DangerHook` 检测到危险模式仍返回 `HookActionAsk`，此时 context 中无任何批准标记，导致审批对话框被二次触发。

**修复：** 新增独立的 `explicitlyAllowedContextKey`（区别于人类审批的 `approvedContextKey`），`HookActionAllow` 时写入此 key；`HookActionAsk` 检测逻辑同时检查两个 key，任意一个置位则跳过审批。
