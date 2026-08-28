// Package main 实现 SWE-bench Lite benchmark runner，
// 用于评估 harness9 在真实 GitHub Issue 修复任务上的 Agent 能力。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"time"
)

// Instance 是 SWE-bench Lite 数据集的一条记录（JSONL 格式）。
type Instance struct {
	InstanceID       string `json:"instance_id"`
	Repo             string `json:"repo"`
	BaseCommit       string `json:"base_commit"`
	ProblemStatement string `json:"problem_statement"`
	HintsText        string `json:"hints_text"`
	// Version / EnvironmentSetupCommit 用于为每实例 provision 正确的运行环境
	// （选对 Python 版本、定位安装规范）；接入官方每实例镜像时按 Version 选 tag。
	Version                string `json:"version"`
	EnvironmentSetupCommit string `json:"environment_setup_commit"`
	// FailToPass / PassToPass 是评测目标测试的 JSON 编码字符串（形如 `["a::b"]`），
	// 用 parseTestIDs 解析为 []string。⚠️ 这些测试由 test_patch 在评测阶段注入，
	// 不在 Agent 运行时的工作区中——仅用于运行后分析/调试，**绝不**在 Agent 运行时暴露或应用，
	// 以保持 SWE-bench 评测协议（隐藏测试）不被污染。
	FailToPass string `json:"FAIL_TO_PASS"`
	PassToPass string `json:"PASS_TO_PASS"`
	// TestPatch 是评测阶段注入的隐藏测试补丁。同样**绝不**在 Agent 运行时应用。
	TestPatch string `json:"test_patch"`
}

// parseTestIDs 把 SWE-bench 中"JSON 编码为字符串的测试 ID 数组"（如 `["a::b","c::d"]`）
// 解析为 []string。空字符串、空数组或非法 JSON 均返回 nil（容错，调用方据空判断不可用）。
func parseTestIDs(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(s), &ids); err != nil {
		return nil
	}
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// Prediction 是写入 predictions.jsonl 的一条记录（官方兼容格式）。
// 官方评估器 swebench.harness.run_evaluation 要求同时携带 model_name_or_path 字段。
type Prediction struct {
	InstanceID      string `json:"instance_id"`
	ModelPatch      string `json:"model_patch"`
	ModelNameOrPath string `json:"model_name_or_path"`
}

// RunResult 记录单个 instance 的运行结果，供汇总使用。
type RunResult struct {
	Instance Instance
	Patch    string
	Error    error
	Duration time.Duration
}

// loadDataset 从 JSONL 文件加载所有 instance。
func loadDataset(path string) ([]Instance, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开数据集失败：%w", err)
	}
	defer f.Close()

	var instances []Instance
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 10*1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var inst Instance
		if err := json.Unmarshal([]byte(line), &inst); err != nil {
			return nil, fmt.Errorf("解析 JSONL 行失败：%w", err)
		}
		instances = append(instances, inst)
	}
	return instances, scanner.Err()
}

// sampleByRepo 按 repo 分组，每组随机取最多 n 条，打乱后返回。
func sampleByRepo(instances []Instance, n int, seed int64) []Instance {
	byRepo := make(map[string][]Instance)
	for _, inst := range instances {
		byRepo[inst.Repo] = append(byRepo[inst.Repo], inst)
	}

	// 对 repo 名排序，确保相同 seed 产生相同输出（Go map 遍历非确定性）
	repos := make([]string, 0, len(byRepo))
	for repo := range byRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)

	rng := rand.New(rand.NewSource(seed))

	var sampled []Instance
	for _, repo := range repos {
		group := byRepo[repo]
		rng.Shuffle(len(group), func(i, j int) { group[i], group[j] = group[j], group[i] })
		if len(group) > n {
			group = group[:n]
		}
		sampled = append(sampled, group...)
	}
	rng.Shuffle(len(sampled), func(i, j int) { sampled[i], sampled[j] = sampled[j], sampled[i] })
	return sampled
}
