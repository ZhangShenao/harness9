# SWE-bench Lite 官方打榜执行方案

> 状态：P0 已落地（可复现性打包） ｜ 撰写日期：2026-09-10
> 关联文档：`docs/技术调研/swebench-v4-扩样评测设计.md`（评测内核）、`benchmarks/swebench/run-official-lite.sh`（一键复现入口）

## 1. 目标与赛道选择依据

harness9 以 **SWE-bench Lite** 为首个官方打榜赛道，目标是在官方排行榜
（swebench.com）以可验证、可复现的公开提交留下成绩。

赛道选择依据（为什么是 Lite 而非 Verified / Multilingual）：

- **2025-11-18 起 Verified 与 Multilingual 收紧为学术专属**。官方
  [SWE-bench/experiments](https://github.com/swe-bench/experiments) README 原文：
  > [11/18/2025] SWE-bench Verified and Multilingual now only accepts submissions from
  > academic teams and research institutions with open source methods and ...
- 学术团队资质要求"arXiv 预印本 + 学术机构作者"形态的方法学论文；harness9 作为
  工业开源项目暂不满足，而 **Lite 赛道未受此限制**，仍接受社区提交（PR 制）。
- Lite 与 Verified 同源（300 例 vs 500 例，Verified 为 Lite/全集的人工筛选子集），
  在 Lite 站稳后若未来满足学术门槛，可平移到 Verified。
- 背景风险提示：2025-11-19 官方发布"[Detecting cheating in submissions]"
  (https://www.swebench.com/blog.html)，随后 OpenAI 亦发文质疑 Verified 的污染问题
  ——官方对提交真实性的审查趋严，这正是本方案 P0 阶段（存证与复现基础设施）先行原因。

## 2. 官方新提交流程（PR 制）

提交 = **一个公开 artifacts 仓库 + experiments 仓库里的一条 PR 条目**。官方 CLI
（`pip install swebench` 后的 `swebench submit` 子命令）串起三步：

```bash
# 1) 打包：从完成评分的 run 生成 submission/（artifacts + entry 两部分）
swebench submit package <run_id> -s lite --trajs <轨迹目录> -o ./submission

# 2) 发布：创建并推送公开 artifacts 仓库
swebench submit publish ./submission

# 3) 注册：对 SWE-bench/experiments 仓库开 PR
swebench submit register ./submission -s lite
```

`publish` / `register` 均支持 `--dry-run`（只打印不动网络）；`register` 在 entry 存在
未填 TODO 时拒绝开 PR。也允许手工组织文件后直接开普通 PR。

**artifacts 仓库必备结构**（`swebench eval` 产出、`package` 收集）：

```
all_preds.jsonl
logs/<instance_id>/
    patch.diff            # 该实例最终 model patch
    report.json           # 评测结论（resolved 与否）
    test_output.txt.gz    # patch 应用后的测试输出（gzip）
trajs/<instance_id>.md    # 推理时生成的人类可读轨迹
```

轨迹要求（官方自 2024-07-29 起强制）：每条预测实例一份、以 instance_id 命名、
任意文本格式；必须**人类可读、反映中间步骤、由推理过程同步生成而非事后补写**；
best@k 系统须展示全部 rollout 与择优机制。

**复核通道**：第三方可用 `swebench submit verify <entry> -s lite` 从留存的
test_output 离线重判每个实例（无需 Docker 重跑）；"verified" 勾选还需开 issue
提供运行说明供维护者抽查复跑。

## 3. 分阶段计划

| 阶段 | 内容 | 产出 | 状态 |
|------|------|------|------|
| **P0 可复现性打包** | 模型版本快照存证、一键复现脚本、官方提交物导出、提交 README 模板与本方案 | `cmd/swebench/model_snapshot.go`、`cmd/swebench/export_submission.go`、`benchmarks/swebench/run-official-lite.sh`、`benchmarks/swebench/submission/README.template.md` | ✅ 已完成 |
| **P1 全量跑分** | Lite 全量 300 例 pass@1（Kimi-K3，seed=1）；跑前 50 例校准预估分数与成本 | `swebench-official/<run_id>/` 完整 run 目录（predictions + usage + 轨迹 + 模型快照） | 校准 ✅（2026-09-12，48/57=84.2%，见 §3.1）；全量待执行 |
| **P2 官方评分与导出** | `pull-images.sh` 预拉 amd64 评测镜像 → 官方 harness 评分 → `--mode export` 产出 submission 目录 → 填充 `swebench submit package` 所需件 | submission/（all_preds + logs + trajs + README） | 校准轮已演练通过（trajs 57/57，EXPORT_MANIFEST 如实记录缺失件）；正赛待执行 |
| **P3 发布与登记** | 公开 artifacts 仓库（GitHub public repo）→ `swebench submit publish` → `register` 开 PR → 跟进官方审查/verify 抽查 | experiments 仓库 PR + 公开 artifacts 仓库 | 待执行 |

P1 执行入口：

```bash
./benchmarks/swebench/run-official-lite.sh                 # 全量（默认 --sample 100 覆盖 300 例）
./benchmarks/swebench/run-official-lite.sh --sample 5      # 校准：~50 例
./benchmarks/swebench/run-official-lite.sh --dry-run       # 只打印步骤
```

### 3.1 校准结果（calib-r3，2026-09-12）

**48/57 = 84.2% resolved**（Wilson 95% CI [72.6%, 91.4%]；排除官方 harness
error 口径 48/56 = 85.7%），与 v4 基线（78 例 85.9%、硬化验证 87.2%）一致：
55 个可比实例上两轮各解决 48 个（重叠 42、新修 6、回退 6），净漂移为零。
完整归因、r2 作废证据链与成本（$99.35）见
`swebench-official/calib-r3-final/CALIBRATION_REPORT.md`。

校准过程沉淀的三件质量基础设施（均在 `feat/benchmark` 分支）：

| 工具 | 作用 | 背景 |
|------|------|------|
| `benchmarks/swebench/merge-predictions.py`（575473d） | 多 run keep-best 合并（空 patch 恒被非空覆盖）+ logs/usage/快照拼装 | runner `--resume` 追加式，多轮续跑必须去重 |
| `benchmarks/swebench/audit_run_health.py`（6f578f7） | 评分前审计：轨迹覆盖完整性 + daemon 污染签名（docker.sock / exec format error），不过即禁止评分 | round2 在 daemon 抖动期推理，patch 交付率 93% 但 resolve 仅 63.2%，**整轮作废** |
| `run-official-lite.sh` 步骤 3.6 | 审计门控固化进一键流程 | 同上 |

**正赛执行纪律（从校准教训固化的三条）**：①单轮连续完成，避免 resume 混代次；
②评分前必须过污染审计；③基础设施失败（clone 超时、provider 流中断）可定点重试，
模型行为结果（含空 patch、生成退化）按 pass@1 如实保留，不重骰。

## 4. 预算

以 v4 扩样评测实测用量外推（`benchmarks/swebench/v4-expansion/usage.jsonl`：
78 例共 input 43.4M / output 1.0M tokens，Kimi-K3 计价 $3 / $15 每 MTok）：

| 项目 | 规模 | 预估 tokens | 预估成本 |
|------|------|------------|---------|
| 校准轮（P1 前置） | 50 例 | input ~27.9M / output ~0.67M | **~$94** |
| 全量轮（P1） | 300 例 | input ~167M / output ~4.0M | **~$562** |
| 评分 | 300 例 | 官方镜像本地跑 | $0（仅 Docker 时长） |
| 复跑缓冲（+20% 瞬时失败重试） | — | — | ~$110 |

**一轮全量打榜总预算约 $670，建议按 $800 申请。** 注意 Lite 排行榜取 pass@1
单轮成绩，不允许择优重交；失败实例不会拖出预算黑洞（per-instance 30 分钟超时 +
80 turn 上限兜底）。

## 5. 风险与对策

| 风险 | 影响 | 对策 |
|------|------|------|
| **OpenRouter 路由不可复现**：网关背后的模型版本随上游滚动更新，事后无法证明"该轮分数由哪个版本服务" | 成绩存证被质疑 | P0 已落地：评测启动时从 `GET https://openrouter.ai/api/v1/models`（公开、免鉴权）抓取所用模型元数据（id / name / created / context_length / top_provider 限额 / pricing），落盘 `model_snapshot.json` 并写入 run_summary.md；抓取失败 fail-open 不阻断评测但报告如实标注"未获取" |
| 长跑中断（断网 / 容器风暴） | 浪费预算 | runner 已有 `--resume`（跳过已有非空 patch 实例）+ 固定 seed（同实例集）；官方镜像预拉含并发 ≤9 与僵死连接重试经验 |
| GitHub 上游答案污染评分 | 触发官方红线 | P0-1 已落地：`NetworkBlockedHosts` 以 docker `--add-host` 把 GitHub 系 6 域名钉到 `0.0.0.0`，容器内 DNS 即拒（实测）；轨迹可复核无成功抓取 |
| 残缺补丁送评 | 假性 0 分 | 已落地：collectPatch 独立超时 + validateUnifiedDiff hunk 完整性校验，校验不过按实例错误上报 |
| per-instance 评分产物缺失（旧 run 形态） | 提交被拒 | 导出器不伪造：缺失件跳过并逐条记录 `EXPORT_MANIFEST.md`，发布前按清单补跑评分 |
| 官方审查趋严（11-19 反作弊之后） | PR 被挑战 | 提交 README 逐条对照四条红线声明 + 全链路存证（快照/轨迹/manifest），先自查再提交 |

## 6. 官方 checklist 四条红线逐条对照

| # | 红线 | harness9 现状 | 存证位置 |
|---|------|--------------|---------|
| 1 | pass@1 单次采样，无 best@k | 每实例单轮单轨迹，无择优逻辑 | trajs/ 每实例仅一份推理时轨迹 |
| 2 | 不使用 FAIL_TO_PASS / PASS_TO_PASS 知识 | 数据集加载后隐藏测试字段不进 Agent 上下文；测试由评测阶段注入 | runner.go / prompt.go 源码 + 轨迹 |
| 3 | 不使用 hints 字段 | hints_text 在 prompt 组装中被排除，Agent 不可见 | prompt.go + 轨迹中无 hints 内容 |
| 4 | 无 web 查答案，或有防护 | 容器级防护：`docker --add-host <host>:0.0.0.0` 封禁 github.com / raw.githubusercontent.com / api.github.com / codeload.github.com / objects.githubusercontent.com / gist.github.com，实测 DNS 即拒；PyPI 依赖自举不受影响 | internal/sandbox/container.go + 各实例轨迹 |

## 7. 参考资料

- SWE-bench/experiments（提交流程、11/18 政策原文）：<https://github.com/swe-bench/experiments>
- SWE-bench 官网与博客（反作弊公告）：<https://www.swebench.com/blog.html>
- SWE-bench Lite 排行榜：<https://www.swebench.com>
- Dissecting the SWE-Bench Leaderboards（提交生态分析，arXiv:2506.17208）
