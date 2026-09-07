---
name: debugging-guide
description: Use when debugging Go errors, test failures, or unexpected behavior — step-by-step diagnosis approach
trigger: debug, error, fail, crash, panic, fix, wrong
---

# harness9 调试指南

## 诊断顺序

遇到问题时，按以下顺序排查：

### 1. 编译错误

```bash
go build ./...
```

常见原因：
- 未使用的 import → 删除或添加 `_` blank import
- 类型不匹配 → 检查接口实现是否完整
- 循环依赖 → 将接口定义移到使用者侧包中

### 2. 测试失败

```bash
# 详细输出
go test -v ./internal/engine/

# 单个测试
go test -v -run TestAgentLoop ./internal/engine/

# 带 race detector
go test -race ./...
```

### 3. 运行时 panic

查看完整 goroutine stack：
```bash
go run ./cmd/harness9 2>&1 | head -100
```

nil pointer panic 通常来自：
- 未初始化的 map（用 `make(map[K]V)` 初始化）
- 接口值为 nil 但调用了方法

### 4. Agent 行为异常

**LLM 不调用工具：** 检查工具的 `Definition()` 描述是否清晰，JSON Schema 是否正确。

**工具执行失败：** 查看 `ToolResult.IsError` 和 `Output` 字段，错误信息会回传给 LLM。

**无限循环：** 检查 `WithMaxTurns` 配置，默认 50 Turn。

## 常用调试技巧

### 打印 System Prompt

在 `internal/context/builder.go` 的 `Build()` 方法末尾临时添加：
```go
fmt.Fprintf(os.Stderr, "=== SYSTEM PROMPT ===\n%s\n===================\n", prompt)
```

### 检查工具注册

在 `registry.Execute` 前打印可用工具列表：
```go
for _, def := range registry.GetAvailableTools() {
    fmt.Fprintf(os.Stderr, "tool: %s\n", def.Name)
}
```

### Provider 请求/响应

如需查看实际 API 请求，在 `internal/provider/openai.go` 中打印消息列表。

## harness9 特有问题

### Anthropic Provider：user/assistant 必须严格交替

症状：`400 Bad Request` 或 `invalid_request_error`

原因：Anthropic Messages API 禁止连续 assistant 消息。

修复：检查 `contextHistory` 的消息顺序，确保 system→user→assistant→user→assistant 交替。

### 路径沙箱拒绝访问

症状：工具返回 `路径超出工作区范围` 或类似错误

原因：路径包含 `../` 或绝对路径指向启动目录之外。

修复：Agent 应使用相对于启动目录的路径，如 `internal/engine/agent_loop.go` 而非 `/absolute/path/...`。

### Skills 未加载

症状：Agent 不知道有 Skills 可用

检查：
1. `skills/` 目录是否在项目根目录（启动目录）下
2. 每个 Skill 是否在独立子目录中，且子目录内有 `SKILL.md` 文件
3. `SKILL.md` 是否包含 `name` 和 `description` frontmatter 字段
4. 启动日志中是否有 `[skills]` warn 输出

正确的目录结构示例：
```
skills/
├── go-coding-standards/
│   └── SKILL.md      ← 必须是这个文件名
└── debugging-guide/
    └── SKILL.md
```

## go vet 常见 warning

| Warning | 含义 | 修复 |
|---------|------|------|
| `printf` 格式不匹配 | `%s` 传入了非 string 类型 | 修正格式或类型 |
| `unreachable code` | return 后有代码 | 删除死代码 |
| `loop variable captured` | goroutine 捕获了循环变量 | 传参而非捕获 |
