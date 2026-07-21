---
title: "Sandbox：把 Agent 的手脚关进一座悬浮孤岛"
date: 2026-07-13
tags: [harness9, agent, golang, sandbox, docker, isolation]
summary: "harness9 的 Sandbox 模块用一个 Environment 接口把 LocalEnvironment 和 DockerEnvironment 统一成同一套抽象，工具层完全无感知路由。本文拆解为什么选 Docker 而非 gVisor/Firecracker、Container 五状态机怎么设计、Manager 如何并发安全地管理容器生命周期、bash 与文件工具如何分别走 docker exec 和 bind mount 两条路由，以及 --cap-drop all 之后为什么又要加回三个能力。"
---

# Sandbox：把 Agent 的手脚关进一座悬浮孤岛

## 关于 harness9

harness9 是一款 Local-First、轻量级、功能完备、生产可用的通用 Go Agent 框架。

- **官网**：[https://zhangshenao.github.io/harness9/zh/](https://zhangshenao.github.io/harness9/zh/)
- **GitHub**：[https://github.com/ZhangShenao/harness9](https://github.com/ZhangShenao/harness9)

⭐ Star 是对开源工作最直接的支持，欢迎提 Issue 和 PR。

---

## TL;DR

- 抽象 `Environment` 接口：`LocalEnvironment`（在本机进程里跑）和 `DockerEnvironment`（在容器里跑）实现同一套方法，工具代码完全不知道、也不需要知道自己在跟谁打交道
- 之所以选 Docker 而不是 gVisor、Firecracker 这些更"硬核"的方案，是因为 harness9 要防的问题用 namespace + capability 裁剪就够了，而且大部分人电脑上本来就装着 Docker，不用再折腾一套新环境
- `bash` 命令走 `docker exec` 真正进入容器里执行，而 `read_file`/`write_file`/`edit_file` 这几个文件工具则走 bind mount（把宿主机的目录挂载到容器中，两边共享同一份文件）——方式不同，但 Agent 看到的 workDir 是同一个
- `Container` 的生命周期是个五状态的流程：创建中 → 运行中 → 停止中 → 已终止/失败。启动的时候不是问一次"好了没"就完事，而是反复轮询确认容器真的跑起来了；停止的时候不管 `docker stop` 成不成功，都会接着执行 `docker rm` 把容器删掉
- 容器权限先用 `--cap-drop all` 全部收走，再手动加回来 `DAC_OVERRIDE`/`SETUID`/`SETGID` 这三个——这是让 `apt`、`pip` 这类装软件包的工具还能正常干活所必需的最小权限，不是图省事随手放开
- 用 `label=harness9=1` 打标签 + 启动时跑一遍 `ReapOrphans` 扫描，专门用来收拾那种进程被强制杀掉（`kill -9`）之后忘了清理的容器；整套机制默认打开（`SANDBOX_ENABLED` 不设为 false 就是开着的），关掉之后行为和引入 Sandbox 之前一模一样

## 本文你将学到

- 为什么"给 Agent 一个能跑任意命令的 bash 工具"这件事，天生就需要一层容器兜底
- `Environment` 接口是怎么让"本地跑"和"容器里跑"这两种方式在工具层无缝切换的
- Container 状态机具体怎么转换，Manager 又是怎么用锁保证并发操作不会误杀正在运行的容器
- bash 工具和文件工具为什么走两条完全不同的路，却依然能让 Agent 看到一致的文件系统
- `--cap-drop all` 之后加回来的三个权限分别解决什么问题，`pids-limit`、`no-new-privileges`、tmpfs 挂载策略又各自防住了什么攻击手法

---

## YOLO 是把双刃剑

harness9 的 `bash` 工具走的是"YOLO 哲学"：不限制能执行什么命令，判断权全部交给 LLM。工具定义里的注释写得很直接：

```go
// 让 Agent 具备完整的命令行操作能力，是 harness9 "YOLO 哲学"（Trust-the-LLM）的核心：
// 不限制可执行命令的种类，把所有判断与决策权完全交给大模型。
```

这么做带来的好处很明显：Agent 能装依赖、跑测试、改配置、清理进程，基本什么都能干。但代价也很突出——LLM 会犯错、会理解错任务范围，还可能被恶意提示词（prompt injection）诱导去执行破坏性命令。`rm -rf`、`curl | bash`、往 `~/.ssh/authorized_keys` 里写东西，这些操作要是发生在 `LocalEnvironment` 下，跟你自己在终端里手滑敲错命令没什么两样。

虽然项目里已经有 `hooks.DangerHook` 拦截 19 条已知的高危命令模式，还有 `PermissionHook` 做白名单审批，但这两个说到底都是"提前识别出已知的坏东西再拦下来"。真正拦不住的是**那些没见过的、拐弯抹角组合出来的、绕开关键词匹配的破坏路径**——想彻底兜底，靠的不是列举更多危险命令，而是从根本上现在高危操作的影响范围。这正是容器隔离要干的事：不管 LLM 到底生成了什么命令，它的执行结果都被框在一个独立的空间里，宿主机的其他部分碰都碰不到。

![图：LocalEnvironment 与 Sandbox 隔离的风险对比](/blog/sandbox/images/local-vs-sandbox-risk-01.png)


---

## 为什么选择 Docker？

Sandbox 这个赛道上选择挺多的：gVisor 在用户态拦截系统调用，Firecracker 用 microVM 做硬件级隔离，chroot 只是把文件系统的根目录换了个地方。harness9 最后选的是看起来最传统的 Docker，但这不是图省事或者技术保守，而是权衡了三个很实际的问题：

**要防范的风险没那么复杂，用不着上重方案。** Agent 工具调用真正要防的，无非是"误删了文件"、"装了乱七八糟的依赖污染系统"、"脚本失控疯狂 fork 进程"这几类，这些用 namespace（给进程一个独立视角）+ cgroup（限制资源用量）+ capability 裁剪（砍掉不必要的系统权限）就足够罩住了。gVisor 拦截系统调用、Firecracker 上硬件虚拟化，这些是为了应对更极端的场景，比如云厂商要在同一台物理机上给互不信任的多个租户跑代码——对本地跑一个 Agent 会话来说，这属于杀鸡用牛刀。

**装起来麻不麻烦，直接决定这功能有没有人真的会用。** gVisor 得单独装一个 `runsc` 运行时，Firecracker 需要 KVM 支持，还得自己折腾 rootfs 和内核镜像。而 Docker 基本是开发机的标配，只要本地跑着 Docker daemon，`SandboxConfig.Enabled` 一开就能用。harness9 的定位是 Local-First 的轻量框架，不能让"开一个安全特性"变成"先给我装一整套新的虚拟化环境"。

**只用命令行调用，不额外引入 SDK 依赖。** 翻开 `container.go` 就能看到，harness9 没有引入 Docker 官方的 Go SDK，而是直接用 `exec.Command("docker", args...)` 调命令行：

```go
func realCmdRunner(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
```

这个选择跟项目一贯"尽量少引入直接依赖"的做法是一致的——调命令行换来的好处是：编译期不用多依赖任何库，行为跟你自己手敲 `docker run` 完全一样，出问题的时候把参数复制到终端就能复现。`cmdRunner` 被定义成一个函数类型，可以在测试时换成假的实现，不用真的拉起 Docker 环境就能验证状态机转换对不对。

![图：三种隔离技术的粒度与部署成本对比](/blog/sandbox/images/isolation-tech-comparison-02.png)


### Environment 接口封装不同环境

Agent Engine 这一层应该完全不用关心工具到底是在本机进程里跑，还是在容器里跑，之所以能实现这样的解耦，靠的就是 `Environment` 这个接口：

```go
type Environment interface {
	RunBash(ctx context.Context, cmd, workDir string) (string, error)
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte) error
	ID() string
	Close(ctx context.Context) error
}
```

`LocalEnvironment` 用 `exec.Command("bash", "-c", cmd)` 来实现 `RunBash`，`DockerEnvironment` 则用 `docker exec` 实现同样的方法签名。工具层拿到手的只是这个接口类型，`BashTool.env` 字段是 `nil` 就走本地那条路，不是 `nil` 就转去容器里跑——这也是整个 Sandbox 模块里最关键的一个设计决定：**加了 Sandbox 之后，工具对外表现出来的行为一点没变，变的只是背后谁在真正执行**。

```go
if t.env != nil {
	return t.runInSandbox(timeoutCtx, input.Command, timeout)
}
return t.runLocal(timeoutCtx, input.Command, timeout)
```

也就是说，`SANDBOX_ENABLED=false` 的时候，代码走的分支跟没有 Sandbox 之前几乎一样，只多了一次判断，没别的差异。这种向后兼容不是靠外面裹一层开关判断实现的，而是接口本身"给个空值就退回原来的行为"这个语义天然就成立。

![图：Environment 接口统一 Local 与 Docker 两种实现](/blog/sandbox/images/environment-interface-03.png)


---

## Container 状态机

**Sandbox 的生命周期是一个状态机：一个容器从被创建出来到最后销毁，会经过五个状态**，`ContainerState` 这个枚举定义很清晰：

```go
const (
	StatePending    ContainerState = iota // 容器创建中，等待就绪
	StateRunning                          // 容器正常运行，接受工具调用
	StateStopping                         // 容器停止中（docker stop 已发出）
	StateTerminated                       // 容器已停止并移除
	StateFailed                           // 发生不可恢复错误
)
```

`Start` 方法有个关键设计：**反复问几次"好了没"，而不是问一次就当真**。`docker run -d` 拿到容器 ID 并不代表容器已经真正准备好干活了，尤其是要拉取新镜像或者启动比较重的场景时更明显。所以 `Start` 拿到 dockerID 之后，会进入一个每 200 毫秒问一次的轮询循环，反复执行 <code v-pre>docker inspect --format={{.State.Running}}</code>，直到看到返回 `true`，或者等到 `StartTimeout`（默认 30 秒）到点还没成功，就转成 `StateFailed`：

```go
for {
	out, inspectErr := c.run(startCtx, "inspect", "--format={{.State.Running}}", dockerID)
	if inspectErr == nil && out == "true" {
		break
	}
	select {
	case <-startCtx.Done():
		c.setState(StateFailed, fmt.Errorf("等待容器就绪超时（%v）", c.cfg.StartTimeout))
		return c.err
	case <-time.After(200 * time.Millisecond):
	}
}
```

`Stop` 方法则体现了另一种取舍：**`docker stop` 成不成功都不重要，反正接下来都要跑一次 `docker rm`**。

```go
_, _ = c.run(stopCtx, "stop", "-t", "5", dockerID)
_, _ = c.run(stopCtx, "rm", dockerID)
c.setState(StateTerminated, nil)
```

如果容器因为某些原因已经卡死了，`docker stop` 有可能失败或者超时，但资源该回收还是得回收——所以这两条命令的返回值都被直接丢掉了（`_, _ =`），因为不管前面成不成功，最终都会走到 `StateTerminated` 这个状态。这是一种"先把资源收拾干净，不追求每一步都精确"的做法：宁可默默吞掉一次 stop 失败，也不能让容器一直占着资源不放。`Stop` 开头还有个小小的保险：已经是 `Terminated` 或 `Failed` 状态的容器，直接返回 `nil`，避免并发调用时重复清理。

![图：Container 五状态转换图](/blog/sandbox/images/container-state-machine-04.png)


---

## Manager —— 集中管理 Sandbox

`Manager` 是整个 Sandbox 系统的管理器，手里握着一个 `map[string]*Container`，它对外提供的所有方法都得保证并发安全——因为主 Agent 和每一个 Sub-Agent 都有可能同时在创建、销毁各自的容器。

`Create` 的主干很直接：生成一个 UUID，构造出 `Container`，调用 `Start` 启动，启动成功后跑一次可选的 bootstrap 命令，最后才把容器记进 `containers` 这张 map 里：

```go
func (m *Manager) Create(ctx context.Context, workDir string) (Environment, error) {
	id := generateID()
	run := cmdRunner(realCmdRunner)
	// ...（测试注入用的 runnerFactory 分支略）
	c := newContainer(id, workDir, m.cfg, run)
	if err := c.Start(ctx); err != nil {
		return nil, fmt.Errorf("sandbox: 启动容器失败: %w", err)
	}
	env := newDockerEnvironment(c.DockerID(), id, workDir, run)

	// 依赖 bootstrap：容器就绪后、Agent 开始前执行一次初始化命令，
	// 独立的较长超时预算，fail-open（失败只告警，不阻断创建）
	if cmd := strings.TrimSpace(m.cfg.BootstrapCmd); cmd != "" {
		m.runBootstrap(env, cmd, workDir)
	}

	m.mu.Lock()
	m.containers[id] = c
	m.mu.Unlock()
	m.notify()
	return env, nil
}
```

这里真正值得多看一眼的是 `BootstrapCmd`：容器起来之后、Agent 真正开始干活之前，可以让它先跑一条初始化命令（比如 `pip install -e . -q` 装好项目依赖），跑在一个独立的、比单条 bash 命令超时长得多的时间预算里（默认 10 分钟，`SANDBOX_BOOTSTRAP_CMD` / `SANDBOX_BOOTSTRAP_TIMEOUT_SECS` 两个环境变量控制）。这一步是 fail-open 的：装依赖失败不会让 `Create` 报错退出，顶多是 Agent 接下来干活时环境没装全——不阻断，只降级。留这个接缝，是为了以后接官方预装好依赖的镜像（比如 SWE-bench 每个实例自带的仓库镜像）时，配一个镜像加一条命令就能用，不用再让 Sandbox 自己去猜要装什么。

`DestroyAll` 的写法值得多看一眼：它没有直接遍历 map、一个一个同步调用 `Stop`，而是先在加锁的区间里把所有容器都取出来、把 map 清空，锁一放开，再用 `sync.WaitGroup` 并发地去停止它们：

```go
m.mu.Lock()
cs := make([]*Container, 0, len(m.containers))
for _, c := range m.containers {
	cs = append(cs, c)
}
m.containers = make(map[string]*Container)
m.mu.Unlock()

var wg sync.WaitGroup
for _, c := range cs {
	wg.Add(1)
	c := c
	go func() {
		defer wg.Done()
		_ = c.Stop(ctx)
	}()
}
wg.Wait()
```

这么写能避开两个坑：一是不会因为 `docker stop`/`docker rm` 这种慢操作而让锁一直被占着；二是主 Agent 的容器和多个 Sub-Agent 的容器可以同时并发销毁，不用排队一个个来——程序退出时 `defer sandboxMgr.DestroyAll(ctx)` 能跑多快，直接就看这一段。

### 孤儿容器清理机制

正常退出的话，`defer sandboxMgr.DestroyAll(ctx)` 会把所有容器清理干净。但如果进程被 `SIGKILL` 强行杀掉（比如用户手动 `kill -9`，或者系统内存不够被 OOM killer 干掉），`defer` 根本来不及执行，容器就会以 `Running` 状态留在系统里，成了没人管的孤儿。`manager.go` 里专门有一段注释说明这个坑：

```go
// 原实现只清理 status=exited 的容器；进程被 SIGKILL 强杀时 defer 不运行，
// 容器会以 Running 状态残留——持有已删除 tmpDir 的 bind mount，
// 在 macOS Docker Desktop 上会导致 VirtioFS 慢，使后续容器启动超时。
```

这是个真实踩过的坑：残留下来的容器挂载着一个早就已经不存在的临时目录（bind mount 指向的路径没了），但 Docker Desktop 底层维护这个失效挂载的 VirtioFS 层还在那儿空转，把整个虚拟机的文件系统性能拖慢，连带着下一次启动新容器也变得很慢，甚至直接超时。修复办法是每次进程启动的时候先跑一遍 `ReapOrphans`，靠 `label=harness9=1` 这个标签把所有历史遗留的容器筛出来（不管它是什么状态），一律用 `docker rm -f` 强制删掉：

```go
out, err := realCmdRunner(ctx,
	"ps", "-a",
	"--filter", "label=harness9=1",
	"--format", "{{.ID}}",
)
```

但这里有个真正需要小心的地方：**万一同时开着好几个 harness9 进程实例，`ReapOrphans` 绝对不能把别的进程正在用的活跃容器也误杀了**。所以 `ReapOrphans` 会先看看自己这个 `Manager` 手里正管着哪些容器（取 dockerID 的前 12 位短哈希来对比），把这些"自己人"排除掉，只清理真正没人认领的孤儿：

```go
m.mu.RLock()
owned := make(map[string]bool, len(m.containers))
for _, c := range m.containers {
	c.mu.RLock()
	if c.dockerID != "" {
		owned[shortDockerID(c.dockerID)] = true
	}
	c.mu.RUnlock()
}
m.mu.RUnlock()
```

`ListAll` 里还留了一句很明确的提醒：拿锁的顺序必须是先 `Manager.mu`（读锁）、再 `Container.mu`（读锁），绝对不能反过来，不然可能会死锁。这种"锁的顺序说明"在并发代码里花不了几个字，但关键时刻真的能救命。

![图：孤儿容器回收流程](/blog/sandbox/images/orphan-reaping-flow-05.png)


---

## Sandbox 接入 Tool-Calling

Sandbox 接入工具层的方式，并不是简单粗暴地"把所有文件操作也一股脑塞进容器里执行"，而是按操作类型分成了两条完全不同的路。

**bash 命令走 `docker exec`。** 这是唯一真正需要在容器内部跑一个进程的场景：

```go
func (e *DockerEnvironment) RunBash(ctx context.Context, cmd, workDir string) (string, error) {
	out, err := e.run(ctx,
		"exec", "-w", workDir, e.containerID,
		"bash", "-c", cmd,
	)
	if err != nil {
		return fmt.Sprintf("执行报错: %v\n输出:\n%s", err, out), nil
	}
	return out, nil
}
```

**文件读写走的是宿主机这边的 bind mount。** `DockerEnvironment` 的 `ReadFile`/`WriteFile` 压根没调用任何 docker 命令，直接就是普通的 `os.ReadFile`/`os.WriteFile`：

```go
func (e *DockerEnvironment) ReadFile(_ context.Context, path string) ([]byte, error) {
	return os.ReadFile(path)
}
```

这个设计乍一看有点反常识——都叫 Sandbox 了，文件操作为啥不走容器？答案就藏在创建容器时的挂载参数里：

```go
"-v", fmt.Sprintf("%s:%s", c.workDir, c.workDir),
```

简单说，bind mount 就是把宿主机上的 `workDir` 这个文件夹，原样"挂"到容器里同样的路径下——容器里看到的 `/path/to/project` 和宿主机上的 `/path/to/project`，其实是同一份文件，底层存储都是一个东西。既然文件系统本来就是共享的，那直接在宿主机这边读写就能拿到跟容器内部完全一样的结果，没必要再多绕一道，比如用 `docker exec cat` 或者 `docker cp` 去容器里搬。这是个很实在的取舍：`docker exec` 本身要新建一个进程，是有开销的，而文件操作走本地系统调用明显更快；至于两边看到的东西一不一致，靠 bind mount 这层机制就自然保证了，不需要应用层自己再做同步。

`newDockerEnvironment` 的注释把这个设计动机说得很清楚：

```go
func newDockerEnvironment(containerID, id, _ string, run cmdRunner) *DockerEnvironment {
	// workDir 参数不存储：文件读写通过 bind mount 在宿主机侧执行，无需 DockerEnvironment 持有路径。
	return &DockerEnvironment{...}
}
```

三个文件工具（`read_file`/`write_file`/`edit_file`）在 `main.go` 里都是通过各自对应的 `ReadFileWithEnvironment`/`WriteFileWithEnvironment`/`EditFileWithEnvironment` 这几个配置项，统一注入同一个 `sandboxEnv`：

```go
tools.NewReadFileTool(workDir, tools.ReadFileWithEnvironment(sandboxEnv)),
tools.NewWriteFileTool(workDir, tools.WriteFileWithEnvironment(sandboxEnv)),
tools.NewBashTool(workDir, tools.WithEnvironment(sandboxEnv)),
tools.NewEditFileTool(workDir, tools.EditFileWithEnvironment(sandboxEnv)),
```

这四个工具共用同一套沙箱边界校验（`safePath()`），Sandbox 只是替换了它们各自背后真正干活的执行/读写方式，路径安全这块的逻辑完全没受影响。

![图：bash 与文件工具的双路由](/blog/sandbox/images/dual-routing-06.png)


### MainAgent 与 SubAgent 的 Sandbox 是隔离的

Sandbox 的隔离粒度是按 Agent 分的，不是全局共用一个容器。`main.go` 里主 Agent 在启动阶段就调一次 `sandboxMgr.Create`：

```go
sandboxEnv, sandboxErr = sandboxMgr.Create(ctx, workDir)
```

而每一个 Sub-Agent 在 `Runner.Run` 里，会独立地再创建一个属于自己的容器，用完就销毁：

```go
if r.sandboxMgr != nil {
	sandboxEnv, err := r.sandboxMgr.Create(ctx, r.workDir)
	if err != nil {
		return SubAgentResult{}, fmt.Errorf("sandbox: 为子代理创建环境失败: %w", err)
	}
	defer r.sandboxMgr.Destroy(r.baseCtx, sandboxEnv.ID())
	effectiveBaseTools = wrapToolsWithSandbox(r.baseTools, sandboxEnv, r.workDir)
}
```

`wrapToolsWithSandbox` 干的活是把 Sub-Agent 用的这套基础工具重新包一层，塞进它专属的那个 `Environment`：

```go
func wrapToolsWithSandbox(ts []tools.BaseTool, env sandbox.Environment, workDir string) []tools.BaseTool {
	result := make([]tools.BaseTool, len(ts))
	for i, t := range ts {
		switch t.Name() {
		case "bash":
			result[i] = tools.NewBashTool(workDir, tools.WithEnvironment(env))
		// read_file / write_file / edit_file 同理
		default:
			result[i] = t
		}
	}
	return result
}
```

也就是说，如果主 Agent 一口气委派了三个并发的 Sub-Agent，同一时刻其实会有四个各自独立的容器在跑——主 Agent 一个，每个 Sub-Agent 一个。某个 Sub-Agent 里跑的破坏性命令，物理上根本碰不到主 Agent 或者其他 Sub-Agent 的容器。`defer r.sandboxMgr.Destroy(r.baseCtx, sandboxEnv.ID())` 保证了不管 Sub-Agent 任务是成功还是失败，结束了容器立马回收，不会一直占着资源不放。**这样也就实现了 MainAgent 与 SubAgent 的完全隔离。**

---

## 最小化 Sandbox 权限

容器启动时传的那些参数，是整个 Sandbox 安全模型的核心，值得把 `Start` 里 `docker run` 的每一行都拆开看看：

```go
c.run(startCtx,
	"run", "-d",
	"--name", "harness9-"+c.id,
	"--label", "harness9=1",
	"--cap-drop", "all",
	"--cap-add", "DAC_OVERRIDE",
	"--cap-add", "SETUID",
	"--cap-add", "SETGID",
	"--security-opt", "no-new-privileges:true",
	"--pids-limit", fmt.Sprintf("%d", c.cfg.PidsLimit),
	"--cpus", c.cfg.CPUs,
	"--memory", c.cfg.Memory,
	"--tmpfs", "/tmp:size=256m,nosuid,noexec,nodev",
	"-v", fmt.Sprintf("%s:%s", c.workDir, c.workDir),
	c.cfg.Image,
	"sleep", "infinity",
)
```

**`--cap-drop all` 只是个起点，不是终点。** 可以把 Linux capability 理解成把"root 权限"这个大权限拆成了几十个小开关：能不能绑定特权网络端口、能不能读写原始网络包、能不能加载内核模块……每一项都是独立的开关。全部关掉之后，容器里就算是 root 用户，能干的事也跟一个普通用户差不多。但完全不留一个开关会出问题：像 `apt install`、`pip install` 这类装软件包的工具，装的过程中经常需要临时切换文件的属主、或者处理带 setuid 标记的二进制文件（比如 `sudo` 本身，或者某些需要临时提权的动态库钩子）。这些动作依赖三个特定的开关：

- `DAC_OVERRIDE`：绕过常规的文件读写执行权限检查——包管理器解压文件、往系统目录里覆盖写文件的时候要用到
- `SETUID` / `SETGID`：允许进程切换自己的用户/组身份——有些安装脚本（比如 postinst）要以特定用户的身份跑，靠的就是这个

这三个开关加起来，刚好是让 `apt`、`pip` 这类工具能正常干活的**最小权限集合**，不是"反正要留几个索性多留几个"图省事的做法。类似能直接读磁盘设备的 `SYS_ADMIN`、能绑定特权端口的 `NET_BIND_SERVICE`、能加载内核模块的 `SYS_MODULE` 这些风险更高的开关，始终保持关闭状态，不会被打开。

**`--security-opt no-new-privileges:true`** 防的是另一种攻击手法：就算容器里某个二进制文件本身带着 setuid 标记（意味着执行它能临时提权），这个选项也会拦住进程通过 `execve` 去获得比启动时更高的权限。这是第二道防线——就算前面收紧的权限被想办法绕开了，这一层依然拦得住。

**`--pids-limit 256`** 就是专门防 fork bomb（进程炸弹，指一个脚本疯狂地自我复制进程）的。一条经典的 fork bomb，或者一个失控的递归脚本，如果容器不限制进程数，能把宿主机的进程表直接撑爆，导致整台机器卡死；`PidsLimit` 把这个上限硬性定在 256，容器里的进程数一超过这个数字，新进程直接创建失败，破坏范围被死死锁在这一个容器内部。

**`--tmpfs /tmp:size=256m,nosuid,noexec,nodev`** 是专门给 `/tmp` 目录加的一道保险——这个目录天生就是攻击者爱用的跳板。`nosuid` 让这里面就算有 setuid 标记的文件也不生效，`noexec` 直接禁止在这里执行任何二进制文件（很多攻击链的套路就是先把恶意程序写到 `/tmp`，再从那儿执行），`nodev` 禁止在这里创建设备文件。三个选项加在一起，`/tmp` 就变成一个只能存东西、没法拿来搞事情的临时空间。

![图：安全加固的四层防御](/blog/sandbox/images/security-hardening-layers-07.png)


---

## `SANDBOX_ENABLED=false` 时一切照旧

`SandboxConfig.Enabled` 默认读环境变量 `SANDBOX_ENABLED`，只有明确设成 `"false"` 才会关掉：

```go
Enabled: strings.ToLower(os.Getenv("SANDBOX_ENABLED")) != "false",
```

`main.go` 里只有 `sandboxCfg.Enabled` 是真的时候才会去创建 `Manager` 和容器；否则 `sandboxEnv` 就一直是 `nil`，所有工具都走本地那条老路。这个默认值本身也值得说一下：**Sandbox 默认是开着的，不是默认关着**——harness9 把容器隔离当成生产环境里理所当然该有的东西，而不是一个需要用户自己发现、自己动手打开的加分项。

万一容器启动失败了（比如本地根本没装 Docker daemon），`main.go` 会接住这个错误，自动降级：

```go
if sandboxErr != nil {
	log.Print(logfmt.FormatMsg("main", fmt.Sprintf("Sandbox 启动失败，已降级为本地进程模式: %v", sandboxErr)))
	sandboxMgr = nil
	sandboxEnv = nil
}
```

这是一条降级路径：Sandbox 初始化失败不会导致整个程序直接退出，而是退回到本地执行模式，同时把失败原因打印出来方便排查。配合前面说的 `Environment` 接口"传 `nil` 就走本地路径"这个默认语义，"降级"这件事在代码层面其实就是把一个指针留空而已，没有任何额外的特殊分支要处理。

---

## TUI 侧展示 Sandbox 状态

TUI 在状态栏下方会渲染一条 SandboxBar，只有存在活跃的 Sandbox 时才显示出来：

![图：TUI 侧展示 Sandbox 状态](/blog/sandbox/images/tui-sandbox-08.png)

```go
func (m tuiModel) renderSandboxBar() string {
	if len(m.sandboxes) == 0 {
		return ""
	}
	// ...
	for i, info := range m.sandboxes {
		label := "main"
		if i > 0 {
			label = fmt.Sprintf("sub-%d", i)
		}
		// ...
	}
}
```

四种状态对应四种颜色：`Running` 绿色、`Pending` 黄色、`Stopping`/`Terminated` 灰色、`Failed` 红色。这个映射直接复用了 `ContainerState` 这个枚举，TUI 不用理解状态机是怎么转的，只要把状态值映射成颜色就行——展示层和背后的领域逻辑再一次靠接口/枚举分开了，TUI 不需要自己再维护一份重复的状态判断。`Manager.WithUpdateNotify` 把状态变化通过 channel 推给 TUI，`sandboxNotifyCh` 这个通知通道必须在第一次调用 `Create` 之前就注册好，不然启动时的初始状态通知会因为回调还没设置好而丢掉——这是 `main.go` 里专门用注释提醒过的一个时序上的坑。

---

## 结语

回头把这几件事串一遍。Agent 手里那个不设限的 bash 工具，是 YOLO 哲学定的调——相信 LLM，但信任得有兜底。这就引出第一个问题：要不要隔离。答案是要，而且不用隔离得太狠：Docker 的 namespace 加 capability 裁剪，就够挡住"删错文件"、"装乱依赖"、"进程失控"这几类风险，用不着 gVisor、Firecracker 那种给互不信任的多租户准备的重装备。

隔离方式定了，下一个问题是容器怎么活下去。Container 老老实实转五个状态；Start 反复轮询，确认容器真的起来了；Stop 不管前面成不成功，都把资源收拾干净；Manager 用锁保证并发创建销毁不会互相打架；ReapOrphans 专门捡进程被强杀之后落下的孤儿。容器能稳定地生、稳定地死，才轮到下一步：怎么让 Agent 已有的工具无缝接进去。bash 走 docker exec，文件走 bind mount——两条路由方式不同，工具代码却一行没改，靠的是 `Environment` 接口把"在哪跑"这件事从工具逻辑里摘了出去。最后落到权限上：`--cap-drop all` 打底，只留 apt、pip 干活所需的三个开关；`pids-limit` 防 fork bomb；tmpfs 挂载选项把 `/tmp` 变成没法搞事的地方。每一个参数背后都是同一句追问：这个功能到底需不需要这项权限。

这几层做的其实是同一件事：先弄清楚问题的真实边界，再用刚好够用的机制去盖住它——多一分是浪费，少一分是隐患。这个思路在 harness9 里不止用了这一次：`Environment` 接口把领域逻辑和调用方式拆开，`ContainerState` 这个状态机被 TUI 直接拿去当颜色映射表用，`cmdRunner` 定义成函数类型，方便测试时替换掉真实的 docker 命令。单独看都是小决定，连起来看，做的是让每一层只管好自己该管的事，剩下的交给接口去屏蔽。这个思路不只对 Sandbox 有用——任何一个要在"安全"和"能不能正常干活"之间找平衡的系统，大概都得先问同一个问题：到底在防什么，防住它最少需要收紧到什么程度？

如果是你来设计这套权限模型，你会在那三个 `--cap-add` 里再多留一个，还是干脆全部去掉，换成一个只读的静态分析模式？

---
