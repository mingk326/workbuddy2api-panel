// key_quota_test.go 密钥级每日配额（Token / 请求数）的网关入口行为。
package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/keyquota"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

// TestKeyDailyRequestLimit 每日请求数：第 2 次直接 429（不打上游），其他密钥不受影响。
func TestKeyDailyRequestLimit(t *testing.T) {
	var bodies []string
	up := thinkingOverrideUpstream(&bodies)
	quota := keyquota.New("")
	defer quota.Close()
	live := livecfg.New(livecfg.Snapshot{APIKeys: []httpauth.Key{
		{ID: "admin", Secret: "sk-admin", Admin: true},
		{ID: "codex", Secret: "sk-codex", DailyRequestLimit: 1},
	}})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: up, Live: live, KeyQuota: quota})

	do := func(key string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := do("sk-codex"); rec.Code != http.StatusOK {
		t.Fatalf("first code=%d body=%s", rec.Code, rec.Body.String())
	}
	bodies = nil
	rec := do("sk-codex")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second code=%d want 429 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "insufficient_quota") || !strings.Contains(rec.Body.String(), "request quota") {
		t.Errorf("429 body 缺配额说明: %s", rec.Body.String())
	}
	if ra, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || ra <= 0 {
		t.Errorf("Retry-After=%q want 正整数", rec.Header().Get("Retry-After"))
	}
	if len(bodies) != 0 {
		t.Errorf("配额拒绝不应打上游: %v", bodies)
	}

	// 无配额的密钥不受影响。
	if rec := do("sk-admin"); rec.Code != http.StatusOK {
		t.Errorf("admin code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestKeyDailyTokenLimit 每日 Token：上游回报 total_tokens=2，上限 1 → 下一次 429。
func TestKeyDailyTokenLimit(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusOK, sseOK, true
	})
	quota := keyquota.New("")
	defer quota.Close()
	live := livecfg.New(livecfg.Snapshot{APIKeys: []httpauth.Key{
		{ID: "codex", Secret: "sk-codex", DailyTokenLimit: 1},
	}})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: up, Live: live, KeyQuota: quota})

	do := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer sk-codex")
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := do(); rec.Code != http.StatusOK {
		t.Fatalf("first code=%d body=%s", rec.Code, rec.Body.String())
	}
	// st.done() 在 handler 返回前执行，此时 token 已累计。
	if tok, reqN := quota.Snapshot("codex"); tok != 2 || reqN != 1 {
		t.Fatalf("quota after first = (%d,%d) want (2,1)", tok, reqN)
	}
	rec := do()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second code=%d want 429 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "token quota") {
		t.Errorf("429 body 缺 token 配额说明: %s", rec.Body.String())
	}
}
