#!/usr/bin/env python3
from pathlib import Path
import sys
import tomllib
import unittest

TEST_DIR = Path(__file__).resolve().parent
ROOT = TEST_DIR.parents[1]
sys.path.insert(0, str(TEST_DIR))

import validate_config


KNOWLEDGE_ROOT = "/Users/zsa/Desktop/workspace/harness9/知识库日报"
KNOWLEDGE_AGENTS = ("collector", "analyzer", "organizer")
ENGINEERING_AGENTS = (
    "harness-blog-writer",
    "harness-enhancer",
    "harness-researcher",
    "test-runner",
)


class AgentContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        agent_dir = ROOT / ".codex" / "agents"
        cls.agent_toml = {}
        for name in ENGINEERING_AGENTS + KNOWLEDGE_AGENTS:
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

    def knowledge_data(self, name: str) -> dict:
        return tomllib.loads(self.agent_toml[name])

    def test_current_engineering_agents_satisfy_contracts(self) -> None:
        for name in ENGINEERING_AGENTS:
            with self.subTest(name=name):
                self.assertEqual([], self.validate(name))

    def test_current_knowledge_agents_exist_and_satisfy_contracts(self) -> None:
        for name in KNOWLEDGE_AGENTS:
            with self.subTest(name=name):
                self.assertEqual([], self.validate(name))

    def test_knowledge_agents_require_workspace_write_sandbox(self) -> None:
        for name in KNOWLEDGE_AGENTS:
            with self.subTest(name=name):
                data = self.knowledge_data(name)
                data["sandbox_mode"] = "read-only"
                errors = validate_config.validate_agent_data(name, data)
                self.assert_error(
                    errors,
                    "sandbox_mode must be workspace-write",
                )

    def test_knowledge_agents_require_exact_writable_root(self) -> None:
        for name in KNOWLEDGE_AGENTS:
            with self.subTest(name=name):
                data = self.knowledge_data(name)
                data["sandbox_workspace_write"]["writable_roots"] = [
                    KNOWLEDGE_ROOT,
                    "/tmp",
                ]
                errors = validate_config.validate_agent_data(name, data)
                self.assert_error(errors, "writable_roots mismatch")

    def test_knowledge_agents_forbid_shell_network_access(self) -> None:
        for name in KNOWLEDGE_AGENTS:
            with self.subTest(name=name):
                data = self.knowledge_data(name)
                data["sandbox_workspace_write"]["network_access"] = True
                errors = validate_config.validate_agent_data(name, data)
                self.assert_error(errors, "network_access must be false")

    def test_knowledge_agents_must_inherit_parent_model(self) -> None:
        for name in KNOWLEDGE_AGENTS:
            with self.subTest(name=name):
                data = self.knowledge_data(name)
                data["model"] = "gpt-5.6"
                errors = validate_config.validate_agent_data(name, data)
                self.assert_error(errors, "must inherit the parent model")

    def test_collector_requires_live_codex_web_research(self) -> None:
        mutations = {
            "missing web_search": lambda data: data.pop("web_search"),
            "cached web_search": lambda data: data.__setitem__(
                "web_search",
                "cached",
            ),
        }
        for label, mutate in mutations.items():
            with self.subTest(label=label):
                data = self.knowledge_data("collector")
                mutate(data)
                errors = validate_config.validate_agent_data(
                    "collector",
                    data,
                )
                self.assert_error(errors, "web_search must be live")

    def test_analyzer_requires_live_codex_web_research(self) -> None:
        mutations = {
            "missing web_search": lambda data: data.pop("web_search"),
            "cached web_search": lambda data: data.__setitem__(
                "web_search",
                "cached",
            ),
        }
        for label, mutate in mutations.items():
            with self.subTest(label=label):
                data = self.knowledge_data("analyzer")
                mutate(data)
                errors = validate_config.validate_agent_data(
                    "analyzer",
                    data,
                )
                self.assert_error(errors, "web_search must be live")

    def test_organizer_forbids_web_search_configuration(self) -> None:
        data = self.knowledge_data("organizer")
        data["web_search"] = "live"
        errors = validate_config.validate_agent_data("organizer", data)
        self.assert_error(errors, "web_search must be absent")

    def test_collector_rejects_knowledge_policy_mutations(self) -> None:
        mutations = {
            "write outside raw": (
                "write_scope = raw_only",
                "write_scope = knowledge_root",
            ),
            "shell networking": (
                "shell_networking = forbidden",
                "shell_networking = allowed",
            ),
            "extra source": (
                "source_allowlist = github_trending,hacker_news,"
                "anthropic_engineering,langchain_blog",
                "source_allowlist = github_trending,hacker_news,"
                "anthropic_engineering,langchain_blog,reddit",
            ),
            "wrong GitHub threshold": (
                "github_stars = >100",
                "github_stars = >=100",
            ),
            "wrong blog window": (
                "langchain_final_window_days = 7",
                "langchain_final_window_days = 30",
            ),
            "missing schema field": (
                "required_fields = title,url,source,popularity,summary,"
                "collected_at",
                "required_fields = title,url,source,popularity,summary",
            ),
            "allow fabrication": (
                "no_fabrication = true",
                "no_fabrication = false",
            ),
        }
        for label, (old, new) in mutations.items():
            with self.subTest(label=label):
                data = self.knowledge_data("collector")
                data["developer_instructions"] = data[
                    "developer_instructions"
                ].replace(old, new, 1)
                errors = validate_config.validate_agent_data(
                    "collector",
                    data,
                )
                self.assert_error(errors, "knowledge policy mismatch")

    def test_analyzer_rejects_knowledge_policy_mutations(self) -> None:
        mutations = {
            "write outside analysis": (
                "write_scope = analysis_only",
                "write_scope = knowledge_root",
            ),
            "unconditional fetch": (
                "web_research = only_when_source_insufficient",
                "web_research = unconditional",
            ),
            "weaken fetch condition": (
                "fetch_condition = summary_under_20_chars_or_missing_"
                "technical_detail",
                "fetch_condition = optional",
            ),
            "drop collected_at preservation": (
                "preserve_collected_at = exact",
                "preserve_collected_at = optional",
            ),
            "change scoring range": (
                "importance_score = integer_1_to_10",
                "importance_score = integer_0_to_100",
            ),
            "drop output field": (
                "required_added_fields = highlights,importance_score,"
                "importance_label,suggested_tags,deep_summary,analyzed_at,"
                "raw_files",
                "required_added_fields = highlights,importance_score,"
                "importance_label,suggested_tags,deep_summary,analyzed_at",
            ),
            "allow fabrication": (
                "no_fabrication = true",
                "no_fabrication = false",
            ),
        }
        for label, (old, new) in mutations.items():
            with self.subTest(label=label):
                data = self.knowledge_data("analyzer")
                data["developer_instructions"] = data[
                    "developer_instructions"
                ].replace(old, new, 1)
                errors = validate_config.validate_agent_data(
                    "analyzer",
                    data,
                )
                self.assert_error(errors, "knowledge policy mismatch")

    def test_organizer_rejects_knowledge_policy_mutations(self) -> None:
        mutations = {
            "write outside articles": (
                "write_scope = articles_only",
                "write_scope = knowledge_root",
            ),
            "weaken dedup": (
                "dedup_key = normalized_title_or_source_url",
                "dedup_key = normalized_title",
            ),
            "overwrite article": (
                "existing_name = append_v2_v3_without_overwrite",
                "existing_name = overwrite",
            ),
            "add frontmatter": (
                "frontmatter = forbidden",
                "frontmatter = required",
            ),
            "hard-code cleanup day": (
                "cleanup_command = ./.codex/scripts/"
                "cleanup-knowledge-day.sh YYYYMMDD",
                "cleanup_command = ./.codex/scripts/"
                "cleanup-knowledge-day.sh 20260723",
            ),
            "cleanup before article": (
                "cleanup_order = final_article_exists_then_guarded_cleanup",
                "cleanup_order = cleanup_then_write_article",
            ),
            "allow direct rm": (
                "direct_rm = forbidden",
                "direct_rm = allowed",
            ),
            "drop input field": (
                "required_input_fields = title,url,source,popularity,"
                "summary,collected_at,highlights,importance_score,"
                "importance_label,suggested_tags,deep_summary,analyzed_at,"
                "raw_files",
                "required_input_fields = title,url,source,popularity,"
                "summary,collected_at,highlights,importance_score,"
                "importance_label,suggested_tags,deep_summary,analyzed_at",
            ),
        }
        for label, (old, new) in mutations.items():
            with self.subTest(label=label):
                data = self.knowledge_data("organizer")
                data["developer_instructions"] = data[
                    "developer_instructions"
                ].replace(old, new, 1)
                errors = validate_config.validate_agent_data(
                    "organizer",
                    data,
                )
                self.assert_error(errors, "knowledge policy mismatch")

    def test_organizer_rejects_fenced_cleanup_bypasses(self) -> None:
        bypasses = {
            "empty quote obfuscation": (
                "zsh",
                f'r\'\'m -rf "{KNOWLEDGE_ROOT}/raw/20260723"',
            ),
            "absolute rm": (
                "sh",
                f'/bin/rm -rf "{KNOWLEDGE_ROOT}/raw/20260723"',
            ),
            "arbitrary absolute rm": (
                "bash",
                f'/usr/local/bin/rm -rf "{KNOWLEDGE_ROOT}/raw/20260723"',
            ),
            "double quote obfuscation": (
                "shell",
                f'r""m -rf "{KNOWLEDGE_ROOT}/raw/20260723"',
            ),
            "sudo wrapper": (
                "shell",
                f'sudo /usr/bin/rm -rf "{KNOWLEDGE_ROOT}/raw/20260723"',
            ),
            "command wrapper": (
                "bash",
                f'command rm -r "{KNOWLEDGE_ROOT}/analysis/20260723"',
            ),
            "env wrapper": (
                "",
                f'env rm -rf "{KNOWLEDGE_ROOT}/raw/20260723"',
            ),
            "busybox wrapper": (
                "zsh",
                f'busybox rm -rf "{KNOWLEDGE_ROOT}/raw/20260723"',
            ),
            "xargs wrapper": (
                "bash",
                f'printf "%s" target | xargs rm -rf',
            ),
            "find delete": (
                "bash",
                f'find "{KNOWLEDGE_ROOT}/raw/20260723" -delete',
            ),
            "unlink": (
                "sh",
                f'unlink "{KNOWLEDGE_ROOT}/raw/20260723/item.json"',
            ),
            "rmdir": (
                "shell",
                f'rmdir "{KNOWLEDGE_ROOT}/analysis/20260723"',
            ),
            "python os.remove": (
                "",
                "python3 -c 'import os; os.remove(\"target\")'",
            ),
            "python shutil.rmtree": (
                "zsh",
                "python3 -c 'import shutil; shutil.rmtree(\"target\")'",
            ),
            "python pathlib unlink": (
                "bash",
                "python3 -c 'from pathlib import Path; "
                "Path(\"target\").unlink()'",
            ),
            "typed python os.remove": (
                "python",
                "import os\nos.remove(\"target\")",
            ),
            "typed python shutil.rmtree": (
                "python3",
                "import shutil\nshutil.rmtree(\"target\")",
            ),
            "typed python pathlib unlink": (
                "py",
                "from pathlib import Path\nPath(\"target\").unlink()",
            ),
            "alternate cleanup script": (
                "sh",
                "./scripts/purge-knowledge-day.sh 20260723",
            ),
            "unknown alternate script": (
                "",
                "./scripts/do-maintenance.sh 20260723",
            ),
            "hard-coded guarded cleanup day": (
                "bash",
                "./.codex/scripts/cleanup-knowledge-day.sh 20260723",
            ),
            "guarded cleanup chained with rm": (
                "bash",
                "./.codex/scripts/cleanup-knowledge-day.sh YYYYMMDD"
                f' && rm -rf "{KNOWLEDGE_ROOT}/raw/20260723"',
            ),
        }
        for label, (language, command) in bypasses.items():
            with self.subTest(label=label):
                data = self.knowledge_data("organizer")
                data["developer_instructions"] += (
                    f"\n```{language}\n{command}\n```\n"
                )
                errors = validate_config.validate_agent_data(
                    "organizer",
                    data,
                )
                self.assert_error(
                    errors,
                    "unsafe or alternative cleanup command",
                )

    def test_organizer_rejects_inline_cleanup_bypasses(self) -> None:
        commands = (
            f'r\'\'m -rf "{KNOWLEDGE_ROOT}/raw/20260723"',
            f'/bin/rm -rf "{KNOWLEDGE_ROOT}/raw/20260723"',
            f'find "{KNOWLEDGE_ROOT}/raw/20260723" -delete',
            f'unlink "{KNOWLEDGE_ROOT}/raw/20260723/item.json"',
            f'rmdir "{KNOWLEDGE_ROOT}/analysis/20260723"',
            "python3 -c 'import os; os.remove(\"target\")'",
            "python3 -c 'import shutil; shutil.rmtree(\"target\")'",
            "python3 -c 'from pathlib import Path; Path(\"target\").unlink()'",
            "./scripts/cleanup-knowledge-day.sh 20260723",
            "./scripts/do-maintenance.sh 20260723",
        )
        for command in commands:
            with self.subTest(command=command):
                data = self.knowledge_data("organizer")
                data["developer_instructions"] += (
                    f"\n文章完成后运行 `{command}`。\n"
                )
                errors = validate_config.validate_agent_data(
                    "organizer",
                    data,
                )
                self.assert_error(
                    errors,
                    "unsafe or alternative cleanup command",
                )

    def test_organizer_allows_only_exact_guarded_cleanup_examples(self) -> None:
        exact = "./.codex/scripts/cleanup-knowledge-day.sh YYYYMMDD"
        additions = (
            f"\n再次强调，只能调用 `{exact}`。\n",
            f"\n```\n{exact}\n```\n",
            f"\n```zsh\n{exact}\n```\n",
        )
        for addition in additions:
            with self.subTest(addition=addition):
                data = self.knowledge_data("organizer")
                data["developer_instructions"] += addition
                self.assertEqual(
                    [],
                    validate_config.validate_agent_data("organizer", data),
                )

    def test_knowledge_agents_reject_operative_policy_contradictions(
        self,
    ) -> None:
        mutations = {
            "collector analysis write": (
                "collector",
                "\n将采集结果写入 "
                f"`{KNOWLEDGE_ROOT}/analysis/{{YYYYMMDD}}/collector.json`。\n",
                "operative write boundary contradiction",
            ),
            "collector articles write": (
                "collector",
                "\n另存一份到 "
                f"`{KNOWLEDGE_ROOT}/articles/collector.json`。\n",
                "operative write boundary contradiction",
            ),
            "collector fenced write instruction": (
                "collector",
                "\n```text\n将结果写入 "
                f"`{KNOWLEDGE_ROOT}/analysis/{{YYYYMMDD}}/collector.json`。"
                "\n```\n",
                "operative write boundary contradiction",
            ),
            "analyzer unconditional fetch": (
                "analyzer",
                "\n对所有条目无条件回源抓取。\n",
                "conditional fetch contradiction",
            ),
            "analyzer fetch every URL": (
                "analyzer",
                "\n无论本地摘要是否充分，都访问每条 URL 获取网页正文。\n",
                "conditional fetch contradiction",
            ),
            "analyzer fenced unconditional fetch": (
                "analyzer",
                "\n```markdown\n对所有条目无条件回源抓取。\n```\n",
                "conditional fetch contradiction",
            ),
            "organizer cleanup first": (
                "organizer",
                "\n先清理，再写文章。\n",
                "cleanup order contradiction",
            ),
            "organizer cleanup before create": (
                "organizer",
                "\n清理中间数据后再创建最终文章。\n",
                "cleanup order contradiction",
            ),
            "organizer fenced cleanup first": (
                "organizer",
                "\n```text\n先清理，再写文章。\n```\n",
                "cleanup order contradiction",
            ),
            "organizer alternate cleanup prose": (
                "organizer",
                "\n也可以使用备用清理命令完成中间数据清理。\n",
                "alternative cleanup policy contradiction",
            ),
        }
        for label, (name, addition, marker) in mutations.items():
            with self.subTest(label=label):
                data = self.knowledge_data(name)
                data["developer_instructions"] += addition
                errors = validate_config.validate_agent_data(name, data)
                self.assert_error(errors, marker)

    def test_knowledge_agents_allow_harmless_negative_policy_text(self) -> None:
        additions = {
            "collector": (
                "\n不得将结果写入 "
                f"`{KNOWLEDGE_ROOT}/analysis/{{YYYYMMDD}}/collector.json`；"
                "没有要求另存到 "
                f"`{KNOWLEDGE_ROOT}/articles/collector.json`。\n"
            ),
            "analyzer": (
                "\n不得对所有条目无条件回源；"
                "无需访问每条 URL 获取网页正文。\n"
            ),
            "organizer": (
                "\n不得先清理再写文章；不要运行 "
                f"`/bin/rm -rf \"{KNOWLEDGE_ROOT}/raw/20260723\"`，"
                "也不能执行 "
                f"`find \"{KNOWLEDGE_ROOT}/analysis/20260723\" -delete`；"
                "不允许使用备用清理命令。\n"
            ),
        }
        for name, addition in additions.items():
            with self.subTest(name=name):
                data = self.knowledge_data(name)
                data["developer_instructions"] += addition
                self.assertEqual(
                    [],
                    validate_config.validate_agent_data(name, data),
                )

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
