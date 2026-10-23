// models.go 密钥级模型白名单（allowed_models）。
//
// 语义（与面板/配置同口径）：
//   - 空名单 = 不限制（旧密钥零回归，config.api_key 恒不限制）；
//   - 裸名条目 "glm-5.3"：cn / global 两个域都允许；
//   - 带域前缀条目 "global:hy-image-3.5"：只允许该域的该模型。
//
// realm/bare 由调用方用与出站路由同一套解析（server.resolveModel）得出，
// 本包不重复实现模型名协议。
package httpauth

import "strings"

// AllowsModel 报告 realm 域下的模型 bare 是否在该密钥的白名单内。
func (k Key) AllowsModel(realm, bare string) bool {
	if len(k.AllowedModels) == 0 {
		return true
	}
	for _, e := range k.AllowedModels {
		if e == bare {
			return true
		}
		if r, b := splitModelEntry(e); r != "" && r == realm && b == bare {
			return true
		}
	}
	return false
}

// splitModelEntry 拆 "realm:model"；前缀不是 cn/global 时按裸名整体处理
// （与 server.resolveModel 一致：模型名本身含冒号时不被误拆）。
func splitModelEntry(s string) (string, string) {
	if i := strings.IndexByte(s, ':'); i > 0 {
		switch p := s[:i]; p {
		case "cn", "global":
			return p, s[i+1:]
		}
	}
	return "", s
}
