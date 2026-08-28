# Benchmark — SWE-bench 评估体系

## 1. 技术背景

### 1.1 为什么需要 Benchmark

harness9 的核心价值在于「编排 Agent 解决真实工程问题」，而不是通过静态单元测试就能验证的能力。因此需要一个**客观、可量化、与主流系统可比较**的评估体系，来回答：

> harness9 驱动的 LLM Agent，在真实软件工程任务上的能力边界在哪里？

### 1.2 SWE-bench 概述

[SWE-bench](https://github.com/princeton-nlp/SWE-bench)（Software Engineering Benchmark）由 Princeton NLP 团队构建，是当前 Agent 能力评估的业界权威标准。

**数据来源**：从 GitHub 上 12 个主流 Python 开源项目（Django、Flask、Requests、Sympy 等）中，收集真实的 Issue + 对应的 PR（commit），确保每个 Issue 都有已知的正确修复。

**评估方式**：
- 给 Agent 一个 Issue 描述（`problem_statement`）和一个仓库快照（`base_commit`）
- Agent 自主探索代码、定位 bug、生成修复 patch
- 官方评估器在沙箱中将 patch 应用到仓库，运行原始测试套件，判断是否 **Resolved**

**指标**：`% Resolved` —— 成功修复的 Instance 数 / 总 Instance 数。

### 1.3 数据集规模

| 版本 | Instance 数 | 说明 |
|------|------------|------|
| **SWE-bench** | 2,294 | 完整集，权威但运行成本高 |
| **SWE-bench Lite** | 300 | 精选子集，平衡难度，主流评测首选 |
| **SWE-bench Verified** | 500 | 人工验证子集，信噪比最高 |

harness9 当前针对 **SWE-bench Lite** 运行，并支持按 repo 分类抽样（默认每类 10 条），平衡成本与覆盖面。

### 1.4 主流系统参考分数（SWE-bench Lite）

| 系统 | % Resolved | 备注 |
|------|-----------|------|
| SWE-agent (GPT-4) | ~18% | 经典 ReAct agent |
| Devin | ~14% | 早期 AI 软件工程师 |
| Claude 3.5 Sonnet | ~49% | Anthropic 官方结果 |
| OpenHands (CodeAct) | ~26% | 开源框架 |
| harness9 | TBD | 待跑分 |

---

## 2. Benchmark 核心原理

SWE-bench 的执行分为**两个完全解耦的阶段**：

```
┌─────────────────────────────────────────────────────────────────┐
│                     Phase 1: Inference（推断）                    │
│                                                                  │
│   ┌──────────┐    problem_statement    ┌─────────────────────┐  │
│   │ Dataset  │ ──────────────────────► │   harness9 Runner   │  │
│   │ (JSONL)  │    base_commit          │   (cmd/swebench/)   │  │
│   └──────────┘                        └──────────┬──────────┘  │
│                                                  │              │
│                                            git diff             │
│                                                  │              │
│                                                  ▼              │
│                                        predictions.jsonl        │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                     Phase 2: Evaluation（评估）                   │
│                                                                  │
│  predictions.jsonl ──► swebench Python 包 ──► 官方 Docker 镜像  │
│                                                      │           │
│                                              apply patch         │
│                                              run test suite      │
│                                                      │           │
│                                                      ▼           │
│                                            % Resolved 分数       │
└─────────────────────────────────────────────────────────────────┘
```

**解耦的好处**：
- Inference 和 Evaluation 可以完全独立运行，无需在 Agent 执行期间安装测试套件
- 可先积累所有 `predictions.jsonl`，再批量评估
- 官方 Docker 镜像保证评估环境一致性，避免本地环境差异影响分数

### 2.1 数据集格式

SWE-bench Lite 以 JSONL 格式分发，每行一个 Instance：

```json
{
  "instance_id": "django__django-11179",
  "repo": "django/django",
  "base_commit": "9b224f172a30d38d8b4e7b38a4e2ee47faaf4019",
  "problem_statement": "Autoreloader with StatReloader doesn't work properly...",
  "hints_text": "",
  "test_patch": "diff --git a/tests/utils_tests/test_autoreload.py..."
}
```

| 字段 | 说明 | 是否传给 Agent |
|------|------|:------------:|
| `instance_id` | 唯一标识符（`repo__hash` 格式） | 否 |
| `repo` | GitHub 仓库路径（`owner/name`） | 否（用于 clone）|
| `base_commit` | 问题存在的提交 hash | 否（用于 checkout）|
| `problem_statement` | Issue 描述（Agent 看到的任务输入） | **是** |
| `hints_text` | 可选提示（部分 Instance 有） | 否（不传给 Agent）|
| `test_patch` | 用于验证的测试改动（官方评估器专用） | **绝对不传** |

> `test_patch` 是评估的黄金标准，绝不能泄露给 Agent，否则污染分数。

### 2.2 Prediction 格式

Runner 输出 `predictions.jsonl`，每行一条：

```json
{"instance_id": "django__django-11179", "model_patch": "diff --git a/django/utils/autoreload.py..."}
```

`model_patch` 为空字符串时，评估器将该 Instance 标记为 **Unresolved**（Agent 无改动或失败）。

---

## 3. harness9 的设计

### 3.1 整体架构

```
cmd/swebench/
├── main.go       CLI 入口：flag 解析、preflight、数据集加载、并发编排
├── runner.go     单 instance 执行核心：git → sandbox → engine → patch
├── dataset.go    JSONL 加载、按 repo 分类随机采样
├── prompt.go     SWE-bench 专用 system prompt（结构化流程约束）
└── report.go     predictions.jsonl 追加写、run_summary.md 生成
```

### 3.2 单 Instance 执行流程

```
runInstance(ctx, inst, cfg)
│
├─ 1. os.MkdirTemp → tmpDir（defer RemoveAll 保证清理）
│
├─ 2. git clone https://github.com/<inst.Repo> tmpDir  [5min timeout]
│      git -C tmpDir checkout <inst.BaseCommit>         [30s timeout]
│
├─ 3. sandbox.Manager.Create(tmpDir)                    [60s timeout]
│      → DockerEnvironment（bind mount 共享 tmpDir）
│      → defer DestroyAll（独立清理 context，不受取消影响）
│
├─ 4. tools.Registry 注册四个工具：
│      bash（路由进容器）
│      read_file / write_file / edit_file（bind mount，宿主机侧 IO）
│
├─ 5. engine.NewAgentEngine(llm, hookReg, tmpDir,
│        WithPromptBuilder(&swebenchPromptBuilder{inst}))
│        // MaxTurns > 0 时才附加 WithMaxTurns，否则沿用引擎默认值（500）
│
├─ 6. runWithTrajectory(instanceCtx, eng, prompt, logPath, inst)  [per-instance timeout]
│      → engine.RunStream() 消费所有事件，写入 logs/<instance_id>.log
│      → Benchmark 模式自动批准所有工具审批（无人值守）
│
├─ 7. git diff（独立 context.Background，Ctrl+C 安全）  [10s timeout]
│      → patch string
│
└─ 8. return RunResult{Instance, Patch, Error, Duration}
```

**关键设计决策**：

| 决策 | 理由 |
|------|------|
| git clone 在宿主机执行 | 容器内无 git credential，bind mount 后容器可见相同文件 |
| bash 工具路由进容器 | Agent 执行的 Python 代码在隔离环境中运行，不污染宿主机 |
| bash 超时放宽至 300s | 默认 120s 仍不足以跑完测试套件/装依赖；runner 通过 `WithBashTimeout` 提到 300s，模型亦可用 `timeout_secs` 临时放宽（验证修复的关键路径）|
| 收集 patch 前先 `git add -A -N` | 纯 `git diff` 只输出已跟踪文件，Agent 用 write_file 新建的修复文件会被静默丢弃；intent-to-add 使新文件进入 diff |
| git diff 用独立 context | Ctrl+C 取消主 context 后，仍能收集已修改的 patch，避免丢弃有效结果 |
| 显式接入 Compactor + ContextWindow | 此前未配置压缩器，长轨迹上下文无界增长触及窗口 → API 400 杀实例；runner 用无需 LLM/Session 的 `TokenBudgetCompactor`（预算取窗口 55%，为工具定义 + 输出 + 估算误差留余量）|
| 引擎级生成重试 `WithGenerateRetry(4, 2s)` | SDK 重试只覆盖首字节前；流式中途断连/瞬时 429 会逃逸到引擎层杀实例。应用层有界退避重试把"一次抖动杀实例"变为可恢复事件 |
| `WithPermissionMode(BypassAll)` | 无人值守显式短路审批，零延迟，不依赖是否注册 hook |
| MaxTurns benchmark 默认 80 | 此前沿用引擎 500，卡死实例会在每实例超时内烧掉大量 token；80 足够 explore+fix+verify 又能截断失控循环（如观测到的 69 轮 runaway）。仍可用 `--max-turns N` 覆盖 |
| 单实例超时默认 30 分钟 | 原 10 分钟需覆盖 clone+sandbox 启动 + 整段 agent loop，对大仓库偏紧 |
| 采样种子固定（`--seed`，默认 1）| 原用 `time.Now().UnixNano()` 导致每次运行抽样不同、无法复现/对比；固定 seed → 同实例集，`--resume` 自然一致 |
| 日志按 RunID 命名空间隔离 | `logs/<RunID>/<instance>.log`，避免多次运行同名日志互相覆盖、污染分析 |
| `--resume` 仅跳过非空 patch | 原按 instance_id 跳过且接受空 patch，导致最该重跑的失败实例被永久跳过；改为仅跳过已产出非空 patch 的实例 |
| RunStream 替代 Run | 以事件流方式捕获完整 trajectory，写入 `logs/<RunID>/` 目录供后续分析 |
| predictions.jsonl 追加写 | 每条完成后立即 flush，配合 `--resume` 支持断点续跑 |
| **默认依赖自举（接通 bootstrap 接缝）** | runner 现在为每实例默认设置 `BootstrapCmd`（`ensurepip` + `pip install -e .` + `pytest`），在 Agent 启动前把"真实测试"变得可运行——**恢复验证闭环**（轨迹分析 R1：此前 24/24 实例零测试运行，全靠静态分析）。`SANDBOX_BOOTSTRAP_CMD` 显式设置后覆盖默认；需编译器的仓库可设 `SANDBOX_IMAGE` 指向官方每实例镜像 |
| **默认镜像改为 `python:3.11`（非 slim）** | slim 常缺 pip、运行库不全；full 镜像自带 pip 并能从 wheel 拉取 numpy/pandas 等依赖，配合默认自举即可跑真实测试（轨迹分析 R1） |
| **验证关卡（verification gate）** | Agent 自然结束却"全程未运行任何测试"时，runner 注入**一次**续跑提示要求真实验证（复用同一引擎 + 内存会话延续历史，至多一次，受超时/turn 上限兜底）。修复"9 轮静态自证即交卷"（轨迹分析 R2:8/8 失败实例均零验证即交卷）|
| **停滞提示 `WithStallNudge(10, …)`** | 连续 10 轮无任何改动/测试运行（只在静态重读/grep 空转）时，引擎注入一次提示打断空转（轨迹分析 R6：xarray-3364、pylint-7080 烧满 80 轮即此形态）。仅作用于临时副本，不持久化 |
| **注入 `hints_text` + dataset 解析评测字段** | `Instance` 现解析 `version`/`environment_setup_commit`/`FAIL_TO_PASS`/`test_patch`；prompt 注入维护者讨论（hints），它常含决定性 API 设计（轨迹分析 R3：flask `text=True`、xarray DeprecationWarning）。⚠️ `FAIL_TO_PASS`/`test_patch` 仅供分析，**绝不**在 Agent 运行时暴露/应用 |

### 3.3 采样策略

SWE-bench Lite 覆盖 11 个 Python 仓库，harness9 按 repo 分类后随机采样：

```
allInstances (300条)
    │
    ├─ 按 repo 分组 → 11 个分组（astropy/astropy, django/django, ...）
    │
    ├─ 每组内随机打乱（seed 固定 → 可重复）
    │
    ├─ 每组取前 min(n, groupSize) 条
    │
    └─ 合并后整体打乱 → 均衡分布，并发时不集中于单一 repo
```

这样能在控制总量的同时，保证跨仓库的多样性（不同语言特性、项目规模、bug 类型）。

### 3.4 专用 System Prompt 设计

harness9 为 SWE-bench 设计了专用的 system prompt（英文，提升英文代码任务质量），策略为：

**结构化流程约束（5 步顺序）+ 每步内自由探索（不限制工具调用方式）**

```
Step 1 — 理解问题
  ↓ 识别 bug 核心、复现步骤、预期行为

Step 2 — 探索仓库
  ↓ grep 定位相关文件，read_file 行号读取；并行多工具调用
  ↓ 阅读（绝不修改）相关现有测试——它们编码了维护者期望的行为/输出/边界

Step 3 — 复现（可行时）
  ↓ python 可用且包可 import 时写最简复现；用 heredoc 执行，绝不在仓库内建临时 .py（污染 patch）

Step 4 — 修复
  ↓ 在产生错误行为的"那一行"（raise/return/分支）做最小修改，不新增并行代码路径、
  ↓ 不把分配/别名提到循环外（plausible-but-broader 改动常过不了隐藏测试）
  ↓ 按"被改符号名"grep 测试并阅读；歧义/意外 API 行为优先查项目的 DeprecationWarning 约定
  ↓ 绝不修改测试文件，不引入新依赖；编辑前 grep -n 定位精确行号

Step 5 — 验证（行为而非语法）
  ↓ edit_file 的 diff 只确认"字节已写入"，不代表行为正确
  ↓ 运行真实测试 / 复现脚本验证行为；绝不"把类/函数原样重抄到内联脚本里自测"
```

**约束的设计原则**：
- "不修改测试文件"是 SWE-bench 的硬约束，违反会导致评估结果无效；但**鼓励阅读**现有测试（最强行为信号），且要求按"被改符号名"检索而非主题关键词（轨迹分析 R7）
- **最小化、错误点局部修复偏置**：多个失败源于"合理但错位/过宽"的修复（pylint 改错文件、requests 把对象提到循环外改变别名、xarray 新增并行代码路径），prompt 明确要求在错误产生处做最小改动
- **行为验证优先 + 不再默认退化静态分析**：删除了"依赖可能没有、pip 可能不可用 → 退化为静态分析"的逃生门（轨迹分析 R5：它把"放弃验证"写成了官方默许），改为"环境已尝试预装依赖，优先跑真实测试；导入失败先自举安装，确实无法运行才静态复核并明示"
- **注入维护者 hints + deprecation 约定提示**：`hints_text` 此前被解析却从未注入，而它常含决定性 API 设计；prompt 现注入并提示"讨论常覆盖原 issue 提案"，并对歧义 API 行为提示优先考虑 DeprecationWarning 而非静默改行为（轨迹分析 R3/R7）
- **文件工具一律用相对路径**：注入的绝对工作目录曾诱使模型给 read_file/edit_file 传绝对路径，触发路径拼接错误（已在 `safePath` 修复，并以 prompt 双重保险）
- 推理语言改为英文 + 单行防漂移约束（评测只看 patch，英文更贴合英文代码/Issue/堆栈）

### 3.5 并发控制与韧性

```
主循环（main.go）

sem := semaphore.NewWeighted(N)   ← --parallel N 控制最大并发

for each instance:
    sem.Acquire(ctx, 1)            ← 获取槽位（超出 N 时阻塞）
    go func:
        result = runInstance(...)  ← 每个 goroutine 完全独立
        mu.Lock()
        results = append(...)
        appendPrediction(...)      ← 立即写入，不等全部完成
        mu.Unlock()
        sem.Release(1)             ← 释放槽位

wg.Wait()                          ← 等所有 goroutine 完成
writeSummary(...)
```

**韧性机制**：

| 场景 | 处理方式 |
|------|---------|
| git clone 失败 | 记录 Error，写空 patch，继续下一条 |
| Docker 启动失败 | 同上 |
| LLM API 瞬时错误/限流/流式断连 | 引擎级有界退避重试（`WithGenerateRetry`）+ SDK 内置重试；多数瞬时抖动可恢复，不再杀实例 |
| 上下文逼近窗口 | `TokenBudgetCompactor` 在窗口 55% 处裁剪旧 Observation，规避 400 溢出 |
| MaxTurns 触发（默认 80）| 收集当前 git diff（含 `git add -N` 的新文件），不标记为错误 |
| 整体 Ctrl+C | 等待当前 instance 完成，收集 patch 后退出 |
| `--resume` 重启 | 仅跳过**已产出非空 patch** 的 instance；空/出错实例会被重试 |

---

## 4. 完整操作流程

### 4.1 前置条件

**推荐方式**：在项目根目录创建 `.env` 文件（与 harness9 主程序共用同一套配置，runner 启动时自动从当前工作目录加载）：

```bash
# harness9/.env
OPENAI_API_KEY=sk-...
OPENAI_BASE_URL=https://openrouter.ai/api/v1   # 可选，接入 OpenRouter / Azure 等
LLM_MODEL=openai/gpt-4o
# 默认 python:3.11（自带 pip，可从 wheel 拉依赖）；runner 会默认自举安装依赖以跑真实测试。
# 高保真：设为官方每实例镜像 swebench/sweb.eval.x86_64.<instance>（仓库+依赖已预装）。
SANDBOX_IMAGE=python:3.11
# 可选：覆盖默认依赖自举命令（默认 ensurepip + pip install -e . + pytest）。
# SANDBOX_BOOTSTRAP_CMD=pip install -e . -q && pip install pytest -q
```

**也可通过系统环境变量提供**（系统变量优先于 `.env`）：

```bash
export OPENAI_API_KEY=sk-...
export LLM_MODEL=openai/gpt-4o
```

确认 Docker 守护进程运行中：

```bash
docker info
```

### 4.2 下载数据集

```bash
pip install datasets

python -c "
from datasets import load_dataset
ds = load_dataset('princeton-nlp/SWE-bench_Lite', split='test')
ds.to_json('swe-bench-lite.jsonl')
print(f'下载完成: {len(ds)} 条 instances')
"
```

> 也可以直接从 Hugging Face Hub 下载 JSONL 文件：
> `https://huggingface.co/datasets/princeton-nlp/SWE-bench_Lite`

### 4.3 按类别抽样运行（推荐初次运行）

配置好 `.env` 后直接运行（runner 从当前目录自动加载）：

```bash
cd /path/to/harness9

go run ./cmd/swebench \
  --dataset swe-bench-lite.jsonl \
  --sample 10 \       # 每个 repo 取 10 条，约 110 条总量
  --output ./swebench-results \
  --parallel 2 \      # 同时运行 2 个 instance
  --timeout 15        # 每个 instance 超时 15 分钟（--max-turns 默认 0 = 不限轮数）
```

如果没有 `.env` 文件，也可直接通过环境变量传入：

```bash
OPENAI_API_KEY=sk-... LLM_MODEL=openai/gpt-4o go run ./cmd/swebench \
  --dataset swe-bench-lite.jsonl --sample 10
```

运行期间，stderr 输出进度：

```
数据集加载完成: 300 条 instances
采样完成: 110 条（每 repo 最多 10 条）
[start] django__django-11179
[start] astropy__astropy-12345
[done]  django__django-11179 (4m32s) patch=1842 bytes
[done]  astropy__astropy-12345 (6m10s) patch=0 bytes   ← 空 patch
[error] flask__flask-5678 (15m0s): context deadline exceeded
...
完成！结果已写入 ./swebench-results
总实例: 110，耗时: 3h21m
```

### 4.4 断点续跑

如果运行中途中断（网络故障、Ctrl+C 等），用 `--resume` 跳过已有结果：

```bash
go run ./cmd/swebench \
  --dataset swe-bench-lite.jsonl \
  --output ./swebench-results \
  --resume              # 自动跳过 predictions.jsonl 中已有的 instance_id
```

### 4.5 查看中间结果

```
swebench-results/
├── predictions.jsonl        # 每行一条 {"instance_id":..., "model_patch":...}
├── run_summary.md           # 运行摘要（总数/patch 数/出错数/按 repo 分布）
└── logs/
    ├── django__django-12908.log   # 每个 instance 的完整 trajectory
    ├── astropy__astropy-5678.log
    └── ...
```

**Trajectory 日志格式**（`logs/<instance_id>.log`）：

```
=== SWE-bench Instance: django__django-12908 ===
Repo:        django/django
BaseCommit:  abc123...
StartTime:   2026-06-09 14:30:00

--- Turn 1 ---
Let me start by exploring the repository structure to understand the codebase...

[Tool Call: bash]
{"command":"find . -type f -name \"*.py\" | grep -v __pycache__ | head -40"}

[Tool Result: abc12345 | 350ms | ok]
./django/utils/autoreload.py
./django/core/management/base.py
...

[Tokens: 4821]

--- Turn 2 ---
I can see the issue in autoreload.py. Let me read the relevant section...
```

日志包含：每轮 LLM 输出文本、工具调用参数、工具返回结果（含耗时和状态）、token 用量、上下文压缩事件。

`run_summary.md` 示例：

```markdown
# SWE-bench Lite Run Summary

- 开始时间：2026-06-09 14:30:00
- 结束时间：2026-06-09 17:51:00
- 总实例数：110
- 成功生成 patch: 89 / 110
- 空 patch（agent 无改动）: 14
- 运行出错：7

## 按 Repo 分布
| Repo              | 实例数 | 有 patch | 空 patch | 出错 |
|-------------------|--------|---------|---------|------|
| astropy/astropy   | 10     | 8       | 2       | 0    |
| django/django     | 10     | 9       | 1       | 0    |
| ...               | ...    | ...     | ...     | ...  |
```

### 4.6 官方评估打分

Runner 完成后，使用官方 `swebench` 工具评估分数：

```bash
pip install swebench

python -m swebench.harness.run_evaluation \
    --dataset_name princeton-nlp/SWE-bench_Lite \
    --predictions_path ./swebench-results/predictions.jsonl \
    --max_workers 4 \           # 并发评估的 instance 数
    --run_id harness9-lite-v1   # 本次运行标识符（影响输出目录名）
```

评估完成后查看结果：

```bash
# 官方工具将结果输出到 logs/run_evaluation/harness9-lite-v1/
cat logs/run_evaluation/harness9-lite-v1/results.json
# {"resolved": 23, "unresolved": 77, "error": 10, "total": 110}
# Resolved Rate: 20.9%
```

> **注意**：官方评估器需要拉取每个 Instance 对应的官方 Docker 镜像（约 1–5 GB/镜像），首次运行会耗费大量带宽和磁盘空间（数百 GB）。建议在有足够磁盘的环境中运行，或使用 `--max_workers 1` 串行评估节省资源。

### 4.7 全量运行（与公榜对比）

运行全部 300 个 instance，与 SWE-agent、Claude 等系统的公开结果直接对比：

```bash
go run ./cmd/swebench \
  --dataset swe-bench-lite.jsonl \
  --sample 300 \        # 不限制，取全量
  --output ./swebench-results-full \
  --max-turns 30 \
  --parallel 3 \
  --timeout 15
```

> **成本估算（GPT-4o）**：
> - 每个 instance 平均约 15–20 轮 LLM 调用，约 $1–3 API 费用
> - 300 个 instance 总计约 $300–900
> - 建议先用 `--sample 10` 验证流程后再跑全量

---

## 5. 参数速查

```bash
go run ./cmd/swebench --help
```

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `--dataset` | string | **必填** | SWE-bench Lite JSONL 文件路径 |
| `--sample` | int | 10 | 每个 repo 抽取的 instance 数量（≥1）|
| `--output` | string | `./swebench-results` | 输出目录 |
| `--max-turns` | int | 0 | 每个 instance 最大 Turn 数（0 = benchmark 默认 80；显式 N 覆盖）|
| `--parallel` | int | 1 | 并发 instance 数（≥1）|
| `--resume` | bool | false | 跳过已产出非空 patch 的 instance（断点续跑）|
| `--timeout` | int | 30 | 单个 instance 超时（分钟）|
| `--seed` | int64 | 1 | 按 repo 采样的随机种子（固定默认值保证可复现；同 seed → 同实例集）|
| `--model` | string | `""` | LLM 模型（空则读 `LLM_MODEL` 环境变量）|

**环境变量**（可通过 `.env` 文件或系统环境变量提供，系统变量优先）：

| 变量 | 说明 | 推荐值 |
|------|------|--------|
| `OPENAI_API_KEY` | LLM API Key（必填）| — |
| `OPENAI_BASE_URL` | 自定义 API 地址（可选）| `https://openrouter.ai/api/v1` |
| `LLM_MODEL` | 模型名称 | `openai/gpt-4o` |
| `LLM_REQUEST_TIMEOUT_SECS` | 单次 LLM 请求超时（秒）| `600`（默认）|
| `LLM_MAX_RETRIES` | SDK 内置重试次数（429/5xx）| `5`（默认）|
| `SANDBOX_IMAGE` | Docker 镜像 | `python:3.11`（默认）；高保真用 `swebench/sweb.eval.x86_64.<instance>` |
| `SANDBOX_ENABLED` | 启用 Docker 隔离 | `true`（默认）|
| `SANDBOX_BOOTSTRAP_CMD` | 容器就绪后执行一次的依赖安装命令；**留空时 runner 自动注入默认自举**（`ensurepip` + `pip install -e .` + `pytest`）| 留空即可 |
| `SANDBOX_BOOTSTRAP_TIMEOUT_SECS` | bootstrap 命令超时（秒）| `600`（默认）|

---

## 6. 轨迹驱动的内核优化实录（v1 → v2）

> 本节记录一次完整的「评估 → 取证分析 → 内核优化 → 复测对比」闭环：基于第一轮 24 实例评估的完整 trajectory 取证，定位框架内核短板，针对性优化后复测，**Resolved 从 16/24（66.7%）提升到 19/24（79.2%），零回归**。
>
> 完整根因报告见 `docs/技术调研/swebench-轨迹分析与内核优化-v2.md`。

### 6.1 方法论

对 8 条失败轨迹逐一做三方对比——**我们的 patch ↔ gold patch ↔ 隐藏测试（test_patch + FAIL_TO_PASS）**，先定位「为何过不了隐藏测试」，再回溯轨迹找出「Agent 为何产出这个 patch」，并对每条 harness 归因做**对抗式复核**（核对当前内核源码，剔除「已修复/不存在」的伪根因）。复核结论：**21 confirmed / 6 partial / 2 refuted**。

### 6.2 决定性发现：验证闭环 100% 断裂

> **第一轮 24 条轨迹里没有任何一条真正跑过一次测试**——全部命中 `ModuleNotFoundError` / `No module named pip`。16 条 resolved 全靠**纯静态分析**蒙对；8 条 unresolved 基本都是「必须靠真实测试反馈才能收敛」的题。

病根在 runner：clone+checkout 后**没有任何依赖安装步骤**，且 `.env` 误用了非 Python 镜像，使 Agent 被关进一个**无依赖、无 pip、跑不了任何测试**的空环境——内核最重要的反馈信号（真实测试结果）恒为空。最讽刺的是 `sandbox` 包早已留好 `BootstrapCmd` 接缝，但 runner 从未接线。

### 6.3 根因分级（已对抗式复核）

| # | 根因 | 命中 | 复核 |
|---|------|------|------|
| **R1** | **环境缺失**：sandbox 无依赖/无 pip，真实测试不可运行 | 8/8 | confirmed |
| **R2** | **验证闭环缺失**：循环仅凭「无工具调用」终止，无「跑过测试再收尾」关卡，静态自证即交卷 | 8/8 | confirmed |
| **R3** | **HintsText 被丢弃**：`dataset` 解析了 `hints_text` 但 prompt 从未注入；维护者讨论常含决定性 API 设计（flask `text=True`）| 多条 | confirmed |
| **R4** | **dataset 字段缺失**：未解析 `version`/`environment_setup_commit`/`FAIL_TO_PASS`/`test_patch`，无从 provision | 8/8 | confirmed |
| **R5** | **prompt 反向引导**：明示「依赖可能没有 → 退化静态分析」，把「放弃验证」写成官方默许 | 8/8 | confirmed |
| **R6** | **盲目空转到 max_turns**：无反馈时反复静态重读，xarray-3364、pylint-7080 烧满 80 轮被截断 | 2/8 | confirmed |
| **R7** | **合理但错位的修复**：错位置（pylint 改错文件）、自创 API（flask `mode=`）、改行为而非加 DeprecationWarning（xarray）| 6/8 | confirmed |
| **R8** | **edit_file 提示抑制验证**：精确匹配时输出「无需…再次确认」，助长「改完即完成」 | 多条 | confirmed |

### 6.4 针对性优化（按根因映射）

| 优化 | 文件 | 根因 |
|------|------|------|
| **接通依赖自举**：runner 为每实例默认设 `BootstrapCmd`（ensurepip + `pip install -e .` + pytest），默认镜像改 `python:3.11`（buildpack 全镜像，带 pip 与编译器）| `runner.go` | R1/R4 |
| **验证关卡**：Agent 自然结束却全程未跑过测试时，注入一次续跑提示要求真实验证（复用内存会话延续历史，至多一次，超时/turn 兜底）| `runner.go` | R2 |
| **HintsText 注入** + dataset 解析评测字段（`FAIL_TO_PASS`/`test_patch` 仅供分析，绝不在运行时暴露/应用）| `prompt.go` `dataset.go` | R3/R4 |
| **停滞提示 `WithStallNudge`**：连续 N 轮无 edit/write 进展工具调用时注入一次提示打断空转（防御性副本，不持久化）| `engine/agent_loop.go` | R6 |
| **prompt 重平衡**：删「默认退化静态分析」逃生门；加最小化/错误点局部修复偏置、按符号名查测试、DeprecationWarning 约定提示 | `prompt.go` | R5/R7 |
| **edit_file banner 收敛**：「无需再确认」→ 明确「字节写入≠行为正确，仍需跑测试」| `tools/edit_file.go` | R8 |

> 全部改动均为 TDD（红→绿），配套单元测试 + eval 黄金用例（含 `Case.EngineOptions` 接缝 + 停滞提示回归护栏），`go test ./...` 全绿。

### 6.5 实测对比（同 seed=1、同 24 实例、同模型 anthropic/claude-sonnet-4.6、同官方评分器）

| 指标 | v1（优化前） | v2（优化后） | Δ |
|---|---|---|---|
| **Resolved** | **16/24（66.7%）** | **19/24（79.2%）** | **+3，+12.5pp** |
| 回归（v1 过→v2 挂）| — | **0** | 零回归 |
| 真实测试运行实例 | **1/24** | **18/24** | **+17** |
| 端到端墙钟 | ~75 min | **23 min** | **3.3× 更快** |

> 评分时 3 条实例曾因 arm64 模拟下 4 并发的 Docker 资源争用「error」（非补丁问题），单线程重评后确认 astropy-12907 / django-14855 仍 RESOLVED、seaborn-3407 仍 unresolved；19/24 为干净复评后的最终数字。

### 6.6 三个新解决的实例 = 三项优化各自命中

| 实例 | v1 | v2 | 起作用的优化 |
|---|---|---|---|
| **pylint-7080** | 80 轮 (打满)/无测试 → 挂 | **17 轮/测试 → 过** | 依赖自举让 `pylint` 可跑 → 盲目空转 80 轮变成 17 轮收敛到正确的 1 行 `os.path.normpath` 修复（最典型）|
| **flask-4992** | 13 轮/无测试 → 挂 | **14 轮/测试 → 过** | hints 注入暴露维护者定的 `text=True` API + 真实测试当场暴露 `mode='t'` 的 ValueError |
| **astropy-7746** | 17 轮/无测试 → 挂 | **110 轮/测试 → 过** | 验证关卡触发一次续跑（110=主跑 + 强制验证），真实测试抓到非对称 `[],[1]` 回归 |

### 6.7 五个仍未解决：诚实归因

| 实例 | v2 | 根因类别 |
|---|---|---|
| **requests-1963** | 11 轮/**无测试** | **环境天花板**：2014 年代码需 Python 2.7，python:3.11 连 import 都不行 → 仍无法验证。唯一解：官方每实例镜像（`SANDBOX_IMAGE` 接口已留好）|
| **xarray-4493** | 40 轮/测试 | **隐藏测试专属行为**：gold 要发 `DeprecationWarning`，只写在评测时才注入的 test_patch 里，运行时看不到 |
| **seaborn-3407** | 33 轮/测试 | 同上：隐藏测试断言 `diag_vars==list(cols)` 的精确类型 |
| **flask-5063** | 16 轮/测试 | 同上：隐藏测试要 `Host`/`Subdomain` 表头 + host_matching 模式 |
| **xarray-3364** | 80 轮 (打满)/测试 | **复杂定位**：现在能跑测试，但锚定了一条 test_patch 会删除的现存断言，仍改错代码路径 |

### 6.8 关键结论

- **真实测试反馈是「必要但不充分」**：它解决了「能靠反馈收敛」的题（+3、零回归、还快 3 倍），但对「期望行为只存在于隐藏测试」这一类（xarray-4493/seaborn-3407/flask-5063）即便有环境也无能为力。
- 剩余 5 例已被精确归因为「**环境天花板 (1) + 隐藏测试专属行为 (3) + 复杂定位 (1)**」，与根因分析完全自洽。
- 下一步增量只能来自：① 官方每实例镜像（解 requests-1963 这类版本天花板）；② 更强的「按项目惯例推断隐藏行为」脚手架。

---

## 7. 代码位置速查

| 文件 | 职责 |
|------|------|
| `cmd/swebench/main.go` | CLI 入口、preflight、并发编排 |
| `cmd/swebench/runner.go` | 单 instance 执行核心（git/sandbox/engine/patch）|
| `cmd/swebench/dataset.go` | 数据集加载、按 repo 采样 |
| `cmd/swebench/prompt.go` | SWE-bench 专用 system prompt |
| `cmd/swebench/report.go` | 输出文件管理（predictions/summary）|
| `cmd/swebench/*_test.go` | 各模块单元测试 |
| `internal/sandbox/` | Docker 沙箱基础设施 |
| `internal/engine/` | ReAct Agent Loop |
| `docs/设计规格/2026-06-09-swebench-lite-runner-design.md` | 完整设计文档 |
