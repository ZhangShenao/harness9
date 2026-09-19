package subagent

import (
	"sort"
	"testing"
)

func TestDefinitionValidate(t *testing.T) {
	tests := []struct {
		name    string
		def     SubAgentDefinition
		wantErr bool
	}{
		{"valid", SubAgentDefinition{Name: "code-reviewer", Description: "d", SystemPrompt: "p"}, false},
		{"empty name", SubAgentDefinition{Description: "d", SystemPrompt: "p"}, true},
		{"bad name uppercase", SubAgentDefinition{Name: "Bad", Description: "d", SystemPrompt: "p"}, true},
		{"bad name space", SubAgentDefinition{Name: "a b", Description: "d", SystemPrompt: "p"}, true},
		{"empty desc", SubAgentDefinition{Name: "x", SystemPrompt: "p"}, true},
		{"empty prompt", SubAgentDefinition{Name: "x", Description: "d"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.def.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

// TestResolveToolsStripsTaskFamily 验证协调工具家族永远不出现在子代理工具集
// （防递归 + 防越权操纵主 tracker，spec §7.1）。
func TestResolveToolsStripsTaskFamily(t *testing.T) {
	def := SubAgentDefinition{
		Name:  "evil",
		Tools: []string{"read_file", "task", "task_status", "task_wait", "task_control"},
	}
	got := def.ResolveTools([]string{"read_file", "bash", "task", "task_status", "task_wait", "task_control"})
	want := []string{"read_file"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("ResolveTools = %v, want %v", got, want)
	}
}

func TestResolveTools(t *testing.T) {
	all := []string{"read_file", "write_file", "bash", "edit_file", "task"}

	tests := []struct {
		name string
		def  SubAgentDefinition
		want []string
	}{
		{
			name: "nil tools inherits all minus task",
			def:  SubAgentDefinition{},
			want: []string{"bash", "edit_file", "read_file", "write_file"},
		},
		{
			name: "allowlist",
			def:  SubAgentDefinition{Tools: []string{"read_file", "bash"}},
			want: []string{"bash", "read_file"},
		},
		{
			name: "allowlist drops unknown",
			def:  SubAgentDefinition{Tools: []string{"read_file", "nonexist"}},
			want: []string{"read_file"},
		},
		{
			name: "denylist",
			def:  SubAgentDefinition{DisallowedTools: []string{"bash", "write_file"}},
			want: []string{"edit_file", "read_file"},
		},
		{
			name: "task always removed even if whitelisted",
			def:  SubAgentDefinition{Tools: []string{"read_file", "task"}},
			want: []string{"read_file"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.def.ResolveTools(all)
			sort.Strings(got)
			if len(got) != len(tt.want) {
				t.Fatalf("ResolveTools()=%v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("ResolveTools()=%v, want %v", got, tt.want)
				}
			}
		})
	}
}
