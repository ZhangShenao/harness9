#!/bin/bash
# SWE-bench 官方 eval 镜像批量预拉取（P0-2，v4 评分基础设施）。
#
# 背景：官方镜像只发布 amd64 manifest，Apple Silicon 上 swebench 的拉取走 docker-py、
# 不读 DOCKER_DEFAULT_PLATFORM，按 arm64 拉必 404；CLI `docker pull --platform linux/amd64`
# 预拉进本地缓存后，swebench 发现本地镜像即跳过拉取，评分方可进行。
#
# 用法：
#   ./pull-images.sh <predictions.jsonl> [并发数，默认 6]
#
# 实测工程参数（2026-09-09/10，Apple Silicon + Docker Desktop）：
#   - 并发上限 9：12 路会触发 daemon 500 错误风暴并回滚已拉镜像；
#   - 大镜像（matplotlib 系）易遇下载连接僵死：日志停滞 >5 分钟时杀掉对应 pull 进程
#     换新连接重试是唯一有效恢复手段（退避等待无效）；
#   - 单镜像最多重试 5 次，每次重试间 sleep 递增。
set -u

PRED_FILE="${1:?用法: pull-images.sh <predictions.jsonl> [并发数]}"
P="${2:-6}"
LOG=/tmp/swebench-pull.log

command -v docker >/dev/null || { echo "docker 不可用" >&2; exit 1; }
[ -f "$PRED_FILE" ] || { echo "predictions 文件不存在: $PRED_FILE" >&2; exit 1; }

# 从 predictions.jsonl 提取有 patch 的实例，按 swebench 镜像命名规则生成清单：
# swebench/sweb.eval.x86_64.{instance_id 小写，__ → _1776_}
LIST=$(python3 - "$PRED_FILE" <<'EOF'
import json, sys
for line in open(sys.argv[1]):
    d = json.loads(line)
    if d.get("model_patch"):
        print("swebench/sweb.eval.x86_64." + d["instance_id"].lower().replace("__", "_1776_"))
EOF
)
TOTAL=$(echo "$LIST" | grep -c .)
echo "目标镜像 $TOTAL 个，并发 $P，日志 $LOG" | tee -a "$LOG"

pull_one() {
  img="$1"
  docker image inspect "$img:latest" >/dev/null 2>&1 && { echo "SKIP $img"; return 0; }
  for attempt in 1 2 3 4 5; do
    docker pull --platform linux/amd64 "$img:latest" >/dev/null 2>&1 && { echo "OK $img"; return 0; }
    sleep $((attempt * 30))
  done
  echo "FAIL $img"
  return 1
}
export -f pull_one

HAVE=$(mktemp)
docker images --format '{{.Repository}}' | grep "sweb.eval" | sed 's/:latest$//' | sort > "$HAVE"
echo "$LIST" | grep -vxFf "$HAVE" | xargs -P "$P" -I{} bash -c 'pull_one "$@"' _ {}

docker images --format '{{.Repository}}' | grep -c "sweb.eval" | xargs -I{} echo "完成：本地 eval 镜像 {}/$TOTAL" | tee -a "$LOG"
