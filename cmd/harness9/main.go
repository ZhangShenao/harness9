package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/env"
	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/schema"
	"github.com/harness9/internal/tools"
)

func main() {
	workDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("[main] 获取工作目录失败: %v", err)
	}

	if err := env.Load(filepath.Join(workDir, ".env")); err != nil {
		log.Fatalf("[main] 加载环境配置失败: %v", err)
	}

	llm, err := provider.NewOpenAIProvider("openai/gpt-5.4-mini")
	if err != nil {
		log.Fatalf("[main] 创建 Provider 失败: %v", err)
	}

	registry := tools.NewRegistry()
	readFileTool := tools.NewReadFileTool(workDir)
	registry.Register(readFileTool)

	eng := engine.NewAgentEngine(llm, registry, workDir, false)

	prompt := "查看下我当前的工作路径下 poem.txt 文件，总结其核心内容"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	useStream := len(os.Args) > 1 && os.Args[1] == "stream"

	if useStream {
		fmt.Println("=== 流式调用模式 ===")
		runStream(ctx, eng, prompt)
	} else {
		fmt.Println("=== 阻塞式调用模式 ===")
		runBlocking(ctx, eng, prompt)
	}
}

func runBlocking(ctx context.Context, eng *engine.AgentEngine, prompt string) {
	if err := eng.Run(ctx, prompt); err != nil {
		log.Fatalf("[main] 引擎异常退出: %v", err)
	}
}

func runStream(ctx context.Context, eng *engine.AgentEngine, prompt string) {
	stream, err := eng.RunStream(ctx, prompt)
	if err != nil {
		log.Fatalf("[main] RunStream 启动失败: %v", err)
	}

	for evt := range stream {
		switch evt.Type {
		case engine.EventThinkingDelta:
			fmt.Print(evt.Data.(string))
		case engine.EventActionDelta:
			fmt.Print(evt.Data.(string))
		case engine.EventToolStart:
			if tc, ok := evt.Data.(schema.ToolCall); ok {
				fmt.Printf("\n[tool-start] %s (%s)\n", tc.Name, tc.ID)
			}
		case engine.EventToolResult:
			if tr, ok := evt.Data.(schema.ToolResult); ok {
				fmt.Printf("\n[tool-result] %s\n", truncStr(tr.Output, 200))
			}
		case engine.EventDone:
			fmt.Println("\n[done]")
		case engine.EventError:
			fmt.Printf("\n[error] %v\n", evt.Data)
		}
	}
}

func truncStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
