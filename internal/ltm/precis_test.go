package ltm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrecisRegenerateAndRead(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	s.Add(ctx, &Entry{Title: "高优先", Content: "重要事实", Importance: 9, Category: CategoryKnowledge})
	s.Add(ctx, &Entry{Title: "低优先", Content: "次要事实", Importance: 1})

	path := filepath.Join(t.TempDir(), "memories", "MEMORY.md")
	p := NewPrecis(s, path, 4096)
	if err := p.Regenerate(ctx); err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	content, err := p.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(content, "高优先") || !strings.Contains(content, "重要事实") {
		t.Errorf("精华应包含高优先条目: %q", content)
	}
	// 文件确实落盘。
	if _, err := os.Stat(path); err != nil {
		t.Errorf("MEMORY.md 应已写入磁盘: %v", err)
	}
}

func TestPrecisByteCap(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		if _, err := s.Add(ctx, &Entry{
			Title:      fmt.Sprintf("条目%d", i),
			Content:    fmt.Sprintf("第%d条记忆内容：%s", i, strings.Repeat("填充", 20)),
			Importance: 5,
		}); err != nil {
			t.Fatal(err)
		}
	}
	p := NewPrecis(s, filepath.Join(t.TempDir(), "MEMORY.md"), 500)
	if err := p.Regenerate(ctx); err != nil {
		t.Fatal(err)
	}
	content, _ := p.Read()
	if len(content) > 500 {
		t.Errorf("精华应被截断到不超过 500 字节，got %d", len(content))
	}
	if !strings.Contains(content, "…（已截断）") {
		t.Errorf("超长精华应包含截断标记，got %q", content)
	}
}

func TestPrecisReadMissing(t *testing.T) {
	p := NewPrecis(nil, filepath.Join(t.TempDir(), "nope.md"), 4096)
	content, err := p.Read()
	if err != nil {
		t.Errorf("缺失文件应返回空串而非错误: %v", err)
	}
	if content != "" {
		t.Errorf("缺失文件应返回空串，got %q", content)
	}
}

// TestTruncateUTF8 验证 truncateUTF8 在 UTF-8 rune 边界处截断，不截断多字节字符。
// 注意：截断标记本身占用若干字节（"\n…（已截断）"），因此 maxBytes 需大于标记长度才会输出标记。
func TestTruncateUTF8(t *testing.T) {
	// budget = maxBytes - len(marker)；budget<=0 时返回空串。
	// 这里用 len(marker) 动态取标记字节数，避免硬编码与实现脱节。
	marker := "\n…（已截断）"
	markerLen := len(marker)

	cases := []struct {
		name          string
		input         string
		maxBytes      int
		wantTruncated bool
	}{
		{
			name:          "短于上限，不截断",
			input:         "hello",
			maxBytes:      100,
			wantTruncated: false,
		},
		{
			name:          "恰好等于上限，不截断",
			input:         "hello",
			maxBytes:      5,
			wantTruncated: false,
		},
		{
			// 需要 maxBytes 大于 markerLen 才能输出截断标记
			name:          "ASCII 超出，截断",
			input:         strings.Repeat("a", markerLen+20),
			maxBytes:      markerLen + 10,
			wantTruncated: true,
		},
		{
			// 多字节 UTF-8:4 个汉字 = 12 字节；截断点需回退到合法 rune 边界
			name:          "多字节 UTF-8 不截断字符内部",
			input:         "你好世界" + strings.Repeat("x", markerLen+20),
			maxBytes:      markerLen + 10,
			wantTruncated: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateUTF8(tc.input, tc.maxBytes)
			if len(got) > tc.maxBytes {
				t.Errorf("截断后长度 %d 超过 maxBytes %d", len(got), tc.maxBytes)
			}
			hasTruncated := strings.Contains(got, marker)
			if hasTruncated != tc.wantTruncated {
				t.Errorf("wantTruncated=%v, hasTruncated=%v, output=%q", tc.wantTruncated, hasTruncated, got)
			}
			// 确保输出是合法 UTF-8
			if !isValidUTF8(got) {
				t.Errorf("截断结果不是合法 UTF-8: %q", got)
			}
		})
	}
}

// isValidUTF8 报告 s 是否是合法的 UTF-8 编码字符串。
func isValidUTF8(s string) bool {
	return utf8.ValidString(s)
}
