package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newPollPanel 构造带鉴权的最小面板：以下用例都在触达上游之前返回，
// 不依赖 Pool / Upstream。
func newPollPanel() *Panel {
	return New(Config{Version: "test", APIKey: "test-key"})
}

// TestLoginPollUnknownStateGone state 作废时返回 410（而非 404）：前端据此停止轮询，
// 而网络抖动（5xx/超时）继续重试——两种情况混成一个状态码时，一次抖动就会让前端
// 放弃等待，而 OAuth 其实已在浏览器侧完成，账号却不会入池。
func TestLoginPollUnknownStateGone(t *testing.T) {
	p := newPollPanel()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/login/poll?state=nope", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusGone {
		t.Fatalf("code=%d want 410", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "已过期") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

// TestLoginPollProcessingReturnsPending 同一 state 正在落盘时，并发轮询拿到 done=false，
// 不再把整段流程（落盘 / 签到 / 余额校验）重跑一遍：前端每 3s 轮询，而成功分支的
// 副作用可能耗时数秒（上游慢时更久）。
func TestLoginPollProcessingReturnsPending(t *testing.T) {
	p := newPollPanel()
	p.loginMu.Lock()
	p.logins["busy"] = loginSession{created: time.Now(), realm: "cn", processing: true}
	p.loginMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/panel/api/login/poll?state=busy", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"done":false`) || !strings.Contains(body, "正在写入") {
		t.Fatalf("body=%s", body)
	}
}
