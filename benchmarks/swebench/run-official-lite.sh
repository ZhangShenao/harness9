#!/usr/bin/env bash
# SWE-bench Lite 官方打榜一键复现入口（P0：可复现性打包）。
#
# 串联完整流程：依赖检查 → runner 基础镜像预拉 → runner 跑分（固定 seed，模型快照存证）
#   → 官方每实例 eval 镜像预拉（复用 pull-images.sh）→ 官方 harness 评分
#   → 导出官方提交物（all_preds.jsonl + logs/<id>/ + trajs/<id>.md + EXPORT_MANIFEST.md）
#   → 渲染提交 README（占位符模板）。
#
# 可复现性约定：
#   - 采样 seed 固定为 1（同 seed → 同实例集，见 runner --seed）；
#   - 评测启动时从 OpenRouter 公开元数据端点抓取模型版本快照，落盘 run 目录
#     model_snapshot.json（网络失败 fail-open，不阻断评测）；
#   - 官方每实例 eval 镜像依赖 predictions 清单才知道要拉哪些，故在 runner 产出后预拉。
#
# 用法：
#   ./run-official-lite.sh [--sample N] [--dataset PATH] [--output DIR] [--parallel N] [--dry-run]
#
#   --sample N    每 repo 抽样上限（默认 100，覆盖 SWE-bench Lite 全部 300 条）；
#                 官方打榜要求全量 300 条 pass@1，校准阶段可用 --sample 5（约 50 例）
#   --dry-run     只打印将执行的步骤，不实际执行
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TEMPLATE="$REPO_ROOT/benchmarks/swebench/submission/README.template.md"

SAMPLE=100
DATASET="$REPO_ROOT/swe-bench-lite.jsonl"
OUTPUT=""
PARALLEL="${SWEBENCH_PARALLEL:-1}"
DRY_RUN=0

usage() { sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --sample) SAMPLE="$2"; shift 2 ;;
    --dataset) DATASET="$2"; shift 2 ;;
    --output) OUTPUT="$2"; shift 2 ;;
    --parallel) PARALLEL="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    --help|-h) usage; exit 0 ;;
    *) echo "未知参数: $1（--help 查看用法）" >&2; exit 1 ;;
  esac
done

RUN_ID="official-lite-$(date +%Y%m%d-%H%M%S)"
OUTPUT="${OUTPUT:-$REPO_ROOT/swebench-official/$RUN_ID}"
SANDBOX_IMAGE="${SANDBOX_IMAGE:-python:3.11}"

# run_sh 回显并执行一条命令；--dry-run 模式只打印不执行。
run_sh() {
  echo ""
  echo "==> $1"
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "    [dry-run] 仅打印，不执行"
    return 0
  fi
  bash -c "$1"
}

# need_cmd 检查命令可用；dry-run 下缺失只警告不终止（保持计划可见）。
need_cmd() {
  if command -v "$1" >/dev/null 2>&1; then
    echo "  [ok] $1"
  elif [ "$DRY_RUN" -eq 1 ]; then
    echo "  [dry-run 警告] $1 不可用（正式执行时必需）"
  else
    echo "错误：$1 命令不可用，请先安装" >&2
    exit 1
  fi
}

echo "=== SWE-bench Lite 官方打榜一键复现 ==="
echo "RunID: $RUN_ID"
echo "输出目录: $OUTPUT"
echo "采样上限/repo: $SAMPLE ｜ seed: 1（固定）｜ 并发: $PARALLEL"
echo "Dry-run: $DRY_RUN"

# ---- 步骤 1：依赖检查 ----
echo ""
echo "==> [1/7] 依赖检查"
need_cmd docker
need_cmd go
need_cmd python3
ENV_FILE="$REPO_ROOT/.env"
if grep -qE '^(OPENAI_API_KEY|ORCAROUTER_API_KEY)=.+' "$ENV_FILE" 2>/dev/null \
  || [ -n "${OPENAI_API_KEY:-}" ] || [ -n "${ORCAROUTER_API_KEY:-}" ]; then
  echo "  [ok] LLM API Key（.env 或环境变量）"
elif [ "$DRY_RUN" -eq 1 ]; then
  echo "  [dry-run 警告] 未检测到 OPENAI_API_KEY / ORCAROUTER_API_KEY（正式执行时必需）"
else
  echo "错误：请在 $ENV_FILE 配置 OPENAI_API_KEY（或 ORCAROUTER_API_KEY）" >&2
  exit 1
fi
if [ -f "$DATASET" ]; then
  echo "  [ok] 数据集 $DATASET"
elif [ "$DRY_RUN" -eq 1 ]; then
  echo "  [dry-run 警告] 数据集 $DATASET 不存在（正式执行时必需）"
else
  echo "错误：数据集 $DATASET 不存在，请先下载：" >&2
  echo "  python3 -c \"from datasets import load_dataset; load_dataset('princeton-nlp/SWE-bench_Lite', split='test').to_json('$DATASET')\"" >&2
  exit 1
fi
if [ ! -f "$TEMPLATE" ]; then
  echo "错误：提交 README 模板不存在：$TEMPLATE" >&2
  exit 1
fi
if command -v python3 >/dev/null 2>&1 && ! python3 -c "import swebench" >/dev/null 2>&1; then
  echo "  [提示] python swebench 包未安装，评分阶段需要：pip install swebench"
fi

# ---- 步骤 2：runner 基础镜像预拉 ----
run_sh "docker pull --platform linux/amd64 '$SANDBOX_IMAGE'"

# ---- 步骤 3：runner 跑分（固定 seed + 模型快照存证）----
run_sh "cd '$REPO_ROOT' && go run ./cmd/swebench --dataset '$DATASET' --sample $SAMPLE --seed 1 --parallel $PARALLEL --output '$OUTPUT'"

# ---- 步骤 4：官方每实例 eval 镜像预拉（复用 pull-images.sh，依赖 predictions 清单）----
run_sh "bash '$REPO_ROOT/benchmarks/swebench/pull-images.sh' '$OUTPUT/predictions.jsonl' 9 || echo '警告: 部分镜像预拉失败，评分阶段将按需拉取'"

# ---- 步骤 5：官方 harness 评分（产物落在 OUTPUT/logs/ 下）----
run_sh "cd '$OUTPUT' && python3 -m swebench.harness.run_evaluation --dataset_name princeton-nlp/SWE-bench_Lite --predictions_path predictions.jsonl --run_id '$RUN_ID' --max_workers 4"

# ---- 步骤 6：导出官方提交物 ----
run_sh "cd '$REPO_ROOT' && go run ./cmd/swebench --mode export --from '$OUTPUT' --eval-logs '$OUTPUT/logs' --out '$OUTPUT/submission'"

# ---- 步骤 7：渲染提交 README（模板占位符替换）----
# .env 可能不存在（dry-run / 未配置），管道尾部 || true 防止 pipefail 中断收尾。
MODEL="$(sed -n 's/^LLM_MODEL=//p' "$ENV_FILE" 2>/dev/null | head -1 | tr -d '\"' | tr -d "'" | xargs || true)"
MODEL="${MODEL:-${LLM_MODEL:-openai/gpt-4o-mini}}"
TODAY="$(date +%F)"
run_sh "sed -e 's|{{MODEL}}|$MODEL|g' -e 's|{{DATE}}|$TODAY|g' -e 's|{{RUN_ID}}|$RUN_ID|g' '$TEMPLATE' > '$OUTPUT/submission/README.md'"

echo ""
echo "=== 完成 ==="
echo "Run 目录：$OUTPUT"
echo "提交物：  $OUTPUT/submission（对照 docs/技术调研/swebench-lite-官方打榜执行方案.md 检查 checklist 后发布）"
