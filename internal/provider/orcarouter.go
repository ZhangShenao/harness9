// Package provider — OrcaRouter 网关适配器。
//
// OrcaRouter（https://orcarouter.ai）是类似 OpenRouter 的 OpenAI 兼容模型路由网关：
// 一个 API Key 即可访问多家上游模型（OpenAI / Anthropic / Google / DeepSeek / Qwen 等），
// 模型名沿用 provider 前缀约定（如 "openai/gpt-4o-mini"），与 GetModelLimits 的
// 前缀剥离逻辑天然兼容。因此本适配器直接复用 OpenAIProvider（消息转换、流式、
// 工具调用与重试逻辑完全共享），仅在认证与端点配置上读取 ORCAROUTER_* 专属环境变量。
package provider

import (
	"fmt"
	"os"
	"strings"
)

// DefaultOrcaRouterBaseURL 是 OrcaRouter 官方网关的 OpenAI 兼容端点地址。
// 与 OpenAI 官方约定一致，Base URL 需带 /v1 后缀（SDK 在其后拼接 /chat/completions）。
const DefaultOrcaRouterBaseURL = "https://api.orcarouter.ai/v1"

// NewOrcaRouterProvider 创建指向 OrcaRouter 网关的 OpenAI 兼容 Provider。
//
// 环境变量：
//   - ORCAROUTER_API_KEY   必填，网关 API Key（sk-orca- 前缀）
//   - ORCAROUTER_BASE_URL  可选，默认 DefaultOrcaRouterBaseURL（自建/代理网关时覆盖）
//
// 推理内容说明：OrcaRouter 无需 include_reasoning 标志，即会把上游推理内容放进标准的
// reasoning / reasoning_content delta 字段（由 extractReasoningContent 统一提取并路由为
// ThinkingDelta），因此不加入 baseURLEnablesReasoning 白名单，保持请求体最小化，
// 避免严格校验未知参数的上游因多余字段拒绝请求。
func NewOrcaRouterProvider(model string, opts ...OpenAIOption) (*OpenAIProvider, error) {
	apiKey := os.Getenv("ORCAROUTER_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("请设置 ORCAROUTER_API_KEY 环境变量")
	}
	baseURL := os.Getenv("ORCAROUTER_BASE_URL")
	if baseURL == "" {
		baseURL = DefaultOrcaRouterBaseURL
	}
	return newOpenAICompatProvider(apiKey, baseURL, model, opts...)
}

// NewFromEnv 依据环境变量选择并构建 OpenAI 兼容 Provider，是 main 入口、
// 子代理 ProviderFor 与 SWE-bench runner 的统一装配点。
//
// 选择规则（显式优先，自动探测兜底）：
//   - LLM_PROVIDER=orcarouter 强制走 OrcaRouter；LLM_PROVIDER=openai 强制走 OPENAI_* 端点
//   - LLM_PROVIDER 未设置时自动探测：仅配置 ORCAROUTER_API_KEY（无 OPENAI_API_KEY）时
//     走 OrcaRouter，其余情况沿用 OPENAI_* 端点，保证既有用户行为零变化
//   - 两个 Key 同时配置且未显式指定 LLM_PROVIDER 时按 OPENAI_* 处理，避免静默切换网关
func NewFromEnv(model string, opts ...OpenAIOption) (LLMProvider, error) {
	explicit := strings.ToLower(strings.TrimSpace(os.Getenv("LLM_PROVIDER")))
	switch explicit {
	case "orcarouter":
		return NewOrcaRouterProvider(model, opts...)
	case "openai":
		return NewOpenAIProvider(model, opts...)
	case "":
		if os.Getenv("ORCAROUTER_API_KEY") != "" && os.Getenv("OPENAI_API_KEY") == "" {
			return NewOrcaRouterProvider(model, opts...)
		}
		return NewOpenAIProvider(model, opts...)
	default:
		return nil, fmt.Errorf("未知 LLM_PROVIDER %q：仅支持 openai 或 orcarouter", explicit)
	}
}
