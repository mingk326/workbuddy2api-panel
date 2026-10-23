// key_quota.go 密钥级配额/限流的拒绝响应。
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

// writeKeyQuotaExceeded 输出 429，并带 Retry-After（秒）。
//
// 日配额用 insufficient_quota（OpenAI 语义：额度用尽，次日重置）；
// RPM / 并发用 rate_limit_exceeded（语义：请求过快，等一会再来），
// 客户端通常会对 429 做退避重试。
func writeKeyQuotaExceeded(w http.ResponseWriter, d keyquota.Decision) {
	secs := int(d.RetryAfter.Seconds() + 0.5)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	var code, msg string
	switch d.Reason {
	case "token":
		code = "insufficient_quota"
		msg = fmt.Sprintf("daily token quota exceeded (used %d / %d, resets at 00:00 CST)", d.Used, d.Limit)
	case "request":
		code = "insufficient_quota"
		msg = fmt.Sprintf("daily request quota exceeded (used %d / %d, resets at 00:00 CST)", d.Used, d.Limit)
	case "rpm":
		code = "rate_limit_exceeded"
		msg = fmt.Sprintf("requests per minute limit exceeded (used %d / %d, retry in %ds)", d.Used, d.Limit, secs)
	case "concurrency":
		code = "rate_limit_exceeded"
		msg = fmt.Sprintf("concurrent requests limit exceeded (%d in flight / %d max, retry in %ds)", d.Used, d.Limit, secs)
	default:
		code = "insufficient_quota"
		msg = fmt.Sprintf("quota exceeded (used %d / %d)", d.Used, d.Limit)
	}
	writeOpenAIError(w, http.StatusTooManyRequests, code, msg)
}
