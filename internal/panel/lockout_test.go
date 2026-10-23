package panel

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

// lockoutPanel 单 admin 密钥面板（启用鉴权，非 legacy 形态）。
func lockoutPanel() *Panel {
	live := livecfg.New(livecfg.Snapshot{APIKeys: []httpauth.Key{
		{ID: "admin", Secret: "sk-admin", Admin: true},
	}})
	return New(Config{Version: "test", Live: live})
}

// doPanelFrom 以指定 TCP 对端地址发起面板请求，便于按 IP 隔离锁定状态。
func doPanelFrom(p *Panel, method, path, key, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if remote != "" {
		req.RemoteAddr = remote
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// TestPanelLockoutAfterFirstWrongKey 输错一次即按 IP 锁定一小时：
// 期间正确密钥也 429，其他 IP 不受影响。
func TestPanelLockoutAfterFirstWrongKey(t *testing.T) {
	p := lockoutPanel()
	const ip = "198.51.100.10"

	if got := doPanelFrom(p, "GET", "/panel/api/logs", "sk-wrong", ip+":1111").Code; got != 401 {
		t.Fatalf("wrong key code=%d want 401", got)
	}
	rec := doPanelFrom(p, "GET", "/panel/api/logs", "sk-admin", ip+":2222")
	if rec.Code != 429 {
		t.Fatalf("locked ip code=%d want 429 (body=%s)", rec.Code, rec.Body.String())
	}
	secs, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || secs <= 0 || secs > int(authLockDuration.Seconds()) {
		t.Fatalf("Retry-After=%q want 0<secs<=%d", rec.Header().Get("Retry-After"), int(authLockDuration.Seconds()))
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error != "too_many_attempts" {
		t.Fatalf("locked error body=%s", rec.Body.String())
	}
	// 另一个 IP 不受影响。
	if got := doPanelFrom(p, "GET", "/panel/api/logs", "sk-admin", "198.51.100.11:1234").Code; got != 200 {
		t.Fatalf("other ip code=%d want 200", got)
	}
}

// TestPanelLockoutExpires 锁定一小时后自动恢复（用注入时钟验证边界）。
func TestPanelLockoutExpires(t *testing.T) {
	p := lockoutPanel()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	p.authLock.now = func() time.Time { return now }
	const ip = "203.0.113.5"

	if got := doPanelFrom(p, "GET", "/panel/api/logs", "sk-wrong", ip+":1111").Code; got != 401 {
		t.Fatalf("wrong key code=%d want 401", got)
	}
	now = now.Add(59 * time.Minute)
	if got := doPanelFrom(p, "GET", "/panel/api/logs", "sk-admin", ip+":2222").Code; got != 429 {
		t.Fatalf("within lock window code=%d want 429", got)
	}
	now = now.Add(2 * time.Minute)
	if got := doPanelFrom(p, "GET", "/panel/api/logs", "sk-admin", ip+":2222").Code; got != 200 {
		t.Fatalf("after lock expiry code=%d want 200", got)
	}
}

// TestPanelNoLockWhenAuthDisabled 未配置密钥 = 不鉴权，也不应产生锁定。
func TestPanelNoLockWhenAuthDisabled(t *testing.T) {
	p := New(Config{Version: "test"})
	for i := 0; i < 3; i++ {
		if got := doPanelFrom(p, "GET", "/panel/api/logs", "", "198.51.100.20:1234").Code; got != 200 {
			t.Fatalf("request %d code=%d want 200", i, got)
		}
	}
}

// TestClientIPForLock 锁定 IP 的提取优先级：CF-Connecting-IP → X-Real-IP →
// X-Forwarded-For 首段 → TCP 对端。
func TestClientIPForLock(t *testing.T) {
	req := httptest.NewRequest("GET", "/panel/", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	if got := clientIPForLock(req); got != "203.0.113.7" {
		t.Errorf("XFF got %q want 203.0.113.7", got)
	}
	req.Header.Set("X-Real-IP", "192.0.2.5")
	if got := clientIPForLock(req); got != "192.0.2.5" {
		t.Errorf("X-Real-IP got %q want 192.0.2.5", got)
	}
	req.Header.Set("CF-Connecting-IP", "198.51.100.9")
	if got := clientIPForLock(req); got != "198.51.100.9" {
		t.Errorf("CF-Connecting-IP got %q want 198.51.100.9", got)
	}

	plain := httptest.NewRequest("GET", "/panel/", nil)
	plain.RemoteAddr = "198.51.100.9:54321"
	if got := clientIPForLock(plain); got != "198.51.100.9" {
		t.Errorf("RemoteAddr got %q want 198.51.100.9", got)
	}
}

// TestAuthLockoutBounded 锁定表条目不得无限增长（键来自可伪造的请求头）。
func TestAuthLockoutBounded(t *testing.T) {
	l := newAuthLockout(time.Hour)
	for i := 0; i < authLockMaxEntries+32; i++ {
		l.fail("10." + strconv.Itoa(i/65536) + "." + strconv.Itoa(i/256%256) + "." + strconv.Itoa(i%256))
	}
	l.mu.Lock()
	n := len(l.until)
	l.mu.Unlock()
	if n > authLockMaxEntries {
		t.Fatalf("lock entries=%d want <=%d", n, authLockMaxEntries)
	}
}
