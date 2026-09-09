package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDataset(t *testing.T) {
	content := `{"instance_id":"django__django-1","repo":"django/django","base_commit":"abc123","problem_statement":"Fix bug A","hints_text":""}
{"instance_id":"astropy__astropy-2","repo":"astropy/astropy","base_commit":"def456","problem_statement":"Fix bug B","hints_text":"hint"}
`
	tmp := filepath.Join(t.TempDir(), "lite.jsonl")
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	instances, err := loadDataset(tmp)
	if err != nil {
		t.Fatalf("loadDataset error: %v", err)
	}
	if len(instances) != 2 {
		t.Fatalf("want 2 instances, got %d", len(instances))
	}
	if instances[0].InstanceID != "django__django-1" {
		t.Errorf("want django__django-1, got %s", instances[0].InstanceID)
	}
	if instances[1].ProblemStatement != "Fix bug B" {
		t.Errorf("want 'Fix bug B', got %s", instances[1].ProblemStatement)
	}
}

func TestLoadDatasetFileNotFound(t *testing.T) {
	_, err := loadDataset("/nonexistent/path.jsonl")
	if err == nil {
		t.Fatal("want error for missing file, got nil")
	}
}

func TestSampleByRepo(t *testing.T) {
	instances := []Instance{
		{InstanceID: "django-1", Repo: "django/django"},
		{InstanceID: "django-2", Repo: "django/django"},
		{InstanceID: "django-3", Repo: "django/django"},
		{InstanceID: "astropy-1", Repo: "astropy/astropy"},
		{InstanceID: "astropy-2", Repo: "astropy/astropy"},
		{InstanceID: "flask-1", Repo: "pallets/flask"},
	}

	sampled := sampleByRepo(instances, 2, 42)

	// django 3 条取 2, astropy 2 条取 2, flask 1 条取 1 → 共 5 条
	if len(sampled) != 5 {
		t.Fatalf("want 5 sampled instances, got %d", len(sampled))
	}

	count := make(map[string]int)
	for _, inst := range sampled {
		count[inst.Repo]++
	}
	if count["django/django"] > 2 {
		t.Errorf("django sample exceeds limit: %d", count["django/django"])
	}
}

// TestLoadDataset_ParsesEnvironmentFields 验证新增的环境/评测字段被正确解析：
// version / environment_setup_commit / FAIL_TO_PASS / test_patch。
// 这些字段是为每实例 provision 依赖、选对 Python 版本、了解评测目标所必需的。
func TestLoadDataset_ParsesEnvironmentFields(t *testing.T) {
	content := `{"instance_id":"pallets__flask-4992","repo":"pallets/flask","base_commit":"abc","problem_statement":"toml","hints_text":"use text=True","version":"2.3","environment_setup_commit":"deadbeef","FAIL_TO_PASS":"[\"tests/test_config.py::test_config_from_file_toml\"]","PASS_TO_PASS":"[\"a\",\"b\"]","test_patch":"diff --git a/tests x"}
`
	tmp := filepath.Join(t.TempDir(), "lite.jsonl")
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	insts, err := loadDataset(tmp)
	if err != nil {
		t.Fatalf("loadDataset error: %v", err)
	}
	if len(insts) != 1 {
		t.Fatalf("want 1 instance, got %d", len(insts))
	}
	got := insts[0]
	if got.Version != "2.3" {
		t.Errorf("Version: want 2.3, got %q", got.Version)
	}
	if got.EnvironmentSetupCommit != "deadbeef" {
		t.Errorf("EnvironmentSetupCommit: want deadbeef, got %q", got.EnvironmentSetupCommit)
	}
	if got.FailToPass == "" {
		t.Errorf("FailToPass should be populated, got empty")
	}
	if got.TestPatch == "" {
		t.Errorf("TestPatch should be populated, got empty")
	}
}

// TestParseTestIDs 验证把 SWE-bench 中"JSON 编码为字符串的测试 ID 数组"解析为 []string。
func TestParseTestIDs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"normal", `["tests/test_config.py::test_toml", "tests/test_cli.py::TestRoutes::test_host"]`, []string{"tests/test_config.py::test_toml", "tests/test_cli.py::TestRoutes::test_host"}},
		{"single", `["x::y"]`, []string{"x::y"}},
		{"empty_string", ``, nil},
		{"empty_array", `[]`, nil},
		{"malformed", `not json`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseTestIDs(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("len mismatch: want %v, got %v", c.want, got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("idx %d: want %q, got %q", i, c.want[i], got[i])
				}
			}
		})
	}
}

// TestLoadInstanceFilter 验证清单解析：每行一个 id，# 注释与空行忽略，首尾空白裁剪。
func TestLoadInstanceFilter(t *testing.T) {
	content := "# 冒烟清单：v3 run1 子集\n\ndjango__django-12908\n  astropy__astropy-7746  \n# 下一行是注释\npsf__requests-1963\n"
	tmp := filepath.Join(t.TempDir(), "instances.txt")
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	filter, err := loadInstanceFilter(tmp)
	if err != nil {
		t.Fatalf("loadInstanceFilter error: %v", err)
	}
	if len(filter) != 3 {
		t.Fatalf("want 3 ids, got %d: %v", len(filter), filter)
	}
	for _, id := range []string{"django__django-12908", "astropy__astropy-7746", "psf__requests-1963"} {
		if !filter[id] {
			t.Errorf("filter 应包含 %s", id)
		}
	}
}

// TestLoadInstanceFilterFileNotFound 验证清单文件缺失时报错（与 dataset 缺失同级别处理）。
func TestLoadInstanceFilterFileNotFound(t *testing.T) {
	_, err := loadInstanceFilter("/nonexistent/instances.txt")
	if err == nil {
		t.Fatal("want error for missing file, got nil")
	}
}

// TestFilterInstances 验证交集过滤：保持原顺序，missing 统计清单中数据集没有的 id。
func TestFilterInstances(t *testing.T) {
	instances := []Instance{
		{InstanceID: "django-1", Repo: "django/django"},
		{InstanceID: "django-2", Repo: "django/django"},
		{InstanceID: "flask-1", Repo: "pallets/flask"},
	}
	filter := map[string]bool{"django-2": true, "flask-1": true, "ghost-9": true}
	filtered, missing := filterInstances(instances, filter)
	if len(filtered) != 2 {
		t.Fatalf("want 2 filtered, got %d", len(filtered))
	}
	if filtered[0].InstanceID != "django-2" || filtered[1].InstanceID != "flask-1" {
		t.Fatalf("过滤结果应保持原顺序，got %v", filtered)
	}
	if missing != 1 {
		t.Fatalf("want 1 missing, got %d", missing)
	}
}

// TestSampleByRepo_SupersetProperty 验证采样超集性质：同 seed 下小 n 的实例集
// 是大 n 的子集（dataset.go 的 rng 只播种一次，per-repo shuffle 序列与 n 无关）。
// 这是 v4 复用 v3 的 47 条做参考对照的数学基础。
func TestSampleByRepo_SupersetProperty(t *testing.T) {
	instances := make([]Instance, 0, 20)
	for i := 1; i <= 10; i++ {
		instances = append(instances,
			Instance{InstanceID: fmt.Sprintf("django-%d", i), Repo: "django/django"},
			Instance{InstanceID: fmt.Sprintf("flask-%d", i), Repo: "pallets/flask"},
		)
	}
	small := sampleByRepo(instances, 2, 1)
	large := sampleByRepo(instances, 4, 1)
	inLarge := make(map[string]bool, len(large))
	for _, inst := range large {
		inLarge[inst.InstanceID] = true
	}
	for _, inst := range small {
		if !inLarge[inst.InstanceID] {
			t.Fatalf("同 seed 下 n=2 的实例 %s 应包含于 n=4 的采样结果（超集性质被破坏）", inst.InstanceID)
		}
	}
}
