package tools

import (
	"context"

	"github.com/harness9/internal/schema"
)

// mockRegistry 是 Registry 的桩实现，用于集成测试和早期开发。
// 对任何工具调用均返回硬编码的文件列表，模拟一次成功的 bash 执行，
// 无需实际运行命令。
type mockRegistry struct{}

// GetAvailableTools 返回默认的Bash工具
// mock provider 不依赖 ToolDefinition 来决定调用什么工具。
func (m *mockRegistry) GetAvailableTools() []schema.ToolDefinition {
	return []schema.ToolDefinition{{Name: "bash"}}
}

// Execute 模拟工具执行：无论传入的 ToolCall 是什么，都返回一个确定的文件列表结果。
// 在生产 Registry 中，此方法会根据 call.Name 分发到具体的工具实现。
func (m *mockRegistry) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
	return schema.ToolResult{
		ToolCallID: call.ID,
		Output:     "-rw-r--r--@ 1 zsa  staff   866B Apr 20 17:08 main.go\n",
		IsError:    false,
	}
}

// NewMockRegistry 构造并返回一个新的 mockRegistry 实例。
func NewMockRegistry() Registry {
	return &mockRegistry{}
}
