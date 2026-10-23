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

// TestKeyRPMLimit 每分钟请求数：第 3 次 429 rate_limit_exceeded，且不打上游。
func TestKeyRPMLimit(t *testing.T) {
	var bodies []string
	up := thinkingOverrideUpstream(&bodies)
	quota := keyquota.New("")
	defer quota.Close()
	live := livecfg.New(livecfg.Snapshot{APIKeys: []httpauth.Key{
		{ID: "codex", Secret: "sk-codex", RPMLimit: 2},
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
	for i := 0; i < 2; i++ {
		if rec := do(); rec.Code != http.StatusOK {
			t.Fatalf("request %d code=%d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	bodies = nil
	rec := do()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third code=%d want 429 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_exceeded") || !strings.Contains(rec.Body.String(), "per minute") {
		t.Errorf("429 body 缺 RPM 说明: %s", rec.Body.String())
	}
	if ra, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || ra <= 0 || ra > 60 {
		t.Errorf("Retry-After=%q want 1..60", rec.Header().Get("Retry-After"))
	}
	if len(bodies) != 0 {
		t.Errorf("RPM 拒绝不应打上游: %v", bodies)
	}
	// 被拒请求不计入日请求数（前两次成功才计 2）。
	if _, reqN := quota.Snapshot("codex"); reqN != 2 {
		t.Errorf("daily requests=%d want 2", reqN)
	}
}

// TestKeyConcurrencyLimit 并发上限：占用名额时 429，释放后恢复；
// 正常请求结束（st.done）必须归还名额，否则后续全部误拒。
func TestKeyConcurrencyLimit(t *testing.T) {
	var bodies []string
	up := thinkingOverrideUpstream(&bodies)
	quota := keyquota.New("")
	defer quota.Close()
	live := livecfg.New(livecfg.Snapshot{APIKeys: []httpauth.Key{
		{ID: "codex", Secret: "sk-codex", MaxConcurrency: 1},
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

	// 占住唯一名额，模拟一条还在流式输出的请求。
	hold, lease := quota.Admit("codex", keyquota.Limits{MaxConcurrency: 1})
	if !hold.OK || lease == nil {
		t.Fatalf("hold admit = %+v lease=%v", hold, lease)
	}
	bodies = nil
	rec := do()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("busy code=%d want 429 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "concurrent") || !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
		t.Errorf("429 body 缺并发说明: %s", rec.Body.String())
	}
	if len(bodies) != 0 {
		t.Errorf("并发拒绝不应打上游: %v", bodies)
	}
	lease.Release()

	if rec := do(); rec.Code != http.StatusOK {
		t.Fatalf("after release code=%d body=%s", rec.Code, rec.Body.String())
	}
	// 请求结束（st.done）已归还名额，下一次仍可准入。
	if rec := do(); rec.Code != http.StatusOK {
		t.Fatalf("second after release code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rpm, in := quota.Live("codex"); in != 0 {
		t.Errorf("inflight after done = %d / rpm %d, want 0", in, rpm)
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
