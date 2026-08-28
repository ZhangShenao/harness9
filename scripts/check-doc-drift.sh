#!/usr/bin/env bash
#
# check-doc-drift.sh — 代码模块与技术方案文档的同步漂移检测
#
# 用法:
#   scripts/check-doc-drift.sh [<base-ref>]   # 默认比较 master...HEAD
#   DOC_DRIFT_STRICT=1 时漂移导致退出码 1（CI 阻断），默认仅警告
#
# 依赖: git, jq
# 退出码: 0 通过（或 warn 模式下有漂移） / 1 strict 模式漂移 / 2 环境错误
set -euo pipefail

MAP_FILE="docs/doc-map.json"
BASE="${1:-}"
STRICT="${DOC_DRIFT_STRICT:-0}"

command -v jq >/dev/null 2>&1 || { echo "check-doc-drift: 缺少 jq 依赖" >&2; exit 2; }
[ -f "$MAP_FILE" ] || { echo "check-doc-drift: 未找到 $MAP_FILE" >&2; exit 2; }

if [ -n "$BASE" ]; then
  CHANGED=$(git -c core.quotepath=off diff --name-only "$BASE...HEAD")
else
  CHANGED=$(git -c core.quotepath=off diff --name-only master...HEAD 2>/dev/null || git -c core.quotepath=off diff --name-only HEAD)
fi

if [ -z "$CHANGED" ]; then
  echo "check-doc-drift: 无变更文件，通过"
  exit 0
fi

DRIFT=0
while IFS= read -r entry; do
  DOCS=$(jq -r 'if (.docs | length) == 0 then empty else .docs[] end' <<<"$entry")
  [ -z "$DOCS" ] && continue

  # 该映射的任一 path 是否命中非测试代码变更
  TOUCHED=0
  while IFS= read -r pat; do
    while IFS= read -r f; do
      case "$f" in *_test.go) continue ;; esac
      # shellcheck disable=SC2254
      case "$f" in $pat) TOUCHED=1; break 2 ;; esac
      # 目录前缀匹配：pattern 为目录时命中其下所有文件（引号关闭 glob，仅保留显式 / *）
      case "$f" in "$pat"/*) TOUCHED=1; break 2 ;; esac
    done <<<"$CHANGED"
  done <<<"$(jq -r '.paths[]' <<<"$entry")"
  [ "$TOUCHED" -eq 1 ] || continue

  # 所有映射文档是否都出现在本次变更中
  SYNCED=1
  while IFS= read -r doc; do
    grep -Fxq "$doc" <<<"$CHANGED" || { SYNCED=0; break; }
  done <<<"$DOCS"
  [ "$SYNCED" -eq 1 ] && continue

  while IFS= read -r doc; do
    echo "DRIFT: 代码已变更但文档未同步 -> $doc"
  done <<<"$DOCS"
  DRIFT=1
done < <(jq -c '.[]' "$MAP_FILE")

if [ "$DRIFT" -eq 0 ]; then
  echo "check-doc-drift: 文档同步检查通过"
  exit 0
fi

if [ "$STRICT" = "1" ]; then
  echo "::error::文档漂移检测未通过（strict 模式）: 请更新对应文档后重提" >&2
  exit 1
fi
echo "::warning::检测到文档漂移（warn 模式，不阻断合并）"
exit 0
