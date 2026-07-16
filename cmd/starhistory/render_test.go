package main

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func day(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tm
}

// TestDateRange 验证起止日期取自首末数据点，空数据退化为当前日期的单点区间。
func TestDateRange(t *testing.T) {
	t.Run("空数据", func(t *testing.T) {
		min, max := dateRange(nil)
		if min.IsZero() || max.IsZero() || !min.Equal(max) {
			t.Errorf("dateRange(nil) = (%v, %v), want equal non-zero timestamps", min, max)
		}
	})

	t.Run("多点取首末", func(t *testing.T) {
		points := []dailyPoint{
			{Date: day(t, "2026-05-01"), Count: 1},
			{Date: day(t, "2026-05-10"), Count: 5},
			{Date: day(t, "2026-05-20"), Count: 9},
		}
		min, max := dateRange(points)
		if !min.Equal(points[0].Date) || !max.Equal(points[2].Date) {
			t.Errorf("dateRange = (%v, %v), want (%v, %v)", min, max, points[0].Date, points[2].Date)
		}
	})
}

// TestMaxCount 验证最大值提取，空数据返回 0。
func TestMaxCount(t *testing.T) {
	cases := []struct {
		name   string
		points []dailyPoint
		want   int
	}{
		{"空数据", nil, 0},
		{"单点", []dailyPoint{{Count: 7}}, 7},
		{"非单调也能取到最大值", []dailyPoint{{Count: 3}, {Count: 9}, {Count: 5}}, 9},
	}
	for _, c := range cases {
		if got := maxCount(c.points); got != c.want {
			t.Errorf("%s: maxCount() = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestBuildPaths 验证空数据返回空 path，非空数据首尾闭合成合法的填充区域。
func TestBuildPaths(t *testing.T) {
	x := func(t time.Time) float64 { return float64(t.Unix()) }
	y := func(c int) float64 { return float64(100 - c) }

	t.Run("空数据", func(t *testing.T) {
		line, area := buildPaths(nil, x, y)
		if line != "" || area != "" {
			t.Errorf("buildPaths(nil) = (%q, %q), want empty strings", line, area)
		}
	})

	t.Run("非空数据以M开头且area以Z闭合", func(t *testing.T) {
		points := []dailyPoint{
			{Date: day(t, "2026-05-01"), Count: 1},
			{Date: day(t, "2026-05-02"), Count: 2},
		}
		line, area := buildPaths(points, x, y)
		if !strings.HasPrefix(line, "M ") {
			t.Errorf("line path = %q, want prefix %q", line, "M ")
		}
		if !strings.HasSuffix(area, "Z") {
			t.Errorf("area path = %q, want suffix %q", area, "Z")
		}
		if !strings.HasPrefix(area, line) {
			t.Errorf("area path should extend line path, line=%q area=%q", line, area)
		}
	})
}

// TestRenderSVGWellFormed 验证渲染结果是合法 XML，且包含仓库名与预期结构元素。
func TestRenderSVGWellFormed(t *testing.T) {
	points := []dailyPoint{
		{Date: day(t, "2026-05-01"), Count: 1},
		{Date: day(t, "2026-06-01"), Count: 10},
		{Date: day(t, "2026-07-01"), Count: 42},
	}
	svg := renderSVG("owner/repo", points)

	if !strings.Contains(svg, "owner/repo") {
		t.Errorf("svg does not contain repo name, got: %s", svg)
	}
	if !strings.Contains(svg, "prefers-color-scheme: dark") {
		t.Errorf("svg missing dark-mode media query")
	}
	if err := xml.Unmarshal([]byte(svg), new(any)); err != nil {
		t.Errorf("renderSVG output is not well-formed XML: %v", err)
	}
}

// TestRenderSVGEmptyPoints 验证无数据点时不会 panic 或产生除零错误。
func TestRenderSVGEmptyPoints(t *testing.T) {
	svg := renderSVG("owner/repo", nil)
	if err := xml.Unmarshal([]byte(svg), new(any)); err != nil {
		t.Errorf("renderSVG(nil) output is not well-formed XML: %v", err)
	}
}
