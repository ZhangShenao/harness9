# Terminal-Bench 2.0 Pilot 轨迹分析与 harness9 内核优化（v1）

> 数据来源：Terminal-Bench 2.0 18 任务 pilot，通过 Harbor `BaseInstalledAgent` 适配器（`benchmarks/terminal_bench/harness9_agent.py`）调用预编译 harness9 二进制。9 resolved / 9 unresolved。
> 分析方法：对每条 unresolved 轨迹逐一读取 `agent/harness9.log`（完整执行轨迹）、`verifier/test-stdout.txt` + `ctrf.json`（隐藏测试真实输出）、Harbor 任务缓存中的 `instruction.md`/`task.toml`/`tests/`/`solution/`（任务真实要求与参考解），并对每条根因归因做**对抗式复核**：核对当前 harness9 真实源码（`internal/tools/bash.go`、`internal/engine/agent_loop.go`），必要时用最小 Go 复现程序验证机制性假设，不采信仅凭日志现象的猜测。

---

## 0. 方法论：两轮跑法与噪声剥离

第一轮以 `-n 5` 并发跑 18 个任务，5 个 Docker 环境同时抢占本机资源与出站网络，产出的失败信号混有大量环境噪声（TLS 证书校验失败、verifier/agent 阶段超时），无法直接归因到 harness9 本身。

为区分"真实缺陷"与"并发资源争抢噪声"，对第一轮中结果含糊的 10 个任务用 `-n 1`（无并发争抢）重新单独跑一遍：

- **3 个任务重跑后干净通过**（`configure-git-webserver`、`pypi-server`、`build-pov-ray`），首轮失败原因分别是 verifier/agent 阶段的 880–900s 超时——**证实为并发噪声，非真实问题**。
- **7 个任务重跑后依然失败，但呈现更干净、确定性更强的错误信号**（部分从"TLS 证书错误"变为"TLS 证书错误但更早发生"或维持不变、部分从"环境启动超时"变为"真实执行超时"）——这 7 个 + 首轮 2 个已跑出真实 pytest 失败的任务（`sanitize-git-repo`、`build-cython-ext`，两次都不依赖并发，pytest 确实跑了且确实失败），构成本文档深挖的 9 个 unresolved 案例。

**取值原则**：被重跑过的任务，以重跑（`-n 1`）结果为准；未被重跑的任务以首轮结果为准。

### 结论先行

| 结论 | 任务 | 说明 |
|---|---|---|
| **Resolved（9）** | git-leak-recovery / sqlite-db-truncate / nginx-request-logging / openssl-selfsigned-cert / build-pmars / git-multibranch（名义 unresolved，实为 resolved，见下） / configure-git-webserver / pypi-server / build-pov-ray | 后三者首轮因并发噪声超时，重跑干净通过；git-multibranch 首轮 pytest 1/1 passed 但 Harbor 自身 verifier 采集阶段误判超时 |
| **P0：真实内核缺陷（1 类，命中 1/9）** | kv-store-grpc | `bash` 工具本地执行路径对"`A && B &`"复合后台任务模式存在真实的进程管理缺陷，导致工具调用永久挂起直至 880s 硬超时；已用最小 Go 程序复现 |
| **P1：基础设施韧性缺陷（1 类，命中 3/9）** | compile-compcert / merge-diff-arc-agi-task / sqlite-with-gcov | Turn 1 首次 LLM 调用即遭遇 TLS x509 证书校验失败（OpenRouter 出站连接），重试 3 次后耗尽退避、进程非零退出；不区分"可重试的瞬时网络错误"与"同类错误"，重试策略本身合理但强度不足以应对这类间歇性故障 |
| **不算 harness9 问题（2 类，命中 2/9）** | fix-git（非确定性判断错误）/ custom-memory-heap-crash + fix-ocaml-gc（任务复杂度超预算，2/9） | fix-git 是 LLM 采样层面的推理判断错误，非工具/引擎缺陷；后两者是真实、有条理的深度调试/编译在 880s 预算内没跑完，非空转 |
| **纯模型能力差距（2/9）** | sanitize-git-repo / build-cython-ext | 隐藏测试真的跑了、真的失败，agent 有机会自我纠正但没有——不是 harness9 工具/引擎问题 |

**核心结论**：9 个 unresolved 案例中，只有 **1 个（kv-store-grpc）是 harness9 内核级、可复现、有源码证据支撑的真实缺陷**，值得作为 P0 修复；3 个是可归入"重试策略韧性不足"的 P1；剩下 5 个分别是模型推理/能力边界问题（fix-git、sanitize-git-repo、build-cython-ext）与任务复杂度超预算（custom-memory-heap-crash、fix-ocaml-gc），不应计入 harness9 的技术债。

---

## 1. 决定性证据：kv-store-grpc 的 bash 工具永久挂起

### 1.1 现象

`kv-store-grpc` 任务在两次独立运行中（首轮 `-n5`、重跑 `-n1`）都在**完全相同的执行点**触发 880 秒硬超时：

| 运行 | trial | 挂起前最后一条日志 | 日志总行数 |
|---|---|---|---|
| 首轮 | `2026-07-21__15-51-35/kv-store-grpc__6Sw4dQL` | `工具启动 │ tool=bash │ command="cd /app && nohup python server.py > /app/server.log 2>&1 &\necho \"PID: $!\"\nsleep 2\ncat /app/server.log"` | 319 行，无对应"工具完成" |
| 重跑 | `2026-07-21__17-22-15/kv-store-grpc__Ko4tDmh` | `工具启动 │ tool=bash │ command="cd /app && nohup python server.py > /app/server.log 2>&1 & echo \"PID: $!\""` | 270 行，无对应"工具完成" |

两次日志都在这条 `bash` 调用的"工具启动"行之后**直接截断**，没有下一行"工具完成"，直到 Harbor 侧的 880 秒外层超时把整个进程杀掉。任务指令本身（`instruction.md` 第 5 步）明确要求"Run the server.py file and keep it running in the background"——这是任务规范强制要求的操作模式，agent 没有选择余地。

### 1.2 根因定位：`os/exec.Cmd.CombinedOutput()` 与 bash 后台任务语义冲突

`internal/tools/bash.go` 的本地执行路径（`runLocal`，第 153–170 行）：

```go
func (t *BashTool) runLocal(ctx context.Context, cmd string, timeout time.Duration) (string, error) {
	c := exec.CommandContext(ctx, "bash", "-c", cmd)
	c.Dir = t.workDir
	out, err := c.CombinedOutput()
	...
}
```

`CombinedOutput()` 内部创建一对 pipe 作为子进程（顶层 `bash -c`）的 stdout/stderr，并调用 `Wait()`。Go 的 `os/exec.Wait()` 会阻塞，直到该 pipe 的写端被**所有持有者**关闭——不仅是顶层 `bash -c` 进程本身，还包括它 fork 出的任何子孙进程，只要那些子孙进程在 fork 时继承了这个 pipe 的写端且没有显式关闭它。

问题出在 bash 的后台任务（job control）语义：形如 `A && B &` 时，`&` 作用于**整个 `A && B` 复合列表**，bash 会为这个复合列表 fork 一个子 shell 去异步执行，父 shell 立即返回。`nohup python server.py > /app/server.log 2>&1` 这部分内部命令的 stdout/stderr 确实被 `nohup` 正确重定向到了文件——但 fork 出来执行 `cd /app && nohup ... &` 这个复合列表的中间子 shell 进程，在 fork 那一刻已经**继承**了顶层 `bash -c` 进程的 stdout/stderr pipe 写端副本，且这一层不会被 `nohup` 的重定向影响（重定向只发生在 `nohup` exec 替换之后的最终进程里）。只要这个中间层/最终的后台进程还活着，pipe 写端就还有存活的持有者，Go 侧的 `Wait()` 就会一直阻塞。

`echo "PID: $!"` 这条同一命令里的后续语句会立即执行并打印，但因为它和 `&` 之前的整段命令是**同一次 `bash -c` 调用**，Go 侧看到的不是"某一行的输出"，而是整个进程的"退出+关闭所有 fd"，只要背景进程还没退出，`Wait()` 就不会返回，`echo` 的输出也无法被读到——因为 `CombinedOutput()` 是"进程退出后一次性读取全部缓冲"的模式，不是流式的。

### 1.3 最小复现（已验证，非源码阅读猜测）

用与轨迹完全一致的命令模式独立复现：

```go
ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
c := exec.CommandContext(ctx, "bash", "-c",
    `cd /tmp && nohup sleep 20 > /tmp/e.log 2>&1 & echo "PID: $!"`)
out, err := c.CombinedOutput()
// 结果：elapsed=20.01s（等满被 nohup 的进程的完整生命周期，而不是立即返回）
```

对照组证实问题定位准确：

| 命令模式 | 结果 |
|---|---|
| `nohup sleep 30 > file 2>&1 & echo ...`（无 `cd &&` 前缀） | **立即返回**（~7ms） |
| `true && nohup sleep 30 > file 2>&1 & echo ...`（有 `&&` 前缀） | **挂起满 30s** |
| `cd X && nohup sleep 20 > file 2>&1 & echo ...`（轨迹原始模式） | **挂起满 20s** |
| `(cd X && nohup sleep 20 > file 2>&1 &) ; echo ...`（用 `()` 包裹整段后台任务） | **立即返回**（~5ms） |
| `cd X && setsid nohup sleep 20 > file 2>&1 < /dev/null & echo ...`（`setsid` + 显式 stdin 重定向） | **立即返回**（~4ms） |

复现证实：**只要 `&` 前面存在 `&&` 链接的复合命令列表**，Go 的 `exec.Cmd.CombinedOutput()`/`Wait()` 就会阻塞到背景进程结束——这与"是否用了 `nohup`"无关，根源是 bash job control 对复合列表取整体后台化时产生的中间层进程继承了 pipe fd。用 `()` 子 shell 包裹整段、或 `setsid` 脱离会话，都能规避。

### 1.4 对抗式复核

- **驳回"这是 Sandbox/Docker 层问题"的猜测**：两次轨迹开头都记录 `Sandbox 启动失败，已降级为本地进程模式: ... exec: "docker": executable file not found in $PATH`——运行环境里 Docker 二进制本身不存在，engine 走的是 `runLocal`（本地进程）路径，不涉及 `runInSandbox`/`docker exec`。该缺陷与 Sandbox 模块无关，是本地路径的独立问题；但需注意如果 Sandbox 开启、命令通过 `docker exec` 转发，`docker exec` 本身也是"父进程等子进程树全部退出"的语义，同样的复合后台任务模式大概率会在 `DockerEnvironment.RunBash` 上重现（未在本次 pilot 环境验证，因为 Docker 不可用；标记为需要后续在 Sandbox 开启环境下单独复现确认）。
- **驳回"这只是 880s 超时预算不够"的猜测**：`timeout_secs` 未在这条命令里显式指定，因此走 `defaultBashTimeout=120s`——理论上这条 bash 调用应该在 120 秒后被工具层的 `context.WithTimeout` 打断并返回 `[TIMEOUT ...]` 横幅，而不是拖到 880s 才被 Harbor 外层杀掉。这意味着还存在第二层问题：**工具层的 120s 超时本身可能也没有生效**，因为 `context.WithTimeout` 触发后 `ctx.Done()` 会让 `exec.CommandContext` 向子进程发 `SIGKILL`，但 SIGKILL 只杀死顶层 `bash -c` 进程，不会杀死已经 fork 脱离、且不在同一进程组的后台孙进程（`nohup` 的关键作用之一就是脱离父进程信号），管道写端的持有者（后台孙进程）依然存活，`Wait()` 依然会阻塞——**这才是挂起能撑到 880s 而不是在 120s 处被拦下来的真正原因**，且比"根本没有超时保护"更隐蔽。
- **结论：CONFIRMED**，且比最初假设的"没有 120s 超时兜底"更严重——**有超时兜底但兜底本身失效**（`SIGKILL` 打不穿孙进程持有的 pipe fd）。

---

## 2. 根因分级（已对抗式复核）

| # | 根因 | 归属 | 命中实例 | 复核结论 |
|---|------|------|---------|---------|
| **R1** | **bash 本地执行路径对 `A && B &` 复合后台任务挂起，且 120s 工具超时的 `SIGKILL` 打不穿已脱离的孙进程**，导致挂起持续到 880s 外层超时 | tools | kv-store-grpc（1/9，但任务要求"跑后台服务"是一类通用场景，非孤例） | **confirmed**（源码 + 独立最小复现双重验证） |
| **R2** | **LLM 生成失败重试策略不区分错误类别**：TLS/x509 证书校验失败与其他瞬时错误走同一套 3 次、1s/2s 退避逻辑，退避总时长仅 3s（1s+2s），对间歇性网络故障的容错窗口过窄 | engine | compile-compcert / merge-diff-arc-agi-task / sqlite-with-gcov（3/9） | confirmed（源码核对 `generateWithRetry`，无错误类型判断分支） |
| **R3** | **Harbor Terminal-Bench 适配器超时（880s）显著低于部分任务自身声明的 `agent.timeout_sec`**（fix-ocaml-gc 声明 3600s，custom-memory-heap-crash 声明 1800s） | runner（`harness9_agent.py`，非 harness9 核心） | fix-ocaml-gc / custom-memory-heap-crash（2/9） | confirmed（`task.toml` 核对），但**不是 harness9 内核问题**，是 benchmark 适配层的保守取值 |
| **R4** | **LLM 采样层面的推理判断错误**：面对两个都"看起来合理"的候选修复对象，选择了范围更广但技术上错误的路径 | model（非 harness/工具缺陷） | fix-git（1/9） | confirmed 为模型问题，refuted 为 harness9 问题——所有工具调用均 `status=ok`，无超时/截断/沙箱误拦截。**注**：fix-git 首轮（`-n5`）同样命中了 R6 驳回 git-multibranch 时用到的那个 Harbor 侧 `VerifierTimeoutError`（verifier 未跑完，`verifier_result=null`，不能作为首轮判据）；本条结论完全基于重跑（`-n1`）轨迹，与 git-multibranch 不同的是，fix-git 重跑后 verifier 正常跑完且真实判定为 unresolved（两处隐藏测试均失败），不存在"名义失败、实为通过"的反转 |
| **R5** | **模型能力边界**：隐性证据已经出现在工具输出里但被模型自己的启发式规则误判为噪音而放弃；或者遇到一行修复即可解决的报错却没有重试 | model（非 harness/工具缺陷） | sanitize-git-repo / build-cython-ext（2/9） | confirmed 为模型问题——预算充裕（build-cython-ext 仅用 6m15s/15min）、输出未截断，agent 自己选择了停止 |
| **R6**（伪根因，已驳回） | ~~"git-multibranch 是 harness9 判定 unresolved 的真实失败"~~ | — | git-multibranch | **refuted**：`ctrf.json` 显示 1/1 passed，`reward.txt`=1；Harbor 侧 `_collect_buffered_output` 对 docker exec 的异步管道采集自身 900s 超时（`VerifierTimeoutError`），与 harness9 agent 执行结果无关，是 Harbor verifier 阶段的独立 bug |
| **R7**（伪根因，已驳回） | ~~"compile-compcert/merge-diff-arc-agi-task/sqlite-with-gcov 首轮的 TLS 错误是并发噪声，重跑后的失败才是任务本身的真实能力缺口"~~ | — | 这 3 个任务 | **refuted**：重跑后的日志显示 Turn 1 依然是同一条 `x509: certificate signed by unknown authority`，全程只有 9 行日志、连一次工具调用都没发生。首轮与重跑失败原因完全相同（TLS），不存在"噪声退去后暴露的真实能力缺口"——这 3 个任务目前**没有任何一次真正测试过 harness9 的任务解决能力**，需要在网络环境修复后重新采集数据才有意义 |

---

## 3. 逐实例归因表

| 任务 | 失败模式 | 关键证据 | 主因 | 结论 |
|---|---|---|---|---|
| kv-store-grpc | 工具挂起至 880s 硬超时 | 两次独立轨迹在同一条 `nohup ... &` bash 调用后日志截断；最小 Go 程序复现同一挂起 | R1 | **P0** |
| compile-compcert | Turn 1 LLM 调用 0/3 次成功 | `tls: failed to verify certificate: x509: certificate signed by unknown authority`，全程 9 行日志，零工具调用 | R2 | **P1** |
| merge-diff-arc-agi-task | Turn 1 LLM 调用 0/3 次成功 | 同上，同样 9 行日志、同样错误串 | R2 | **P1** |
| sqlite-with-gcov | Turn 1 LLM 调用 0/3 次成功 | 同上（两次运行、两个不同 UTC 时间点均命中，排除单次瞬时巧合） | R2 | **P1** |
| fix-git | 最终产物 MD5 与 gold 不符（基于重跑轨迹，首轮命中 R6 同类 VerifierTimeoutError 不可用作判据） | 用 `git fsck --lost-found` 而非 `git reflog` 发现了 2 个悬空 commit，误判"更完整版本"应采用，合并了任务叙事之外的第二个 commit，覆盖了正确答案 | R4 | 模型推理问题，非 harness9 缺陷 |
| custom-memory-heap-crash | 880s 执行超时 | 56+ 轮均为新的、有方向性的底层调试（符号表/ABI/静态链接分析）；877.1s 里 828.9s（94.5%）是 LLM 思考耗时，仅 48.1s 用于工具执行；task 自评 `junior_time_estimate_min=1200` | R3 | 任务复杂度超预算，非 harness9 缺陷 |
| fix-ocaml-gc | 880s 执行超时 | Turn 25 已定位并应用与官方解一致的修复（`Whsize_hd(hd)`→`wh`），Turn 33 起的单次 `make -j$(nproc)` 编译消耗了约 650s（74% 预算），任务自身声明 `timeout_sec=3600` 但 Harbor 适配器硬编码 880s | R3 | 任务复杂度超预算（且适配器超时取值过紧），非 harness9 内核缺陷 |
| sanitize-git-repo | 2/3 隐藏测试失败 | agent 用正确的正则 `hf_[A-Za-z0-9]{30,}` 扫出了第 5 个密钥的命中，但被自建的"变量名排除过滤器"误判为假阳性而放弃，宣称"清理完毕" | R5 | 模型判断问题，非 harness9 缺陷 |
| build-cython-ext | 9/11 隐藏测试失败 | 正确修复了全部 NumPy 2.x 兼容性问题，但 `build_ext --inplace` 报 `ModuleNotFoundError: No module named 'setuptools'` 后直接放弃，未尝试 `pip install setuptools`（预算仅用 6m15s/15min） | R5 | 模型持久性问题，非 harness9 缺陷 |
| git-multibranch | 名义 unresolved | `ctrf.json` 1/1 passed，`reward.txt`=1，Harbor 侧异步管道采集自身超时（`VerifierTimeoutError`） | R6（伪根因） | **实为 resolved**，Harbor 侧 bug |

---

## 4. 优化方案

### P0 — bash 后台任务挂起（决定性、内核级）

1. **`internal/tools/bash.go` `runLocal`**：不再用 `exec.CommandContext` + `CombinedOutput()` 的"一次性等待+读取"模式执行原始命令字符串。推荐两条互补路径：
   - **路线 A（进程组隔离）**：用 `syscall.SysProcAttr{Setpgid: true}` 给顶层 `bash -c` 进程分配独立进程组；超时或取消时改用 `syscall.Kill(-pgid, syscall.SIGKILL)` 杀整个进程组而非单个 PID——解决"SIGKILL 打不穿孙进程"的问题，但**打不穿已经因 `&` 完全脱离进程组、且被 `nohup` 忽略了 HUP 的后台任务**（`kv-store-grpc` 场景恰好如此，因为任务要求"保持运行"，杀死它反而是错误行为）。
   - **路线 B（流式读取 + 不等待孙进程）**：改用 `StdoutPipe()`/`StderrPipe()` + `cmd.Start()` 后立即用 goroutine 流式读取，只等待**顶层进程**（`bash -c` 本身）退出（`cmd.Wait()` 在部分平台仍会等 fd 全关闭，需要配合 `Setpgid`+显式关闭 pipe 写端或改用 `pty`/中间层脚本包裹用户命令为 `cmd 2>&1 & disown; wait $!`-类模式规避）。更稳妥的落地：在传给 bash 的命令外包一层 `timeout`-safe 的 subshell 隔离，即把 LLM 原始 `command` 用 `bash -c '{ %s ; } 3>&- ' ` 或显式在 harness 侧对识别出末尾 `&` 的模式追加 `disown`/重定向 stdin/stdout/stderr 到 `/dev/null` 的兜底逻辑。
   - 二者选一均可解决 kv-store-grpc 场景；**建议路线 A 优先**（改动小、语义清晚，只需在 `exec.Cmd` 构造时加 `SysProcAttr`，超时时改为杀进程组），路线 B 作为后续增强。
2. **补充自动化测试**：在 `internal/tools/bash_test.go` 中新增一个表驱动用例，精确复现本文档 §1.3 的命令模式（`cd X && nohup <long-running> > file 2>&1 & echo ...`），断言在远小于 120s 的时间内返回，而不是等满超时——防止此问题在未来的实现变更中悄悄复发。
3. **`runInSandbox`（Docker 路径）同构缺陷，已通过完整调用链源码交叉验证**：`DockerEnvironment.RunBash`（`internal/sandbox/docker_environment.go` 第 35–44 行）把 LLM 的原始命令整体传给 `docker exec -w workDir containerID bash -c cmd`，实际执行通过其 `run` 字段（`cmdRunner` 函数类型）完成；生产环境下这个字段在 `internal/sandbox/manager.go` 第 56 行被注入为 `realCmdRunner`（`internal/sandbox/container.go` 第 52–55 行）——也就是说 `RunBash` 最终确实落到 `exec.CommandContext("docker", args...).CombinedOutput()`，与 `bash.go` `runLocal` 是**同一个 Go 标准库模式**，调用链已完整追溯（`RunBash` → `e.run` → `realCmdRunner` 注入点），不是"猜测两者用了类似写法"。只要 `cmd` 内部包含 `A && B &` 这种复合后台任务，容器内 fork 出的中间子 shell 同样会继承 `docker exec` 进程的 stdout/stderr 管道写端，Go 侧的 `CombinedOutput()` 一样会等到容器内后台进程退出才返回。**唯一未验证的一环**：当前 pilot 环境 Docker 二进制本身不可用（两条日志都记录了 `docker: executable file not found in $PATH`），未能端到端实测复现容器内的挂起现象本身——源码调用链已确认，但"bash 在 `docker exec` 场景下的 job-control 行为与本机场景完全一致"这一步仍是合理推断而非实测。理应视为与 `runLocal` 同一等级的缺陷，需要在修复 `bash.go` 时同步修复 `internal/sandbox/container.go` 的 `realCmdRunner`（两处收敛到同一套进程组隔离逻辑），而不是只修一处。

### P1 — LLM 生成重试的错误分类（中杠杆）

4. **`internal/engine/agent_loop.go` `generateWithRetry`**：区分错误类别后采用不同重试策略。TLS/证书/DNS 解析失败这类连接建立阶段的错误，本质上和限流/5xx 不同——它可能需要更长的等待窗口（网络路径抖动、CDN 边缘节点切换），当前"3 次、1s→2s、总计 &lt;10s 退避后即放弃整个 turn 并使 harness9 进程非零退出"的窗口太窄。建议：识别 `x509`/`tls:`/`no such host`/`connection refused` 一类传输层错误，对这类错误使用更多次数（如 5–8 次）、更长退避（如 5s 起、指数增长到 60s）的独立重试预算，与其余错误（如 4xx/5xx HTTP 状态码）区分对待，因为 880s 的整体任务预算相对于多等 1-2 分钟网络重试是完全负担得起的。
5. **Terminal-Bench 适配器超时对齐（`benchmarks/terminal_bench/harness9_agent.py`）**：`_RUN_TIMEOUT_SEC=880` 是所有任务统一硬编码，但部分任务自身在 `task.toml` 声明了远高于 900s 的 `agent.timeout_sec`（如 fix-ocaml-gc 的 3600s）。建议适配器读取任务自身声明的超时并取 `min(task 声明值, 一个更宽松的硬上限如 3300s) - 缓冲`，而不是无差别用 880s 兜底——这不是 harness9 内核问题，但影响 pilot 的评测公平性，是评测基础设施层面值得改的一项。

### P2 — 低风险收敛项

6. **kv-store-grpc 类"启动后台服务"场景的 prompt 引导**（可选，缓解而非根治）：`internal/context/builder.go` 的 `DefaultPromptBuilder` 可以补充一条通用建议——"启动长期运行的后台进程时，优先用 `setsid cmd < /dev/null > out.log 2>&1 &` 或将进程组隔离交给外层脚本处理，避免复合命令整体被 `&&` 链接后台化"。这只是缓解手段，**不能替代 P0 的工具层修复**，因为不能假设所有场景下 LLM 都会主动选用这种更安全的模式（本次 kv-store-grpc 两次轨迹里 LLM 用的都是最直觉、最常见的 `nohup ... &` 写法，这恰恰是大多数人类工程师也会写的模式）。
7. **错误消息可观测性**：R2 命中的三个任务里，harness9 进程非零退出前打印的最终一行是"错误: 模型生成失败 (turn 1): ..."，这条信息在 Harbor 的 `result.json` 里被归入笼统的 `RuntimeError`/退出码，不便于批量筛查"这是 TLS 类基础设施故障还是真实任务失败"。建议为 `generateWithRetry` 耗尽后的最终错误加一个机器可读的错误分类前缀（如 `[NETWORK]`/`[AUTH]`/`[UNKNOWN]`），便于评测脚本自动过滤基础设施噪声、不需要每次都靠人工读日志判断。

---

## 5. 不可回归项

9 个 resolved 任务里，`configure-git-webserver`、`pypi-server`、`build-pov-ray` 三个是本次 pilot 中"确认为并发资源争抢噪声、重跑后干净通过"的关键对照组——它们直接证明了以下机制在无干扰环境下工作良好，后续任何改动都不能破坏：

- **路径沙箱 + 自愈闭环**：`configure-git-webserver` 的 Turn 12 中，`write_file` 尝试写入 `/git/server/hooks/post-receive`（超出 `workDir=/app` 边界），`safePath()` 正确拒绝并返回 `Error executing write_file: 路径 '...' 超出工作区范围`（`IsError: true`，未终止循环）；LLM 在下一 turn 立即改用 `bash` heredoc（`cat > ... << 'EOF'`）完成了同一份文件写入，任务最终通过。这是"工具执行失败原样回传给 LLM、触发自动重试"的自愈设计的真实生效案例，**不能因为要修复 kv-store-grpc 的 bash 挂起问题而削弱 bash 工具本身的灵活性**（bash 恰恰是本例中绕过路径沙箱限制的合法退路）。
- **`timeout_secs` 单次调用覆盖机制**：多个 resolved 轨迹里 LLM 主动为耗时较长的 `pip install` / 编译类命令传入更大的 `timeout_secs`（如 kv-store-grpc 轨迹本身在 `pip install grpcio` 时用了 120s），说明"默认超时 + 单次调用可覆盖"的设计对 LLM 是可感知、可利用的——P0 修复方案改造 `runLocal` 的进程管理方式时，必须保持 `effectiveTimeout()` 的钳制逻辑（`timeout_secs` 优先、否则工具级默认、再否则全局默认，且上限 `maxBashTimeout=600s`）完全不变。
- **并发工具执行 + Observation 顺序保证**：`pypi-server`、`build-pov-ray` 的轨迹里都出现过同一 Turn 内 2+ 个工具并发执行（如 `write_file` 与 `bash` 同时启动）且各自独立超时、结果按序写回 history 的模式，未见结果错位或竞态。P0 改造只涉及 `runLocal` 单个工具内部的进程管理，不触碰 `agent_loop.go` 的并发调度层，理应保持这一保证不受影响。
- **`todo_write` 状态机 + Plan 持久化**：所有 9 个 resolved 轨迹和大部分 unresolved 轨迹（包括最终失败的 kv-store-grpc）都展示了连贯的 `pending → in_progress → completed` 状态转换，没有出现状态机校验失败或计划丢失的情况——即使 kv-store-grpc 最终因 bash 挂起而失败，其失败点完全在 P0 修复的目标范围之外，不涉及 Planning 模块本身的缺陷。
- **截断策略（head+tail 保留）**：`build-pov-ray` 编译日志量较大（1921 行轨迹），未发现因 `truncateOutput` 头尾截断丢失关键报错信息导致误判的情况；`sqlite-with-gcov`/`compile-compcert`/`merge-diff-arc-agi-task` 三个 R2 命中的任务甚至没有走到任何工具调用，与截断逻辑完全无关，可排除"截断策略导致这批任务失败"的可能性。

所有 P0/P1/P2 改动均是**新增进程管理健壮性与错误分类能力**，不应削减、也不涉及上述已验证有效的路径沙箱、自愈重试、并发调度、超时覆盖、Planning 状态机、截断策略——这些机制在本次 18 任务 pilot 中经受了真实（非 mock）的 Terminal-Bench 环境考验，应作为后续改动的回归测试基线。

---

## 6. 后续方向：先修 P0，暂不扩大到全量 89 题

**结论：不建议现在扩大到全量 89 题**。18 个任务的样本量已经挖出 1 个可复现、有源码证据的内核级缺陷（kv-store-grpc 的 bash 挂起）和 1 类命中 3 个任务的 P1 基础设施韧性问题——与 SWE-bench 当年"24 实例即可挖干净"的经验一致，样本量本身不是当前的瓶颈。在修复 P0 之前扩大规模，只会让"需要起后台服务"这类任务反复暴露同一个已知缺陷，边际信息量低；而每个任务真实耗时 3–15 分钟 + Docker 环境搭建，本轮 18+10=28 次真实执行合计约 2.5 小时，扩大到 89 题的成本不是可以忽略的。

**建议顺序**：
1. 落地 P0（`internal/tools/bash.go` `runLocal` 改为进程组隔离 + `internal/sandbox/container.go` `realCmdRunner` 同步修复 + 补充 §4 item 2 提到的回归测试）和 P1（`generateWithRetry` 按错误类别区分重试预算）。
2. 排查并修复 compile-compcert / merge-diff-arc-agi-task / sqlite-with-gcov 三个任务命中的 TLS 证书信任问题（这 3 个任务目前从未被真正测试过，网络环境问题不解决，扩大规模也测不出它们的真实能力）。
3. 修复后再评估是否扩大到全量 89 题——彼时的目标应该是"验证 P0/P1 修复是否解决了同类问题、有没有暴露新的根因类型"，而不是单纯为了对标官方 leaderboard 刷分。

**不需要新增工具能力**：本轮暴露的都是既有 `bash` 工具的进程管理健壮性问题、既有重试逻辑的错误分类问题，不存在"agent 想用但没有对应工具"的情况——18 个任务里现有的 bash/read_file/write_file/edit_file 工具集覆盖了包括起后台服务、编译、底层符号调试在内的全部实际操作需求。
