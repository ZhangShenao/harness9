---
name: go-coding-standards
description: Use when writing or reviewing Go code — explains harness9 project coding conventions and patterns
trigger: write, review, refactor, add, implement
---

# harness9 Go 编码规范

## 命名规范

| 类别 | 规范 | 示例 |
|------|------|------|
| 包名 | 小写、单单词、无下划线 | `engine`、`provider`、`schema` |
| 导出类型/函数 | PascalCase | `AgentEngine`、`NewRegistry` |
| 未导出类型/函数 | camelCase | `mainLoop`、`runLoop` |
| 接口名 | 以 `-er` 后缀为惯例 | `Provider`、`Registry`、`BaseTool` |
| 常量 | PascalCase（导出）或 camelCase（未导出），**不使用全大写** | `RoleSystem`、`maxLogOutputLen` |
| 配置选项函数 | `With` 前缀 | `WithMaxTurns`、`WithToolTimeout` |

## 错误处理

- 显式检查所有 `error` 返回值，**禁止 `_` 忽略**
- 错误消息不以大写字母开头、不以句号结尾
- 使用 `fmt.Errorf("context: %w", err)` 包装错误，保留错误链

```go
// ✅ 正确
result, err := doSomething()
if err != nil {
    return fmt.Errorf("do something: %w", err)
}

// ❌ 错误
result, _ := doSomething()
```

## 构造函数

命名规范：`New` + 类型名

```go
func NewReadFileTool(workDir string) *ReadFileTool {
    return &ReadFileTool{workDir: filepath.Clean(workDir)}
}
```

## 接口定义原则

**接口定义在使用者侧，而非实现者侧。**

```go
// ✅ 正确：Registry 接口定义在 tools 包（使用者侧）
// internal/tools/registry.go
type Registry interface {
    Register(tool BaseTool) error
    GetAvailableTools() []schema.ToolDefinition
    Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult
}

// ❌ 错误：不要在实现所在的包中定义接口
```

## 并发模式

并发工具执行使用预分配切片 + 索引写入，确保结果顺序：

```go
// Go 1.22+ 中 for range 每次迭代已自动创建新绑定，无需手动传参捕获。
// 本项目使用 Go 1.25，以下写法是正确的：
results := make([]schema.ToolResult, len(toolCalls))
var wg sync.WaitGroup
for i, tc := range toolCalls {
    wg.Add(1)
    go func() {
        defer wg.Done()
        toolCtx, cancel := context.WithTimeout(ctx, e.toolTimeout)
        defer cancel()
        results[i] = e.registry.Execute(toolCtx, tc)
    }()
}
wg.Wait()
```

## 添加新工具的步骤

1. 在 `internal/tools/` 下创建 `xxx.go`，实现 `BaseTool` 接口
2. 使用 `safePath()` 校验所有文件路径参数
3. 在 `internal/tools/xxx_test.go` 中添加表驱动测试
4. 在 `cmd/harness9/main.go` 中注册工具

```go
type MyTool struct {
    workDir string
}

func (t *MyTool) Name() string { return "my_tool" }
func (t *MyTool) Definition() schema.ToolDefinition { /* JSON Schema */ }
func (t *MyTool) Execute(ctx context.Context, args json.RawMessage) (string, error) { /* ... */ }
```

## 测试规范

- 使用标准库 `testing` 包，不引入第三方断言库
- 表驱动测试优先
- 运行：`go test ./...`

```go
func TestXxx(t *testing.T) {
    cases := []struct {
        name  string
        input string
        want  string
    }{
        {"normal", "foo", "bar"},
        {"empty", "", ""},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            got := Xxx(tc.input)
            if got != tc.want {
                t.Errorf("got %q, want %q", got, tc.want)
            }
        })
    }
}
```

## 代码检查

提交前必须通过：

```bash
gofmt -l .        # 无输出表示格式正确
go vet ./...      # 无 warning
go test ./...     # 全部通过
```
