// key_models_test.go 密钥级模型白名单（allowed_models）的网关入口行为。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestKeyAllowedModelsChat 名单外模型 403 且不打上游；名单内正常放行；
// admin 密钥同样受名单约束（admin 只决定面板权限）。
func TestKeyAllowedModelsChat(t *testing.T) {
	var bodies []string
	up := thinkingOverrideUpstream(&bodies)
	live := livecfg.New(livecfg.Snapshot{APIKeys: []httpauth.Key{
		{ID: "admin", Secret: "sk-admin", Admin: true},
		{ID: "codex", Secret: "sk-codex", AllowedModels: []string{"glm-5.2"}},
		{ID: "cn-only", Secret: "sk-cn", AllowedModels: []string{"cn:glm-5.2"}},
	}})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: up, Live: live})

	do := func(key, model string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		h.ServeHTTP(rec, req)
		return rec
	}

	// 1) 名单内 → 200，打上游一次。
	bodies = nil
	if rec := do("sk-codex", "glm-5.2"); rec.Code != http.StatusOK {
		t.Fatalf("名单内模型 code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(bodies) != 1 {
		t.Fatalf("名单内模型应打上游一次，实际 %d", len(bodies))
	}

	// 2) 名单外 → 403 model_not_allowed，且不产生上游请求。
	bodies = nil
	rec := do("sk-codex", "kimi-k3")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("名单外模型 code=%d want 403 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_not_allowed") || !strings.Contains(rec.Body.String(), "glm-5.2") {
		t.Errorf("403 应说明原因并列出可用模型: %s", rec.Body.String())
	}
	if len(bodies) != 0 {
		t.Errorf("名单外模型不应打上游: %v", bodies)
	}

	// 3) 裸名条目跨域生效：global:glm-5.2 不应被 403（无 global 号时可能是 503）。
	if rec := do("sk-codex", "global:glm-5.2"); rec.Code == http.StatusForbidden {
		t.Errorf("裸名应对 global 域同样放行: %s", rec.Body.String())
	}

	// 4) cn: 前缀只限定 cn：global:glm-5.2 → 403。
	if rec := do("sk-cn", "global:glm-5.2"); rec.Code != http.StatusForbidden {
		t.Errorf("cn: 前缀不应放行 global 域: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 5) admin 密钥无名单 → 不受限（admin 只决定面板权限，但白名单对 admin 同样生效）。
	bodies = nil
	if rec := do("sk-admin", "kimi-k3"); rec.Code != http.StatusOK {
		t.Fatalf("无名单的 admin 密钥应放行: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestKeyAllowedModelsModelsList /v1/models 只返回该密钥名单内的模型。
func TestKeyAllowedModelsModelsList(t *testing.T) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "glm-5.2"}, {ID: "kimi-k3"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	})

	live := livecfg.New(livecfg.Snapshot{APIKeys: []httpauth.Key{
		{ID: "admin", Secret: "sk-admin", Admin: true},
		{ID: "codex", Secret: "sk-codex", AllowedModels: []string{"glm-5.2"}},
	}})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}), Upstream: upstream.New(), Live: live})

	listIDs := func(key string) map[string]bool {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("models code=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, d := range resp.Data {
			out[d.ID] = true
		}
		return out
	}

	got := listIDs("sk-codex")
	if len(got) != 1 || !got["cn:glm-5.2"] {
		t.Errorf("受限密钥模型列表=%v，应只含 cn:glm-5.2", got)
	}
	if all := listIDs("sk-admin"); !all["cn:glm-5.2"] || !all["cn:kimi-k3"] {
		t.Errorf("无名单密钥应看到全部模型: %v", all)
	}
}

// TestKeyAllowedModelsBusy 忙闲预检：名单外模型回 unknown，不暴露池状态。
func TestKeyAllowedModelsBusy(t *testing.T) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "glm-5.2"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	})

	live := livecfg.New(livecfg.Snapshot{APIKeys: []httpauth.Key{
		{ID: "admin", Secret: "sk-admin", Admin: true},
		{ID: "codex", Secret: "sk-codex", AllowedModels: []string{"kimi-k3"}},
	}})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}), Upstream: upstream.New(), Live: live})

	level := func(key string) string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v2/chat/busy", strings.NewReader(`{"models":["glm-5.2"]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("busy code=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Data []struct {
				Level string `json:"busy_level"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Data) != 1 {
			t.Fatalf("busy resp=%s err=%v", rec.Body.String(), err)
		}
		return resp.Data[0].Level
	}

	if got := level("sk-codex"); got != busyLevelUnknown {
		t.Errorf("名单外模型 busy_level=%q want unknown", got)
	}
	if got := level("sk-admin"); got != busyLevelIdle {
		t.Errorf("无名单密钥 busy_level=%q want idle", got)
	}
}
