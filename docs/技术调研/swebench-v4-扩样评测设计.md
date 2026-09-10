# SWE-bench v4 评测设计——Kimi-K3 基线建立与优化后行为观测

> 日期：2026-09-09 ｜ 分支：`opt/eval` ｜ 状态：v2 修订（样本 78 / 模型 Kimi-K3）

## 1. 背景与目标

harness9 已有两轮 SWE-bench Lite 评测基线：

| 轮次 | 日期 | 实例集 | 模型 | Resolved | 备注 |
|------|------|--------|------|----------|------|
| v1 | 2026-06-20 | 24（seed=1，每 repo 2 条） | claude-sonnet-4.6 | 16/24（66.7%） | 轨迹分析定位 R1-R8，催生验证关卡/stall nudge/bootstrap 优化 |
| v3 | 2026-09-03~04 | 47（seed=1，每 repo 4 条） | claude-sonnet-4.6 | 32/47（68.1%） | runner 自 8/10 后无行为变更 |

v3 之后落地、与本轮相关的优化：**Planning 原生化重构**（f8df50e，9/6）、**plan_write 部分更新修复**（2ac251c/0cc35e6，9/8）、engine 主循环阶段化重构（行为等价，不作为验证对象）。

**模型切换的影响**：本轮模型改为 `moonshotai/kimi-k3`（OpenRouter，`.env` 已配置）。v1/v3 均为 sonnet-4.6 所跑，模型与 Provider 同时变化后，**v3 的预测结果不再构成受控对照**——跨轮 resolve 率差异无法归因于 harness9 优化。据此本轮定位调整为：

1. **建立 Kimi-K3 × 当前优化后代码的基线**（78 实例，95% CI 约 ±10pp），作为后续所有迭代的对照锚点
2. **轮内行为观测**验证优化效果：token/turns 效率、plan_write 实际采用率、验证关卡触发率、停滞提示命中——这些指标不依赖历史基线即可表征"规划是否减少空转"
3. **失败轨迹分析**：unresolved 实例逐条归因，捕捉 Kimi-K3 特有的新失败模式
4. （可选后续）严格归因：同一 78 实例集 + 9/4 时点代码（优化前）+ Kimi-K3 双臂补跑，做 flip 分析——本轮不执行，成本 ×2，需要时随时可启动（实例集与指标落盘已为此设计）

## 2. 决策记录

| 决策点 | 结论 | 理由 |
|--------|------|------|
| 评测规模 | **78 实例**（seed=1，`--sample 8`） | 用户目标 80；均匀 per-repo 上限无法恰好命中（N=8→78、N=9→84），取 78 保持预算内且保留 v3 超集性质。若必须精确 80 需改采样器（全局截断会破坏超集性质），不值 |
| 模型 | `moonshotai/kimi-k3`，OpenRouter（`.env`：`LLM_MODEL` + `OPENAI_BASE_URL` 已配置） | 用户指定；与 v1/v3 不一致可接受，代价是跨轮归因失效（见 §1） |
| 基线策略 | **单臂建立新基线**；v3 数据仅作参考对照（非归因，须声明模型变量不同） | 模型切换后配对对比不再受控；严格归因留作可选双臂（§1 目标 4） |

## 3. 实例集设计

`sampleByRepo`（dataset.go）按 repo 名排序后逐组 shuffle、取前 N 条，rng 只在启动时播种一次 → **同 seed 下，N=8 的实例集是 N=4（v3）的严格超集**（47 ⊂ 78，供参考对照）。

seed=1、N=8 的实例分布（共 78 条）：

| Repo | 全库 | 抽取 |
|------|-----:|-----:|
| django/django | 114 | 8 |
| sympy/sympy | 77 | 8 |
| matplotlib/matplotlib | 23 | 8 |
| scikit-learn/scikit-learn | 23 | 8 |
| pytest-dev/pytest | 17 | 8 |
| sphinx-doc/sphinx | 16 | 8 |
| astropy/astropy | 6 | 6（全量） |
| psf/requests | 6 | 6（全量） |
| pylint-dev/pylint | 6 | 6（全量） |
| pydata/xarray | 5 | 5（全量） |
| mwaskom/seaborn | 4 | 4（全量） |
| pallets/flask | 3 | 3（全量） |
| **合计** | **300** | **78** |

复现命令：

```bash
go run ./cmd/swebench --dataset ./swe-bench-lite.jsonl --sample 8 --seed 1 \
    --output ./benchmarks/swebench/v4-expansion --parallel 5 --resume
```

## 4. 公平性与口径控制

本轮为单臂基线建立，无跨轮归因诉求，控制目标改为**轮内口径一致与记录完整**：

| 变量 | 控制 |
|------|------|
| 模型/Provider | `moonshotai/kimi-k3`，OpenRouter 端点，全程不设 `ORCAROUTER_API_KEY`；运行中不换模型 |
| runner 行为 | max-turns 80、bash 300s、验证关卡、stall nudge、TokenBudgetCompactor 均维持现状 |
| 实例集 | seed=1 可复现；评分用官方 harness 同一数据集 |
| 环境记录 | run_summary.md 记录并行度、起止时间、Docker 版本、模型名与 BaseURL 主机名（不记 Key） |
| 上下文窗口 | Phase 0 为 kimi-k3 补 `knownModels` 条目（当前 256K 回退会低估其 1M 窗口，导致压缩过早触发，见 §5.7） |

## 5. runner 增强（Phase 0 代码工作）

主体改动限于 `cmd/swebench/`；唯一的 `internal/` 改动是 §5.7 的模型注册表条目（一行 + 注释）。属 benchmark 工具增强，按项目规范补单元测试即可，不涉及 `internal/evals/` 黄金数据集（未新增 Agent 行为特性）。

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

每行一个 instance_id，`#` 注释；与 `--dataset` 组合做交集过滤。用途：Phase 1 冒烟只跑 v3 的 24 实例子集；为 §1 目标 4 的可选双臂预留同一实例集。

### 5.5 对比报告工具（P1）

新增 `benchmarks/swebench/compare.py`：输入一个或两个官方 harness 评分 JSON + 可选 usage.jsonl，输出 markdown：

- 单臂模式（本轮）：总体 resolve 率 + 95% Wilson CI、per-repo 分布、效率指标（tokens/turns/时长中位数与均值）、plan_write 采用率交叉表
- 双臂模式（可选后续）：两轮 resolved ids diff → wins/losses 翻转清单 + 配对效率对比
- v3 参考对照：47 重叠实例的 resolved 交集对照表（显著标注"模型不同，仅方向性参考"）

说明：评分产物是 python 生态（swebench harness）生成的，分析脚本与评分同生态、免编译，且不进入 Go 生产二进制；故此处有意不用 Go。

### 5.6 单元测试

- `usage_test.go`：countingProvider 表驱动（流式含 Usage/不含 Usage/nil、并发累加、重试计入）
- `dataset_test.go` 补充：超集性质测试（同 seed 下 n=4 ⊂ n=8 的实例 ID 集合）
- `main_test.go`/`runner_test.go` 补充：--instances 过滤、usage.jsonl 追加写与 resume 交互
- provider 包：`GetModelLimits("moonshotai/kimi-k3")` 返回 1M 窗口的断言（随 §5.7 条目）
- 全量 `go test ./...` 与 `gofmt -l .` 必须通过（含 AutoCorrect 文案 lint）

### 5.7 kimi-k3 模型注册表条目（P0，唯一 internal/ 改动）

`internal/provider/model_limits.go` 的 `knownModels` 无 kimi/moonshot 条目，`GetModelLimits("moonshotai/kimi-k3")` 回退 256K。OpenRouter 官方元数据（2026-09-09 实测查询）：

- `context_length: 1048576`（1M）——256K 回退会把 TokenBudgetCompactor 预算算成 140K（55% × 256K），长轨迹压缩过早触发；方向安全（不会爆窗口）但影响长轨迹效率与压缩质量
- `max_completion_tokens: 943718`

新增条目（runner 仅消费 `ContextTokens`）：

```go
// Moonshot Kimi（OpenRouter 元数据，2026-09 实测）
"kimi-k3": {ContextTokens: 1_048_576, OutputTokens: 65_536},
```

OutputTokens 取保守值 64K（OpenRouter 上限 943K 无实际意义：单次响应远用不到，且 `WithGenerateRetry` 场景下过大 max_tokens 无收益）。

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
- 结果 JSON 拷贝为 `benchmarks/swebench/v4-expansion/moonshotai__kimi-k3.harness9-lite-v4.json`

## 7. 执行流程

| Phase | 内容 | 预计耗时 | 产物 |
|-------|------|---------|------|
| 0 | runner 增强 + 模型条目 + 单测（5.1-5.7） | ~0.5 天 | opt/eval 分支提交 |
| 1 | 冒烟：`--instances` 跑 v3 的 24 实例子集，输出到独立目录 `swebench-smoke/`（不混入正式结果），验证 OpenRouter 链路/用量落盘/评分链路/实测单实例成本与限速 | 1-2h | 冒烟 summary + 成本外推 |
| 2 | 正式跑 78 实例（`--parallel 5 --resume`；冒烟后若外推成本超预算，先用 `--instances` 截子集） | 6-12h | predictions.jsonl + usage.jsonl + run_summary.md |
| 3 | 官方 harness 评分 | ~1h | v4 评分 JSON |
| 4 | compare.py 分析 + 报告撰写 | ~0.5 天 | benchmarks/swebench/v4-expansion/ + 结论 |

kimi-k3 定价 $3/$15 每 MTok（OpenRouter，2026-09-09 实测），与 sonnet-4.6 同价位，成本预期与原方案持平。

冒烟通过标准：usage.jsonl 每行字段完整非零（input_tokens > 0）、plan_writes 字段存在、24/24 有 patch、评分可跑通、无 OpenRouter 限速导致的系统性错误。

## 8. 分析方法

1. **主指标**：78 实例 resolve 率 + 95% Wilson CI（p=0.68 时约 ±10.4pp；Kimi-K3 实际 resolve 率不同则 CI 相应收窄/放宽）
2. **参考对照**（非归因，显著声明模型变量不同）：47 重叠实例上 v4 与 v3 的 resolved 交集/差集，仅作方向性观察
3. **效率基线**：tokens/turns/时长分布（中位数 + P90），建立 Kimi-K3 口径的效率锚点；供后续迭代对比
4. **Plan 采用观察**（描述性）：plan_writes > 0 的实例占比、其 resolve 率与 turns 分布；复杂度以 problem_statement 长度或 repo 粗分
5. **失败轨迹分析**：unresolved 实例逐条回看轨迹归因（沿用 R 编号体系）；若 ≥2 条同因成模式，写 `docs/技术调研/swebench-轨迹分析-v3.md`
6. （可选后续）**严格归因双臂**：同 78 实例集 + 9/4 时点 commit + Kimi-K3 补跑一臂 → flip 分析 + 配对效率对比

## 9. 风险与应对

| 风险 | 应对 |
|------|------|
| OpenRouter 限速/可用性波动（新链路未经验证） | Phase 1 冒烟实测限速与错误率；`--resume` + 逐实例追加写兜底；`WithGenerateRetry(4)` 已有 |
| kimi-k3 上下文窗口注册缺失 | §5.7 Phase 0 补条目；冒烟阶段核对 `EventTokenUpdate` 与实际用量是否合理 |
| kimi-k3 输出 reasoning_content | OpenAI Provider 已支持提取（extractReasoningContent），runner 仅记录轨迹，不影响评分 |
| 长跑中断（网络/机器休眠） | `--resume` 已内建；重跑同命令即续 |
| 单实例烧钱失控 | max-turns 80 + 30min timeout 双保险已有；冒烟阶段实测单实例成本后再放全量 |
| 评分容器名冲突 | 唯一 run_id `harness9-lite-v4` + 评分前预清理（见 §6） |
| 磁盘耗尽（镜像累积） | 评分前 `docker system df`；`--run_id` 复用可命中已拉取镜像 |
| 结果污染 | 冒烟结果独立目录；正式 predictions.jsonl 只写一次（新文件目录 v4-expansion） |
| OrcaRouter 误接管 | 运行环境不设 `ORCAROUTER_API_KEY`，preflight 的 OPENAI_API_KEY 分支先行命中 |

## 10. 交付物清单

- [ ] `cmd/swebench/`：usage.go（countingProvider）、--instances、指标落盘 + 全套单测（opt/eval）
- [ ] `internal/provider/model_limits.go`：kimi-k3 条目 + 断言测试
- [ ] `benchmarks/swebench/compare.py`（单臂/双臂/v3 参考三模式）
- [ ] `benchmarks/swebench/v4-expansion/`：predictions.jsonl、usage.jsonl、run_summary.md、评分 JSON、compare-report.md
- [ ] 结论：Kimi-K3 resolve 率 ± CI、效率锚点、Plan 采用观察、失败模式清单
- [ ] （条件触发）`docs/技术调研/swebench-轨迹分析-v3.md`
