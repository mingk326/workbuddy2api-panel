package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

func resetModelCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

// TestHiddenModelsFilteredFromList 列表不列黑名单模型；裸名藏两域、cn: 前缀只藏 CN。
func TestHiddenModelsFilteredFromList(t *testing.T) {
	resetModelCache()
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"glm-5.2"},{"id":"kimi-k3-1"}],"agents":[{"name":"cli","models":["glm-5.2","kimi-k3-1"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, ModelsHidden: []string{"glm-5.2"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	for _, m := range resp["data"].([]any) {
		if m.(map[string]any)["id"] == "cn:glm-5.2" {
			t.Fatalf("隐藏模型不应出现在列表: %s", rec.Body.String())
		}
	}
}

// TestHiddenModelChatRejectedBeforeUpstream 点名调用隐藏模型直接 400，不打上游。
func TestHiddenModelChatRejectedBeforeUpstream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		t.Error("隐藏模型的请求不该打到上游")
		return 500, "", false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: up, ModelsHidden: []string{"glm-5.2"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_unavailable") || !strings.Contains(rec.Body.String(), "hidden") {
		t.Fatalf("400 应说明 model_unavailable/hidden: %s", rec.Body.String())
	}
}

// TestHiddenModelImagesRejected 出图入口同样受黑名单约束（裸名对两域生效）。
func TestHiddenModelImagesRejected(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		t.Error("隐藏出图模型不该打上游")
		return 500, "", false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: up, ModelsHidden: []string{"hunyuan-image-alpha"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"cn:hunyuan-image-alpha","prompt":"一只猫"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

// TestHiddenModelRealmScope 裸名两域都藏，cn:/global: 前缀只藏该域。
func TestHiddenModelRealmScope(t *testing.T) {
	h := NewHandler(Config{ModelsHidden: []string{"glm-5.2"}})
	if !h.hiddenModel("cn", "glm-5.2") || !h.hiddenModel("global", "glm-5.2") {
		t.Error("裸名应两域都隐藏")
	}
	h2 := NewHandler(Config{ModelsHidden: []string{"cn:glm-5.2"}})
	if !h2.hiddenModel("cn", "glm-5.2") {
		t.Error("cn: 前缀应隐藏 CN 域")
	}
	if h2.hiddenModel("global", "glm-5.2") {
		t.Error("cn: 前缀不应影响 global 域")
	}
	if h2.hiddenModel("cn", "") {
		t.Error("空模型名不应判为隐藏")
	}
}

// TestHiddenModelHotReload Live 快照优先：保存配置后无需重启即生效。
func TestHiddenModelHotReload(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{})
	h := NewHandler(Config{Live: live})
	if h.hiddenModel("cn", "glm-5.2") {
		t.Fatal("初始不应隐藏")
	}
	live.Store(livecfg.Snapshot{HiddenModels: livecfg.NormalizeHiddenModels([]string{"glm-5.2"})})
	if !h.hiddenModel("cn", "glm-5.2") || !h.hiddenModel("global", "glm-5.2") {
		t.Error("热更新后应隐藏")
	}
}
