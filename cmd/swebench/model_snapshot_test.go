package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFetchModelSnapshot 验证 OpenRouter 公开元数据端点的抓取与解析：
// 正常路径（字段逐项映射）、裸模型名后缀匹配、模型未收录、非 200、非法 JSON。
func TestFetchModelSnapshot(t *testing.T) {
	okPayload := `{
	  "data": [
	    {"id": "openai/gpt-4o-mini", "name": "GPT-4o mini", "created": 1720000000,
	     "context_length": 128000,
	     "top_provider": {"context_length": 128000, "max_completion_tokens": 16384},
	     "pricing": {"prompt": "0.00000015", "completion": "0.0000006"}},
	    {"id": "moonshotai/kimi-k3", "name": "Kimi K3", "created": 1760000000,
	     "context_length": 262144,
	     "top_provider": {"context_length": 262144, "max_completion_tokens": 262144},
	     "pricing": {"prompt": "0.000003", "completion": "0.000015"}}
	  ]
	}`

	tests := []struct {
		name    string
		payload string
		status  int
		model   string
		want    *ModelSnapshot
		wantErr bool
	}{
		{
			name:    "正常抓取完整字段",
			payload: okPayload,
			status:  http.StatusOK,
			model:   "moonshotai/kimi-k3",
			want: &ModelSnapshot{
				ID:                  "moonshotai/kimi-k3",
				Name:                "Kimi K3",
				Created:             1760000000,
				ContextLength:       262144,
				MaxCompletionTokens: 262144,
				PricingPrompt:       "0.000003",
				PricingCompletion:   "0.000015",
			},
		},
		{
			name:    "裸模型名按后缀匹配",
			payload: okPayload,
			status:  http.StatusOK,
			model:   "kimi-k3",
			want: &ModelSnapshot{
				ID:            "moonshotai/kimi-k3",
				ContextLength: 262144,
			},
		},
		{
			name:    "模型未收录报错",
			payload: okPayload,
			status:  http.StatusOK,
			model:   "vendor/not-exist",
			wantErr: true,
		},
		{
			name:    "非 200 响应报错",
			payload: `{"error":"boom"}`,
			status:  http.StatusInternalServerError,
			model:   "moonshotai/kimi-k3",
			wantErr: true,
		},
		{
			name:    "非法 JSON 报错",
			payload: "not json",
			status:  http.StatusOK,
			model:   "moonshotai/kimi-k3",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.payload))
			}))
			defer ts.Close()

			got, err := fetchModelSnapshot(context.Background(), nil, ts.URL, tc.model)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (snap=%+v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil {
				t.Fatal("want snapshot, got nil")
			}
			if got.ID != tc.want.ID {
				t.Errorf("ID = %q, want %q", got.ID, tc.want.ID)
			}
			if tc.want.Name != "" && got.Name != tc.want.Name {
				t.Errorf("Name = %q, want %q", got.Name, tc.want.Name)
			}
			if tc.want.Created != 0 && got.Created != tc.want.Created {
				t.Errorf("Created = %d, want %d", got.Created, tc.want.Created)
			}
			if got.ContextLength != tc.want.ContextLength {
				t.Errorf("ContextLength = %d, want %d", got.ContextLength, tc.want.ContextLength)
			}
			if tc.want.MaxCompletionTokens != 0 && got.MaxCompletionTokens != tc.want.MaxCompletionTokens {
				t.Errorf("MaxCompletionTokens = %d, want %d", got.MaxCompletionTokens, tc.want.MaxCompletionTokens)
			}
			if tc.want.PricingPrompt != "" && got.PricingPrompt != tc.want.PricingPrompt {
				t.Errorf("PricingPrompt = %q, want %q", got.PricingPrompt, tc.want.PricingPrompt)
			}
			if tc.want.PricingCompletion != "" && got.PricingCompletion != tc.want.PricingCompletion {
				t.Errorf("PricingCompletion = %q, want %q", got.PricingCompletion, tc.want.PricingCompletion)
			}
			// 存证字段：来源端点与抓取时间必须留痕
			if got.Source != ts.URL {
				t.Errorf("Source = %q, want %q（来源端点应留痕）", got.Source, ts.URL)
			}
			if got.FetchedAt == "" {
				t.Error("FetchedAt 不应为空（抓取时间存证）")
			}
		})
	}
}

// TestFetchModelSnapshotNilFields 验证 OpenRouter 响应中 top_provider/max_completion_tokens
// 为 null 时（部分模型如此）解析不报错、字段安全回退为 0。
func TestFetchModelSnapshotNilFields(t *testing.T) {
	payload := `{"data": [{"id": "a/b", "name": "B", "top_provider": null,
	  "pricing": {"prompt": "0", "completion": "0"}}]}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer ts.Close()

	snap, err := fetchModelSnapshot(context.Background(), nil, ts.URL, "a/b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.ContextLength != 0 || snap.MaxCompletionTokens != 0 {
		t.Errorf("null 字段应回退为 0，got context=%d maxCompletion=%d",
			snap.ContextLength, snap.MaxCompletionTokens)
	}
}

// TestCaptureModelSnapshotFailOpen 验证 fail-open 包装：网络失败时返回 nil 而非 error，
// 评测主流程不阻断（官方要求存证但不应因元数据端点不可达放弃一轮昂贵的评测）。
func TestCaptureModelSnapshotFailOpen(t *testing.T) {
	// 端口 1 保留端口，连接必然失败
	snap := captureModelSnapshot(context.Background(), nil, "http://127.0.0.1:1/models", "x/y")
	if snap != nil {
		t.Fatalf("网络失败应 fail-open 返回 nil，got %+v", snap)
	}

	// 上下文已取消同样 fail-open
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if snap := captureModelSnapshot(ctx, nil, "http://127.0.0.1:1/models", "x/y"); snap != nil {
		t.Fatalf("ctx 取消应 fail-open 返回 nil，got %+v", snap)
	}
}

// TestSaveModelSnapshot 验证快照落盘：JSON 可回读、字段无损。
func TestSaveModelSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_snapshot.json")
	snap := &ModelSnapshot{
		ID:                  "moonshotai/kimi-k3",
		Name:                "Kimi K3",
		Created:             1760000000,
		ContextLength:       262144,
		MaxCompletionTokens: 262144,
		PricingPrompt:       "0.000003",
		PricingCompletion:   "0.000015",
		FetchedAt:           "2026-09-10T00:00:00Z",
		Source:              openRouterModelsURL,
	}
	if err := saveModelSnapshot(path, snap); err != nil {
		t.Fatalf("saveModelSnapshot error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got ModelSnapshot
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("快照文件不是合法 JSON: %v", err)
	}
	if got != *snap {
		t.Errorf("回读不一致:\n got  %+v\n want %+v", got, *snap)
	}
}

// TestDescribeModelSnapshot 验证报告摘要行的生成（nil 安全）。
func TestDescribeModelSnapshot(t *testing.T) {
	tests := []struct {
		name string
		snap *ModelSnapshot
		want string
	}{
		{
			name: "nil 快照回退文案",
			snap: nil,
			want: "未获取",
		},
		{
			name: "完整快照含 id 与定价",
			snap: &ModelSnapshot{ID: "moonshotai/kimi-k3", Name: "Kimi K3",
				ContextLength: 262144, PricingPrompt: "0.000003", PricingCompletion: "0.000015"},
			want: "moonshotai/kimi-k3",
		},
		{
			name: "最小快照仅 id",
			snap: &ModelSnapshot{ID: "a/b"},
			want: "a/b",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := describeModelSnapshot(tc.snap)
			// 统一按"包含关键子串"断言；nil 快照必须如实标注"未获取"（fail-open 存证语义）
			if !containsAll(got, []string{tc.want}) {
				t.Errorf("describe = %q, want contains %q", got, tc.want)
			}
			if tc.snap == nil {
				return
			}
			if tc.snap.PricingPrompt != "" && !containsAll(got, []string{tc.snap.PricingPrompt, tc.snap.PricingCompletion}) {
				t.Errorf("describe = %q, want contains pricing %q/%q", got, tc.snap.PricingPrompt, tc.snap.PricingCompletion)
			}
			if tc.snap.ContextLength != 0 && !containsAll(got, []string{"262144"}) {
				t.Errorf("describe = %q, want contains context_length", got)
			}
		})
	}
}

// containsAll 报告 s 是否包含 subs 中全部子串（测试辅助）。
func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
