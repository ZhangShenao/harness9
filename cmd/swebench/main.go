// Package main 实现 SWE-bench Lite benchmark runner，
// 用于评估 harness9 在真实 GitHub Issue 修复任务上的 Agent 能力。
//
// 用法：
//
//	go run ./cmd/swebench --dataset swe-bench-lite.jsonl --sample 10 --output ./results
//
// 环境变量（可通过 .env 文件或系统环境变量提供）：
//
//	OPENAI_API_KEY        LLM Provider API Key（必填）
//	OPENAI_BASE_URL       自定义 OpenAI 兼容 API 地址（可选，用于 OpenRouter / Azure 等）
//	LLM_MODEL             模型名称（默认：openai/gpt-4o-mini）
//	SANDBOX_IMAGE         Docker 镜像（默认 python:3.11；高保真可设为官方每实例镜像
//	                      swebench/sweb.eval.x86_64.<instance>，仓库与依赖已预装）
//	SANDBOX_BOOTSTRAP_CMD 容器就绪后、Agent 启动前执行的依赖安装命令（默认自举：
//	                      ensurepip + pip install -e . + pytest）。设置后覆盖默认自举。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/harness9/internal/env"
	"github.com/harness9/internal/sandbox"
)

func main() {
	// 从当前工作目录加载 .env 文件（系统环境变量优先，与 cmd/harness9 保持一致）
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "获取工作目录失败: %v\n", err)
		os.Exit(1)
	}
	if err := env.Load(filepath.Join(cwd, ".env")); err != nil {
		fmt.Fprintf(os.Stderr, "加载环境配置失败: %v\n", err)
		os.Exit(1)
	}

	cfg := Config{}
	flag.StringVar(&cfg.DatasetPath, "dataset", "", "SWE-bench Lite JSONL 文件路径（必填）")
	flag.IntVar(&cfg.SampleN, "sample", 10, "每个 repo 抽取的 instance 数量")
	flag.StringVar(&cfg.OutputDir, "output", "./swebench-results", "输出目录")
	flag.IntVar(&cfg.MaxTurns, "max-turns", 0, "每个 instance 最大 LLM Turn 数（0 = 沿用引擎默认值 500）")
	flag.IntVar(&cfg.Parallel, "parallel", 1, "并发 instance 数")
	flag.BoolVar(&cfg.Resume, "resume", false, "跳过已有非空结果的 instance（断点续跑）")
	flag.IntVar(&cfg.TimeoutMins, "timeout", 30, "单个 instance 超时（分钟）")
	flag.StringVar(&cfg.Model, "model", "", "LLM 模型名称（默认使用 LLM_MODEL 环境变量）")
	flag.Int64Var(&cfg.Seed, "seed", 1, "按 repo 采样的随机种子（固定默认值保证可复现；同 seed → 同实例集）")
	flag.Parse()

	if cfg.DatasetPath == "" {
		fmt.Fprintln(os.Stderr, "错误：--dataset 必填")
		flag.Usage()
		os.Exit(1)
	}

	// Preflight checks
	if err := preflight(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "启动检查失败: %v\n", err)
		os.Exit(1)
	}

	// 清理上次崩溃遗留的孤儿容器（包括 Running 状态）
	// 孤儿容器持有已删除 tmpDir 的 bind mount，在 macOS Docker Desktop 上
	// 会导致 VirtioFS 混乱，使新容器启动超时。
	{
		reapCtx, reapCancel := context.WithTimeout(context.Background(), 30*time.Second)
		mgr := sandbox.NewManager(sandbox.DefaultConfig())
		if err := mgr.ReapOrphans(reapCtx); err != nil {
			fmt.Fprintf(os.Stderr, "警告: 清理孤儿容器失败: %v\n", err)
		}
		reapCancel()
	}

	// 加载数据集
	allInstances, err := loadDataset(cfg.DatasetPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载数据集失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "数据集加载完成：%d 条 instances\n", len(allInstances))

	// 解析实际使用的模型名（用于填写 predictions.jsonl 的 model_name_or_path 字段）
	modelName := resolveModelName(cfg.Model)
	fmt.Fprintf(os.Stderr, "使用模型: %s\n", modelName)

	// 按 repo 采样（固定 seed → 可复现；--resume 时同 seed 自然复现同一实例集）
	instances := sampleByRepo(allInstances, cfg.SampleN, cfg.Seed)
	fmt.Fprintf(os.Stderr, "采样完成：%d 条（每 repo 最多 %d 条，seed=%d）\n", len(instances), cfg.SampleN, cfg.Seed)

	// 加载已有结果（--resume 模式）
	predictionsPath := filepath.Join(cfg.OutputDir, "predictions.jsonl")
	skipIDs := make(map[string]bool)
	if cfg.Resume {
		skipIDs, err = loadExistingIDs(predictionsPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取已有结果失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "断点续跑：跳过 %d 个已有结果\n", len(skipIDs))
	}

	// 为本次运行分配 RunID（时间戳），trajectory 日志写入 logs/<RunID>/，
	// 避免多次运行的同名日志互相覆盖、污染后续分析。
	cfg.RunID = time.Now().Format("20060102-150405")
	fmt.Fprintf(os.Stderr, "本次运行 RunID: %s（日志目录 logs/%s/）\n", cfg.RunID, cfg.RunID)

	// 创建输出目录及本次运行的 trajectory 日志子目录
	if err := os.MkdirAll(filepath.Join(cfg.OutputDir, "logs", cfg.RunID), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "创建输出目录失败: %v\n", err)
		os.Exit(1)
	}

	// 信号处理（Ctrl+C 优雅退出）
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\n收到终止信号，等待当前 instance 完成...")
		cancel()
	}()

	// 并发执行
	sem := semaphore.NewWeighted(int64(cfg.Parallel))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var results []RunResult
	start := time.Now()

	for _, inst := range instances {
		if skipIDs[inst.InstanceID] {
			fmt.Fprintf(os.Stderr, "[skip] %s\n", inst.InstanceID)
			continue
		}
		if ctx.Err() != nil {
			break
		}
		if err := sem.Acquire(ctx, 1); err != nil {
			break
		}
		wg.Add(1)
		go func(inst Instance) {
			defer sem.Release(1)
			defer wg.Done()

			fmt.Fprintf(os.Stderr, "[start] %s\n", inst.InstanceID)
			result := runInstance(ctx, inst, cfg)

			mu.Lock()
			results = append(results, result)
			if appendErr := appendPrediction(predictionsPath, Prediction{
				InstanceID:      inst.InstanceID,
				ModelPatch:      result.Patch,
				ModelNameOrPath: modelName,
			}); appendErr != nil {
				fmt.Fprintf(os.Stderr, "[error] 写入 predictions 失败 (%s): %v\n", inst.InstanceID, appendErr)
			}
			mu.Unlock()

			if result.Error != nil {
				fmt.Fprintf(os.Stderr, "[error] %s (%s): %v\n", inst.InstanceID, result.Duration.Round(time.Second), result.Error)
			} else {
				fmt.Fprintf(os.Stderr, "[done]  %s (%s) patch=%d bytes\n", inst.InstanceID, result.Duration.Round(time.Second), len(result.Patch))
			}
		}(inst)
	}
	wg.Wait()

	// 写汇总
	end := time.Now()
	if err := writeSummary(cfg, results, start, end); err != nil {
		fmt.Fprintf(os.Stderr, "写入摘要失败: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "\n完成！结果已写入 %s\n", cfg.OutputDir)
	fmt.Fprintf(os.Stderr, "总实例: %d，耗时: %s\n", len(results), end.Sub(start).Round(time.Second))
}

// preflight 在启动前验证必要条件，任一失败则终止程序。
func preflight(cfg Config) error {
	if cfg.Parallel <= 0 {
		return fmt.Errorf("--parallel 必须 >= 1，当前值：%d", cfg.Parallel)
	}
	if cfg.SampleN <= 0 {
		return fmt.Errorf("--sample 必须 >= 1，当前值：%d", cfg.SampleN)
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		return fmt.Errorf("OPENAI_API_KEY 未配置")
	}
	if _, err := os.Stat(cfg.DatasetPath); err != nil {
		return fmt.Errorf("dataset 文件不可读：%w", err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git 命令不可用：%w", err)
	}
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer checkCancel()
	if out, err := exec.CommandContext(checkCtx, "docker", "info").CombinedOutput(); err != nil {
		return fmt.Errorf("Docker daemon 不可达: %w\n%s", err, out)
	}
	parent := filepath.Dir(cfg.OutputDir)
	if _, err := os.Stat(parent); err != nil {
		return fmt.Errorf("输出目录的父路径不存在：%w", err)
	}
	return nil
}
