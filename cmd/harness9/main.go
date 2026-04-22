// Package main 是 harness9 agent 二进制的入口点。它组装核心组件 —
// LLM Provider、Tool Registry 和 Agent Engine — 并以示例 prompt 启动 agent loop。
package main

import (
	"context"
	"log"
	"os"

	"github.com/harness9/internal/engine"
	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/tools"
)

func main() {
	workDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("[main] 获取工作目录失败: %v", err)
	}

	p := provider.NewMockProvider()
	r := tools.NewMockRegistry()

	eng := engine.NewAgentEngine(p, r, workDir, true)

	err = eng.Run(context.Background(), "帮我检查当前目录的文件")
	if err != nil {
		log.Fatalf("[main] 引擎异常退出: %v", err)
	}
}
