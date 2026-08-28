package ltm

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// newTestStore 创建一个内存 SQLite Store，now 固定为可控时间。
func newTestStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开内存库: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	s, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	s.now = func() time.Time { return now }
	return s, &now
}

func TestStoreAddAndGet(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	got, err := s.Add(ctx, &Entry{Title: "Go 版本", Content: "项目使用 Go 1.25.3", Category: CategoryKnowledge, Importance: 7})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got.ID == "" {
		t.Fatal("Add 应生成非空 ID")
	}
	if got.Signature == "" {
		t.Fatal("Add 应写入签名")
	}
	fetched, err := s.Get(ctx, got.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fetched.Content != "项目使用 Go 1.25.3" || fetched.Importance != 7 {
		t.Errorf("Get 内容不符：%+v", fetched)
	}
}

func TestStoreAddDedup(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	a, err := s.Add(ctx, &Entry{Title: "x", Content: "重复内容"})
	if err != nil {
		t.Fatalf("第一次 Add: %v", err)
	}
	b, err := s.Add(ctx, &Entry{Title: "y", Content: "重复内容"})
	if err != nil {
		t.Fatalf("第二次 Add: %v", err)
	}
	if a.ID != b.ID {
		t.Errorf("相同内容应去重为同一条目: %s != %s", a.ID, b.ID)
	}
	if b.UseCount != 1 {
		t.Errorf("去重命中应自增 use_count，got %d", b.UseCount)
	}
}

func TestStoreSearchAndReinforce(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Add(ctx, &Entry{Title: "Go 版本", Content: "项目使用 Go 1.25.3 构建"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(ctx, &Entry{Title: "数据库", Content: "使用 SQLite 持久化会话"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Search(ctx, "SQLite", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 || res[0].Title != "数据库" {
		t.Fatalf("期望命中「数据库」，got %+v", res)
	}
	// 强化：命中后 use_count 自增、last_used_at 写入。
	again, _ := s.Get(ctx, res[0].ID)
	if again.UseCount != 1 {
		t.Errorf("命中应自增 use_count，got %d", again.UseCount)
	}
	if again.LastUsedAt.IsZero() {
		t.Error("命中应写入 last_used_at")
	}
}

func TestStoreSearchEmptyQuery(t *testing.T) {
	s, _ := newTestStore(t)
	res, err := s.Search(context.Background(), "   ", 5)
	if err != nil {
		t.Fatalf("空查询不应报错: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("空查询应返回空结果，got %d", len(res))
	}
}

func TestStoreUpdate(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	e, _ := s.Add(ctx, &Entry{Title: "旧标题", Content: "旧内容"})
	e.Title = "新标题"
	e.Content = "新内容"
	e.Importance = 9
	if err := s.Update(ctx, e); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := s.Get(ctx, e.ID)
	if got.Title != "新标题" || got.Content != "新内容" || got.Importance != 9 {
		t.Errorf("更新未生效：%+v", got)
	}
	if got.Signature != Signature("新内容") {
		t.Error("更新内容应重算签名")
	}
	// FTS 应可检索到新内容、检索不到旧内容。
	if res, _ := s.Search(ctx, "新内容", 5); len(res) != 1 {
		t.Errorf("更新后 FTS 应命中新内容，got %d", len(res))
	}
	if res, _ := s.Search(ctx, "旧内容", 5); len(res) != 0 {
		t.Errorf("更新后 FTS 不应命中旧内容，got %d", len(res))
	}
}

func TestStoreSoftDelete(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	e, _ := s.Add(ctx, &Entry{Title: "t", Content: "待删除内容"})
	if err := s.SoftDelete(ctx, e.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	got, _ := s.Get(ctx, e.ID) // 仍可取到（审计）
	if !got.Disabled {
		t.Error("软删除后 disabled 应为 true")
	}
	if res, _ := s.Search(ctx, "待删除内容", 5); len(res) != 0 {
		t.Errorf("软删除后不应被检索到，got %d", len(res))
	}
}

func TestStoreReAddAfterSoftDelete(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	first, err := s.Add(ctx, &Entry{Title: "t", Content: "可复活内容"})
	if err != nil {
		t.Fatalf("首次 Add: %v", err)
	}
	if err := s.SoftDelete(ctx, first.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	// 软删除后重新添加相同内容：不应触发 UNIQUE 冲突，应得到一条新的活跃条目。
	second, err := s.Add(ctx, &Entry{Title: "t", Content: "可复活内容"})
	if err != nil {
		t.Fatalf("软删除后重新 Add 不应报错: %v", err)
	}
	if second.ID == first.ID {
		t.Error("软删除后重新 Add 应创建新条目，而非命中已删除条目")
	}
	if res, _ := s.Search(ctx, "可复活内容", 5); len(res) != 1 {
		t.Errorf("重新添加后应可检索到 1 条活跃记忆，got %d", len(res))
	}
}

func TestStoreListOrderAndFilter(t *testing.T) {
	s, now := newTestStore(t)
	ctx := context.Background()
	s.Add(ctx, &Entry{Title: "低", Content: "低优先", Importance: 2})
	s.Add(ctx, &Entry{Title: "高", Content: "高优先", Importance: 9})
	// 一条已过期条目（updated_at 在过去，ttl=1 天）。
	s.now = func() time.Time { return now.Add(-48 * time.Hour) }
	s.Add(ctx, &Entry{Title: "过期", Content: "过期内容", Importance: 10, TTLDays: 1})
	s.now = func() time.Time { return *now }

	list, err := s.List(ctx, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("过期条目应被过滤，期望 2 条，got %d", len(list))
	}
	if list[0].Title != "高" {
		t.Errorf("应按 importance 降序，首条应为「高」，got %q", list[0].Title)
	}
}

func TestStorePurgeExpired(t *testing.T) {
	s, now := newTestStore(t)
	ctx := context.Background()
	s.now = func() time.Time { return now.Add(-48 * time.Hour) }
	e, _ := s.Add(ctx, &Entry{Title: "x", Content: "过期", TTLDays: 1})
	s.now = func() time.Time { return *now }

	n, err := s.PurgeExpired(ctx)
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if n != 1 {
		t.Errorf("应回收 1 条过期记忆，got %d", n)
	}
	got, _ := s.Get(ctx, e.ID)
	if !got.Disabled {
		t.Error("过期回收应置 disabled")
	}
}

func TestStoreStaleCandidates(t *testing.T) {
	s, now := newTestStore(t)
	ctx := context.Background()
	s.now = func() time.Time { return now.Add(-90 * 24 * time.Hour) }
	s.Add(ctx, &Entry{Title: "陈旧", Content: "低价值且久未使用", Importance: 1})
	s.now = func() time.Time { return *now }
	s.Add(ctx, &Entry{Title: "新鲜", Content: "近期高价值", Importance: 8})

	stale, err := s.StaleCandidates(ctx)
	if err != nil {
		t.Fatalf("StaleCandidates: %v", err)
	}
	if len(stale) != 1 || stale[0].Title != "陈旧" {
		t.Errorf("应识别出 1 条陈旧候选，got %+v", stale)
	}
}

// TestFtsQuery 验证 ftsQuery 将用户输入安全转换为 FTS5 MATCH 表达式：
// 每个 token 被双引号包裹（内部双引号翻倍转义），以 OR 连接。
func TestFtsQuery(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"SQLite", `"SQLite"`},
		{"Go 1.25", `"Go" OR "1.25"`},
		{"  ", ""},        // 纯空白 → 空串
		{`a"b`, `"a""b"`}, // 内部双引号翻倍转义
		{"hello world foo", `"hello" OR "world" OR "foo"`},
	}
	for _, tc := range cases {
		got := ftsQuery(tc.input)
		if got != tc.want {
			t.Errorf("ftsQuery(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestStoreSearchDisabledNotReturned 验证 Search 不返回已软删除的条目。
func TestStoreSearchDisabledNotReturned(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	e, err := s.Add(ctx, &Entry{Title: "disabled", Content: "此内容应不可见"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.SoftDelete(ctx, e.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	res, err := s.Search(ctx, "此内容应不可见", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("软删除后不应出现在搜索结果中，got %d 条", len(res))
	}
}

// TestStoreUpdateNotFound 验证 Update 在条目不存在时返回 ErrNotFound 包装的错误。
func TestStoreUpdateNotFound(t *testing.T) {
	s, _ := newTestStore(t)
	err := s.Update(context.Background(), &Entry{ID: "nonexistent", Content: "x", Title: "t"})
	if err == nil {
		t.Fatal("Update 不存在的 ID 应返回错误")
	}
}

// TestStoreSoftDeleteNotFound 验证 SoftDelete 在条目不存在时返回错误。
func TestStoreSoftDeleteNotFound(t *testing.T) {
	s, _ := newTestStore(t)
	err := s.SoftDelete(context.Background(), "nonexistent-id")
	if err == nil {
		t.Fatal("SoftDelete 不存在的 ID 应返回错误")
	}
}

// TestStorePurgeExpiredReleasesSignature 验证 PurgeExpired 将过期记忆的 signature 置为 NULL，
// 使相同内容在过期后可以被重新添加（不触发 UNIQUE 约束冲突）。
// 修复前：PurgeExpired 仅设 disabled=1，保留 signature，导致 Add 时签名去重仍命中已禁用行。
func TestStorePurgeExpiredReleasesSignature(t *testing.T) {
	s, now := newTestStore(t)
	ctx := context.Background()

	// 48 小时前写入，TTL=1 天 → 现在已过期
	s.now = func() time.Time { return now.Add(-48 * time.Hour) }
	e, err := s.Add(ctx, &Entry{Title: "过期条目", Content: "过期后可复活的内容", TTLDays: 1})
	if err != nil {
		t.Fatalf("首次 Add: %v", err)
	}

	// 恢复到「现在」，执行回收
	s.now = func() time.Time { return *now }
	n, err := s.PurgeExpired(ctx)
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("期望回收 1 条，got %d", n)
	}

	// 确认已被标记为 disabled
	got, _ := s.Get(ctx, e.ID)
	if !got.Disabled {
		t.Fatal("PurgeExpired 应将条目标记为 disabled")
	}

	// 核心断言：使用相同内容重新 Add，不应触发 UNIQUE 冲突，应得到新的活跃条目
	second, err := s.Add(ctx, &Entry{Title: "新标题", Content: "过期后可复活的内容", TTLDays: 0})
	if err != nil {
		t.Fatalf("PurgeExpired 后重新 Add 不应报错（signature 应已被清除）: %v", err)
	}
	if second.ID == e.ID {
		t.Error("PurgeExpired 后重新 Add 应创建新条目，而非命中已过期的 disabled 条目")
	}
	if second.Disabled {
		t.Error("重新添加的条目不应是 disabled 状态")
	}
}
