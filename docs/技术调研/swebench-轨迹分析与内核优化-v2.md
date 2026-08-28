# SWE-bench Lite 轨迹根因分析与 harness9 内核优化（v2）

> 数据来源：RunID `20260620-094930`，24 实例（每 repo 2 条，seed=1），16 resolved / 8 unresolved（66.7%）。
> 分析方法：每条失败轨迹与 **gold patch + test_patch + FAIL_TO_PASS** 逐一对比，定位「我们的 patch 为何过不了隐藏测试」，再回溯轨迹找出「Agent 为何产出这个 patch」，并对每条 harness 归因做**对抗式复核**（核对当前内核源码，剔除"已修复/不存在"的伪根因）。复核结果：**21 confirmed / 6 partial / 2 refuted**。

---

## 0. 一句话结论

> **24 条轨迹里没有任何一条真正跑过一次测试**——全部命中 `ModuleNotFoundError` / `No module named pip`。
> 16 条 resolved 全靠**纯静态分析**蒙对；8 条 unresolved 基本都是「需要真实测试反馈才能收敛」的题。
> harness9 内核（ReAct 循环 / 工具 / 自愈）本身是健康的，**真正的瓶颈是 SWE-bench runner 把 Agent 关进了一个没有依赖、没有 pip、无法运行任何测试的空环境**，使内核最重要的反馈信号（真实测试结果）恒为空。

---

## 1. 决定性证据：验证闭环 100% 断裂

跨全部 24 条日志检索真实 pytest 汇总行（`=== N passed/failed ===`）：

| 指标 | 数值 |
|------|------|
| 出现过真实测试汇总行的实例 | **0 / 24** |
| 出现 `ModuleNotFoundError` / `No module named` 的实例 | **24 / 24** |
| 失败实例中跑过真实 grading 测试的 | **0 / 8** |
| 成功实例中跑过真实 grading 测试的 | **0 / 16** |

根因在 `cmd/swebench/runner.go:98-101`：未设 `SANDBOX_IMAGE` 时**强制** `python:3.11-slim`，且 clone+checkout 之后**没有任何依赖安装步骤**（无 `pip install -e .`，不消费 SWE-bench 的 `environment_setup_commit` / `install` spec）。后果：

- 凡依赖 numpy/pandas 的库（astropy / seaborn / xarray / matplotlib / sklearn / sympy）**连 import 都做不到**；
- 部分 slim 变体里**连 pip 都没有**（flask-4992、pylint-7080 出现 `No module named pip`）；
- requests-1963 需要 **Python 2.7**（2014 年代码用了已被移除的 `collections.MutableMapping`/`cgi`），py3.11 下根本跑不起来。

讽刺的是：**内核早已为此预留接缝但没接线**——
- `internal/sandbox/config.go:29-35` 定义了 `BootstrapCmd` / `BootstrapTimeout`，注释明确写着"为 SWE-bench 仓库安装依赖 / 接入官方每实例镜像的关键接缝"；
- `internal/sandbox/manager.go:71-72` 的 `runBootstrap` 已实现并在容器就绪后自动执行；
- 但 `runner.go` **从未设置 `BootstrapCmd`**，`dataset.go` 的 `Instance` 也**没有解析** `version` / `environment_setup_commit` / `FAIL_TO_PASS` / `test_patch`，无从 provision。

---

## 2. 根因分级（已对抗式复核）

| # | 根因 | 归属 | 命中实例 | 复核结论 |
|---|------|------|---------|---------|
| **R1** | **环境缺失**：sandbox 无依赖/无 pip，真实测试不可运行 | runner | **8/8** | confirmed |
| **R2** | **验证闭环缺失**：循环仅凭 `len(ToolCalls)==0` 终止，无"跑过测试再收尾"的关卡，Agent 静态自证即交卷 | engine+runner | 8/8 | confirmed |
| **R3** | **HintsText 被丢弃**：`dataset.go` 解析了 `hints_text` 但 `prompt.go` 从未注入；维护者讨论里常含决定性 API 设计（如 flask `text=True`） | runner/prompt | flask-4992 等 | confirmed（独立 bug） |
| **R4** | **dataset 字段缺失**：未解析 `version`/`environment_setup_commit`/`FAIL_TO_PASS`/`test_patch`，无法 provision、无法选对 Python 版本、无法把测试命令告诉 Agent | runner | 8/8 | confirmed |
| **R5** | **prompt 反向引导**：`prompt.go:32/48/61` 明示"依赖可能没有、pip 可能不可用，退化为静态分析"，把"放弃验证"写成了官方逃生门 | prompt | 8/8 | confirmed |
| **R6** | **盲目空转到 max_turns**：无执行反馈时反复静态重读，xarray-3364、pylint-7080 烧满 80 轮被 guillotine 截断，交出臃肿错 patch | engine | 2/8 | confirmed |
| **R7** | **合理但错位的修复**：模型常给出"plausible-but-wrong"补丁——错位置（pylint 应改 `expand_modules` 却改 `pylinter`）、自创 API（flask `mode=` vs gold `text=`）、改行为而非加 DeprecationWarning（xarray-4493） | model→prompt | 6/8 | confirmed（harness 杠杆=验证闭环 + minimal-diff 引导） |
| **R8** | **edit_file 成功提示抑制验证**：`edit_file.go` 精确匹配时输出"无需额外 grep/sed/read_file 再次确认"，助长"改完即完成"心态 | tools | 多数 | confirmed（措辞需收敛） |

**2 条被驳回的伪根因**（对抗复核的价值）：
- psf__requests-1963 的 model_reasoning/test_discovery 归因引用了**不存在的测试** `test_requests_in_history_are_not_overridden`，且模拟证明我们的 redirect patch 实际能过那条 graded 测试。该实例真正的问题是：7 条 FAIL_TO_PASS 里有 6 条是 digest-auth/cookie 测试，**与 redirect 修复无关**——它们只有在真实测试环境里才能通过。→ 再次指回 R1：**环境不修，连"我们到底错没错"都判断不了**。

---

## 3. 逐实例归因

| 实例 | 失败模式 | 轮数 | 主因 | 我们 vs gold |
|------|---------|-----|------|-------------|
| astropy-7746 | 错位置 | 17 | R1/R2/R7 | 空数组短路放在 `broadcast_arrays` **之后**，gold 放在**之前**；`[],[1]` 被广播塌成 `[],[]`，丢了 `[1]` |
| seaborn-3407 | 错位置 | 23 | R1/R2/R7 | 把 MultiIndex 列名拍平成字符串；gold 只是不再 `np.array(...,object_)` 保留 tuple 键。误诊"tuple 键有歧义"（实则 pandas 支持） |
| flask-4992 | 错 API | 13 | R1/R3/R7 | 自创 `mode="t"`（且 `open(f,"t")` 直接 ValueError）；gold 是 `text=True` —— 决定性命名藏在 **hints_text**，而 hints 从未注入 |
| flask-5063 | 不完整 | 38 | R1/R2/R7 | 只做了 subdomain 的 "Domain" 列；gold 同时处理 `host_matching`，表头按模式取 "Subdomain"/"Host"。隐藏测试断言这两个字符串 |
| requests-1963 | 环境主导 | 9 | R1 | 我们的 redirect 修复**可能是对的**；6/7 FAIL_TO_PASS 是无关的 digest-auth 测试，只有真实环境能过 |
| xarray-3364 | 错位置 + 超轮 | **80** | R1/R2/R6 | 在 `concat_over` 路径加了 67 行填充；gold 是 `variables_to_merge` 路径删 6 行 `raise`。锚定了**将被 test_patch 删除**的旧断言，刻意保留错误 |
| xarray-4493 | 错机制 | 50 | R1/R2/R7 | 静默抽取 `.variable`；gold 是发 `DeprecationWarning`。隐藏测试 `pytest.warns(DeprecationWarning)` |
| pylint-7080 | 错位置 + 超轮 | **80** | R1/R2/R6 | 在 `pylinter.py` 加一大坨 ignore 检查；gold 仅在 `expand_modules._is_ignored_file` 加一行 `element = os.path.normpath(element)` |

---

## 4. 优化方案（按杠杆排序）

### P0 — 接通环境，恢复验证闭环（决定性）
1. **`dataset.go`**：`Instance` 增加 `Version` / `EnvironmentSetupCommit` / `FAIL_TO_PASS` / `PASS_TO_PASS` / `TestPatch` 字段。
2. **`runner.go`**：消费上述字段为每实例 `sandboxCfg.BootstrapCmd` 赋值（接通已存在的 `manager.runBootstrap`）。两条路线：
   - **A 官方镜像**（高保真）：`SANDBOX_IMAGE=swebench/sweb.eval.x86_64.<instance>`，仓库与依赖已预装，bootstrap 仅需 `pip install -e .`（或空）。
   - **B 自举安装**（轻量）：更全的基础镜像 + `python -m ensurepip && pip install -e . <test-deps>`，best-effort、fail-open。
3. **`prompt.go`**：删除"退化为静态分析"的逃生门（R5），改为"环境已具备，**先复现 issue、再跑相关测试**"，并注入 runner 探测到的测试命令。

### P1 — 验证关卡 + 提示信号（中杠杆，环境就绪后才完全生效）
4. **HintsText 注入**（R3）：`prompt.go` 增加 `{{HINTS}}` 占位并填充 `inst.HintsText`，注明"维护者讨论常覆盖原 issue 提案，最终 API 以讨论为准"。**独立于环境，立即可做、零风险。**
5. **验证关卡**：放在 **runner 的 `runWithTrajectory`**（不放共享的 `agent_loop.go`，避免污染 TUI/CLI）——Agent 自然终止但轨迹中"无任何成功测试运行"时，注入一条续跑提示"尚未跑过任何测试，请运行相关测试再收尾"。仅在环境可运行时硬约束，否则软提示，避免在不可运行环境里 livelock。
6. **stall 检测**（R6）：引擎层 opt-in `WithStallNudge(n)`——连续 n 轮"无 edit_file 且无成功测试运行"时注入一次"要么跑测试、要么定稿"的提示，打断盲目空转。键控于"无进展"而非单纯"无 edit"。

### P2 — 推理偏置矫正（低风险 prompt/tool 收敛）
7. **minimal-diff / error-site 偏置**（R7）：prompt 增加"先定位产生错误行为的那一行（raise/return/branch），在**该处**做最小改动；勿新增并行代码路径、勿把分配/对象提到循环外改变别名语义"。
8. **deprecation 约定提示**（R7）：对"歧义/意外 API 行为"类 bug，提示先 grep 仓库的 `warnings.warn`/`DeprecationWarning` 约定，优先与项目惯例一致的修法。
9. **edit_file banner 收敛**（R8）：把"无需额外…再次确认"改为只声明"字节已写入"，不暗示"无需验证行为"。

---

## 5. 不可回归项（来自成功轨迹对照）

16 条成功**全部是静态分析的胜利**，说明以下机制有效、改动时务必保留：
- `read_file` 行号前缀模式（精确定位、减少 edit 多匹配失败）；
- 并行工具探索（一轮多 grep/read）；
- "阅读但不修改现有测试"引导（对齐隐藏测试的最强可见信号）；
- 当前的指数退避生成重试、bash 头 + 尾截断、safePath 绝对路径分支（均为上一轮 hardening 成果，本轮未再现相关故障）。

所有 P0/P1/P2 改动均为**新增验证能力 + 增强提示**，不削减上述探索能力——成功路径不应回归。
