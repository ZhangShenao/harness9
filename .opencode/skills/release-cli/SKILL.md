---
name: release-cli
description: 发布 harness9 CLI 新版本。接受可选的 version 参数；若未提供，则自动将当前最新 tag 的 patch 号加 1。执行：切换到 master、拉取最新代码、创建 tag、推送 tag 触发 GoReleaser，并基于两个 tag 之间的提交生成详细、结构化的 Release Note 覆盖 GitHub Release 默认说明。
---

# release-cli — 发布 CLI 新版本

## 概述

将 harness9 CLI 发布为新版本。通过在 `master` 分支上创建版本 tag，触发 GitHub Actions 中的 GoReleaser 工作流，自动构建并发布多平台二进制文件。发布完成后，基于上一版本到本版本之间的全部提交，**生成一份详细、分类清晰的中文 Release Note 并覆盖 GitHub Release 的默认说明**，让每个版本都有完善、可读的发布日志。

## 参数

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `version` | string | 否 | 要发布的版本号（如 `v0.1.5` 或 `0.1.5`）。若未提供，自动取当前最新 tag 的 patch+1 |

## 执行步骤

### 1. 确定版本号

**若用户提供了 `version` 参数：**

```bash
# 规范化：确保以 v 开头
version="v0.1.5"  # 示例，若用户输入 "0.1.5" 则补全为 "v0.1.5"
```

**若用户未提供 `version` 参数：**

```bash
# 获取当前最新 tag（按语义化版本排序）
latest=$(git tag --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1)

# 若无任何 tag，从 v0.0.1 开始
if [ -z "$latest" ]; then
  version="v0.0.1"
else
  # 解析 major.minor.patch，将 patch+1
  # 例：v0.1.4 → v0.1.5
  version=$(echo "$latest" | awk -F'[v.]' '{printf "v%d.%d.%d", $2, $3, $4+1}')
fi

echo "当前最新版本：$latest"
echo "即将发布版本：$version"
```

在继续之前，**向用户展示计算出的版本号**，确认无误。

### 2. 前置检查

```bash
# 检查当前分支
git branch --show-current

# 检查工作区是否干净（有未提交内容时警告）
git status --short
```

**若当前不在 `master` 分支：**

提示用户切换：
```bash
git checkout master
```

**若工作区有未提交的改动：** 询问用户是否继续（未提交内容不影响 tag 发布，但可能意味着遗漏了提交）。

### 3. 拉取最新代码

```bash
git pull origin master
```

确认本地 `master` 与远程同步。

### 4. 确认 tag 不存在

```bash
git tag | grep "^${version}$"
```

若 tag 已存在，**停止执行**，提示用户该版本已发布，建议使用更高版本号。

### 5. 收集本次发布的提交（生成 Release Note 的素材）

在创建新 tag **之前**，确定上一个版本 tag 并收集区间内的全部提交。

```bash
# 上一个版本 tag（当前最新的版本 tag，即本次发布的基准点）
prev_tag=$(git tag --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1)

if [ -z "$prev_tag" ]; then
  # 首个版本：收集全部历史提交
  echo "首个版本，收集全部提交："
  git log --pretty=format:'%h%x09%s' --no-merges
else
  echo "对比区间：${prev_tag} → ${version}"
  echo "提交列表："
  git log "${prev_tag}..HEAD" --pretty=format:'%h%x09%s' --no-merges
  echo
  echo "改动统计："
  git diff --stat "${prev_tag}..HEAD"
fi
```

**把上述提交列表读入上下文**，作为下一步撰写 Release Note 的依据。

### 6. 创建并推送 tag

```bash
# 创建轻量 tag
git tag "${version}"

# 推送 tag 到远程，触发 GitHub Actions release.yml
git push origin "${version}"
```

### 7. 撰写详细的 Release Note

基于第 5 步收集到的提交列表，**撰写一份结构化、详细的中文 Release Note**，写入系统临时目录下的文件 `${TMPDIR:-/tmp}/RELEASE_NOTES_${version}.md`（置于工作区之外，避免污染 `git status` 或被误提交；发布后清理）。

**分类规则**——按 Conventional Commits 前缀归类（无前缀的归入「其他改动」）：

| 前缀 | 分组标题 |
|------|----------|
| `feat` | ✨ 新特性 |
| `fix` | 🐛 问题修复 |
| `perf` | ⚡ 性能优化 |
| `refactor` | ♻️ 重构 |
| `docs` | 📝 文档 |
| `test` | ✅ 测试 |
| `ci` / `build` / `chore` | 🔧 构建与维护 |
| 其他 | 📦 其他改动 |

**Release Note 结构**（务必填充真实内容，禁止留占位符）：

```markdown
# harness9 ${version}

> 一句话概括本次发布的主题（从提交整体提炼，例如「Sub-Agent 通用子代理 + 发布流程增强」）。

## 🌟 本次亮点

- 用 2-4 条要点提炼最重要的用户可感知变化，每条说明「带来了什么价值」，而非简单复述 commit。

## ✨ 新特性
- <commit subject 的可读化描述> (`<hash>`)

## 🐛 问题修复
- ...

## ♻️ 重构 / ⚡ 性能 / 📝 文档 / 🔧 构建与维护
- ...（仅保留实际有内容的分组，空分组直接省略）

## 📥 安装与升级

\`\`\`bash
# 全新安装
curl -fsSL https://raw.githubusercontent.com/ZhangShenao/harness9/master/scripts/install.sh | bash

# 已安装用户升级
harness9 upgrade
\`\`\`

**完整变更**：https://github.com/ZhangShenao/harness9/compare/${prev_tag}...${version}
```

撰写要求：
- 把简略的 commit subject 改写为**面向用户、可读**的条目，必要时合并同一主题的多个 commit。
- 「本次亮点」聚焦用户/开发者能感知的价值，不要逐条搬运 commit。
- 仅保留有实际内容的分组，空分组省略。
- 首个版本（无 `prev_tag`）时省略「完整变更」对比链接，改为列出核心功能总览。

### 8. 等待 Release 创建并覆盖说明

GoReleaser 在 CI 中创建 GitHub Release 后，用第 7 步生成的 Release Note 覆盖其默认说明。

```bash
# 轮询等待 GoReleaser 创建出该 Release（最多约 5 分钟）
notes_file="${TMPDIR:-/tmp}/RELEASE_NOTES_${version}.md"
edited=0
for i in $(seq 1 30); do
  if gh release view "${version}" >/dev/null 2>&1; then
    echo "Release 已创建，写入详细 Release Note…"
    if gh release edit "${version}" --notes-file "${notes_file}"; then
      edited=1
      echo "✅ Release Note 已更新：$(gh release view "${version}" --json url -q .url)"
    fi
    break
  fi
  echo "[$i/30] 等待 GoReleaser 创建 Release…（10s 后重试）"
  sleep 10
done

# 仅在成功覆盖说明后才清理临时文件；超时或失败时保留，供用户手动补救
if [ "${edited}" -eq 1 ]; then
  rm -f "${notes_file}"
else
  echo "⚠ 未能自动覆盖 Release Note，已保留 ${notes_file}"
  echo "  请稍后手动执行：gh release edit \"${version}\" --notes-file \"${notes_file}\""
fi
```

**若超时仍未创建或覆盖失败：** 上面的脚本会**保留** `${notes_file}`（即 `${TMPDIR:-/tmp}/RELEASE_NOTES_${version}.md`），提示用户稍后手动执行
`gh release edit "${version}" --notes-file "${notes_file}"`，或在 GitHub Actions 页面确认构建是否失败。

### 9. 确认发布结果

```bash
# 查看最近的 Actions 运行（需 gh CLI 已登录）
gh run list --limit 3
```

告知用户：
- tag 已推送：`${version}`
- GitHub Actions `release.yml` 已触发（由 `on: push: tags: ['v*']` 驱动）
- GoReleaser 已构建多平台二进制并创建 GitHub Release
- Release Note 已覆盖为详细的分类发布日志，链接见上一步输出
- 可通过 `gh run list` 或 GitHub Actions 页面查看完整构建进度

## 常见错误

| 问题 | 处理 |
|------|------|
| tag 已存在 | 停止执行，建议使用更高版本号 |
| 不在 master 分支 | 提示切换到 master 后再发布 |
| push 被拒绝（无权限） | 检查 git remote 权限，确认有 push 到主仓库的权限 |
| `gh release view` 一直超时 | 保留 Release Note 文件，提示用户检查 Actions 构建是否失败，构建成功后手动 `gh release edit` |
| `gh run list` 无输出 | 提示用户通过 GitHub 网页查看 Actions 执行状态 |
| 无任何历史 tag | 从 `v0.0.1` 开始，Release Note 省略对比链接，改列核心功能总览 |

## 发布流程图

```
确定版本号（用户指定 or patch+1）
    ↓
确认当前在 master 分支
    ↓
git pull origin master（同步最新）
    ↓
确认 tag 不存在
    ↓
收集 prev_tag..HEAD 提交（Release Note 素材）
    ↓
git tag v{version}  →  git push origin v{version}
    ↓
GitHub Actions release.yml 触发 → GoReleaser 构建多平台二进制 + 创建 Release
    ↓
撰写详细分类 Release Note
    ↓
轮询等待 Release 创建 → gh release edit 覆盖说明
    ↓
确认发布结果，清理临时文件
```

## 注意事项

- 发布**必须从 `master` 分支**进行，确保发布的是经过 review 的代码
- 版本号遵循 [SemVer](https://semver.org/)：`vMAJOR.MINOR.PATCH`
- Release Note 用 `gh release edit --notes-file` **覆盖** GoReleaser 自动生成的说明，因此无需修改 `.goreleaser.yaml` 的 changelog 配置
- 覆盖说明需要 `gh` CLI 已登录且对仓库有写权限
- GoReleaser 配置见项目根目录 `.goreleaser.yaml`
- GitHub Actions release 工作流见 `.github/workflows/release.yml`
