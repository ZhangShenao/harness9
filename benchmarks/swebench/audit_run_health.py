#!/usr/bin/env python3
"""SWE-bench run 基础设施污染审计（评分前强制质量门控）。

背景（2026-09-12 校准教训）：Docker daemon 抖动期的推理产物全量不可用——agent 的
bash 工具连不上 daemon（docker.sock 消失），模型盲写 patch；round2 patch 交付率
93% 但 resolve 率仅 63.2%（稳定期真实基线 87%）。**patch 交付率不等于质量**，
评分前必须先过本审计，被污染的轮次要整轮作废而非补例（污染按实例随机分布）。

检查项与判定：
  1. 轨迹覆盖：predictions.jsonl 的每个 instance_id 在 logs/*/ 下至少一份
     <id>.log（官方要求 trajs 推理时生成、逐实例齐全；多代日志取最新一代即可）
  2. daemon 污染：单实例全部代次日志中 docker daemon 连接失败签名总数 > 3
     （瞬时抖动 1-2 次可容忍，round2 污染实例为 7-62 次/实例）
  3. 架构翻转：出现 "exec format error" 超过 1 次（真实翻转每次 exec 都会失败；
     单次字面提及常是模型推理散文，见 THRESHOLDS 注释中的实测案例）

用法：
  python3 audit_run_health.py <run_dir>

退出码：0=通过（可评分）；1=存在污染或覆盖缺口（禁止评分）。
签名清单集中在 SIGNATURES，发现新的基础设施故障签名时在此扩充。
"""

import argparse
import json
import sys
from pathlib import Path

# 基础设施故障签名（大小写不敏感子串匹配）。daemon 连接失败在 macOS 上表现为
# docker.sock 路径消失，Go 层报 "failed to connect to the Docker API" 或
# "cannot connect to the Docker daemon" 两种措辞。
SIGNATURES = {
    "daemon": (
        "failed to connect to the docker",
        "cannot connect to the docker",
    ),
    "arch": ("exec format error",),
}

# 架构翻转签名的阈值：真实翻转会让每次 exec 都失败（重复出现），而模型的推理
# 散文里可能出现单次字面提及（2026-09-13 全量正赛实测：sklearn-14087 的模型
# 在猜测 macOS 权限问题时写下 "OSError Exec format error"，该实例最终 resolved）。
# daemon 阈值 3 的理由见模块 docstring；单次瞬时抖动可容忍，污染实例为 7-62 次。
THRESHOLDS = {"daemon": 3, "arch": 1}


def counts_exceed(counts: dict) -> bool:
    return any(
        counts[kind] > threshold
        for kind, threshold in THRESHOLDS.items()
    )


def instance_ids(pred_path: Path) -> list[str]:
    ids = []
    with open(pred_path) as f:
        for line in f:
            line = line.strip()
            if line:
                ids.append(json.loads(line)["instance_id"])
    return ids


def audit(run_dir: Path) -> int:
    pred_path = run_dir / "predictions.jsonl"
    if not pred_path.exists():
        print(f"错误：{pred_path} 不存在", file=sys.stderr)
        return 1
    ids = instance_ids(pred_path)
    logs_root = run_dir / "logs"

    gaps, contaminated = [], []
    for iid in ids:
        logs = sorted(logs_root.glob(f"*/{iid}.log"))
        if not logs:
            gaps.append(iid)
            continue
        counts = {k: 0 for k in SIGNATURES}
        for log in logs:
            text = log.read_text(errors="replace").lower()
            for kind, sigs in SIGNATURES.items():
                counts[kind] += sum(text.count(s) for s in sigs)
        if counts_exceed(counts):
            contaminated.append((iid, counts))

    total = len(ids)
    print(f"审计目标：{run_dir}")
    print(f"  实例 {total} ｜ 轨迹覆盖 {total - len(gaps)}/{total}"
          f" ｜ 污染 {len(contaminated)}")
    for iid in gaps:
        print(f"  [覆盖缺口] {iid}（logs/ 下无任何轨迹日志）")
    for iid, counts in contaminated:
        detail = " ".join(f"{k}={v}" for k, v in counts.items() if v)
        print(f"  [污染] {iid}（阈值 {THRESHOLDS}）：{detail}")

    if gaps or contaminated:
        print("结论：不通过——禁止评分；污染轮次应整轮重跑（补例无法修复同类污染）。")
        return 1
    print("结论：通过——轨迹齐全且无基础设施污染，可以评分。")
    return 0


def main() -> None:
    ap = argparse.ArgumentParser(description="run 目录基础设施污染审计")
    ap.add_argument("run_dir", help="runner run 目录（含 predictions.jsonl 与 logs/）")
    args = ap.parse_args()
    sys.exit(audit(Path(args.run_dir)))


if __name__ == "__main__":
    main()
