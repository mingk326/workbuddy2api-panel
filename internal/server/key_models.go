// key_models.go 密钥级模型白名单（allowed_models）的入口拒绝与说明。
//
// 白名单匹配规则在 httpauth.Key.AllowsModel（裸名对所有域生效，带 cn:/global:
// 前缀只限定该域）。本文件只负责把"不允许"翻译成 OpenAI 兼容错误体。
package server

import (
	"fmt"
	"net/http"
	"strings"
)

// modelNotAllowed 输出 403 model_not_allowed，消息里列出该密钥可用的模型，
// 让客户端/用户一眼知道是密钥限制而不是模型不存在。
func modelNotAllowed(w http.ResponseWriter, model string, allowed []string) {
	msg := fmt.Sprintf("model %q is not allowed for this API key", model)
	if len(allowed) > 0 {
		msg += " (allowed: " + strings.Join(allowed, ", ") + ")"
	}
	writeOpenAIError(w, http.StatusForbidden, "model_not_allowed", msg)
}
