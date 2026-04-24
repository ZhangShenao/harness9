package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/harness9/internal/schema"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// OpenAIProvider 是 LLMProvider 的 OpenAI 兼容实现，支持所有遵循 OpenAI Chat Completion API
// 规范的后端（包括 OpenAI 官方、Azure OpenAI、OpenRouter 等兼容端点）。
//
// 通过 OPENAI_API_KEY 和 OPENAI_BASE_URL 环境变量配置认证和端点，
// 使同一实现可灵活对接不同的 OpenAI 兼容服务。
type OpenAIProvider struct {
	client openai.Client
	model  string
}

// NewOpenAIProvider 创建 OpenAI 兼容的 Provider 实例。
//
// 参数:
//   - model: 模型标识符（如 "gpt-4o"、"openai/gpt-5.4-mini" 等），直接传递给 API
//
// 环境变量:
//   - OPENAI_API_KEY: API 认证密钥，缺失时返回错误
//   - OPENAI_BASE_URL: API 端点基址（如 "https://api.openai.com/v1"），缺失时返回错误
func NewOpenAIProvider(model string) (*OpenAIProvider, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("请设置 OPENAI_API_KEY 环境变量")
	}
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		return nil, fmt.Errorf("请设置 OPENAI_BASE_URL 环境变量")
	}

	return &OpenAIProvider{
		client: openai.NewClient(option.WithAPIKey(apiKey), option.WithBaseURL(baseURL)),
		model:  model,
	}, nil
}

// Generate 实现 LLMProvider 接口，将内部 schema.Message 转换为 OpenAI SDK 的参数格式，
// 调用 Chat Completion API 并将响应映射回 schema.Message。
//
// 转换规则：
//   - schema.RoleSystem   → openai.SystemMessage
//   - schema.RoleUser     → openai.UserMessage 或 openai.ToolMessage（带 ToolCallID 时）
//   - schema.RoleAssistant → openai.ChatCompletionAssistantMessageParam（含 ToolCalls）
//   - schema.ToolDefinition → openai.ChatCompletionFunctionTool
//
// 返回的 Message 中，ToolCalls 在模型决定调用工具时填充；否则 Content 包含纯文本回复。
func (p *OpenAIProvider) Generate(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, error) {
	var openaiMsgs []openai.ChatCompletionMessageParamUnion

	for _, msg := range msgs {
		switch msg.Role {
		case schema.RoleSystem:
			openaiMsgs = append(openaiMsgs, openai.SystemMessage(msg.Content))

		case schema.RoleUser:
			if msg.ToolCallID != "" {
				openaiMsgs = append(openaiMsgs, openai.ToolMessage(msg.Content, msg.ToolCallID))
			} else {
				openaiMsgs = append(openaiMsgs, openai.UserMessage(msg.Content))
			}
		case schema.RoleAssistant:
			astParam := openai.ChatCompletionAssistantMessageParam{}

			if msg.Content != "" {
				astParam.Content = openai.ChatCompletionAssistantMessageParamContentUnion{
					OfString: openai.String(msg.Content),
				}
			}

			if len(msg.ToolCalls) > 0 {
				var toolCalls []openai.ChatCompletionMessageToolCallUnionParam
				for _, tc := range msg.ToolCalls {
					toolCalls = append(toolCalls, openai.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
							ID:   tc.ID,
							Type: "function",
							Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
								Name:      tc.Name,
								Arguments: string(tc.Arguments),
							},
						},
					})
				}
				astParam.ToolCalls = toolCalls
			}

			openaiMsgs = append(openaiMsgs, openai.ChatCompletionMessageParamUnion{
				OfAssistant: &astParam,
			})
		}
	}

	var openaiTools []openai.ChatCompletionToolUnionParam
	for _, toolDef := range availableTools {
		params, err := convertToFunctionParameters(toolDef.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("convert tool %q input schema: %w", toolDef.Name, err)
		}

		openaiTools = append(openaiTools, openai.ChatCompletionFunctionTool(
			shared.FunctionDefinitionParam{
				Name:        toolDef.Name,
				Description: openai.String(toolDef.Description),
				Parameters:  params,
			},
		))
	}

	reqParams := openai.ChatCompletionNewParams{
		Model:    p.model,
		Messages: openaiMsgs,
	}
	if len(openaiTools) > 0 {
		reqParams.Tools = openaiTools
	}

	resp, err := p.client.Chat.Completions.New(ctx, reqParams)
	if err != nil {
		return nil, fmt.Errorf("OpenAI 兼容 API 请求失败: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("OpenAI 兼容 API 返回了空的 Choices")
	}

	choice := resp.Choices[0].Message
	resultMsg := &schema.Message{
		Role:    schema.RoleAssistant,
		Content: choice.Content,
	}

	for _, tc := range choice.ToolCalls {
		if tc.Type == "function" {
			resultMsg.ToolCalls = append(resultMsg.ToolCalls, schema.ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: []byte(tc.Function.Arguments),
			})
		}
	}

	return resultMsg, nil
}

// convertToFunctionParameters 将 interface{} 类型的 InputSchema 转换为 OpenAI SDK 要求的
// shared.FunctionParameters。优先尝试直接类型断言，失败时通过 JSON 往返进行转换。
func convertToFunctionParameters(input interface{}) (shared.FunctionParameters, error) {
	if m, ok := input.(map[string]interface{}); ok {
		return shared.FunctionParameters(m), nil
	}
	b, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal input schema: %w", err)
	}
	var params shared.FunctionParameters
	if err := json.Unmarshal(b, &params); err != nil {
		return nil, fmt.Errorf("unmarshal input schema: %w", err)
	}
	return params, nil
}
