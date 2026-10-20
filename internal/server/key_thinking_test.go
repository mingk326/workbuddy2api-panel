package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

// TestPerKeyThinkingOverride 端到端验证按 API 密钥注入思考档位的完整优先级链：
// 客户端显式 > 密钥 per_model > 密钥默认 > 全局 thinking.*。
func TestPerKeyThinkingOverride(t *testing.T) {
	var bodies []string
	up := thinkingOverrideUpstream(&bodies)
	up.SetThinkingOverride("high", nil) // 全局兜底：admin 密钥应命中 high

	live := livecfg.New(livecfg.Snapshot{APIKeys: []httpauth.Key{
		{ID: "admin", Secret: "sk-admin", Admin: true},
		{ID: "codex", Secret: "sk-codex", DefaultEffort: "max",
			PerModel: map[string]string{"glm-5.2": "low"}},
	}})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: up, Live: live})

	do := func(key, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		h.ServeHTTP(rec, req)
		return rec
	}
	lastBody := func() string {
		if len(bodies) == 0 {
			t.Fatal("上游没有收到请求")
		}
		return bodies[len(bodies)-1]
	}

	// 1) 密钥 per_model 优先于密钥默认与全局。
	bodies = nil
	if rec := do("sk-codex", `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if b := lastBody(); !strings.Contains(b, `"reasoning_effort":"low"`) {
		t.Fatalf("密钥 per_model 未生效: %s", b)
	}

	// 2) 未命中 per_model → 密钥默认（max）。
	bodies = nil
	if rec := do("sk-codex", `{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if b := lastBody(); !strings.Contains(b, `"reasoning_effort":"max"`) {
		t.Fatalf("密钥默认档未生效: %s", b)
	}

	// 3) 客户端显式档位不被密钥覆盖。
	bodies = nil
	if rec := do("sk-codex", `{"model":"glm-5.2","reasoning_effort":"medium","messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if b := lastBody(); !strings.Contains(b, `"reasoning_effort":"medium"`) {
		t.Fatalf("客户端显式档位被覆盖: %s", b)
	}

	// 4) 无档位配置的 admin 密钥 → 落到全局 thinking.*（high）。
	bodies = nil
	if rec := do("sk-admin", `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if b := lastBody(); !strings.Contains(b, `"reasoning_effort":"high"`) {
		t.Fatalf("全局兜底未生效: %s", b)
	}

	// 5) 错误密钥 401。
	if rec := do("sk-wrong", `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`); rec.Code != 401 {
		t.Fatalf("错误密钥 code=%d want 401", rec.Code)
	}

	// 6) Anthropic 入口（x-api-key）同样按密钥身份注入。
	bodies = nil
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "sk-codex")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("messages code=%d body=%s", rec.Code, rec.Body.String())
	}
	if b := lastBody(); !strings.Contains(b, `"reasoning_effort":"low"`) {
		t.Fatalf("Anthropic 入口未按密钥注入: %s", b)
	}
}
