#!/bin/bash
# 将 harness9 项目的重要 Markdown 文档自动同步到 Obsidian Workspace
# 由 Claude Code PostToolUse Hook（stdin 接收工具调用的 JSON payload）
# 或 OpenCode tool.execute.after 插件（$1 直接传入文件路径）触发

set -euo pipefail

OBSIDIAN_VAULT="/Users/zsa/Desktop/workspace/harness9"
PROJECT_ROOT="/Users/zsa/Desktop/harness/harness9"

# 优先使用命令行参数（OpenCode 插件），否则回退到 stdin JSON（Claude Code Hook）
if [[ -n "${1:-}" ]]; then
    FILE_PATH="$1"
else
    INPUT=$(cat)
    FILE_PATH=$(echo "$INPUT" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    print(d.get('tool_input', {}).get('file_path', ''))
except Exception:
    print('')
" 2>/dev/null)
fi

# 无路径则跳过
[[ -z "$FILE_PATH" ]] && exit 0

# 转换为绝对路径
if [[ "$FILE_PATH" != /* ]]; then
    FILE_PATH="$PROJECT_ROOT/$FILE_PATH"
fi

# 只处理 .md 文件
[[ "$FILE_PATH" != *.md ]] && exit 0

# 文件必须存在
[[ ! -f "$FILE_PATH" ]] && exit 0

# 根据路径规则确定同步目标
TARGET=""

if [[ "$FILE_PATH" == "$PROJECT_ROOT/website/blog/"* ]]; then
    # website/blog/<slug>/index.md -> vault/技术博客/<slug>.md
    SLUG=$(basename "$(dirname "$FILE_PATH")")
    FILENAME=$(basename "$FILE_PATH")
    if [[ "$FILENAME" == "index.md" && "$SLUG" != "blog" ]]; then
        TARGET="$OBSIDIAN_VAULT/技术博客/${SLUG}.md"
    else
        TARGET="$OBSIDIAN_VAULT/技术博客/$FILENAME"
    fi

elif [[ "$FILE_PATH" == "$PROJECT_ROOT/docs/"* ]]; then
    # docs/核心功能/ -> vault/核心功能/
    # docs/技术调研/ -> vault/技术调研/
    REL="${FILE_PATH#$PROJECT_ROOT/docs/}"
    TARGET="$OBSIDIAN_VAULT/$REL"

elif [[ "$FILE_PATH" == "$PROJECT_ROOT/knowledge/articles/"* ]]; then
    # knowledge/articles/ -> vault/知识库日报/
    FILENAME=$(basename "$FILE_PATH")
    TARGET="$OBSIDIAN_VAULT/知识库日报/$FILENAME"
fi

# 不在监控范围内则跳过
[[ -z "$TARGET" ]] && exit 0

# 创建目标目录并复制
mkdir -p "$(dirname "$TARGET")"
cp "$FILE_PATH" "$TARGET"
echo "[obsidian-sync] $(date '+%H:%M:%S') synced: ${FILE_PATH#$PROJECT_ROOT/} -> ${TARGET#$OBSIDIAN_VAULT/}"
