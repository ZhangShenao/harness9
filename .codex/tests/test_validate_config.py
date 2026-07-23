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
        for name in (
            "harness-blog-writer",
            "harness-enhancer",
            "harness-researcher",
            "test-runner",
        ):
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

    def test_blog_writer_requires_every_image_contract_key(self) -> None:
        required = {
            "skill": "$imagegen",
            "tool": "image_gen",
            "skill_before_raster_generation": "true",
            "initial_calls_per_distinct_asset": "1",
            "maximum_retry_calls_per_asset": "1",
            "minimum_body_pngs": "6",
            "minimum_cover_pngs": "1",
            "project_image_directory": "website/zh/blog/<slug>/images/",
            "generated_image_source": "$CODEX_HOME/generated_images",
            "visual_inspection": "every_output",
            "visual_inspection_tool": "view_image",
            "retain_final_prompt_in_markdown": "true",
            "verify_project_png_existence": "true",
            "silent_cli_or_api_fallback": "forbidden",
            "prompt_only_fallback": "forbidden",
            "block_if_builtin_unavailable": "true",
        }
        for key, value in required.items():
            with self.subTest(key=key):
                errors = self.validate(
                    "harness-blog-writer",
                    lambda text, line=f"{key} = {value}\n": text.replace(
                        line,
                        "",
                        1,
                    ),
                )
                self.assert_error(errors, "image generation contract mismatch")

    def test_blog_writer_rejects_malformed_image_contract_keys(self) -> None:
        mutations = {
            "renamed initial call key": lambda text: text.replace(
                "initial_calls_per_distinct_asset = 1",
                "initial_call_per_distinct_asset = 1",
                1,
            ),
            "unbounded retry value": lambda text: text.replace(
                "maximum_retry_calls_per_asset = 1",
                "maximum_retry_calls_per_asset = unlimited",
                1,
            ),
        }
        for label, transform in mutations.items():
            with self.subTest(label=label):
                errors = self.validate("harness-blog-writer", transform)
                self.assert_error(errors, "image generation contract mismatch")

    def test_blog_writer_rejects_duplicate_contract_keys(self) -> None:
        duplicates = {
            "same value": "tool = image_gen\n",
            "contradictory value": "tool = external_api\n",
        }
        for label, duplicate in duplicates.items():
            with self.subTest(label=label):
                errors = self.validate(
                    "harness-blog-writer",
                    lambda text, line=duplicate: text.replace(
                        "tool = image_gen\n",
                        line + "tool = image_gen\n",
                        1,
                    ),
                )
                self.assert_error(errors, "duplicate contract assignment")

    def test_blog_writer_requires_workspace_write_sandbox(self) -> None:
        errors = self.validate(
            "harness-blog-writer",
            lambda text: text.replace(
                'sandbox_mode = "workspace-write"',
                'sandbox_mode = "read-only"',
                1,
            ),
        )
        self.assert_error(errors, "sandbox_mode must be workspace-write")

    def test_blog_writer_must_inherit_parent_model(self) -> None:
        errors = self.validate(
            "harness-blog-writer",
            lambda text: text.replace(
                'sandbox_mode = "workspace-write"',
                'model = "gpt-5.6"\nsandbox_mode = "workspace-write"',
                1,
            ),
        )
        self.assert_error(errors, "must inherit the parent model")

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

    def test_test_runner_requires_no_write_no_fix_contract(self) -> None:
        mutations = {
            "remove no_writes": lambda text: text.replace(
                "no_writes = true\n",
                "",
            ),
            "reverse no_writes": lambda text: text.replace(
                "no_writes = true",
                "no_writes = false",
            ),
            "remove no_fixes": lambda text: text.replace(
                "no_fixes = true\n",
                "",
            ),
            "reverse no_fixes": lambda text: text.replace(
                "no_fixes = true",
                "no_fixes = false",
            ),
        }
        for label, transform in mutations.items():
            with self.subTest(label=label):
                errors = self.validate("test-runner", transform)
                self.assert_error(errors, "communication contract mismatch")

    def test_enhancer_requires_workspace_write_sandbox(self) -> None:
        errors = self.validate(
            "harness-enhancer",
            lambda text: text.replace(
                'sandbox_mode = "workspace-write"',
                'sandbox_mode = "read-only"',
            ),
        )
        self.assert_error(errors, "sandbox_mode must be workspace-write")

    def test_researcher_requires_workspace_write_sandbox(self) -> None:
        errors = self.validate(
            "harness-researcher",
            lambda text: text.replace(
                'sandbox_mode = "workspace-write"',
                'sandbox_mode = "read-only"',
            ),
        )
        self.assert_error(errors, "sandbox_mode must be workspace-write")

    def test_researcher_requires_live_web_search(self) -> None:
        errors = self.validate(
            "harness-researcher",
            lambda text: text.replace(
                'web_search = "live"',
                'web_search = "cached"',
            ),
        )
        self.assert_error(errors, "web_search must be live")

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
