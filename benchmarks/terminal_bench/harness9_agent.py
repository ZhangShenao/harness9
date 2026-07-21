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

# run() 单次执行超时（秒）：略小于 Terminal-Bench 2.0 task.toml 里统一的
# [agent].timeout_sec=900，为收尾流程留出余量。
_RUN_TIMEOUT_SEC = 880


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

        await self.exec_as_agent(
            environment,
            command=f"{_BINARY_REMOTE_PATH} --prompt-file {_INSTRUCTION_REMOTE_PATH}",
            env=run_env,
            timeout_sec=_RUN_TIMEOUT_SEC,
        )
