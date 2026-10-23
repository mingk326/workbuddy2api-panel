package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestModelFallbackOn6004 模型降级：原模型 6004 → 同账号改发等价模型并成功；
// 冷却只记在原模型名下（等价模型不应被连带冷却）。
func TestModelFallbackOn6004(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var obj map[string]any
			_ = json.Unmarshal(raw, &obj)
			m, _ := obj["model"].(string)
			mu.Lock()
			sent = append(sent, m)
			mu.Unlock()
			if m == "origin-model" {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":6004,"msg":"将在 2099-01-01 00:00:00 UTC+8 重置"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseWithContent)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		ModelFallback: func() map[string]string {
			return map[string]string{"origin-model": "backup-model"}
		},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"origin-model","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sent) < 2 {
		t.Fatalf("出站请求=%v，应在 6004 后改发等价模型", sent)
	}
	if sent[0] != "origin-model" {
		t.Errorf("首跳模型=%q want origin-model", sent[0])
	}
	if sent[len(sent)-1] != "backup-model" {
		t.Errorf("降级后模型=%q want backup-model（全序列 %v）", sent[len(sent)-1], sent)
	}
	// 冷却口径：原模型被记模型级冷却，等价模型不受牵连。
	if !p.ModelBlocked("origin-model").Blocked {
		t.Error("原模型应进入模型级冷却（限流的是它）")
	}
	if p.ModelBlocked("backup-model").Blocked {
		t.Error("等价模型不应被冷却（降级就白做了）")
	}
}

// TestModelFallbackDisabled 未配置降级表时保持原行为：6004 直接失败，不换模型。
func TestModelFallbackDisabled(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var obj map[string]any
			_ = json.Unmarshal(raw, &obj)
			m, _ := obj["model"].(string)
			mu.Lock()
			sent = append(sent, m)
			mu.Unlock()
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":6004,"msg":"将在 2099-01-01 00:00:00 UTC+8 重置"}`)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"origin-model","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code == http.StatusOK {
		t.Fatalf("未配置降级时不应成功: code=%d", rec.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, m := range sent {
		if m != "origin-model" {
			t.Errorf("未配置降级时不应改写模型: %v", sent)
		}
	}
}
