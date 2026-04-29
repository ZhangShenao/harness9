// 文件路径安全工具（Path Safety Helper）。
//
// 本文件提供 safePath 函数，作为所有访问文件系统的内置工具（read_file、write_file 等）
// 共享的沙箱边界（Sandbox Boundary）校验逻辑，防止路径遍历攻击（Path Traversal Attack）。
//
// 攻击场景示例：
//   - workDir = "/Users/alice/project"
//   - 用户输入 path = "../../etc/passwd"
//   - 直接 filepath.Join 后得到 "/Users/etc/passwd"，已逃逸出工作区
//
// safePath 通过 filepath.Abs + 前缀校验，确保最终绝对路径仍在 workDir 范围内。
package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// safePath 将用户输入的相对路径解析为绝对路径，并校验是否在工作区范围内。
//
// 这是防止路径遍历攻击（Path Traversal Attack）的核心安全屏障：
// 任何试图通过 "../" 逃逸工作区的路径都会被拒绝。
//
// 参数：
//   - workDir:   工具允许访问的根目录（Sandbox Boundary，沙箱边界）
//   - inputPath: LLM 提供的相对路径（Untrusted Input，不可信输入）
//
// 返回：
//   - 校验通过时返回经 filepath.Abs 规范化的绝对路径
//   - 路径超出工作区时返回 error，调用方应将错误回传给 LLM
//
// 例如：
//
//	safePath("/project", "src/main.go")          → "/project/src/main.go", nil
//	safePath("/project", "../../etc/passwd")     → "", error
//	safePath("/project", "./README.md")          → "/project/README.md", nil
func safePath(workDir, inputPath string) (string, error) {
	cleanWorkDir := filepath.Clean(workDir)
	joined := filepath.Join(cleanWorkDir, inputPath)
	absPath, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("解析路径失败: %w", err)
	}

	// 安全校验：确保解析后的绝对路径仍以工作区目录为前缀。
	// 注意：必须比较 cleanWorkDir + PathSeparator（而非仅 cleanWorkDir），
	// 否则 "/project-evil" 会被误判为 "/project" 的合法子路径。
	if !strings.HasPrefix(absPath, cleanWorkDir+string(os.PathSeparator)) && absPath != cleanWorkDir {
		return "", fmt.Errorf("路径 '%s' 超出工作区范围", inputPath)
	}

	return absPath, nil
}
