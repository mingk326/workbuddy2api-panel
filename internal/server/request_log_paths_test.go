// request_log_paths_test.go 请求记录覆盖三个协议入口，并带上命中的密钥身份。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// TestRequestLogCoversProtocolEntries /v1/messages（Claude Code）与 /v1/responses（Codex）
// 现在也会写入请求记录，且带命中的密钥 ID / 名称。
func TestRequestLogCoversProtocolEntries(t *testing.T) {
	var bodies []string
	up := thinkingOverrideUpstream(&bodies)
	reqLog := reqlog.New(reqlog.Config{})
	live := livecfg.New(livecfg.Snapshot{APIKeys: []httpauth.Key{
		{ID: "cc", Secret: "sk-cc", Name: "Claude Code"},
	}})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: up, Live: live, RequestLog: reqLog})

	do := func(path, body, header, key string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set(header, key)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s code=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}

	do("/v1/messages",
		`{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
		"x-api-key", "sk-cc")
	do("/v1/responses",
		`{"model":"glm-5.2","stream":true,"input":"hi"}`,
		"Authorization", "Bearer sk-cc")

	recent := reqLog.Snapshot().Recent
	if len(recent) < 2 {
		t.Fatalf("请求记录条数=%d，want >=2", len(recent))
	}
	byPath := map[string]reqlog.Event{}
	var paths []string
	for _, e := range recent {
		byPath[e.Path] = e
		paths = append(paths, e.Path)
	}
	for _, path := range []string{"/v1/messages", "/v1/responses"} {
		e, ok := byPath[path]
		if !ok {
			t.Errorf("请求记录缺 %s（现有 %v）", path, paths)
			continue
		}
		if e.Status != http.StatusOK || e.Model != "glm-5.2" {
			t.Errorf("%s 记录 = status=%d model=%q", path, e.Status, e.Model)
		}
		if e.KeyID != "cc" || e.KeyName != "Claude Code" {
			t.Errorf("%s 密钥身份 = (%q,%q) want (cc,Claude Code)", path, e.KeyID, e.KeyName)
		}
	}
}
