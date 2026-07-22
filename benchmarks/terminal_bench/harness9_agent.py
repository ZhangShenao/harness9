"""Harbor BaseInstalledAgent 适配器：把 harness9 接入 Terminal-Bench 2.0 评测流程。

install() 把预编译的静态二进制拷进任务环境；run() 把任务指令写入临时文件后
非交互调用该二进制（--prompt-file），避免把多行指令塞进 shell 命令行参数。
"""

import os
import tempfile
from pathlib import Path

from harbor.agents.installed.base import BaseInstalledAgent, with_prompt_template
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

_BINARY_LOCAL_PATH = Path(__file__).parent / "bin" / "harness9"
_BINARY_REMOTE_PATH = "/usr/local/bin/harness9"
_INSTRUCTION_REMOTE_PATH = "/tmp/harness9-instruction.md"
_RUN_LOG_REMOTE_PATH = "/tmp/harness9-run.log"

# 绝对兜底超时，不是主要的超时裁决机制（那是 Harbor 自己按 task.toml 的
# [agent].timeout_sec 算出来的、逐任务不同的值）。Harbor 的 AgentConfig.timeout_sec
# 定义为 float | None，task.toml 并非强制要求声明它——如果某个任务缺失这个字段、
# CLI 也没传 --agent-timeout-multiplier/override，Harbor 外层会以 timeout=None 调用
# asyncio.wait_for，等于完全不超时。这个值只是防止那种边界情况下真的无限挂起，
# 定得足够宽松（远高于目前 pilot 里最长的 3600s 声明值），不应该在正常任务上生效。
_ABSOLUTE_TIMEOUT_SEC = 4 * 60 * 60


class Harness9Agent(BaseInstalledAgent):
    """把 harness9 的预编译二进制接入 Harbor 评测流程。"""

    @staticmethod
    def name() -> str:
        return "harness9"

    async def install(self, environment: BaseEnvironment) -> None:
        if not _BINARY_LOCAL_PATH.exists():
            raise FileNotFoundError(
                f"未找到预编译二进制 {_BINARY_LOCAL_PATH}，"
                "请先执行 GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build 生成"
            )
        # 部分 Terminal-Bench 官方任务镜像（如 alexgshaw/compile-compcert:20251031）
        # 未预装 ca-certificates，/etc/ssl/certs 目录不存在，导致 harness9（Go 静态二进制，
        # 走 crypto/x509 系统信任链）对任何出站 HTTPS（如 OpenRouter）100% 确定性地报
        # "x509: certificate signed by unknown authority"——不是间歇性网络故障，重试无法规避。
        await self.exec_as_root(
            environment,
            command=(
                "DEBIAN_FRONTEND=noninteractive apt-get update -qq "
                "&& DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ca-certificates"
            ),
        )
        await environment.upload_file(
            source_path=_BINARY_LOCAL_PATH, target_path=_BINARY_REMOTE_PATH
        )
        await self.exec_as_root(environment, command=f"chmod +x {_BINARY_REMOTE_PATH}")

    @with_prompt_template
    async def run(
        self, instruction: str, environment: BaseEnvironment, context: AgentContext
    ) -> None:
        with tempfile.NamedTemporaryFile(mode="w", suffix=".md", delete=False) as f:
            f.write(instruction)
            local_instruction_path = f.name

        try:
            await environment.upload_file(
                source_path=local_instruction_path,
                target_path=_INSTRUCTION_REMOTE_PATH,
            )
        finally:
            os.remove(local_instruction_path)

        run_env = {"OPENAI_API_KEY": os.environ["OPENAI_API_KEY"]}
        if "OPENAI_BASE_URL" in os.environ:
            run_env["OPENAI_BASE_URL"] = os.environ["OPENAI_BASE_URL"]
        if "LLM_MODEL" in os.environ:
            run_env["LLM_MODEL"] = os.environ["LLM_MODEL"]

        try:
            # 不在这里设置逐任务精确的 timeout_sec：Harbor 的 Trial._run_agent_phase
            # 已经用 asyncio.wait_for 包住整个 run()，超时取自任务自身 task.toml 的
            # [agent].timeout_sec（每个任务不同，如 compile-compcert=2400、
            # fix-ocaml-gc=3600），而不是所有任务统一的某个硬编码值。之前这里固定写
            # 880 秒，比部分任务真实声明的超时短得多，导致这些任务在完成合法工作
            # （如 compile-compcert 正在编译 Coq/OCaml 工具链）的过程中被本适配器
            # 提前掐断，而不是被 Harbor 自己的、真正符合任务声明的超时掐断。这里传的
            # _ABSOLUTE_TIMEOUT_SEC 只是防止 Harbor 侧超时解析失败（task.toml 缺失该
            # 字段）时无限挂起的绝对兜底，正常情况下 Harbor 自己的超时会先生效。
            await self.exec_as_agent(
                environment,
                command=(
                    f"{_BINARY_REMOTE_PATH} --prompt-file {_INSTRUCTION_REMOTE_PATH} "
                    f"> {_RUN_LOG_REMOTE_PATH} 2>&1"
                ),
                env=run_env,
                timeout_sec=_ABSOLUTE_TIMEOUT_SEC,
            )
        finally:
            # harness9 把 [engine]/[main] 等逐轮执行轨迹写到 stdout/stderr，Harbor
            # 默认不会持久化这部分内容。这里把重定向落盘的日志下载进 logs_dir，
            # 失败任务也能做根因分析。best-effort：下载失败不掩盖真正的执行结果/异常。
            try:
                await environment.download_file(
                    source_path=_RUN_LOG_REMOTE_PATH,
                    target_path=self.logs_dir / "harness9.log",
                )
            except Exception:
                pass
