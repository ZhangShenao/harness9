// Package main 是 harness9 agent 二进制的入口点。它组装核心组件 —
// 环境配置加载、LLM Provider、Tool Registry 和 Agent Engine — 并启动 agent loop。
package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/env"
	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/tools"
)

func main() {
	// 获取物理工作路径
	workDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("[main] 获取工作目录失败: %v", err)
	}

	// 加载环境变量
	if err := env.Load(filepath.Join(workDir, ".env")); err != nil {
		log.Fatalf("[main] 加载环境配置失败: %v", err)
	}

	// 初始化 LLM Provider
	llm, err := provider.NewOpenAIProvider("openai/gpt-5.4-mini")
	if err != nil {
		log.Fatalf("[main] 创建 Provider 失败: %v", err)
	}

	// 创建Tool Registry
	registry := tools.NewRegistry()

	// 创建ReadFile工具，并注册到Registry
	readFileTool := tools.NewReadFileTool(workDir)
	registry.Register(readFileTool)

	// 创建 Agent Engine
	eng := engine.NewAgentEngine(llm, registry, workDir, false)

	// 执行 Agent
	prompt := "查看下我当前的工作路径下 poem.txt 文件，总结其核心内容"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	err = eng.Run(ctx, prompt)
	if err != nil {
		log.Fatalf("[main] 引擎异常退出: %v", err)
	}
}
