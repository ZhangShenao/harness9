// Package env 提供基于 .env 文件的环境变量配置加载能力。
// 在程序启动时调用 Load 即可从项目根目录的 .env 文件读取键值对并注入到进程环境变量中，
// 已存在的环境变量不会被覆盖（优先使用系统环境变量）。
//
// 配置文件格式：
//
//	# 注释行
//	KEY=VALUE
//	EMPTY_VALUE=
//	# 引号值会自动去除
//	QUOTED="hello world"
package env

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Load 从指定路径的 .env 文件读取配置并设置为环境变量。
// 已存在的环境变量不会被覆盖，允许通过系统环境变量覆盖 .env 文件中的值。
// 如果文件不存在，返回 nil（非错误），使程序可以在没有 .env 文件时仍正常运行。
func Load(filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open env file: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := parseLine(line)
		if !ok {
			continue
		}

		if os.Getenv(key) == "" {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("set env %s at line %d: %w", key, lineNum, err)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read env file: %w", err)
	}

	return nil
}

// parseLine 解析单行 KEY=VALUE 格式，返回键、值和是否有效。
// 支持：去除引号、去除前后空白、跳过无等号的行。
func parseLine(line string) (string, string, bool) {
	idx := strings.Index(line, "=")
	if idx < 0 {
		return "", "", false
	}

	key := strings.TrimSpace(line[:idx])
	if key == "" {
		return "", "", false
	}

	value := strings.TrimSpace(line[idx+1:])

	value = unquote(value)

	return key, value, true
}

// unquote 去除值两端成对匹配的引号。仅当首尾引号字符一致（同为 " 或同为 '）时才去除，
// 避免误删除不匹配的引号。
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
