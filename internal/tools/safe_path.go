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

// hardSensitivePaths 是永远拒绝访问的敏感路径前缀，不受 workDir 白名单影响。
// 使用绝对路径前缀匹配，防止 LLM 意外或恶意读写凭证文件。
var hardSensitivePaths = buildSensitivePaths()

func buildSensitivePaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		// 无法确定 home dir 时返回空列表，bash DangerHook 作为备份防线。
		return nil
	}
	return []string{
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".aws"),
		filepath.Join(home, ".kube"),
		filepath.Join(home, ".gnupg"),
		filepath.Join(home, ".netrc"),
		filepath.Join(home, ".config", "gcloud"),
	}
}

// isSensitivePath 检查绝对路径是否落在硬编码敏感目录下。
func isSensitivePath(absPath string) bool {
	for _, prefix := range hardSensitivePaths {
		if absPath == prefix || strings.HasPrefix(absPath, prefix+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

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

	// 绝对路径输入直接规范化，绝不再与 workDir 拼接。
	// filepath.Join(workDir, "/abs/path") 不会把第二个参数当作绝对路径处理，
	// 而是直接拼接成 "/workDir/abs/path"；当输入恰好是 workDir 下的绝对路径
	// （如 SWE-bench prompt 注入的绝对工作目录 + 子路径）时会产生
	// "/workDir/workDir/sub" 的翻倍路径，虽通过前缀校验却指向不存在的文件。
	// 因此对绝对路径走独立分支，相对路径才与 workDir 拼接。
	var absPath string
	if filepath.IsAbs(inputPath) {
		absPath = filepath.Clean(inputPath)
	} else {
		var err error
		absPath, err = filepath.Abs(filepath.Join(cleanWorkDir, inputPath))
		if err != nil {
			return "", fmt.Errorf("解析路径失败：%w", err)
		}
	}

	// 安全校验：确保解析后的绝对路径仍以工作区目录为前缀。
	// 注意：必须比较 cleanWorkDir + PathSeparator（而非仅 cleanWorkDir），
	// 否则 "/project-evil" 会被误判为 "/project" 的合法子路径。
	if !strings.HasPrefix(absPath, cleanWorkDir+string(os.PathSeparator)) && absPath != cleanWorkDir {
		return "", fmt.Errorf("路径 '%s' 超出工作区范围", inputPath)
	}

	if isSensitivePath(absPath) {
		return "", fmt.Errorf("路径 '%s' 是受保护的敏感路径，禁止访问", inputPath)
	}

	return absPath, nil
}
