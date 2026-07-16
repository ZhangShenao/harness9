package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestFetchStarEventsPagination 验证跨分页拉取会合并全部结果并按时间升序排列。
func TestFetchStarEventsPagination(t *testing.T) {
	pages := [][]string{
		{"2026-05-01T00:00:00Z", "2026-05-03T00:00:00Z"},
		{"2026-05-02T00:00:00Z"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/vnd.github.star+json" {
			t.Errorf("Accept header = %q, want star+json", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization header = %q, want Bearer test-token", got)
		}
		page := r.URL.Query().Get("page")
		var body string
		switch page {
		case "1":
			body = fmt.Sprintf(`[{"starred_at":%q},{"starred_at":%q}]`, pages[0][0], pages[0][1])
		case "2":
			body = fmt.Sprintf(`[{"starred_at":%q}]`, pages[1][0])
		default:
			body = `[]`
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	defer srv.Close()

	// perPage=2：第一页返回 2 条（=perPage，继续翻页），第二页返回 1 条（<perPage，终止）
	events, err := fetchStarEvents(srv.URL, "owner/repo", "test-token", 2)
	if err != nil {
		t.Fatalf("fetchStarEvents returned error: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	for i := 1; i < len(events); i++ {
		if events[i].Before(events[i-1]) {
			t.Errorf("events not sorted ascending: %v before %v", events[i], events[i-1])
		}
	}
}

// TestFetchStarEventsErrorResponse 验证非 200 响应会被包装为可读错误而非静默丢弃。
func TestFetchStarEventsErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("Not Found"))
	}))
	defer srv.Close()

	_, err := fetchStarEvents(srv.URL, "owner/repo", "test-token", 100)
	if err == nil {
		t.Fatal("expected error for 403 response, got nil")
	}
}

// TestFetchStarEventsMissingStarredAt 验证响应缺少 starred_at 字段（如 Accept 头未被遵循，
// 退化为默认 user 列表格式）时会显式报错，而不是静默生成零值时间戳。
func TestFetchStarEventsMissingStarredAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"login":"octocat"}]`))
	}))
	defer srv.Close()

	_, err := fetchStarEvents(srv.URL, "owner/repo", "test-token", 100)
	if err == nil {
		t.Fatal("expected error when starred_at is missing, got nil")
	}
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tm
}

// TestAggregateDaily 验证按天聚合累计计数、同日多次 star 合并、乱序输入、并延伸到当前日期。
func TestAggregateDaily(t *testing.T) {
	t.Run("空输入返回空", func(t *testing.T) {
		if got := aggregateDaily(nil); got != nil {
			t.Errorf("aggregateDaily(nil) = %v, want nil", got)
		}
	})

	t.Run("同日多次star合并为一个点", func(t *testing.T) {
		events := []time.Time{
			mustParse(t, "2026-05-01T01:00:00Z"),
			mustParse(t, "2026-05-01T23:00:00Z"),
		}
		points := aggregateDaily(events)
		if len(points) < 1 || points[0].Count != 2 {
			t.Fatalf("got points=%v, want first point Count=2", points)
		}
	})

	t.Run("跨天累计单调不减", func(t *testing.T) {
		events := []time.Time{
			mustParse(t, "2026-05-01T00:00:00Z"),
			mustParse(t, "2026-05-02T00:00:00Z"),
			mustParse(t, "2026-05-02T12:00:00Z"),
			mustParse(t, "2026-05-04T00:00:00Z"),
		}
		points := aggregateDaily(events)
		wantCounts := []int{1, 3, 4}
		if len(points) < len(wantCounts) {
			t.Fatalf("got %d points, want at least %d: %+v", len(points), len(wantCounts), points)
		}
		for i, want := range wantCounts {
			if points[i].Count != want {
				t.Errorf("points[%d].Count = %d, want %d", i, points[i].Count, want)
			}
		}
		for i := 1; i < len(points); i++ {
			if points[i].Count < points[i-1].Count {
				t.Errorf("count decreased at index %d: %+v", i, points)
			}
		}
	})

	t.Run("乱序输入结果与升序输入一致", func(t *testing.T) {
		ordered := []time.Time{
			mustParse(t, "2026-05-01T00:00:00Z"),
			mustParse(t, "2026-05-02T00:00:00Z"),
			mustParse(t, "2026-05-04T00:00:00Z"),
		}
		shuffled := []time.Time{ordered[2], ordered[0], ordered[1]}

		want := aggregateDaily(ordered)
		got := aggregateDaily(shuffled)
		if len(got) != len(want) {
			t.Fatalf("got %d points, want %d", len(got), len(want))
		}
		for i := range want {
			if !got[i].Date.Equal(want[i].Date) || got[i].Count != want[i].Count {
				t.Errorf("points[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	})
}
