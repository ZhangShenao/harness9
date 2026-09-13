package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestFile 写入测试辅助文件（父目录自动创建）。
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// readTestFile 读取测试辅助文件内容。
func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(data)
}

// gunzipTestFile 解压 gzip 文件内容（测试辅助）。
func gunzipTestFile(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", path, err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("创建 gzip reader 失败: %v", err)
	}
	defer zr.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, zr); err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	return buf.String()
}

// buildRunFixture 构造一个模拟的 runner run 目录：
//   - predictions.jsonl 三条实例（bbb 的 model_patch 为空）
//   - logs/20260909-130555/ 与 logs/20260910-090000/ 两代轨迹（验证"取最新"语义）
func buildRunFixture(t *testing.T) string {
	t.Helper()
	from := t.TempDir()
	writeTestFile(t, filepath.Join(from, "predictions.jsonl"), strings.Join([]string{
		`{"instance_id":"aaa__repo-1","model_patch":"diff --git a/x b/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n","model_name_or_path":"moonshotai/kimi-k3"}`,
		`{"instance_id":"bbb__repo-2","model_patch":"","model_name_or_path":"moonshotai/kimi-k3"}`,
		`{"instance_id":"ccc__repo-3","model_patch":"diff --git a/y b/y","model_name_or_path":"moonshotai/kimi-k3"}`,
		"",
	}, "\n"))
	writeTestFile(t, filepath.Join(from, "logs", "20260909-130555", "aaa__repo-1.log"), "old traj aaa")
	writeTestFile(t, filepath.Join(from, "logs", "20260910-090000", "aaa__repo-1.log"), "new traj aaa")
	writeTestFile(t, filepath.Join(from, "logs", "20260909-130555", "bbb__repo-2.log"), "traj bbb")
	return from
}

// buildEvalFixture 构造官方 harness 评分产物目录（对齐官方 logs/<instance_id>/ 布局，
// 含一层模型子目录以验证递归定位），aaa 有 report.json + 明文 test_output.txt，
// bbb 只有已压缩的 test_output.txt.gz，ccc 缺失。
func buildEvalFixture(t *testing.T) string {
	t.Helper()
	eval := t.TempDir()
	writeTestFile(t, filepath.Join(eval, "aaa__repo-1", "moonshotai__kimi-k3", "report.json"),
		`{"resolved": true}`)
	writeTestFile(t, filepath.Join(eval, "aaa__repo-1", "moonshotai__kimi-k3", "test_output.txt"),
		"test output aaa (plain)")
	// bbb 直接写 gzip 字节（模拟官方已压缩产物）
	gzPath := filepath.Join(eval, "bbb__repo-2", "run", "test_output.txt.gz")
	if err := os.MkdirAll(filepath.Dir(gzPath), 0755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte("test output bbb (gzipped)")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gzPath, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	return eval
}

// TestExportSubmission 验证官方提交物导出：正常路径、缺产物路径与致命路径。
// 核心不变量：缺失产物不失败、不伪造，如实记录进 EXPORT_MANIFEST.md。
func TestExportSubmission(t *testing.T) {
	tests := []struct {
		name     string
		withEval bool
		wantErr  bool
		check    func(t *testing.T, out string)
	}{
		{
			name:     "完整产物导出",
			withEval: true,
			check: func(t *testing.T, out string) {
				// all_preds.jsonl：三条、逐行合法 JSON、与源 predictions 逐条一致
				lines := strings.Split(strings.TrimRight(readTestFile(t, filepath.Join(out, "all_preds.jsonl")), "\n"), "\n")
				if len(lines) != 3 {
					t.Fatalf("all_preds.jsonl 应有 3 行，got %d", len(lines))
				}
				var pred Prediction
				if err := json.Unmarshal([]byte(lines[0]), &pred); err != nil {
					t.Fatalf("all_preds.jsonl 首行不是合法 JSON: %v", err)
				}
				if pred.InstanceID != "aaa__repo-1" || pred.ModelNameOrPath != "moonshotai/kimi-k3" {
					t.Errorf("首行内容不符：%+v", pred)
				}
				// patch.diff 来自 model_patch
				if got := readTestFile(t, filepath.Join(out, "logs", "aaa__repo-1", "patch.diff")); !strings.HasPrefix(got, "diff --git a/x b/x") {
					t.Errorf("patch.diff 内容不符: %q", got)
				}
				// report.json 从评分产物复制
				if got := readTestFile(t, filepath.Join(out, "logs", "aaa__repo-1", "report.json")); got != `{"resolved": true}` {
					t.Errorf("report.json 内容不符: %q", got)
				}
				// 明文 test_output.txt 被压缩为 test_output.txt.gz
				if got := gunzipTestFile(t, filepath.Join(out, "logs", "aaa__repo-1", "test_output.txt.gz")); got != "test output aaa (plain)" {
					t.Errorf("test_output.txt.gz 解压内容不符: %q", got)
				}
				// 已压缩产物原样保留压缩格式
				if got := gunzipTestFile(t, filepath.Join(out, "logs", "bbb__repo-2", "test_output.txt.gz")); got != "test output bbb (gzipped)" {
					t.Errorf("bbb test_output.txt.gz 解压内容不符: %q", got)
				}
				// 轨迹取最新一代
				if got := readTestFile(t, filepath.Join(out, "trajs", "aaa__repo-1.md")); got != "new traj aaa" {
					t.Errorf("trajs 应取最新一代日志，got %q", got)
				}
				// manifest 记录 bbb 空 patch 与 ccc 缺评分产物
				manifest := readTestFile(t, filepath.Join(out, "EXPORT_MANIFEST.md"))
				if !strings.Contains(manifest, "bbb__repo-2") || !strings.Contains(manifest, "为空") {
					t.Errorf("manifest 应记录 bbb 空 patch:\n%s", manifest)
				}
				if !strings.Contains(manifest, "ccc__repo-3") || !strings.Contains(manifest, "缺失") {
					t.Errorf("manifest 应记录 ccc 缺失评分产物:\n%s", manifest)
				}
			},
		},
		{
			name:     "无评分产物时不失败并记录 manifest",
			withEval: false,
			check: func(t *testing.T, out string) {
				if _, err := os.Stat(filepath.Join(out, "logs", "aaa__repo-1", "report.json")); !os.IsNotExist(err) {
					t.Error("无评分产物时不应伪造 report.json")
				}
				manifest := readTestFile(t, filepath.Join(out, "EXPORT_MANIFEST.md"))
				for _, id := range []string{"aaa__repo-1", "bbb__repo-2", "ccc__repo-3"} {
					if !strings.Contains(manifest, id) {
						t.Errorf("manifest 应记录 %s 的缺失项:\n%s", id, manifest)
					}
				}
				if strings.Count(manifest, "缺失") == 0 {
					t.Errorf("manifest 应标注缺失数量:\n%s", manifest)
				}
			},
		},
		{
			name:     "predictions.jsonl 缺失报错",
			wantErr:  true,
			withEval: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "submission")
			var from, eval string
			if tc.wantErr {
				from = t.TempDir() // 空目录：无 predictions.jsonl
			} else {
				from = buildRunFixture(t)
			}
			if tc.withEval {
				eval = buildEvalFixture(t)
			}
			_, err := exportSubmission(from, out, eval)
			if tc.wantErr {
				if err == nil {
					t.Fatal("predictions.jsonl 缺失应报错")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tc.check(t, out)
		})
	}
}

// TestExportSubmissionEmptyPatchFile 验证空 model_patch 仍落盘 patch.diff（诚实反映
// 该轮模型无改动），不伪造也不省略——官方 harness 对空 patch 有独立统计口径。
func TestExportSubmissionEmptyPatchFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "submission")
	from := buildRunFixture(t)
	if _, err := exportSubmission(from, out, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := readTestFile(t, filepath.Join(out, "logs", "bbb__repo-2", "patch.diff"))
	if got != "" {
		t.Errorf("空 model_patch 应落盘为空 patch.diff，got %q", got)
	}
}

// TestIndexEvalLogs 验证评分产物索引：按实例名定位目录、报告与测试输出递归查找。
func TestIndexEvalLogs(t *testing.T) {
	eval := buildEvalFixture(t)
	arts := indexEvalLogs(eval, map[string]bool{
		"aaa__repo-1": true, "bbb__repo-2": true, "ccc__repo-3": true,
	})
	if arts == nil {
		t.Fatal("indexEvalLogs 不应返回 nil")
	}
	if arts["aaa__repo-1"] == nil || arts["aaa__repo-1"].ReportPath == "" || arts["aaa__repo-1"].TestOutPath == "" {
		t.Errorf("aaa 的评分产物应被定位到：%+v", arts["aaa__repo-1"])
	}
	if arts["bbb__repo-2"] == nil || arts["bbb__repo-2"].TestOutPath == "" {
		t.Errorf("bbb 的压缩测试输出应被定位到：%+v", arts["bbb__repo-2"])
	}
	if arts["ccc__repo-3"] != nil {
		t.Errorf("ccc 无产物，不应出现在索引中：%+v", arts["ccc__repo-3"])
	}
	// 根目录为空时返回空索引而非失败
	if arts := indexEvalLogs(t.TempDir(), map[string]bool{"x": true}); arts == nil {
		t.Error("空目录应返回空索引而非 nil")
	}
}
