# Terminal-Bench 接入方案（v2，设计已修正，待实现）

> 状态：**设计已确认，未开始实现**。v1 版本调研时把接入目标误判为经典 `tb`/`AbstractInstalledAgent`
> （那是 Terminal-Bench 1.x 的 legacy 接口）；v2 已修正为官方当前真正对应"2.0 版 89 题"的框架
> **Harbor**，全部接口/CLI 细节均已源码级核实（两轮独立对抗式复核，见调研记录）。跑完 pilot 后应产出
> `terminal-bench-轨迹分析-v1.md`（方法论对标 `swebench-轨迹分析与内核优化-v2.md`）。

---

## 0. 背景与目标

harness9 在 2026-06 完成了一轮 SWE-bench Lite（24 实例采样）驱动的内核优化，产出的全部 P0/P1/P2 优化项
已落地（`WithStallNudge`、`cmd/swebench/runner.go` 验证关卡、`prompt.go` hints 注入等）。24 实例小样本
已经"挖干净"，继续加大 SWE-bench 采样量不会暴露新的根因类型。

**目标**：引入一个覆盖面不同的基准维度——不局限于"改代码通过测试"，而是贴近 shell/系统操作的长链条
通用任务——暴露 SWE-bench 覆盖不到的失败模式，然后复用同一套"跑 → 抓轨迹 → 归因 → 改内核"方法论继续
做内核优化。选定的基准是 **Terminal-Bench 2.0**（89 个任务）。

---

## 1. Terminal-Bench 现状（源码级核实，两轮独立复核均确认无误）

### 1.1 项目已分裂为两代框架

| | Terminal-Bench 1.x（legacy） | **Harbor（当前，本方案目标）** |
|---|---|---|
| 仓库 | `harbor-framework/terminal-bench`（原 `laude-institute/terminal-bench`） | `harbor-framework/harbor`（原 `laude-institute/harbor`） |
| CLI | `tb run` / `terminal-bench` | `harbor run`（`hr`/`hb` 为同一入口的短别名） |
| PyPI 包名 | `terminal-bench` | `harbor`（`pyproject.toml` 确认 `version = "0.20.0"`，**未核实是否已发布到 PyPI**，装之前先 `pip show harbor` 确认拿到的是这个包） |
| 自定义 agent 基类 | `AbstractInstalledAgent`（`_env`/`_install_agent_script_path`/`_run_agent_commands` 三个抽象成员，同步） | `BaseInstalledAgent`（唯一抽象方法 `install()`；`run()` 来自父类 `BaseAgent`，均为 **async**） |
| "2.0 版 89 题"数据集 | ❌ 不在这个仓库里 | ✅ 独立仓库 `terminal-bench-2`，通过 Harbor 的 `registry.json` 注册（`name="terminal-bench"`, `version="2.0"`，`len(tasks)==89`，已逐条核实） |
| 官方文档站当前导航 | 已被替换 | `tbench.ai/docs` 现在的三个条目全部指向 Harbor（"How to run Terminal-Bench 2.0/2.1/Challenges"） |

`harbor-framework/terminal-bench` 仓库 README 原文即写着"New users should check out **harbor**...
that can be used to run Terminal-Bench 2.0"。**结论：要拿到"89 题 2.0 版"，必须走 Harbor，不是 `tb`。**

> 次要更正：registry.json 里**没有找到 "2.1" 版本的 terminal-bench 条目**（只有唯一一条
> `terminal-bench@2.0`）。文档站提到的 2.1 暂时找不到对应注册数据，先按 2.0（89 题）实施，
> 如后续发现 2.1 已注册再复核。

### 1.2 Harbor 自定义 agent 接口（源码级核实，两轮独立复核确认逐字一致）

基类：`harbor.agents.installed.base.BaseInstalledAgent(BaseAgent, ABC)`

```python
class MyInstalledAgent(BaseInstalledAgent):
    async def install(self, environment: BaseEnvironment) -> None:
        """安装阶段。exec_as_root 装系统包，exec_as_agent 装用户级依赖。"""
        ...

    @with_prompt_template
    async def run(
        self, instruction: str, environment: BaseEnvironment, context: AgentContext
    ) -> None:
        """执行阶段。instruction 已经是渲染好的 Python 字符串，直接用。"""
        ...

    def populate_context_post_run(self, context: AgentContext) -> None:
        """可选：run() 结束后回填 token 用量等（默认 no-op，v1 pilot 不实现）。"""
        pass
```

`BaseEnvironment` 关键方法（均 async，源码级核实）：

```python
async def exec(
    self, command: str, cwd: str | None = None, env: dict[str, str] | None = None,
    timeout_sec: int | None = None, user: str | int | None = None,
) -> ExecResult: ...   # ExecResult(stdout: str|None, stderr: str|None, return_code: int)

async def upload_file(self, source_path: Path | str, target_path: str) -> None: ...
async def upload_dir(self, source_dir: Path | str, target_dir: str) -> None: ...
```

`exec_as_root`/`exec_as_agent` 是 `BaseInstalledAgent` 对 `environment.exec()` 的薄封装
（固定 `user="root"` / 默认用户，内部 `set -o pipefail` 包装 + 错误分类）。

### 1.3 Harbor CLI（`src/harbor/cli/jobs.py` 源码核实）

`harbor run` 是 `harbor job start` 的别名。关键参数：

| 参数 | 含义 | 备注 |
|---|---|---|
| `-d, --dataset` | 数据集 `name@version` | 本方案填 `terminal-bench@2.0` |
| `-a, --agent` | 内置 agent 名，**或**自定义 import path（含 `:` 即识别为 import path） | `--agent-import-path` 已废弃（`hidden=True`），不要用 |
| `-m, --model` | 模型名（传给 agent 构造函数的 `model_name` kwarg） | 本方案不使用 LiteLLM 路由，此参数可选传或不传 |
| `-i, --include-task-name`（可重复，支持 glob） | 指定要跑的任务 | 对应经典 tb 的 `--task-id` |
| `-x, --exclude-task-name`（可重复） | 排除任务 | |
| `-l, --n-tasks` | 过滤后再截断的任务数上限 | |
| `-o, --jobs-dir` | 结果输出目录 | |
| `-k, --n-attempts` | 每任务重复次数 | pilot 用默认值（1 次）即可 |

自定义 agent 导入用标准 `importlib.import_module`（`src/harbor/utils/import_path.py`），
**不需要 fork harbor 仓库**，agent 代码可以放在我们自己的仓库里，只要模块能被
`PYTHONPATH`/已安装包解析到。**没有** `issubclass(BaseAgent)` 的强制检查（未实现抽象方法会在
**实例化阶段**因 ABC 报 `TypeError`，不是 import 阶段）。

### 1.4 已知但未逐行核实的部分（实现时如遇到不一致，以实际运行结果为准，不要死磕文档）

- Harbor 侧 `results.json`/trial 结果的确切 schema（字段名、位置）——没有找到 tb 那种逐行核实的源码。
  **应对**：pilot 第一步跑完 1 个任务后，直接 `find`/`ls` 探查 `--jobs-dir` 输出目录的真实结构，
  以及是否有 `harbor job status`/`harbor trial ...` 一类的摘要子命令，不要凭猜测的字段名写脚本。
- Docker 环境下 `upload_file` 的具体实现（是不是 `docker cp`）未核实，只核实了抽象接口签名。

---

## 2. 接入方式：Harbor `BaseInstalledAgent`（已确认）

**理由**（延续 v1 决策的原则，只是把目标框架从 tb 修正为 Harbor）：
- 让官方 `harbor run` 驱动全流程（起环境、装 agent、判分、出结果），harness9 只提供
  "安装逻辑 + 执行逻辑"两个 Python 方法的实现，不重新实现环境搭建/判分。
- Harbor 的 `install()`/`run()` 是**纯 Python 方法**（不像 tb 那样要求单独打包一个 shell 脚本文件），
  实现更直接：`install()` 里直接 `await environment.upload_file(...)` 把预编译二进制放进环境，
  `run()` 里直接 `await self.exec_as_agent(environment, command=...)` 非交互调用。

**新发现的前置依赖（v1 未预料到，必须先做）**：harness9 当前的非交互 CLI 模式
（`cmd/harness9/cli.go` 的 `runCLI`）是**逐行 REPL**——从 stdin 按行读取，每一行都当作独立的一次
`eng.Run()` 调用。Terminal-Bench 的任务指令（`instruction.md`）通常是多行/多段文本，如果直接
管道进去会被错误拆成多个独立 Turn（语义完全错误）。**必须先给 harness9 加一个真正的"读取完整
文件内容、执行一次、退出"的一次性模式**（新增 `--prompt-file` flag），才能正确对接 Harbor 的
`run()` 方法。这是本方案第一个实现任务（Task 1），修改范围在 harness9 核心代码，不在
`benchmarks/` 目录下。

---

## 3. 架构与组件

```
cmd/harness9/
├── cli.go      # 新增 RunOnce(ctx, eng, promptFilePath) 一次性执行模式
├── main.go     # 新增 --prompt-file flag，三路分支：--prompt-file / TTY / 管道REPL

benchmarks/
└── terminal_bench/            # Python 包名用下划线（Harbor import path 要求合法 Python 标识符）
    ├── __init__.py
    ├── harness9_agent.py       # Harness9Agent(BaseInstalledAgent)：install() 拷二进制，run() 非交互调用
    ├── bin/harness9            # 预编译 linux/amd64 静态二进制（.gitignore 排除，不进仓库）
    └── README.md               # pilot 任务清单 + 复现命令
```

**关键设计点**：
- `install()`：`environment.upload_file(本地二进制, "/usr/local/bin/harness9")` + `chmod +x`。
  二进制用 `CGO_ENABLED=0` 静态编译（与 `.goreleaser.yaml` 现有构建配置一致），规避容器内
  glibc 版本不兼容风险，且不需要在容器里装 Go 工具链。
- `run()`：把 `instruction` 写到本地临时文件 → `upload_file` 传进容器 → `exec_as_agent` 调用
  `harness9 --prompt-file <容器内路径>`（避免把多行文本塞进 shell 命令行参数导致的转义/截断问题，
  这也是 Harbor 自带 ClaudeCode 参考实现处理长 instruction 的同类思路，只是它们用环境变量、
  我们用文件——两种方式都规避了命令行参数转义问题）。
- API Key 通过 `exec_as_agent(..., env={...})` 显式传入，不依赖容器环境变量继承。
- 不实现 `populate_context_post_run`（可选，默认 no-op，YAGNI，v1 不需要）。

---

## 4. 范围：Pilot 任务清单（18 个，已从 89 题的 category/tags 元数据中确定）

依据 `task.toml` 的 `category`/`tags` 字段筛选（软件工程调试/Git/版本控制/构建工具链/网络与服务类
系统管理），排除 QEMU 虚拟化类（3 个，偏 exotic）、邮件服务器类（niche）、GPU/模型训练类、
"Challenges"长时程变体（不在 89 题主集内，无需额外排除）：

```
fix-git, git-multibranch, configure-git-webserver, git-leak-recovery, sanitize-git-repo,
build-cython-ext, build-pmars, build-pov-ray, compile-compcert,
custom-memory-heap-crash, fix-ocaml-gc, merge-diff-arc-agi-task,
sqlite-db-truncate, sqlite-with-gcov, nginx-request-logging,
pypi-server, kv-store-grpc, openssl-selfsigned-cert
```

`fix-git`（difficulty=easy）作为 Task 5（"跑通 1 个任务"）的验证对象——难度最低，问题描述最短，
适合验证安装链路，不适合用来判断内核能力。

---

## 5. 数据流

```
harbor run -d terminal-bench@2.0 -a benchmarks.terminal_bench.harness9_agent:Harness9Agent
           -i <task1> -i <task2> ... -o benchmarks/terminal_bench/runs
  → 起环境（每任务一个）
  → 调用 Harness9Agent.install(environment)：upload_file 二进制 + chmod +x
  → 调用 Harness9Agent.run(instruction, environment, context)：
      本地写临时文件 → upload_file 进环境 → exec_as_agent 跑 harness9 --prompt-file
  → run() 返回（正常结束或触达 task.toml 的 agent.timeout_sec=900s 超时）
  → 官方 harness 跑 tests/ 目录下的验证脚本 → resolved/unresolved（具体字段名待 pilot 第一步探查确认）
  → 结果落盘到 --jobs-dir 指定目录
```

---

## 6. 产出物

1. `cmd/harness9/` 的 `--prompt-file` 一次性执行模式（Go 代码 + 单元测试）。
2. `benchmarks/terminal_bench/` 下的 Harbor 适配器代码。
3. Pilot（18 任务）跑完后，仿照 `swebench-轨迹分析与内核优化-v2.md` 的方法论，产出
   `docs/技术调研/terminal-bench-轨迹分析-v1.md`：逐任务对比 agent 行为 vs 隐藏测试/参考解，
   对每条 harness 归因做对抗式复核，按 P0/P1/P2 分级输出优化项。
4. 基于 pilot 结果的分级决策：是否扩大到全量 89 题、是否需要针对某类任务补充工具。

---

## 7. 风险与开放问题（v2 更新）

| 风险/问题 | 应对 |
|---|---|
| harness9 二进制在环境里可能缺运行时依赖 | `CGO_ENABLED=0` 静态编译消除 glibc 依赖；pilot 第一步只跑 `fix-git` 1 个任务验证安装链路 |
| Harbor 的 `results.json` 确切 schema 未源码级核实 | Pilot 第一步跑完后直接探查 `--jobs-dir` 真实目录结构，不要凭猜测的字段名写分析脚本 |
| PyPI 包名 `harbor` 是否真的已发布、是否被别的项目占用 | 安装后 `pip show harbor` 核对 Homepage/Summary 确认是目标包 |
| registry.json 里没有 "2.1" 版本，文档站提到的 2.1 目前查无实据 | 先按已确认存在的 `terminal-bench@2.0`（89 题）实施 |
| harness9 主线的 `cmd/harness9/main.go` 目前只接 `provider.NewOpenAIProvider`，未接线 Anthropic Provider | pilot 用 OPENAI_API_KEY / OPENAI_BASE_URL（可路由到 OpenRouter）+ LLM_MODEL 配置，不要指望直接用 ANTHROPIC_API_KEY |
| Pilot 任务清单是"精选"而非随机采样，分数不能直接对标官方 leaderboard | 本阶段目标是挖根因、不是刷分 |

---

## 8. 明确不做的事（本轮范围之外）

- 不引入 Go 生态基准（已搁置，未来可另起一轮讨论）。
- 不做 agent loop 结构性升级——应等根因分析给出具体证据后再决定。
- 不追求效率指标（回合数/token 数）优化。
- 不实现 `populate_context_post_run`（可选 hook，YAGNI）。
- 不改动 `cmd/harness9/main.go` 里的 Provider 选择逻辑（目前只支持 OpenAI 兼容路径），这是另一个
  独立话题，不在本方案范围内。
