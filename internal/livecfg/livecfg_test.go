package livecfg

import "testing"

// TestNormalizeHiddenModels 裸名展开两域；cn:/global: 前缀只藏该域；空项丢弃。
func TestNormalizeHiddenModels(t *testing.T) {
	m := NormalizeHiddenModels([]string{" glm-5.2 ", "cn:kimi-k3-1", "global:hy3", "", ":"})
	want := []string{"cn:glm-5.2", "global:glm-5.2", "cn:kimi-k3-1", "global:hy3", "cn::", "global::"}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %q in %v", k, m)
		}
	}
	if len(m) != len(want) {
		t.Errorf("len=%d want %d (%v)", len(m), len(want), m)
	}
	if NormalizeHiddenModels(nil) != nil {
		t.Error("nil input should return nil")
	}
	if NormalizeHiddenModels([]string{"  "}) != nil {
		t.Error("only blanks should return nil")
	}
}

// TestStoreCopiesHiddenModels Store 后外部修改原 map 不影响快照（快照不可变）。
func TestStoreCopiesHiddenModels(t *testing.T) {
	src := map[string]struct{}{"cn:glm-5.2": {}}
	h := New(Snapshot{HiddenModels: src})
	delete(src, "cn:glm-5.2")
	if _, ok := h.Load().HiddenModels["cn:glm-5.2"]; !ok {
		t.Error("快照不应受外部 map 修改影响")
	}
}
