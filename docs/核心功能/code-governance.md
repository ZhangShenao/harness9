# 代码治理（Code Governance）

harness9 的代码治理由**三项自动化质量门禁**（jscpd 重复度、autocorrect + typos 文案格式化、check-doc-drift 文档漂移检测）与**一条文档同步管线**（/sync-docs）构成。目标只有一个：让「代码、注释、文档」三者不漂移——代码改了注释跟着改，注释改了文档跟着改，而这一切由 CI 强制兜底，不依赖个人自觉。

```
重复度门禁   jscpd                → 代码重复度不恶化（threshold 门禁）
文案格式化   autocorrect + typos  → 注释与文档格式统一、拼写正确
漂移检测     check-doc-drift.sh   → 代码变更必须携带文档变更
同步管线     /sync-docs           → 代码 → 中文文档 → 英文镜像的落地执行手段
```

三个门禁与既有 lint（gofmt / goimports / go vet）一起跑在 CI 的 `lint` job 中，jscpd 独立占用一个 `duplication` job。本文档描述各自的原理、配置与使用方式。

---

## 重复度门禁

### 检测原理

重复度门禁基于 [jscpd](https://github.com/kucherenko/jscpd)（copy-paste detector），工作流程分三步：

1. **token 化**：按语言语法（本项目为 Go）将源码解析成统一的 token 流，剥离空白与格式差异，只保留语义单元；
2. **相似片段窗口匹配**：在 token 流上做滑动窗口哈希匹配，连续片段长度 ≥ `minTokens` 且行数 ≥ `minLines` 的两段相同代码被记为一个 clone；
3. **重复行百分比**：统计所有 clone 覆盖的行数占总行数的比例，与 `threshold` 比较——超过即非零退出。

### 门禁配置

配置位于仓库根目录 `.jscpd.json`，CI 与本地 `npx jscpd@5.0.16 .` 共用这一份：

| 字段 | 值 | 含义 |
|------|-----|------|
| `path` | `["internal", "cmd"]` | 只扫描生产代码目录，不动 skills、docs 等 |
| `formats` | `["go"]` | 只检查 Go 源码 |
| `minTokens` | `50` | 片段至少 50 个 token 才计为 clone（过滤无意义的短重复） |
| `minLines` | `5` | 且至少 5 行 |
| `threshold` | `5` | 重复行百分比上限，超过则 exit 1（CI 阻断） |
| `reporters` | `["consoleFull", "json"]` | 控制台输出完整报告 + 生成 JSON 报告（质量报告的数据源） |
| `output` | `jscpd-report` | JSON 报告落盘目录（实测产物为 `jscpd-report/jscpd-report.json`） |
| `gitignore` | `true` | 遵循 `.gitignore`，生成产物自动排除 |
| `ignore` | `["**/*_test.go"]` | 文件级排除测试代码 |

`ignore` 排除测试文件是**数据驱动**的决策：实测中约 86% 的重复行来自 `*_test.go`（表驱动测试的 case 表与 setup 样板属合理的测试样板重复，并非生产代码坏味道），排除后才能让门禁聚焦真正需要关注的实现代码。

### 先测量，再定阈值

阈值不是拍脑袋定的。门禁落地前先对存量代码做基线测量（jscpd 5.0.16，`internal/` + `cmd/` 全量口径）：

| 口径 | 文件数 | 总行数 | Clones | Duplicated lines |
|------|-------|--------|--------|------------------|
| 全量（internal + cmd） | 209 | 36365 | 87 | 754（**2.07%**） |
| 排除 `*_test.go`（门禁实际观测口径） | 110 | 19396 | 13 | 107（0.55%） |

threshold 按固定公式计算：

```
threshold = ceil(基线重复行百分比) + 2
          = ceil(2.07) + 2
          = 5
```

「基线 + 2」的策略意味着：**只防恶化，不炸存量**。存量代码里的合理重复不会被误伤，任何人往仓库里塞大段复制粘贴代码时门禁才会亮红灯。随着重构推进，基线下降后可同步下调 threshold，逐步收紧。

### CI 中的行为

`duplication` job 每次跑 `npx jscpd@5.0.16 .`（版本显式锁定，不用 `@latest`）：

```yaml
- name: Run jscpd
  shell: bash
  run: set -o pipefail; npx jscpd@5.0.16 . 2>&1 | tee jscpd-output.txt

- name: Report summary
  if: always()
  run: |
    echo "## Duplication Report" >> $GITHUB_STEP_SUMMARY
    grep -E "Clone|duplicated" jscpd-output.txt | head -50 >> $GITHUB_STEP_SUMMARY || true
```

两个细节：`set -o pipefail` 保证 jscpd 超阈值的非零退出码能穿透 `tee` 使 job 失败；`if: always()` 保证即使超阈值失败，重复度报告仍会写入 PR 的 Step Summary，方便直接查看 clone 列表定位问题。

---

## 文档漂移检测

### 映射表

`docs/doc-map.json` 是代码模块与技术方案文档之间的映射表，当前共 **23 条**条目，每条结构为：

```json
{ "paths": ["cmd/harness9/tui.go", "cmd/harness9/tui_*.go"], "docs": ["docs/核心功能/tui.md"] }
```

- `paths`：glob 模式数组，匹配该模块的源码路径（如 `cmd/harness9/tui_*.go`，或直接给目录 `internal/engine`）；
- `docs`：需要与之同步的中文文档列表；
- `docs` 为空数组 `[]` 表示该模块暂无对应文档，**仅登记、不检查**——这是「先登记模块、后补文档」的渐进路径，避免因为暂时没有文档而无法登记模块。

### 检测算法

`scripts/check-doc-drift.sh` 的检测流程：

1. **取变更集**：`git diff --name-only <base>...HEAD`（默认 `origin/master...HEAD`，依次回退 `master...HEAD`、HEAD 工作区；也可显式传 base-ref）；`git -c core.quotepath=off` 保证 `docs/核心功能/` 等中文路径不被转义，路径比对不会因为引号转义而失配；
2. **测试文件豁免**：`*_test.go` 的变更不触发文档检查——测试内部调整（case 增删、断言微调）不要求文档跟着动；
3. **路径匹配**：变更文件依次与映射条目的 `paths` 做 glob 匹配；pattern 为目录时按目录前缀匹配，命中其下所有文件；
4. **文档同步核对**：某模块被命中后，其 `docs` 列表中的**所有**文档必须出现在本次变更集中，否则报 `DRIFT: 代码已变更但文档未同步`。

### 退出码与 warn/strict 分级

| 退出码 | 含义 |
|-------|------|
| `0` | 通过；或 warn 模式下存在漂移（仅告警，不阻断） |
| `1` | strict 模式下存在漂移（CI 阻断） |
| `2` | 环境错误（缺少 `jq` 依赖、`doc-map.json` 缺失） |

检测分两级：设 `DOC_DRIFT_STRICT=1` 时漂移导致退出码 1（阻断合并）；默认 warn 模式只输出 `::warning::` 不阻断。

分级是刻意的演进策略：**先 warn 观察期，再 strict 反转默认值**。映射表刚建立时难免有疏漏（某些代码变更确实无需文档更新），直接 strict 会制造大量误报、逼人绕过门禁；先以 warn 模式观察映射表的准确率，确认无误报后再把 CI 中的默认值反转为 1。当前 CI 中显式设置 `DOC_DRIFT_STRICT: "0"`，处于观察期。

### 与 CI 的接入点

漂移检测跑在 `lint` job 中：PR 触发时以 `origin/${{ github.base_ref }}` 为 base 比对（即「这个 PR 相对目标分支改了什么」）；push 到 master 时显式与 `origin/master` 比对——此时 HEAD 已是最新提交，diff 为空，检查显式通过（真正的门禁发生在 PR 阶段）。

---

## 注释与文档格式化

### autocorrect：中英文混排格式

[autocorrect](https://github.com/huacnlee/autocorrect) 按文件类型解析源码，**识别出注释区与文案区**（如 Go 的 `//` 注释、Markdown 正文），只处理注释、字符串与正文内容，不碰代码逻辑——格式化永远不会改变程序行为。

核心规则两类：

- **CJK 空格规则**：中文与英文单词、数字、行内代码之间补空格（`配置位于仓库根目录.jscpd.json` → `配置位于仓库根目录 .jscpd.json`）；
- **全角/半角标点规则**：中文语境下的标点转全角、英文语境转半角、全角字母数字转半角。

配置位于 `.autocorrectrc`，关键项：

| 规则 | 级别 | 说明 |
|------|------|------|
| `space-word` / `space-punctuation` / `space-bracket` / `space-backticks` | 1 | CJK 与单词/标点/括号/反引号之间补空格 |
| `fullwidth` / `no-space-fullwidth` | 1 | CJK 语境标点转全角，全角标点附近去空格 |
| `halfwidth-word` / `halfwidth-punctuation` | 1 | 全角字母数字与英文语境标点转半角 |
| `spellcheck` | 0 | **拼写检查关闭**——由 typos 专职负责，避免两套词典互相打架 |
| `context.codeblock` | 1 | Markdown 代码块内容不纠正（代码就是代码） |

`.autocorrectignore` 排除非文案资产：`.worktrees/`（嵌套 worktree 完整副本）、`benchmarks/`、`swebench-baseline-v1/`、`test_fixtures/`、`*.svg`、`*.png`。

### typos：拼写检测与白名单

[typos](https://github.com/crate-ci/typos) 基于内置词典检测源码与文档中的英文拼写错误，覆盖注释、字符串与标识符。误报通过 `_typos.toml` 白名单豁免，当前白名单：

```toml
[default]
extend-ignore-identifiers-re = [
  "UDDG",       # DuckDuckGo 加密 URL 参数名（decodeUDDG）
  "harness9",
  "Ratatui",    # Rust TUI 框架名，非 Ratatouille 拼写错误
  "HASS",       # Home Assistant 令牌前缀（HASS_TOKEN）
  "provid",     # AGENTS.md 架构图 ASCII 盒子里 provider 的有意截断
]

[default.extend-words]
harness9 = "harness9"
```

白名单策略是「**随实际误报逐步补充**」：不预先罗列猜测性条目，每次 CI 撞上误报就把专有名词加进来，白名单始终与项目实际术语表一致。

### 换行策略决策：不做 prose wrap

autocorrect 与多数英文 lint 工具不同，**不启用自动硬折行**（prose wrap）。原因：中文文档若按固定列宽硬折行，一句话的局部修改会导致整段重新折行，git diff 产生大段噪音，review 与 blame 全部失效。段落按语义自然成行，行长度交给编辑器软换行处理。

### 已知局限

- typos 的词典机制只能检测**英文**拼写错误；中文错别字（如「的地得」、形近字）目前没有成熟的开源检测方案，只能靠 review 兜底；
- autocorrect 修正的是「格式」而非「内容」，语法与表述问题不在其管辖范围。

---

## /sync-docs 三阶段管线

漂移检测只能告诉你「文档没动」，不能替你「把文档改对」。落地执行靠 `/sync-docs` 命令（`.opencode/commands/sync-docs.md`），一次运行覆盖三阶段：

### Phase 1：代码变更 → 中文文档

1. 确定变更范围（用户指定 / 工作区未提交改动 / `git diff --name-only origin/master...HEAD`，回退 `master...HEAD`、工作区），过滤掉 `*_test.go` 与生成产物；
2. 以 `docs/doc-map.json` 为准，`paths` 命中且 `docs` 非空的条目进入候选清单；
3. 对每个候选文档：通读全文 + 相关代码 diff，直接更新过时描述、新增功能段落、失效的代码片段；无实质影响的变更可跳过但需说明理由。

### Phase 2：中文 → 英文镜像

1. **扫描**：逐个比对 `docs/核心功能/*.md` 与 `docs/core-features-en/` 同名文件的存在性与修改时间，分类为 MISSING（英文缺失，必须创建）/ STALE（英文过期，需审查更新）/ OK；
2. **创建 MISSING**：英文版按**独立编写**原则撰写——不是逐句机翻，而是用英文技术写作惯例重新组织；但结构对齐（章节、表格、代码块一致），代码块与 CLI 示例保持原样；
3. **更新 STALE**：逐节比对中英文版本，中文新增的章节补英文、修改的对齐、删除的移除；
4. **复核**：重跑扫描，要求零 MISSING、零 STALE。

### Phase 3：收尾报告

统一输出两个阶段的更新清单（每份文档列出更新要点）、跳过理由、需人工确认的事项，以及两个目录的文档总数（应一致）。

### 命令与 CI 的关系

两者是**互补**而非重复：`check-doc-drift.sh` 是机械门槛，保证「文档动没动」可被 CI 客观测证；`/sync-docs` 是 LLM 辅助的落地手段，解决「动得对不对」的内容问题。工作流上的约定：改完代码先跑 `/sync-docs` 补文档，提 PR 时 CI 的漂移检测自然通过。

---

## 本地使用

### 工具安装

```bash
# autocorrect（macOS 用 Homebrew，或 Rust 工具链）
brew install autocorrect
cargo install autocorrect-cli

# typos
brew install typos-cli
cargo install typos-cli

# jscpd 无需安装，npx 直接运行
```

### 常用命令速查

| 命令 | 作用 |
|------|------|
| `autocorrect --fix .` | 全仓自动修复文案格式（提交前跑一次） |
| `autocorrect --lint` | CI 同款格式检查，只报不改 |
| `typos` | 全仓拼写检查 |
| `typos -w` | 自动修复拼写错误（改动前建议先 `git diff` 确认） |
| `npx jscpd@5.0.16 .` | 本地重复度报告（读取 `.jscpd.json`） |
| `scripts/check-doc-drift.sh` | 文档漂移检测，默认 warn 模式 |
| `DOC_DRIFT_STRICT=1 scripts/check-doc-drift.sh` | strict 模式，漂移时退出码 1 |
| `/sync-docs` | opencode 命令：三阶段文档同步管线 |

CI 中的 jscpd 已锁定为同一版本（`npx jscpd@5.0.16`）；升级 jscpd 时应同步更新 CI 与本处的版本号，并复测重复度基线。

推荐的个人工作流：提交前 `autocorrect --fix .` + `typos` 清理自己的改动范围，涉及 `internal/`、`cmd/`、`skills/` 的变更先跑 `/sync-docs`。

---

## CI 集成总览

`.github/workflows/ci.yml` 中与本主题相关的检查项全景：

| Job | 检查项 | 实现 | 失败行为 |
|-----|--------|------|---------|
| lint | gofmt | `gofmt -l .` 输出非空即失败 | 阻断 |
| lint | goimports | `goimports -l .`（v0.33.0） | 阻断 |
| lint | 静态检查 | `go vet ./...` | 阻断 |
| lint | 文案格式 | `huacnlee/autocorrect-action@main` | 阻断 |
| lint | 拼写 | `crate-ci/typos@v1.49.1` | 阻断 |
| lint | 文档漂移 | `scripts/check-doc-drift.sh`（`DOC_DRIFT_STRICT: "0"`） | warn 告警，不阻断 |
| duplication | 代码重复度 | `npx jscpd@5.0.16 .`（threshold 5） | 阻断 + Step Summary 报告 |
| build | 编译 | `go build ./...` | 阻断 |
| test | 测试 | `go test -race ./...`（附覆盖率采集） | 阻断 |
| quality-report | 质量报告 | `scripts/quality-report.sh`（sticky 评论） | 不阻断，见下节 |

### CI 质量报告

`quality-report` job 在 PR 的四个门禁 job（lint / build / test / duplication）全部结束后运行（`if: always()`，部分失败也会出报告；push 到 master 不触发）。它把分散在各 job 的质量信号聚合成一条 markdown 评论，发到 PR 内并**持续更新同一条**：

| 指标 | 来源 | 说明 |
|------|------|------|
| 门禁矩阵 | 四个 job 的 result | 每个门禁一行，✅ success / ❌ failure / ⏭ skipped 一目了然 |
| 单元测试覆盖率 | `coverage.out`（test job artifact） | 总覆盖率 + 按包聚合明细表（升序）；**当前为参考值，不设阈值门禁** |
| 代码重复率 | jscpd metrics json（duplication job artifact） | 百分比与 threshold 的对比、clone 数与重复行数 |
| 文档漂移警告数 | lint job 的 `drifts` 输出 | warn 模式计数，提示用 `/sync-docs` 修复 |
| Eval 状态 | `gh api` 按 head SHA 查 Eval CI run（best-effort） | 结论徽标 + 运行链接；查询失败降级为一行提示，不影响报告 |

更新机制依赖评论开头的 HTML marker（`<!-- harness9-ci-quality-report -->`）：脚本先列出 PR 评论，按 marker 找到既有报告则 PATCH 更新，找不到才 POST 创建——同一个 PR 反复 push 只会有一条报告评论，不会刷屏。

数据管线：test job 把 `coverage.out`、duplication job 把 jscpd metrics json 分别作为 artifact 上传，`quality-report` job 通过 `actions/download-artifact` 拉取后交给 `scripts/quality-report.sh` 渲染。两个产物都已加入 `.gitignore`。脚本内部所有 best-effort 查询单独容错，只有评论操作本身失败才会导致 job 失败；评论 payload 用 `jq -n --arg` 构造，杜绝注入。

后续演进方向：漂移检测结束 warn 观察期后将 `DOC_DRIFT_STRICT` 反转为 `1`；threshold 随基线下降逐步收紧；覆盖率观察稳定后可评估是否引入阈值。三者都只改配置值，不动结构。
