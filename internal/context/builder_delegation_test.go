package context

import (
	"strings"
	"testing"
)

// TestBuild_DelegationSection 验证子代理委派准则段的注入开关：
// WithDelegationGuide(true) 时包含委派指引（含 task_wait 等协调工具）；
// 未启用时整段缺失（准则仅在 task 系工具已注册时注入）。
func TestBuild_DelegationSection(t *testing.T) {
	b := NewPromptBuilder(t.TempDir(), nil).WithDelegationGuide(true)
	out := b.Build()
	for _, want := range []string{"子代理委派", "task_wait"} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt should contain %q, got: %s", want, out)
		}
	}

	// 未启用时不注入
	out2 := NewPromptBuilder(t.TempDir(), nil).Build()
	if strings.Contains(out2, "子代理委派") {
		t.Error("delegation section should be absent when disabled")
	}
}
