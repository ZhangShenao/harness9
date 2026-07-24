#!/bin/bash
# Synchronize selected harness9 Markdown files after Codex writes them.

set -u

PROJECT_ROOT="/Users/zsa/Desktop/harness/harness9"
OBSIDIAN_VAULT="/Users/zsa/Desktop/workspace/harness9"

python3 -c '
import json
import os
import re
import secrets
import stat
import sys

DIRECTORY_FLAGS = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0)
NOFOLLOW = getattr(os, "O_NOFOLLOW", 0)


def canonical_root(path, require_temporary):
    candidate = os.path.abspath(path)
    candidate_stat = os.lstat(candidate)
    if stat.S_ISLNK(candidate_stat.st_mode):
        raise ValueError("root is a symlink")
    resolved = os.path.realpath(candidate)
    if not os.path.isdir(resolved):
        raise ValueError("root is not a directory")
    if require_temporary:
        trusted_roots = {
            os.path.realpath(fixed)
            for fixed in ("/tmp", "/private/tmp")
            if os.path.isdir(fixed)
        }
        if not any(
            resolved != trusted
            and os.path.commonpath((resolved, trusted)) == trusted
            for trusted in trusted_roots
        ):
            raise ValueError("test root is not beneath a trusted temporary root")
    return candidate, resolved


def relative_source_path(file_path, project_input, project_root):
    if os.path.isabs(file_path):
        absolute = os.path.abspath(file_path)
        roots = (project_input, project_root)
        matching_root = next(
            (
                root
                for root in roots
                if os.path.commonpath((absolute, root)) == root
            ),
            None,
        )
        if matching_root is None:
            raise ValueError("absolute path escapes project root")
        relative = os.path.relpath(absolute, matching_root)
    else:
        relative = file_path

    components = relative.split("/")
    if (
        not components
        or any(component in ("", ".", "..") for component in components)
        or os.path.isabs(relative)
    ):
        raise ValueError("invalid relative path")
    return "/".join(components)


def target_path(relative):
    if not relative.endswith(".md"):
        return None
    if relative.startswith("website/zh/blog/") and relative.endswith(
        "/index.md"
    ):
        slug = relative[len("website/zh/blog/") : -len("/index.md")]
        if not slug or "/" in slug:
            return None
        return ("技术博客", slug + ".md")
    if relative.startswith("docs/"):
        remainder = relative[len("docs/") :]
        return tuple(remainder.split("/")) if remainder else None
    if relative.startswith("knowledge/articles/"):
        filename = relative.rsplit("/", 1)[-1]
        return ("知识库日报", filename)
    return None


def open_parent(root, components, create):
    current = os.open(root, DIRECTORY_FLAGS | NOFOLLOW)
    try:
        for component in components:
            if create:
                try:
                    os.mkdir(component, 0o755, dir_fd=current)
                except FileExistsError:
                    pass
            following = os.open(
                component,
                DIRECTORY_FLAGS | NOFOLLOW,
                dir_fd=current,
            )
            os.close(current)
            current = following
        return current
    except Exception:
        os.close(current)
        raise


def write_all(file_descriptor, data):
    view = memoryview(data)
    while view:
        written = os.write(file_descriptor, view)
        if written <= 0:
            raise OSError("short write")
        view = view[written:]


def atomic_copy(project_root, vault_root, relative, target_components):
    source_components = relative.split("/")
    try:
        source_parent = open_parent(
            project_root,
            source_components[:-1],
            False,
        )
    except FileNotFoundError:
        return
    source = None
    target_parent = None
    temporary_name = None
    try:
        try:
            source = os.open(
                source_components[-1],
                os.O_RDONLY | NOFOLLOW,
                dir_fd=source_parent,
            )
        except FileNotFoundError:
            return
        if not stat.S_ISREG(os.fstat(source).st_mode):
            raise ValueError("source is not a regular file")

        target_parent = open_parent(
            vault_root,
            target_components[:-1],
            True,
        )
        target_name = target_components[-1]
        try:
            destination_stat = os.stat(
                target_name,
                dir_fd=target_parent,
                follow_symlinks=False,
            )
        except FileNotFoundError:
            destination_stat = None
        if destination_stat is not None and not stat.S_ISREG(
            destination_stat.st_mode
        ):
            raise ValueError("destination is not a regular file")

        temporary_name = (
            "." + target_name + ".harness9-" + secrets.token_hex(8) + ".tmp"
        )
        temporary = os.open(
            temporary_name,
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | NOFOLLOW,
            0o600,
            dir_fd=target_parent,
        )
        try:
            while True:
                chunk = os.read(source, 65536)
                if not chunk:
                    break
                write_all(temporary, chunk)
            os.fchmod(temporary, 0o644)
            os.fsync(temporary)
        finally:
            os.close(temporary)

        try:
            destination_stat = os.stat(
                target_name,
                dir_fd=target_parent,
                follow_symlinks=False,
            )
        except FileNotFoundError:
            destination_stat = None
        if destination_stat is not None and not stat.S_ISREG(
            destination_stat.st_mode
        ):
            raise ValueError("destination changed to a non-regular file")

        os.replace(
            temporary_name,
            target_name,
            src_dir_fd=target_parent,
            dst_dir_fd=target_parent,
        )
        temporary_name = None
        os.fsync(target_parent)
    finally:
        if temporary_name is not None and target_parent is not None:
            try:
                os.unlink(temporary_name, dir_fd=target_parent)
            except OSError:
                pass
        if source is not None:
            os.close(source)
        os.close(source_parent)
        if target_parent is not None:
            os.close(target_parent)


try:
    production_project = sys.argv[1]
    production_vault = sys.argv[2]
    testing = sys.argv[3] == "1"
    project_override = sys.argv[4]
    vault_override = sys.argv[5]

    project_path = production_project
    vault_path = production_vault
    if testing:
        if not project_override or not vault_override:
            raise ValueError("test roots are required")
        project_path = project_override
        vault_path = vault_override

    project_input, project_root = canonical_root(project_path, testing)
    _, vault_root = canonical_root(vault_path, testing)

    payload = json.load(sys.stdin)
    if (
        not isinstance(payload, dict)
        or payload.get("hook_event_name") != "PostToolUse"
        or payload.get("tool_name") not in ("apply_patch", "Edit", "Write")
    ):
        raise ValueError("irrelevant hook event")
    tool_input = payload.get("tool_input")
    if not isinstance(tool_input, dict):
        raise ValueError("missing tool input")

    paths = []
    command = tool_input.get("command")
    if isinstance(command, str):
        for line in command.splitlines():
            match = re.match(
                r"^\*\*\* (?:Add File|Update File|Delete File|Move to):"
                r"\s*(.+?)\s*$",
                line,
            )
            if match:
                paths.append(match.group(1))
    if not paths:
        file_path = tool_input.get("file_path")
        if isinstance(file_path, str) and file_path.strip():
            paths.append(file_path.strip())

    for file_path in paths:
        try:
            relative = relative_source_path(
                file_path,
                project_input,
                project_root,
            )
            target = target_path(relative)
            if target is not None:
                atomic_copy(project_root, vault_root, relative, target)
        except Exception:
            print(
                "[obsidian-sync] unable to sync selected Markdown path",
                file=sys.stderr,
            )
except Exception:
    pass
' \
	"$PROJECT_ROOT" \
	"$OBSIDIAN_VAULT" \
	"${HARNESS9_HOOK_TESTING:-}" \
	"${HARNESS9_PROJECT_ROOT:-}" \
	"${HARNESS9_OBSIDIAN_VAULT:-}" \
	|| true

exit 0
