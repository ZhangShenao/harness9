package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 以下两个常量是 astropy-14182 / astropy-14365 评分失败后从 eval 侧 patch.diff
// 原样提取的截断样本（runner.go 旧实现把被超时杀死的半截 git diff 静默提交所致）。
// 它们是 validateUnifiedDiff 的回归用例：校验器必须能识别这两类截断。

// truncatedPatch14182 复刻 14182：hunk 头声明 +1,3 且 3 行齐全，但结尾缺少终止换行——
// git diff 的真实输出永远以换行结尾，缺换行即证明输出流被截断（容器内
// `patch` 报 "unexpectedly ends in middle of line"）。
const truncatedPatch14182 = "diff --git a/docs/changes/io.ascii/14182.feature.rst b/docs/changes/io.ascii/14182.feature.rst\n" +
	"new file mode 100644\n" +
	"index 000000000..34ac5a5cc\n" +
	"--- /dev/null\n" +
	"+++ b/docs/changes/io.ascii/14182.feature.rst\n" +
	"@@ -0,0 +1,3 @@\n" +
	"+The ``RST`` writer now accepts the ``header_rows`` keyword, allowing the\n" +
	"+output of additional header rows (e.g. ``unit``) like the other fixed-width\n" +
	"+writers."

// truncatedPatch14365 复刻 14365：hunk 头声明 -1,7 +1,7，正文只有 4 行就戛然而止
// 且无终止换行（容器内报 "malformed patch at line 31"）。
const truncatedPatch14365 = "diff --git a/astropy/io/ascii/qdp.py b/astropy/io/ascii/qdp.py\n" +
	"index 111111111..222222222 100644\n" +
	"--- a/astropy/io/ascii/qdp.py\n" +
	"+++ b/astropy/io/ascii/qdp.py\n" +
	"@@ -1,7 +1,7 @@\n" +
	" import re\n" +
	" import warnings\n" +
	" from pathlib import Path\n" +
	"-import numpy as np\n" +
	"+import numpy"

// validMultiFilePatch 是结构完整的多文件 diff：上下文行、纯增、纯删、
// 新建文件、二进制提示、"\ No newline" 标记各覆盖一例。
const validMultiFilePatch = `diff --git a/pkg/a.py b/pkg/a.py
index 111111111..222222222 100644
--- a/pkg/a.py
+++ b/pkg/a.py
@@ -1,4 +1,5 @@ def helper():
 context line stays
-removed line
+added line
+another added line
 second context
 third context
\ No newline at end of file
diff --git a/pkg/new.py b/pkg/new.py
new file mode 100644
index 000000000..333333333
--- /dev/null
+++ b/pkg/new.py
@@ -0,0 +1,2 @@
+fresh line one
+fresh line two
diff --git a/pkg/old.py b/pkg/old.py
deleted file mode 100644
index 444444444..000000000
--- a/pkg/old.py
+++ /dev/null
@@ -1,2 +0,0 @@
-doomed one
-doomed two
diff --git a/pkg/logo.png b/pkg/logo.png
index 555555555..666666666 100644
Binary files a/pkg/logo.png and b/pkg/logo.png differ
`

func TestValidateUnifiedDiff(t *testing.T) {
	tests := []struct {
		name    string
		patch   string
		wantErr bool
	}{
		{name: "空 patch 合法（无改动）", patch: ""},
		{name: "完整多文件 diff", patch: validMultiFilePatch},
		{name: "仅模式变更无 hunk", patch: "diff --git a/x.sh b/x.sh\nold mode 100644\nnew mode 100755\n"},
		{name: "14182 型截断：缺终止换行", patch: truncatedPatch14182, wantErr: true},
		{name: "14365 型截断：hunk 行数不足且缺换行", patch: truncatedPatch14365, wantErr: true},
		{name: "hunk 行数多于声明（溢出污染）", patch: `diff --git a/x.py b/x.py
index 111111111..222222222 100644
--- a/x.py
+++ b/x.py
@@ -1,2 +1,2 @@
-a
+b
+extra
`, wantErr: true},
		{name: "hunk 体内混入非 diff 行（stderr 污染形态）", patch: `diff --git a/x.py b/x.py
index 111111111..222222222 100644
--- a/x.py
+++ b/x.py
@@ -1,2 +1,2 @@
-a
warning: some git noise here
+b
`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateUnifiedDiff(tt.patch)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateUnifiedDiff() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

// TestCollectPatch 用真实 git 仓库验证完整提取链路：已跟踪文件修改 + write_file
// 式新建文件都必须进入 diff（intent-to-add 语义），且输出通过完整性校验。
func TestCollectPatch(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "tracked.py"), []byte("orig = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "init")

	// 模拟 agent：修改已跟踪文件 + 新建文件（未 add）
	if err := os.WriteFile(filepath.Join(dir, "tracked.py"), []byte("fixed = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "brand_new.py"), []byte("new = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	patch, err := collectPatch(dir)
	if err != nil {
		t.Fatalf("collectPatch() error = %v", err)
	}
	// git diff 的合法输出恒以终止换行结尾；补丁必须原样保留它——
	// 剥掉换行的补丁在 eval 容器里会被 git apply 判为 corrupt（2026-09-10
	// 验证轮 3 例 Patch Apply Failed 的根因，实弹复现确认）。
	if !strings.HasSuffix(patch, "\n") {
		t.Fatalf("collectPatch() 输出缺少终止换行，结尾: %q", patch[max(0, len(patch)-40):])
	}
	for _, want := range []string{"diff --git a/tracked.py", "-orig = 1", "+fixed = 2", "new file mode", "+new = 1"} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch 缺少 %q\npatch:\n%s", want, patch)
		}
	}
}

func TestFinalEditUnverified(t *testing.T) {
	tests := []struct {
		name  string
		stats streamStats
		want  bool
	}{
		{name: "跑过测试且测试在最后一次改动之后", stats: streamStats{ranTest: true, lastEditTurn: 3, lastTestTurn: 5}, want: false},
		{name: "跑过测试但之后又改了代码（最后一改未验证）", stats: streamStats{ranTest: true, lastEditTurn: 7, lastTestTurn: 5}, want: true},
		{name: "全程没跑过测试（属验证关卡管辖，不重复计）", stats: streamStats{ranTest: false, lastEditTurn: 7, lastTestTurn: 0}, want: false},
		{name: "跑过测试但从未改动", stats: streamStats{ranTest: true, lastEditTurn: 0, lastTestTurn: 3}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := finalEditUnverified(tt.stats); got != tt.want {
				t.Fatalf("finalEditUnverified(%+v) = %v, want %v", tt.stats, got, tt.want)
			}
		})
	}
}

func TestMergeStatsTruncationFields(t *testing.T) {
	a := streamStats{ranTest: true, planWrites: 1, maxTurn: 10, lastEditTurn: 8, lastTestTurn: 3}
	b := streamStats{ranTest: false, planWrites: 2, maxTurn: 20, lastEditTurn: 5, lastTestTurn: 15}
	got := mergeStats(a, b)
	if got.lastEditTurn != 8 || got.lastTestTurn != 15 {
		t.Fatalf("mergeStats 取大失败：lastEditTurn=%d lastTestTurn=%d", got.lastEditTurn, got.lastTestTurn)
	}
	if !got.ranTest || got.planWrites != 3 || got.maxTurn != 20 {
		t.Fatalf("mergeStats 原有语义被破坏：%+v", got)
	}
}
