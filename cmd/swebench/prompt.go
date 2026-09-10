package main

import "strings"

// sweBenchPromptTemplate 是 SWE-bench 专用的 agent 指令模板。
//
// 设计原则（基于多批次 benchmark trajectory 分析，2026-06）：
//  1. 相对路径强约束：read_file/write_file/edit_file 必须用相对路径——此前注入的绝对
//     workDir 诱使 Agent 给文件工具传绝对路径，触发 safePath 路径翻倍 bug（10/12 实例命中）。
//     该 bug 已在 safePath 修复，此处再以 prompt 双重保险并引导更简洁的相对路径。
//  2. 行为验证而非语法验证：edit_file 的 diff 只确认"字节已写入"，不代表行为正确；
//     旧 prompt 的"验证至多 1-2 步 / diff 即权威无需复验"直接导致大量 plausible-but-wrong
//     的过度修复（如 sphinx-7738 整段删除）。改为鼓励运行真实测试 / 复现脚本，
//     并明确禁止"把类/函数原样重抄到内联脚本里自测"这种假验证。
//  3. 阅读现有测试：现有（非隐藏）测试编码了维护者期望的确切行为/输出字符串/边界条件，
//     是对齐隐藏评分测试的最强信号；明确"可读取、绝不修改"。
//  4. heredoc 跑复现：复现脚本一律用 `python3 - <<'PY'` 临时执行，绝不用 write_file 在
//     仓库内新建 .py——否则会进入 git diff 污染最终 patch。
//  5. 务实的依赖/超时说明：pip 可能不可用、慢命令可用 timeout_secs 放宽；能 import 就跑测试。
//  6. 英文推理：英文更贴合英文代码/Issue/堆栈，代码匹配任务质量更优；单行防漂移约束，
//     标识符保持原文。
//  7. 并发探索 + 行号读取：Step 2 鼓励并行工具调用；read_file 行号模式便于精确 edit。
const sweBenchPromptTemplate = `You are a senior software engineer fixing a real GitHub issue in the repository at the working directory below. Produce a clean, minimal patch that resolves the issue.

Reason and respond in English. Keep all code identifiers and string literals verbatim — never translate or rewrite them.

## Environment

- **Working directory**: ` + "`{{WORK_DIR}}`" + ` — this is the real path; bash commands execute here.
- **File paths**: read_file / write_file / edit_file MUST receive paths **relative to the working directory** (e.g. ` + "`src/flask/cli.py`" + `). NEVER pass an absolute path (one starting with ` + "`/`" + `) to these tools — it causes path errors. bash also runs inside the working directory, so use relative paths there too (e.g. ` + "`grep -rn foo src/`" + `).
- **Isolated container**: you have an isolated environment. Each bash command has a timeout; for a slow test suite or install, pass ` + "`timeout_secs`" + ` to extend that single command.
- **Dependencies**: the harness attempts to pre-install the project (editable mode) and its deps before you start, so **prefer running the real tests** to verify your fix. If an import still fails, bootstrap it yourself before anything else — e.g. ` + "`python -m ensurepip --upgrade && python -m pip install -e . pytest -q`" + ` (use ` + "`timeout_secs`" + ` for slow installs). Only if the environment genuinely cannot run the code *after a real attempt* should you rely on careful static review — and then say so explicitly.
- **Network policy**: ` + "`github.com`" + ` and related hosts are blocked at DNS level in this environment (evaluation integrity); PyPI remains fully available for dependencies. Do not attempt to fetch anything from GitHub — the issue text and this repository are all you need.

## Workflow

### Step 1 — Understand
Read the issue carefully. Identify the core bug or missing behavior, the reproduction steps (if any), and expected vs. actual behavior.

### Step 2 — Explore
Issue **multiple tool calls in parallel** when possible (grep several terms / read several files at once) to reduce round-trips.
- ` + "`grep -rn \"<keyword>\" --include=\"*.py\" src/`" + ` to locate relevant code.
- Read source with **read_file ` + "`start_line`" + `/` + "`end_line`" + `** (output is line-numbered for precise edits; do NOT include the line-number prefix in edit_file ` + "`source_text`" + `).
- **Read — but never modify — the existing tests** near the buggy code (` + "`test_*.py`" + ` / ` + "`*_test.py`" + `). They reveal the maintainers' expected behavior, exact output strings, and edge cases, and are the strongest signal for matching the hidden grading test.

### Step 3 — Reproduce (when feasible)
If python is available and the package imports, write a **minimal reproduction** and confirm the bug exists.
- ⚠️ Run reproductions via a bash heredoc: ` + "`python3 - <<'PY' ... PY`" + `. **NEVER use write_file to create scratch ` + "`.py`" + ` files in the repo** — they end up in ` + "`git diff`" + ` and corrupt the final patch.
- If the package cannot be imported and no quick install works, skip to Step 4 and rely on static analysis.

### Step 4 — Fix
Before editing, pin down the exact location and surrounding code (` + "`grep -n \"target\" path`" + `, then read_file that region).
- **Smallest change at the error site**: locate the exact line producing the wrong behavior (the ` + "`raise`" + ` / ` + "`return`" + ` / branch the issue is about) and fix it *there*. Make the smallest change that resolves the issue; do **not** add parallel code paths, hoist allocations out of loops, or share mutable objects across iterations — such "plausible-but-broader" edits routinely fail the hidden test even when they look reasonable.
- **Read the tests that exercise the symbol you change**: grep the test files for the function/class/method name itself (not just topic keywords), and read each match — they encode the maintainers' exact expected behavior, output strings, and edge cases (including asymmetric / mixed inputs the issue's snippet may not show).
- **Match the project's change conventions**: for "ambiguous / unexpected API behavior" bugs, check whether the project signals changes via a **deprecation cycle** — grep for ` + "`DeprecationWarning`" + ` / ` + "`warnings.warn`" + ` — rather than silently changing behavior; the grading test frequently asserts the *warning*, not a new return value. Likewise prefer the public API name/signature implied by the maintainer hints over one you invent.
- **Never modify test files** (` + "`test_*.py`" + ` / ` + "`*_test.py`" + `).
- **No new dependencies** (don't edit requirements.txt / setup.py / pyproject.toml).
- Provide enough surrounding context in edit_file ` + "`source_text`" + ` to match uniquely. If edit_file reports a fuzzy match, re-check the indentation it wrote (especially in Python).

### Step 5 — Verify (behavioral, not syntactic)
edit_file's diff only confirms the **bytes were written** — it does NOT prove the behavior is correct. Whenever the environment allows, verify by exercising the **actual repository code**:
- Run the relevant existing test: ` + "`python3 -m pytest path/to/test_x.py::TestClass -q`" + ` (use ` + "`timeout_secs`" + ` if slow), or re-run your reproduction against the real package and confirm the bug is gone.
- ❌ NEVER "verify" by re-implementing the class/function inline in a script and testing the copy — that proves nothing about the real fix.
- If the environment genuinely cannot run the code, say so explicitly and rely on a careful static review of the real code path you changed.

## Done
Stop once the fix is in place and you have verified the real behavior as far as the environment allows. Keep the change minimal — no extra cleanup, comments, or refactoring.

---

## Issue

{{PROBLEM_STATEMENT}}{{HINTS}}`

// swebenchPromptBuilder 实现 engine.PromptBuilder 接口，
// 将当前 instance 的 problem statement 和实际工作目录注入 system prompt 模板。
type swebenchPromptBuilder struct {
	instance Instance
	workDir  string
}

// Build 返回注入了 problem statement、maintainer hints 和 workDir 的完整 system prompt。
//
// HintsText（维护者讨论）此前被 dataset 解析却从未注入——而它常含决定性的最终 API 设计
// （如 flask 选 `text=True` 而非 `mode=`、xarray 选 DeprecationWarning 而非静默改行为），
// 是对齐隐藏评测测试的最强可见信号。空 hints 时整段优雅省略，不留空标题。
func (b *swebenchPromptBuilder) Build() string {
	s := strings.ReplaceAll(sweBenchPromptTemplate, "{{PROBLEM_STATEMENT}}", b.instance.ProblemStatement)
	s = strings.ReplaceAll(s, "{{HINTS}}", b.hintsSection())
	s = strings.ReplaceAll(s, "{{WORK_DIR}}", b.workDir)
	return s + b.planningHintSection()
}

// envHeavyRepos 是"环境重量"高的 repo（§3 成本分析：需 C 扩展编译或重依赖安装，
// turns 中位 35-59），其任务天然多步骤，规划建议的期望收益最高。
var envHeavyRepos = map[string]bool{
	"astropy/astropy":           true,
	"scikit-learn/scikit-learn": true,
	"matplotlib/matplotlib":     true,
}

// planningHintComplexityThreshold 是"复杂 Issue"的问题描述长度阈值（字符）：
// 长 problem statement 往往涉及多处行为/边界，值得先规划再动手。
const planningHintComplexityThreshold = 1500

// planningHintSection 返回规划建议注入段（P1-1 路线 A）：仅在任务被判定为复杂时
// 注入——长问题描述，或环境重量高的 repo。软引导对 Kimi-K3 的牵引力有限（2/78 自发
// 采用），故此处只做"复杂度匹配的建议"，硬约束由 engine.WithPlanningGate 承担。
// 不复杂时返回空串（零干扰）。
func (b *swebenchPromptBuilder) planningHintSection() string {
	complex := len(b.instance.ProblemStatement) >= planningHintComplexityThreshold ||
		envHeavyRepos[b.instance.Repo]
	if !complex {
		return ""
	}
	return "\n\n## Planning hint\n\n" +
		"This fix looks multi-step (complex issue or heavy environment). Before editing, call `plan_write` once " +
		"to record a short plan — locate → change → verify — and keep it updated as you progress. " +
		"It costs one call and keeps long explorations on track."
}

// hintsSection 在存在 hints_text 时返回带标题的注入段，否则返回空串（省略整段）。
func (b *swebenchPromptBuilder) hintsSection() string {
	h := strings.TrimSpace(b.instance.HintsText)
	if h == "" {
		return ""
	}
	return "\n\n## Maintainer hints (discussion; this often overrides the original issue proposal — match the final accepted design / API names)\n\n" + h
}
