package main

// patch 提取与完整性校验。
//
// 背景（2026-09-10 复盘，astropy-14182/14365 评分失败归因）：旧实现用单个 15s context
// 串起 `git add -A -N` 与 `git diff`，且以 `patchOut, _ := CombinedOutput()` 吞掉超时与
// 错误——重产物仓库（如 astropy 的 C 构建文件）下 diff 被超时杀死时，半截输出被静默当作
// model_patch 提交评分，在容器内以 "malformed patch" 全军覆没。本文件将提取收敛为
// collectPatch：add/diff 各自独立超时、显式检查错误、Output 只取 stdout（stderr 不再
// 混入补丁）、失败重试一次，并在提交前用 validateUnifiedDiff 做 hunk 完整性校验——
// 校验不过宁可报错丢弃，也绝不把残缺补丁送进评分。

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	// patchAddTimeout 是 `git add -A -N`（intent-to-add 登记）的独立超时：
	// 需全树扫描登记新文件，重产物仓库下耗时显著高于普通仓库。
	patchAddTimeout = 30 * time.Second
	// patchDiffTimeout 是单次 `git diff` 的独立超时；超时产物必为截断补丁，
	// 将被 validateUnifiedDiff 拒绝并触发重试。
	patchDiffTimeout = 60 * time.Second
)

// gitOut 在独立超时内运行 git 子命令，只返回 stdout（stderr 不与补丁内容混淆）。
func gitOut(tmpDir string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	full := append([]string{"-C", tmpDir}, args...)
	out, err := exec.CommandContext(ctx, "git", full...).Output()
	return string(out), err
}

// collectPatch 收集 agent 在 tmpDir 中的全部改动为 unified diff。
// `git add -A -N`（intent-to-add）使 write_file 新建的文件以新增 hunk 进入 diff，
// 避免被纯 `git diff` 静默丢弃；随后校验完整性，失败重试一次，仍失败则返回错误
// （调用方按实例错误上报，绝不提交残缺补丁）。
func collectPatch(tmpDir string) (string, error) {
	if out, err := gitOut(tmpDir, patchAddTimeout, "add", "-A", "-N"); err != nil {
		return "", fmt.Errorf("git add -A -N 失败: %w: %s", err, strings.TrimSpace(out))
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		out, err := gitOut(tmpDir, patchDiffTimeout, "diff")
		if err != nil {
			lastErr = fmt.Errorf("git diff 失败（第 %d 次）: %w", attempt+1, err)
			continue
		}
		// 校验必须针对原始输出：git diff 的合法输出恒以换行结尾，TrimSpace 会把它
		// 剥掉、令校验器误报截断。
		if verr := validateUnifiedDiff(string(out)); verr != nil {
			lastErr = fmt.Errorf("git diff 输出未通过完整性校验（第 %d 次）: %w", attempt+1, verr)
			continue
		}
		// 返回必须保留原始输出的终止换行：剥掉换行的补丁在 eval 容器里会被
		// git apply 判为 corrupt（"corrupt patch at line N"），2026-09-10 验证轮
		// 3 例 Patch Apply Failed 即此根因（TrimSpace 所致，实弹复现确认）。
		// model_patch 末尾多一个 \n 对 git apply / GNU patch 均无害。
		return string(out), nil
	}
	return "", lastErr
}

// validateUnifiedDiff 校验 git diff 输出的结构完整性，拦截一切截断/污染形态：
//   - 未以换行结尾（git diff 的合法输出必然以换行结尾，缺换行即证明输出流被截断，
//     astropy-14182 即此形态，容器内报 "unexpectedly ends in middle of line"）；
//   - hunk 头声明的行数与正文实际行数不符（astropy-14365 形态，容器内报
//     "malformed patch at line N"）；
//   - hunk 体内混入非 diff 行（CombinedOutput 把 stderr 混进补丁的污染形态）。
//
// 空 patch（无改动）与无 hunk 的输出（纯模式变更、二进制文件提示）均为合法。
func validateUnifiedDiff(patch string) error {
	if patch == "" {
		return nil
	}
	if !strings.HasSuffix(patch, "\n") {
		return fmt.Errorf("diff 未以换行结尾，输出流被截断")
	}
	// 去掉末尾换行产生的空元素；此后 hunk 体内如再遇空行按上下文行计数
	// （兼容行尾空白被剥离的 diff），但不会把"物理不存在"的行误计为上下文。
	lines := strings.Split(patch, "\n")
	lines = lines[:len(lines)-1]

	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "@@ ") {
			continue
		}
		oldN, newN, ok := parseHunkHeader(lines[i])
		if !ok {
			return fmt.Errorf("无法解析 hunk 头：%.60s", lines[i])
		}
		i++
		var oldCnt, newCnt int
		for i < len(lines) && (oldCnt < oldN || newCnt < newN) {
			switch {
			case strings.HasPrefix(lines[i], "\\"): // "\ No newline at end of file"，不计行数
			case strings.HasPrefix(lines[i], "+"):
				newCnt++
			case strings.HasPrefix(lines[i], "-"):
				oldCnt++
			case strings.HasPrefix(lines[i], " "):
				oldCnt++
				newCnt++
			default:
				// git diff 的 hunk 体内只会出现上述四种前缀；其余（stderr 文本等）
				// 即为输出被污染/截断的铁证，直接拒绝而非按上下文行吞掉。
				return fmt.Errorf("hunk 体内出现非 diff 行（疑似输出被污染或截断）: %.60s", lines[i])
			}
			i++
		}
		if oldCnt != oldN || newCnt != newN {
			return fmt.Errorf("hunk 行数不匹配：头声明 -%d +%d，实际 -%d +%d，疑似截断", oldN, newN, oldCnt, newCnt)
		}
		// 行数已满足时 i 停在下一 hunk/文件头或 "\ No newline" 标记上，交回外层循环。
		i--
	}
	return nil
}

// parseHunkHeader 解析 unified diff 的 hunk 头 `@@ -l,s +l,s @@ ...`。
// git 恒输出 start,count 形式，但 count 为 1 时按规范可省略，这里一并兼容。
func parseHunkHeader(line string) (oldN, newN int, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "@@" {
		return 0, 0, false
	}
	oldN, ok = parseRange(fields[1])
	if !ok {
		return 0, 0, false
	}
	newN, ok = parseRange(fields[2])
	if !ok {
		return 0, 0, false
	}
	return oldN, newN, true
}

// parseRange 解析 "-l,s" / "+l,s" 中的行数 s（l,s 形式）或裸行号 l（按 1 行计）。
func parseRange(field string) (int, bool) {
	body := field[1:] // 去掉前导 '-' 或 '+'
	if idx := strings.Index(body, ","); idx >= 0 {
		n, err := strconv.Atoi(body[idx+1:])
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 1, true
}
