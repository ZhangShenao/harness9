package env

import (
	"os"
	"path/filepath"
	"testing"
)

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

func TestLoad_FileNotFound(t *testing.T) {
	err := Load("/nonexistent/.env")
	if err != nil {
		t.Fatalf("Load should return nil for missing file, got: %v", err)
	}
}

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
