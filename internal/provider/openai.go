// Package provider — OpenAI 兼容 API 适配器。
// 本文件实现基于 OpenAI Chat Completion API 的 LLMProvider，支持 OpenAI 官方、Azure、OpenRouter 等兼容端点。
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/harness9/internal/schema"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// providerEnvInt 读取整型环境变量，缺省/非法时返回 def。
func providerEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

// providerRequestTimeout 返回单次 LLM 请求的超时（默认 600s）。
// 用于防止"连接已建立但服务端中途挂起、字节不再流动"的请求吞掉整个实例预算；
// 取值需远小于实例级超时、又足够长以容纳一次完整生成。env: LLM_REQUEST_TIMEOUT_SECS。
func providerRequestTimeout() time.Duration {
	return time.Duration(providerEnvInt("LLM_REQUEST_TIMEOUT_SECS", 600)) * time.Second
}

// providerMaxRetries 返回 SDK 内置重试次数（默认 5，高于 SDK 默认 2）。
// 覆盖批量评测下常见的 429 限流；SDK 会自动遵循 Retry-After。env: LLM_MAX_RETRIES。
// 注意：SDK 重试仅覆盖首字节到达前的错误，流式中途断连由引擎层 WithGenerateRetry 兜底。
func providerMaxRetries() int {
	return providerEnvInt("LLM_MAX_RETRIES", 5)
}

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
	// includeReasoning 为 true 时在请求体中注入 include_reasoning=true，
	// 用于 OpenRouter 等代理层将推理内容暴露在 delta.reasoning 字段中。
	includeReasoning bool
}

// OpenAIOption 是 OpenAIProvider 的功能选项类型。
type OpenAIOption func(*OpenAIProvider)

// WithIncludeReasoning 在每次请求中注入 include_reasoning=true，
// 使 OpenRouter 在流式响应的 delta.reasoning 字段中返回推理内容。
// 对不支持此参数的后端无副作用（参数被忽略）。
func WithIncludeReasoning() OpenAIOption {
	return func(p *OpenAIProvider) {
		p.includeReasoning = true
	}
}

// baseURLEnablesReasoning 判断给定的 base URL 是否为需要 include_reasoning=true
// 才会暴露推理内容的 OpenAI 兼容网关（OpenRouter、Requesty）。
// baseURLEnablesReasoning reports whether the base URL is an OpenAI-compatible
// gateway (OpenRouter, Requesty) that needs include_reasoning=true to surface
// reasoning content in delta.reasoning.
func baseURLEnablesReasoning(baseURL string) bool {
	return strings.Contains(baseURL, "openrouter") || strings.Contains(baseURL, "requesty")
}

// NewOpenAIProvider 创建并返回 OpenAIProvider 实例。
// 从环境变量读取 OPENAI_API_KEY 和 OPENAI_BASE_URL 完成认证配置。
// 当 OPENAI_BASE_URL 中包含 "openrouter" 时自动启用 includeReasoning，
// 使 OpenRouter 在流式响应中暴露推理内容（delta.reasoning 字段）。
func NewOpenAIProvider(model string, opts ...OpenAIOption) (*OpenAIProvider, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("请设置 OPENAI_API_KEY 环境变量")
	}
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		return nil, fmt.Errorf("请设置 OPENAI_BASE_URL 环境变量")
	}

	p := &OpenAIProvider{
		client: openai.NewClient(
			option.WithAPIKey(apiKey),
			option.WithBaseURL(baseURL),
			option.WithMaxRetries(providerMaxRetries()),
			option.WithRequestTimeout(providerRequestTimeout()),
		),
		model: model,
		// OpenRouter 和 Requesty 需要 include_reasoning=true 才会在 delta.reasoning 中返回推理内容。
		// 两者均为 OpenAI 兼容网关，使用相同的 reasoning 字段；其他后端不含此参数时默认忽略，不产生副作用。
		// OpenRouter and Requesty both expose reasoning via delta.reasoning when include_reasoning=true.
		includeReasoning: baseURLEnablesReasoning(baseURL),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// Generate 实现 LLMProvider 接口的阻塞式调用。
// 通过共享的 convertMessages / convertTools 完成类型转换后，
// 调用 OpenAI SDK 的 Chat Completions API 获取完整响应，并提取实际 token 用量。
func (p *OpenAIProvider) Generate(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
	openaiMsgs := p.convertMessages(msgs)
	openaiTools, err := p.convertTools(availableTools)
	if err != nil {
		return nil, nil, err
	}

	reqParams := openai.ChatCompletionNewParams{
		Model:    p.model,
		Messages: openaiMsgs,
	}
	if len(openaiTools) > 0 {
		reqParams.Tools = openaiTools
	}

	var reqOpts []option.RequestOption
	if p.includeReasoning {
		reqOpts = append(reqOpts, option.WithJSONSet("include_reasoning", true))
	}

	resp, err := p.client.Chat.Completions.New(ctx, reqParams, reqOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("OpenAI 兼容 API 请求失败：%w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, nil, fmt.Errorf("OpenAI 兼容 API 返回了空的 Choices")
	}

	usage := &schema.Usage{
		InputTokens:  int(resp.Usage.PromptTokens),
		OutputTokens: int(resp.Usage.CompletionTokens),
	}
	return p.extractMessage(resp.Choices[0].Message), usage, nil
}

// GenerateStream 实现 LLMProvider 接口的流式调用。
// 在独立 goroutine 中迭代 SDK 流，文本增量发送 StreamChunkTextDelta，
// 工具调用通过 accumulator 累积后随 StreamChunkDone 一并输出。
// 通过 StreamOptions.IncludeUsage 请求实际 token 用量，在末尾 chunk 中提取并随 Done 发出。
func (p *OpenAIProvider) GenerateStream(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition) (<-chan schema.StreamChunk, error) {
	openaiMsgs := p.convertMessages(msgs)
	openaiTools, err := p.convertTools(availableTools)
	if err != nil {
		return nil, err
	}

	reqParams := openai.ChatCompletionNewParams{
		Model:    p.model,
		Messages: openaiMsgs,
		// IncludeUsage 使 OpenAI 在流末尾的空 Choices chunk 中填充 Usage 字段。
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		},
	}
	if len(openaiTools) > 0 {
		reqParams.Tools = openaiTools
	}

	var streamOpts []option.RequestOption
	if p.includeReasoning {
		streamOpts = append(streamOpts, option.WithJSONSet("include_reasoning", true))
	}

	stream := p.client.Chat.Completions.NewStreaming(ctx, reqParams, streamOpts...)

	ch := make(chan schema.StreamChunk)
	go func() {
		defer close(ch)

		var contentBuf strings.Builder
		toolAccs := newToolCallAccumulators()
		var actualUsage *schema.Usage

		for stream.Next() {
			chunk := stream.Current()

			// 末尾 Usage chunk：Choices 为空，但 Usage 已填充（当 IncludeUsage=true 时）。
			if chunk.Usage.PromptTokens > 0 {
				actualUsage = &schema.Usage{
					InputTokens:  int(chunk.Usage.PromptTokens),
					OutputTokens: int(chunk.Usage.CompletionTokens),
				}
			}

			if len(chunk.Choices) == 0 {
				continue
			}

			// 提取 reasoning_content（DeepSeek-R1 等模型通过此字段暴露推理内容）。
			if rc := extractReasoningContent(chunk.RawJSON()); rc != "" {
				if !sendStreamChunk(ctx, ch, schema.StreamChunk{
					Type:  schema.StreamChunkThinkingDelta,
					Delta: rc,
				}) {
					return
				}
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
				if tc.ID != "" {
					toolAccs.start(idx, tc.ID, tc.Function.Name)
				}
				if tc.Function.Arguments != "" {
					toolAccs.appendArgs(idx, tc.Function.Arguments)
				}
			}
		}

		if err := stream.Err(); err != nil {
			// 流式错误：使用 select 避免 context 取消后 goroutine 永久阻塞在 channel 发送上。
			select {
			case <-ctx.Done():
			case ch <- schema.StreamChunk{
				Type:  schema.StreamChunkError,
				Error: fmt.Sprintf("OpenAI 流式错误: %v", err),
			}:
			}
			return
		}

		msg := &schema.Message{
			Role:      schema.RoleAssistant,
			Content:   contentBuf.String(),
			ToolCalls: toolAccs.finalize(),
		}

		// Done chunk：使用 select 避免 context 取消时阻塞。
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

// convertToFunctionParameters 将 JSON Schema interface{} 转换为 OpenAI SDK 的 FunctionParameters 类型。
// 优先路径：若 input 已是 map[string]interface{}，直接类型断言（无内存分配）。
// 回退路径：先 Marshal 再 Unmarshal，适用于其他实现了 JSON 序列化的 Schema 类型。
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

// extractReasoningContent 从 OpenAI 兼容流式 chunk 的原始 JSON 中提取推理内容。
//
// 两种字段格式均支持：
//   - choices.0.delta.reasoning_content — DeepSeek-R1 原生格式
//   - choices.0.delta.reasoning         — OpenRouter 代理 OpenAI gpt-5.x 等模型的格式
//
// 标准 OpenAI 响应（无推理内容）返回空字符串，不产生副作用。
func extractReasoningContent(rawJSON string) string {
	if rc := gjson.Get(rawJSON, "choices.0.delta.reasoning_content").String(); rc != "" {
		return rc
	}
	return gjson.Get(rawJSON, "choices.0.delta.reasoning").String()
}
