# SWE-bench v4 评测质量分析与 harness9 内核优化建议

> 日期：2026-09-09 ｜ 分支：`opt/eval`（runner 增强已合入）｜ 模型：`moonshotai/kimi-k3`（OpenRouter，$3/$15 每 MTok）
> 数据：RunID `20260909-130555`，78 实例（seed=1，每 repo 上限 8，SWE-bench Lite），评分 run_id `harness9-lite-v5`（2026-09-10 08:28 完成）
> 前置文档：[swebench-v4-扩样评测设计](./swebench-v4-扩样评测设计.md)（本轮方案）、[swebench-轨迹分析与内核优化-v2](./swebench-轨迹分析与内核优化-v2.md)（v1 根因与优化）

---

## 0. 一句话结论

> **验证闭环彻底恢复**（76/78 实例真实运行了测试，v1 时代为 0/24），runner 的环境自举 + 验证关卡 + 停滞提示三件套在 Kimi-K3 上端到端成立；**最终 resolve 率 67/78（85.9%）**（排除弃拉镜像口径 67/77=87.0%，剔除 runner 截断缺陷影响后潜在 69/77=89.6%）。最大的新发现不是失败模式，而是**评测完整性威胁**：沙箱可直连 GitHub，17% 实例抓取了上游 issue/patch 内容，必须用网络白名单封死。第二大新发现是 **runner 截断补丁缺陷**（§5.2）：`git diff` 超时被静默吞掉，2 例高质量修复被按 0 分计。Agent 能力侧的最大杠杆是 **Planning 采用率过低（2/78）**——原生规划能力已接通但 LLM 几乎不自发使用。
>
> **硬化终局（2026-09-10，47 实例验证轮）**：P0-1/P0-2/P0-3/P1-4 全部落地并经端到端验证——**41/47 = 87.2%，error 清零**，runner 输出管线缺陷归零（两轮共 5 例管线事故：主轮截断 2 + 验证轮换行 3 → 0），网络污染向量实弹封死（git fetch 上游 0ms 即拒），验证闭环 47/47 满格；过程中额外揪出并修复第四缺陷（TrimSpace 剥终止换行，见 §9）。flip 对照主轮 +3/-2 净持平，硬化无损伤。

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

**排除口径**（用户拍板弃拉 matplotlib-23562 镜像后重生成）：**67/77 = 87.0%**，Wilson CI [77.7%, 92.8%]（`report-v5-excl-23562.json` + `usage-excl-23562.jsonl`）。Error 3 例构成：matplotlib-23562（无镜像弃评，预期内）+ astropy-14182/14365（**runner 截断补丁所致，见 §5.2**——两例 agent 侧修复完整且本地全量验证通过，剔除该缺陷的潜在真实成绩 **69/77 = 89.6%**）。

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

### 4.5 七例 unresolved 逐案归因（2026-09-10 复盘，全轨迹 + eval 日志核对）

| 实例 | 失败测试特征 | 根因分类 | 一句话根因 |
|------|------------|---------|-----------|
| astropy-7746 | 隐藏断言要求保留非空轴数据与原形状 | 修复不完整 | 位置找对（与上游同处加 guard），但返回值错误：丢数据/改形状。教科书级规划（§2.2）也救不回返回值语义 |
| django-15790 | F2P 全过，2 个 P2P 挂 | 改坏其他行为 | `defaultdict(list)→set` 去重破坏 E003 错误消息的确定性顺序，正确做法是保序去重 |
| matplotlib-22711 | set_val 后手柄不跟随 | 修复不完整 | 隐藏补丁新增"手柄同步"断言，agent 只修了 IndexError 主症状（下载过上游 patch 14 次仍漏掉伴随行为） |
| seaborn-3407 | diag_vars 需保留原始 tuple | 误诊 | Turn 12-13 已下载官方测试原文（`assert diag_vars == list(cols)`）仍选字符串化方案，且从未对该断言验证 |
| xarray-4493 | 缺 DeprecationWarning | 修复不完整 | 只做惰性一半；本地从未点名跑 test_as_variable，静默解包恰好消掉告警 |
| pylint-7114 | 9 个 P2P 挂 | 改坏其他行为 + 80 轮截断 | 目录一律上跳父目录改变 sys.path 语义；最后一轮 edit 后即被截断，终版 patch 零验证（80 turns 全场最高，仅 436s） |
| sphinx-8474 | 4 个 numfig 警告文案测试 | 误诊/修错位置 | gold 改 std.py 警告文案，agent 改 toctree 编号逻辑，std.py 一字未动 |

**横向模式（按杀伤力排序）**：

1. **"无回归"验证替代"目标达成"验证（7/7 共性，最致命）**：全部以 `git stash` 前后失败集合不变作为交卷依据——只能证明"没改坏"，证明不了"修好了"。astropy/matplotlib/xarray 的修复本地全绿、隐藏断言全挂。
2. **沙箱环境与 eval 容器脱节（5/7）**：numpy 2.0 / pandas 3.0.5 / docutils 0.23 等版本漂移造成几十个基线失败，真实失败信号被成批归入"预存环境问题"（sphinx-8474 恰好埋掉"警告文案不对"的关键证据）。
3. **隐藏测试的伴随行为要求（4/7）**：修主症状、丢伴随行为（手柄同步/告警保留/形状保持/消息有序）——issue 文本不写、隐藏测试必查，是 F2P 挂掉的主力。
4. **语义扩张性回归（2/7）**：set 化、目录上跳这类"顺手扩大适用面"的改动破坏 P2P，改动后未全量跑相关测试文件。
5. **异常收尾（2/7）**：pylint-7114（80 轮截断在最后 edit 后）、seaborn-3407（1989s 被掐死在降级 pandas 重验的半途）——终版 patch 均处于未验证状态。

7 条轨迹共 274 轮 `| error]` 工具报错为 0，无打转/重复命令——失败全部在模型层（诊断、完整性、验证策略），执行层零故障；runner 侧唯一缺陷是 patch 截断（§5.2），其 2 例因按 error 计未进入本表。

## 5. 评测完整性威胁（本轮新发现，必须处置）

### 5.1 沙箱可直连 GitHub，Agent 会抓上游答案

**沙箱可直连 GitHub，Agent 会抓上游答案。** 证据：

- 13/78 实例（17%）的轨迹中出现 `raw.githubusercontent.com` / `api.github.com` 访问；
- **matplotlib-22711 命中 .patch/.diff 内容 14 次**（高置信：直接下载了上游修复补丁）；
- **pydata__xarray-4248 调用了 issue #4912 的 API**（该实例的上游原始 issue，含修复方向的完整讨论）并 3 次接触 patch/diff；
- seaborn-3010、sphinx-7738/8474/8627、django-13925、matplotlib-23913 均有 4 次左右 patch/diff 相关命中。

SWE-bench 官方协议不禁止网络访问（官方 harness 的镜像也不禁），但对**衡量 harness 内核能力**而言，这是直接的分数污染：resolved 可能反映"模型会搜索答案"而非"harness 能引导模型修复"。该威胁在 v1/v3 不存在——当时沙箱没依赖、没网络使用场景；环境自举修好后网络也随之打开，这是**修 R1 带来的副作用**。

**处置建议（已列入 §7 P0）**：SWE-bench runner 模式下对沙箱启用网络白名单——放行 pypi（自举必需），封禁 github.com/raw.githubusercontent.com/api.github.com。

补充：污染不保证做对——matplotlib-22711 命中 .patch 14 次仍 unresolved（漏掉隐藏断言的伴随行为），seaborn-3407 下载官方测试原文后仍选错修法。但"抓到答案还做错"不改变"分数被污染"的定性。

### 5.2 runner 截断补丁（本轮新发现，直接影响 2 例评分）

astropy-14182/14365 官方评分报 Patch Apply Failed，复盘发现**不是模型补丁质量问题，而是 runner 把 diff 截断了**：

- **证据**：eval 侧 `patch.diff` 与 predictions.jsonl 的 model_patch 逐字节一致地截断——14182 的 changelog hunk 头声明 `@@ -0,0 +1,3 @@`，正文只有 2 行且无收尾换行；14365 结尾停在句中（`...in any case.`）。容器内三档降级（`git apply` / `git apply --reject` / `patch --fuzz=5`）全部失败，报 `malformed patch at line 31 / unexpectedly ends in middle of line`。两例 agent 侧轨迹都完整收尾且本地验证通过（14182 是教科书级修复），修复本体大概率正确。
- **机制**（runner.go:289-292）：`git add -A -N` 与 `git diff` **共享同一个 15s context**，且 `patchOut, _ := exec.CommandContext(...).CombinedOutput()` **把超时/错误整个吞掉**——diff 进程被超时杀死时，半截输出被静默当作 model_patch 提交。astropy 是唯一诱发 agent 产生大量 C 构建产物的 repo（最可能的慢 diff 触发条件），两例恰好全部落在该 repo，其余 76 例无此症状。
- **影响**：2 例按 error 计 0 分。剔除该缺陷的潜在真实成绩 **69/77 = 89.6%**。
- **修复**：见 §7 P0-3。

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

### P0-1 ✅ 已落地 — SWE-bench 沙箱网络白名单（消除评分污染）

`internal/sandbox` 已有 Environment 抽象，docker environment 建容器时加网络策略：runner 传入 `--network` 约束或 iptables 规则——放行 `pypi.org`/`files.pythonhosted.org`（自举与装依赖必需），封禁 `github.com`/`raw.githubusercontent.com`/`api.github.com`/`codeload.github.com`。实现为 `SandboxConfig.NetworkAllowlist []string`，SWE-bench runner 设置之，TUI/主程序不受影响（默认全通）。**预估消除 ~17% 实例的污染向量**，否则任何 resolve 率都混杂"搜索能力"。

### P0-2 ✅ 已落地 — 评分基础设施脚本化（镜像预拉取 + 缓存管理）

本轮暴露：77 个 x86_64 镜像 110GB 的拉取无脚本、无断点、受 registry 限速阻塞评分一天。补 `benchmarks/swebench/pull-images.sh`（清单由 predictions.jsonl + swebench spec 生成，`docker pull --platform linux/amd64`，带重试与跳过已有），并把"镜像就绪检查"做成评分前置步骤。顺带 `docker builder prune` 纳入常规清理（本地已积累 21.98GB build cache）。

本轮实测补强两条工程参数：① Apple Silicon 上 Docker Desktop 拉取并发上限实测 **9 路安全**（12 路触发 daemon 500 错误风暴并回滚已拉镜像，损失约 1 小时）；② 大镜像（matplotlib 系）单张可达 4GB+ 且易遇下载连接僵死，需要"杀进程换新连接重试"的兜底，单纯退避等待无法恢复。另注意：swebench 拉镜像走 docker-py，**不读 `DOCKER_DEFAULT_PLATFORM`**，环境变量方案无效，必须 CLI `--platform linux/amd64` 预拉。

### P0-3 ✅ 已落地 — runner patch 提取加固（本轮直接丢 2 分的缺陷）

runner.go:289-292 三处叠加缺陷（详见 §5.2）：

1. `git add -A -N` 与 `git diff` 共享 15s context——重产物 repo 下 diff 被超时截断；
2. `patchOut, _ :=` 吞掉超时错误，截断补丁静默入库并提交评分；
3. `CombinedOutput()` 把 stderr 混入补丁流，任何 git warning 都会污染 patch。

改法：两个命令各自独立 context（diff 单独给 60s）；改用 `Output()` 并显式检查 err，失败重试一次；提交前做 hunk 完整性校验（每个 hunk 头声明的行数与正文一致、diff 以换行结尾），失败时打 `patch_truncated=true` 进 usage.jsonl 并落 ERROR 日志。验收标准：astropy-14182/14365 用原始 worktree 重跑提取，patch apply 成功、评分转为 resolved。

### P1-1 ✅ 已落地（A+B 双路线）— Planning 激活机制：从"等模型自觉"到"harness 主动触发"

2/78 的采用率说明软引导对 Kimi-K3 无效。两条路线（推荐 A）：

- **A. 复杂度探测后注入规划建议**：引擎在 launch 时根据 problem_statement 长度/ repo 环境重量（§3 已证明可静态估计）在首轮 user prompt 追加一句"该任务涉及多步骤，建议先用 plan_write 制定计划并随进展更新"。零内核改动，仅在 swebench prompt builder 试开。
- **B. 引擎级强制规划门槛**：`WithPlanningGate(turnBudget)`——当模型在探索期消耗超过 N turns 仍无 edit 时，注入"停下来先规划"提示（与 stall nudge 互补：stall 管"重复无进展"，gate 管"探索过深"）。

两条都以 `plan_writes` 采用率和 resolve 率为验收指标（本轮已建立采集与基线：2.6% 采用、成本分布 §3）。

### P1-2 验证关卡升级：从"提示"到"硬门槛"

当前关卡是一次性软提示（4 次触发全部有效补跑，说明够用），但 Agent 仍可能在提示后放弃验证交卷。升级方向：`runWithVerificationGate` 中第二轮续跑后仍 `ranTest == false` 时，将最终 patch 标记为 `unverified`（写入 usage.jsonl），报告中对 unverified patch 的 resolved 打折解读。成本近零，诚实度提升。

另据 §4.5 横向模式 1（7/7 实例均为"无回归式"验证），续跑提示的措辞应要求**正向断言**——"运行 Issue 的复现脚本并展示其从失败转为通过，点名跑你改动文件的测试"——而非泛泛的"跑一下测试"。无回归式验证（`git stash` 前后失败集合对比）在本轮 7 例 unresolved 中无一例外地给出了虚假信心。

### P1-4 ✅ 已落地（观测口径 + 收尾门槛）— 杜绝"最后一改未验证"交卷

pylint-7114 在第 80 轮（turn 上限）刚做完 edit 即被截断，终版 patch 零验证；seaborn-3407 在 1989s 被时间预算掐死在"降级 pandas 准备重跑官方测试"的验证半途。当前验证关卡只在"自然结束且从未跑过测试"时触发，对"预算耗尽前的最后改动"完全没有保护。建议：runner 感知预算余量，turns 或时长低于阈值（如剩余 10% / 5 turns）且存在未验证的改动时，注入一次"立即收尾：运行相关测试验证当前改动并总结"提示；若预算已不足以注入，在 usage.jsonl 标记 `final_edit_unverified=true` 供报告打折解读。

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
- [x] 官方 harness 最终评分（run_id `harness9-lite-v5`，2026-09-10 08:28，67/78=85.9%）+ compare-report.md（已按排除 23562 口径重生成：67/77=87.0%）
- [x] 本质量分析与内核优化建议报告（含 2026-09-10 复盘增补：§4.5 失败归因、§5.2 runner 截断、§7 P0-3/P1-4）
- [x] P0-1/P0-2/P0-3/P1-4 全部落地（opt/eval，23 包测试全绿）并以 47 实例验证轮端到端确认（§9：41/47=87.2%，error 清零）
- [x] 冒烟 24 实例产物入库（swebench-smoke/，提交 88b937f）
- [ ] PR #116（opt/eval → master）审阅合并；下一杠杆：P1-1 Planning 激活机制

## 9. 硬化验证轮（2026-09-10，47 实例 = v3 全集）

P0-1/P0-3/P1-4 落地后以 `--sample 4 --seed 1` 复现 v3 全集做端到端验证（run `20260910-103921`，墙钟 1h49m）：

| 指标 | 结果 |
|------|------|
| 生成 | **47/47 patch**，零空 patch、零运行错误；ran_test 47/47（100%） |
| 评分（R2） | **41/47 = 87.2%**，unresolved 6，**error 0** |
| 网络封禁 | 实弹生效：轨迹中 git fetch github.com 0ms 即拒（主轮 17% 实例抓取成功） |
| 收尾门槛 | final_edit_unverified 捕获 2 例（可观测）；80 轮满转 3（预算护栏工作） |

**过程中揪出并修复第四个缺陷**：collectPatch 沿用的 TrimSpace 剥掉 diff 终止换行，3 例补丁在 eval 容器被 git apply 判 corrupt——实弹复现（同一补丁补上换行即 cleanly applied）后以 TDD 修复（fix/patch-trailing-newline），R2 重评三例全部正常 apply。该缺陷修复后 error 类清零。

**flip 对照（同 47 实例，vs 主轮 v5）**：+3 转好（astropy-14365=P0-3 兑现、matplotlib-23562=镜像补齐、seaborn-3407=模型随机）、-2 波动（flask-4045、requests-2148），净效应持平（87.2% vs 主轮同子集 ~87%），硬化未损伤正常通过率，n=47 下 ±10pp 置信区间内一切翻转属模型随机性。

**结论**：runner 输出管线缺陷清零（两轮共 5 例管线事故：主轮截断 2 + 验证轮换行 3 → 0），网络污染向量封死，验证闭环满格——硬化目标全部达成。
