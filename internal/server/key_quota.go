// key_quota.go 密钥级每日配额的拒绝响应。
//
// 与 keyquota 包分离：包只做计数/判定，HTTP 形状（429 + Retry-After + OpenAI 兼容
// 错误体）属于 server 层的职责。
package server

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/linguo2625469/workbuddy2api-panel/internal/keyquota"
)

// writeKeyQuotaExceeded 输出 429 insufficient_quota，并带 Retry-After
//（到次日 00:00 CST 的秒数）。
func writeKeyQuotaExceeded(w http.ResponseWriter, d keyquota.Decision) {
	secs := int(d.RetryAfter.Seconds() + 0.5)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	what := "request"
	if d.Reason == "token" {
		what = "token"
	}
	msg := fmt.Sprintf("daily %s quota exceeded (used %d / %d, resets at 00:00 CST)", what, d.Used, d.Limit)
	writeOpenAIError(w, http.StatusTooManyRequests, "insufficient_quota", msg)
}
