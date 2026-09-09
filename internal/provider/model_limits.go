// Package provider — model_limits：已知模型的上下文窗口注册表与限制查询。
// 本文件维护 knownModels 静态注册表，存储各主流模型的 ContextTokens 和 OutputTokens。
// GetModelLimits 自动剥离 provider 前缀（如 "openai/gpt-4o" → "gpt-4o"），
// 未知模型返回 256K 保守回退值（与 HermesAgent 策略一致）。
// 此注册表是静态的，新模型上线后需手动添加条目。最后更新：2026-05。
package provider

import "strings"

// ModelLimits 存储单个模型的上下文窗口和最大输出 token 数。
type ModelLimits struct {
	ContextTokens int
	OutputTokens  int
}

// knownModels 以裸模型名（不含 provider 前缀）为键，存储已知模型的上下文限制。
// 数据来源：OpenAI API 文档、Anthropic API 文档、各模型 provider 官网。
// 最后更新：2026-05。
var knownModels = map[string]ModelLimits{
	// Anthropic Claude 4.x
	"claude-opus-4-7":           {ContextTokens: 200_000, OutputTokens: 32_000},
	"claude-sonnet-4-6":         {ContextTokens: 200_000, OutputTokens: 64_000},
	"claude-haiku-4-5":          {ContextTokens: 200_000, OutputTokens: 8_192},
	"claude-haiku-4-5-20251001": {ContextTokens: 200_000, OutputTokens: 8_192},
	// Anthropic Claude 3.x
	"claude-3-5-sonnet-20241022": {ContextTokens: 200_000, OutputTokens: 8_192},
	"claude-3-5-haiku-20241022":  {ContextTokens: 200_000, OutputTokens: 8_192},
	"claude-3-opus-20240229":     {ContextTokens: 200_000, OutputTokens: 4_096},
	// OpenAI GPT-4.x
	"gpt-4o":       {ContextTokens: 128_000, OutputTokens: 16_384},
	"gpt-4o-mini":  {ContextTokens: 128_000, OutputTokens: 16_384},
	"gpt-4.1":      {ContextTokens: 1_047_576, OutputTokens: 32_768},
	"gpt-4.1-mini": {ContextTokens: 1_047_576, OutputTokens: 32_768},
	"gpt-4.1-nano": {ContextTokens: 1_047_576, OutputTokens: 32_768},
	"gpt-4-turbo":  {ContextTokens: 128_000, OutputTokens: 4_096},
	// OpenAI o-series
	"o3":      {ContextTokens: 200_000, OutputTokens: 100_000},
	"o4-mini": {ContextTokens: 200_000, OutputTokens: 100_000},
	// DeepSeek
	"deepseek-v3": {ContextTokens: 64_000, OutputTokens: 8_000},
	"deepseek-r1": {ContextTokens: 64_000, OutputTokens: 8_000},
	// Qwen
	"qwen2.5-72b-instruct": {ContextTokens: 131_072, OutputTokens: 8_192},
	"qwen3-235b-a22b":      {ContextTokens: 131_072, OutputTokens: 8_192},
	// Gemini
	"gemini-2.0-flash": {ContextTokens: 1_048_576, OutputTokens: 8_192},
	"gemini-2.5-pro":   {ContextTokens: 1_048_576, OutputTokens: 65_536},
	// Moonshot Kimi（OpenRouter 元数据，2026-09 实测：context 1M / max_completion 943K，
	// OutputTokens 取保守值 64K——单次响应远用不到 OpenRouter 上限）
	"kimi-k3": {ContextTokens: 1_048_576, OutputTokens: 65_536},
}

// defaultLimits 用于 knownModels 中未找到的模型，256K 是保守回退值（与 HermesAgent 策略一致）。
var defaultLimits = ModelLimits{ContextTokens: 256_000, OutputTokens: 8_192}

// GetModelLimits 返回指定模型名称的上下文窗口限制。
// 查找前自动剥离 provider 前缀（如 "openai/gpt-4o" → "gpt-4o"）。
// 未知模型返回 256K 保守回退值。
func GetModelLimits(modelName string) ModelLimits {
	bare := modelName
	if idx := strings.LastIndex(modelName, "/"); idx >= 0 {
		bare = modelName[idx+1:]
	}
	if limits, ok := knownModels[bare]; ok {
		return limits
	}
	return defaultLimits
}
