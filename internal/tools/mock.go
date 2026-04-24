package tools

import (
	"context"
	"log"

	"github.com/harness9/internal/schema"
)

// mockRegistry 是 Registry 的桩实现，用于集成测试和早期开发。
// 注册了一个 get_weather 工具，对任何调用均返回硬编码的天气查询结果，
// 无需实际调用天气 API。
type mockRegistry struct{}

// GetAvailableTools 返回 get_weather 工具定义，包含城市参数的 JSON Schema。
func (m *mockRegistry) GetAvailableTools() []schema.ToolDefinition {
	return []schema.ToolDefinition{
		{
			Name:        "get_weather",
			Description: "获取指定城市的天气情况。",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"city": map[string]interface{}{
						"type": "string",
					},
				},
				"required": []string{"city"},
			},
		},
	}
}

// Execute 模拟天气查询工具执行：返回硬编码的天气结果。
// 在生产 Registry 中，此方法会根据 call.Name 分发到具体的工具实现。
func (m *mockRegistry) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
	log.Printf("  -> [Mock 工具执行] 正在查询 %s 的天气...\n", call.Name)
	return schema.ToolResult{
		ToolCallID: call.ID,
		Output:     "API 返回：今天天气晴，最低温度 14 度，最高温度 25 度，体感舒适。",
		IsError:    false,
	}
}

// NewMockRegistry 构造并返回一个新的 mockRegistry 实例。
func NewMockRegistry() Registry {
	return &mockRegistry{}
}
