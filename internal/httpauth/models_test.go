package httpauth

import "testing"

// TestKeyAllowsModel 白名单匹配口径：空 = 不限制；裸名跨域生效；
// 带 cn:/global: 前缀只限定该域；未知前缀按裸名整体匹配。
func TestKeyAllowsModel(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		realm   string
		bare    string
		want    bool
	}{
		{"空名单不限制", nil, "cn", "glm-5.3", true},
		{"空数组不限制", []string{}, "global", "hy3", true},
		{"裸名匹配 cn", []string{"glm-5.3"}, "cn", "glm-5.3", true},
		{"裸名匹配 global", []string{"glm-5.3"}, "global", "glm-5.3", true},
		{"裸名不匹配其他模型", []string{"glm-5.3"}, "cn", "kimi-k3", false},
		{"域前缀匹配同域", []string{"global:hy-image-3.5"}, "global", "hy-image-3.5", true},
		{"域前缀不匹配另一域", []string{"global:hy-image-3.5"}, "cn", "hy-image-3.5", false},
		{"未知前缀按裸名整体", []string{"openai:gpt-x"}, "cn", "openai:gpt-x", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := Key{AllowedModels: c.allowed}
			if got := k.AllowsModel(c.realm, c.bare); got != c.want {
				t.Errorf("AllowsModel(%q,%q) with %v = %v want %v", c.realm, c.bare, c.allowed, got, c.want)
			}
		})
	}
}
