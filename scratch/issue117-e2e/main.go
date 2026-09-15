// issue #117 写回式压缩端到端验证驱动（一次性工具，不随修复提交）。
//
// 用真实 LLM 驱动完整 ReAct 循环：人为调小 context window，让真实模型连续 cat
// 多个 5KB 文件制造工具输出洪峰，穿越 Warn(60%)/Soft(70%)/Full(80%) 各档位；
// FileRecordStore 落盘每一次 CompactionRecord，运行后输出审计报告：
//   - 各档位压缩的 tier / tokens before-after / 耗时 / 摘要文本片段
//   - 真实 LLM 调用拆分（主循环 turn 数 vs 压缩摘要调用数）
//   - Session 终态（压缩产物是否落库、消息条数、有无重复 tool_call_id）
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/hooks"
	"github.com/harness9/internal/memory"
	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/tools"
)

// countedProvider 统计真实 LLM 调用：区分主循环调用与压缩摘要调用
// （摘要调用的 system prompt 以 "context compaction engine" 开头）。
type countedProvider struct {
	provider.LLMProvider
	mu           sync.Mutex
	mainCalls    int
	summaryCalls int
}

func (p *countedProvider) record(msgs []schema.Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(msgs) > 0 && msgs[0].Role == schema.RoleSystem &&
		strings.Contains(msgs[0].Content, "context compaction engine") {
		p.summaryCalls++
		return
	}
	p.mainCalls++
}

func (p *countedProvider) Generate(ctx context.Context, msgs []schema.Message, td []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
	p.record(msgs)
	return p.LLMProvider.Generate(ctx, msgs, td)
}

func (p *countedProvider) GenerateStream(ctx context.Context, msgs []schema.Message, td []schema.ToolDefinition) (<-chan schema.StreamChunk, error) {
	p.record(msgs)
	return p.LLMProvider.GenerateStream(ctx, msgs, td)
}

func main() {
	workDir := flag.String("work", "", "Agent 工作目录（内含 f1..f6.txt）")
	dbPath := flag.String("db", "", "SQLite session DB 路径")
	recordsDir := flag.String("records", "", "CompactionRecord JSONL 目录")
	window := flag.Int("window", 5500, "人为调小的 context window（token）")
	label := flag.String("label", "run", "报告标签")
	promptFlag := flag.String("prompt", "", "自定义任务提示词（覆盖默认多文件场景）")
	flag.Parse()
	if *workDir == "" || *dbPath == "" || *recordsDir == "" {
		log.Fatal("需要 -work -db -records 参数")
	}
	log.SetFlags(log.Ltime)

	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()

	model := os.Getenv("LLM_MODEL")
	if model == "" {
		model = "openai/gpt-4o-mini"
	}
	counted := &countedProvider{}
	raw, err := provider.NewFromEnv(model)
	if err != nil {
		log.Fatalf("创建 provider 失败: %v", err)
	}
	counted.LLMProvider = raw

	reg := tools.NewRegistry()
	if err := reg.Register(tools.NewBashTool(*workDir)); err != nil {
		log.Fatalf("注册 bash 失败: %v", err)
	}
	hookReg := hooks.NewHookRegistry(reg)

	mgr, err := memory.NewManager(*dbPath)
	if err != nil {
		log.Fatalf("创建 manager 失败: %v", err)
	}
	defer func() { _ = mgr.Close() }()
	sess, err := mgr.NewSession(ctx)
	if err != nil {
		log.Fatalf("打开 session 失败: %v", err)
	}

	compactor := memory.NewProgressiveCompactor(counted, *window,
		memory.WithProgressiveOffloader(memory.NewCompactionOffloader(*workDir, "e2e117")),
		memory.WithProgressiveRecordStore(memory.NewFileRecordStore(*recordsDir)),
		memory.WithProgressiveSessionID("e2e117"),
	)
	compactor.OffloadThreshold = 3000 // 夹具文件约 3.5KB，调低阈值让 Warn 档也能触发 offload

	eng := engine.NewAgentEngine(counted, hookReg, *workDir,
		engine.WithSession(sess),
		engine.WithContextWindow(*window),
		engine.WithCompactor(compactor),
	)

	prompt := *promptFlag
	if prompt == "" {
		prompt = "请严格按顺序完成以下操作：用 bash 工具的 cat 命令完整逐个读取当前目录下的 f1.txt、f2.txt、f3.txt、f4.txt、f5.txt、f6.txt（每次只读一个，读完简要复述其主题后再读下一个，必须 cat 完整文件）。全部读完后回答两个问题：(1) f1.txt 中的 SECRET-CODE 是什么？(2) 用一句话概括这 6 个文件共同的整体主题。"
	}
	start := time.Now()
	runErr := eng.Run(ctx, prompt)
	elapsed := time.Since(start)
	if runErr != nil {
		log.Printf("Run 出错（继续输出已采集证据）: %v", runErr)
	}

	// ---- 审计报告 ----
	var sb strings.Builder
	w := func(f string, a ...any) { sb.WriteString(fmt.Sprintf(f, a...)); sb.WriteString("\n") }
	w("\n================ E2E 报告 [%s] ================", *label)
	w("模型: %s | context window: %d token | 总耗时: %s", model, *window, elapsed)
	w("真实 LLM 调用：主循环 %d 次 + 压缩摘要 %d 次 = %d 次", counted.mainCalls, counted.summaryCalls, counted.mainCalls+counted.summaryCalls)

	rs := memory.NewFileRecordStore(*recordsDir)
	records, _ := rs.List("e2e117")
	w("\n---- 压缩记录（%d 条，按触发顺序）----", len(records))
	for i, r := range records {
		w("[%d] tier=%d(%s) tokens %d→%d (ratio %.2f) msgs %d→%d 摘要条数=%d 保留尾=%d offload=%d 耗时=%s err=%q",
			i+1, r.Tier, tierName(r.Tier), r.TokensBefore, r.TokensAfter, r.CompressionRatio,
			r.MsgsBefore, r.MsgsAfter, r.Summarized, r.PreservedTail, len(r.Offloaded), r.Duration, r.Error)
		summary := strings.ReplaceAll(r.SummaryText, "\n", " ")
		if len(summary) > 220 {
			summary = summary[:220] + "…"
		}
		if summary != "" {
			w("    摘要: %s", summary)
		}
		for _, o := range r.Offloaded {
			w("    offload: %s (%d bytes / %d 行)", o.FilePath, o.Bytes, o.Lines)
		}
	}

	var finalMsg *schema.Message
	msgs, gerr := sess.GetMessages(context.Background(), 0)
	if gerr != nil {
		w("读取 session 失败: %v", gerr)
	} else {
		w("\n---- Session 终态 ----")
		w("持久化消息总数：%d", len(msgs))
		markerCount, dupTool := 0, 0
		seen := map[string]bool{}
		for _, m := range msgs {
			if strings.Contains(m.Content, "[Context Compaction]") {
				markerCount++
			}
			if m.ToolCallID != "" {
				if seen[m.ToolCallID] {
					dupTool++
				}
				seen[m.ToolCallID] = true
			}
			if m.Role == schema.RoleAssistant {
				last := m
				finalMsg = &last
			}
		}
		w("压缩产物消息：%d 条 | 重复 tool_call_id: %d 个", markerCount, dupTool)
	}

	offloads, _ := filepath.Glob(filepath.Join(*workDir, ".harness9", "tool_results", "e2e117", "*.txt"))
	w("offload 外存文件：%d 个", len(offloads))

	if runErr != nil {
		w("Run 错误: %v", runErr)
	}
	if finalMsg != nil {
		reply := strings.TrimSpace(finalMsg.Content)
		if len(reply) > 600 {
			reply = reply[:600] + "…"
		}
		w("\n---- 最终回复（Session 最后一条 assistant 消息）----\n%s", reply)
		w("最终回复包含 E2E-8371: %v", strings.Contains(finalMsg.Content, "E2E-8371"))
	}

	out := fmt.Sprintf("%s/report-%s.md", *recordsDir, *label)
	if werr := os.WriteFile(out, []byte(sb.String()), 0600); werr != nil {
		log.Fatalf("写报告失败: %v", werr)
	}
	fmt.Print(sb.String())
	fmt.Printf("\n报告已写入 %s\n", out)
}

func tierName(t memory.CompactionTier) string {
	switch t {
	case memory.TierWarn:
		return "Warn"
	case memory.TierSoft:
		return "Soft"
	case memory.TierFull:
		return "Full"
	case memory.TierEmergency:
		return "Emergency"
	default:
		return "None"
	}
}
