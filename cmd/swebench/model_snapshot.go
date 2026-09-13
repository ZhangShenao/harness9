package main

// model_snapshot：SWE-bench 评测的模型版本快照存证（官方打榜 P0 可复现性）。
//
// 背景：经 OpenRouter 等路由网关服务的一轮评测，"模型版本"随上游权重滚动更新而漂移，
// 事后无法证明"该轮分数由哪个版本模型服务"。本文件在评测启动时从 OpenRouter 公开
// 元数据端点（无需鉴权）抓取所用模型的元数据快照并落盘 run 目录，同时写入报告摘要，
// 作为提交材料的版本存证。
//
// 设计约束：fail-open——元数据抓取是存证性质，端点不可达 / 模型未收录只记警告，
// 绝不阻断一轮昂贵的评测。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// openRouterModelsURL 是 OpenRouter 公开元数据端点（GET，无需鉴权）。
const openRouterModelsURL = "https://openrouter.ai/api/v1/models"

// modelsSnapshotTimeout 是元数据抓取的 HTTP 超时。
const modelsSnapshotTimeout = 10 * time.Second

// ModelSnapshot 是一次评测所用模型的版本快照（落盘 model_snapshot.json）。
// 数值字段取自 OpenRouter 模型元数据：context_length 为模型声明上下文，
// top_provider.context_length/max_completion_tokens 为路由方实际限额，
// pricing.prompt/completion 为 USD/token 计价字符串（保留原始字符串避免浮点误差）。
type ModelSnapshot struct {
	ID                  string `json:"id"`
	Name                string `json:"name,omitempty"`
	Created             int64  `json:"created,omitempty"`
	ContextLength       int64  `json:"context_length,omitempty"`
	MaxCompletionTokens int64  `json:"max_completion_tokens,omitempty"`
	PricingPrompt       string `json:"pricing_prompt,omitempty"`
	PricingCompletion   string `json:"pricing_completion,omitempty"`
	// FetchedAt/Source 是存证字段：抓取时间与数据来源端点，事后可复核。
	FetchedAt string `json:"fetched_at"`
	Source    string `json:"source"`
}

// orModelsResponse 是 OpenRouter 元数据端点的响应骨架（仅取所需字段）。
type orModelsResponse struct {
	Data []orModel `json:"data"`
}

// orModel 是端点返回的单模型元数据；top_provider 与 pricing 的部分字段可能为 null。
type orModel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Created       int64  `json:"created"`
	ContextLength int64  `json:"context_length"`
	TopProvider   struct {
		ContextLength       int64 `json:"context_length"`
		MaxCompletionTokens int64 `json:"max_completion_tokens"`
	} `json:"top_provider"`
	Pricing struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
}

// fetchModelSnapshot 从 baseURL 抓取并解析 model 的元数据快照。
// 匹配规则：先精确匹配 id；未命中时对裸模型名（无 vendor 前缀）按 "/<model>" 后缀匹配。
// 任何失败（网络/状态码/解析/未收录）返回 error，由调用方决定 fail-open 策略。
// client 为 nil 时使用带 modelsSnapshotTimeout 超时的默认客户端。
func fetchModelSnapshot(ctx context.Context, client *http.Client, baseURL, model string) (*ModelSnapshot, error) {
	if client == nil {
		client = &http.Client{Timeout: modelsSnapshotTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造模型元数据请求失败：%w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求模型元数据失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("模型元数据端点返回非 200：HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("读取模型元数据响应失败：%w", err)
	}
	var parsed orModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析模型元数据失败：%w", err)
	}
	for _, m := range parsed.Data {
		if m.ID != model && !strings.HasSuffix(m.ID, "/"+model) {
			continue
		}
		snap := &ModelSnapshot{
			ID:                  m.ID,
			Name:                m.Name,
			Created:             m.Created,
			ContextLength:       m.ContextLength,
			MaxCompletionTokens: m.TopProvider.MaxCompletionTokens,
			PricingPrompt:       m.Pricing.Prompt,
			PricingCompletion:   m.Pricing.Completion,
			FetchedAt:           time.Now().UTC().Format(time.RFC3339),
			Source:              baseURL,
		}
		// top_provider.context_length 是路由方实际生效的上下文限额，缺失时回退模型声明值。
		if m.TopProvider.ContextLength > 0 {
			snap.ContextLength = m.TopProvider.ContextLength
		}
		return snap, nil
	}
	return nil, fmt.Errorf("模型 %q 未收录于元数据端点", model)
}

// captureModelSnapshot 是 fail-open 包装：抓取失败记警告并返回 nil，绝不阻断评测。
func captureModelSnapshot(ctx context.Context, client *http.Client, baseURL, model string) *ModelSnapshot {
	snap, err := fetchModelSnapshot(ctx, client, baseURL, model)
	if err != nil {
		fmt.Fprintf(os.Stderr, "警告: 模型元数据快照抓取失败（fail-open，不阻断评测）: %v\n", err)
		return nil
	}
	return snap
}

// saveModelSnapshot 将快照以 JSON 落盘（存证文件 model_snapshot.json）。
func saveModelSnapshot(path string, snap *ModelSnapshot) error {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化模型快照失败：%w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("写入模型快照失败：%w", err)
	}
	return nil
}

// describeModelSnapshot 生成报告摘要用的一行描述；nil 快照回退为如实的"未获取"文案。
func describeModelSnapshot(snap *ModelSnapshot) string {
	if snap == nil {
		return "未获取（元数据端点不可达或模型未收录，fail-open 继续评测）"
	}
	var b strings.Builder
	b.WriteString(snap.ID)
	if snap.Name != "" {
		b.WriteString(" " + snap.Name)
	}
	if snap.ContextLength > 0 {
		fmt.Fprintf(&b, " context_length=%d", snap.ContextLength)
	}
	if snap.MaxCompletionTokens > 0 {
		fmt.Fprintf(&b, " max_completion_tokens=%d", snap.MaxCompletionTokens)
	}
	if snap.PricingPrompt != "" {
		fmt.Fprintf(&b, " pricing prompt=%s/completion=%s USD/token", snap.PricingPrompt, snap.PricingCompletion)
	}
	fmt.Fprintf(&b, " fetched_at=%s", snap.FetchedAt)
	return b.String()
}
