# Terminal-Bench (Harbor) Pilot

harness9 接入 [Harbor](https://github.com/harbor-framework/harbor) 评测框架，跑
Terminal-Bench 2.0（`terminal-bench@2.0`，89 题）里精选的 18 个 pilot 任务。
背景与设计见 `docs/技术调研/terminal-bench-集成方案.md`。

## 环境准备

```bash
python3 --version   # 需要 >=3.12
pip install harbor
pip show harbor      # 确认 Home-page 指向 github.com/harbor-framework/harbor，
                      # 排除 PyPI 同名包冲突
docker --version     # Harbor 任务环境依赖 Docker
```

## 构建二进制

```bash
cd /Users/zsa/Desktop/harness/harness9
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build \
  -o benchmarks/terminal_bench/bin/harness9 \
  ./cmd/harness9
```

## 运行 Pilot

```bash
export OPENAI_API_KEY=...
# 可选：export OPENAI_BASE_URL=... （OpenRouter 等兼容端点）
# 可选：export LLM_MODEL=...

cd /Users/zsa/Desktop/harness/harness9
PYTHONPATH=benchmarks harbor run \
  -d terminal-bench@2.0 \
  -a terminal_bench.harness9_agent:Harness9Agent \
  -i fix-git -i git-multibranch -i configure-git-webserver \
  -i git-leak-recovery -i sanitize-git-repo \
  -i build-cython-ext -i build-pmars -i build-pov-ray -i compile-compcert \
  -i custom-memory-heap-crash -i fix-ocaml-gc -i merge-diff-arc-agi-task \
  -i sqlite-db-truncate -i sqlite-with-gcov -i nginx-request-logging \
  -i pypi-server -i kv-store-grpc -i openssl-selfsigned-cert \
  -o benchmarks/terminal_bench/runs
```

## 查看结果

### 真实目录结构（已实测确认，2026-07-21，`fix-git` 单任务 pilot）

```
benchmarks/terminal_bench/runs/<job_name>/               # job_name = 时间戳，如 2026-07-21__15-31-45
├── job.log                                              # 顶层 job 日志（内容与各 trial 的 trial.log 相同粒度）
├── lock.json                                            # 本次 job 的完整可复现配置快照（schema_version/harbor 版本/retry 策略/trials[]）
├── config.json                                          # job 级精简配置（task/trial_name/trials_dir/agent/job_id）
├── result.json                                          # job 级汇总结果（见下方字段说明）
└── <task_name>__<random_suffix>/                        # 每个 trial 一个目录，如 fix-git__VUL3pFU
    ├── config.json                                      # trial 级精简配置
    ├── lock.json                                        # trial 级配置快照（与 job lock.json 的 trials[] 条目一致）
    ├── result.json                                      # trial 级详细结果（见下方字段说明，含各阶段时间戳）
    ├── trial.log                                        # 该 trial 执行的宿主机命令日志（install/run 两条 exec_as_root/exec_as_agent 命令 + "Command outputs captured" 确认，不含 harness9 内部 stdout/ReAct 轨迹）
    ├── agent/
    │   └── setup/                                       # 空目录（本次 pilot 未产生 agent setup 产物）
    ├── artifacts/
    │   ├── manifest.json                                # 声明式产物清单（本次为 /logs/artifacts → artifacts/logs/artifacts，status: "empty"）
    │   └── logs/
    │       └── artifacts/                                # 空目录（对应 manifest 里的 empty 声明）
    └── verifier/
        ├── test-stdout.txt                               # 验证脚本（pytest）的完整 stdout（含 apt/uv 安装日志 + 测试用例明细）
        ├── ctrf.json                                     # CTRF（Common Test Report Format）格式的结构化测试结果
        └── reward.txt                                    # 单行数字，最终 reward（本次为 "1"）
```

**关键结论：harness9 自身的 ReAct 轨迹（LLM 调用、工具调用明细）不会被 Harbor 落盘。**
`trial.log` 只记录 Harbor 适配器发起的宿主机侧命令（`chmod +x` / `harness9 --prompt-file ...`）
及"Command outputs captured"确认，看不到 harness9 内部的 turn-by-turn 过程。Task 6 做轨迹分析时
需要额外方案（如让 harness9_agent.py 把 harness9 的 stdout/stderr 重定向落盘到 workDir 下再通过
`artifacts` 声明收集，或使用 Harbor 的 `resume_trajectory`/`load_trajectory` 机制）。

### 关键结果文件字段说明（实测）

- **job 级 `result.json`**：`stats.evals.<eval_name>.metrics[].mean`（本次 `1.0`）、
  `stats.evals.<eval_name>.reward_stats.reward["1.0"]`（该 reward 下的 trial 名称列表）、
  `stats.n_completed_trials` / `n_errored_trials`。
- **trial 级 `result.json`**：`verifier_result.rewards.reward`（本次 `1.0`）、
  `environment_setup` / `agent_setup` / `agent_execution` / `verifier` 四段各自的
  `started_at`/`finished_at`（可用来拆解耗时：本次 pilot 环境构建 ~2m32s，agent_setup ~1s，
  harness9 实际执行 ~50s，verifier 校验 ~39s，总运行时长 job 级报告为 `4m 4s`）。
- **`verifier/reward.txt`**：单行纯数字（`0`/`1`/浮点），Harbor 最终判分的原始来源。
- **`verifier/ctrf.json`**：`results.summary.{tests,passed,failed}` + `results.tests[].{name,status,duration}`。

### 快速查看命令

```bash
find benchmarks/terminal_bench/runs -maxdepth 5
harbor view benchmarks/terminal_bench/runs   # Harbor 自带的结果查看 TUI/CLI
```
