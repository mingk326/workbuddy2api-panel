package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// sseWithContent 一条正文 + [DONE] 的最小合法 SSE（空流会被记为 502）。
const sseWithContent = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
	"event: ping\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

// thinkingOverrideUpstream 记录出站 body 的假上游（回最小 SSE）。
func thinkingOverrideUpstream(bodies *[]string) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			*bodies = append(*bodies, string(raw))
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseWithContent)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// TestChatCompletionsInjectsThinkingOverride 端到端：配置 thinking.default_effort 后，
// 客户端不带档位的请求出站应带注入的 reasoning_effort；客户端显式带档位不覆盖。
func TestChatCompletionsInjectsThinkingOverride(t *testing.T) {
	var bodies []string
	up := thinkingOverrideUpstream(&bodies)
	up.SetThinkingOverride("high", nil)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(bodies) == 0 || !strings.Contains(bodies[0], `"reasoning_effort":"high"`) {
		t.Fatalf("出站缺注入档位: %v", bodies)
	}

	// 显式档位不被覆盖。
	bodies = nil
	up.SetThinkingOverride("high", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(bodies) == 0 || !strings.Contains(bodies[0], `"reasoning_effort":"low"`) {
		t.Fatalf("显式档位被覆盖: %v", bodies)
	}

	// 关闭（全空）零注入。
	bodies = nil
	up.SetThinkingOverride("", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(bodies) > 0 && strings.Contains(bodies[0], "reasoning_effort") {
		t.Fatalf("关闭后不应注入: %v", bodies)
	}
}
