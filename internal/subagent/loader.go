// Package subagent — loader：从目录扫描文件式子代理定义并注册。
// 本文件实现 Registry.LoadFromDir，扫描 dir（通常为 workDir/.harness9/agents/）下的 *.md 文件，
// 逐一解析为 SubAgentDefinition 并注册。目录不存在时静默返回，保证零配置可运行。
// 文件定义覆盖同名编程式定义（如内置 general-purpose），便于项目级自定义覆盖内置行为。
package subagent

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/harness9/internal/logfmt"
)

// LoadFromDir 扫描 dir 下的 *.md 文件，解析为子代理定义并注册。
// 语义：
//   - 目录不存在：静默返回 nil（零配置可运行）
//   - 单文件解析失败 / 缺必填字段：跳过并记录 warning，不中断
//   - 缺 name：回退用文件名（去 .md）
//   - 文件定义覆盖同名编程式定义（记录日志）
func (r *Registry) LoadFromDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取 agents 目录失败：%w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			log.Print(logfmt.FormatMsg("subagent", fmt.Sprintf("跳过 %s: 读取失败: %v", entry.Name(), err)))
			continue
		}
		def, err := parseAgentFile(string(data))
		if err != nil {
			log.Print(logfmt.FormatMsg("subagent", fmt.Sprintf("跳过 %s: %v", entry.Name(), err)))
			continue
		}
		if def.Name == "" {
			def.Name = strings.TrimSuffix(entry.Name(), ".md")
		}
		def.Source = path
		if err := def.Validate(); err != nil {
			log.Print(logfmt.FormatMsg("subagent", fmt.Sprintf("跳过 %s: %v", entry.Name(), err)))
			continue
		}
		if _, exists := r.defs[def.Name]; exists {
			log.Print(logfmt.FormatMsg("subagent", fmt.Sprintf("文件定义覆盖同名子代理 %q", def.Name)))
		}
		r.defs[def.Name] = def
	}
	return nil
}
