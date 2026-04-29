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
	// 绑定工作路径
	workDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("[main] 获取工作目录失败: %v", err)
	}

	// 加载环境变量
	if err := env.Load(filepath.Join(workDir, ".env")); err != nil {
		log.Fatalf("[main] 加载环境配置失败: %v", err)
	}

	// 指定 LLMProvider
	llm, err := provider.NewOpenAIProvider("openai/gpt-5.4-mini")
	if err != nil {
		log.Fatalf("[main] 创建 Provider 失败: %v", err)
	}

	// 创建ToolRegistry并注册Tools
	registry := tools.NewRegistry()
	registry.Register(tools.NewReadFileTool(workDir))
	registry.Register(tools.NewWriteFileTool(workDir))
	registry.Register(tools.NewBashTool(workDir))

	// 创建Agent Engine，并关闭慢思考模式
	eng := engine.NewAgentEngine(llm, registry, workDir, false)

	prompt := `
	帮我完成以下几件事情：
	1. 通过控制台命令，查看我本地Go语言的具体版本
	2. 使用Go语言，编写一个简单的 hello.go 脚本，打印字符串 “hello harness9!”
	3. 编译脚本，并实际执行，确认输出结果
	`
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
