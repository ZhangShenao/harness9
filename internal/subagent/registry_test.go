package subagent

import "testing"

func validDef(name string) SubAgentDefinition {
	return SubAgentDefinition{Name: name, Description: "d", SystemPrompt: "p"}
}

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(validDef("a")); err != nil {
		t.Fatal(err)
	}
	got, ok := r.Get("a")
	if !ok || got.Name != "a" {
		t.Fatalf("Get(a)=%+v ok=%v", got, ok)
	}
	if _, ok := r.Get("missing"); ok {
		t.Fatal("Get(missing) 应返回 ok=false")
	}
}

func TestRegistryRejectsDuplicate(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(validDef("a"))
	if err := r.Register(validDef("a")); err == nil {
		t.Fatal("重名注册应返回 error")
	}
}

func TestRegistryRejectsInvalid(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(SubAgentDefinition{Name: "Bad"}); err == nil {
		t.Fatal("非法定义注册应返回 error")
	}
}

func TestRegistryListSorted(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(validDef("zebra"))
	_ = r.Register(validDef("alpha"))
	_ = r.Register(validDef("mike"))
	list := r.List()
	if len(list) != 3 || list[0].Name != "alpha" || list[1].Name != "mike" || list[2].Name != "zebra" {
		t.Fatalf("List 未按名称排序：%+v", list)
	}
}
