---
title: "Hooks 与 Human-in-the-Loop：harness9 的工具权限拦截体系"
date: 2026-06-29
tags: [harness9, agent, golang, hooks, permission, human-in-the-loop]
summary: "harness9 的权限体系由洋葱模型 HookRegistry、HookDecision 三级决策、DangerHook 19 条子串规则、PermissionHook 动态白名单和工具层硬保护五层机制构成。本文拆解每一层的架构决策与设计取舍。"
---

# Hooks 与 Human-in-the-Loop：harness9 的工具权限拦截体系

## 关于 harness9

harness9 是一款 Local-First、轻量级、功能完备、生产可用的通用 Go Agent 框架。

- **官网**：[https://zhangshenao.github.io/harness9/zh/](https://zhangshenao.github.io/harness9/zh/)
- **GitHub**：[https://github.com/ZhangShenao/harness9](https://github.com/ZhangShenao/harness9)

⭐ Star 是对开源工作最直接的支持，欢迎提 Issue 和 PR。

---

## TL;DR

- HookRegistry 以洋葱模型包装工具注册表（Registry），工具执行前每层 Hook 依次检查、执行后再反向逐层收尾，Hook 链对引擎完全透明
- HookDecision 三级决策（Allow / Deny / Ask）通过 Context 键值传递，`withApproved` 和 `withExplicitlyAllowed` 两个独立标记防止同一工具调用被重复拦截
- DangerHook 用 19 条子串匹配规则覆盖最高频的高危 bash 操作，刻意不用正则——速度优先，误报宁多勿少
- PermissionHook（`internal/permission`）每次工具调用都从磁盘重新加载 JSON 规则文件，使"总是允许"白名单写入后立即生效，无需重启进程
- 敏感路径硬保护（`~/.ssh`、`~/.aws` 等）在工具层（`safe_path.go`）而非 Hook 层实现，是最后一道防线，不受规则配置影响
- Sub-Agent 的工具集只能是父 Agent 可用工具集的子集，权限在委派链上单向收紧，不可越权

## 本文你将学到

- 你将看清 HookRegistry 洋葱模型的控制流，以及 Context 标记如何避免多层 Hook 对同一次工具调用重复弹框
- 你将理解 DangerHook 为什么选择子串匹配而不是正则表达式
- 你将掌握 PermissionHook 的动态重载机制：一次"总是允许"点击如何从 TUI 写穿到磁盘再到下次工具调用
- 你将看清 PermissionMode 四种模式与 PlanMode 的正交关系
- 你将理解 harness9 如何在 Sub-Agent 委派链上强制权限单向收紧

---

## 洋葱模型：HookRegistry 的架构决策

工具拦截最直观的做法是在 `Execute` 里加 `if` 判断。harness9 没有这样做。

HookRegistry 以装饰器模式包装原始 `tools.Registry`，自身也实现 `tools.Registry` 接口，对引擎完全透明。引擎调用 `registry.Execute(ctx, call)`，不知道也不需要知道中间经过了几层 Hook。

```go
// HookRegistry 用 hook 链包装原始 Registry，实现 tools.Registry 接口。
// 零 hook 时行为与原始 Registry 完全一致。
type HookRegistry struct {
    inner tools.Registry
    hooks []ToolHook
}

func (r *HookRegistry) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
    executed := 0
    for _, h := range r.hooks {
        newCtx, dec, err := h.BeforeExecute(ctx, call)
        // ... 决策处理
        executed++
    }
    result := r.inner.Execute(ctx, call)
    for i := executed - 1; i >= 0; i-- {
        result = r.hooks[i].AfterExecute(ctx, call, result)
    }
    return result
}
```

执行前每层 Hook 依次往里走，执行后再从里往外逐层收尾——标准洋葱模型。`executed` 记录已完成前置检查的 Hook 数量，确保只有真正走过前置检查的 Hook 才会收到后置回调。Deny 短路后不触发任何后置回调，语义上保持一致。

![洋葱模型：HookRegistry 控制流](/blog/hooks-human-in-the-loop/images/hook-registry-onion-01.png)


---

## 三级决策：HookDecision 的设计哲学

Hook 的 BeforeExecute 返回 `HookDecision`，只有三种值：Allow、Deny、Ask。

```go
type HookDecision struct {
    Action       HookAction
    Reason       string
    RiskLevel    string          // "low" | "medium" | "high"
    ModifiedArgs json.RawMessage // hook 修改后的工具参数
}
```

Allow 和 Deny 语义直接。Ask 是最有意思的设计。

Ask 不直接弹框——它把决策权委托给 Context 中注入的 `ApprovalFunc`。Hook 不依赖 TUI，不知道审批界面长什么样，只知道"这个操作需要人类决策"。TUI 在启动时通过 `hooks.WithApprovalFn(ctx, fn)` 把审批回调注入 Context，Hook 从 Context 取出来调用。

这个设计让 Hook 与渲染层完全解耦。在 Hermetic eval 环境里，`ApprovalFunc` 可以被替换成自动批准的 mock，Hook 代码一行不改。

### Context 标记防止重复拦截

多层 Hook 叠加时，每层都可能对同一个工具调用触发 Ask。harness9 用两个独立的 Context 键处理这个问题：

```go
// 人类实时审批通过后写入
func withApproved(ctx context.Context) context.Context { ... }

// 规则显式放行（白名单命中）后写入
func withExplicitlyAllowed(ctx context.Context) context.Context { ... }
```

Ask 决策被触发时，先检查这两个标记：

```go
case HookActionAsk:
    if isApproved(newCtx) || isExplicitlyAllowed(newCtx) {
        break // 已批准或已放行，跳过审批
    }
    if fn := ApprovalFnFromContext(newCtx); fn != nil {
        resp := fn(newCtx, call, dec.Reason, dec.RiskLevel)
        // ...
        newCtx = withApproved(newCtx)
    }
```

两个标记语义不同：`withApproved` 是用户实时点击"允许"的结果；`withExplicitlyAllowed` 是白名单规则静默放行的结果。它们都能阻止后续 Hook 重复弹框，但来源可追溯，行为日志里不会混淆。

![HookDecision 三级决策状态机](/blog/hooks-human-in-the-loop/images/hook-decision-state-02.png)


---

## DangerHook：为什么不用正则

DangerHook 内置 19 条模式，检测最常见的高危 bash 操作。核心数据结构很朴素：

```go
type dangerPattern struct {
    substr    string
    riskLevel string
    reason    string
}

var defaultDangerPatterns = []dangerPattern{
    {substr: "rm -rf",   riskLevel: "high",   reason: "强制递归删除文件/目录"},
    {substr: "| bash",   riskLevel: "high",   reason: "管道执行远程脚本（curl|bash 攻击）"},
    {substr: ":(){ :|:", riskLevel: "high",   reason: "Fork Bomb"},
    {substr: "sudo ",    riskLevel: "medium", reason: "以 root 权限执行命令"},
    {substr: "systemctl ", riskLevel: "medium", reason: "管理系统服务"},
    // ...共 19 条
}
```

检测逻辑用 `strings.Contains` 做子串匹配，命令先统一转小写：

```go
cmd := strings.ToLower(args.Command)
for _, p := range h.patterns {
    if strings.Contains(cmd, strings.ToLower(p.substr)) {
        return ctx, Ask(p.reason, p.riskLevel), nil
    }
}
```

不用正则的原因是工程取舍：正则更精确，但在工具拦截这个场景里，误报宁多勿少——多问一次用户是低成本的，漏掉一次 `rm -rf` 是高成本的。子串匹配有误报（比如命令里包含 `"sudo"` 字样的注释），但不会漏报。速度也更快，每次工具调用都要经过这里。

两个变体覆盖空格差异（`"| bash"` 和 `"|bash"`）也体现了这种思路：宁可多写一条，不留规避空间。

![DangerHook 19 条模式分级](/blog/hooks-human-in-the-loop/images/danger-hook-patterns-03.png)


---

## PermissionHook：JSON 白名单与动态重载

DangerHook 是静态的——规则写死在代码里。PermissionHook 是动态的——规则从 JSON 配置文件加载，每次工具调用时重新读取磁盘。

配置格式很直观：

```json
{
  "permissions": {
    "allow": ["bash(git *)", "read_file"],
    "deny":  ["bash(rm -rf *)"],
    "ask":   ["bash(sudo *)"]
  }
}
```

规则语法分两层：
- `"toolName"` — 匹配该工具的任意调用
- `"toolName(pattern)"` — 工具名匹配 AND 参数符合 glob 模式

评估逻辑是有序匹配，第一条命中规则胜出：

```go
func (r *Rules) Evaluate(toolName, argStr string) string {
    for _, ru := range r.rules {
        for _, p := range ru.patterns {
            if matchPattern(toolName, argStr, p) {
                return ru.action
            }
        }
    }
    return RuleAsk // 无匹配时默认 Ask
}
```

`LoadRules` 加载顺序是 deny → allow → ask，确保拒绝规则最先被评估。

### 动态重载：每次调用都读磁盘

`NewFileHook` 创建的 PermissionHook 在每次 `BeforeExecute` 时调用 `LoadRules` 重新加载文件：

```go
func (h *Hook) BeforeExecute(ctx context.Context, tc schema.ToolCall) (...) {
    rules := h.rules
    if h.settingsPath != "" {
        if loaded, err := LoadRules(h.settingsPath); err == nil {
            rules = loaded
        }
    }
    // ...
}
```

这看起来有点"暴力"，但这是正确的权衡。TUI 里用户点击"总是允许"后，`writeApprovalToConfig` 立刻把该工具的模式写入 JSON 文件。下次同类工具调用时，PermissionHook 读到新规则，白名单命中，静默放行，不再弹框。整个流程不需要重启，不需要信号量，不需要状态同步。

磁盘 IO 的代价在工具调用这个粒度上完全可以接受——工具执行本身往往是毫秒到秒级的，多一次文件读取不是瓶颈。

![PermissionHook 动态重载流程](/blog/hooks-human-in-the-loop/images/permission-hook-reload-04.png)


---

## 敏感路径硬保护：不在 Hook 层做

`~/.ssh`、`~/.aws`、`~/.kube`、`~/.gnupg`、`~/.netrc`、`~/.config/gcloud` 这六个路径的保护不在 Hook 层实现，而在工具层的 `safe_path.go` 里：

```go
var hardSensitivePaths = buildSensitivePaths()

func buildSensitivePaths() []string {
    home, _ := os.UserHomeDir()
    return []string{
        filepath.Join(home, ".ssh"),
        filepath.Join(home, ".aws"),
        filepath.Join(home, ".kube"),
        filepath.Join(home, ".gnupg"),
        filepath.Join(home, ".netrc"),
        filepath.Join(home, ".config", "gcloud"),
    }
}

func safePath(workDir, inputPath string) (string, error) {
    // ... 路径规范化 ...
    if isSensitivePath(absPath) {
        return "", fmt.Errorf("路径 '%s' 是受保护的敏感路径，禁止访问", inputPath)
    }
    return absPath, nil
}
```

这是一个刻意的架构决策。Hook 层的规则可以被配置，可以被 BypassAll 模式绕过；但 `safePath` 的检查在工具内部执行，直接返回 error，不经过 Hook 链，也不受 PermissionMode 影响。凭据文件的保护是无条件的。

路径穿越（Path Traversal）防护也在同一个函数里：`filepath.Abs` 规范化后检查是否仍以 `workDir` 为前缀，绝对路径和相对路径分别处理，防止 `../` 逃逸和绝对路径翻倍（`workDir + "/workDir/subpath"` 的经典陷阱）。

![敏感路径双重防线](/blog/hooks-human-in-the-loop/images/sensitive-path-guard-05.png)

---

## TUI 五选项审批对话框

用户在 TUI 里看到审批对话框时，有五个选项：

![工具调用审批](/blog/hooks-human-in-the-loop/images/tool_call_approval.png)


选项 5 触发内联输入框，用户可以用自然语言说明拒绝原因。这段反馈通过 `ApprovalResponse.Feedback` 回传给引擎，以 `"操作被用户拒绝: <反馈>"` 的形式作为工具调用的错误结果注入上下文。LLM 能读到这段文字，下一轮可以据此调整策略。

对话框标题颜色根据 `RiskLevel` 变化：高风险红色、中风险琥珀色、低风险青色。这个信号来自 DangerHook 的 `riskLevel` 字段，从检测层一路透传到渲染层，不经过额外转换。

```go
func (m tuiModel) renderApprovalDialog() string {
    switch req.RiskLevel {
    case "high":
        titleStyle = approvalTitleHighStyle
    case "medium":
        titleStyle = approvalTitleMedStyle
    default:
        titleStyle = approvalTitleLowStyle
    }
    // ...
}
```

选项 3 的实现值得关注——`confirmApproval(2)` 调用 `writeApprovalToConfig`，把本次工具调用的模式写入 JSON 配置：

```go
case 2:
    resp = hooks.ApprovalResponse{Approved: true, Remember: true}
    m.writeApprovalToConfig(req)
```

`writeApprovalToConfig` 对 bash 工具取命令的第一个单词作为关键词，生成 `bash(*git*)` 这样的 glob 模式追加到 allow 列表。粒度不算精细，但在实际使用中够用——用户说"总是允许 git 操作"，以后所有 `git *` 命令静默通过。

![TUI 五选项审批对话框交互流](/blog/hooks-human-in-the-loop/images/tui-approval-dialog-06.png)


---

## PermissionMode 四种模式

`PermissionMode` 定义在 `internal/engine/permission.go`，与 `planning.PlanMode` 正交——两个维度独立控制不同的行为：

```go
const (
    PermissionModeDefault     PermissionMode = iota // 危险操作触发审批对话框
    PermissionModeAutoApprove                       // 白名单内自动通过，其余仍需审批
    PermissionModeReadOnly                          // 拒绝所有写操作
    PermissionModeBypassAll                         // 绕过所有权限检查
)
```

四种模式的使用场景：

- **Default**：日常开发。DangerHook + PermissionHook 正常工作，危险操作弹框。
- **AutoApprove**：已充分信任当前工作区，白名单里的操作无需每次确认。TUI `/approve` 命令切换。
- **ReadOnly**：代码审查、只读分析任务。明确阻止任何文件修改，防止 Agent 意外写入。
- **BypassAll**：Docker Sandbox 或受控 CI 环境。容器内已经有隔离保障，不需要软件层的权限检查，换来最低的工具调用延迟。

BypassAll 的存在揭示了一个重要的设计立场：harness9 的权限系统是给人机协作场景设计的，不是给完全自动化场景设计的。当运行环境本身已提供隔离时，软件层的权限检查只是开销，可以关掉。

![PermissionMode 与 PlanMode 正交关系](/blog/hooks-human-in-the-loop/images/permission-mode-matrix-07.png)


---

## Sub-Agent 权限单向收紧

Sub-Agent 的工具集在 `SubAgentDefinition.ResolveTools` 里计算：

```go
// 计算公式：可用工具集 = (父工具集 ∩ 白名单) - 黑名单 - {task}
```

这条规则保证了委派链上的权限只能越来越小，不能扩大。父 Agent 没有的工具，子 Agent 绝对不能拥有。即使 `.harness9/agents/dev.md` 里写了某个工具的白名单，如果父 Agent 启动时没有注册这个工具，子 Agent 也得不到它。

`task` 工具被从子 Agent 工具集中强制移除，阻止递归委派（Sub-Agent 再创建 Sub-Agent）。这不是技术上不可能实现，而是有意的设计约束——无限递归委派会让权限审计变得极其复杂，而且实际需求里几乎不存在需要三层以上委派的场景。

从 Hook 视角看，Sub-Agent 运行在独立的引擎实例上，拥有自己的 HookRegistry。主 Agent 的审批决策（`withApproved`、`withExplicitlyAllowed`）不会泄漏到子 Agent 的 Context，每次委派都是一个权限意义上的新会话。

![Sub-Agent 权限继承链](/blog/hooks-human-in-the-loop/images/subagent-permission-chain-08.png)


---

## 整体架构概览

从工具调用触发到最终执行，完整路径如下：

```
LLM 发出 ToolCall
    ↓
engine.Execute(ctx, call)
    ↓
HookRegistry（洋葱入口）
    ↓ BeforeExecute
PermissionHook → LoadRules(磁盘) → Evaluate → Allow/Deny/Ask
    ↓ Allow 继续
DangerHook → 子串匹配 19 条模式 → Allow/Ask
    ↓ Ask 时
ApprovalFunc(TUI) → 用户五选项 → ApprovalResponse
    ↓ Approved
tools.Registry.Execute（实际工具）
    ↓
safePath（路径穿越 + 敏感路径）← 最后一道防线，无法绕过
    ↓
各 Hook 后置收尾（从里到外逐层）
    ↓
ToolResult 回传 LLM
```

每一层都是可以独立替换的。PermissionHook 可以换成任何实现了 `ToolHook` 接口的结构体，DangerHook 可以用自定义规则覆盖，`ApprovalFunc` 在不同运行环境里映射到不同的 UI 或自动决策逻辑。`safePath` 是唯一不可替换的层，它在工具内部，不经过接口。

![完整工具调用权限拦截路径](/blog/hooks-human-in-the-loop/images/full-permission-pipeline-09.png)


---

## 结语

harness9 的权限体系没有中央策略引擎，没有 DSL，没有规则语言。它是几个简单机制的正交组合：洋葱模型 + Context 传递决策状态 + 磁盘持久化白名单 + 工具内的无条件硬保护。

真正值得思考的问题是：哪些安全约束应该可配置，哪些必须硬编码？harness9 的答案是，凭据文件不可配置，其他都可以。

---
