// env 包的单元测试（Unit Tests）。
// 覆盖 .env 文件解析的各种场景：正常解析、文件缺失、系统环境变量优先、
// 以及 parseLine 的边界情况（引号处理、空行、注释等）。
package env

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_ExistingFile 验证 Load 正确解析包含各种格式的 .env 文件：
//
//	普通值、双引号值、单引号值、空值
func TestLoad_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")

	content := `# Test config
TEST_KEY_1=value1
TEST_KEY_2="quoted value"
TEST_KEY_3='single quoted'
TEST_KEY_4=
`
	if err := os.WriteFile(envFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Load(envFile); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// 表驱动测试（Table-Driven Tests）：验证各种格式的键值对都被正确解析
	tests := []struct {
		key, want string
	}{
		{"TEST_KEY_1", "value1"},
		{"TEST_KEY_2", "quoted value"},
		{"TEST_KEY_3", "single quoted"},
		{"TEST_KEY_4", ""},
	}

	for _, tt := range tests {
		got := os.Getenv(tt.key)
		if got != tt.want {
			t.Errorf("os.Getenv(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

// TestLoad_FileNotFound 验证 .env 文件不存在时的优雅降级：
// Load 应返回 nil（而非错误），使程序可以在没有 .env 文件时仍正常运行。
func TestLoad_FileNotFound(t *testing.T) {
	err := Load("/nonexistent/.env")
	if err != nil {
		t.Fatalf("Load should return nil for missing file, got: %v", err)
	}
}

// TestLoad_DoesNotOverride 验证已存在的系统环境变量不会被 .env 文件覆盖。
// 这确保了"系统环境变量 > .env 文件"的优先级策略。
func TestLoad_DoesNotOverride(t *testing.T) {
	os.Setenv("EXISTING_KEY", "system_value")
	defer os.Unsetenv("EXISTING_KEY")

	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	content := "EXISTING_KEY=file_value\n"

	if err := os.WriteFile(envFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Load(envFile); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if got := os.Getenv("EXISTING_KEY"); got != "system_value" {
		t.Errorf("expected system_value, got %q", got)
	}
}

// TestParseLine 使用表驱动测试（Table-Driven Tests）验证 parseLine 的各种边界情况：
//
//	普通键值对、双引号值去除、空值、带空格的键值、注释行、无等号行、
//	无键行、不匹配引号（不应去除）、无引号值。
func TestParseLine(t *testing.T) {
	tests := []struct {
		line    string
		key     string
		value   string
		isValid bool
	}{
		{"KEY=VALUE", "KEY", "VALUE", true},
		{"KEY=\"quoted\"", "KEY", "quoted", true},
		{"NO_VALUE=", "NO_VALUE", "", true},
		{"  SPACED  =  value  ", "SPACED", "value", true},
		{"#COMMENT", "", "", false},
		{"NOEQUALS", "", "", false},
		{"=NOKEY", "", "", false},
		{"KEY=\"mismatch'", "KEY", "\"mismatch'", true},
		{"KEY='mismatch\"", "KEY", "'mismatch\"", true},
		{"KEY=noquotes", "KEY", "noquotes", true},
	}

	for _, tt := range tests {
		key, value, ok := parseLine(tt.line)
		if ok != tt.isValid {
			t.Errorf("parseLine(%q) ok = %v, want %v", tt.line, ok, tt.isValid)
		}
		if ok {
			if key != tt.key || value != tt.value {
				t.Errorf("parseLine(%q) = (%q, %q), want (%q, %q)", tt.line, key, value, tt.key, tt.value)
			}
		}
	}
}
