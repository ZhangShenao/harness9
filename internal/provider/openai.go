package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

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
//
// 内部架构采用统一的消息转换层：Generate 和 GenerateStream 共享同一套 convertMessages /
// convertTools 转换逻辑，仅在底层 SDK 调用方式上有所不同：
//   - Generate 使用 client.Chat.Completions.New()（阻塞式）
//   - GenerateStream 使用 client.Chat.Completions.NewStreaming()（流式）
type OpenAIProvider struct {
	// client OpenAI SDK 客户端，封装了 HTTP 通信、认证和重试逻辑。
	client openai.Client
	// model 模型标识符，如 "gpt-4o"、"openai/gpt-5.4-mini" 等，直接传递给 Chat Completion API。
	model string
}

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

// Generate 实现 LLMProvider 接口的阻塞式调用。
// 通过共享的 convertMessages / convertTools 完成类型转换后，
// 调用 OpenAI SDK 的 Chat Completions API 获取完整响应。
func (p *OpenAIProvider) Generate(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, error) {
	openaiMsgs := p.convertMessages(msgs)
	openaiTools, err := p.convertTools(availableTools)
	if err != nil {
		return nil, err
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

	return p.extractMessage(resp.Choices[0].Message), nil
}

// GenerateStream 实现 LLMProvider 接口的流式调用。
// 使用 OpenAI SDK 的 NewStreaming API，将 ChatCompletionChunk 逐个读取并转换为
// 统一的 StreamChunk 通过 channel 输出。
//
// 流式处理流程：
//  1. 调用 convertMessages / convertTools 构建 SDK 请求参数
//  2. 通过 client.Chat.Completions.NewStreaming() 创建流式连接
//  3. 在独立 goroutine 中迭代 stream.Next()，处理每个 chunk：
//     - delta.Content → StreamChunkTextDelta（逐 token 文本）
//     - delta.ToolCalls[].ID != "" → StreamChunkToolCallStart（新工具调用）
//     - delta.ToolCalls[].Function.Arguments → StreamChunkToolCallDelta（参数增量）
//  4. 使用 openaiToolCallAccumulator 按 Index 累积工具调用的完整参数
//  5. 流结束后发送 StreamChunkDone（含累积完成的完整 Message）
//
// 所有 channel 发送都通过 sendStreamChunk 进行，支持 context 取消感知。
func (p *OpenAIProvider) GenerateStream(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition) (<-chan schema.StreamChunk, error) {
	openaiMsgs := p.convertMessages(msgs)
	openaiTools, err := p.convertTools(availableTools)
	if err != nil {
		return nil, err
	}

	reqParams := openai.ChatCompletionNewParams{
		Model:    p.model,
		Messages: openaiMsgs,
	}
	if len(openaiTools) > 0 {
		reqParams.Tools = openaiTools
	}

	stream := p.client.Chat.Completions.NewStreaming(ctx, reqParams)

	ch := make(chan schema.StreamChunk)
	go func() {
		defer close(ch)

		var contentBuf strings.Builder
		toolAccs := make(map[int]*openaiToolCallAccumulator)

		for stream.Next() {
			chunk := stream.Current()
			if len(chunk.Choices) == 0 {
				continue
			}

			delta := chunk.Choices[0].Delta

			if delta.Content != "" {
				contentBuf.WriteString(delta.Content)
				if !sendStreamChunk(ctx, ch, schema.StreamChunk{
					Type:  schema.StreamChunkTextDelta,
					Delta: delta.Content,
				}) {
					return
				}
			}

			for _, tc := range delta.ToolCalls {
				idx := int(tc.Index)
				acc := toolAccs[idx]
				if acc == nil {
					acc = &openaiToolCallAccumulator{index: idx}
					toolAccs[idx] = acc
				}
				if tc.ID != "" {
					acc.id = tc.ID
					acc.name = tc.Function.Name
					if !sendStreamChunk(ctx, ch, schema.StreamChunk{
						Type: schema.StreamChunkToolCallStart,
						ToolCall: &schema.ToolCallDelta{
							Index: idx,
							ID:    tc.ID,
							Name:  tc.Function.Name,
						},
					}) {
						return
					}
				}
				if tc.Function.Arguments != "" {
					acc.args.WriteString(tc.Function.Arguments)
					if !sendStreamChunk(ctx, ch, schema.StreamChunk{
						Type: schema.StreamChunkToolCallDelta,
						ToolCall: &schema.ToolCallDelta{
							Index:     idx,
							Arguments: json.RawMessage(tc.Function.Arguments),
						},
					}) {
						return
					}
				}
			}
		}

		if err := stream.Err(); err != nil {
			sendStreamChunk(ctx, ch, schema.StreamChunk{
				Type:  schema.StreamChunkError,
				Error: fmt.Sprintf("OpenAI 流式错误: %v", err),
			})
			return
		}

		msg := &schema.Message{
			Role:    schema.RoleAssistant,
			Content: contentBuf.String(),
		}
		for i := 0; i < len(toolAccs); i++ {
			if acc, ok := toolAccs[i]; ok {
				msg.ToolCalls = append(msg.ToolCalls, schema.ToolCall{
					ID:        acc.id,
					Name:      acc.name,
					Arguments: []byte(acc.args.String()),
				})
			}
		}

		sendStreamChunk(ctx, ch, schema.StreamChunk{
			Type:    schema.StreamChunkDone,
			Message: msg,
		})
	}()

	return ch, nil
}

// openaiToolCallAccumulator 累积 OpenAI 流式响应中单个工具调用的完整信息。
// OpenAI SDK 在流式模式下，每个工具调用的 ID/Name 和 Arguments 分多次 chunk 传输：
//   - 首个 chunk 包含 ID 和 Name（在 tool_call_start 中发送）
//   - 后续 chunk 包含 Arguments 的部分 JSON 文本（在 tool_call_delta 中发送）
//
// 使用 Index 作为 key 存储在 map 中，支持多个并行工具调用的独立累积。
type openaiToolCallAccumulator struct {
	index int
	id    string
	name  string
	args  strings.Builder
}

// convertMessages 将内部 schema.Message 转换为 OpenAI SDK 的消息参数格式。
// Generate 和 GenerateStream 共享此方法，避免重复的转换逻辑。
//
// 转换规则：
//   - schema.RoleSystem   → openai.SystemMessage
//   - schema.RoleUser     → openai.UserMessage 或 openai.ToolMessage（带 ToolCallID 时）
//   - schema.RoleAssistant → openai.ChatCompletionAssistantMessageParam（含 ToolCalls）
func (p *OpenAIProvider) convertMessages(msgs []schema.Message) []openai.ChatCompletionMessageParamUnion {
	var result []openai.ChatCompletionMessageParamUnion

	for _, msg := range msgs {
		switch msg.Role {
		case schema.RoleSystem:
			result = append(result, openai.SystemMessage(msg.Content))

		case schema.RoleUser:
			if msg.ToolCallID != "" {
				result = append(result, openai.ToolMessage(msg.Content, msg.ToolCallID))
			} else {
				result = append(result, openai.UserMessage(msg.Content))
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

			result = append(result, openai.ChatCompletionMessageParamUnion{
				OfAssistant: &astParam,
			})
		}
	}

	return result
}

// convertTools 将内部 schema.ToolDefinition 转换为 OpenAI SDK 的工具参数格式。
// 返回 nil 表示无工具可用（用于 Phase 1 Thinking 的 nil tools 场景）。
func (p *OpenAIProvider) convertTools(availableTools []schema.ToolDefinition) ([]openai.ChatCompletionToolUnionParam, error) {
	if len(availableTools) == 0 {
		return nil, nil
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
	return openaiTools, nil
}

// extractMessage 从 OpenAI SDK 的 ChatCompletionMessage 中提取 schema.Message。
// 过滤非 function 类型的 ToolCalls，统一转换为 schema.ToolCall。
func (p *OpenAIProvider) extractMessage(choice openai.ChatCompletionMessage) *schema.Message {
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

	return resultMsg
}

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


