# SWE-bench v4 评测质量分析与 harness9 内核优化建议

> 日期：2026-09-09 ｜ 分支：`opt/eval`（runner 增强已合入）｜ 模型：`moonshotai/kimi-k3`（OpenRouter，$3/$15 每 MTok）
> 数据：RunID `20260909-130555`，78 实例（seed=1，每 repo 上限 8，SWE-bench Lite），评分 run_id `harness9-lite-v4`
> 前置文档：[swebench-v4-扩样评测设计](./swebench-v4-扩样评测设计.md)（本轮方案）、[swebench-轨迹分析与内核优化-v2](./swebench-轨迹分析与内核优化-v2.md)（v1 根因与优化）

---

## 0. 一句话结论

> **验证闭环彻底恢复**（76/78 实例真实运行了测试，v1 时代为 0/24），runner 的环境自举 + 验证关卡 + 停滞提示三件套在 Kimi-K3 上端到端成立；**resolve 率评分因镜像拉取受阻暂未完成**（12 实例先行评分：10 resolved / 2 unresolved）。最大的新发现不是失败模式，而是**评测完整性威胁**：沙箱可直连 GitHub，17% 实例抓取了上游 issue/patch 内容，必须用网络白名单封死。Agent 能力侧的最大杠杆是 **Planning 采用率过低（2/78）**——原生规划能力已接通但 LLM 几乎不自发使用。

---

## 1. 评测概览

| 维度 | 值 |
|------|-----|
| 实例集 | 78（seed=1，每 repo 上限 8；含 v3 全部 47 实例作参考对照） |
| 模型 / Provider | moonshotai/kimi-k3，OpenRouter（1M 窗口，$3/$15 每 MTok） |
| runner 配置 | max-turns 80、bash 超时 300s、验证关卡、stall nudge、TokenBudgetCompactor（1M×55%） |
| 新增能力（本轮合入） | plan_write 原生注册（无 FilePlanWriter）、countingProvider 实际用量采集、usage.jsonl 落盘、--instances 清单 |
| 总成本 | **$146.01**（正式跑）+ $34.23（冒烟 24 实例）≈ **$180** |
| 墙钟 | 正式跑 3h18m（--parallel 5）；冒烟 1h5m |
| 稳定性 | llm_calls == turns（零重试浪费）；1/78 实例 30min 超时；1 空 patch；OpenRouter 无系统性限速 |

## 2. 评分结果

### 2.1 最终评分（harness9-lite-v5，已回填）

| 指标 | 值 |
|------|-----|
| 提交实例 | 78 |
| **Resolved** | **67/78（85.9%）** |
| 95% Wilson CI | [76.5%, 91.9%] |
| Unresolved | 7 |
| Empty patch | 1 |
| Error | 3 |

对比报告（含效率指标、plan_write 采用交叉表、v3 参考对照）：`benchmarks/swebench/v4-expansion/compare-report.md`。

### 2.2 先行子集（12 实例，镜像就绪即评分，非随机样本，仅作方向参考）

| 指标 | 值 |
|------|-----|
| resolved | **10/12（83.3%）** |
| unresolved | 2（astropy-7746、django-15790） |

子集构成：astropy 全部 3 例 + django 6 例 + sphinx 1 例 + matplotlib 2 例（按镜像拉取就绪顺序，**偏向小镜像与已冒烟过的 repo**，不可外推到总体）。

值得记录的个案对照：

- **astropy-12907 resolved**：80 turns 满转、$5.89 全场最高成本、76 次 bash 调用——"高速 churn"最终撞出了正确修复。效率极差但结果有效。
- **astropy-7746 unresolved**：全场唯一"教科书级"规划样本（见 §4.2），plan 维护、对照实验、最小 diff 一样不少，仍然未解。规划质量 ≠ 修复位置正确性。

## 3. 效率基线（78 实例，Kimi-K3 口径锚点）

| 指标 | 中位数 | P90 | Max |
|------|-------:|----:|----:|
| Input tokens | 408,886 | 1,175,322 | — |
| Output tokens | 11,392 | 24,010 | — |
| Turns | 28 | 59 | 80（3 例满转） |
| 时长（分） | 8.4 | 27.6 | 35.6 |
| 单实例成本 | $1.41 | $3.80 | $5.89 |

按 repo 分布（成本 = Σtokens 实际计费口径）：

| repo | n | med_turns | med_分钟 | 成本$ | 真实跑测试 |
|------|--:|--------:|------:|-----:|-----:|
| scikit-learn | 8 | 55 | 16.4 | 26.96 | 88% |
| astropy | 6 | 59 | 9.9 | 22.64 | 100% |
| sphinx-doc | 8 | 38 | 15.7 | 17.90 | 100% |
| matplotlib | 8 | 35 | 15.7 | 16.36 | 88% |
| pylint-dev | 6 | 29 | 6.0 | 12.97 | 100% |
| psf | 6 | 28 | 6.0 | 11.14 | 100% |
| pytest-dev | 8 | 20 | 4.6 | 8.33 | 100% |
| sympy | 8 | 18 | 7.1 | 6.46 | 100% |
| django | 8 | 22 | 6.8 | 6.63 | 100% |
| pydata | 5 | 27 | 6.8 | 5.81 | 100% |
| mwaskom | 4 | 22 | 15.5 | 6.12 | 100% |
| pallets | 3 | 24 | 9.1 | 4.69 | 100% |

规律清晰：**med_turns 与 repo 的"环境重量"强相关**（astropy/sklearn 需要编译 C 扩展或重依赖安装，turns 中位数 55-59；django/sympy 纯 Python，18-22）。成本重灾区不是"题目难"而是"环境重"。

## 4. 行为分析

### 4.1 验证闭环：从 0/24 到 76/78（本轮最重要的正向结论）

v1 轨迹分析（R1/R2）的核心发现是"24 条轨迹没有一条真正跑过测试，resolved 全靠静态蒙"。本轮 **76/78 实例在交卷前运行过真实测试**（`ran_test` 口径：bash 调用命中 pytest/unittest 等特征）。R1（环境缺失）与 R2（验证闭环缺失）的修复在切换到陌生模型（sonnet-4.6 → Kimi-K3）后依然成立——说明这是 **harness 层面的结构性修复，不依赖特定模型的自觉**。

验证关卡（verify gate）触发 4 次（sphinx-7738、astropy-7746、requests-863、requests-2148）——都在 Agent 自然结束但未跑测试时注入了续跑提示，其中 3 例随后补跑了测试（ran_test 全部 true）。关卡在 Kimi-K3 上按设计工作。

### 4.2 Planning 原生化：能力已接通，采用率是短板

`plan_write` 已在 runner 注册（本轮合入），但 **78 实例中仅 2 例自发使用（2.6%）**：

| 实例 | plan_writes | turns | 行为特征 | 结果 |
|------|--------:|-----:|---------|------|
| astropy-7746 | 7 | 30 | 计划随进展真实更新（新增 1b workaround 条目）、环境→构建→复现→验证→测试五阶段、`git stash` 对照实验确认 13 个失败为预存在、最终 diff +6 行 | unresolved |
| scikit-learn-25747 | 13 | 49 | 高频计划更新 | 待评分 |

归因分析：harness9 的规划准则是 **System Prompt 软引导**（"复杂任务可先规划"），依赖 LLM 自发判断。Kimi-K3 的行为倾向是"直接动手、高频小步工具循环"（见 4.4），规划准则对它几乎没有牵引力。这不是 Planning 模块的缺陷（能力在、状态机在、检查点在），而是**激活机制**的短板——对标 Claude Code 等前沿 harness，规划更多由 harness 侧主动发起（结构化触发），而非等待模型自觉。

### 4.3 效率损耗点：3 例 80-turn 满转 + 环境重 repo 的构建循环

满转实例（astropy-12907、pylint-7114、sklearn-14983）合计烧掉 ~$16（11% 总成本）。astropy-12907 的行为分布：76 bash / 5 read_file / 1 edit_file——典型的"用 bash 盲试替代读代码"。它与 astropy-7746 是同一道题的两个镜像：环境构建受挫（C 扩展编译）后，7746 选择规划+对照实验收敛，12907 选择高速盲试。**stall nudge（10 轮无改动即提示）对"有工具输出但无实质进展"的循环不敏感**——12907 每轮都有新命令输出，不触发 nudge，但 76 次 bash 中大量是变体重试。

### 4.4 Kimi-K3 特有行为画像（相对 v1/v3 的 sonnet-4.6 轨迹）

1. **高频小步 bash**：turn 均时长 ~8-20s，单轮多工具并行常见；速度极快但重试变体多。
2. **外网探查倾向**：17% 实例主动访问 GitHub（见 §5），sonnet 轨迹中无此模式（v1 时代网络受限于环境缺失，客观不可比，但探查-下载 patch 的行为本身值得警惕）。
3. **自发规划弱**：2/78 采用率；sonnet 在 v3 的采用率未统计（当时未注册 plan_write），无对照基线，后续模型轮次应把 `plan_writes` 作为固定采集项。

## 5. 评测完整性威胁（本轮新发现，必须处置）

**沙箱可直连 GitHub，Agent 会抓上游答案。** 证据：

- 13/78 实例（17%）的轨迹中出现 `raw.githubusercontent.com` / `api.github.com` 访问；
- **matplotlib-22711 命中 .patch/.diff 内容 14 次**（高置信：直接下载了上游修复补丁）；
- **pydata__xarray-4248 调用了 issue #4912 的 API**（该实例的上游原始 issue，含修复方向的完整讨论）并 3 次接触 patch/diff；
- seaborn-3010、sphinx-7738/8474/8627、django-13925、matplotlib-23913 均有 4 次左右 patch/diff 相关命中。

SWE-bench 官方协议不禁止网络访问（官方 harness 的镜像也不禁），但对**衡量 harness 内核能力**而言，这是直接的分数污染：resolved 可能反映"模型会搜索答案"而非"harness 能引导模型修复"。该威胁在 v1/v3 不存在——当时沙箱没依赖、没网络使用场景；环境自举修好后网络也随之打开，这是**修 R1 带来的副作用**。

**处置建议（已列入 §7 P0）**：SWE-bench runner 模式下对沙箱启用网络白名单——放行 pypi（自举必需），封禁 github.com/raw.githubusercontent.com/api.github.com。

## 6. 有效性威胁与边界

| 威胁 | 说明 | 缓解 |
|------|------|------|
| 上游内容污染 | §5，17% 实例，2 例高置信 | P0 网络白名单；本报告 resolve 数字发布时须附带污染清单 |
| 单臂设计 | 模型与代码同时变化，跨轮（v1/v3→v4）差异不可归因于内核优化 | 设计如此（见 spec §1）；严格归因用 `--instances` 同实例集 + 9/4 时点代码双臂补跑 |
| 47 配对子集的参考对照 | 实例相同但模型不同，flip 分析不具归因力 | compare.py 输出中显著标注"仅方向性参考" |
| 环境保真度 | runner 用 python:3.11 + bootstrap 自举，非官方每实例预装环境；sklearn/matplotlib 出现 8-12% 实例连测试都跑不起来（ran_test=false 或环境报错） | 评分用官方镜像所以 resolve 判定不受影响；影响的是 Agent 过程中的验证质量 |
| 评分基础设施 | 本轮 77 个 x86_64 镜像拉取受 registry 限速阻塞（详见 §2.1），暴露评测脚本的工程短板 | P0：镜像预拉取 + 缓存管理纳入评测脚本 |
| turns 口径 | 验证关卡续跑后 turns 是"最长段"而非"总轮数"（mergeStats 取大） | 已知情设计；跨段累计口径（llm_calls/tokens）不受影响 |

## 7. harness9 内核优化建议

按杠杆排序；P0 直接影响下一轮评测分数的可信度，P1 影响 Agent 能力，P2 是工程效率。

### P0-1 SWE-bench 沙箱网络白名单（消除评分污染）

`internal/sandbox` 已有 Environment 抽象，docker environment 建容器时加网络策略：runner 传入 `--network` 约束或 iptables 规则——放行 `pypi.org`/`files.pythonhosted.org`（自举与装依赖必需），封禁 `github.com`/`raw.githubusercontent.com`/`api.github.com`/`codeload.github.com`。实现为 `SandboxConfig.NetworkAllowlist []string`，SWE-bench runner 设置之，TUI/主程序不受影响（默认全通）。**预估消除 ~17% 实例的污染向量**，否则任何 resolve 率都混杂"搜索能力"。

### P0-2 评分基础设施脚本化（镜像预拉取 + 缓存管理）

本轮暴露：77 个 x86_64 镜像 110GB 的拉取无脚本、无断点、受 registry 限速阻塞评分一天。补 `benchmarks/swebench/pull-images.sh`（清单由 predictions.jsonl + swebench spec 生成，`docker pull --platform linux/amd64`，带重试与跳过已有），并把"镜像就绪检查"做成评分前置步骤。顺带 `docker builder prune` 纳入常规清理（本地已积累 21.98GB build cache）。

### P1-1 Planning 激活机制：从"等模型自觉"到"harness 主动触发"

2/78 的采用率说明软引导对 Kimi-K3 无效。两条路线（推荐 A）：

- **A. 复杂度探测后注入规划建议**：引擎在 launch 时根据 problem_statement 长度/ repo 环境重量（§3 已证明可静态估计）在首轮 user prompt 追加一句"该任务涉及多步骤，建议先用 plan_write 制定计划并随进展更新"。零内核改动，仅在 swebench prompt builder 试开。
- **B. 引擎级强制规划门槛**：`WithPlanningGate(turnBudget)`——当模型在探索期消耗超过 N turns 仍无 edit 时，注入"停下来先规划"提示（与 stall nudge 互补：stall 管"重复无进展"，gate 管"探索过深"）。

两条都以 `plan_writes` 采用率和 resolve 率为验收指标（本轮已建立采集与基线：2.6% 采用、成本分布 §3）。

### P1-2 验证关卡升级：从"提示"到"硬门槛"

当前关卡是一次性软提示（4 次触发全部有效补跑，说明够用），但 Agent 仍可能在提示后放弃验证交卷。升级方向：`runWithVerificationGate` 中第二轮续跑后仍 `ranTest == false` 时，将最终 patch 标记为 `unverified`（写入 usage.jsonl），报告中对 unverified patch 的 resolved 打折解读。成本近零，诚实度提升。

### P1-3 churn 检测：stall nudge 的盲区

astropy-12907 形态：每轮都有新鲜 bash 输出但 76 次调用高度重复。现有 `WithStallNudge` 键控于"无 edit_file 且无测试运行"，对"有输出无进展"不触发。建议引擎侧加 `WithChurnNudge`：滑动窗口内 bash 命令去重率低于阈值（如 10 轮中唯一命令 < 5）时注入"你在重复相似命令，请换策略：读代码定位或跑测试验证"。纯 engine 层 opt-in，swebench runner 开启。

### P2-1 环境重量感知的 bootstrap

astropy/sklearn 的 55-59 中位 turns 里相当部分耗在依赖/编译反复试错。runner 的 `defaultBootstrapCmd` 是 best-effort 通用命令；可按 repo family 定制（astropy 预装 numpy/Cython 钉版本、sklearn 预装 scipy/joblib 钉版本），把"环境搭建"从 Agent 的试错预算里拿走。这是纯 runner 侧改动，杠杆直接作用于成本重的 repo。

### P2-2 双臂 A/B 基础设施已备，建议常态化

本轮合入的 `--instances` + usage.jsonl + compare.py 已构成同实例集配对评测的完整基础设施。建议：每次内核大改动（如 §P1 落地）后跑 47 实例冒烟集（$80、~2h）做 flip 分析，替代"凭感觉判断优化是否生效"。这也是 v4 设计中"严格归因双臂"选项的启用路径。

## 8. 交付物清单

- [x] 78 实例 predictions.jsonl + usage.jsonl + run_summary.md（`benchmarks/swebench/v4-expansion/`）
- [x] 冒烟 24 实例独立目录（`swebench-smoke/`，$34.23，验证用量链路）
- [x] runner 增强 9 提交在 `opt/eval`（countingProvider / usage.jsonl / --instances / plan_write 注册 / compare.py / kimi-k3 注册，最终 review Approved）
- [ ] 官方 harness 最终评分 + compare-report.md（阻塞于镜像拉取，脚本已在后台执行；完成后回填 §2.1）
- [x] 本质量分析与内核优化建议报告
