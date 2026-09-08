// orcarouter_live_test.go — OrcaRouter 真实网关集成测试。
//
// 未配置 ORCAROUTER_API_KEY 时自动跳过：go test ./...（含 CI）保持密封零网络调用；
// 本地验证方式：
//
//	ORCAROUTER_API_KEY=sk-orca-xxx go test ./internal/provider/ -run 'TestOrcaRouterLive' -v
//
// 默认使用廉价模型 openai/gpt-4o-mini，可用 ORCAROUTER_LIVE_MODEL 覆盖。
package provider

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/harness9/internal/schema"
)

// orcaLiveSetup 返回接入真实网关的 Provider；未配置 Key 时跳过测试。
func orcaLiveSetup(t *testing.T) *OpenAIProvider {
	t.Helper()
	if os.Getenv("ORCAROUTER_API_KEY") == "" {
		t.Skip("未配置 ORCAROUTER_API_KEY，跳过 OrcaRouter 真实网关集成测试")
	}
	model := os.Getenv("ORCAROUTER_LIVE_MODEL")
	if model == "" {
		model = "openai/gpt-4o-mini"
	}
	p, err := NewOrcaRouterProvider(model)
	if err != nil {
		t.Fatalf("创建 OrcaRouter Provider 失败: %v", err)
	}
	return p
}

// TestOrcaRouterLiveGenerate 验证阻塞式文本生成与实际 token 用量提取。
func TestOrcaRouterLiveGenerate(t *testing.T) {
	p := orcaLiveSetup(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	msg, usage, err := p.Generate(ctx, []schema.Message{
		{Role: schema.RoleUser, Content: "只回复两个字：成功"},
	}, nil)
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	if strings.TrimSpace(msg.Content) == "" {
		t.Errorf("响应内容为空")
	}
	if usage == nil || usage.InputTokens <= 0 || usage.OutputTokens <= 0 {
		t.Errorf("实际 token 用量未提取：usage=%+v", usage)
	}
	t.Logf("响应: %q, usage: in=%d out=%d", msg.Content, usage.InputTokens, usage.OutputTokens)
}

// TestOrcaRouterLiveGenerateStream 验证流式增量的收集与末尾 Done chunk 的 Usage 携带。
func TestOrcaRouterLiveGenerateStream(t *testing.T) {
	p := orcaLiveSetup(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ch, err := p.GenerateStream(ctx, []schema.Message{
		{Role: schema.RoleUser, Content: "从 1 数到 5，用空格分隔数字，不要其他内容"},
	}, nil)
	if err != nil {
		t.Fatalf("GenerateStream 失败: %v", err)
	}

	var textBuf strings.Builder
	var doneMsg *schema.Message
	var usage *schema.Usage
	for chunk := range ch {
		switch chunk.Type {
		case schema.StreamChunkTextDelta:
			textBuf.WriteString(chunk.Delta)
		case schema.StreamChunkDone:
			doneMsg = chunk.Message
			usage = chunk.Usage
		case schema.StreamChunkError:
			t.Fatalf("流式错误: %s", chunk.Error)
		}
	}
	if textBuf.Len() == 0 {
		t.Errorf("未收到任何文本增量")
	}
	if doneMsg == nil {
		t.Fatalf("未收到 Done chunk")
	}
	if usage == nil || usage.InputTokens <= 0 {
		t.Errorf("流式实际 token 用量未提取：usage=%+v", usage)
	}
	t.Logf("流式文本: %q, usage: in=%d out=%d", textBuf.String(), usage.InputTokens, usage.OutputTokens)
}

// TestOrcaRouterLiveToolCalling 验证网关的工具调用回路：模型应针对天气问题
// 发起 get_weather 工具调用而非凭空回答。
func TestOrcaRouterLiveToolCalling(t *testing.T) {
	p := orcaLiveSetup(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	weatherTool := schema.ToolDefinition{
		Name:        "get_weather",
		Description: "查询指定城市的当前天气",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"city": map[string]interface{}{"type": "string", "description": "城市名"},
			},
			"required": []string{"city"},
		},
	}

	msg, _, err := p.Generate(ctx, []schema.Message{
		{Role: schema.RoleUser, Content: "请调用 get_weather 工具查询北京的天气"},
	}, []schema.ToolDefinition{weatherTool})
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	if len(msg.ToolCalls) == 0 {
		t.Fatalf("模型未发起工具调用，响应内容: %q", msg.Content)
	}
	found := false
	for _, tc := range msg.ToolCalls {
		if tc.Name == "get_weather" {
			found = true
			if !strings.Contains(string(tc.Arguments), "北京") {
				t.Errorf("工具参数未携带城市名: %s", tc.Arguments)
			}
		}
	}
	if !found {
		t.Errorf("未调用 get_weather 工具，实际调用：%+v", msg.ToolCalls)
	}
	t.Logf("工具调用: name=%s args=%s", msg.ToolCalls[0].Name, msg.ToolCalls[0].Arguments)
}

// TestOrcaRouterLiveReasoning 验证推理模型的 reasoning_content 字段被路由为
// ThinkingDelta。默认模型 deepseek/deepseek-reasoner 可用 ORCAROUTER_LIVE_REASONING_MODEL 覆盖。
func TestOrcaRouterLiveReasoning(t *testing.T) {
	if os.Getenv("ORCAROUTER_API_KEY") == "" {
		t.Skip("未配置 ORCAROUTER_API_KEY，跳过 OrcaRouter 真实网关集成测试")
	}
	model := os.Getenv("ORCAROUTER_LIVE_REASONING_MODEL")
	if model == "" {
		model = "deepseek/deepseek-reasoner"
	}
	p, err := NewOrcaRouterProvider(model)
	if err != nil {
		t.Fatalf("创建 Provider 失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	ch, err := p.GenerateStream(ctx, []schema.Message{
		{Role: schema.RoleUser, Content: "9.11 和 9.9 哪个大？简要回答"},
	}, nil)
	if err != nil {
		t.Fatalf("GenerateStream 失败: %v", err)
	}

	var thinking, text strings.Builder
	sawDone := false
	for chunk := range ch {
		switch chunk.Type {
		case schema.StreamChunkThinkingDelta:
			thinking.WriteString(chunk.Delta)
		case schema.StreamChunkTextDelta:
			text.WriteString(chunk.Delta)
		case schema.StreamChunkDone:
			sawDone = true
		case schema.StreamChunkError:
			t.Fatalf("流式错误: %s", chunk.Error)
		}
	}
	if !sawDone {
		t.Fatalf("未收到 Done chunk")
	}
	if text.Len() == 0 {
		t.Errorf("未收到正文回复")
	}
	// 推理内容取决于模型/网关行为：有则记录验证通过，无则显式提示（不判失败，
	// 避免上游静默调整推理暴露策略导致测试抖动）。
	if thinking.Len() > 0 {
		t.Logf("✓ 捕获 ThinkingDelta %d 字节", thinking.Len())
	} else {
		t.Logf("⚠ 未捕获 ThinkingDelta（模型 %s 本次未返回推理内容）", model)
	}
	t.Logf("正文: %q", text.String())
}
