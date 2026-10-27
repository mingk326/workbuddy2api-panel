package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

// TestModelsAllowedEmptyPassesAll 未配置（空/全空白）时白名单不启用，默认放行所有模型。
func TestModelsAllowedEmptyPassesAll(t *testing.T) {
	for _, in := range [][]string{nil, {}, {"  ", ""}} {
		h := NewHandler(Config{ModelsAllowed: in})
		if !h.allowedModel("cn", "glm-5.2") || !h.allowedModel("global", "glm-5.2") {
			t.Errorf("ModelsAllowed=%v 应默认放行所有模型", in)
		}
	}
}

// TestModelsAllowedNormalizedAndScoped 裸名两域允许；cn:/global: 前缀只允许该域。
func TestModelsAllowedNormalizedAndScoped(t *testing.T) {
	h := NewHandler(Config{ModelsAllowed: []string{" glm-5.2 ", "cn:kimi-k3-1", "global:gpt-5.6-sol"}})
	cases := []struct {
		realm, model string
		want         bool
	}{
		{"cn", "glm-5.2", true},
		{"global", "glm-5.2", true},
		{"cn", "kimi-k3-1", true},
		{"global", "kimi-k3-1", false},
		{"global", "gpt-5.6-sol", true},
		{"cn", "gpt-5.6-sol", false},
		{"cn", "not-listed", false},
		{"cn", "", false},
	}
	for _, c := range cases {
		if got := h.allowedModel(c.realm, c.model); got != c.want {
			t.Errorf("allowedModel(%q,%q)=%v want %v", c.realm, c.model, got, c.want)
		}
	}
}

// TestModelsAllowedRejectsChatBeforeUpstream 白名单外模型在选号前直接 400，
// 不打上游（calls==0），文案不暗示「存在但未授权」。
func TestModelsAllowedRejectsChatBeforeUpstream(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:      up,
		ModelsAllowed: []string{"glm-5.2"},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"other-model","messages":[]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if calls != 0 {
		t.Errorf("calls=%d want 0（白名单外不该打上游）", calls)
	}
	var resp struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Error.Code != "model_unavailable" {
		t.Fatalf("error.code=%q want model_unavailable (body=%s)", resp.Error.Code, rec.Body)
	}
}

// TestModelsAllowedAliasCannotBypass 别名映射后的真实模型同样受白名单约束。
func TestModelsAllowedAliasCannotBypass(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:      up,
		ModelsAllowed: []string{"glm-5.2"},
		ModelsAliases: []string{"my-glm=secret-model"},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"my-glm","messages":[]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400（别名不能绕过白名单）body=%s", rec.Code, rec.Body)
	}
	if calls != 0 {
		t.Errorf("calls=%d want 0", calls)
	}
}

// TestModelsAllowedHotReloadViaLive 面板保存后经 livecfg 快照热生效。
func TestModelsAllowedHotReloadViaLive(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{})
	h := NewHandler(Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:      newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		Live:          live,
		ModelsAllowed: []string{"glm-5.2"},
	})
	if !h.allowedModel("cn", "other-model") {
		t.Fatal("Live 未启用白名单时应放行全部")
	}
	allowed, enabled := livecfg.NormalizeAllowedModels([]string{"glm-5.2"})
	live.Store(livecfg.Snapshot{AllowedModels: allowed, AllowlistEnabled: enabled})
	if h.allowedModel("cn", "other-model") {
		t.Error("Live 启用白名单后 other-model 应被拒")
	}
	if !h.allowedModel("cn", "glm-5.2") {
		t.Error("Live 启用白名单后 glm-5.2 应放行")
	}
}

// TestModelsAllowedIntersectHidden 白名单与黑名单取交集：允许但仍被 hidden 的模型不可用。
func TestModelsAllowedIntersectHidden(t *testing.T) {
	h := NewHandler(Config{
		ModelsAllowed: []string{"glm-5.2"},
		ModelsHidden:  []string{"glm-5.2"},
	})
	if !h.allowedModel("cn", "glm-5.2") {
		t.Fatal("allowedModel 只回答白名单，不负责 hidden")
	}
	if !h.hiddenModel("cn", "glm-5.2") {
		t.Fatal("hidden 应独立命中")
	}
}
