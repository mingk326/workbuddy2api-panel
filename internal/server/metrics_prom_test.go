package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// /metrics 未开启时不注册路由（404），开启后需鉴权且输出 Prometheus 文本。
func TestMetricsEndpointGateAndFormat(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2"})

	// 关闭：路由不存在。
	off := NewHandler(Config{Pool: p, APIKey: "k"})
	rec := httptest.NewRecorder()
	off.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("metrics 未启用时应 404，got %d", rec.Code)
	}

	// 开启：无 key 401，有 key 200 + 文本格式。
	on := NewHandler(Config{Pool: p, APIKey: "k", MetricsEnabled: true})
	rec = httptest.NewRecorder()
	on.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 api_key 应 401，got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer k")
	on.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE wb2api_pool_accounts_total gauge",
		`wb2api_pool_accounts_total{realm="all"} 2`,
		"# TYPE wb2api_requests_total counter",
		"# TYPE wb2api_model_tokens_per_second gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics 缺少 %q\n---\n%s", want, body)
		}
	}
	if strings.Contains(body, "u1") || strings.Contains(body, "u2") {
		t.Errorf("metrics 不得输出 uid")
	}
}

// 池状态聚合：健康/冷却/禁用/暂停分别计数，realm 标签按账号域拆分。
func TestAggregatePoolStates(t *testing.T) {
	now := time.Now()
	list := []pool.Status{
		{UID: "a", Realm: "cn"},
		{UID: "b", Realm: "cn", Cooling: true},
		{UID: "c", Realm: "cn", Disabled: true},
		{UID: "d", Realm: "global", Paused: true},
		{UID: "e", Realm: "global", InFlight: 2},
		{UID: "f", Realm: "global", BreakerUntil: now.Add(time.Hour)},
		{UID: "g", Realm: "cn", RateLimitedModels: []pool.RateLimitedModel{{Model: "glm-5.3"}}},
	}
	cn := aggregatePool(list, "cn")
	if cn.total != 4 || cn.healthy != 2 || cn.cooling != 1 || cn.disabled != 1 || cn.modelCooled != 1 {
		t.Fatalf("cn 聚合错误: %+v", cn)
	}
	gl := aggregatePool(list, "global")
	if gl.total != 3 || gl.paused != 1 || gl.inFlight != 2 || gl.breaker != 1 {
		t.Fatalf("global 聚合错误: %+v", gl)
	}
	all := aggregatePool(list, "")
	if all.total != 7 || all.inFlight != 2 {
		t.Fatalf("all 聚合错误: %+v", all)
	}
}

// 指标累计：按模型分组，失败与成功分开计，credit 只累计观测到的值。
func TestNotePromAccumulates(t *testing.T) {
	// 用独立模型名，避免与其他用例共享全局计数器造成串扰。
	const model = "test-prom-model"
	s1 := &chatStat{model: model, mode: "stream", status: 200, toks: 100, ttfb: 100 * time.Millisecond,
		hasCache: true, cacheHit: 80, cacheMiss: 20, hasCredit: true, credit: 1.5}
	noteProm(s1, time.Second)
	s2 := &chatStat{model: model, mode: "sync", status: 429, hasCredit: false}
	noteProm(s2, 500*time.Millisecond)

	snap := MetricsSnapshotOf()
	var found *ModelStatPayload
	for i := range snap.Models {
		if snap.Models[i].Model == model {
			found = &snap.Models[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("未找到模型 %s 的指标", model)
	}
	if found.Success != 1 || found.Failed != 1 || found.Streaming != 1 {
		t.Errorf("success=%d failed=%d streaming=%d", found.Success, found.Failed, found.Streaming)
	}
	if found.Credit != 1.5 {
		t.Errorf("credit=%v want 1.5（缺失值不得按 0 混入）", found.Credit)
	}
	if found.CacheHitTokens != 80 || found.CacheMissTokens != 20 {
		t.Errorf("cache hit/miss=%d/%d", found.CacheHitTokens, found.CacheMissTokens)
	}
	if found.CompletionTokens != 100 {
		t.Errorf("completion tokens=%d want 100", found.CompletionTokens)
	}
}
