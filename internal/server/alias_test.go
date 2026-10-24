package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const aliasSSE = `data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n" +
	"data: [DONE]\n\n"

func aliasUpstream(t *testing.T, gotBody *string) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			*gotBody = string(raw)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(aliasSSE)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

func aliasHandler(t *testing.T, up *upstream.Client, hidden []string) *Handler {
	t.Helper()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	return NewHandler(Config{
		Pool:          p,
		Upstream:      up,
		ModelsAliases: []string{"claude-opus-4=glm-5.2"},
		ModelsHidden:  hidden,
	})
}

// TestModelAliasNonStream 非流式：出站用真实模型，响应 model 回显客户端原发名。
func TestModelAliasNonStream(t *testing.T) {
	var got string
	h := aliasHandler(t, aliasUpstream(t, &got), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"claude-opus-4","stream":false,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(got, `"model":"glm-5.2"`) {
		t.Fatalf("出站应使用真实模型: %s", got)
	}
	if strings.Contains(got, "claude-opus-4") {
		t.Fatalf("出站不应保留客户端别名: %s", got)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["model"] != "claude-opus-4" {
		t.Fatalf("响应 model=%v want claude-opus-4", resp["model"])
	}
}

// TestModelAliasStream 流式：SSE 帧 model 同样回显原发名。
func TestModelAliasStream(t *testing.T) {
	var got string
	h := aliasHandler(t, aliasUpstream(t, &got), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"claude-opus-4","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	s := rec.Body.String()
	if !strings.Contains(s, `"model":"claude-opus-4"`) {
		t.Fatalf("SSE model 应回显原发名: %s", s)
	}
	if strings.Contains(s, `"model":"glm-5.2"`) {
		t.Fatalf("SSE 不应暴露真实模型名: %s", s)
	}
}

// TestModelAliasCannotBypassHidden 别名目标仍受 models.hidden 约束。
func TestModelAliasCannotBypassHidden(t *testing.T) {
	var got string
	h := aliasHandler(t, aliasUpstream(t, &got), []string{"glm-5.2"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"claude-opus-4","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	if got != "" {
		t.Errorf("隐藏模型的别名请求不该打到上游: %s", got)
	}
}
