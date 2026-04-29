package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/harness9/internal/schema"
)

// AnthropicProvider 是 LLMProvider 的 Anthropic Claude 实现，支持所有遵循 Anthropic Messages API
// 规范的后端（包括 Anthropic 官方、OpenRouter 等 Anthropic 兼容端点）。
//
// 通过 ANTHROPIC_API_KEY 和 ANTHROPIC_BASE_URL 环境变量配置认证和端点，
// 使同一实现可灵活对接不同的 Anthropic 兼容服务。
//
// 注意：Anthropic Messages API 与 OpenAI Chat Completion API 的关键差异：
//   - System Prompt 不在 messages 数组中，而是作为独立的 system 参数传入
//   - 响应使用 Content Blocks（text / tool_use）而非单一的 content + tool_calls 结构
//   - 必须指定 maxTokens 参数（OpenAI 可省略）
//
// 内部架构采用统一的消息转换层：Generate 和 GenerateStream 共享同一套 convertMessages /
// convertTools 转换逻辑，仅在底层 SDK 调用方式上有所不同：
//   - Generate 使用 client.Messages.New()（阻塞式）
//   - GenerateStream 使用 client.Messages.NewStreaming()（流式）
type AnthropicProvider struct {
	// client Anthropic SDK 客户端，封装了 HTTP 通信、认证和重试逻辑。
	client anthropic.Client
	// model 模型标识符，如 "claude-sonnet-4-20250514"。
	model string
	// maxTokens 单次响应的最大输出 Token 数。Anthropic API 要求必须指定此参数。
	maxTokens int64
}

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

// Generate 实现 LLMProvider 接口的阻塞式调用。
// 通过共享的 convertMessages / convertTools 完成类型转换后，
// 调用 Anthropic SDK 的 Messages API 获取完整响应。
func (p *AnthropicProvider) Generate(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, error) {
	anthropicMsgs, systemPrompt, err := p.convertMessages(msgs)
	if err != nil {
		return nil, err
	}
	anthropicTools, err := p.convertTools(availableTools)
	if err != nil {
		return nil, err
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

	return p.extractMessage(resp.Content), nil
}

// GenerateStream 实现 LLMProvider 接口的流式调用。
// 使用 Anthropic SDK 的 NewStreaming API，将 MessageStreamEventUnion 逐个读取并转换为
// 统一的 StreamChunk 通过 channel 输出。
//
// Anthropic 流式事件类型映射：
//   - content_block_start (type=tool_use) → StreamChunkToolCallStart（含 ID、Name）
//   - content_block_delta (type=text_delta) → StreamChunkTextDelta（文本增量）
//   - content_block_delta (type=input_json_delta) → StreamChunkToolCallDelta（参数增量）
//
// 工具调用参数使用 anthropicToolCallAccumulator 按 Index 累积，
// Anthropic SDK 通过 input_json_delta 的 PartialJSON 字段逐片段传输参数。
func (p *AnthropicProvider) GenerateStream(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition) (<-chan schema.StreamChunk, error) {
	anthropicMsgs, systemPrompt, err := p.convertMessages(msgs)
	if err != nil {
		return nil, err
	}
	anthropicTools, err := p.convertTools(availableTools)
	if err != nil {
		return nil, err
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

	stream := p.client.Messages.NewStreaming(ctx, params)

	ch := make(chan schema.StreamChunk)
	go func() {
		defer close(ch)

		var contentBuf strings.Builder
		toolAccs := make(map[int]*anthropicToolCallAccumulator)

		for stream.Next() {
			event := stream.Current()

			switch event.Type {
			case "content_block_start":
				cb := event.AsContentBlockStart()
				if cb.ContentBlock.Type == "tool_use" {
					idx := int(cb.Index)
					toolAccs[idx] = &anthropicToolCallAccumulator{
						index: idx,
						id:    cb.ContentBlock.ID,
						name:  cb.ContentBlock.Name,
					}
					if !sendStreamChunk(ctx, ch, schema.StreamChunk{
						Type: schema.StreamChunkToolCallStart,
						ToolCall: &schema.ToolCallDelta{
							Index: idx,
							ID:    cb.ContentBlock.ID,
							Name:  cb.ContentBlock.Name,
						},
					}) {
						return
					}
				}

			case "content_block_delta":
				delta := event.AsContentBlockDelta()
				switch delta.Delta.Type {
				case "text_delta":
					td := delta.Delta.AsTextDelta()
					contentBuf.WriteString(td.Text)
					if !sendStreamChunk(ctx, ch, schema.StreamChunk{
						Type:  schema.StreamChunkTextDelta,
						Delta: td.Text,
					}) {
						return
					}
				case "input_json_delta":
					ijd := delta.Delta.AsInputJSONDelta()
					idx := int(delta.Index)
					if acc, ok := toolAccs[idx]; ok {
						acc.args.WriteString(ijd.PartialJSON)
					}
					if !sendStreamChunk(ctx, ch, schema.StreamChunk{
						Type: schema.StreamChunkToolCallDelta,
						ToolCall: &schema.ToolCallDelta{
							Index:     idx,
							Arguments: json.RawMessage(ijd.PartialJSON),
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
				Error: fmt.Sprintf("Anthropic 流式错误: %v", err),
			})
			return
		}

		msg := &schema.Message{
			Role:    schema.RoleAssistant,
			Content: contentBuf.String(),
		}
		// 按 Index 升序收集工具调用。
		// 旧实现使用 `for i := 0; i < len(toolAccs); i++` 遍历 map，
		// 当 SDK 传回的 index 有跳号（例如 0 和 2，缺 1）时会漏掉 index 2。
		// 改成显式按 keys 排序后迭代，对任意 index 分布都正确。
		indices := make([]int, 0, len(toolAccs))
		for idx := range toolAccs {
			indices = append(indices, idx)
		}
		sort.Ints(indices)
		for _, idx := range indices {
			acc := toolAccs[idx]
			msg.ToolCalls = append(msg.ToolCalls, schema.ToolCall{
				ID:        acc.id,
				Name:      acc.name,
				Arguments: []byte(acc.args.String()),
			})
		}

		sendStreamChunk(ctx, ch, schema.StreamChunk{
			Type:    schema.StreamChunkDone,
			Message: msg,
		})
	}()

	return ch, nil
}

// anthropicToolCallAccumulator 累积 Anthropic 流式响应中单个工具调用的完整信息。
// Anthropic SDK 在流式模式下，工具调用的传输分为：
//   - content_block_start: 包含 Index、ID、Name（在 StreamChunkToolCallStart 中发送）
//   - input_json_delta: 包含 PartialJSON 片段（在 StreamChunkToolCallDelta 中发送）
//
// 使用 Index 作为 key 存储在 map 中，支持多个并行工具调用的独立累积。
type anthropicToolCallAccumulator struct {
	index int
	id    string
	name  string
	args  strings.Builder
}

// convertMessages 将内部 schema.Message 转换为 Anthropic SDK 的消息参数格式。
// Generate 和 GenerateStream 共享此方法。
//
// 转换规则：
//   - schema.RoleSystem    → 提取为独立的 systemPrompt 返回值（Anthropic API 要求）
//   - schema.RoleUser      → anthropic.NewUserMessage（TextBlock）
//   - schema.RoleAssistant → anthropic.NewAssistantMessage（含 TextBlock 和 ToolUseBlock）
//   - schema.RoleTool      → anthropic.NewUserMessage（ToolResultBlock，携带 is_error）
//
// 关键修复：RoleTool 的 Message.IsError 会被透传到 tool_result.is_error，
// 使 Claude 能够明确感知"这次工具调用失败了"，触发更精准的自愈（Self-Healing）
// 重试，而不是把错误文本当成正常输出解读。
//
// 向后兼容：RoleUser + ToolCallID != "" 仍被识别为 Tool Observation，
// 便于从旧代码平滑迁移；新代码应显式使用 RoleTool 并设置 IsError。
//
// 返回 (anthropicMsgs, systemPrompt, error)，systemPrompt 为空表示无系统提示词。
func (p *AnthropicProvider) convertMessages(msgs []schema.Message) ([]anthropic.MessageParam, string, error) {
	var anthropicMsgs []anthropic.MessageParam
	var systemPrompt string

	for _, msg := range msgs {
		switch msg.Role {
		case schema.RoleSystem:
			systemPrompt = msg.Content
		case schema.RoleTool:
			// 核心修复：IsError 原样透传给 Anthropic，让 Claude 准确感知工具失败。
			anthropicMsgs = append(anthropicMsgs, anthropic.NewUserMessage(
				anthropic.NewToolResultBlock(msg.ToolCallID, msg.Content, msg.IsError),
			))
		case schema.RoleUser:
			// 向后兼容路径：旧代码可能把 Observation 放在 RoleUser + ToolCallID 上。
			// 此分支无 IsError 信息，默认当成非错误处理，保持旧行为不变。
			if msg.ToolCallID != "" {
				anthropicMsgs = append(anthropicMsgs, anthropic.NewUserMessage(
					anthropic.NewToolResultBlock(msg.ToolCallID, msg.Content, msg.IsError),
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
					return nil, "", fmt.Errorf("unmarshal tool call %q arguments: %w", tc.Name, err)
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

	return anthropicMsgs, systemPrompt, nil
}

// convertTools 将内部 schema.ToolDefinition 转换为 Anthropic SDK 的工具参数格式。
// 通过 extractSchemaFields 提取 properties 和 required 字段。
// 返回 nil 表示无工具可用（用于 Phase 1 Thinking 的 nil tools 场景）。
func (p *AnthropicProvider) convertTools(availableTools []schema.ToolDefinition) ([]anthropic.ToolUnionParam, error) {
	if len(availableTools) == 0 {
		return nil, nil
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
	return anthropicTools, nil
}

// extractMessage 从 Anthropic SDK 的 ContentBlockUnion 切片中提取 schema.Message。
// 遍历所有 Content Block，合并 text 类型的文本和 tool_use 类型的工具调用。
func (p *AnthropicProvider) extractMessage(content []anthropic.ContentBlockUnion) *schema.Message {
	resultMsg := &schema.Message{
		Role: schema.RoleAssistant,
	}

	for _, block := range content {
		switch block.Type {
		case "text":
			resultMsg.Content += block.Text
		case "tool_use":
			argsBytes, err := json.Marshal(block.Input)
			if err != nil {
				continue
			}
			resultMsg.ToolCalls = append(resultMsg.ToolCalls, schema.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: argsBytes,
			})
		}
	}

	return resultMsg
}

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
