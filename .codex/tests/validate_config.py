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
GUARDED_CLEANUP_COMMAND = (
    "./.codex/scripts/cleanup-knowledge-day.sh YYYYMMDD"
)
KNOWLEDGE_POLICIES = {
    "collector": {
        "knowledge_root": KNOWLEDGE_ROOT,
        "read_directory": "none",
        "write_directory": f"{KNOWLEDGE_ROOT}/raw/{{YYYYMMDD}}/",
        "write_scope": "raw_only",
        "web_research": "codex_live_only",
        "shell_networking": "forbidden",
        "source_allowlist": (
            "github_trending,hacker_news,anthropic_engineering,"
            "langchain_blog"
        ),
        "github_stars": ">100",
        "github_recent_days": "10",
        "github_top": "10",
        "hacker_news_scan": "30",
        "anthropic_window_days": "7",
        "langchain_rss_initial_window_days": "30",
        "langchain_final_window_days": "7",
        "required_fields": (
            "title,url,source,popularity,summary,collected_at"
        ),
        "sort": "popularity_desc",
        "no_fabrication": "true",
    },
    "analyzer": {
        "knowledge_root": KNOWLEDGE_ROOT,
        "read_directory": f"{KNOWLEDGE_ROOT}/raw/{{YYYYMMDD}}/",
        "write_directory": f"{KNOWLEDGE_ROOT}/analysis/{{YYYYMMDD}}/",
        "write_scope": "analysis_only",
        "web_research": "only_when_source_insufficient",
        "fetch_condition": (
            "summary_under_20_chars_or_missing_technical_detail"
        ),
        "shell_networking": "forbidden",
        "required_input_fields": (
            "title,url,source,popularity,summary,collected_at"
        ),
        "required_added_fields": (
            "highlights,importance_score,importance_label,suggested_tags,"
            "deep_summary,analyzed_at,raw_files"
        ),
        "preserve_collected_at": "exact",
        "importance_score": "integer_1_to_10",
        "importance_labels": (
            "9-10:⭐ 改变格局;7-8:🔧 直接有帮助;"
            "5-6:📖 值得了解;1-4:👀 可略过"
        ),
        "highlights": "3-5",
        "suggested_tags": "3-6",
        "deep_summary_sentences": "3-5",
        "sort": "importance_score_desc",
        "no_fabrication": "true",
    },
    "organizer": {
        "knowledge_root": KNOWLEDGE_ROOT,
        "read_directory": f"{KNOWLEDGE_ROOT}/analysis/{{YYYYMMDD}}/",
        "dedup_directory": f"{KNOWLEDGE_ROOT}/articles/",
        "write_directory": f"{KNOWLEDGE_ROOT}/articles/",
        "write_scope": "articles_only",
        "web_research": "forbidden",
        "shell_networking": "forbidden",
        "required_input_fields": (
            "title,url,source,popularity,summary,collected_at,highlights,"
            "importance_score,importance_label,suggested_tags,deep_summary,"
            "analyzed_at,raw_files"
        ),
        "dedup_key": "normalized_title_or_source_url",
        "title_normalization": (
            "lowercase_trim_collapse_spaces_strip_hn_prefixes"
        ),
        "sort": "topic_then_importance_score_desc",
        "article_name": "{YYYYMMDD}-daily.md",
        "existing_name": "append_v2_v3_without_overwrite",
        "frontmatter": "forbidden",
        "cleanup_command": GUARDED_CLEANUP_COMMAND,
        "cleanup_order": "final_article_exists_then_guarded_cleanup",
        "direct_rm": "forbidden",
        "no_fabrication": "true",
    },
}
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
FENCED_BLOCK = re.compile(
    r"```(?P<language>[^\n`]*)\n(?P<body>.*?)```",
    flags=re.DOTALL,
)
INLINE_CODE = re.compile(r"(?<!`)`([^`\n]+)`(?!`)")
SHELL_FENCE_LANGUAGES = frozenset({"", "bash", "sh", "shell", "zsh"})
PYTHON_FENCE_LANGUAGES = frozenset({"python", "python3", "py"})
EXECUTABLE_FENCE_LANGUAGES = (
    SHELL_FENCE_LANGUAGES | PYTHON_FENCE_LANGUAGES
)
NEGATION_MARKERS = (
    "禁止",
    "不得",
    "不可",
    "不能",
    "不应",
    "严禁",
    "绝不",
    "不要",
    "切勿",
    "从未",
    "不允许",
    "无需",
    "没有要求",
    "不接受",
)
ACTION_BOUNDARY = re.compile(r"[,，;；]|但是|然而|但")


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


def contract_assignments(
    section: str,
    label: str,
    errors: list[str],
) -> dict[str, str] | None:
    assignments = {}
    for line in section.splitlines():
        key, separator, value = line.partition("=")
        if not separator:
            continue
        key = key.strip()
        if key in assignments:
            errors.append(f"{label}: duplicate contract assignment {key}")
            return None
        assignments[key] = value.strip()
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


def executable_fenced_blocks(text: str) -> list[tuple[str, str, str]]:
    blocks = []
    for match in FENCED_BLOCK.finditer(text):
        language = match.group("language").strip().casefold()
        language = language.split(maxsplit=1)[0] if language else ""
        if language in EXECUTABLE_FENCE_LANGUAGES:
            preceding = text[: match.start()].rstrip()
            preceding_line = (
                preceding.rsplit("\n", 1)[-1] if preceding else ""
            )
            blocks.append(
                (language, match.group("body"), preceding_line)
            )
    return blocks


def text_without_fenced_blocks(text: str) -> str:
    return FENCED_BLOCK.sub(
        lambda match: " " * len(match.group(0)),
        text,
    )


def clause_around(text: str, start: int, end: int) -> str:
    separators = "\n。！？；"
    clause_start = max(text.rfind(char, 0, start) for char in separators)
    clause_ends = [
        position
        for char in separators
        if (position := text.find(char, end)) >= 0
    ]
    clause_end = min(clause_ends) if clause_ends else len(text)
    return text[clause_start + 1 : clause_end]


def has_negative_context(text: str) -> bool:
    return any(marker in text for marker in NEGATION_MARKERS)


def explicitly_prohibits_invocation(text: str) -> bool:
    return bool(
        re.search(
            r"(?:禁止|不得|不要|不能|不允许|切勿|严禁)"
            r".{0,24}(?:执行|运行|调用|使用)",
            text,
        )
    )


def preceding_context_prohibits_guarded_cleanup(text: str) -> bool:
    segments = action_segments(text)
    if not segments:
        return False
    local = segments[-1]
    if explicitly_prohibits_invocation(local):
        return True
    return bool(
        re.search(
            r"(?:禁止|不得|不要|不能|不允许|切勿|严禁)"
            r".{0,16}(?:以下|下面|下列)"
            r".{0,12}(?:命令|脚本)?",
            local,
        )
    )


def action_segments(text: str) -> list[str]:
    return [
        segment.strip()
        for segment in ACTION_BOUNDARY.split(text)
        if segment.strip()
    ]


def action_context_around(text: str, start: int, end: int) -> str:
    context = clause_around(text, start, end)
    matched_text = text[start:end]
    local_start = context.find(matched_text)
    if local_start < 0:
        return context
    local_end = local_start + len(matched_text)

    segment_start = 0
    segment_end = len(context)
    for boundary in ACTION_BOUNDARY.finditer(context):
        if boundary.end() <= local_start:
            segment_start = boundary.end()
        elif boundary.start() >= local_end:
            segment_end = boundary.start()
            break
    return context[segment_start:segment_end]


def normalize_cleanup_command(command: str) -> str:
    normalized = command
    previous = None
    while previous != normalized:
        previous = normalized
        normalized = normalized.replace("''", "").replace('""', "")
    normalized = re.sub(r"^\s*\$\s*", "", normalized)
    return " ".join(normalized.split())


def looks_like_unsafe_or_alternative_cleanup(command: str) -> bool:
    normalized = normalize_cleanup_command(command)
    if not normalized or normalized == GUARDED_CLEANUP_COMMAND:
        return False

    destructive_patterns = (
        r"(?:^|[\s;&|])(?:/[^\s;&|]+/)?rm(?=\s|$|[;&|])",
        r"(?:^|[\s;&|])(?:/[^\s;&|]+/)?(?:unlink|rmdir)"
        r"(?=\s|$|[;&|])",
        r"(?:^|[\s;&|])find(?=\s).*(?:^|\s)-delete(?=\s|$)",
        r"\bos\s*\.\s*(?:remove|unlink)\s*\(",
        r"\bshutil\s*\.\s*rmtree\s*\(",
        r"(?:\bpathlib\s*\.\s*)?\bPath\s*\([^)]*\)"
        r"\s*\.\s*(?:unlink|rmdir)\s*\(",
    )
    if any(
        re.search(pattern, normalized, flags=re.IGNORECASE | re.DOTALL)
        for pattern in destructive_patterns
    ):
        return True

    cleanup_command_patterns = (
        r"(?:^|[\s;&|])\S*(?:cleanup|clean-up|purge|prune|delete|remove)"
        r"\S*(?=\s|$|[;&|])",
        r"(?:^|[\s;&|])(?:git|make)\s+clean(?=\s|$|[;&|])",
    )
    return any(
        re.search(pattern, normalized, flags=re.IGNORECASE)
        for pattern in cleanup_command_patterns
    )


def looks_like_executable_inline(command: str) -> bool:
    normalized = normalize_cleanup_command(command)
    return bool(
        re.match(
            r"^(?:\./|\.\./)\S+"
            r"|^(?:(?:sudo|command|env|busybox|xargs)\s+)*"
            r"(?:rm|find|unlink|rmdir|python\d*|bash|sh|shell|zsh|"
            r"git|make)(?=\s|$)",
            normalized,
            flags=re.IGNORECASE,
        )
    )


def validate_organizer_cleanup_instructions(
    instructions: str,
) -> list[str]:
    errors = []
    guarded_cleanup_found = False

    for language, block, preceding_line in executable_fenced_blocks(
        instructions
    ):
        for line in block.splitlines():
            command = line.strip()
            if not command or command.startswith("#"):
                continue
            normalized = normalize_cleanup_command(command)
            if language in SHELL_FENCE_LANGUAGES:
                if normalized != GUARDED_CLEANUP_COMMAND:
                    errors.append(
                        "organizer: unsafe or alternative cleanup command "
                        "forbidden"
                    )
                    break
                if preceding_context_prohibits_guarded_cleanup(
                    preceding_line
                ):
                    errors.append(
                        "organizer: explicitly prohibits guarded cleanup "
                        "invocation"
                    )
                else:
                    guarded_cleanup_found = True
                continue

            if (
                normalized != GUARDED_CLEANUP_COMMAND
                and looks_like_unsafe_or_alternative_cleanup(command)
            ):
                errors.append(
                    "organizer: unsafe or alternative cleanup command "
                    "forbidden"
                )
                break

    inline_text = text_without_fenced_blocks(instructions)
    for match in INLINE_CODE.finditer(inline_text):
        command = match.group(1).strip()
        context = action_context_around(
            inline_text,
            match.start(),
            match.end(),
        )
        normalized = normalize_cleanup_command(command)
        if normalized == GUARDED_CLEANUP_COMMAND:
            if explicitly_prohibits_invocation(context):
                errors.append(
                    "organizer: explicitly prohibits guarded cleanup "
                    "invocation"
                )
            continue
        if has_negative_context(context):
            continue
        if (
            looks_like_unsafe_or_alternative_cleanup(command)
            or looks_like_executable_inline(command)
        ):
            errors.append(
                "organizer: unsafe or alternative cleanup command forbidden"
            )
            break

    if not guarded_cleanup_found:
        errors.append(
            "organizer: affirmative fenced guarded cleanup invocation "
            "missing"
        )
    return errors


def text_outside_contract(text: str, contract: str) -> str:
    start = f"<!-- codex-contract:{contract}:start -->"
    end = f"<!-- codex-contract:{contract}:end -->"
    pattern = re.compile(
        re.escape(start) + r".*?" + re.escape(end),
        flags=re.DOTALL,
    )
    return pattern.sub(
        lambda match: " " * len(match.group(0)),
        text,
    )


def operative_instruction_clauses(instructions: str) -> list[str]:
    operative = text_outside_contract(instructions, "knowledge-policy")
    return [
        clause.strip()
        for clause in re.split(r"[\n。！？；]+", operative)
        if clause.strip()
    ]


def has_write_boundary_contradiction(
    instructions: str,
    allowed_directory: str,
) -> bool:
    allowed_prefix = f"{KNOWLEDGE_ROOT}/{allowed_directory}/"
    write_intent = re.compile(
        r"(?:写入|另存|保存(?:到|至)|持久化(?:到|至)|"
        r"输出(?:到|至)|创建(?:文件)?(?:到|于|在))"
    )
    for clause in operative_instruction_clauses(instructions):
        for segment in action_segments(clause):
            if (
                has_negative_context(segment)
                or not write_intent.search(segment)
            ):
                continue

            paths = [
                match.group(1).strip()
                for match in INLINE_CODE.finditer(segment)
                if (match.group(1).strip()).startswith("/")
            ]
            paths.extend(
                match.group(0)
                for match in re.finditer(
                    re.escape(KNOWLEDGE_ROOT)
                    + r"(?:/[^\s`，、：]*)?",
                    segment,
                )
            )
            for path in paths:
                if (
                    path != allowed_prefix.rstrip("/")
                    and not path.startswith(allowed_prefix)
                ):
                    return True
    return False


def has_unconditional_fetch_contradiction(instructions: str) -> bool:
    fetch_action = re.compile(r"(?:回源|抓取|访问|获取)")
    web_or_source = re.compile(
        r"(?:回源|网页|网络|URL|链接|页面|源站)",
        flags=re.IGNORECASE,
    )
    unconditional_scope = re.compile(
        r"(?:无条件|无论|所有条目|每(?:一)?条\s*"
        r"(?:记录|条目|URL|链接|页面)|全部条目|^都)",
        flags=re.IGNORECASE,
    )
    for clause in operative_instruction_clauses(instructions):
        for segment in action_segments(clause):
            if has_negative_context(segment):
                continue
            if (
                fetch_action.search(segment)
                and web_or_source.search(segment)
                and unconditional_scope.search(segment)
            ):
                return True
    return False


def has_cleanup_order_contradiction(instructions: str) -> bool:
    article_action = (
        r"(?:写(?:入)?|创建|生成)(?:最终)?(?:文章|终稿)"
    )
    before_relations = (
        re.compile(rf"先\s*清理.*?再\s*{article_action}"),
        re.compile(rf"清理.*?后\s*再\s*{article_action}"),
        re.compile(rf"清理.*?(?:早于|先于).*?{article_action}"),
        re.compile(rf"清理.*?在.*?{article_action}.*?之前"),
        re.compile(rf"在.*?{article_action}.*?之前.*?清理"),
    )
    for clause in operative_instruction_clauses(instructions):
        for pattern in before_relations:
            for match in pattern.finditer(clause):
                context = action_context_around(
                    clause,
                    match.start(),
                    match.end(),
                )
                if not has_negative_context(context):
                    return True
    return False


def has_alternative_cleanup_policy_contradiction(
    instructions: str,
) -> bool:
    alternate = re.compile(r"(?:备用|替代|其他|任意)")
    execution = re.compile(r"(?:使用|执行|运行|调用|改用|允许|接受)")
    for clause in operative_instruction_clauses(instructions):
        for segment in action_segments(clause):
            if has_negative_context(segment):
                continue
            if (
                "清理" in segment
                and alternate.search(segment)
                and execution.search(segment)
            ):
                return True
    return False


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
        scope_assignments = contract_assignments(
            scope,
            "harness-enhancer repository-scope",
            errors,
        )
        if (
            scope_assignments is not None
            and scope_assignments != expected_scope
        ):
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
        policy_assignments = contract_assignments(
            policy,
            "harness-researcher research-policy",
            errors,
        )
        if (
            policy_assignments is not None
            and policy_assignments != expected_policy
        ):
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
        communication_assignments = contract_assignments(
            communication,
            "test-runner communication",
            errors,
        )
        if (
            communication_assignments is not None
            and communication_assignments != expected_communication
        ):
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
    if image_generation is not None:
        image_assignments = contract_assignments(
            image_generation,
            "harness-blog-writer image-generation",
            errors,
        )
        if (
            image_assignments is not None
            and image_assignments != BLOG_IMAGE_GENERATION_CONTRACT
        ):
            errors.append(
                "harness-blog-writer: image generation contract mismatch"
            )
    return errors


def validate_knowledge_agent(
    name: str,
    data: dict,
    instructions: str,
) -> list[str]:
    errors = []
    if data.get("sandbox_mode") != "workspace-write":
        errors.append(f"{name}: sandbox_mode must be workspace-write")

    workspace_write = get_table(
        data,
        "sandbox_workspace_write",
        f"{name}: sandbox_workspace_write",
        errors,
    )
    if workspace_write.get("writable_roots") != [KNOWLEDGE_ROOT]:
        errors.append(f"{name}: writable_roots mismatch")
    if workspace_write.get("network_access") is not False:
        errors.append(f"{name}: network_access must be false")

    if name in {"collector", "analyzer"}:
        if data.get("web_search") != "live":
            errors.append(f"{name}: web_search must be live")
    elif "web_search" in data:
        errors.append("organizer: web_search must be absent")

    policy = contract_section(
        instructions,
        "knowledge-policy",
        name,
        errors,
    )
    if policy is not None:
        assignments = contract_assignments(
            policy,
            f"{name} knowledge-policy",
            errors,
        )
        if (
            assignments is not None
            and assignments != KNOWLEDGE_POLICIES[name]
        ):
            errors.append(f"{name}: knowledge policy mismatch")

    allowed_write_directories = {
        "collector": "raw",
        "analyzer": "analysis",
        "organizer": "articles",
    }
    if has_write_boundary_contradiction(
        instructions,
        allowed_write_directories[name],
    ):
        errors.append(f"{name}: operative write boundary contradiction")

    if name == "analyzer" and has_unconditional_fetch_contradiction(
        instructions
    ):
        errors.append("analyzer: conditional fetch contradiction")

    if name == "organizer":
        errors.extend(validate_organizer_cleanup_instructions(instructions))
        if has_cleanup_order_contradiction(instructions):
            errors.append("organizer: cleanup order contradiction")
        if has_alternative_cleanup_policy_contradiction(instructions):
            errors.append(
                "organizer: alternative cleanup policy contradiction"
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
        errors.extend(validate_knowledge_agent(name, data, instructions))
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
