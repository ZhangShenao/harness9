// orcarouter_test.go — OrcaRouter 网关适配器单元测试。
// 覆盖构造器环境变量解析、默认 Base URL、选项透传与 NewFromEnv 的选择规则。
package provider

import (
	"strings"
	"testing"
)

func TestNewOrcaRouterProvider(t *testing.T) {
	tests := []struct {
		name               string
		env                map[string]string
		model              string
		opts               []OpenAIOption
		wantErrContains    string
		wantModel          string
		wantIncludeReas    bool
		wantIncludeReasSet bool
	}{
		{
			name:            "缺少 ORCAROUTER_API_KEY 报错",
			env:             map[string]string{},
			wantErrContains: "ORCAROUTER_API_KEY",
		},
		{
			name:               "仅配置 Key 时使用默认网关地址",
			env:                map[string]string{"ORCAROUTER_API_KEY": "sk-orca-test"},
			model:              "openai/gpt-4o-mini",
			wantModel:          "openai/gpt-4o-mini",
			wantIncludeReas:    false,
			wantIncludeReasSet: true,
		},
		{
			name: "自定义 ORCAROUTER_BASE_URL 覆盖默认值",
			env: map[string]string{
				"ORCAROUTER_API_KEY":  "sk-orca-test",
				"ORCAROUTER_BASE_URL": "https://my-proxy.example.com/v1",
			},
			model:              "deepseek/deepseek-chat",
			wantModel:          "deepseek/deepseek-chat",
			wantIncludeReas:    false,
			wantIncludeReasSet: true,
		},
		{
			name:               "WithIncludeReasoning 选项透传生效",
			env:                map[string]string{"ORCAROUTER_API_KEY": "sk-orca-test"},
			model:              "openai/gpt-4o-mini",
			opts:               []OpenAIOption{WithIncludeReasoning()},
			wantModel:          "openai/gpt-4o-mini",
			wantIncludeReas:    true,
			wantIncludeReasSet: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			p, err := NewOrcaRouterProvider(tt.model, tt.opts...)
			if tt.wantErrContains != "" {
				if err == nil {
					t.Fatalf("期望报错包含 %q，实际成功", tt.wantErrContains)
				}
				if !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Fatalf("错误信息 %q 不包含 %q", err.Error(), tt.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewOrcaRouterProvider 意外报错: %v", err)
			}
			if p.model != tt.wantModel {
				t.Errorf("model = %q, 期望 %q", p.model, tt.wantModel)
			}
			if tt.wantIncludeReasSet && p.includeReasoning != tt.wantIncludeReas {
				t.Errorf("includeReasoning = %v, 期望 %v", p.includeReasoning, tt.wantIncludeReas)
			}
			// 不论 Base URL 是什么，OrcaRouter 都不应自动启用 include_reasoning
			//（网关原生在标准 reasoning 字段返回推理内容，无需额外标志）。
			if p.includeReasoning && len(tt.opts) == 0 {
				t.Errorf("OrcaRouter 默认不应启用 includeReasoning")
			}
		})
	}
}

// TestBaseURLEnablesReasoningOrcaRouter 锁定白名单行为：
// OrcaRouter 网关地址不在 include_reasoning 白名单中（与 OpenRouter/Requesty 不同）。
func TestBaseURLEnablesReasoningOrcaRouter(t *testing.T) {
	if baseURLEnablesReasoning(DefaultOrcaRouterBaseURL) {
		t.Errorf("OrcaRouter 默认地址不应启用 include_reasoning（网关原生返回 reasoning 字段）")
	}
}

func TestNewFromEnv(t *testing.T) {
	tests := []struct {
		name            string
		env             map[string]string
		wantErrContains string
	}{
		{
			name: "显式 LLM_PROVIDER=orcarouter 且 Key 就绪",
			env: map[string]string{
				"LLM_PROVIDER":       "orcarouter",
				"ORCAROUTER_API_KEY": "sk-orca-test",
			},
		},
		{
			name:            "显式 LLM_PROVIDER=orcarouter 但缺少 Key",
			env:             map[string]string{"LLM_PROVIDER": "orcarouter"},
			wantErrContains: "ORCAROUTER_API_KEY",
		},
		{
			name: "显式 LLM_PROVIDER=openai 时忽略 OrcaRouter Key（走 OPENAI_* 路径）",
			env: map[string]string{
				"LLM_PROVIDER":       "openai",
				"ORCAROUTER_API_KEY": "sk-orca-test",
				"OPENAI_API_KEY":     "sk-openai-test",
				// 故意不设 OPENAI_BASE_URL：openai 路径必须因缺 BASE_URL 报错，以此证明未走 OrcaRouter
			},
			wantErrContains: "OPENAI_BASE_URL",
		},
		{
			name: "自动探测：仅有 ORCAROUTER_API_KEY 时走 OrcaRouter",
			env: map[string]string{
				"ORCAROUTER_API_KEY": "sk-orca-test",
			},
		},
		{
			name: "自动探测：两 Key 并存时保持 OPENAI_* 优先（避免静默切换网关）",
			env: map[string]string{
				"ORCAROUTER_API_KEY": "sk-orca-test",
				"OPENAI_API_KEY":     "sk-openai-test",
				// 缺 OPENAI_BASE_URL 使 openai 路径报错，证明走了 openai 分支
			},
			wantErrContains: "OPENAI_BASE_URL",
		},
		{
			name: "自动探测：无任何 OrcaRouter 配置时沿用 OPENAI_* 路径",
			env: map[string]string{
				"OPENAI_API_KEY": "sk-openai-test",
			},
			wantErrContains: "OPENAI_BASE_URL",
		},
		{
			name:            "未知 LLM_PROVIDER 值报错",
			env:             map[string]string{"LLM_PROVIDER": "azure"},
			wantErrContains: "LLM_PROVIDER",
		},
		{
			name: "LLM_PROVIDER 大小写不敏感",
			env: map[string]string{
				"LLM_PROVIDER":       "OrcaRouter",
				"ORCAROUTER_API_KEY": "sk-orca-test",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			p, err := NewFromEnv("openai/gpt-4o-mini")
			if tt.wantErrContains != "" {
				if err == nil {
					t.Fatalf("期望报错包含 %q，实际成功", tt.wantErrContains)
				}
				if !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Fatalf("错误信息 %q 不包含 %q", err.Error(), tt.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewFromEnv 意外报错: %v", err)
			}
			op, ok := p.(*OpenAIProvider)
			if !ok {
				t.Fatalf("返回类型 %T 不是 *OpenAIProvider", p)
			}
			if op.model != "openai/gpt-4o-mini" {
				t.Errorf("model = %q, 期望 openai/gpt-4o-mini", op.model)
			}
		})
	}
}
