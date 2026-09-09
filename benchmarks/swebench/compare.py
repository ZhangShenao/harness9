#!/usr/bin/env python3
"""SWE-bench 评测对比报告工具。

消费官方 swebench harness 的评分 JSON 与 runner 的 usage.jsonl，输出 markdown 报告。

用法（单臂，本轮 v4 主模式）：
    python3 compare.py --report v4.json --usage usage.jsonl --out report.md

用法（双臂翻转，严格归因）：
    python3 compare.py --report v4.json --compare-report baseline.json

用法（v3 参考对照，模型不同仅方向性参考）：
    python3 compare.py --report v4.json --reference-report v3.json
"""

import argparse
import json
import math
import sys
from collections import defaultdict
from pathlib import Path
from statistics import mean, median


def load_json(path):
    with open(path) as f:
        return json.load(f)


def load_usage(path):
    records = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line:
                records.append(json.loads(line))
    return records


def repo_of(instance_id):
    """django__django-12908 → django/django"""
    parts = instance_id.split("__", 1)
    return parts[0] + "/" + parts[1].rsplit("-", 1)[0] if len(parts) == 2 else instance_id


def wilson_ci(successes, total, z=1.96):
    """95% Wilson score interval，返回 (lower, upper)，比例口径。"""
    if total == 0:
        return 0.0, 0.0
    p = successes / total
    denom = 1 + z * z / total
    center = (p + z * z / (2 * total)) / denom
    half = z * math.sqrt(p * (1 - p) / total + z * z / (4 * total * total)) / denom
    return center - half, center + half


def pct(n, d):
    return f"{n}/{d}（{100 * n / d:.1f}%）" if d else "0/0"


def overall_section(report, title):
    total = report["submitted_instances"]
    resolved = report["resolved_instances"]
    lo, hi = wilson_ci(resolved, total)
    lines = [f"## {title}", "",
             f"- 提交实例：{total}",
             f"- Resolved：{pct(resolved, total)}",
             f"- 95% Wilson CI：[{100 * lo:.1f}%, {100 * hi:.1f}%]",
             f"- Unresolved：{report['unresolved_instances']}",
             f"- Empty patch：{report['empty_patch_instances']}",
             f"- Error：{report['error_instances']}", ""]
    return lines


def repo_section(report):
    by_repo = defaultdict(lambda: [0, 0])
    for iid in report["submitted_ids"]:
        by_repo[repo_of(iid)][0] += 1
    for iid in report["resolved_ids"]:
        by_repo[repo_of(iid)][1] += 1
    lines = ["## 按 Repo 分布", "", "| Repo | 实例数 | Resolved | Rate |",
             "|------|-------:|---------:|-----:|"]
    for repo in sorted(by_repo):
        total, resolved = by_repo[repo]
        rate = f"{100 * resolved / total:.0f}%" if total else "-"
        lines.append(f"| {repo} | {total} | {resolved} | {rate} |")
    return lines + [""]


def efficiency_section(records):
    if not records:
        return ["## 效率指标", "", "（未提供 usage.jsonl，跳过）", ""]
    fields = [("input_tokens", "Input Tokens"), ("output_tokens", "Output Tokens"),
              ("llm_calls", "LLM 调用"), ("turns", "Turns"),
              ("duration_sec", "时长（秒）")]
    lines = ["## 效率指标", "", "| 指标 | 中位数 | 均值 | P90 |",
             "|------|-------:|-----:|----:|"]
    for key, label in fields:
        values = sorted(r[key] for r in records if key in r)
        if not values:
            continue
        p90 = values[min(len(values) - 1, math.ceil(0.9 * len(values)) - 1)]
        lines.append(f"| {label} | {median(values):.0f} | {mean(values):.0f} | {p90:.0f} |")
    adopters = [r for r in records if r.get("plan_writes", 0) > 0]
    non = [r for r in records if r.get("plan_writes", 0) == 0]
    lines += ["", f"- plan_write 采用：{len(adopters)}/{len(records)} 实例"
              f"（{100 * len(adopters) / len(records):.0f}%）" if records else ""]
    for label, group in [("采用 plan_write", adopters), ("未采用", non)]:
        if group:
            med_turns = median(r["turns"] for r in group)
            lines.append(f"- {label}：{len(group)} 实例，Turns 中位数 {med_turns:.0f}")
    return lines + [""]


def flip_section(new_report, old_report):
    new_res, new_unres = set(new_report["resolved_ids"]), set(new_report["unresolved_ids"])
    old_res, old_unres = set(old_report["resolved_ids"]), set(old_report["unresolved_ids"])
    wins = sorted(new_res & old_unres)
    losses = sorted(new_unres & old_res)
    lines = ["## 翻转分析（配对）", "",
             f"- Wins（旧失败 → 新解决）：{len(wins)}",
             f"- Losses（旧解决 → 新失败）：{len(losses)}", ""]
    if wins:
        lines += ["### Wins"] + [f"- {i}" for i in wins] + [""]
    if losses:
        lines += ["### Losses（需逐条归因）"] + [f"- {i}" for i in losses] + [""]
    return lines


def reference_section(report, ref_report):
    overlap = sorted(set(report["submitted_ids"]) & set(ref_report["submitted_ids"]))
    ref_res = set(ref_report["resolved_ids"])
    new_res = set(report["resolved_ids"])
    both = sorted(i for i in overlap if i in new_res and i in ref_res)
    only_new = sorted(i for i in overlap if i in new_res and i not in ref_res)
    only_ref = sorted(i for i in overlap if i not in new_res and i in ref_res)
    lines = ["## v3 参考对照（⚠️ 模型不同，仅方向性参考，不可归因）", "",
             f"- 重叠实例（两轮 submitted 交集）：{len(overlap)}",
             f"- 两轮均解决：{len(both)}",
             f"- 仅本轮解决：{len(only_new)}",
             f"- 仅 v3 解决：{len(only_ref)}", ""]
    if only_ref:
        lines += ["v3 独有解决（候选回归，逐条复核轨迹后再定性）："] + [f"- {i}" for i in only_ref] + [""]
    return lines


def main():
    parser = argparse.ArgumentParser(description="SWE-bench 评测对比报告")
    parser.add_argument("--report", required=True, help="本轮评分 JSON（官方 harness 产物）")
    parser.add_argument("--usage", help="本轮 usage.jsonl（runner 产出，启用效率指标）")
    parser.add_argument("--compare-report", help="对照轮评分 JSON（同模型双臂，启用翻转分析）")
    parser.add_argument("--reference-report", help="v3 评分 JSON（模型不同，参考对照）")
    parser.add_argument("--out", help="输出 markdown 路径（缺省打印 stdout）")
    args = parser.parse_args()

    report = load_json(args.report)
    lines = [f"# SWE-bench 评测报告：{Path(args.report).stem}", ""]
    lines += overall_section(report, "总体结果")
    lines += repo_section(report)
    if args.usage:
        lines += efficiency_section(load_usage(args.usage))
    if args.compare_report:
        lines += flip_section(report, load_json(args.compare_report))
    if args.reference_report:
        lines += reference_section(report, load_json(args.reference_report))

    output = "\n".join(lines)
    if args.out:
        Path(args.out).write_text(output, encoding="utf-8")
        print(f"报告已写入 {args.out}", file=sys.stderr)
    else:
        print(output)


if __name__ == "__main__":
    main()
