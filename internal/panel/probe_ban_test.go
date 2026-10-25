package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type probeRT func(*http.Request) (*http.Response, error)

func (f probeRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestAccountProbeBan 封禁号探活：只读探测，按上游答复给处置结论，不改池状态。
func TestAccountProbeBan(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		wantVerdict string
	}{
		{"已解封", 200, `{"code":0,"data":{"Accounts":[]}}`, "lifted"},
		{"仍被封（11140）", 400, `{"code":11140,"msg":"request illegal"}`, "still_banned"},
		{"无结论（未知 4xx）", 400, `{"code":999,"msg":"weird"}`, "inconclusive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New("")})
			p.cfg.Pool.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
			p.cfg.Pool.Disable("u1", "account restricted by upstream (11140 request illegal)")
			p.cfg.Upstream = &upstream.Client{
				HTTP: &http.Client{Transport: probeRT(func(*http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: c.status,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(c.body)),
					}, nil
				})},
				BillingBaseCN: "https://fake.example",
			}

			req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/u1/probe_ban", nil)
			req.Header.Set("Authorization", "Bearer test-key")
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body)
			}
			var resp struct {
				Verdict     string `json:"verdict"`
				WasDisabled bool   `json:"was_disabled"`
				WasReason   string `json:"was_reason"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("bad JSON: %v body=%s", err, rec.Body)
			}
			if resp.Verdict != c.wantVerdict {
				t.Errorf("verdict=%q want %q (body=%s)", resp.Verdict, c.wantVerdict, rec.Body)
			}
			if !resp.WasDisabled || !strings.Contains(resp.WasReason, "11140") {
				t.Errorf("探活不该改池状态：was_disabled=%v was_reason=%q", resp.WasDisabled, resp.WasReason)
			}
			// 只读：探测后账号仍禁用。
			if st, ok := p.cfg.Pool.Status("u1"); !ok || !st.Disabled {
				t.Errorf("probe_ban 后账号不应被解冻: %+v ok=%v", st, ok)
			}
		})
	}
}
