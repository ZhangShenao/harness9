package ltm

import (
	"context"
	"errors"
	"testing"

	"github.com/harness9/internal/schema"
)

// fakeGen 是 Generator 桩：固定返回 text，或返回 err。
type fakeGen struct {
	text string
	err  error
}

func (f fakeGen) Generate(_ context.Context, _ []schema.Message, _ []schema.ToolDefinition) (*schema.Message, *schema.Usage, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	return &schema.Message{Role: schema.RoleAssistant, Content: f.text}, nil, nil
}

func TestExtractorUpsertsFacts(t *testing.T) {
	s, _ := newTestStore(t)
	gen := fakeGen{text: "```json\n[{\"title\":\"偏好\",\"content\":\"用户偏好中文回复\",\"category\":\"preference\",\"importance\":8}]\n```"}
	ex := NewExtractor(gen, s)
	ex.Extract([]schema.Message{{Role: schema.RoleUser, Content: "请用中文"}})

	list, _ := s.List(context.Background(), 10)
	if len(list) != 1 || list[0].Content != "用户偏好中文回复" {
		t.Fatalf("提取应写入 1 条记忆，got %+v", list)
	}
	if list[0].Category != CategoryPreference || list[0].Importance != 8 {
		t.Errorf("分类/重要度解析错误：%+v", list[0])
	}
}

func TestExtractorFailOpen(t *testing.T) {
	s, _ := newTestStore(t)
	ex := NewExtractor(fakeGen{err: errors.New("network")}, s)
	// 不应 panic；提取失败静默吞掉。
	ex.Extract([]schema.Message{{Role: schema.RoleUser, Content: "x"}})
	list, _ := s.List(context.Background(), 10)
	if len(list) != 0 {
		t.Errorf("提取失败不应写入记忆，got %d", len(list))
	}
}

func TestExtractorIgnoresEmptyArray(t *testing.T) {
	s, _ := newTestStore(t)
	ex := NewExtractor(fakeGen{text: "[]"}, s)
	ex.Extract([]schema.Message{{Role: schema.RoleUser, Content: "闲聊"}})
	list, _ := s.List(context.Background(), 10)
	if len(list) != 0 {
		t.Errorf("空数组不应写入记忆，got %d", len(list))
	}
}
