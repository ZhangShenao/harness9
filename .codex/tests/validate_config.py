#!/usr/bin/env python3
from pathlib import Path
import re
import sys
import tomllib

ROOT = Path(__file__).resolve().parents[2]
CONFIG = ROOT / ".codex" / "config.toml"
AGENT_DIR = ROOT / ".codex" / "agents"
EXPECTED = {
    "harness-blog-writer",
    "harness-enhancer",
    "harness-researcher",
    "test-runner",
    "collector",
    "analyzer",
    "organizer",
}
KNOWLEDGE_ROOT = "/Users/zsa/Desktop/workspace/harness9/知识库日报"
FRAMEWORK_ALLOWLIST = frozenset(
    {
        "deepagents",
        "openharness",
        "opencode",
        "openclaw",
        "hermesagent",
        "claude agent sdk",
    }
)
ENHANCER_INVENTORY_COMMAND = (
    "rg --files -g '*.go' -g '!**/*_test.go' -g '!vendor/**'"
)
ENHANCER_VALIDATION_COMMANDS = (
    "go build ./...",
    "go vet ./...",
    "go test ./...",
    "gofmt -l .",
)
TEST_RUNNER_COMMAND = "go test ./... -v -count=1"
BLOG_IMAGE_GENERATION_CONTRACT = {
    "skill": "$imagegen",
    "tool": "image_gen",
    "skill_before_raster_generation": "true",
    "calls_per_distinct_asset": "1",
    "minimum_body_pngs": "6",
    "minimum_cover_pngs": "1",
    "project_image_directory": "website/zh/blog/<slug>/images/",
    "generated_image_source": "$CODEX_HOME/generated_images",
    "visual_inspection": "every_output",
    "visual_inspection_tool": "view_image",
    "targeted_retries_per_image": "1",
    "retain_final_prompt_in_markdown": "true",
    "verify_project_png_existence": "true",
    "silent_cli_or_api_fallback": "forbidden",
    "prompt_only_fallback": "forbidden",
    "block_if_builtin_unavailable": "true",
}
UNIX_ABSOLUTE_PATH = re.compile(
    r"(?<![A-Za-z0-9_.:/\\])/"
    r"[A-Za-z0-9_~.-]+(?:/[A-Za-z0-9_~.-]+)*"
)
WINDOWS_DRIVE_PATH = re.compile(
    r"(?i)(?<![A-Za-z0-9])"
    r"[a-z]:[\\/](?:[^\\/\s`\"'<>]+[\\/])*[^\\/\s`\"'<>]*"
)
WINDOWS_UNC_PATH = re.compile(
    r"(?<!\\)\\\\[^\\\s`\"'<>]+\\[^\\\s`\"'<>]+"
)


def load_toml(path: Path, errors: list[str]) -> dict:
    try:
        with path.open("rb") as handle:
            return tomllib.load(handle)
    except (OSError, tomllib.TOMLDecodeError) as exc:
        errors.append(f"{path}: {exc}")
        return {}


def get_table(
    data: dict,
    key: str,
    label: str,
    errors: list[str],
) -> dict:
    value = data.get(key, {})
    if not isinstance(value, dict):
        errors.append(f"{label} must be a table")
        return {}
    return value


def contract_section(
    instructions: str,
    contract: str,
    label: str,
    errors: list[str],
) -> str | None:
    start = f"<!-- codex-contract:{contract}:start -->"
    end = f"<!-- codex-contract:{contract}:end -->"
    if instructions.count(start) != 1 or instructions.count(end) != 1:
        errors.append(f"{label}: malformed {contract} contract section")
        return None
    before, separator, remainder = instructions.partition(start)
    section, end_separator, after = remainder.partition(end)
    if not separator or not end_separator or end in before or start in after:
        errors.append(f"{label}: malformed {contract} contract section")
        return None
    return section.strip()


def contract_assignments(section: str) -> dict[str, str]:
    assignments = {}
    for line in section.splitlines():
        key, separator, value = line.partition("=")
        if not separator:
            continue
        assignments[key.strip()] = value.strip()
    return assignments


def shell_blocks(text: str) -> list[str]:
    return re.findall(
        r"```(?:bash|sh|shell)\s*\n(.*?)```",
        text,
        flags=re.IGNORECASE | re.DOTALL,
    )


def shell_commands(text: str) -> tuple[str, ...]:
    commands = []
    for block in shell_blocks(text):
        for line in block.splitlines():
            command = line.strip()
            if command and not command.startswith("#"):
                commands.append(command)
    return tuple(commands)


def normalize_framework_name(value: str) -> str:
    without_markup = re.sub(r"[`*_]", "", value)
    return " ".join(without_markup.split()).casefold()


def framework_rows(section: str) -> list[str]:
    frameworks = []
    for line in section.splitlines():
        stripped = line.strip()
        if not stripped.startswith("|") or not stripped.endswith("|"):
            continue
        cells = [cell.strip() for cell in stripped.strip("|").split("|")]
        if not cells:
            continue
        framework = normalize_framework_name(cells[0])
        if framework == "框架" or not framework:
            continue
        if set(framework) <= {"-", ":"}:
            continue
        frameworks.append(framework)
    return frameworks


def has_absolute_working_directory_path(instructions: str) -> bool:
    return any(
        pattern.search(instructions)
        for pattern in (
            UNIX_ABSOLUTE_PATH,
            WINDOWS_DRIVE_PATH,
            WINDOWS_UNC_PATH,
        )
    )


def has_cd_shell_command(instructions: str) -> bool:
    for block in shell_blocks(instructions):
        for line in block.splitlines():
            command = line.strip()
            if re.match(r"^(?:\$\s*)?cd(?:\s|$)", command, re.IGNORECASE):
                return True
    return False


def validate_enhancer(instructions: str) -> list[str]:
    errors = []
    scope = contract_section(
        instructions,
        "repository-scope",
        "harness-enhancer",
        errors,
    )
    if scope is not None:
        expected_scope = {
            "inventory_command": ENHANCER_INVENTORY_COMMAND,
            "review_every_inventory_path": "true",
        }
        if contract_assignments(scope) != expected_scope:
            errors.append(
                "harness-enhancer: repository scope contract mismatch"
            )

    validation = contract_section(
        instructions,
        "validation-commands",
        "harness-enhancer",
        errors,
    )
    if (
        validation is not None
        and shell_commands(validation) != ENHANCER_VALIDATION_COMMANDS
    ):
        errors.append("harness-enhancer: validation commands mismatch")

    for block in shell_blocks(instructions):
        if re.search(r"go test ./\.\.\.[^\n]*\|", block):
            errors.append(
                "harness-enhancer: go test pipeline masks the test exit status"
            )
            break
    return errors


def validate_researcher(instructions: str) -> list[str]:
    errors = []
    allowlist = contract_section(
        instructions,
        "framework-allowlist",
        "harness-researcher",
        errors,
    )
    if allowlist is not None:
        frameworks = framework_rows(allowlist)
        if (
            len(frameworks) != len(FRAMEWORK_ALLOWLIST)
            or frozenset(frameworks) != FRAMEWORK_ALLOWLIST
        ):
            errors.append("harness-researcher: framework allowlist mismatch")

    policy = contract_section(
        instructions,
        "research-policy",
        "harness-researcher",
        errors,
    )
    if policy is not None:
        expected_policy = {
            "context7_ids_must_match_allowlist": "true",
            "live_source_verification": "true",
            "report_directory": "docs/技术调研/",
            "authoritative_source_policy": "single",
            "allow_anthropic_research": "true",
            "allow_academic_sources": "true",
            "http_status": "only_if_exposed",
        }
        if contract_assignments(policy) != expected_policy:
            errors.append("harness-researcher: research policy mismatch")
    return errors


def validate_test_runner(instructions: str) -> list[str]:
    errors = []
    command_section = contract_section(
        instructions,
        "test-command",
        "test-runner",
        errors,
    )
    if (
        command_section is not None
        and shell_commands(command_section) != (TEST_RUNNER_COMMAND,)
    ):
        errors.append("test-runner: test command mismatch")

    communication = contract_section(
        instructions,
        "communication",
        "test-runner",
        errors,
    )
    if communication is not None:
        expected_communication = {
            "progress_updates": "required",
            "final_report": "required",
            "no_writes": "true",
            "no_fixes": "true",
        }
        if contract_assignments(communication) != expected_communication:
            errors.append("test-runner: communication contract mismatch")

    if has_cd_shell_command(instructions):
        errors.append("test-runner: must not use cd shell commands")
    if has_absolute_working_directory_path(instructions):
        errors.append("test-runner: absolute working-directory path forbidden")
    return errors


def validate_blog_writer(instructions: str) -> list[str]:
    errors = []
    image_generation = contract_section(
        instructions,
        "image-generation",
        "harness-blog-writer",
        errors,
    )
    if (
        image_generation is not None
        and contract_assignments(image_generation)
        != BLOG_IMAGE_GENERATION_CONTRACT
    ):
        errors.append(
            "harness-blog-writer: image generation contract mismatch"
        )
    return errors


def validate_agent_data(name: str, data: dict) -> list[str]:
    errors = []
    for field in ("name", "description", "developer_instructions"):
        if not isinstance(data.get(field), str) or not data[field].strip():
            errors.append(f"{name}: missing non-empty {field}")
    if data.get("name") != name:
        errors.append(f"{name}: name field mismatch")
    if name == "test-runner":
        if data.get("model") != "gpt-5.6-luna":
            errors.append("test-runner: model must be gpt-5.6-luna")
    elif "model" in data:
        errors.append(f"{name}: must inherit the parent model")

    instructions = data.get("developer_instructions", "")
    if not isinstance(instructions, str):
        return errors
    if name == "harness-enhancer":
        if data.get("sandbox_mode") != "workspace-write":
            errors.append(
                "harness-enhancer: sandbox_mode must be workspace-write"
            )
        errors.extend(validate_enhancer(instructions))
    if name == "harness-researcher":
        if data.get("sandbox_mode") != "workspace-write":
            errors.append(
                "harness-researcher: sandbox_mode must be workspace-write"
            )
        if data.get("web_search") != "live":
            errors.append("harness-researcher: web_search must be live")
        errors.extend(validate_researcher(instructions))
    if name == "test-runner":
        if data.get("sandbox_mode") != "read-only":
            errors.append("test-runner: sandbox_mode must be read-only")
        errors.extend(validate_test_runner(instructions))
    if name == "harness-blog-writer":
        if data.get("sandbox_mode") != "workspace-write":
            errors.append(
                "harness-blog-writer: sandbox_mode must be workspace-write"
            )
        errors.extend(validate_blog_writer(instructions))
    if name in {"collector", "analyzer", "organizer"}:
        workspace_write = get_table(
            data,
            "sandbox_workspace_write",
            f"{name}: sandbox_workspace_write",
            errors,
        )
        roots = workspace_write.get("writable_roots", [])
        if roots != [KNOWLEDGE_ROOT]:
            errors.append(f"{name}: writable_roots mismatch")
        if KNOWLEDGE_ROOT not in instructions:
            errors.append(f"{name}: missing knowledge root")
    if name == "collector" and "/raw/" not in instructions:
        errors.append("collector: missing raw-only boundary")
    if name == "analyzer" and "/analysis/" not in instructions:
        errors.append("analyzer: missing analysis-only boundary")
    if name == "organizer":
        if "./.codex/scripts/cleanup-knowledge-day.sh" not in instructions:
            errors.append("organizer: missing guarded cleanup command")
        if "禁止直接执行 `rm -rf`" not in instructions:
            errors.append("organizer: missing direct-delete prohibition")
    return errors


def validate_repository(
    config_path: Path = CONFIG,
    agent_dir: Path = AGENT_DIR,
) -> list[str]:
    errors = []
    if not config_path.is_file():
        errors.append(f"missing {config_path}")
    else:
        config = load_toml(config_path, errors)
        agents = get_table(config, "agents", "config: agents", errors)
        if agents.get("enabled") is not True:
            errors.append("config: agents.enabled must be true")
        if agents.get("max_concurrent_threads_per_session") != 4:
            errors.append(
                "config: agents.max_concurrent_threads_per_session must be 4"
            )
        mcp_servers = get_table(
            config,
            "mcp_servers",
            "config: mcp_servers",
            errors,
        )
        context7 = get_table(
            mcp_servers,
            "context7",
            "config: mcp_servers.context7",
            errors,
        )
        if context7.get("command") != "npx":
            errors.append("config: context7 command must be npx")
        if context7.get("args") != ["-y", "@upstash/context7-mcp"]:
            errors.append("config: context7 args mismatch")

    found = (
        {path.stem for path in agent_dir.glob("*.toml")}
        if agent_dir.is_dir()
        else set()
    )
    for extra in sorted(found - EXPECTED):
        errors.append(f"unexpected agent file: {extra}.toml")

    for name in sorted(EXPECTED):
        path = agent_dir / f"{name}.toml"
        if not path.is_file():
            errors.append(f"missing {path}")
            continue
        data = load_toml(path, errors)
        errors.extend(validate_agent_data(name, data))
    return errors


def main() -> int:
    errors = validate_repository()
    if errors:
        for error in errors:
            print(error, file=sys.stderr)
        return 1
    print("Codex configuration is valid")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
