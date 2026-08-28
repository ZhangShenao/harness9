package memory_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/schema"
)

// TestProgressiveCompactor_E2E_FullPipeline 是一个端到端测试，模拟真实 Agent 长程任务：
//
// 1. 构建一个带有小 context window 的 ProgressiveCompactor（便于快速触发压缩）
// 2. 注入 Offloader + RecordStore（真实文件系统 I/O）
// 3. 分阶段注入大量消息，依次触发 TierWarn -> TierFull
// 4. 验证每个阶段的行为：
//   - TierWarn: offload 大 tool_result，无 LLM 调用
//   - TierFull: LLM 摘要 + 锚点提取 + offload
//   - 增量更新：第二次 TierFull 使用增量模板
//   - 持久化：JSONL 文件包含正确的压缩记录
//   - 可检索性：offload 文件真实存在于文件系统
func TestProgressiveCompactor_E2E_FullPipeline(t *testing.T) {
	workDir := t.TempDir()
	sessionID := "e2e-sess-001"
	recordsDir := filepath.Join(workDir, "compaction_records")

	offloader := memory.NewCompactionOffloader(workDir, sessionID)
	recordStore := memory.NewFileRecordStore(recordsDir)

	// mockSummarizer 返回结构化锚点输出
	anchorOutput := `## Anchors

### User Intent
Build a Go web server with chi router and middleware

### Execution Progress
- Created project structure
- Implemented health endpoint
- Added logging middleware

### Key Decisions
- Using chi router for performance
- JSON middleware for API responses

### Tried Solutions
- Tried gin: too heavy for this use case
- Tried net/http stdlib: too verbose for routing

### Next Steps
- Add authentication middleware
- Write integration tests
- Deploy to staging

## Summary
The project uses Go 1.25 with chi router. Main file is cmd/server/main.go. Configuration via environment variables.`
	mock := &mockSummarizer{responses: []string{anchorOutput, anchorOutput}}

	// contextWindow = 3000 tokens (12000 chars)
	// TierWarn: 60% = 1800 tokens = 7200 chars
	// TierSoft: 70% = 2100 tokens = 8400 chars
	// TierFull: 80% = 2400 tokens = 9600 chars
	// TierEmergency: 95% = 2850 tokens = 11400 chars
	c := memory.NewProgressiveCompactor(mock, 3000,
		memory.WithProgressiveOffloader(offloader),
		memory.WithProgressiveRecordStore(recordStore),
		memory.WithProgressiveSessionID(sessionID),
	)
	c.MinTailMessages = 2

	t.Run("Phase1_TierWarn_OffloadOnly", func(t *testing.T) {
		// 构建约 8000 chars 的消息（~2000 tokens = 66.7% → TierWarn）
		msgs := []schema.Message{
			{Role: schema.RoleSystem, Content: "sys"},
			{Role: schema.RoleUser, Content: "帮我搭建一个 Go web 服务器"},
			{Role: schema.RoleAssistant, Content: "好的，我先检查项目结构。", ToolCalls: []schema.ToolCall{
				{ID: "tc_warn_1", Name: "bash", Arguments: []byte(`{"command":"ls -la /usr/local/go/src"}`)},
			}},
			// 大 tool_result（8000 chars → 超过 4000 OffloadThreshold）
			{Role: schema.RoleUser, Content: "[tool_result]" + strings.Repeat("line of output\n", 530), ToolCallID: "tc_warn_1"},
			{Role: schema.RoleAssistant, Content: "项目结构已确认。"},
			// tail (MinTailMessages=2)
			{Role: schema.RoleUser, Content: "tail1"},
			{Role: schema.RoleAssistant, Content: "tail2"},
		}

		result, record := c.CompactWithRecord(msgs)

		if record.Tier != memory.TierWarn {
			t.Fatalf("Phase1: want TierWarn, got %d (ratio=%.2f)", record.Tier, float64(record.TokensBefore)/3000)
		}
		if len(mock.calls) != 0 {
			t.Error("Phase1: provider should not be called for TierWarn")
		}
		if len(record.Offloaded) == 0 {
			t.Error("Phase1: should have offloaded at least 1 tool_result")
		}
		if record.Summarized != 0 {
			t.Error("Phase1: TierWarn should not summarize")
		}

		// 验证 offload 文件真实存在
		for _, entry := range record.Offloaded {
			absPath := filepath.Join(workDir, entry.FilePath)
			if _, err := os.Stat(absPath); err != nil {
				t.Errorf("Phase1: offloaded file should exist at %s: %v", absPath, err)
			}
		}

		// 验证压缩后 token 减少
		if record.TokensAfter >= record.TokensBefore {
			t.Error("Phase1: tokens should decrease after offload")
		}

		// 验证 result 仍以 system 开头
		if result[0].Role != schema.RoleSystem {
			t.Error("Phase1: result must start with system message")
		}
	})

	t.Run("Phase2_TierFull_AnchorAndSummary", func(t *testing.T) {
		// 构建约 10000 chars 的消息（~2500 tokens = 83% → TierFull）
		msgs := []schema.Message{
			{Role: schema.RoleSystem, Content: "sys"},
			{Role: schema.RoleUser, Content: "帮我搭建一个 Go web 服务器，用 chi router"},
			{Role: schema.RoleAssistant, Content: "好的，我来检查项目结构并搭建。", ToolCalls: []schema.ToolCall{
				{ID: "tc_full_1", Name: "bash", Arguments: []byte(`{"command":"find . -name '*.go'"}`)},
			}},
			{Role: schema.RoleUser, Content: "[tool_result]" + strings.Repeat("file entry\n", 500), ToolCallID: "tc_full_1"},
			{Role: schema.RoleAssistant, Content: "找到了 Go 文件。现在开始搭建服务器。", ToolCalls: []schema.ToolCall{
				{ID: "tc_full_2", Name: "write_file", Arguments: []byte(`{"path":"main.go"}`)},
			}},
			{Role: schema.RoleUser, Content: "文件已写入。", ToolCallID: "tc_full_2"},
			{Role: schema.RoleAssistant, Content: strings.Repeat("服务器代码已创建，包含路由和中间件。", 50)},
			{Role: schema.RoleUser, Content: strings.Repeat("看起来不错。", 50)},
			{Role: schema.RoleAssistant, Content: strings.Repeat("接下来添加健康检查端点。", 50)},
			// tail (MinTailMessages=2)
			{Role: schema.RoleUser, Content: "tail1"},
			{Role: schema.RoleAssistant, Content: "tail2"},
		}

		result, record := c.CompactWithRecord(msgs)

		if record.Tier != memory.TierFull {
			t.Fatalf("Phase2: want TierFull, got %d (ratio=%.2f)", record.Tier, float64(record.TokensBefore)/3000)
		}
		if len(mock.calls) != 1 {
			t.Fatalf("Phase2: expected 1 LLM call, got %d", len(mock.calls))
		}
		if len(record.Anchors) != 5 {
			t.Errorf("Phase2: want 5 anchors, got %d", len(record.Anchors))
		}
		if record.Summarized == 0 {
			t.Error("Phase2: should have summarized messages")
		}
		if record.SummaryText == "" {
			t.Error("Phase2: summary text should not be empty")
		}
		if len(record.Offloaded) == 0 {
			t.Error("Phase2: should have offloaded tool_results")
		}

		// 验证 result[1] 是 [Context Compaction] 消息
		if len(result) < 2 {
			t.Fatal("Phase2: result too short")
		}
		if !strings.HasPrefix(result[1].Content, "[Context Compaction]") {
			t.Errorf("Phase2: result[1] should be compaction msg, got: %q", result[1].Content[:min(50, len(result[1].Content))])
		}

		// 验证锚点内容
		intentFound := false
		for _, a := range record.Anchors {
			if a.Type == memory.AnchorUserIntent && strings.Contains(a.Content, "web server") {
				intentFound = true
			}
		}
		if !intentFound {
			t.Error("Phase2: user_intent anchor should mention 'web server'")
		}

		// 验证压缩比
		if record.CompressionRatio >= 1.0 {
			t.Error("Phase2: compression ratio should be < 1.0")
		}
	})

	t.Run("Phase3_IncrementalUpdate", func(t *testing.T) {
		// 第二次 TierFull，验证增量更新
		// 上次压缩后 lastSummary/lastAnchors 已通过 Phase2 设置
		msgs := []schema.Message{
			{Role: schema.RoleSystem, Content: "sys"},
			{Role: schema.RoleUser, Content: "帮我搭建一个 Go web 服务器，用 chi router"},
			{Role: schema.RoleAssistant, Content: "好的，我来检查项目结构并搭建。", ToolCalls: []schema.ToolCall{
				{ID: "tc_inc_1", Name: "bash", Arguments: []byte(`{"command":"find . -name '*.go'"}`)},
			}},
			{Role: schema.RoleUser, Content: "[tool_result]" + strings.Repeat("file entry\n", 600), ToolCallID: "tc_inc_1"},
			{Role: schema.RoleAssistant, Content: strings.Repeat("服务器代码已创建。", 40)},
			{Role: schema.RoleUser, Content: strings.Repeat("看起来不错。", 40)},
			{Role: schema.RoleAssistant, Content: strings.Repeat("接下来添加健康检查端点。", 30)},
			// tail
			{Role: schema.RoleUser, Content: "tail1"},
			{Role: schema.RoleAssistant, Content: "tail2"},
		}

		c.CompactWithRecord(msgs)

		if len(mock.calls) != 2 {
			t.Fatalf("Phase3: expected 2 total LLM calls, got %d", len(mock.calls))
		}

		// 验证第二次调用使用增量模板（包含 <previous-compaction>）
		var userPrompt string
		for _, m := range mock.calls[1] {
			if m.Role == schema.RoleUser {
				userPrompt = m.Content
			}
		}
		if !strings.Contains(userPrompt, "<previous-compaction>") {
			t.Error("Phase3: incremental prompt should contain <previous-compaction> tag")
		}
	})

	t.Run("Phase4_RecordPersistence", func(t *testing.T) {
		// 验证 JSONL 文件包含压缩记录
		records, err := recordStore.List(sessionID)
		if err != nil {
			t.Fatalf("Phase4: List failed: %v", err)
		}
		if len(records) < 2 {
			t.Fatalf("Phase4: want at least 2 records (Phase1 + Phase2 + Phase3), got %d", len(records))
		}

		// 验证记录包含正确的 tier
		hasWarn := false
		hasFull := false
		for _, r := range records {
			if r.Tier == memory.TierWarn {
				hasWarn = true
			}
			if r.Tier == memory.TierFull {
				hasFull = true
			}
			if r.ID == "" {
				t.Error("Phase4: record ID should not be empty")
			}
			if r.Timestamp.IsZero() {
				t.Error("Phase4: record timestamp should not be zero")
			}
		}
		if !hasWarn {
			t.Error("Phase4: should have at least 1 TierWarn record")
		}
		if !hasFull {
			t.Error("Phase4: should have at least 1 TierFull record")
		}
	})

	t.Run("Phase5_OffloadFilesExist", func(t *testing.T) {
		// 验证所有 offload 文件真实存在于文件系统
		records, _ := recordStore.List(sessionID)
		for _, r := range records {
			for _, entry := range r.Offloaded {
				absPath := filepath.Join(workDir, entry.FilePath)
				data, err := os.ReadFile(absPath)
				if err != nil {
					t.Errorf("Phase5: offloaded file should exist at %s: %v", absPath, err)
					continue
				}
				if len(data) == 0 {
					t.Errorf("Phase5: offloaded file %s should not be empty", absPath)
				}
			}
		}
	})
}

// TestProgressiveCompactor_E2E_LLMFailureFallback 验证 LLM 失败时的回退行为：
// TierFull 的 LLM 调用失败 → 回退到 TokenBudgetCompactor → record.Error 非空
func TestProgressiveCompactor_E2E_LLMFailureFallback(t *testing.T) {
	// mockSummarizer 返回错误
	mock := &mockSummarizer{
		errs: []error{fmt.Errorf("LLM service unavailable")},
	}

	c := memory.NewProgressiveCompactor(mock, 3000)
	c.MinTailMessages = 2
	fb := memory.NewTokenBudgetCompactor(3000)
	fb.MinTailMessages = 2
	c.Fallback = fb

	msgs := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: longContent(2500)},
		{Role: schema.RoleAssistant, Content: longContent(2500)},
		{Role: schema.RoleUser, Content: longContent(2500)},
		{Role: schema.RoleAssistant, Content: longContent(2500)},
		{Role: schema.RoleUser, Content: "tail1"},
		{Role: schema.RoleAssistant, Content: "tail2"},
	}

	result, record := c.CompactWithRecord(msgs)

	if record.Error == "" {
		t.Error("record should have error set when LLM fails")
	}
	if !strings.Contains(record.Error, "LLM summary failed") {
		t.Errorf("error should mention LLM failure, got: %q", record.Error)
	}
	if result[0].Role != schema.RoleSystem {
		t.Error("result must start with system message after fallback")
	}
	// Fallback should have reduced messages
	if len(result) >= len(msgs) {
		t.Error("fallback should have reduced message count")
	}
}

// TestProgressiveCompactor_E2E_EmergencyTruncation 验证 TierEmergency 的强制截断行为
func TestProgressiveCompactor_E2E_EmergencyTruncation(t *testing.T) {
	mock := &mockSummarizer{}

	c := memory.NewProgressiveCompactor(mock, 1000)
	c.MinTailMessages = 1
	c.Fallback = memory.NewTokenBudgetCompactor(100)

	msgs := []schema.Message{
		{Role: schema.RoleSystem, Content: "sys"},
		{Role: schema.RoleUser, Content: longContent(4000)},
		{Role: schema.RoleAssistant, Content: longContent(4000)},
		{Role: schema.RoleUser, Content: "tail"},
	}

	result, record := c.CompactWithRecord(msgs)

	if record.Tier != memory.TierEmergency {
		t.Errorf("want TierEmergency, got %d", record.Tier)
	}
	if len(mock.calls) != 0 {
		t.Error("LLM should not be called for Emergency")
	}
	if record.Error == "" {
		t.Error("emergency record should have error message")
	}
	if result[0].Role != schema.RoleSystem {
		t.Error("result must start with system")
	}
	// Emergency should drastically reduce messages
	if len(result) > 3 {
		t.Errorf("emergency should reduce to <= 3 messages, got %d", len(result))
	}
}
