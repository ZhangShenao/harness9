# SWE-bench v4 扩样评测设计——验证 Planning 原生化与 Agent Loop 优化

> 日期：2026-09-09 ｜ 分支：`opt/eval` ｜ 状态：待评审

## 1. 背景与目标

harness9 已有两轮 SWE-bench Lite 评测基线：

| 轮次 | 日期 | 实例集 | 模型 | Resolved | 备注 |
|------|------|--------|------|----------|------|
| v1 | 2026-06-20 | 24（seed=1，每 repo 2 条） | claude-sonnet-4.6 | 16/24（66.7%） | 轨迹分析定位 R1-R8，催生验证关卡/stall nudge/bootstrap 优化 |
| v3 | 2026-09-03~04 | 47（seed=1，每 repo 4 条） | claude-sonnet-4.6 | 32/47（68.1%） | runner 自 8/10 后无行为变更，v3 是干净基线 |

v3 之后落地、本轮要验证的优化：

- **Planning 原生化重构**（f8df50e，9/6）：Plan 引擎级注入、写时检查点、压缩免疫、autoExecuting 续跑
- **plan_write 部分更新修复**（2ac251c/0cc35e6，9/8）：已开始条目保留
- **Sandbox 降级可见性**（同上，防御性改进，预期不改变 resolve 率）
- engine 主循环阶段化重构（a3dba5f/d819c5f，9/3）：行为等价重构，不作为本轮验证对象（与 v3 的先后关系不影响对比有效性）

**核心问题**：47 实例的 95% 置信区间约 ±13pp，v1→v3 的 +1.4pp 无法与噪声区分。本轮目标：

1. 扩样到 102 实例，把置信区间压到 ±9pp，识别 ~10pp 级别的效果
2. 与 v3 的 47 条做**同实例配对对比**（flip 分析），比总体率灵敏得多
3. 新增 token/turns 效率指标——Planning 优化的核心主张之一是"复杂任务先规划减少空转"，没有效率数据无法验证
4. 观测 plan_write 实际采用率（新能力的真实使用信号）

## 2. 决策记录

| 决策点 | 结论 | 理由 |
|--------|------|------|
| 评测规模 | **102 实例**（seed=1，`--sample 12`） | 已确认；成本约为 v3 的 2.2 倍，CI ±9pp |
| 基线策略 | **复用 v3 的 47 条配对**，不重跑基线臂 | 已确认；同 seed 超集性质保证实例完全一致；若出现显著回归，再对 losses 实例补跑基线确认 |
| 模型/Provider | claude-sonnet-4.6，Anthropic 直连 | 与 v1/v3 可比；OrcaRouter 余额 $0 且引入 provider 变量 |

## 3. 实例集设计

`sampleByRepo`（dataset.go）按 repo 名排序后逐组 shuffle、取前 N 条，rng 只在启动时播种一次 → **同 seed 下，N=12 的实例集是 N=4 的严格超集**。

seed=1、N=12 的实例分布（共 102 条）：

| Repo | 全库 | 抽取 |
|------|-----:|-----:|
| django/django | 114 | 12 |
| sympy/sympy | 77 | 12 |
| matplotlib/matplotlib | 23 | 12 |
| scikit-learn/scikit-learn | 23 | 12 |
| pytest-dev/pytest | 17 | 12 |
| sphinx-doc/sphinx | 16 | 12 |
| astropy/astropy | 6 | 6（全量） |
| psf/requests | 6 | 6（全量） |
| pylint-dev/pylint | 6 | 6（全量） |
| pydata/xarray | 5 | 5（全量） |
| mwaskom/seaborn | 4 | 4（全量） |
| pallets/flask | 3 | 3（全量） |
| **合计** | **300** | **102** |

其中 47 条与 v3 完全一致（配对子集），55 条为新增。复现命令：

```bash
go run ./cmd/swebench --dataset ./swe-bench-lite.jsonl --sample 12 --seed 1 \
    --output ./benchmarks/swebench/v4-expansion --parallel 5 --resume
```

## 4. 公平性控制

| 变量 | 控制 |
|------|------|
| 模型 | claude-sonnet-4.6（`LLM_MODEL` 固定），Anthropic 直连，不设 `ORCAROUTER_API_KEY` |
| runner 行为 | max-turns 80、bash 300s、验证关卡、stall nudge、TokenBudgetCompactor 均不动（自 8/10 起未变） |
| 实例集 | seed=1 保证可复现；评分用官方 harness 同一数据集 |
| 唯一自由变量 | harness9 代码版本（v3 时点 ↔ opt/eval HEAD） |
| 环境记录 | run_summary.md 记录并行度、起止时间、Docker 版本，便于事后归因 |

## 5. runner 增强（Phase 0 代码工作）

全部改动限于 `cmd/swebench/`，不触碰 `internal/`（引擎/Provider 零改动）。属 benchmark 工具增强，按项目规范补单元测试即可，不涉及 `internal/evals/` 黄金数据集（未新增 Agent 行为特性）。

### 5.1 countingProvider——实际 token 用量采集（P0）

新文件 `cmd/swebench/usage.go`：实现 `provider.LLMProvider` 装饰器，包装 `provider.NewFromEnv` 的返回值。

- `GenerateStream`：拦截 `StreamChunkDone` 的 `chunk.Usage` 累加；`Generate` 直接读返回的 `*schema.Usage`
- 并发安全（per-instance 一个实例，无跨实例共享；仍用原子计数或 mutex 防御）
- 记录：`input_tokens`、`output_tokens`、`llm_calls`（含重试，即真实账单口径）

选型说明：不改 `TokenUpdateData`（其 `EstimatedTokens` 只承载 input 侧且语义为"当前上下文"，混入累计 output 会污染 TUI 语义）；装饰器模式对引擎零侵入，SWE-bench 专属不外溢。

### 5.2 per-instance 指标落盘（P0）

`RunResult` 扩展字段：`InputTokens`、`OutputTokens`、`LLMCalls`、`Turns`、`PlanWrites`、`VerifyGateTriggered`、`RanTest`。

新增 `usage.jsonl`（与 predictions.jsonl 同目录、同追加写模式），每实例一行：

```json
{"instance_id":"django__django-12908","input_tokens":123456,"output_tokens":7890,"llm_calls":23,"turns":12,"plan_writes":1,"verify_gate":false,"ran_test":true,"duration_sec":487}
```

### 5.3 plan_write 采用计数与验证关卡信号（P0）

`streamOnce` 的事件循环中：

- `EventToolStart` 且 `tc.Name == "plan_write"` → `PlanWrites++`（Planning 实际采用率）
- `looksLikeTestRun` 结果透出为 `RanTest`；验证关卡注入过 → `VerifyGateTriggered`

### 5.4 `--instances <file>` 显式实例清单（P1）

每行一个 instance_id，`#` 注释；与 `--dataset` 组合做交集过滤。用途：Phase 1 冒烟只跑 v3 的 24 实例子集；未来若需 A/B 双臂可复用同一清单。

### 5.5 对比报告工具（P1）

新增 `benchmarks/swebench/compare.py`：输入两个官方 harness 评分 JSON + 可选两个 usage.jsonl，输出 markdown：

- 翻转分析：`unresolved→resolved`（wins）与 `resolved→unresolved`（losses）逐实例清单
- per-repo resolve 率对照表
- 效率对照：配对实例的 tokens/turns/时长中位数与均值
- 总体率与 95% Wilson 置信区间

说明：评分产物是 python 生态（swebench harness）生成的，分析脚本与评分同生态、免编译，且不进入 Go 生产二进制；故此处有意不用 Go。

### 5.6 单元测试

- `usage_test.go`：countingProvider 表驱动（流式含 Usage/不含 Usage/nil、并发累加、重试计入）
- `dataset_test.go` 补充：超集性质测试（同 seed 下 n=4 ⊂ n=12 的实例 ID 集合）
- `main_test.go`/`runner_test.go` 补充：--instances 过滤、usage.jsonl 追加写与 resume 交互
- 全量 `go test ./...` 与 `gofmt -l .` 必须通过（含 AutoCorrect 文案 lint）

## 6. 评分方案

沿用本地官方 harness（v1/v3 已验证）：

```bash
python -m swebench.harness.run_evaluation \
    --dataset_name princeton-nlp/SWE-bench_Lite \
    --predictions_path ./benchmarks/swebench/v4-expansion/predictions.jsonl \
    --max_workers 5 --run_id harness9-lite-v4
```

- 评分前清理同名容器（v2 时踩过 409 Conflict：`docker ps -aq --filter name=sweb.eval | xargs docker rm -f`）
- 评分前 `docker system df` 检查磁盘（官方每实例镜像为 x86_64，Apple Silicon 下走 QEMU 模拟，累计可达数十 GB）
- 结果 JSON 拷贝为 `benchmarks/swebench/v4-expansion/anthropic__claude-sonnet-4.6.harness9-lite-v4.json`

## 7. 执行流程

| Phase | 内容 | 预计耗时 | 产物 |
|-------|------|---------|------|
| 0 | runner 增强 + 单测（5.1-5.6） | ~0.5 天 | opt/eval 分支提交 |
| 1 | 冒烟：`--instances` 跑 v3 的 24 实例子集，输出到独立目录 `swebench-smoke/`（不混入正式结果），验证用量落盘/评分链路/实测单实例成本 | 1-2h | 冒烟 summary + 成本外推 |
| 2 | 正式跑 102 实例（`--parallel 5 --resume`；冒烟后若外推成本超预算，先用 `--instances` 截子集） | 6-12h | predictions.jsonl + usage.jsonl + run_summary.md |
| 3 | 官方 harness 评分 | ~1h | v4 评分 JSON |
| 4 | compare.py 对比分析 + 报告撰写 | ~0.5 天 | benchmarks/swebench/v4-expansion/ + 结论 |

冒烟通过标准：usage.jsonl 每行字段完整非零（input_tokens > 0）、plan_writes 字段存在、24/24 有 patch、评分可跑通。

## 8. 分析方法

1. **主指标**：102 实例 resolve 率 + 95% Wilson CI
2. **配对 flip 分析**（核心证据）：47 重叠实例上 v4 vs v3 的 wins/losses 清单；wins ≫ losses 支持优化有效，losses ≫ wins 触发回归调查
3. **回归处置**：若 losses ≥ 3，对 losses 实例在 v3 时点 commit（3a5a6cb，9/4）checkout 基线二进制补跑确认（单实例重跑，成本可控）
4. **效率指标**：47 配对实例的 tokens/turns/时长中位数对比（符号检验方向性结论即可，不苛求显著性）
5. **Plan 采用观察**（描述性）：plan_writes > 0 的实例占比、其 resolve 率与 turns 分布；复杂度以 problem_statement 长度或 repo 粗分
6. **新失败模式**：losses 实例逐一回看轨迹；若成模式（≥2 条同因），写 `docs/技术调研/swebench-轨迹分析-v3.md`（沿用 R 编号续接）

## 9. 风险与应对

| 风险 | 应对 |
|------|------|
| 长跑中断（API 限速/网络/机器休眠） | `--resume` + predictions.jsonl 逐实例追加写已内建；重跑同命令即续 |
| 单实例烧钱失控 | max-turns 80 + 30min timeout 双保险已有；冒烟阶段实测单实例成本后再放全量 |
| 评分容器名冲突 | 唯一 run_id `harness9-lite-v4` + 评分前预清理（见 §6） |
| 磁盘耗尽（镜像累积） | 评分前 `docker system df`；`--run_id` 复用可命中已拉取镜像 |
| 结果污染 | 冒烟结果独立目录；正式 predictions.jsonl 只写一次（新文件目录 v4-expansion） |
| OrcaRouter 误接管 | 运行环境不设 `ORCAROUTER_API_KEY`，preflight 已支持两种 Key，.env 检查前置 |

## 10. 交付物清单

- [ ] `cmd/swebench/`：usage.go（countingProvider）、--instances、指标落盘 + 全套单测（opt/eval）
- [ ] `benchmarks/swebench/compare.py` + 其对 v3/v4 JSON 的自测
- [ ] `benchmarks/swebench/v4-expansion/`：predictions.jsonl、usage.jsonl、run_summary.md、评分 JSON、compare-report.md
- [ ] 结论：resolve 率 ± CI、47 配对 flip 清单、效率对照、Plan 采用观察
- [ ] （条件触发）`docs/技术调研/swebench-轨迹分析-v3.md`
