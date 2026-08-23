// Package logfmt 提供 harness9 项目内共享的"块状日志"（Block-Style Log）渲染工具。
//
// 设计目标：让分散在不同模块（engine、provider 等）的日志保持统一的视觉风格 ——
// 首行单行头部便于 grep，续行以固定缩进对齐形成"信息块"。
//
// # 视觉风格示例
//
//	[engine] Turn 1 │ 工具完成 │ tool=bash id=call_xyz status=ok duration=1.2s bytes=1305 (truncated to 512)
//	        output:
//	        │ go version go1.25.3 darwin/arm64
//	        │ /Users/zsa/Desktop/harness/harness9
//
// # 公共 API 总览
//
//   - FormatMsg                        — 通用单行日志条目
//   - FormatLoopStart / FormatLoopEnd  — agent loop 启动 / 结束
//   - FormatTurnStart / FormatTurnDone — agent loop 每个 Turn 的头部 / 完成
//   - FormatObservation                — Observation 注入完成
//   - FormatParallelTools              — 并行工具执行开始
//   - FormatToolStart / FormatToolDone — 单个工具调用启动 / 完成（含多行 output 块）
//   - FormatJSON                       — 把 json.RawMessage 渲染为内联/pretty-print 形式
//   - FormatOutput                     — 把任意字符串渲染为 "│ " 前缀的多行块
//   - MaxOutputLen / Indent            — 输出长度上限与续行缩进常量
//
// # 风格一致性原则
//
// 其他模块在编写多行日志时，应优先使用 FormatJSON / FormatOutput 这类底层 helper，
// 并保持 Indent 作为续行缩进，避免引入第二种视觉风格。
package logfmt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/harness9/internal/schema"
)

// MaxOutputLen 日志中单条输出的最大字节数。超出部分被截断并附加提示。
// 防止单条工具/系统输出撑爆日志文件或终端缓冲区。
const MaxOutputLen = 512

// Indent 块状日志续行（Continuation Line）的统一缩进。
// 其他模块若编写多行日志，应使用此常量保持视觉对齐。
const Indent = "        "

// argInlineThreshold 当 JSON 压缩后长度小于该阈值时以单行内联展示；否则切换为多行 pretty-print。
const argInlineThreshold = 80

// FormatToolStart 渲染"工具启动"日志条目。
//
// 参数：
//   - logPrefix: 调用方日志前缀（如 "engine" / "engine-stream"），与所属循环对齐。
//   - turn:      当前 Turn 编号。
//   - tc:        工具调用请求（含 Name、ID、Arguments）。
//
// 输出形态示例（短 arguments，单行内联）：
//
//	[engine] Turn 1 │ 工具启动 │ tool=bash id=call_xyz
//	        arguments: {"command":"go version"}
//
// 输出形态示例（长 arguments，pretty-print）：
//
//	[engine] Turn 1 │ 工具启动 │ tool=write_file id=call_xyz
//	        arguments:
//	          {
//	            "path": "src/main.go",
//	            "content": "package main\n..."
//	          }
func FormatToolStart(logPrefix string, turn int, tc schema.ToolCall) string {
	header := fmt.Sprintf("[%s] Turn %d │ 工具启动 │ tool=%s id=%s",
		logPrefix, turn, tc.Name, tc.ID)
	args := FormatJSON(tc.Arguments)
	if strings.Contains(args, "\n") {
		return header + "\n" + Indent + "arguments:\n" + args
	}
	return header + "\n" + Indent + "arguments: " + args
}

// FormatToolDone 渲染"工具完成 / 工具失败"日志条目。
//
// 参数：
//   - logPrefix: 调用方日志前缀。
//   - turn:      当前 Turn 编号。
//   - tc:        原始工具调用请求。
//   - result:    工具执行结果（含 Output、IsError）。
//   - d:         工具执行耗时（如 "1.2s"、"50ms"）。
//
// 输出形态示例（成功）：
//
//	[engine] Turn 1 │ 工具完成 │ tool=bash id=call_xyz status=ok duration=1.2s bytes=1305 (truncated to 512)
//	        output:
//	        │ go version go1.25.3 darwin/arm64
//	        │ /Users/zsa/Desktop/harness/harness9
//
// 输出形态示例（失败）：
//
//	[engine] Turn 1 │ 工具失败 │ tool=bash id=call_xyz status=error duration=5s bytes=42
//	        output:
//	        │ command not found: foo
func FormatToolDone(logPrefix string, turn int, tc schema.ToolCall, result schema.ToolResult, d time.Duration) string {
	phaseLabel := "工具完成"
	status := "ok"
	if result.IsError {
		phaseLabel = "工具失败"
		status = "error"
	}

	body, total, truncated := FormatOutput(result.Output)
	truncSuffix := ""
	if truncated {
		truncSuffix = fmt.Sprintf(" (truncated to %d)", MaxOutputLen)
	}

	return fmt.Sprintf(
		"[%s] Turn %d │ %s │ tool=%s id=%s status=%s duration=%s bytes=%d%s\n%soutput:\n%s",
		logPrefix, turn, phaseLabel, tc.Name, tc.ID, status, d, total, truncSuffix, Indent, body,
	)
}

// FormatJSON 把原始 JSON 渲染为人类友好的日志字符串。
//
// 行为：
//   - 空输入 → "{}"
//   - 短 payload (≤ argInlineThreshold 字节) → 单行内联展示
//   - 长 payload → pretty-print 多行，每行前补统一 Indent
//
// 注意：Go 的 encoding/json 默认会将 &、<、> 等转义为 & 等 Unicode 形式
// （HTML-Escape），日志中阅读极不友好。本函数显式关闭该行为，让命令字符串
// （如 `go version && pwd`）原样可读。
func FormatJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}

	// Compact 阶段同样需关闭 HTML escape，否则短 payload 也会出现 &。
	// 注意：json.Encoder.Encode 总会附加一个尾随换行符，需手动 TrimRight。
	var compact bytes.Buffer
	if err := encodeJSONNoEscape(&compact, raw, false); err != nil {
		return string(raw)
	}
	compactStr := strings.TrimRight(compact.String(), "\n")
	if len(compactStr) <= argInlineThreshold {
		return compactStr
	}

	var pretty bytes.Buffer
	if err := encodeJSONNoEscape(&pretty, raw, true); err != nil {
		return compactStr
	}
	// json.Encoder 的 Indent 不会缩进首行，需手动补齐
	indented := strings.ReplaceAll(strings.TrimRight(pretty.String(), "\n"), "\n", "\n"+Indent+"  ")
	return Indent + "  " + indented
}

// FormatOutput 将任意字符串渲染为多行块状文本（Block-Style）：
// 超出 MaxOutputLen 时截断；每行以 "│ " 起头并对齐到 Indent，便于扫读。
//
// 参数：
//   - s: 原始字符串内容（如工具 stdout、文件片段等）。
//
// 返回值：
//   - body:      格式化后的多行文本（首行已含 Indent 前缀）
//   - total:     原始输入的总字节数（截断前）
//   - truncated: 是否发生过截断
func FormatOutput(s string) (body string, total int, truncated bool) {
	total = len(s)
	if total > MaxOutputLen {
		s = truncateUTF8(s, MaxOutputLen)
		truncated = true
	}
	if s == "" {
		return Indent + "│ <empty>", total, truncated
	}

	// 去掉末尾多余换行，避免日志中出现孤立的 "│ " 空行
	s = strings.TrimRight(s, "\n")

	lines := strings.Split(s, "\n")
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(Indent)
		b.WriteString("│ ")
		b.WriteString(line)
	}
	return b.String(), total, truncated
}

// truncateUTF8 按字节截断 s 至 maxBytes 以内，并在截断点回退到最近的合法 UTF-8 rune 边界，
// 避免把中文等多字节字符从中间切开、在日志中产生无效 UTF-8 乱码。
// 与 cmd/harness9 的 truncateUTF8 采用相同的回退策略：对被截断的多字节字符回退 ≤3 字节；
// 若输入本身含连续非法字节（二进制垃圾），会持续剥离直到遇到合法边界或清空。
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	s = s[:maxBytes]
	// DecodeLastRuneInString 返回 (RuneError, 1) 表示末尾是不完整序列的残缺字节 → 剥离；
	// 注意不能用 RuneStart 判断——被截断序列的首字节（如孤立的 \xe8）同样是"起始字节"。
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// FormatMsg 渲染通用单行日志条目：[prefix] msg。
func FormatMsg(prefix, msg string) string {
	return fmt.Sprintf("[%s] %s", prefix, msg)
}

// FormatLoopStart 渲染 agent loop 启动日志（含配置参数）。
func FormatLoopStart(prefix, workDir string, maxTurns int, toolTimeout time.Duration, maxConcurrent int) string {
	return fmt.Sprintf("[%s] 启动 │ workdir=%s maxTurns=%d toolTimeout=%v maxConcurrent=%d",
		prefix, workDir, maxTurns, toolTimeout, maxConcurrent)
}

// FormatTurnStart 渲染每个 Turn 的头部日志。
func FormatTurnStart(prefix string, turn, historyLen, toolsLen int) string {
	return fmt.Sprintf("[%s] Turn %d │ history=%d tools=%d", prefix, turn, historyLen, toolsLen)
}

// FormatTurnDone 渲染 Turn 无工具调用时的任务完成日志。
func FormatTurnDone(prefix string, turn int, llmDuration, totalDuration time.Duration) string {
	return fmt.Sprintf("[%s] Turn %d │ 任务完成 │ llm=%s total=%s",
		prefix, turn, llmDuration, totalDuration)
}

// FormatObservation 渲染工具执行结束后的 Observation 注入日志。
func FormatObservation(prefix string, turn, historyLen int, llmDuration, toolsDuration, turnDuration time.Duration) string {
	return fmt.Sprintf("[%s] Turn %d │ Observation │ history=%d llm=%s tools=%s turn=%s",
		prefix, turn, historyLen, llmDuration, toolsDuration, turnDuration)
}

// FormatLoopEnd 渲染 agent loop 正常结束日志。
func FormatLoopEnd(prefix string, turns int, totalDuration time.Duration) string {
	return fmt.Sprintf("[%s] 循环结束 │ turns=%d total=%s", prefix, turns, totalDuration)
}

// FormatParallelTools 渲染并行工具执行开始日志。
func FormatParallelTools(prefix string, turn, count, maxConcurrent int) string {
	return fmt.Sprintf("[%s] Turn %d │ 并行执行 %d 个工具 │ maxConcurrent=%d",
		prefix, turn, count, maxConcurrent)
}

// encodeJSONNoEscape 使用 json.Encoder 重新编码原始 JSON，关闭 HTML 字符转义
// （SetEscapeHTML(false)），可选启用 indent。indent=true 时使用两个空格缩进。
func encodeJSONNoEscape(buf *bytes.Buffer, raw json.RawMessage, indent bool) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(v)
}
