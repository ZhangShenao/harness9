#!/usr/bin/env python3
"""校验 .dsh/skills 是否符合 DeepSeek Harness 项目技能规范。

用法：python3 .dsh/scripts/validate.py
退出码 0 表示全部通过；1 表示存在问题。
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

SKILLS_DIR = Path(__file__).resolve().parent.parent / "skills"
NAME_RE = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)*$")
FORBIDDEN_KEYS = {"tools", "model", "mcpServers", "trigger", "argument-hint"}
FORBIDDEN_TEXT = ["$ARGUMENTS", "model: inherit", "mcpServers", "task("]


def parse_frontmatter(text: str) -> dict[str, str] | None:
    lines = text.split("\n")
    if not lines or lines[0].strip() != "---":
        return None
    end = next((i for i in range(1, len(lines)) if lines[i].strip() == "---"), None)
    if end is None:
        return None
    data: dict[str, str] = {}
    for line in lines[1:end]:
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if ":" not in line:
            continue
        key, value = line.split(":", 1)
        data[key.strip()] = value.strip().strip('"').strip("'")
    return data


def main() -> int:
    errors: list[str] = []
    warnings: list[str] = []
    if not SKILLS_DIR.is_dir():
        print(f"缺少目录：{SKILLS_DIR}")
        return 1

    skill_dirs = sorted(p for p in SKILLS_DIR.iterdir() if p.is_dir())
    if not skill_dirs:
        print("没有找到任何技能目录")
        return 1

    names: dict[str, str] = {}
    for d in skill_dirs:
        skill_file = d / "SKILL.md"
        rel = skill_file.relative_to(SKILLS_DIR.parent)
        if not skill_file.is_file():
            errors.append(f"{d.name}: 缺少 SKILL.md")
            continue
        text = skill_file.read_text(encoding="utf-8")
        fm = parse_frontmatter(text)
        if fm is None:
            errors.append(f"{rel}: 缺少合法的 YAML frontmatter")
            continue

        name = fm.get("name", "")
        desc = fm.get("description", "")
        if not name:
            errors.append(f"{rel}: 缺少 name")
        elif not NAME_RE.match(name):
            errors.append(f"{rel}: name `{name}` 不是 kebab-case")
        elif name != d.name:
            errors.append(f"{rel}: name `{name}` 与目录名 `{d.name}` 不一致")
        else:
            names.setdefault(name, str(rel))
            if names[name] != str(rel):
                errors.append(f"{rel}: 技能名 `{name}` 重复")
        if not desc:
            errors.append(f"{rel}: 缺少 description")

        for key in FORBIDDEN_KEYS:
            if key in fm:
                errors.append(f"{rel}: frontmatter 含 DSH 不支持的字段 `{key}`")
        for token in FORBIDDEN_TEXT:
            if token in text:
                errors.append(f"{rel}: 正文仍残留旧平台写法 `{token}`")
        if "`subagent`" not in text and d.name.startswith("agent-"):
            warnings.append(f"{rel}: 角色技能建议在正文说明子代理委派方式")

    print(f"检查技能：{len(skill_dirs)} 个")
    for name in sorted(names):
        print(f"  ✓ {name}")
    for w in warnings:
        print(f"  ! {w}")
    for e in errors:
        print(f"  ✗ {e}")
    print("结果：" + ("通过" if not errors else f"失败（{len(errors)} 个问题）"))
    return 0 if not errors else 1


if __name__ == "__main__":
    sys.exit(main())
