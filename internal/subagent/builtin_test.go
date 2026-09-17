// 内置子代理库测试：六个定义全部合法、名字与工具白名单符合 spec §5.10。
package subagent

import (
	"strings"
	"testing"
)

func TestRegisterBuiltins(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterBuiltins(reg); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"general-purpose": nil, // 空 = 继承全部
		"explorer":        {"read_file", "glob", "grep", "bash"},
		"researcher":      {"web_search", "web_fetch", "read_file"},
		"implementer":     {"read_file", "write_file", "edit_file", "bash", "glob", "grep"},
		"reviewer":        {"read_file", "glob", "grep", "bash"},
		"planner":         {"read_file", "glob", "grep"},
	}
	if got := len(reg.List()); got != len(want) {
		t.Fatalf("内置数量 = %d, want %d", got, len(want))
	}
	for name, toolsWant := range want {
		def, ok := reg.Get(name)
		if !ok {
			t.Fatalf("缺少内置子代理 %s", name)
		}
		if def.Source != "builtin" {
			t.Fatalf("%s Source 应为 builtin", name)
		}
		if strings.Join(def.Tools, ",") != strings.Join(toolsWant, ",") {
			t.Fatalf("%s Tools = %v, want %v", name, def.Tools, toolsWant)
		}
	}
}

// TestBuiltinToolsResolvable 验证内置白名单经 ResolveTools 后不含 task 家族。
func TestBuiltinToolsResolvable(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterBuiltins(reg); err != nil {
		t.Fatal(err)
	}
	all := []string{"read_file", "write_file", "edit_file", "bash", "glob", "grep",
		"web_search", "web_fetch", "task", "task_status", "task_wait", "task_control"}
	def, _ := reg.Get("explorer")
	got := def.ResolveTools(all)
	for _, name := range got {
		if alwaysDeniedTools[name] {
			t.Fatalf("explorer 工具集不应含 %s", name)
		}
	}
}
