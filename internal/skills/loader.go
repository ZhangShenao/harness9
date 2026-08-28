// Package skills — LoadSkills：从文件系统扫描并初始化 Skills Index。
// 本文件实现 LoadSkills，扫描 skillsDir 下各子目录中的 SKILL.md 文件，
// 解析 YAML frontmatter 提取元数据，构建并返回 *Index。
// 目录不存在时静默返回空 Index，保证零配置可运行。
package skills

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"github.com/harness9/internal/logfmt"
)

// LoadSkills 扫描 skillsDir 下各子目录，在每个子目录中读取 SKILL.md 构建 Index。
// skillsDir 不存在时返回空 Index（不报错），保证零配置可运行。
// 子目录缺少 SKILL.md、或 SKILL.md 的 frontmatter 缺少必填字段时跳过并记录警告日志。
//
// 目录结构约定：
//
//	skills/
//	├── go-refactor/
//	│   └── SKILL.md
//	└── testing-guide/
//	    └── SKILL.md
func LoadSkills(skillsDir string) (*Index, error) {
	entries, err := os.ReadDir(skillsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return &Index{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 skills 目录失败：%w", err)
	}

	var loaded []Skill
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		filePath := filepath.Join(skillsDir, entry.Name(), "SKILL.md")
		data, err := os.ReadFile(filePath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				log.Print(logfmt.FormatMsg("skills", fmt.Sprintf("跳过 %s: 子目录缺少 SKILL.md", entry.Name())))
			} else {
				log.Print(logfmt.FormatMsg("skills", fmt.Sprintf("跳过 %s: 读取 SKILL.md 失败: %v", entry.Name(), err)))
			}
			continue
		}
		name, desc, trigger, _ := parseFrontmatter(string(data))
		if name == "" || desc == "" {
			log.Print(logfmt.FormatMsg("skills", fmt.Sprintf("跳过 %s: frontmatter 缺少 name 或 description", entry.Name())))
			continue
		}
		loaded = append(loaded, Skill{
			Name:        name,
			Description: desc,
			Trigger:     trigger,
			filePath:    filePath,
		})
	}
	return &Index{skills: loaded}, nil
}
