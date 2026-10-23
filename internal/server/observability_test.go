package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestChatBusyEndpoint /v2/chat/busy（官方客户端预检）：已知模型 idle、未知模型
// unknown、该域全池模型冷却时 busy。
func TestChatBusyEndpoint(t *testing.T) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "glm-5.2"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	})

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})

	busyLevels := func(body string) map[string]string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v2/chat/busy", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Code int `json:"code"`
			Data []struct {
				Model string `json:"model"`
				Level string `json:"busy_level"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Code != 0 {
			t.Fatalf("code=%d", resp.Code)
		}
		out := map[string]string{}
		for _, d := range resp.Data {
			out[d.Model] = d.Level
		}
		return out
	}

	got := busyLevels(`{"models":["glm-5.2","no-such-model"]}`)
	if got["glm-5.2"] != busyLevelIdle {
		t.Errorf("glm-5.2 level=%q want idle", got["glm-5.2"])
	}
	if got["no-such-model"] != busyLevelUnknown {
		t.Errorf("no-such-model level=%q want unknown", got["no-such-model"])
	}

	// 唯一 CN 号对该模型进入模型级冷却 → busy。
	p.BlockModelBackoff("u1", "glm-5.2", "11102 model not found")
	got = busyLevels(`{"models":["glm-5.2"]}`)
	if got["glm-5.2"] != busyLevelBusy {
		t.Errorf("冷却后 level=%q want busy", got["glm-5.2"])
	}

	// 非法 JSON → 400（与其它端点同口径）。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v2/chat/busy", strings.NewReader("{")))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad json code=%d want 400", rec.Code)
	}
}

// TestEffortLabel 档位溯源渲染：客户端显式 / 网关侧注入 / 三段皆空。
func TestEffortLabel(t *testing.T) {
	cases := []struct {
		name                  string
		client, in, out, want string
	}{
		{"客户端降级", "max", "max", "xhigh", "max->xhigh"},
		{"客户端未降级", "high", "high", "high", "high"},
		{"网关注入降级", "", "max", "xhigh", "gw:max->xhigh"},
		{"网关注入未降级", "", "high", "high", "gw:high"},
		{"无档位", "", "", "", ""},
		{"客户端有值但未参与降级", "max", "", "", "max"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effortLabel(c.client, c.in, c.out); got != c.want {
				t.Errorf("effortLabel(%q,%q,%q)=%q want %q", c.client, c.in, c.out, got, c.want)
			}
		})
	}
}

// TestRequestTraceEventEffort 面板「请求记录」的档位列与流水行 effort= 同源；
// 无 stat（非 chat 路径）时不写档位。
func TestRequestTraceEventEffort(t *testing.T) {
	st := &chatStat{effortClient: "max", effortIn: "max", effortOut: "xhigh"}
	ev := (&requestTrace{id: "req-effort", start: time.Now(), stat: st}).event(http.StatusOK)
	if ev.Effort != "max->xhigh" {
		t.Errorf("effort=%q want max->xhigh", ev.Effort)
	}
	if got := (&requestTrace{start: time.Now()}).event(http.StatusOK).Effort; got != "" {
		t.Errorf("no stat effort=%q want empty", got)
	}
}

// TestLogChatRowEffortSegment 流水行：有档位时追加 effort 段，无档位时旧格式不变。
func TestLogChatRowEffortSegment(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRowFull(10*time.Millisecond, 2*time.Second, "glm-5.3", "stream", "u123456789", "示例号",
			http.StatusOK, 42, "", "", 0, 0, false, "", "", "max->high")
	})
	if !strings.Contains(out, "effort=max->high") {
		t.Errorf("缺 effort 段：\n%s", out)
	}

	out = captureStdout(t, func() {
		logChatRowFull(10*time.Millisecond, 2*time.Second, "glm-5.3", "stream", "u123456789", "示例号",
			http.StatusOK, 42, "", "", 0, 0, false, "", "", "")
	})
	if strings.Contains(out, "effort=") {
		t.Errorf("档位为空时不应出现 effort 段：\n%s", out)
	}
}
