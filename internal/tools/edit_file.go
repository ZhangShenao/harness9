// 内置工具：EditFile（文件编辑工具 / File Editing Tool）。
//
// 提供受限工作区（Sandboxed Workspace）内的精确文本替换能力，
// 基于多级模糊匹配算法（Multi-Level Fuzzy Matching）实现：
//
//  1. 沙箱边界（Sandbox Boundary）：所有路径通过 safePath 校验，
//     拒绝路径遍历攻击（Path Traversal Attack）。
//  2. 多级匹配（Multi-Level Matching）：四级容错机制逐级降级，
//     精确匹配 → 换行符归一化 → 整体去空 → 逐行去缩进。
//  3. 唯一性校验（Uniqueness Guard）：所有级别均要求唯一匹配，
//     多匹配时返回明确错误，要求 LLM 提供更多上下文。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/harness9/internal/sandbox"
	"github.com/harness9/internal/schema"
)

// EditFileTool 实现了 BaseTool 接口，提供受限工作区内的精确文本替换能力。
type EditFileTool struct {
	// workDir 沙箱边界（Sandbox Boundary），所有文件操作被限制在此目录内。
	workDir string
	// TODO: 当需要将文件操作路由至容器内执行时，接入 env.ReadFile/WriteFile。
	// 目前文件操作通过 bind mount 在宿主机侧执行，与容器内视图一致，无需路由。
	env sandbox.Environment
}

// EditFileOption 是 EditFileTool 的功能选项函数。
type EditFileOption func(*EditFileTool)

// EditFileWithEnvironment 注入执行环境（当前文件工具通过 bind mount 无需路由，预留扩展）。
func EditFileWithEnvironment(env sandbox.Environment) EditFileOption {
	return func(t *EditFileTool) { t.env = env }
}

// NewEditFileTool 创建绑定到指定工作区的文件编辑工具。
// workDir 会被 filepath.Clean 清洗，确保路径规范化（Path Normalization）。
func NewEditFileTool(workDir string, opts ...EditFileOption) *EditFileTool {
	t := &EditFileTool{workDir: filepath.Clean(workDir)}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Name 返回工具标识符 "edit_file"。
func (t *EditFileTool) Name() string {
	return "edit_file"
}

// Definition 返回工具的元信息，包含描述和 JSON Schema 参数定义。
// LLM 会根据此定义决定何时调用该工具以及如何构造参数。
func (t *EditFileTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        t.Name(),
		Description: "对现有文件进行局部的字符串替换。这比重写整个文件更安全、更快速。请提供足够的 source_text 上下文以确保匹配的唯一性。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "要修改的文件路径，如 src/main.go",
				},
				"source_text": map[string]interface{}{
					"type":        "string",
					"description": "文件中原有的文本。必须包含足够的上下文，以确保在文件中的唯一性。",
				},
				"target_text": map[string]interface{}{
					"type":        "string",
					"description": "要替换成的新文本",
				},
			},
			"required": []string{"path", "source_text", "target_text"},
		},
	}
}

// editFileArgs 定义 edit_file 工具的 JSON 参数结构（Argument Payload），
// 对应 LLM 在 ToolCall 中传递的 Arguments 载荷。
type editFileArgs struct {
	Path       string `json:"path"`        // 目标文件路径
	SourceText string `json:"source_text"` // 待匹配的原始文本片段
	TargetText string `json:"target_text"` // 替换后的新文本
}

// Execute 执行文件编辑操作。流程如下：
//  1. 反序列化 JSON 参数，提取路径、源文本和目标文本
//  2. 通过 safePath 校验并解析为绝对路径（含沙箱边界检查）
//  3. 读取源文件内容
//  4. 通过 fuzzyReplace 多级模糊匹配执行替换
//  5. 将结果写回文件
func (t *EditFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input editFileArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("参数解析失败：%w", err)
	}

	fullPath, err := safePath(t.workDir, input.Path)
	if err != nil {
		return "", err
	}

	unlock := LockPath(fullPath)
	defer unlock()

	contentBytes, err := os.ReadFile(fullPath)
	if err != nil {
		return "", fmt.Errorf("读取文件失败，请确认路径是否正确：%w", err)
	}
	originalContent := string(contentBytes)

	newContent, level, err := fuzzyReplaceWithLevel(originalContent, input.SourceText, input.TargetText)
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(fullPath, []byte(newContent), 0644); err != nil {
		return "", fmt.Errorf("写回文件失败：%w", err)
	}

	// level==1（精确匹配）时改动字节与意图完全一致，可声明权威无需复核；
	// L2-L4（换行/去空/缩进容错）写入的字节可能与模型预期不同（尤其 Python 缩进敏感），
	// 因此给出"建议复核缩进"的提示而非"无需确认"，避免在最该验证时抑制自愈。
	return buildEditSummary(input.Path, originalContent, newContent, level == matchExact), nil
}

// 匹配级别常量：标识 fuzzyReplace 命中的容错层级，用于决定编辑结果的可信度提示。
const (
	matchExact      = 1 // L1 精确匹配
	matchNewline    = 2 // L2 换行符归一化
	matchTrimmed    = 3 // L3 整体首尾去空
	matchLineByLine = 4 // L4 逐行去缩进
)

// editContextLines 是改动上下文中前后各保留的行数。
const editContextLines = 3

// buildEditSummary 根据编辑前后的内容构建带改动上下文的成功提示，
// 帮助 Agent 无需额外 read_file 调用即可直接验证改动是否正确。
//
// 使用公共前缀/后缀算法（不依赖外部库）定位变更区域，并以 unified diff 风格输出：
//   - 改动前的 3 行上下文（无前缀）
//   - 被删除的行（前缀 -）
//   - 新增的行（前缀 +）
//   - 改动后的 3 行上下文（无前缀）
//
// 当变更行数超过 20 行时，只输出行数统计，避免结果过长撑满 LLM 上下文。
//
// exactMatch 标识本次替换是否为 L1 精确匹配：
//   - true：diff 即权威确认，提示无需额外复核；
//   - false：L2-L4 容错匹配，写入字节可能与意图有出入，提示建议复核缩进/上下文。
func buildEditSummary(path, originalContent, newContent string, exactMatch bool) string {
	// 归一化换行符，保证跨平台比较一致
	orig := strings.ReplaceAll(originalContent, "\r\n", "\n")
	next := strings.ReplaceAll(newContent, "\r\n", "\n")

	// trimTrailingEmpty 去掉 strings.Split 在文件末尾 \n 后产生的空串元素，
	// 而非用 TrimRight 吃掉所有尾部换行——后者会丢失有意义的末尾空行。
	trimTrailingEmpty := func(lines []string) []string {
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			return lines[:len(lines)-1]
		}
		return lines
	}
	origLines := trimTrailingEmpty(strings.Split(orig, "\n"))
	nextLines := trimTrailingEmpty(strings.Split(next, "\n"))

	// 找到第一处差异的行（公共前缀长度）
	start := 0
	for start < len(origLines) && start < len(nextLines) && origLines[start] == nextLines[start] {
		start++
	}

	// 从尾部向前找公共后缀的起始位置
	origEnd := len(origLines) - 1
	nextEnd := len(nextLines) - 1
	for origEnd >= start && nextEnd >= start && origLines[origEnd] == nextLines[nextEnd] {
		origEnd--
		nextEnd--
	}

	// 计算删除/新增行数（纯插入或纯删除时对应计数为 0）
	removed := origEnd - start + 1
	if removed < 0 {
		removed = 0
	}
	added := nextEnd - start + 1
	if added < 0 {
		added = 0
	}

	// 变更超过 20 行只报数字，避免输出过长
	if removed+added > 20 {
		return fmt.Sprintf("成功修改文件: %s（删除 %d 行，新增 %d 行）", path, removed, added)
	}

	ctxStart := max(0, start-editContextLines)
	ctxEnd := min(len(origLines)-1, origEnd+editContextLines)

	var sb strings.Builder
	fmt.Fprintf(&sb, "成功修改文件: %s\n\n--- 改动上下文 ---\n", path)
	// 改动前的上下文行
	for i := ctxStart; i < start; i++ {
		fmt.Fprintf(&sb, "  %s\n", origLines[i])
	}
	// 被删除的行
	for i := start; i <= origEnd; i++ {
		fmt.Fprintf(&sb, "- %s\n", origLines[i])
	}
	// 新增的行
	for i := start; i <= nextEnd; i++ {
		fmt.Fprintf(&sb, "+ %s\n", nextLines[i])
	}
	// 改动后的上下文行（取 original 后续行）
	for i := origEnd + 1; i <= ctxEnd; i++ {
		fmt.Fprintf(&sb, "  %s\n", origLines[i])
	}
	if exactMatch {
		// 仅声明"字节已写入"，不暗示"行为已验证"：避免抑制后续的真实行为验证（自愈的关键反馈）。
		fmt.Fprint(&sb, "---\n✓ 精确匹配，以上 diff 已确认字节写入，无需再 grep/read_file 确认本次改动是否落地。"+
			"但这只代表「写入成功」，不代表行为正确——请运行真实测试或复现脚本来验证修复行为。")
	} else {
		fmt.Fprint(&sb, "---\n⚠️ 本次为模糊匹配（换行/空白/缩进经过容错处理），写入字节可能与预期略有出入。"+
			"建议用 read_file 复核改动处的缩进与上下文是否正确（尤其 Python 等缩进敏感语言）。")
	}
	return sb.String()
}

// fuzzyReplace 对文件内容执行多级模糊匹配替换（Multi-Level Fuzzy Matching）。
//
// 四级容错机制（Four-Level Fallback Pipeline）：
//
//	L1 — 精确匹配（Exact Match）：sourceText 在原始内容中精确出现一次，直接替换。
//	    替换使用原始 targetText 而非归一化版本，保留所有原始格式（含 \r\n）。
//	L2 — 换行符归一化（Line Ending Normalization）：
//	    将 \r\n 统一为 \n 后再匹配，兼容跨平台文件格式。
//	    替换后根据原始内容是否包含 \r\n 自动恢复换行风格。
//	L3 — 整体首尾去空（Trim Surrounding Whitespace）：
//	    去除 sourceText 两端的空白字符后匹配，容忍 LLM 产生的多余空白。
//	    若 trimmedSource 为空（全空白文本），跳过此步避免误匹配空行。
//	L4 — 逐行去缩进匹配（Line-by-Line Indent-Agnostic Matching）：
//	    逐行去除首尾空白后滑动窗口匹配，容忍缩进差异（空格 vs Tab）。
//
// 所有级别的匹配结果必须是唯一的（count == 1），多匹配或零匹配均返回明确错误。
// fuzzyReplace 是 fuzzyReplaceWithLevel 的兼容封装，仅返回结果与错误。
func fuzzyReplace(originalContent, sourceText, targetText string) (string, error) {
	result, _, err := fuzzyReplaceWithLevel(originalContent, sourceText, targetText)
	return result, err
}

// fuzzyReplaceWithLevel 在 fuzzyReplace 基础上额外返回命中的匹配层级（matchExact..matchLineByLine），
// 供调用方据此调整结果可信度提示（精确匹配可声明权威，模糊匹配建议复核）。
func fuzzyReplaceWithLevel(originalContent, sourceText, targetText string) (string, int, error) {
	// 归一化 targetText 的换行符，避免替换后混用 \r\n 和 \n
	normalizedTarget := strings.ReplaceAll(targetText, "\r\n", "\n")

	// L1: 原始文本精确匹配（Exact Match）
	count := strings.Count(originalContent, sourceText)
	if count == 1 {
		return strings.Replace(originalContent, sourceText, targetText, 1), matchExact, nil
	}
	if count > 1 {
		return "", 0, fmt.Errorf("source_text 匹配到了 %d 处，请提供更多的上下文代码以确保唯一性", count)
	}

	// 对原始内容做换行符归一化，进入 L2-L4
	normalizedContent := strings.ReplaceAll(originalContent, "\r\n", "\n")
	normalizedSource := strings.ReplaceAll(sourceText, "\r\n", "\n")

	// 判断是否需要重建 \r\n（恢复原始换行风格）
	hasCRLF := strings.Contains(originalContent, "\r\n")

	// L2: 换行符归一化匹配（Line Ending Normalization Match）
	count = strings.Count(normalizedContent, normalizedSource)
	if count == 1 {
		result := strings.Replace(normalizedContent, normalizedSource, normalizedTarget, 1)
		if hasCRLF {
			result = strings.ReplaceAll(result, "\n", "\r\n")
		}
		return result, matchNewline, nil
	}

	// L3: 整体首尾去空匹配（Trimmed Match）
	trimmedSource := strings.TrimSpace(normalizedSource)
	if trimmedSource != "" {
		count = strings.Count(normalizedContent, trimmedSource)
		if count == 1 {
			result := strings.Replace(normalizedContent, trimmedSource, normalizedTarget, 1)
			if hasCRLF {
				result = strings.ReplaceAll(result, "\n", "\r\n")
			}
			return result, matchTrimmed, nil
		}
	}

	// L4: 逐行去缩进匹配（Line-by-Line Indent-Agnostic Matching）
	result, err := lineByLineReplace(normalizedContent, normalizedSource, normalizedTarget, hasCRLF)
	if err != nil {
		return "", 0, err
	}
	return result, matchLineByLine, nil
}

// lineByLineReplace 将文本按行切割，去除首尾空白后进行滑动窗口匹配（Sliding Window Matching）。
//
// 应用场景：LLM 提供的 source_text 与文件中实际内容的缩进不一致时，
// 通过逐行去缩进实现模糊匹配（Indent-Agnostic Matching）。
//
// 参数说明：
//   - content:   已归一化（\r\n → \n）的文件内容
//   - sourceText: 已归一化的待匹配文本（由 fuzzyReplace 传入 normalizedSource）
//   - targetText: 已归一化的替换文本（由 fuzzyReplace 传入 normalizedTarget）
//   - hasCRLF:   原始文件是否使用 \r\n 换行风格，用于匹配后恢复
//
// 匹配成功后，使用 targetText 替换整个匹配块（contentLines 中从 matchStartIndex
// 到 matchEndIndex 的范围），并保留原始换行风格。
func lineByLineReplace(content, sourceText, targetText string, hasCRLF bool) (string, error) {
	contentLines := strings.Split(content, "\n")
	sourceLines := strings.Split(strings.TrimSpace(sourceText), "\n")

	if len(sourceLines) == 0 || len(contentLines) < len(sourceLines) {
		return "", fmt.Errorf("在文件中未找到 source_text，请检查内容和缩进")
	}

	// 去除源文本每行的首尾空白
	for i := range sourceLines {
		sourceLines[i] = strings.TrimSpace(sourceLines[i])
	}

	matchCount := 0
	matchStartIndex := -1
	matchEndIndex := -1

	// 滑动窗口匹配
	for i := 0; i <= len(contentLines)-len(sourceLines); i++ {
		isMatch := true
		for j := 0; j < len(sourceLines); j++ {
			if strings.TrimSpace(contentLines[i+j]) != sourceLines[j] {
				isMatch = false
				break
			}
		}

		if isMatch {
			matchCount++
			matchStartIndex = i
			matchEndIndex = i + len(sourceLines)
		}
	}

	if matchCount == 0 {
		return "", fmt.Errorf("在文件中未找到 source_text，请检查内容和缩进")
	}
	if matchCount > 1 {
		return "", fmt.Errorf("模糊匹配到了 %d 处代码，请提供更多上下文以定位", matchCount)
	}

	// 关键修正：L4 是缩进无关匹配，模型提供的 targetText 缩进未必与文件一致，
	// 若原样写入会破坏缩进敏感语言（Python）的结构，产生 IndentationError。
	// 这里以被匹配块首行的缩进为基准，对 targetText 重新缩进：保留其相对结构，
	// 但将整体基准对齐到文件中被替换块的缩进。
	baseIndent := leadingWhitespace(contentLines[matchStartIndex])
	reindentedTarget := reindentBlock(targetText, baseIndent)

	var newContentLines []string
	newContentLines = append(newContentLines, contentLines[:matchStartIndex]...)
	newContentLines = append(newContentLines, reindentedTarget)
	newContentLines = append(newContentLines, contentLines[matchEndIndex:]...)

	result := strings.Join(newContentLines, "\n")
	if hasCRLF {
		result = strings.ReplaceAll(result, "\n", "\r\n")
	}
	return result, nil
}

// leadingWhitespace 返回字符串开头的空白前缀（空格 / Tab）。
func leadingWhitespace(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

// commonStringPrefix 返回 a 与 b 的最长公共前缀。
func commonStringPrefix(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return a[:i]
}

// reindentBlock 将多行 target 重新缩进到 baseIndent 基准。
//
// 算法：先求所有非空行的最长公共空白前缀（target 自身的基准缩进），
// 逐行剥离该公共前缀后改用 baseIndent，从而保留行间相对缩进、把整体对齐到文件中
// 被替换块的缩进层级。纯空白行归一化为空行。
func reindentBlock(target, baseIndent string) string {
	lines := strings.Split(target, "\n")
	common := ""
	seeded := false
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		lead := leadingWhitespace(ln)
		if !seeded {
			common = lead
			seeded = true
			continue
		}
		common = commonStringPrefix(common, lead)
	}
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			lines[i] = ""
			continue
		}
		lines[i] = baseIndent + strings.TrimPrefix(ln, common)
	}
	return strings.Join(lines, "\n")
}
