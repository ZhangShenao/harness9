// Package provider — Anthropic 流式 token 用量合并逻辑单元测试。
package provider

import (
	"testing"

	"github.com/harness9/internal/schema"
)

// TestApplyStreamUsage 表驱动测试 applyStreamUsage 的合并规则：
//   - 仅覆盖 > 0 的字段（message_delta 场景保留 message_start 已记录的 InputTokens）
//   - actualUsage 为 nil 时自动新建
//   - 两条事件先后到达时用量正确累积
func TestApplyStreamUsage(t *testing.T) {
	cases := []struct {
		name        string
		initial     *schema.Usage // 传 nil 表示"尚无用量（首个事件之前）"
		inputTokens int64
		outTokens   int64
		wantIn      int
		wantOut     int
	}{
		{
			// message_start：仅 InputTokens 有效，OutputTokens 为 0
			name:        "message_start sets input tokens",
			initial:     nil,
			inputTokens: 1500,
			outTokens:   0,
			wantIn:      1500,
			wantOut:     0,
		},
		{
			// message_delta：仅 OutputTokens 有效，保留已记录的 InputTokens
			name:        "message_delta merges output tokens keeping input",
			initial:     &schema.Usage{InputTokens: 1500, OutputTokens: 0},
			inputTokens: 0,
			outTokens:   320,
			wantIn:      1500,
			wantOut:     320,
		},
		{
			// 防御：消息段未携带任何用量时不应破坏既有值
			name:        "zero values keep existing usage",
			initial:     &schema.Usage{InputTokens: 100, OutputTokens: 50},
			inputTokens: 0,
			outTokens:   0,
			wantIn:      100,
			wantOut:     50,
		},
		{
			// 边界：所有字段为 0 且 initial 为 nil → 返回非 nil 空用量（避免 nil 引用）
			name:        "nil usage with zero values returns empty non-nil usage",
			initial:     nil,
			inputTokens: 0,
			outTokens:   0,
			wantIn:      0,
			wantOut:     0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applyStreamUsage(tc.initial, tc.inputTokens, tc.outTokens)
			if got == nil {
				t.Fatal("applyStreamUsage returned nil")
			}
			if got.InputTokens != tc.wantIn {
				t.Errorf("InputTokens = %d, want %d", got.InputTokens, tc.wantIn)
			}
			if got.OutputTokens != tc.wantOut {
				t.Errorf("OutputTokens = %d, want %d", got.OutputTokens, tc.wantOut)
			}
		})
	}
}
