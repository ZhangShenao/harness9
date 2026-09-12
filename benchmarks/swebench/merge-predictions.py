#!/usr/bin/env python3
"""合并多个 SWE-bench run 目录的 predictions 为单一最终提交文件。

背景：runner --resume 是追加式（不覆盖旧行），断点续跑/补跑会产生同一 instance_id
的多条记录；官方评分要求每个 instance 恰好一条预测。本脚本按 keep-best 语义去重：

  - 非空 model_patch 恒优于空 patch（空 patch = 基础设施失败，不代表模型能力）；
  - 同为非空（或同为空）时，取命令行中**靠后**的 run 目录（越晚的 run 越新）。

除 predictions.jsonl 外，还拼装导出与复盘所需的其他件：
  - logs/：union 各 run 目录的 logs/<RunID>/ 子目录（时间戳命名天然不冲突，
    export 工具按 mtime 取每个实例最新一份轨迹日志）；
  - usage.jsonl：顺序追加（消费方按 instance_id keep-last 去重，见 v4 复盘教训）；
  - model_snapshot.json：取最高优先级 run 目录中的那份（同模型多轮内容一致）。

用法：
  python3 merge-predictions.py -o <out_dir> <run_dir1> [run_dir2 ...]

优先级：run_dir_N > run_dir_{N-1} > ... > run_dir1。
"""

import argparse
import json
import shutil
from pathlib import Path


def load_preds(run_dir: Path) -> dict[str, dict]:
    """读取单 run 的 predictions.jsonl → {instance_id: record}（同 id 取最后一行）。"""
    path = run_dir / "predictions.jsonl"
    if not path.exists():
        raise SystemExit(f"错误：{path} 不存在")
    preds: dict[str, dict] = {}
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            rec = json.loads(line)
            preds[rec["instance_id"]] = rec
    return preds


def main() -> None:
    ap = argparse.ArgumentParser(description="多 run 目录 predictions keep-best 合并")
    ap.add_argument("-o", "--out", required=True, help="合并输出目录")
    ap.add_argument("run_dirs", nargs="+", help="run 目录，靠后者优先级高")
    args = ap.parse_args()

    out = Path(args.out)
    runs = [Path(d) for d in args.run_dirs]

    merged: dict[str, dict] = {}
    source: dict[str, str] = {}
    for run in runs:  # 靠后的 run 后处理 → 天然覆盖同名 instance
        for iid, rec in load_preds(run).items():
            old = merged.get(iid)
            new_patch = (rec.get("model_patch") or "").strip()
            old_patch = (old.get("model_patch") or "").strip() if old else ""
            if old is None or (new_patch and not old_patch):
                merged[iid] = rec
                source[iid] = run.name

    out.mkdir(parents=True, exist_ok=True)
    with open(out / "predictions.jsonl", "w") as f:
        for iid in sorted(merged):
            f.write(json.dumps(merged[iid], ensure_ascii=False) + "\n")

    # logs/ union：copytree 各 run 的 logs/<RunID>/ 子目录（ exist_ok 容忍重名）。
    (out / "logs").mkdir(exist_ok=True)
    for run in runs:
        src_logs = run / "logs"
        if not src_logs.is_dir():
            continue
        for sub in src_logs.iterdir():
            if sub.is_dir():
                shutil.copytree(sub, out / "logs" / sub.name, dirs_exist_ok=True)

    # usage.jsonl 顺序追加（低优先级在前），消费方按 instance_id keep-last 去重。
    usage_lines: list[str] = []
    for run in runs:
        u = run / "usage.jsonl"
        if u.exists():
            usage_lines.extend(l for l in u.read_text().splitlines() if l.strip())
    if usage_lines:
        (out / "usage.jsonl").write_text("\n".join(usage_lines) + "\n")

    for run in reversed(runs):  # 最高优先级优先
        snap = run / "model_snapshot.json"
        if snap.exists():
            shutil.copy2(snap, out / "model_snapshot.json")
            break

    non_empty = sum(1 for r in merged.values() if (r.get("model_patch") or "").strip())
    print(f"合并完成：{out}/predictions.jsonl 共 {len(merged)} 条，非空 patch {non_empty} 条")
    by_src: dict[str, int] = {}
    for s in source.values():
        by_src[s] = by_src.get(s, 0) + 1
    for s, n in sorted(by_src.items()):
        print(f"  来自 {s}: {n} 条")


if __name__ == "__main__":
    main()
