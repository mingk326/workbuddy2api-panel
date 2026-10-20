package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// travelPanel 构造带旅行依赖的面板：单账号 + growth 端点桩。
// 返回面板、账号池与 buddy/info 调用计数。
func travelPanel(t *testing.T, a *auth.Auth) (*Panel, *pool.Pool, *atomic.Int32) {
	t.Helper()
	var infoCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity/growth/buddy/info":
			infoCalls.Add(1)
			fmt.Fprint(w, `{"code":0,"msg":"ok","data":{"buddy":null}}`)
		case "/activity/growth/buddy/agreement":
			fmt.Fprint(w, `{"code":0,"msg":"ok","data":{"agreed":true}}`)
		case "/activity/growth/buddy/first":
			fmt.Fprint(w, `{"code":0,"msg":"ok","data":{"buddy":{"id":1}}}`)
		case "/v2/report":
			fmt.Fprint(w, `{"code":0,"msg":"ok","data":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	if a != nil {
		p.Add(a)
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	return New(Config{Version: "test", Pool: p, Upstream: up, Scheduler: sch}), p, &infoCalls
}

// TestAccountTravelEndpoint 单号旅行端点（issue #227）：404 / 409 / 跳过 / 执行四种语义。
func TestAccountTravelEndpoint(t *testing.T) {
	// 未配置 scheduler → 501。
	pn := New(Config{Version: "test", Pool: pool.New("")})
	if rec := doPanel(pn, "POST", "/panel/api/accounts/u1/travel", "", ""); rec.Code != http.StatusNotImplemented {
		t.Fatalf("no scheduler code=%d want 501", rec.Code)
	}

	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	pn, p, infoCalls := travelPanel(t, a)

	// 不存在的账号 → 404，且不发上游请求。
	if rec := doPanel(pn, "POST", "/panel/api/accounts/ghost/travel", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing account code=%d want 404", rec.Code)
	}
	if infoCalls.Load() != 0 {
		t.Fatal("不存在的账号不应发起上游请求")
	}

	// 账号任务锁被占用 → 409（避免与任务中心并发「查状态→派出」重复派出）。
	if !pn.tryLockAccount("u1") {
		t.Fatal("预占锁失败")
	}
	if rec := doPanel(pn, "POST", "/panel/api/accounts/u1/travel", "", ""); rec.Code != http.StatusConflict {
		t.Fatalf("locked code=%d want 409", rec.Code)
	}
	pn.unlockAccount("u1")

	// 正常执行：无猫 → 领养，buddy/info 恰好一次。
	rec := doPanel(pn, "POST", "/panel/api/accounts/u1/travel", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("run code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true {
		t.Fatalf("resp=%v want ok:true", resp)
	}
	if infoCalls.Load() != 1 {
		t.Errorf("buddy/info calls=%d want 1", infoCalls.Load())
	}

	// 门控跳过：企业版 → 200 + ok:false + skipped，不发上游请求。
	p.AuthByUID("u1").EnterpriseID = "ent-1"
	before := infoCalls.Load()
	rec = doPanel(pn, "POST", "/panel/api/accounts/u1/travel", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("enterprise code=%d body=%s", rec.Code, rec.Body.String())
	}
	resp = nil
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["ok"] != false || !strings.Contains(rec.Body.String(), "skipped") {
		t.Fatalf("enterprise resp=%s want ok:false + skipped", rec.Body.String())
	}
	if infoCalls.Load() != before {
		t.Error("企业版不应发起上游请求")
	}
}
