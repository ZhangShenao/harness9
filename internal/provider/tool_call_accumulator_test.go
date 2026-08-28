package provider

import "testing"

// TestFinalize_ContiguousIndices 验证常规（OpenAI）密集 index 从 0 起的情形。
func TestFinalize_ContiguousIndices(t *testing.T) {
	a := newToolCallAccumulators()
	a.start(0, "id0", "bash")
	a.appendArgs(0, `{"command":"ls"}`)
	a.start(1, "id1", "read_file")
	a.appendArgs(1, `{"path":"x"}`)

	got := a.finalize()
	if len(got) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(got))
	}
	if got[0].ID != "id0" || got[1].ID != "id1" {
		t.Errorf("顺序/ID 错误：%+v", got)
	}
}

// TestFinalize_SparseIndices 是核心回归测试：当 index 不从 0 开始（Anthropic 在
// thinking/text 块占据 index 0 时，tool_use 块从 index 1 起，key 集合形如 {1,2}），
// 旧实现 `for i:=0;i<len(a);i++` 会漏掉末尾工具调用。这里验证全部被保留且按 index 升序。
func TestFinalize_SparseIndices(t *testing.T) {
	a := newToolCallAccumulators()
	a.start(1, "id1", "bash")
	a.appendArgs(1, `{"command":"a"}`)
	a.start(2, "id2", "edit_file")
	a.appendArgs(2, `{"path":"b"}`)

	got := a.finalize()
	if len(got) != 2 {
		t.Fatalf("稀疏 index 时应保留全部 2 个工具调用，got %d（旧 bug 会漏掉）", len(got))
	}
	if got[0].ID != "id1" || got[1].ID != "id2" {
		t.Errorf("应按 index 升序：%+v", got)
	}
}

// TestFinalize_OutOfOrderArrival 验证乱序到达的 index 也按升序输出。
func TestFinalize_OutOfOrderArrival(t *testing.T) {
	a := newToolCallAccumulators()
	a.start(3, "id3", "t3")
	a.start(1, "id1", "t1")
	a.start(2, "id2", "t2")

	got := a.finalize()
	if len(got) != 3 {
		t.Fatalf("expected 3, got %d", len(got))
	}
	want := []string{"id1", "id2", "id3"}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("位置 %d: got %q, want %q", i, got[i].ID, w)
		}
	}
}

// TestFinalize_Empty 验证无工具调用时返回 nil。
func TestFinalize_Empty(t *testing.T) {
	a := newToolCallAccumulators()
	if got := a.finalize(); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}
