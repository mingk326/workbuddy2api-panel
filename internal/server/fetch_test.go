package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// fetchUpstream 只处理 /agenttool/v1/webfetch，记录调用次数。
func fetchUpstream(t *testing.T, status int, body string) (*upstream.Client, *int) {
	t.Helper()
	calls := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if !strings.HasSuffix(r.URL.Path, "/agenttool/v1/webfetch") {
				t.Errorf("unexpected upstream path: %s", r.URL.Path)
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return up, &calls
}

func doFetch(h *Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch", strings.NewReader(body)))
	return rec
}

// TestFetchEndpoint 成功抓取：响应保留上游 content。
func TestFetchEndpoint(t *testing.T) {
	up, calls := fetchUpstream(t, 200, `{"url":"https://example.com","title":"Example","content":"# hello"}`)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := doFetch(h, `{"url":"https://example.com"}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"# hello"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if *calls != 1 {
		t.Errorf("upstream calls=%d want 1", *calls)
	}
}

// TestFetchRejectsBadURL 非 http(s) 或空 url 在本地 400，不打上游。
func TestFetchRejectsBadURL(t *testing.T) {
	up, calls := fetchUpstream(t, 200, `{"content":"x"}`)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: up})
	for _, body := range []string{`{}`, `{"url":"ftp://example.com/a"}`, `{"url":"not a url"}`} {
		if rec := doFetch(h, body); rec.Code != 400 {
			t.Errorf("body=%s code=%d want 400", body, rec.Code)
		}
	}
	if *calls != 0 {
		t.Errorf("非法 url 不应打上游: calls=%d", *calls)
	}
}

// TestFetchMissingContentIs502 上游 200 但缺 content → 502，不伪造空正文。
func TestFetchMissingContentIs502(t *testing.T) {
	up, _ := fetchUpstream(t, 200, `{"url":"https://example.com","title":"Example"}`)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := doFetch(h, `{"url":"https://example.com"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body.String())
	}
}

// TestFetchUpstreamBadRequestPassThrough 上游 400（url 校验失败）原样透传，不换号。
func TestFetchUpstreamBadRequestPassThrough(t *testing.T) {
	up, calls := fetchUpstream(t, 400, `{"code":15013,"msg":"url is required"}`)
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := doFetch(h, `{"url":"https://example.com"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	if *calls != 1 {
		t.Errorf("请求级 400 不应换号: calls=%d", *calls)
	}
}
