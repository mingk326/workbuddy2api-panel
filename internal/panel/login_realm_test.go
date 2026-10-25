package panel

import (
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/loginsite"
)

// TestLoginEndpoints realm→端点映射：global 三端点全在 workbuddy.ai 域，
// cn/空/非法值 → copilot.tencent.com（零回归兜底）。
func TestLoginEndpoints(t *testing.T) {
	cases := []struct {
		realm      string
		wantState  string
		wantOrigin string
	}{
		{"global", "https://www.workbuddy.ai/v2/plugin/auth/state?platform=CLI", "https://www.workbuddy.ai"},
		{"cn", "https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI", "https://www.codebuddy.cn"},
		{"", "https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI", "https://www.codebuddy.cn"},
		{"weird", "https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI", "https://www.codebuddy.cn"},
	}
	for _, c := range cases {
		t.Run(c.realm, func(t *testing.T) {
			st, tok, acct, origin := loginEndpoints(c.realm)
			if st != c.wantState {
				t.Errorf("realm=%q state=%q want %q", c.realm, st, c.wantState)
			}
			if origin != c.wantOrigin {
				t.Errorf("realm=%q origin=%q want %q", c.realm, origin, c.wantOrigin)
			}
			// token/account 端点必与 state 同 base。
			if c.realm == "global" {
				if tok[:len("https://www.workbuddy.ai")] != "https://www.workbuddy.ai" ||
					acct[:len("https://www.workbuddy.ai")] != "https://www.workbuddy.ai" {
					t.Errorf("global token/account endpoints should be workbuddy.ai: %q %q", tok, acct)
				}
			} else {
				if tok[:len("https://copilot.tencent.com")] != "https://copilot.tencent.com" ||
					acct[:len("https://copilot.tencent.com")] != "https://copilot.tencent.com" {
					t.Errorf("cn token/account endpoints should be copilot.tencent.com: %q %q", tok, acct)
				}
			}
		})
	}
}

// TestLoginEndpointsForSite 站点表（对齐官方 CLI 2.174.0）：intl 走 codebuddy.ai、
// workbuddy 走旧桌面端、cn 走 copilot.tencent.com；Origin 与 API 同域（cn 既定例外）。
func TestLoginEndpointsForSite(t *testing.T) {
	cases := []struct {
		site       string
		wantState  string
		wantOrigin string
	}{
		{"cn", "https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI", "https://www.codebuddy.cn"},
		{"intl", "https://www.codebuddy.ai/v2/plugin/auth/state?platform=CLI", "https://www.codebuddy.ai"},
		{"workbuddy", "https://www.workbuddy.ai/v2/plugin/auth/state?platform=CLI", "https://www.workbuddy.ai"},
		// 空/未知 → 国内站（零回归）。
		{"", "https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI", "https://www.codebuddy.cn"},
		{"global", "https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI", "https://www.codebuddy.cn"},
	}
	for _, c := range cases {
		t.Run("site="+c.site, func(t *testing.T) {
			st, tok, acct, origin := loginEndpointsFor(siteOrDefault(c.site))
			if st != c.wantState || origin != c.wantOrigin {
				t.Errorf("site=%q state=%q origin=%q want %q / %q", c.site, st, origin, c.wantState, c.wantOrigin)
			}
			api := strings.TrimSuffix(st, "/v2/plugin/auth/state?platform=CLI")
			if !strings.HasPrefix(tok, api) || !strings.HasPrefix(acct, api) {
				t.Errorf("site=%q token/account 未与 state 同域: %q %q", c.site, tok, acct)
			}
		})
	}
}

// TestClientUAAlignedWithLoginsite UA 与官方域表同源（升级版本号时两处不能漂移）。
func TestClientUAAlignedWithLoginsite(t *testing.T) {
	if clientUA != loginsite.ClientUA || !strings.Contains(clientUA, loginsite.ClientVersion) {
		t.Errorf("clientUA=%q want loginsite.ClientUA=%q", clientUA, loginsite.ClientUA)
	}
}

// TestSiteForRealmCompat 老调用（realm）语义不变：global → 旧桌面端 workbuddy.ai。
func TestSiteForRealmCompat(t *testing.T) {
	if got := siteForRealm("global"); got.ID != siteWorkBuddy || got.API != "https://www.workbuddy.ai" {
		t.Errorf("siteForRealm(global)=%+v want workbuddy 旧桌面端", got)
	}
	if got := siteForRealm(""); got.ID != siteCN {
		t.Errorf("siteForRealm(\"\") id=%q want cn", got.ID)
	}
}
