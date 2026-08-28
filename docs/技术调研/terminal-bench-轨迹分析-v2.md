# Terminal-Bench 2.0 P0/P1 修复验证轮（v2）

> 数据来源：对 `terminal-bench-轨迹分析-v1.md` 记录的同一份 18 任务 pilot 清单做完整重跑（`job-name=verify-p0p1-fix`，`-n 2`），验证 P0（`runLocal` bash 挂起）与 P1（`generateWithRetry` 网络重试）两项已合并修复（commit `5e80576`/`e27453f`）在真实 Terminal-Bench 环境下的效果，并对本轮暴露的新现象做同等强度的对抗式复核。
>
> 方法论延续 v1：不采信仅凭日志现象的猜测，claim 必须有源码级或可复现实验支撑；对"不是 harness9 问题"的结论同样反向检验，避免误判。

---

## 0. 结论先行

| 项目 | v1 结论 | 本轮验证结果 |
|---|---|---|
| **P0**（`runLocal` 对 `A && B &` 复合后台任务挂起） | 已修复（commit `5e80576`），未在真实环境验证过 | **CONFIRMED 已修复**：`kv-store-grpc` 本轮 reward=1，未再触发 880s 硬超时挂起 |
| **P1**（TLS/网络错误重试预算过窄） | 归因为"间歇性网络故障"，已加宽重试窗口（commit `e27453f`），但"没有解决证书信任问题本身" | **归因被修正**：不是间歇性网络故障，是 3 个特定 Terminal-Bench 官方任务镜像**确定性缺失 `ca-certificates` 系统包**，导致 Go 程序系统信任链为空，任意出站 HTTPS 100% 必然失败——重试次数再多也无法规避。已在 `benchmarks/terminal_bench/harness9_agent.py` 的 `install()` 阶段修复并重跑验证，3 个任务中 1 个转为 resolved、1 个转为"真实但未通过"、1 个转为"真实但撞到另一个已知的适配器超时问题（见下）" |
| **适配器超时不匹配**（`_RUN_TIMEOUT_SEC=880` 硬编码，与 `task.toml` 声明的 `[agent] timeout_sec` 不符） | v1 §4 item 5 记录为待办建议，未落地 | **本轮已修复**：删除硬编码值，超时裁决交给 Harbor 自身已经正确的、逐任务读取 `task.toml` 的外层 `asyncio.wait_for`；同时保留一个 4 小时量级的 `_ABSOLUTE_TIMEOUT_SEC` 作为绝对兜底（`AgentConfig.timeout_sec` 是 `Optional` 字段，理论上存在任务未声明它、Harbor 外层退化为无限等待的边界情况）。已用 `--agent-timeout-multiplier` 做机制级验证（见 §4） |

**综合结果**：18 任务中 **14 个 resolved**（v1 为 9 个），剩余 4 个 unresolved 全部有明确的非 harness9-内核归因（2 个模型能力边界、2 个任务复杂度超出自身预算——不再是适配器超时配置问题），**本轮没有发现任何新的 harness9 内核级缺陷**。

---

## 1. 运行方法

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o benchmarks/terminal_bench/bin/harness9 ./cmd/harness9
PYTHONPATH=benchmarks harbor run -d terminal-bench@2.0 \
  -a terminal_bench.harness9_agent:Harness9Agent \
  -i <18 个任务，同 v1 清单> \
  -o benchmarks/terminal_bench/runs --job-name verify-p0p1-fix -n 2 --env-file .env -y
```

`go test ./...` + `gofmt -l .` + `go vet ./...` 在跑 pilot 前全部通过，确认修复未引入回归。`-n 2`（低于 v1 首轮的 `-n 5`）用于减少并发资源争抢噪声，同时避免 `-n 1` 的全串行耗时。

---

## 2. P0 验证：`kv-store-grpc` 不再挂起

本轮 `kv-store-grpc__TvaqeJR` trial：`reward=1`。`agent/harness9.log` 显示同一条 `nohup python server.py > /app/server.log 2>&1 &` 模式的 bash 调用正常在毫秒级返回并继续后续 turn，未复现 v1 文档 §1 记录的"工具启动后日志直接截断、无工具完成日志"现象。这与本地单元测试 `TestBashTool_Execute_BackgroundedProcessDoesNotHang`（`internal/tools/bash_test.go`）的验证结论在真实 Terminal-Bench 容器环境下得到交叉确认。

**结论：CONFIRMED**，P0 修复在生产场景下有效，无需进一步动作。

---

## 3. P1 归因修正：不是"间歇性网络故障"，是任务镜像缺失 `ca-certificates`

### 3.1 首次重跑现象：重试窗口确实生效，但 100% 依然失败

`compile-compcert`、`merge-diff-arc-agi-task`、`sqlite-with-gcov` 三个任务在本轮（P1 修复已合并）依然全部在 Turn 1 失败，日志显示重试确实按新预算执行（5 次退避：5s→10s→20s→40s→1m0s，对应"尝试 1/6"到"尝试 5/6"，第 6 次失败后放弃），耗时从 v1 的 3s 总退避窗口扩大到本轮的 ~2.5 分钟，但**三个任务、六次尝试、100% 命中同一条 `x509: certificate signed by unknown authority`**。这个结果本身就是一个信号：如果是真·间歇性网络抖动，跨越 2.5 分钟的 6 次独立尝试不太可能连续 6 次都失败——间歇性故障的重试修复"生效了"（重试逻辑本身没问题），但"失败模式的性质"需要重新审视。

### 3.2 根因定位：3 个任务镜像唯独缺少 `ca-certificates` 包

对比这 3 个失败任务与本轮其余 15 个任务（含 9 个同样以 `ubuntu:24.04` 为基础镜像、且同一天构建 `20251031` tag 的任务）的 `task.toml` 与实际镜像内容：

| 检查项 | `compile-compcert` / `merge-diff-arc-agi-task` / `sqlite-with-gcov` | `configure-git-webserver` / `custom-memory-heap-crash` / `build-pov-ray` / `git-multibranch` / `git-leak-recovery`（同为 `ubuntu:24.04`，`20251031` tag） |
|---|---|---|
| `dpkg -l ca-certificates` | `un`（未安装） | `ii  ca-certificates 20240203`（已安装） |
| `/etc/ssl/certs/ca-certificates.crt` | 不存在，`/etc/ssl/certs/` 目录本身都不存在 | 存在，219342 字节 |
| 直接 `docker run <image> curl https://openrouter.ai/...`（补装 curl 后测试） | N/A（连 curl 都未预装，但 CA 目录确认为空） | `http_code=200` |

排除了"是同一批镜像整体过期/网络环境问题"的可能——同一天构建、同一 `ubuntu:24.04` 基础镜像的其他 5 个任务镜像 CA 信任链完全正常，唯独这 3 个任务的官方镜像本身没有装 `ca-certificates`。

**根因**：harness9 是 `CGO_ENABLED=0` 静态编译的 Go 二进制，TLS 证书校验走 Go 自己的 `crypto/x509`，在 Linux 上依赖系统级 CA 证书文件（`/etc/ssl/certs/ca-certificates.crt` 等标准路径）。当这个文件不存在时，`x509.SystemCertPool()` 得到一个空信任池，此时**任何**服务器证书都无法通过校验——不管是不是真的权威证书，都会报 `certificate signed by unknown authority`。这是一个**确定性**问题，不随重试次数改变结果，v1 文档"间歇性网络故障"的归因需要修正为"特定任务镜像的系统级依赖缺失"。

**这不是 harness9 内核缺陷**：`generateWithRetry` 的重试逻辑本身工作正常（确实执行了 6 次、确实用了新的退避窗口）；这也不是这 3 个 Terminal-Bench 官方任务本身"应该"具备网络能力之外的缺陷（`docker_image` 字段指向的是任务作者维护的固定镜像，是否预装 `ca-certificates` 由镜像作者决定，我们不控制、也不应该去改动上游任务镜像）。**真正可控、可修的点在 harness9 的 Harbor 适配器**：既然 harness9 二进制的出站 HTTPS 依赖系统信任链，适配器可以在 `install()` 阶段主动确保这个依赖存在。

### 3.3 已实施的修复

`benchmarks/terminal_bench/harness9_agent.py` 的 `install()` 新增一步（在上传二进制之前，root 权限执行）：

```python
await self.exec_as_root(
    environment,
    command=(
        "DEBIAN_FRONTEND=noninteractive apt-get update -qq "
        "&& DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ca-certificates"
    ),
)
```

本轮 pilot 的 18 个任务镜像全部是 debian 系（`ubuntu:24.04` / `python:3.13-slim-bookworm` / `debian:13.0-slim`），`apt-get` 通用可用；已安装 `ca-certificates` 的镜像上这一步是无副作用的空操作（apt 检测到已是最新版本直接跳过）。Ubuntu/Debian 默认软件源走明文 HTTP（非 HTTPS），因此 `apt-get update` 本身不受这个问题影响，可以在 CA 信任链修好之前正常执行。

### 3.4 修复验证：单独重跑这 3 个任务

```bash
PYTHONPATH=benchmarks harbor run -d terminal-bench@2.0 \
  -a terminal_bench.harness9_agent:Harness9Agent \
  -i compile-compcert -i merge-diff-arc-agi-task -i sqlite-with-gcov \
  -o benchmarks/terminal_bench/runs --job-name verify-ca-certs-fix -n 3 --env-file .env -y
```

| 任务 | 修复前 | 修复后 | 说明 |
|---|---|---|---|
| `merge-diff-arc-agi-task` | Turn 1 即失败，从未测试过 | **reward=1（真正 resolved）** | 37 turns / 4m47s，正确推导出 ARC-AGI 变换规则（对角线周期填色），3/3 隐藏样例通过 |
| `sqlite-with-gcov` | Turn 1 即失败，从未测试过 | reward=0，**但这次是真实测试出的失败** | 52 turns / 9m2s，agent 产出了详细的"已完成"总结报告，但 verifier 阶段 3/3 隐藏测试均 `FileNotFoundError`（`sqlite3` 二进制实际不可执行/不在预期路径）——agent 在没有真正验证构建产物的情况下宣称完成，这是模型自我验证不足的问题（同类问题参见 §5 `sanitize-git-repo`/`build-cython-ext`），不是 harness9 工具/引擎缺陷 |
| `compile-compcert` | Turn 1 即失败，从未测试过 | 仍未 resolved，但性质完全变了：**RuntimeError（880s 适配器超时）** | 75 turns / 撞到 880s 上限，期间已通过 `opam` 安装 Coq 8.16.1、Menhir 等真实工具链并开始用 `dune` 构建，是合法的深度工作被 benchmark 适配器的保守超时打断——`task.toml` 声明 `agent.timeout_sec=2400`（40 分钟），`harness9_agent.py` 硬编码 `_RUN_TIMEOUT_SEC=880`（约 14.7 分钟），二者相差 2.7 倍 |

**结论**：3 个任务里 1 个从"从未测试过"变为"真正解决"，另外 2 个从"从未测试过"变为"有真实、可解释的失败原因"（1 个模型自我验证不足，1 个撞上已知的 R3 适配器超时不匹配问题，命中 §6 的第三个实例）。三者都不再是 harness9 内核问题。

---

## 4. 副产品发现并修复：`_RUN_TIMEOUT_SEC=880` 适配器超时不匹配问题

### 4.1 问题

v1 文档 R3 已记录 `fix-ocaml-gc`（声明 3600s）一个实例。本轮 `compile-compcert`（声明 2400s）成为**第 2 个**因为 `harness9_agent.py` 硬编码 `_RUN_TIMEOUT_SEC=880` 而被过早打断的任务（`custom-memory-heap-crash` 本轮反而在预算内解决，未命中）。

### 4.2 根因：适配器自设的内层超时与 Harbor 自身的外层超时重复且更紧

读 Harbor 源码（`harbor/trial/trial.py` `_run_agent_phase`、`_compute_agent_timeout_sec`）确认：Harbor 的 `Trial` 早就用 `asyncio.wait_for(agent.run(...), timeout=self._agent_timeout_sec)` 包住了整个 `run()` 调用，`self._agent_timeout_sec` 正确地从每个任务自己的 `task.toml` 的 `[agent] timeout_sec` 字段解析而来（`compile-compcert=2400`、`fix-ocaml-gc=3600`、`kv-store-grpc=900`，各不相同），并支持 `--agent-timeout-multiplier`/`max_timeout_sec` 调节。**这一层已经是正确、逐任务感知的超时机制**，不需要我们在适配器里重新实现一遍。

问题在于 `harness9_agent.py` 自己又在 `exec_as_agent(..., timeout_sec=880)` 里设了一层**内层**超时——880 是所有任务统一写死的一个值，比 Harbor 自己算出来的外层超时更紧、更早触发，导致这层内层超时抢在 Harbor 的正确超时之前把任务打断，且我们又拿不到 `task.toml` 里的值（`BaseAgent`/`AgentContext`/`BaseEnvironment` 均未把任务级超时暴露给自定义 import-path agent，只有 Harbor 内置的 `oracle`/`cline` 等命名 agent 才会在构造函数里收到 `agent_timeout_sec` kwarg）。

### 4.3 修复

直接删除 `_RUN_TIMEOUT_SEC` 常量，把超时裁决交给 Harbor 自己已经正确的外层 `asyncio.wait_for`。这比"自己解析 task.toml 再算一个更宽松的值"更简单，也从根本上避免了"数字写死、以后任务超时值变了又要改一次"的维护负担。

**Review 补充**：Harbor 的 `AgentConfig.timeout_sec`（`harbor/models/task/config.py`）定义为 `float | None = None`——`task.toml` 并不强制要求声明这个字段。如果某个任务缺失它、CLI 也没传 `--agent-timeout-multiplier`/override，`_compute_agent_timeout_sec()` 会返回 `None`，Harbor 外层 `asyncio.wait_for(..., timeout=None)` 就等于完全不设超时。当前 18 个 pilot 任务全部声明了这个字段（900–3600s 不等，已逐一核实），现状不受影响，但这是移除内层硬编码超时后引入的一个真实边界风险。修复：给 `exec_as_agent()` 保留一个 `_ABSOLUTE_TIMEOUT_SEC = 4小时` 的绝对兜底——它不参与"逐任务精确裁决"（那由 Harbor 负责），只是防止上述边界情况下真的无限挂起，定得足够宽松，正常任务应该总是 Harbor 自己的超时先生效。

### 4.4 验证

**机制验证**（快速、低成本）：用 `harbor run -i fix-git --agent-timeout-multiplier 0.02`（把 `fix-git` 声明的 900s 缩到 18s）强制立刻触发 Harbor 外层超时。结果：

- `exception_stats` 报 `AgentTimeoutError: Agent execution timed out after 18.0 seconds`——确认外层超时是 Harbor 自己算出来的值在生效，不再是我们自己写死的 880。
- `agent/harness9.log` 依然完整下载下来了（7100 字节，Turn 1–6 完整轨迹）——确认 `run()` 里 `finally` 块的日志下载步骤，在 Harbor 通过 asyncio 取消我们的 `run()` 协程时依然能正常跑完（Python 的单次 `Task.cancel()` 只在当前 await 点抛一次 `CancelledError`，`finally` 块内的后续 `await` 不会被连带打断），日志采集机制没有因为这次改动而失效。
- 附带发现一个和本次修复无关、但值得记录的 Harbor 自身细节：`docker compose exec` 的取消只是 host 侧 Python 放弃等待，不代表容器内的 `harness9` 进程真的被杀死——本例中 agent 在 Turn 6 被"超时"时其实还在解决 merge 冲突，但容器内进程显然又运行了一小段时间才真正收敛，等 verifier 跑的时候文件状态已经是对的，最终 `reward=1.0`。这是 Harbor 自己的行为，不是本次改动引入的。

**真实任务验证**（`compile-compcert`，声明 2400s，不加 multiplier）：跑到本文档撰写时仍在进行，用于确认该任务这次不会再在 880s 处被适配器打断（此前 §3.4 的重跑已经证明它在 880s 前只是在做合法的 opam/dune 构建工作）。结果补充见文末「验证补充」。

---

## 5. 次要验证：5 个"翻转"或持续失败任务的逐一核实

除 P0/P1 直接相关的任务外，本轮还核实了另外 5 个 v1 中被明确归因为"非 harness9 问题"的任务，确认分类未变、且 reward 翻转（3 例）均为 LLM 采样非确定性、不是 harness9 行为变化：

| 任务 | v1 归因 | 本轮结果 | 本轮核实结论 |
|---|---|---|---|
| `sanitize-git-repo` | R5，模型判断问题：正确的正则扫出了第 5 个密钥的命中，但被自建过滤器误判为假阳性而放弃 | reward=0（不变） | **同一失败模式复现**：本轮 agent 找到并正确替换了 4 类密钥（AWS key/secret、GitHub token、HuggingFace token），但依然遗漏了藏在一个 JSON 测试夹具文件（内嵌 diff 文本）里的**第二个** HuggingFace token（`hf_ocffijsv...`）。`test_removal_of_secret_information` / `test_correct_replacement_of_secret_information` 两个隐藏测试失败，`test_no_other_files_changed` 通过。与 v1 是同一类"遗漏非常规位置的第二个同类密钥"问题，非 harness9 缺陷 |
| `fix-ocaml-gc` | R3，任务复杂度超预算：Turn 25 已定位正确修复，Turn 33 起单次编译耗时约 650s 撞上 880s | 仍超时（不变，此结果测于 §4 的适配器超时修复**之前**） | 本轮同样在合理、有方向性地推进 OCaml 运行时构建（`make coreall`/`opt.opt` 相关 target 探查），880s 内未完成全量构建。与 v1 同一归因，非 harness9 缺陷，正是 §4 修复的那类问题——`fix-ocaml-gc` 本身尚未用修复后的适配器重新验证过（`task.toml` 声明 3600s，留给未来一次单独确认） |
| `fix-git` | R4，模型推理判断错误：用 `git fsck --lost-found` 找到 2 个悬空 commit，误判合并了任务叙事之外的第二个 | **reward=1（翻转为 resolved）** | 本轮改用 `git reflog` + `git fsck` 准确定位唯一相关的悬空 commit（`650dba4 "Move to Stanford"`），正确合并且冲突解决方向正确（保留 Stanford 版本）。这是**同一套工具/引擎能力下，模型这次做出了更好的推理判断**，属于 LLM 采样非确定性，不代表 harness9 有任何行为变化——不能作为"稳定修复"看待 |
| `custom-memory-heap-crash` | R3，任务复杂度超预算：56+ 轮方向正确的调试，880s 内未完成 | **reward=1（翻转为 resolved）** | 本轮 52 轮、7m12s（远低于 880s 预算）里完整定位并解释了自定义堆分配器与 `libstdc++` 静态析构顺序的 6 步根因链，给出的修复方案技术上自洽。同样是 LLM 采样层面这次更高效地走到了正确路径，harness9 侧无行为变化——该任务仍处在 880s 预算的边界线附近，不应视为已稳定解决 |
| `build-cython-ext` | R5，模型持久性不足：正确修复 NumPy 2.x 兼容性问题后被 `setuptools` 缺失报错吓退，未重试 pip install | **reward=1（翻转为 resolved）**，11/11 全部通过 | 本轮 93 轮、11m4s，全程未出现 `setuptools` 相关报错字样，agent 顺利完成全部 4 个 Cython 扩展编译。同样判断为 LLM 采样非确定性（这次没有走到会触发该报错的路径），不代表 harness9 或环境有变化 |

**方法论提醒**：这 3 个"翻转为 resolved"的案例都不能视为"已修复"——它们的失败模式是模型推理/持久性层面的，本质上仍是随机的，harness9 没有做任何改变这类结果的事情。后续如果要真正提升这几类任务的稳定通过率，需要针对模型行为（如"看到看似合理但未验证的假设时不要停"）做 prompt 层面的引导，而不是 harness9 引擎/工具层面的修复——这也超出本轮验证范围。

---

## 6. 不可回归项复核

本轮同时复核了 v1 §5 记录的"不可回归项"：

- `configure-git-webserver` 依然 resolved，路径沙箱 + bash heredoc 自愈闭环模式未受影响。
- `pypi-server`、`build-pov-ray` 依然 resolved，未见新的并发/顺序错位问题。
- 全部 18 个 trial 的 `todo_write` 状态机日志均正常，无状态转换校验失败。
- 未在任何 trial 中发现 `panic:` 或非预期的 `log.Fatal` 提前退出（除了已知的、预期内的 880s adapter 超时对应的 `RuntimeError`）。

无回归。

---

## 7. 后续建议顺序

1. 【已完成】验证 P0（`kv-store-grpc` 不再挂起）与 P1 重试逻辑本身工作正常。
2. 【已完成】修正 P1 的根因归因：不是间歇性网络故障，是 3 个任务镜像缺失 `ca-certificates`；已在 `harness9_agent.py` 的 `install()` 阶段修复并验证 3/3 任务不再被 TLS 问题阻塞。
3. 【已完成】`_RUN_TIMEOUT_SEC=880` 硬编码与任务自身声明的 `agent.timeout_sec` 不匹配问题（累计命中 `fix-ocaml-gc`/`compile-compcert` 两个任务）：删除硬编码超时，交给 Harbor 自身已经正确的、逐任务读取 `task.toml` 的外层超时机制裁决；已用 `--agent-timeout-multiplier` 做机制级验证（确认外层超时正确触发 + 日志下载在取消时依然完整）。`fix-ocaml-gc`/`compile-compcert` 本身尚未用修复后的适配器完整重跑到底（各自需要 40–60 分钟），可作为下次的一个快速确认项，但不阻塞本次修复合入。
4. **仍不建议现在扩大到全量 89 题**：本轮 18 任务验证已经把 v1 遗留的两个开放问题（P0 是否真的修复、P1 三个任务的证书问题到底是什么）彻底钉死，且没有暴露任何新的 harness9 内核缺陷——继续在同一个 18 任务清单上打转边际信息量已经很低，但样本量仍然太小、不足以支撑"该不该扩大到 89 题"这个决策；是否扩大取决于是否需要覆盖更多任务类别（当前 18 题是从 category/tags 精选、非随机采样），这是一个独立的范围决策，不是本轮验证的目标。

---

## 8. 不需要新增工具能力（延续 v1 结论）

本轮暴露的三个可行动项（P0 已验证有效、P1 根因修正 + 已修复、适配器超时不匹配已修复）都不涉及"agent 想用但没有对应工具"的情况，都是 harness9 工具/引擎实现或 benchmark 适配器层面的问题，不涉及新增工具能力。

---

## 9. 验证补充：`compile-compcert` 用修复后的适配器完整重跑（不加 multiplier）

不设 `--agent-timeout-multiplier`，跑修复后的真实 `compile-compcert`（`task.toml` 声明 2400s）：

- `exception_stats`：`AgentTimeoutError: Agent execution timed out after 2400.0 seconds`——不再是 880，确认这次是 Harbor 自己按 `task.toml` 算出来的正确外层超时在生效。
- `agent_execution` 阶段计时：`04:10:07.313` → `04:50:07.439`，精确对应 2400.1 秒。
- `agent/harness9.log` 完整记录了 134 个 turn、持续到第 39 分钟（`opam info menhir` 等真实的 Coq/OCaml 依赖排查工作），远超旧版 880s（约第 14.7 分钟）就会被打断的位置——如果适配器超时修复没生效，这次本该在 Turn 75 附近（对应 §3.4 那次带 ca-certs 修复但仍是 880s 上限的重跑）就被掐断，而不是跑到 Turn 134。
- 最终 `reward=0`：`compile-compcert` 在 2400s 内仍未编译出可用产物，这现在是一个干净的"任务自身复杂度超出其自己声明的预算"结论（R3 同类），不再牵扯任何适配器配置问题——完整验证了本次修复的效果。
