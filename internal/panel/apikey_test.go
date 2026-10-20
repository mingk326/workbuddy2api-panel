package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// apiKeyPanel 造一个只认额度端点的假上游，用于「添加 API Key 账号」测试。
// calls 记录打到的路径，供断言「探测确实发出了额度查询」。
//
// 路由未在 panel.go 注册前，测试直接调用 p.addAPIKey（handler 逻辑与路由解耦）。
func apiKeyPanel(t *testing.T, respBody string) (*Panel, *pool.Pool, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	p := pool.New("")
	pn := New(Config{
		Version:  "test",
		Pool:     p,
		Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL},
		AuthDir:  t.TempDir(),
	})
	return pn, p, &calls
}

// enterpriseUsageOK 企业口径额度答复：limitNum=500、credit=120 → 剩余 380。
// （静态 Key 无个人账号身份，个人 get-user-resource 恒返回空 Accounts，
// 故 auth.UsesEnterpriseBilling 把它与企业版同列，走企业口径端点。）
const enterpriseUsageOK = `{"code":0,"msg":"OK","data":{"credit":120,"limitNum":500}}`

func postAPIKey(t *testing.T, pn *Panel, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	pn.addAPIKey(rec, httptest.NewRequest("POST", "/panel/api/accounts/add_apikey", strings.NewReader(body)))
	return rec
}

// TestAddAPIKeySavesAndLoadsIntoPool 正常路径：探测成功 → 落盘（无 refreshToken）→
// 入池且额度为探测值。落盘形态必须与 OAuth 账号一致，否则 LoadDir 重启后读不出来。
func TestAddAPIKeySavesAndLoadsIntoPool(t *testing.T) {
	pn, p, calls := apiKeyPanel(t, enterpriseUsageOK)
	rec := postAPIKey(t, pn, `{"access_token":"ck_abc123.secretpart","nickname":"我的Key"}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v", err)
	}
	if resp["api_key"] != true {
		t.Errorf("应标记 api_key=true：%v", resp)
	}
	uid, _ := resp["uid"].(string)
	if uid != "apikey-abc123" {
		t.Fatalf("uid=%q want apikey-abc123（由 key 的 id 段派生）", uid)
	}

	// 探测必须真的打了一次企业口径额度端点（静态 Key 无个人身份）。
	if len(*calls) == 0 || !strings.Contains((*calls)[0], "/billing/meter/get-enterprise-user-usage") {
		t.Fatalf("探测应走 get-enterprise-user-usage，实际 %v", *calls)
	}

	// 入池且额度写入。
	a := p.AuthByUID(uid)
	if a == nil {
		t.Fatal("添加后账号应已入池")
	}
	if !a.IsAPIKey() {
		t.Errorf("入池账号应判为 API Key：%+v", a)
	}
	if a.RefreshTokenValue() != "" {
		t.Errorf("静态 Key 不该带 refreshToken：%q", a.RefreshTokenValue())
	}
	// 静态 Key 无过期时间，也不该被判定需要 refresh（否则每个请求都会先打刷新端点）。
	if a.NeedsRefresh(time.Hour) {
		t.Error("静态 API Key 不该需要刷新")
	}
	st, ok := p.Status(uid)
	if !ok {
		t.Fatal("状态查询不到该账号")
	}
	if st.Credits != 380 || st.CreditsTotal != 500 {
		t.Errorf("额度 credits=%d total=%d want 380/500", st.Credits, st.CreditsTotal)
	}

	// 落盘 → 重载：重启后账号仍然可用（这是「添加成功」的最终判据）。
	auths, err := auth.LoadDir(pn.cfg.AuthDir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(auths) != 1 || !auths[0].IsAPIKey() || auths[0].UID != uid {
		t.Fatalf("落盘账号不可重载：%+v", auths)
	}
	if auths[0].RefreshTokenValue() != "" {
		t.Errorf("重载后不该带 refreshToken：%q", auths[0].RefreshTokenValue())
	}
}

// TestAddAPIKeyRejectsBadInput 三条拒绝路径都不落盘：前缀不对 / 空 token / uid 非法。
func TestAddAPIKeyRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"非 Key 前缀", `{"access_token":"eyJhbGciOiJSUzI1NiJ9.eyJ9.sig"}`},
		{"空 token", `{"access_token":"  "}`},
		{"uid 路径穿越", `{"access_token":"ck_abc.def","uid":"../../etc/passwd"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pn, p, _ := apiKeyPanel(t, enterpriseUsageOK)
			rec := postAPIKey(t, pn, c.body)
			if rec.Code != 400 {
				t.Fatalf("code=%d body=%s（want 400）", rec.Code, rec.Body)
			}
			if n := len(p.List()); n != 0 {
				t.Fatalf("被拒绝的请求不该入池：%d 个账号", n)
			}
			entries, _ := os.ReadDir(pn.cfg.AuthDir)
			if len(entries) != 0 {
				t.Fatalf("被拒绝的请求不该落盘：%v", entries)
			}
		})
	}
}

// TestAddAPIKeyInvalidKeyNotPersisted 上游探测失败（无效 Key → 401 业务信封）必须 400 且不落盘。
func TestAddAPIKeyInvalidKeyNotPersisted(t *testing.T) {
	pn, p, _ := apiKeyPanel(t, `{"code":401,"msg":"invalid api key","data":null}`)
	rec := postAPIKey(t, pn, `{"access_token":"ck_dead.dead","nickname":""}`)
	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s（want 400）", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "API Key") {
		t.Errorf("错误文案应指明是 API Key 的问题：%s", rec.Body)
	}
	if n := len(p.List()); n != 0 {
		t.Fatalf("探测失败不该入池：%d", n)
	}
	entries, _ := os.ReadDir(pn.cfg.AuthDir)
	if len(entries) != 0 {
		t.Fatalf("探测失败不该落盘：%v", entries)
	}
}

// TestResolveAPIKeyUID 显式 uid 走白名单校验；派生 uid 时长超限要截断（拼文件名前的兜底）。
func TestResolveAPIKeyUID(t *testing.T) {
	if uid, err := resolveAPIKeyUID("my-key_1", "ck_abc.def"); err != nil || uid != "my-key_1" {
		t.Fatalf("显式 uid 应原样使用：%q %v", uid, err)
	}
	if _, err := resolveAPIKeyUID("bad uid", "ck_abc.def"); err == nil {
		t.Fatal("含空格的 uid 应被拒")
	}
	if uid, err := resolveAPIKeyUID("", "pt_zzz.999"); err != nil || uid != "apikey-zzz" {
		t.Fatalf("派生 uid=%q err=%v want apikey-zzz", uid, err)
	}
	// 无 id 段（前缀后直接是点）→ 必须报错而不是造出 "apikey-"。
	if _, err := resolveAPIKeyUID("", "ck_.secret"); err == nil {
		t.Fatal("无 id 段应报错，请显式指定 uid")
	}
	long := "ck_" + strings.Repeat("a", 200) + ".s"
	uid, err := resolveAPIKeyUID("", long)
	if err != nil {
		t.Fatalf("超长 id 应截断而非报错：%v", err)
	}
	if len(uid) > apiKeyUIDMaxLen {
		t.Fatalf("派生 uid 长度=%d 超过上限 %d", len(uid), apiKeyUIDMaxLen)
	}
}
