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
	// 解析当前工作目录，作为 agent 的工作区根路径，
	// 注入到 system prompt 中使 LLM 了解其操作上下文。
	workDir, _ := os.Getwd()

	// 初始化 LLM Provider。当前使用 Mock Provider 进行开发和测试，
	// 生产环境应替换为真实 Provider（如 OpenAI、Anthropic）。
	p := provider.NewMockProvider()

	// 初始化 Tool Registry。Mock Registry 返回硬编码结果而不执行真实命令，
	// 生产环境应替换为真实 Registry。
	r := tools.NewMockRegistry()

	// 组装 Agent Engine，注入 Provider、Registry 和工作区路径。
	eng := engine.NewAgentEngine(p, r, workDir)

	// 以用户 prompt 启动 agent loop。引擎将执行 Reasoning-ToolCall-Observation
	// 循环，直到 LLM 产出最终回复。
	err := eng.Run(context.Background(), "帮我检查当前目录的文件")
	if err != nil {
		log.Fatalf("引擎崩溃: %v", err)
	}
}
