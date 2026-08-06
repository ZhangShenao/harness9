// 迁移自 Claude Code 的 PostToolUse Hook（matcher: Write|Edit）
// 文件写入/编辑后，把重要 Markdown 文档同步到 Obsidian Workspace（scripts/sync-to-obsidian.sh）
export const SyncToObsidian = async ({ directory, $ }) => {
  return {
    "tool.execute.after": async (input, output) => {
      if (input.tool !== "write" && input.tool !== "edit") return

      const filePath = output?.args?.filePath
      if (!filePath) return

      await $`${directory}/scripts/sync-to-obsidian.sh ${filePath}`
    },
  }
}
