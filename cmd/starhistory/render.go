package main

import (
	"fmt"
	"strings"
	"time"
)

const (
	svgWidth  = 860
	svgHeight = 420
	padLeft   = 56
	padRight  = 24
	padTop    = 48
	padBottom = 40
	gridLines = 4
	xTicks    = 5
)

// renderSVG 将按天累计的 star 曲线渲染为一张支持 GitHub 明暗主题自适应的 SVG 图表。
func renderSVG(repo string, points []dailyPoint) string {
	plotW := float64(svgWidth - padLeft - padRight)
	plotH := float64(svgHeight - padTop - padBottom)

	minDate, maxDate := dateRange(points)
	hoursSpan := maxDate.Sub(minDate).Hours()
	if hoursSpan <= 0 {
		hoursSpan = 1
	}
	yMax := float64(maxCount(points)) * 1.15
	if yMax <= 0 {
		yMax = 1
	}

	x := func(d time.Time) float64 { return float64(padLeft) + d.Sub(minDate).Hours()/hoursSpan*plotW }
	y := func(c int) float64 { return float64(padTop) + plotH - float64(c)/yMax*plotH }

	linePath, areaPath := buildPaths(points, x, y)

	generated := time.Now().UTC().Format("2006-01-02 15:04 UTC")

	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Helvetica,Arial,sans-serif">
  <style>
    .bg { fill: #ffffff; }
    .grid { stroke: #d0d7de; stroke-width: 1; }
    .axis { fill: #57606a; font-size: 12px; }
    .title { fill: #24292f; font-size: 16px; font-weight: 600; }
    .footer { fill: #8c959f; font-size: 11px; }
    .area { fill: #2f81f7; opacity: 0.12; }
    .line { stroke: #2f81f7; stroke-width: 2; fill: none; }
    @media (prefers-color-scheme: dark) {
      .bg { fill: #0d1117; }
      .grid { stroke: #30363d; }
      .axis { fill: #8b949e; }
      .title { fill: #c9d1d9; }
      .footer { fill: #6e7681; }
      .area { fill: #58a6ff; opacity: 0.15; }
      .line { stroke: #58a6ff; }
    }
  </style>
  <rect class="bg" x="0" y="0" width="%d" height="%d"/>
  <text class="title" x="%d" y="26">%s Star History</text>
  %s
  %s
  <path class="area" d="%s"/>
  <path class="line" d="%s"/>
  %s
  <text class="footer" x="%d" y="%d" text-anchor="end">自动生成 · %s</text>
</svg>`,
		svgWidth, svgHeight, svgWidth, svgHeight,
		svgWidth, svgHeight,
		padLeft, repo,
		renderYGrid(plotH),
		renderYLabels(yMax, plotH),
		areaPath,
		linePath,
		renderXLabels(minDate, hoursSpan, x),
		svgWidth-padRight, svgHeight-6, generated,
	)
}

// dateRange 返回数据点覆盖的起止日期，空数据时退化为当前日期的单点区间。
func dateRange(points []dailyPoint) (time.Time, time.Time) {
	if len(points) == 0 {
		now := time.Now().UTC()
		return now, now
	}
	return points[0].Date, points[len(points)-1].Date
}

// maxCount 返回数据点中的最大累计值，空数据时返回 0。
func maxCount(points []dailyPoint) int {
	m := 0
	for _, p := range points {
		m = max(m, p.Count)
	}
	return m
}

// buildPaths 将数据点转换为折线与其下方填充区域的 SVG path 描述。
func buildPaths(points []dailyPoint, x func(time.Time) float64, y func(int) float64) (line, area string) {
	if len(points) == 0 {
		return "", ""
	}
	coords := make([]string, len(points))
	for i, p := range points {
		coords[i] = fmt.Sprintf("%.1f,%.1f", x(p.Date), y(p.Count))
	}
	line = "M " + strings.Join(coords, " L ")
	area = fmt.Sprintf("%s L %.1f,%.1f L %.1f,%.1f Z", line, x(points[len(points)-1].Date), y(0), x(points[0].Date), y(0))
	return line, area
}

// renderYGrid 绘制水平网格线。
func renderYGrid(plotH float64) string {
	var b strings.Builder
	for i := 0; i <= gridLines; i++ {
		frac := float64(i) / float64(gridLines)
		gy := float64(padTop) + plotH*(1-frac)
		fmt.Fprintf(&b, `<line class="grid" x1="%d" y1="%.1f" x2="%d" y2="%.1f"/>`, padLeft, gy, svgWidth-padRight, gy)
	}
	return b.String()
}

// renderYLabels 绘制纵轴的累计 star 数刻度标签。
func renderYLabels(yMax, plotH float64) string {
	var b strings.Builder
	for i := 0; i <= gridLines; i++ {
		frac := float64(i) / float64(gridLines)
		gy := float64(padTop) + plotH*(1-frac)
		val := int(yMax * frac)
		fmt.Fprintf(&b, `<text class="axis" x="%d" y="%.1f" text-anchor="end">%d</text>`, padLeft-8, gy+4, val)
	}
	return b.String()
}

// renderXLabels 绘制横轴的日期刻度标签。
func renderXLabels(minDate time.Time, hoursSpan float64, x func(time.Time) float64) string {
	var b strings.Builder
	for i := 0; i <= xTicks; i++ {
		frac := float64(i) / float64(xTicks)
		t := minDate.Add(time.Duration(hoursSpan*frac) * time.Hour)
		fmt.Fprintf(&b, `<text class="axis" x="%.1f" y="%d" text-anchor="middle">%s</text>`, x(t), svgHeight-padBottom+20, t.Format("2006-01-02"))
	}
	return b.String()
}
