#!/usr/bin/env bash
#
# quality-report.sh — 组装 PR CI 质量报告并创建/更新 sticky 评论
#
# 用途：
#   CI 的 quality-report job 在 PR 的四个门禁 job（lint / build / test / duplication）结束后运行，
#   本脚本聚合单元测试覆盖率、代码重复率、门禁矩阵、文档漂移警告数与 Eval 状态，
#   渲染为 markdown 并通过 gh api 在 PR 内创建/更新一条带 marker 的 sticky 评论；
#   push 到 master 不运行本脚本（job 侧 if 条件保证）。
#
# 依赖：gh（评论读写与 Eval 查询）、jq（payload 构造与 metrics 解析）、go tool cover（覆盖率解析）
# 输入：
#   环境变量 REPO / PR_NUMBER / SHA / LINT_RESULT / BUILD_RESULT / TEST_RESULT /
#            DUP_RESULT / DRIFTS / GITHUB_SERVER_URL / GITHUB_RUN_ID
#   文件 coverage.out（test job artifact）、jscpd-report/jscpd-report.json（duplication job
#   artifact，也兼容 artifact 下载后落在仓库根的布局）、.jscpd.json（threshold）
# 容错：
#   所有 best-effort 查询（Eval run 查询、metrics 可选字段）单独 || true 保护，失败只降级为
#   N/A，绝不拖垮报告；只有评论操作本身失败才导致非零退出。DRY_RUN=1 时仅输出 markdown。
set -euo pipefail

MARKER='<!-- harness9-ci-quality-report -->'
REPO="${REPO:-}"
PR_NUMBER="${PR_NUMBER:-}"
SHA="${SHA:-}"
LINT_RESULT="${LINT_RESULT:-unknown}"
BUILD_RESULT="${BUILD_RESULT:-unknown}"
TEST_RESULT="${TEST_RESULT:-unknown}"
DUP_RESULT="${DUP_RESULT:-unknown}"
DRIFTS="${DRIFTS:-}"

# ---------- 覆盖率（go tool cover + 按包聚合，升序） ----------
COV_FILE=""
for c in coverage.out quality-inputs/coverage.out; do
  if [ -f "$c" ]; then COV_FILE="$c"; break; fi
done

COVERAGE_SECTION="N/A（coverage.out 缺失或 go 不可用）"
if [ -n "$COV_FILE" ] && command -v go >/dev/null 2>&1; then
  total_line=$(go tool cover -func="$COV_FILE" | tail -1) || total_line=""
  total_pct=$(awk '{print $NF}' <<<"$total_line")
  # coverage.out 行格式：github.com/harness9/<pkg>/file.go:12.34,56.7 <语句数> <执行计数>
  # 取 github.com/harness9/ 之后、最后一段 / 之前的目录为包，count>0 视为覆盖，升序输出
  pkg_table=$(awk '
    NR>1 {
      split($1, a, ":")
      sub(/^github.com\/harness9\//, "", a[1])
      n = split(a[1], segs, "/")
      pkg = segs[1]
      for (i = 2; i < n; i++) pkg = pkg "/" segs[i]
      stmts[pkg] += $2
      if ($3 > 0) covs[pkg] += $2
    }
    END {
      for (p in stmts) printf "%.1f %s\n", 100 * covs[p] / stmts[p], p
    }' "$COV_FILE" | sort -n | awk '{ printf "| `%s` | %s%% |\n", $2, $1 }')
  if [ -n "$total_pct" ] && [ -n "$pkg_table" ]; then
    COVERAGE_SECTION=$(printf '**总覆盖率：%s**（go tool cover -func）\n\n> 覆盖率当前为参考值，不设阈值门禁。\n\n| 包 | 覆盖率 |\n|----|--------|\n%s' "$total_pct" "$pkg_table")
  fi
fi

# ---------- 重复率（jscpd metrics json） ----------
DUP_JSON=""
for c in jscpd-report/jscpd-report.json jscpd-report.json quality-inputs/jscpd-report/jscpd-report.json quality-inputs/jscpd-report.json; do
  if [ -f "$c" ]; then DUP_JSON="$c"; break; fi
done

THRESHOLD=$(jq -r '.threshold' .jscpd.json 2>/dev/null || true)
if [ -z "$THRESHOLD" ] || [ "$THRESHOLD" = "null" ]; then THRESHOLD="?"; fi

DUP_SECTION="N/A（jscpd metrics json 缺失）"
if [ -n "$DUP_JSON" ]; then
  dup_pct=$(jq -r '.statistics.total.percentage' "$DUP_JSON" 2>/dev/null || true)
  dup_clones=$(jq -r '.statistics.total.clones' "$DUP_JSON" 2>/dev/null || true)
  dup_lines=$(jq -r '.statistics.total.duplicatedLines' "$DUP_JSON" 2>/dev/null || true)
  all_lines=$(jq -r '.statistics.total.lines' "$DUP_JSON" 2>/dev/null || true)
  if [ -n "$dup_pct" ] && [ "$dup_pct" != "null" ]; then
    dup_fmt=$(awk -v p="$dup_pct" 'BEGIN { printf "%.2f", p }')
    verdict=$(awk -v p="$dup_pct" -v t="$THRESHOLD" 'BEGIN { print (p + 0 <= t + 0) ? "✅ 未超阈值" : "❌ 超过阈值" }')
    DUP_SECTION=$(printf '**%s%%** %s（threshold %s%%，来自 .jscpd.json；%s clones，%s/%s 行）' "$dup_fmt" "$verdict" "$THRESHOLD" "${dup_clones:-?}" "${dup_lines:-?}" "${all_lines:-?}")
  else
    DUP_SECTION="N/A（metrics json 解析失败）"
  fi
fi

# ---------- 门禁矩阵 ----------
gate_row() {
  case "$2" in
    success)   printf '| %s | ✅ success |\n' "$1" ;;
    failure)   printf '| %s | ❌ failure |\n' "$1" ;;
    cancelled) printf '| %s | ⛔ cancelled |\n' "$1" ;;
    skipped)   printf '| %s | ⏭ skipped |\n' "$1" ;;
    *)         printf '| %s | ❔ %s |\n' "$1" "$2" ;;
  esac
}
MATRIX=$(gate_row "Lint" "$LINT_RESULT")$'\n'
MATRIX+="$(gate_row "Build" "$BUILD_RESULT")"$'\n'
MATRIX+="$(gate_row "Test" "$TEST_RESULT")"$'\n'
MATRIX+="$(gate_row "Duplication" "$DUP_RESULT")"$'\n'

# ---------- 文档漂移警告 ----------
if [ -n "$DRIFTS" ]; then
  DRIFT_SECTION=$(printf '**%s 条**（warn 模式：仅告警不阻断合并；运行 `/sync-docs` 可修复）' "$DRIFTS")
else
  DRIFT_SECTION="N/A"
fi

# ---------- Eval 状态（best-effort，查询失败降级不拖垮报告） ----------
EVAL_SECTION=""
if [ -n "$REPO" ] && [ -n "$SHA" ] && command -v gh >/dev/null 2>&1; then
  eval_info=$(gh api "repos/$REPO/actions/runs?head_sha=$SHA" --jq '
    ([.workflow_runs[] | select(.name | test("Eval"))][0])
    | if . == null then "" else ((.conclusion // "unknown") + "|" + .html_url) end' 2>/dev/null) || eval_info=""
  if [ -n "${eval_info:-}" ]; then
    eval_conc="${eval_info%%|*}"
    eval_url="${eval_info#*|}"
    case "$eval_conc" in
      success) eval_badge="✅ success" ;;
      failure) eval_badge="❌ failure" ;;
      *)       eval_badge="➖ $eval_conc" ;;
    esac
    EVAL_SECTION=$(printf '**%s** — [查看 Eval 运行](%s)' "$eval_badge" "$eval_url")
  fi
fi
if [ -z "$EVAL_SECTION" ]; then
  EVAL_SECTION="查询失败或无记录，见 Eval CI（独立 workflow）"
fi

# ---------- 渲染 markdown ----------
run_url="${GITHUB_SERVER_URL:-https://github.com}/$REPO/actions/runs/${GITHUB_RUN_ID:-}"
generated=$(date -u '+%Y-%m-%d %H:%M UTC')
tmp_report=$(mktemp)
tmp_payload=$(mktemp)
trap 'rm -f "$tmp_report" "$tmp_payload"' EXIT

{
  printf '%s\n' "$MARKER"
  printf '# 📊 CI 质量报告\n\n'
  printf '## 门禁矩阵\n\n'
  printf '| 门禁 | 结果 |\n'
  printf '|------|------|\n'
  printf '%s' "$MATRIX"
  printf '\n## 单元测试覆盖率\n\n%s\n\n' "$COVERAGE_SECTION"
  printf '## 代码重复率（jscpd）\n\n%s\n\n' "$DUP_SECTION"
  printf '## 文档漂移警告\n\n%s\n\n' "$DRIFT_SECTION"
  printf '## Eval\n\n%s\n\n' "$EVAL_SECTION"
  if [ -n "$SHA" ]; then
    printf -- '---\n*自动生成于 %s · commit `%s`' "$generated" "${SHA:0:7}"
    if [ -n "$REPO" ]; then printf ' · [查看 CI 运行](%s)*\n' "$run_url"; else printf '*\n'; fi
  else
    printf -- '---\n*自动生成于 %s*\n' "$generated"
  fi
} > "$tmp_report"

if [ "${DRY_RUN:-0}" = "1" ]; then
  cat "$tmp_report"
  exit 0
fi

# ---------- sticky 评论（marker 识别，有则 PATCH 无则 POST） ----------
if [ -z "$REPO" ] || [ -z "$PR_NUMBER" ]; then
  echo "quality-report: 缺少 REPO 或 PR_NUMBER，无法发布评论" >&2
  exit 1
fi

jq -n --arg body "$(cat "$tmp_report")" '{body: $body}' > "$tmp_payload"

comments_json=$(gh api "repos/$REPO/issues/$PR_NUMBER/comments?per_page=100" 2>/dev/null) || comments_json="[]"
comment_id=$(printf '%s' "$comments_json" | jq -r --arg m "$MARKER" '[.[] | select(.body | startswith($m))][0].id // empty' 2>/dev/null || true)

if [ -n "$comment_id" ]; then
  # 注意：更新评论必须用 /issues/comments/{id} 直连形式——
  # /issues/{n}/comments/{id} 路由不支持 PATCH，返回 404
  gh api -X PATCH "repos/$REPO/issues/comments/$comment_id" --input "$tmp_payload" >/dev/null
  echo "quality-report: 已更新 sticky 评论（id=${comment_id}）"
else
  gh api -X POST "repos/$REPO/issues/$PR_NUMBER/comments" --input "$tmp_payload" >/dev/null
  echo "quality-report: 已创建 sticky 评论"
fi
