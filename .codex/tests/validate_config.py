#!/usr/bin/env python3
from pathlib import Path
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
errors: list[str] = []


def load_toml(path: Path) -> dict:
    try:
        with path.open("rb") as handle:
            return tomllib.load(handle)
    except (OSError, tomllib.TOMLDecodeError) as exc:
        errors.append(f"{path}: {exc}")
        return {}


def get_table(data: dict, key: str, label: str) -> dict:
    value = data.get(key, {})
    if not isinstance(value, dict):
        errors.append(f"{label} must be a table")
        return {}
    return value


if not CONFIG.is_file():
    errors.append(f"missing {CONFIG}")
else:
    config = load_toml(CONFIG)
    agents = get_table(config, "agents", "config: agents")
    if agents.get("enabled") is not True:
        errors.append("config: agents.enabled must be true")
    if agents.get("max_concurrent_threads_per_session") != 4:
        errors.append("config: agents.max_concurrent_threads_per_session must be 4")
    mcp_servers = get_table(config, "mcp_servers", "config: mcp_servers")
    context7 = get_table(mcp_servers, "context7", "config: mcp_servers.context7")
    if context7.get("command") != "npx":
        errors.append("config: context7 command must be npx")
    if context7.get("args") != ["-y", "@upstash/context7-mcp"]:
        errors.append("config: context7 args mismatch")

found = {path.stem for path in AGENT_DIR.glob("*.toml")} if AGENT_DIR.is_dir() else set()
for extra in sorted(found - EXPECTED):
    errors.append(f"unexpected agent file: {extra}.toml")

for name in sorted(EXPECTED):
    path = AGENT_DIR / f"{name}.toml"
    if not path.is_file():
        errors.append(f"missing {path}")
        continue
    data = load_toml(path)
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
    if name == "harness-enhancer":
        for marker in ("go build ./...", "go vet ./...", "go test ./...", "gofmt -l ."):
            if marker not in instructions:
                errors.append(f"{name}: missing {marker}")
    if name == "harness-researcher":
        for marker in (
            "DeepAgents",
            "OpenHarness",
            "OpenCode",
            "OpenClaw",
            "HermesAgent",
            "Claude Agent SDK",
            "Context7",
            "docs/技术调研/",
        ):
            if marker not in instructions:
                errors.append(f"{name}: missing {marker}")
    if name == "test-runner":
        if data.get("sandbox_mode") != "read-only":
            errors.append("test-runner: sandbox_mode must be read-only")
        for marker in ("go test ./... -v -count=1", "不修改"):
            if marker not in instructions:
                errors.append(f"test-runner: missing {marker}")
    if name == "harness-blog-writer":
        for marker in (
            "$imagegen",
            "image_gen",
            "website/zh/blog/",
            "至少 6 张正文",
            "1 张封面",
            "$CODEX_HOME/generated_images",
        ):
            if marker not in instructions:
                errors.append(f"{name}: missing {marker}")
    if name in {"collector", "analyzer", "organizer"}:
        workspace_write = get_table(
            data,
            "sandbox_workspace_write",
            f"{name}: sandbox_workspace_write",
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

if errors:
    for error in errors:
        print(error, file=sys.stderr)
    raise SystemExit(1)

print("Codex configuration is valid")
