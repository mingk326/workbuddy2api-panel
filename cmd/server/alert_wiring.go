// alert_wiring.go 把池状态与 handler 的 WAF 状态适配成 alert.Source，并构造监视器。
//
// 单独成文件是为了让 main.go 的装配段保持可读：告警是旁路能力，不影响主链路。
package main

import (
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/alert"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
)

// alertSource alert.Source 的适配器。
type alertSource struct {
	p *pool.Pool
	h *server.Handler
}

// Health 聚合单个域的池健康计数（口径与 /metrics 一致：暂停选号并入「不可用」）。
func (s alertSource) Health(realm string) alert.Health {
	var out alert.Health
	if s.p == nil {
		return out
	}
	now := time.Now()
	for _, st := range s.p.List() {
		if realm != "" && st.Realm != realm {
			continue
		}
		out.Total++
		out.InFlight += st.InFlight
		switch {
		case st.Disabled:
			out.Disabled++
		case st.Paused:
			out.Disabled++
		case st.Cooling:
			out.Cooling++
		default:
			out.Healthy++
		}
		if !st.BreakerUntil.IsZero() && st.BreakerUntil.After(now) {
			out.Breaker++
		}
		if !st.DegradeUntil.IsZero() && st.DegradeUntil.After(now) {
			out.Degraded++
		}
	}
	return out
}

// WAFActive 透出 IP 级 WAF 拦截状态。
func (s alertSource) WAFActive() bool {
	if s.h == nil {
		return false
	}
	return s.h.WAFActive()
}

// newAlertMonitor 按配置构造告警监视器；未启用或没配 webhook 时返回 nil。
func newAlertMonitor(cfg *Config, p *pool.Pool, h *server.Handler) *alert.Monitor {
	if cfg == nil || !cfg.Alerting.Enabled || cfg.Alerting.WebhookURL == "" {
		return nil
	}
	interval := time.Duration(cfg.Alerting.IntervalSeconds) * time.Second
	return alert.New(alert.Config{
		Enabled:          true,
		WebhookURL:       cfg.Alerting.WebhookURL,
		Secret:           cfg.Alerting.Secret,
		Interval:         interval,
		MinHealthyCN:     cfg.Alerting.MinHealthyCN,
		MinHealthyGlobal: cfg.Alerting.MinHealthyGlobal,
		BreakerThreshold: cfg.Alerting.BreakerThreshold,
		SendResolve:      cfg.Alerting.SendResolve,
	}, alertSource{p: p, h: h})
}
