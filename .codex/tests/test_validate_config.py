#!/usr/bin/env python3
from pathlib import Path
import sys
import tomllib
import unittest

TEST_DIR = Path(__file__).resolve().parent
ROOT = TEST_DIR.parents[1]
sys.path.insert(0, str(TEST_DIR))

import validate_config


class AgentContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        agent_dir = ROOT / ".codex" / "agents"
        cls.agent_toml = {}
        for name in ("harness-enhancer", "harness-researcher", "test-runner"):
            cls.agent_toml[name] = (
                agent_dir / f"{name}.toml"
            ).read_text(encoding="utf-8")

    def validate(self, name: str, transform=None) -> list[str]:
        text = self.agent_toml[name]
        if transform is not None:
            text = transform(text)
        data = tomllib.loads(text)
        return validate_config.validate_agent_data(name, data)

    def append_instructions(self, toml_text: str, addition: str) -> str:
        escaped = addition.replace("\\", "\\\\")
        before, delimiter, after = toml_text.rpartition('"""')
        self.assertTrue(delimiter, "developer_instructions closing delimiter missing")
        return before + escaped + delimiter + after

    def assert_error(self, errors: list[str], marker: str) -> None:
        self.assertTrue(
            any(marker in error for error in errors),
            f"missing {marker!r} in errors: {errors}",
        )

    def test_current_engineering_agents_satisfy_contracts(self) -> None:
        for name in self.agent_toml:
            with self.subTest(name=name):
                self.assertEqual([], self.validate(name))

    def test_researcher_rejects_normalized_seventh_framework(self) -> None:
        def add_seventh(instructions: str) -> str:
            row = "| `cReWaI` | Example | https://example.invalid |\n"
            return instructions.replace(
                "<!-- codex-contract:framework-allowlist:end -->",
                row + "<!-- codex-contract:framework-allowlist:end -->",
            )

        errors = self.validate("harness-researcher", add_seventh)
        self.assert_error(errors, "framework allowlist mismatch")

    def test_researcher_ignores_framework_names_outside_allowlist_section(self) -> None:
        errors = self.validate(
            "harness-researcher",
            lambda text: self.append_instructions(
                text,
                "\n范围外示例（不得调研）：`CrewAI`。\n",
            ),
        )
        self.assertEqual([], errors)

    def test_test_runner_rejects_unix_absolute_working_directory(self) -> None:
        errors = self.validate(
            "test-runner",
            lambda text: self.append_instructions(
                text,
                "\n工作目录：`/home/user/harness9`\n",
            ),
        )
        self.assert_error(errors, "absolute working-directory path")

    def test_test_runner_rejects_windows_absolute_working_directory(self) -> None:
        errors = self.validate(
            "test-runner",
            lambda text: self.append_instructions(
                text,
                "\n工作目录：`C:\\work\\harness9`\n",
            ),
        )
        self.assert_error(errors, "absolute working-directory path")

    def test_test_runner_rejects_windows_unc_working_directory(self) -> None:
        errors = self.validate(
            "test-runner",
            lambda text: self.append_instructions(
                text,
                "\n工作目录：`\\\\server\\share\\harness9`\n",
            ),
        )
        self.assert_error(errors, "absolute working-directory path")

    def test_test_runner_rejects_cd_command(self) -> None:
        def add_cd(instructions: str) -> str:
            block = "\n```bash\ncd ../other\n```\n"
            return instructions.replace(
                "<!-- codex-contract:test-command:end -->",
                "<!-- codex-contract:test-command:end -->" + block,
            )

        errors = self.validate("test-runner", add_cd)
        self.assert_error(errors, "must not use cd")

    def test_test_runner_allows_negated_silent_wording(self) -> None:
        errors = self.validate(
            "test-runner",
            lambda text: self.append_instructions(
                text,
                "\n不要静默执行；应提供进度更新。\n",
            ),
        )
        self.assertEqual([], errors)

    def test_test_runner_requires_exact_execution_command(self) -> None:
        errors = self.validate(
            "test-runner",
            lambda instructions: instructions.replace(
                "go test ./... -v -count=1",
                "go test ./... -v",
                1,
            ),
        )
        self.assert_error(errors, "test command mismatch")

    def test_enhancer_rejects_non_exhaustive_scope_contract(self) -> None:
        errors = self.validate(
            "harness-enhancer",
            lambda instructions: instructions.replace(
                "review_every_inventory_path = true",
                "review_every_inventory_path = false",
            ),
        )
        self.assert_error(errors, "repository scope contract mismatch")

    def test_enhancer_requires_exact_validation_commands(self) -> None:
        errors = self.validate(
            "harness-enhancer",
            lambda instructions: instructions.replace(
                "go vet ./...",
                "go vet ./internal/...",
                1,
            ),
        )
        self.assert_error(errors, "validation commands mismatch")


if __name__ == "__main__":
    unittest.main()
