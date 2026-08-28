// Package provider — Anthropic Messages API 适配器。
// 本文件实现基于 Anthropic Messages API 的 LLMProvider，支持 extended thinking（WithThinkingBudget）。
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	// thinkingBudget 启用 extended thinking 时的最大推理 token 预算。0 表示禁用。
	thinkingBudget int64
}

// AnthropicOption 是 AnthropicProvider 的功能选项类型。
type AnthropicOption func(*AnthropicProvider)

// WithThinkingBudget 启用 Anthropic extended thinking，并设置推理 token 预算上限。
// budget 为 0 时禁用 extended thinking。
// Anthropic API 要求 budget_tokens ≥ 1024，小于该值时自动提升到 1024。
func WithThinkingBudget(budget int64) AnthropicOption {
	return func(p *AnthropicProvider) {
		if budget > 0 && budget < 1024 {
			budget = 1024
		}
		p.thinkingBudget = budget
	}
}

// NewAnthropicProvider 创建并返回 AnthropicProvider 实例。
// 从环境变量读取 ANTHROPIC_API_KEY 和 ANTHROPIC_BASE_URL 完成认证配置；
// maxTokens <= 0 时自动设为 4096（Anthropic API 要求必须指定此参数）。
func NewAnthropicProvider(model string, maxTokens int64, opts ...AnthropicOption) (*AnthropicProvider, error) {
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
	p := &AnthropicProvider{
		client: anthropic.NewClient(
			option.WithAPIKey(apiKey),
			option.WithBaseURL(baseURL),
			option.WithMaxRetries(providerMaxRetries()),
			option.WithRequestTimeout(providerRequestTimeout()),
		),
		model:     model,
		maxTokens: maxTokens,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// Generate 实现 LLMProvider 接口的阻塞式调用。
// 通过共享的 convertMessages / convertTools 完成类型转换后，
// 调用 Anthropic SDK 的 Messages API 获取完整响应，并提取实际 token 用量。
func (p *AnthropicProvider) Generate(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
	anthropicMsgs, systemPrompt, err := p.convertMessages(msgs)
	if err != nil {
		return nil, nil, err
	}
	anthropicTools, err := p.convertTools(availableTools)
	if err != nil {
		return nil, nil, err
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

	if p.thinkingBudget > 0 {
		params.Thinking = anthropic.ThinkingConfigParamOfEnabled(p.thinkingBudget)
	}

	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return nil, nil, fmt.Errorf("Anthropic 兼容 API 请求失败：%w", err)
	}

	usage := &schema.Usage{
		InputTokens:  int(resp.Usage.InputTokens),
		OutputTokens: int(resp.Usage.OutputTokens),
	}
	return p.extractMessage(resp.Content), usage, nil
}

// GenerateStream 实现 LLMProvider 接口的流式调用。
// 在独立 goroutine 中迭代 SDK 流，文本增量发送 StreamChunkTextDelta，
// 工具调用通过 accumulator 累积后随 StreamChunkDone 一并输出。
// 从 message_start 事件提取实际 InputTokens，随 StreamChunkDone 发出。
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

	if p.thinkingBudget > 0 {
		params.Thinking = anthropic.ThinkingConfigParamOfEnabled(p.thinkingBudget)
	}

	stream := p.client.Messages.NewStreaming(ctx, params)

	ch := make(chan schema.StreamChunk)
	go func() {
		defer close(ch)

		var contentBuf strings.Builder
		toolAccs := newToolCallAccumulators()
		var actualUsage *schema.Usage

		for stream.Next() {
			event := stream.Current()

			switch event.Type {
			case "message_start":
				// message_start 携带本次请求的实际 InputTokens（在响应开始时即可获得）。
				// OutputTokens 此时仍为 0——输出 token 只在 message_delta 事件（流末尾）才被填充。
				ms := event.AsMessageStart()
				actualUsage = applyStreamUsage(actualUsage, ms.Message.Usage.InputTokens, ms.Message.Usage.OutputTokens)

			case "message_delta":
				// message_delta 在流末尾（所有 content 增量之后）上报累计的 OutputTokens。
				// 不处理此事件会导致流式场景下 actualUsage.OutputTokens 恒为 0，
				// 使 TUI 的 token 用量展示与实际不符。
				md := event.AsMessageDelta()
				actualUsage = applyStreamUsage(actualUsage, md.Usage.InputTokens, md.Usage.OutputTokens)

			case "content_block_start":
				cb := event.AsContentBlockStart()
				switch cb.ContentBlock.Type {
				case "tool_use":
					toolAccs.start(int(cb.Index), cb.ContentBlock.ID, cb.ContentBlock.Name)
				case "thinking", "redacted_thinking":
					// thinking 内容通过后续 thinking_delta 事件增量到达，此处无需初始化。
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
				case "thinking_delta":
					td := delta.Delta.AsThinkingDelta()
					if !sendStreamChunk(ctx, ch, schema.StreamChunk{
						Type:  schema.StreamChunkThinkingDelta,
						Delta: td.Thinking,
					}) {
						return
					}
				case "input_json_delta":
					ijd := delta.Delta.AsInputJSONDelta()
					toolAccs.appendArgs(int(delta.Index), ijd.PartialJSON)
				}
			}
		}

		if err := stream.Err(); err != nil {
			// 流式错误：使用直接 channel 发送而非 sendStreamChunk，确保消费方一定能收到错误通知。
			// goroutine 已运行到流末尾，若 context 已取消则消费方早已停止读取，发送无意义。
			// 此处 select 避免 goroutine 在已取消 context 下永久阻塞在 channel 发送上。
			select {
			case <-ctx.Done():
			case ch <- schema.StreamChunk{
				Type:  schema.StreamChunkError,
				Error: fmt.Sprintf("Anthropic 流式错误: %v", err),
			}:
			}
			return
		}

		msg := &schema.Message{
			Role:      schema.RoleAssistant,
			Content:   contentBuf.String(),
			ToolCalls: toolAccs.finalize(),
		}

		// Done chunk：同样使用 select 避免 context 取消时阻塞。
		select {
		case <-ctx.Done():
		case ch <- schema.StreamChunk{
			Type:    schema.StreamChunkDone,
			Message: msg,
			Usage:   actualUsage,
		}:
		}
	}()

	return ch, nil
}

// convertMessages 将内部 schema.Message 转换为 Anthropic SDK 的消息参数格式。
// Generate 和 GenerateStream 共享此方法。
//
// 转换规则：
//   - schema.RoleSystem   → 提取为独立的 systemPrompt 返回值（Anthropic API 要求）
//   - schema.RoleUser     → anthropic.NewUserMessage（含 ToolResultBlock 或 TextBlock）
//   - schema.RoleAssistant → anthropic.NewAssistantMessage（含 TextBlock 和 ToolUseBlock）
//
// 返回 (anthropicMsgs, systemPrompt, error)，systemPrompt 为空表示无系统提示词。
func (p *AnthropicProvider) convertMessages(msgs []schema.Message) ([]anthropic.MessageParam, string, error) {
	var anthropicMsgs []anthropic.MessageParam
	var systemPrompt string

	for _, msg := range msgs {
		switch msg.Role {
		case schema.RoleSystem:
			systemPrompt = msg.Content
		case schema.RoleUser:
			if msg.ToolCallID != "" {
				// 透传结构化错误标记：工具失败时 is_error=true，强化模型自愈，
				// 而非硬编码 false 仅靠文本前缀让模型猜测。
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

// applyStreamUsage 将单个流式事件携带的 token 用量合并进 actualUsage，返回更新后的用量。
//
// 覆盖 Anthropic 流式中的两种用法来源：
//   - message_start：携带 InputTokens（OutputTokens 此时恒为 0）
//   - message_delta：流末尾上报累计 OutputTokens（InputTokens 字段为 0）
//
// 合并规则：仅当某字段 > 0 时才覆盖（message_delta 场景下保留 message_start
// 已记录的 InputTokens）。actualUsage 为 nil 时新建，供 StreamChunkDone 携带。
// 独立为纯函数便于单元测试，无需构造 SDK 流式事件对象。
func applyStreamUsage(actualUsage *schema.Usage, inputTokens, outputTokens int64) *schema.Usage {
	if actualUsage == nil {
		actualUsage = &schema.Usage{}
	}
	if inputTokens > 0 {
		actualUsage.InputTokens = int(inputTokens)
	}
	if outputTokens > 0 {
		actualUsage.OutputTokens = int(outputTokens)
	}
	return actualUsage
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

// extractSchemaFields 从 JSON Schema map 中提取 properties 和 required 字段。
// 同时兼容 []string 和 []interface{} 两种 required 数组类型，
// 后者在 JSON 反序列化为 interface{} 时常见。
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
					return nil, nil, fmt.Errorf("required 数组中包含非字符串元素：%T", item)
				}
				required = append(required, s)
			}
		}
	}

	return properties, required, nil
}
