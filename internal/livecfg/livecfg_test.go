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

// TestNormalizeModelAliases 别名归一化：键小写、裸名两域、前缀只该域、自映射丢弃。
func TestNormalizeModelAliases(t *testing.T) {
	m := NormalizeModelAliases([]string{
		"Claude-Opus-4 = glm-5.2",
		"cn:kimi-k3 = global:glm-5.3",
		"self=self",
		"bad-no-eq",
		"=x",
		"y=",
	})
	if m["cn:claude-opus-4"] != "glm-5.2" || m["global:claude-opus-4"] != "glm-5.2" {
		t.Errorf("裸名键应两域生效: %v", m)
	}
	if m["cn:kimi-k3"] != "global:glm-5.3" {
		t.Errorf("cn: 前缀键应只在该域: %v", m)
	}
	if _, ok := m["global:kimi-k3"]; ok {
		t.Errorf("cn: 前缀键不应扩散到 global: %v", m)
	}
	if _, ok := m["cn:self"]; ok {
		t.Errorf("自映射应丢弃: %v", m)
	}
	if len(m) != 3 {
		t.Errorf("len=%d want 3 (%v)", len(m), m)
	}
	if NormalizeModelAliases(nil) != nil {
		t.Error("nil input should return nil")
	}
}
func TestStoreCopiesHiddenModels(t *testing.T) {
	src := map[string]struct{}{"cn:glm-5.2": {}}
	h := New(Snapshot{HiddenModels: src})
	delete(src, "cn:glm-5.2")
	if _, ok := h.Load().HiddenModels["cn:glm-5.2"]; !ok {
		t.Error("快照不应受外部 map 修改影响")
	}
}
