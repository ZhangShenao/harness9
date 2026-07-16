// Package main 生成 GitHub Star History 图表并写入本地 SVG 文件。
//
// GitHub 于 2026-06-30 起限制 stargazers API，仅允许仓库自身的
// owner/collaborator 读取 starred_at 时间戳，导致 star-history.com
// 等第三方服务无法再为非自有仓库生成实时徽章，README 中原有的实时图表因此失效。
// 本工具运行于 CI，使用仓库自带的 GITHUB_TOKEN（对本仓库天然具备访问权限）
// 离线抓取数据并渲染为静态 SVG 定期提交回仓库，从而彻底摆脱对第三方服务
// 运行时可用性的依赖。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	defaultAPIBase = "https://api.github.com"
	defaultPerPage = 100
	// defaultRepo 是本仓库自身维护脚本的专属默认值（非通用参数化设计），
	// 仅在 GITHUB_REPOSITORY 未设置时（如本地手动运行）回退到此仓库。
	defaultRepo    = "ZhangShenao/harness9"
	defaultOutput  = "star-history.svg"
	requestTimeout = 15 * time.Second
)

// dailyPoint 表示某一天结束时的累计 star 数。
type dailyPoint struct {
	Date  time.Time
	Count int
}

func main() {
	outputPath := flag.String("output", defaultOutput, "生成的 SVG 文件路径")
	flag.Parse()

	repo := os.Getenv("GITHUB_REPOSITORY")
	if repo == "" {
		repo = defaultRepo
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "error: GITHUB_TOKEN environment variable is required")
		os.Exit(1)
	}

	events, err := fetchStarEvents(defaultAPIBase, repo, token, defaultPerPage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	points := aggregateDaily(events)
	svg := renderSVG(repo, points)

	if err := os.WriteFile(*outputPath, []byte(svg), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: write svg: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("wrote %s (%d stars, %d data points)\n", *outputPath, len(events), len(points))
}

// fetchStarEvents 分页拉取仓库全部 stargazers 的 starred_at 时间戳，按时间升序返回。
func fetchStarEvents(apiBase, repo, token string, perPage int) ([]time.Time, error) {
	client := &http.Client{Timeout: requestTimeout}
	var all []time.Time

	for page := 1; ; page++ {
		url := fmt.Sprintf("%s/repos/%s/stargazers?per_page=%d&page=%d", apiBase, repo, perPage, page)
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		// star+json 媒体类型是拿到 starred_at 时间戳的必要条件，默认响应只返回 user 对象
		req.Header.Set("Accept", "application/vnd.github.star+json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		batch, err := requestStarPage(client, req, page)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
		if len(batch) < perPage {
			break
		}
	}

	sort.Slice(all, func(i, j int) bool { return all[i].Before(all[j]) })
	return all, nil
}

// requestStarPage 执行单页请求并解析出 starred_at 时间戳列表。
func requestStarPage(client *http.Client, req *http.Request, page int) ([]time.Time, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request stargazers page %d: %w", page, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("stargazers page %d returned %d (failed to read response body: %w)", page, resp.StatusCode, readErr)
		}
		return nil, fmt.Errorf("stargazers page %d returned %d: %s", page, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var rows []struct {
		StarredAt time.Time `json:"starred_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("decode stargazers page %d: %w", page, err)
	}

	timestamps := make([]time.Time, len(rows))
	for i, r := range rows {
		// 零值时间戳意味着响应里没有 starred_at 字段，通常是 Accept 头未被遵循、
		// 退化为默认 user 列表格式——必须报错而非静默生成一张数据错乱的图表。
		if r.StarredAt.IsZero() {
			return nil, fmt.Errorf("stargazers page %d: entry %d missing starred_at (unexpected response format)", page, i)
		}
		timestamps[i] = r.StarredAt
	}
	return timestamps, nil
}

// aggregateDaily 将逐次 star 事件聚合为按天累计的计数曲线，并延伸到当前日期。
// events 不要求调用方预先排序，函数内部会先复制并按时间升序排列。
func aggregateDaily(events []time.Time) []dailyPoint {
	if len(events) == 0 {
		return nil
	}

	sorted := make([]time.Time, len(events))
	copy(sorted, events)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Before(sorted[j]) })

	var points []dailyPoint
	cur := sorted[0].UTC().Truncate(24 * time.Hour)
	count := 0
	for _, t := range sorted {
		day := t.UTC().Truncate(24 * time.Hour)
		if day.After(cur) {
			points = append(points, dailyPoint{Date: cur, Count: count})
			cur = day
		}
		count++
	}
	points = append(points, dailyPoint{Date: cur, Count: count})

	today := time.Now().UTC().Truncate(24 * time.Hour)
	if today.After(cur) {
		points = append(points, dailyPoint{Date: today, Count: count})
	}
	return points
}
