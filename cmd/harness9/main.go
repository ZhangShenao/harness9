// Package main 是 harness9 agent 二进制的入口点（Entry Point）。
// 它按照依赖顺序组装核心组件 — 环境配置加载、LLM Provider、Tool Registry 和 Agent Engine —
// 并启动 Agent Loop 执行用户任务。
//
// 启动流程：
//
//	1. 确定物理工作路径（WorkDir）
//	2. 从 .env 文件加载环境变量（API Key、Base URL 等）
//	3. 创建 LLM Provider（模型后端适配器）
//	4. 创建 Tool Registry 并注册内置工具
//	5. 组装 Agent Engine 并启动主循环
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
	// Step 1: 获取物理工作路径，作为 agent 的操作沙箱（Sandbox）边界。
	// 所有工具（如文件读取）的访问范围被限制在此目录树内。
	workDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("[main] 获取工作目录失败: %v", err)
	}

	// Step 2: 加载 .env 环境变量配置。
	// 从项目根目录读取 .env 文件，提取 API Key 和 Base URL 等敏感配置。
	// 已存在的系统环境变量不会被覆盖（系统级 > 文件级）。
	if err := env.Load(filepath.Join(workDir, ".env")); err != nil {
		log.Fatalf("[main] 加载环境配置失败: %v", err)
	}

	// Step 3: 初始化 LLM Provider（大模型后端适配器）。
	// 当前使用 OpenAI 兼容端点，通过 OPENAI_API_KEY 和 OPENAI_BASE_URL
	// 环境变量配置认证和端点，可灵活对接 OpenAI / Azure / OpenRouter 等服务。
	llm, err := provider.NewOpenAIProvider("openai/gpt-5.4-mini")
	if err != nil {
		log.Fatalf("[main] 创建 Provider 失败: %v", err)
	}

	// Step 4: 创建工具注册表（Tool Registry）并注册内置工具。
	// Registry 是引擎与工具系统之间的桥梁，负责工具发现与执行分发。
	registry := tools.NewRegistry()

	// 注册 ReadFile 工具：提供受限工作区内的安全文件读取能力。
	readFileTool := tools.NewReadFileTool(workDir)
	registry.Register(readFileTool)

	// Step 5: 创建 Agent Engine（核心编排器）。
	// 参数 enableThinking=false 表示使用标准单阶段 ReAct 模式。
	// 设置为 true 则启用两阶段 Thinking-Action 模式（慢思考 + 精准行动）。
	eng := engine.NewAgentEngine(llm, registry, workDir, false)

	// Step 6: 构造用户任务并启动 Agent Loop。
	// 设置 5 分钟全局超时，防止 LLM 陷入无限循环消耗 Token。
	prompt := "查看下我当前的工作路径下 poem.txt 文件，总结其核心内容"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	err = eng.Run(ctx, prompt)
	if err != nil {
		log.Fatalf("[main] 引擎异常退出: %v", err)
	}
}
