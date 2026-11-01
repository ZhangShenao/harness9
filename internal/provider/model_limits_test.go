package provider_test

import (
	"testing"

	"github.com/harness9/internal/provider"
)

func TestGetModelLimits_KnownModel(t *testing.T) {
	limits := provider.GetModelLimits("gpt-4o")
	if limits.ContextTokens != 128_000 {
		t.Fatalf("want 128000, got %d", limits.ContextTokens)
	}
	if limits.OutputTokens != 16_384 {
		t.Fatalf("want 16384, got %d", limits.OutputTokens)
	}
}

func TestGetModelLimits_WithProviderPrefix(t *testing.T) {
	limits := provider.GetModelLimits("openai/gpt-4o-mini")
	if limits.ContextTokens != 128_000 {
		t.Fatalf("want 128000, got %d", limits.ContextTokens)
	}
}

func TestGetModelLimits_UnknownModel(t *testing.T) {
	limits := provider.GetModelLimits("unknown-model-xyz")
	if limits.ContextTokens != 256_000 {
		t.Fatalf("unknown model fallback should be 256000, got %d", limits.ContextTokens)
	}
}

func TestGetModelLimits_Anthropic(t *testing.T) {
	limits := provider.GetModelLimits("claude-sonnet-4-6")
	if limits.ContextTokens != 200_000 {
		t.Fatalf("want 200000, got %d", limits.ContextTokens)
	}
}

func TestGetModelLimits_AnthropicWithPrefix(t *testing.T) {
	limits := provider.GetModelLimits("anthropic/claude-opus-4-7")
	if limits.ContextTokens != 200_000 {
		t.Fatalf("want 200000, got %d", limits.ContextTokens)
	}
}

func TestGetModelLimits_EmptyString(t *testing.T) {
	limits := provider.GetModelLimits("")
	if limits.ContextTokens != 256_000 {
		t.Fatalf("empty string should return fallback 256000, got %d", limits.ContextTokens)
	}
}

func TestGetModelLimits_GeminiLargeContext(t *testing.T) {
	limits := provider.GetModelLimits("gemini-2.5-pro")
	if limits.ContextTokens != 1_048_576 {
		t.Fatalf("want 1048576, got %d", limits.ContextTokens)
	}
}

func TestGetModelLimits_KimiK3(t *testing.T) {
	limits := provider.GetModelLimits("moonshotai/kimi-k3")
	if limits.ContextTokens != 1_048_576 {
		t.Fatalf("want 1048576, got %d", limits.ContextTokens)
	}
	if limits.OutputTokens != 65_536 {
		t.Fatalf("want 65536, got %d", limits.OutputTokens)
	}
}

func TestGetModelLimits_DeepSeekFlash(t *testing.T) {
	limits := provider.GetModelLimits("deepseek-flash")
	if limits.ContextTokens != 1_048_576 {
		t.Fatalf("want 1048576, got %d", limits.ContextTokens)
	}
	if limits.OutputTokens != 393_216 {
		t.Fatalf("want 393216, got %d", limits.OutputTokens)
	}
}

func TestGetModelLimits_DeepSeekV4Pro(t *testing.T) {
	limits := provider.GetModelLimits("deepseek-v4-pro")
	if limits.ContextTokens != 1_048_576 {
		t.Fatalf("want 1048576, got %d", limits.ContextTokens)
	}
}

func TestGetModelLimits_DeepSeekLegacyAlias(t *testing.T) {
	// 旧模型名 deepseek-v4-flash 官网声明仍可调用（由新模型提供服务），
	// 应命中同一条注册表条目而非回退 256K 默认值。
	limits := provider.GetModelLimits("deepseek-v4-flash")
	if limits.ContextTokens != 1_048_576 {
		t.Fatalf("legacy alias should resolve to new model limits, want 1048576, got %d", limits.ContextTokens)
	}
}

func TestGetModelLimits_DeepSeekLegacyV3(t *testing.T) {
	limits := provider.GetModelLimits("deepseek-v3")
	if limits.ContextTokens != 64_000 {
		t.Fatalf("want 64000, got %d", limits.ContextTokens)
	}
}

func TestGetModelLimits_DeepSeekWithProviderPrefix(t *testing.T) {
	limits := provider.GetModelLimits("openai/deepseek-flash")
	if limits.ContextTokens != 1_048_576 {
		t.Fatalf("want 1048576, got %d", limits.ContextTokens)
	}
}
