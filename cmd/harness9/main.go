// Package main 是 harness9 agent 二进制的入口点。它组装核心组件 —
// 环境配置加载、LLM Provider、Tool Registry 和 Agent Engine — 并启动 agent loop。
package main

import (
	"context"
	"log"
	"os"
	"path/filepath"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/env"
	"github.com/harness9/internal/provider"
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

	p, err := provider.NewOpenAIProvider("openai/gpt-5.4-mini")
	if err != nil {
		log.Fatalf("[main] 创建 Provider 失败: %v", err)
	}

	r := tools.NewMockRegistry()

	eng := engine.NewAgentEngine(p, r, workDir, false)

	err = eng.Run(context.Background(), "我今天想去北京旅游，帮我看看天气合适吗？")
	if err != nil {
		log.Fatalf("[main] 引擎异常退出: %v", err)
	}
}
