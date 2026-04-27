package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/harness9/internal/schema"
)

// AnthropicProvider 是 LLMProvider 的 Anthropic Claude 实现，支持所有遵循 Anthropic Messages API
// 规范的后端（包括 Anthropic 官方、OpenRouter 等 Anthropic 兼容端点）。
//
// 通过 ANTHROPIC_API_KEY 和 ANTHROPIC_BASE_URL 环境变量配置认证和端点，
// 使同一实现可灵活对接不同的 Anthropic 兼容服务。
type AnthropicProvider struct {
	client    anthropic.Client
	model     string
	maxTokens int64
}

// NewAnthropicProvider 创建 Anthropic 兼容的 Provider 实例。
//
// 参数:
//   - model: 模型标识符（如 "claude-sonnet-4-20250514"、"anthropic/claude-sonnet-4.6" 等）
//   - maxTokens: 单次响应的最大 token 数（建议 4096+）
//
// 环境变量:
//   - ANTHROPIC_API_KEY: API 认证密钥，缺失时返回错误
//   - ANTHROPIC_BASE_URL: API 端点基址，缺失时返回错误
func NewAnthropicProvider(model string, maxTokens int64) (*AnthropicProvider, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("请设置 ANTHROPIC_API_KEY 环境变量")
	}
	baseURL := os.Getenv("ANTHROPIC_BASE_URL")
	if baseURL == "" {
		return nil, fmt.Errorf("请设置 ANTHROPIC_BASE_URL 环境变量")
	}
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	return &AnthropicProvider{
		client:    anthropic.NewClient(option.WithAPIKey(apiKey), option.WithBaseURL(baseURL)),
		model:     model,
		maxTokens: maxTokens,
	}, nil
}

// Generate 实现 LLMProvider 接口，将内部 schema.Message 转换为 Anthropic SDK 的参数格式，
// 调用 Messages API 并将响应映射回 schema.Message。
//
// 转换规则：
//   - schema.RoleSystem    → params.System (Anthropic 的 system prompt 不在 messages 数组中)
//   - schema.RoleUser      → anthropic.NewUserMessage (含 ToolResultBlock 或 TextBlock)
//   - schema.RoleAssistant → anthropic.NewAssistantMessage (含 TextBlock 和 ToolUseBlock)
//   - schema.ToolDefinition → anthropic.ToolParam
//
// 注意：Anthropic Messages API 要求 system prompt 作为独立参数传入，而非 messages 数组的一部分。
func (p *AnthropicProvider) Generate(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, error) {
	var anthropicMsgs []anthropic.MessageParam
	var systemPrompt string

	for _, msg := range msgs {
		switch msg.Role {
		case schema.RoleSystem:
			systemPrompt = msg.Content
		case schema.RoleUser:
			if msg.ToolCallID != "" {
				anthropicMsgs = append(anthropicMsgs, anthropic.NewUserMessage(
					anthropic.NewToolResultBlock(msg.ToolCallID, msg.Content, false),
				))
			} else {
				anthropicMsgs = append(anthropicMsgs, anthropic.NewUserMessage(
					anthropic.NewTextBlock(msg.Content),
				))
			}
		case schema.RoleAssistant:
			var blocks []anthropic.ContentBlockParamUnion
			if msg.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(msg.Content))
			}
			for _, tc := range msg.ToolCalls {
				var inputMap map[string]interface{}
				if err := json.Unmarshal(tc.Arguments, &inputMap); err != nil {
					return nil, fmt.Errorf("unmarshal tool call %q arguments: %w", tc.Name, err)
				}
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{
						ID:    tc.ID,
						Name:  tc.Name,
						Input: inputMap,
					},
				})
			}
			if len(blocks) > 0 {
				anthropicMsgs = append(anthropicMsgs, anthropic.NewAssistantMessage(blocks...))
			}
		}
	}

	var anthropicTools []anthropic.ToolUnionParam
	for _, toolDef := range availableTools {
		properties, required, err := extractSchemaFields(toolDef.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("convert tool %q input schema: %w", toolDef.Name, err)
		}

		tp := anthropic.ToolParam{
			Name:        toolDef.Name,
			Description: anthropic.String(toolDef.Description),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: properties,
				Required:   required,
			},
		}
		anthropicTools = append(anthropicTools, anthropic.ToolUnionParam{OfTool: &tp})
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: p.maxTokens,
		Messages:  anthropicMsgs,
	}

	if systemPrompt != "" {
		params.System = []anthropic.TextBlockParam{
			{Text: systemPrompt},
		}
	}

	if len(anthropicTools) > 0 {
		params.Tools = anthropicTools
	}

	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("Anthropic 兼容 API 请求失败: %w", err)
	}

	resultMsg := &schema.Message{
		Role: schema.RoleAssistant,
	}

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			resultMsg.Content += block.Text
		case "tool_use":
			argsBytes, err := json.Marshal(block.Input)
			if err != nil {
				return nil, fmt.Errorf("marshal tool_use %q input: %w", block.Name, err)
			}
			resultMsg.ToolCalls = append(resultMsg.ToolCalls, schema.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: argsBytes,
			})
		}
	}

	return resultMsg, nil
}

// extractSchemaFields 从 interface{} 类型的 InputSchema 中提取 properties 和 required 字段。
// 安全处理 []interface{} 到 []string 的类型转换（JSON 反序列化后 required 的实际类型）。
func extractSchemaFields(input interface{}) (map[string]any, []string, error) {
	m, ok := input.(map[string]interface{})
	if !ok {
		return nil, nil, fmt.Errorf("input schema 期望 map[string]interface{}，实际类型 %T", input)
	}

	var properties map[string]any
	if p, ok := m["properties"].(map[string]interface{}); ok {
		properties = p
	}

	var required []string
	if r, ok := m["required"]; ok {
		switch v := r.(type) {
		case []string:
			required = v
		case []interface{}:
			for _, item := range v {
				s, ok := item.(string)
				if !ok {
					return nil, nil, fmt.Errorf("required 数组中包含非字符串元素: %T", item)
				}
				required = append(required, s)
			}
		}
	}

	return properties, required, nil
}
