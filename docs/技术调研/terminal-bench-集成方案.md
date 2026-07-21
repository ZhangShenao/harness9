# Terminal-Bench 接入方案（v1，设计阶段）

> 状态：**设计已确认，未开始实现**。本文档是实现前的架构与范围约定，跑完 pilot 后应产出一份对应的
> `terminal-bench-轨迹分析-v1.md`（方法论对标 `swebench-轨迹分析与内核优化-v2.md`）。

---

## 0. 背景与目标

harness9 在 2026-06 完成了一轮 SWE-bench Lite（24 实例采样）驱动的内核优化：跑 agent → 生成 patch →
官方 evaluation harness 判定 resolved/unresolved → 逐条分析失败轨迹 → 定位 ReAct 循环/工具/prompt
层面的根因 → 修内核。该轮方法论产出的全部 P0/P1/P2 优化项（环境依赖接通、HintsText 注入、验证关卡、
stall 检测、minimal-diff/deprecation 提示、`edit_file` 文案收敛）**已全部落地**（见
`internal/engine/agent_loop.go` 的 `WithStallNudge`、`cmd/swebench/runner.go` 的验证关卡、
`cmd/swebench/prompt.go` 的 hints 注入）。24 实例小样本已经"挖干净"，继续加大 SWE-bench 采样量只是
在同一类任务（Python 单仓库改代码通过隐藏测试）上收窄置信区间，不会暴露新的根因类型。

**目标**：引入一个覆盖面完全不同的基准维度——不局限于"改代码通过测试"，而是贴近 shell/系统操作的
长链条通用任务（配置排查、多工具协同、非纯代码任务）——用来暴露 harness9 作为**通用 Agent Harness**
（而非纯 coding agent）在 SWE-bench 覆盖不到的失败模式，然后复用同一套"跑 → 抓轨迹 → 归因 → 改内核"
方法论继续做内核优化。选定的基准是 **Terminal-Bench**。

---

## 1. Terminal-Bench 现状（已核实，供实现阶段参考）

| 项 | 结论 |
|---|---|
| 维护方 | Stanford + Laude Institute |
| 官网 / Leaderboard | tbench.ai / tbench.ai/leaderboard |
| 仓库 | `laude-institute/terminal-bench`（已迁移至 `harbor-framework/terminal-bench`） |
| License | Apache-2.0，开源 |
| 版本与规模 | 2.0/2.1，89 个高质量任务（1.0 beta 版 ~100 个任务已精简） |
| 任务构成 | 自然语言指令 + Docker 沙箱环境 + pytest 风格隐藏测试 + oracle 参考解 |
| 判分 | 二值（全部隐藏测试通过才算 resolved），指标为 resolved 任务占比 |
| 官方 CLI | `tb run`（原仓库）/ `harbor run`（新仓库），支持 `--agent-import-path` 一类的自定义 agent 接入 |
| 自定义 agent 集成方式 | 官方支持三种：`AbstractInstalledAgent`（CLI 可安装型，**最简单**）/ 直接 Python 集成 / container 安装方式 |
| tmux/PTY 协议 | **非强制**——只是官方参考 agent "Terminus" 为公平对比而做的设计选择，自定义 agent 可以用结构化工具调用 |
| 单次全量成本 | 参考区间 $1～$100（视模型定价），多数任务 <20 分钟，个别 >2 小时；"Challenges" 长时程变体可达 12h+ |
| Leaderboard 水位 | 1.0 头部 Claude Sonnet 4.5 约 0.50；2.0 头部模型 0.7～0.9 区间 |

> **实现阶段需重新核实**：`tb`/`harbor` CLI 的确切命令行参数、`AbstractInstalledAgent` 的确切接口签名、
> 自定义 agent 是否需要 fork 官方仓库还是可以纯本地 import——这些细节可能随版本迭代变化，上表是设计
> 阶段调研的快照，不是实现依据本身。

---

## 2. 接入方式：官方 `AbstractInstalledAgent` 适配器（已确认，非自建 runner）

**决策**：不复刻 `cmd/swebench` 那种自建 dataset/runner/report 的完整基础设施，而是让官方 `tb run`
驱动全流程（起容器、装 agent、判分、出 trajectory），harness9 只提供"安装脚本 + 非交互执行入口"这一薄层
胶水代码。

**理由**：
- SWE-bench 那次也是"自己跑 agent，但用官方 harness 判分"——没有重新实现 SWE-bench 的评测逻辑。这次
  应延续同一原则：官方 harness 已经把环境搭建、判分、trajectory 记录做扎实了，重新实现只会增加维护成本
  且降低结果可信度（不是标准流程跑出来的分数无法对标 leaderboard）。
- `AbstractInstalledAgent` 不强制 tmux/PTY 协议，harness9 可以用自己原生的工具调用方式跑，不需要为了
  适配对方协议改造 agent loop。
- 工作量最小：不需要理解/复刻对方的任务 schema、Docker 镜像体系，这些留给官方 harness 处理。

**代价（已接受的 trade-off）**：执行细节（超时、日志格式）受官方 harness 约束，定制空间比自建 runner
小；如果未来想要更细粒度的 harness9 内部遥测（比如把 OTEL trace 接进分析），需要额外打通导出路径。

---

## 3. 架构与组件

本次集成**不需要新增 Go 代码**——是纯"胶水层"：

```
benchmarks/terminal-bench/
├── agent.py           # AbstractInstalledAgent 子类：定义安装脚本路径 + 执行命令
├── install.sh          # 在任务容器内执行：把预编译的 harness9 二进制装进容器
│                        # （不在容器内装 Go 工具链再编译——直接拷贝静态二进制更轻量、更快）
├── run_task.sh          # 非交互调用入口：读取 Terminal-Bench 提供的任务指令文件/参数，
│                        # 喂给 harness9 CLI 的管道模式（复用 cmd/harness9 已有的非 TTY 分支），
│                        # 跑到 agent 自然终止或官方超时
└── README.md            # pilot 任务清单 + 复现步骤
```

**关键设计点**：
- 容器内跑 harness9 时使用 `LocalEnvironment`（即不开 `SANDBOX_ENABLED`）——Terminal-Bench 的任务
  容器本身就是隔离边界，在其内部再套一层 Docker Sandbox 属于不必要的嵌套隔离，只会增加复杂度和失败面。
- `benchmarks/` 是新开的仓库根目录，与 `cmd/`（Go main package 的既有约定）区分——这批文件是
  Python + shell，不属于 Go 构建产物。
- 不写单元测试（这批代码不是 Go，也不是"产品功能"，属于一次性评测工具）；正确性验证方式是"先用 1 个
  任务跑通安装+调用链路，再铺开到全部 pilot 任务"（见第 5 节风险）。

---

## 4. 范围：Pilot 子集（15～20 个任务）

**不跑全量 89 个任务**，先跑一个精选子集，复现 SWE-bench 当年"24 实例 pilot → 挖根因 → 修内核 → 再决定
要不要扩大"的节奏。

**筛选标准**：
- 优先纳入：软件工程调试、系统管理/环境排查、Git/版本控制、构建工具链、网络与服务类任务——这些跟
  harness9 现有工具集（bash/read_file/write_file/edit_file + web_search/web_fetch）能力边界匹配。
- 排除：GPU/模型训练类任务（工具集不匹配，且成本/时长不可控）、"Challenges" 长时程变体（12h+，
  超出 pilot 预算）。
- 具体任务清单**留到实现阶段**——需要拉取官方当前任务清单（按 category 筛选）后确定，本文档只约定
  筛选标准，不锁定具体 20 个任务名单（避免此刻列出的清单在实现时已过期/改名）。

---

## 5. 数据流

```
tb run（宿主机）
  → 起 Docker 容器（每任务一个，官方镜像）
  → 执行 install.sh：拷贝预编译 harness9 二进制 + 写入 API Key 等必要环境变量
  → 执行 run_task.sh：把任务指令喂给 harness9 CLI（非交互管道模式）
  → harness9 ReAct 循环直接操作容器文件系统（bash/read_file/write_file/edit_file）
  → agent 自然终止（无 ToolCall）或触达官方超时
  → 官方 harness 在容器内跑隐藏测试 → resolved/unresolved 二值判定
  → 官方保留 trajectory/日志到宿主机结果目录，可对标 leaderboard
```

---

## 6. 产出物

1. `benchmarks/terminal-bench/` 下的适配器代码（`agent.py` + `install.sh` + `run_task.sh` + README）。
2. Pilot 跑完后，仿照 `swebench-轨迹分析与内核优化-v2.md` 的方法论，产出
   `docs/技术调研/terminal-bench-轨迹分析-v1.md`：逐任务对比 agent 行为 vs 官方隐藏测试/oracle 预期，
   对每条 harness 归因做对抗式复核（核对当前内核源码，剔除已修复/不存在的伪根因），按 P0/P1/P2 分级
   输出优化项。
3. 基于 pilot 结果的分级决策：是否值得扩大到全量 89 个任务、是否需要针对某类任务补充工具（比如任务里
   频繁出现某类 CLI 工具而 harness9 当前 prompt/工具集没有覆盖）。

---

## 7. 风险与开放问题

| 风险/问题 | 应对 |
|---|---|
| harness9 二进制在任务容器里可能缺运行时依赖（glibc 版本、CA 证书等） | Pilot 第一步只跑 1 个任务验证"安装成功 + 能跑起来"，通过后才铺开到全部 15~20 个任务，避免批量踩坑后才发现环境问题 |
| 官方 CLI/`AbstractInstalledAgent` 接口签名可能随版本变化，本文档第 1 节数据是调研快照 | 实现阶段第一步重新核实当前已安装版本的确切接口，而非直接照抄本文档 |
| 官方超时/日志格式受限，出问题时不易debug | 若某任务反复失败且原因不明，允许在 pilot 阶段用官方允许的本地/verbose 模式单独复现，不强求在 CI 式黑盒环境里排障 |
| Pilot 子集是"精选"而非随机采样，分数不能直接对标 leaderboard | 本阶段目标是挖根因、不是刷分；是否需要全量跑分对标 leaderboard 留给第 6 节的分级决策 |

---

## 8. 明确不做的事（本轮范围之外）

- 不引入 Go 生态基准（本次讨论中用户已选择 Terminal-Bench 方向，Go 生态基准作为候选方向被搁置，未来
  可另起一轮讨论）。
- 不做 agent loop 结构性升级（多候选生成+投票、更智能 context 压缩等）——这类改动应该等 Terminal-Bench
  的根因分析给出具体证据后再决定是否需要，而不是先改后验证。
- 不追求效率指标（回合数/token 数）优化——本轮仍以"能不能做对"为主要信号，效率是后续阶段的候选方向。
