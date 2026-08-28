package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/logfmt"
	"github.com/harness9/internal/skills"
)

// RunCLI 启动交互式 REPL，从 os.Stdin 读取用户输入。
// idx 不为 nil 时，支持 /skill-name 斜杠命令直接加载技能正文。
// 直到 ctx 取消、用户输入 exit/quit 或 EOF 时返回。
func RunCLI(ctx context.Context, eng *engine.AgentEngine, idx *skills.Index) {
	runCLI(ctx, eng, os.Stdin, idx)
}

// RunOnce 从指定文件读取完整 prompt，非交互执行一次后返回，用于评测/CI 场景（--prompt-file）。
// 与 RunCLI 的逐行 REPL 不同，文件全部内容作为单次 userPrompt 传给 eng.Run，
// 避免多行任务指令被误拆成多个独立 Turn。
func RunOnce(ctx context.Context, eng *engine.AgentEngine, promptFilePath string) error {
	data, err := os.ReadFile(promptFilePath)
	if err != nil {
		return fmt.Errorf("读取 prompt 文件失败：%w", err)
	}
	return eng.Run(ctx, string(data))
}

// runCLI 是 RunCLI 的可测试内核，允许注入任意 io.Reader 作为输入源。
func runCLI(ctx context.Context, eng *engine.AgentEngine, in io.Reader, idx *skills.Index) {
	fmt.Println("harness9 │ 输入 \"exit\" 或按 Ctrl-C 退出")
	fmt.Println()

	reader := bufio.NewReader(in)
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\n再见！")
			return
		default:
		}

		fmt.Print("harness9> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			// EOF 或管道关闭，正常退出
			fmt.Println("\n再见！")
			return
		}

		input := strings.TrimSpace(line)
		if input == "" {
			continue
		}
		if input == "exit" || input == "quit" {
			fmt.Println("再见！")
			return
		}

		prompt, ok := resolvePrompt(input, idx)
		if !ok {
			continue
		}

		if err := eng.Run(ctx, prompt); err != nil {
			if ctx.Err() != nil {
				fmt.Println("\n再见！")
				return
			}
			fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		}
	}
}

// resolvePrompt 处理斜杠命令和普通输入，返回最终传给 LLM 的 prompt。
// 返回 false 表示本次输入已处理完毕（如斜杠命令找不到技能），不需要调用 LLM。
func resolvePrompt(input string, idx *skills.Index) (prompt string, ok bool) {
	if !strings.HasPrefix(input, "/") || idx == nil {
		return input, true
	}

	// 分割 "/skill-name [可选附加文本]"
	rest := strings.TrimPrefix(input, "/")
	name, extra, _ := strings.Cut(rest, " ")
	extra = strings.TrimSpace(extra)

	body, err := idx.GetFullContent(name)
	if err != nil {
		log.Print(logfmt.FormatMsg("skills", fmt.Sprintf("激活失败: %v", err)))
		return "", false
	}

	log.Print(logfmt.FormatMsg("skills", fmt.Sprintf("激活技能: %s (斜杠命令)", name)))

	if extra == "" {
		return body, true
	}
	return body + "\n\n" + extra, true
}
