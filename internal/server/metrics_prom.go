// metrics_prom.go GET /metrics，Prometheus 文本 0.0.4。零第三方依赖。
//
// 请求维度在 chatStat.done 现记（进程内累计，重启清零）；池健康从 Pool.Status()
// 聚合。**不输出 uid**（只出汇总与模型名），避免把账号标识泄给抓指标的一方。
package server

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// promMetricsCap 模型维度上限：模型名来自请求体，是客户端可控输入，必须封顶，
// 否则一个乱写 model 的客户端就能把进程内存撑爆。
const promMetricsCap = 512

type promAcc struct {
	requests, success, failed, streaming int64
	ttfbSum                              float64
	ttfbN                                int64
	latSum                               float64
	genSec                               float64
	genTok                               int64 // 只统计「有生成段」的行，与 genSec 同边界
	compTok                              int64
	cacheHit, cacheMiss                  int64
	credit                               float64
}

var promMetrics = struct {
	mu    sync.Mutex
	since time.Time
	by    map[string]*promAcc
}{since: time.Now(), by: map[string]*promAcc{}}

// noteProm 记一次请求（chatStat.done 调用；任何出口路径都会走到）。
func noteProm(s *chatStat, total time.Duration) {
	model := s.model
	if model == "" {
		model = "-"
	}
	promMetrics.mu.Lock()
	defer promMetrics.mu.Unlock()
	mm := promMetrics.by[model]
	if mm == nil {
		if len(promMetrics.by) >= promMetricsCap {
			return
		}
		mm = &promAcc{}
		promMetrics.by[model] = mm
	}
	mm.requests++
	if s.status == http.StatusOK {
		mm.success++
	} else {
		mm.failed++
	}
	if s.mode == "stream" {
		mm.streaming++
	}
	totalMS := float64(total.Milliseconds())
	mm.latSum += totalMS
	gen := 0.0
	if s.ttfb > 0 {
		// 缺 TTFB 观测（非流式）时刻意不计生成时长：totalMS 含预填，混进均值会把
		// 平均速率拉成不可比的低值。
		mm.ttfbSum += float64(s.ttfb.Milliseconds())
		mm.ttfbN++
		gen = totalMS - float64(s.ttfb.Milliseconds())
	}
	if s.toks > 0 {
		mm.compTok += int64(s.toks)
		if gen > 0 {
			mm.genSec += gen / 1000
			mm.genTok += int64(s.toks)
		}
	}
	if s.hasCache {
		mm.cacheHit += s.cacheHit
		mm.cacheMiss += s.cacheMiss
	}
	if s.hasCredit && s.credit > 0 {
		mm.credit += s.credit
	}
}

// ModelStatPayload 单个模型的累计指标（供 /metrics 渲染）。
type ModelStatPayload struct {
	Model            string
	Success          int64
	Failed           int64
	Streaming        int64
	CompletionTokens int64
	CacheHitTokens   int64
	CacheMissTokens  int64
	Credit           float64
	AvgTTFBMS        float64
	AvgLatencyMS     float64
	TokensPerSec     float64
}

// MetricsSnapshot 一次指标快照。
type MetricsSnapshot struct {
	Since  time.Time
	Total  ModelStatPayload
	Models []ModelStatPayload
}

// MetricsSnapshotOf 取当前指标快照（导出供面板/测试使用）。
func MetricsSnapshotOf() MetricsSnapshot {
	promMetrics.mu.Lock()
	defer promMetrics.mu.Unlock()
	out := MetricsSnapshot{Since: promMetrics.since}
	var tot promAcc
	for name, mm := range promMetrics.by {
		out.Models = append(out.Models, deriveProm(name, mm))
		tot.requests += mm.requests
		tot.success += mm.success
		tot.failed += mm.failed
		tot.streaming += mm.streaming
		tot.ttfbSum += mm.ttfbSum
		tot.ttfbN += mm.ttfbN
		tot.latSum += mm.latSum
		tot.genSec += mm.genSec
		tot.genTok += mm.genTok
		tot.compTok += mm.compTok
		tot.cacheHit += mm.cacheHit
		tot.cacheMiss += mm.cacheMiss
		tot.credit += mm.credit
	}
	out.Total = deriveProm("total", &tot)
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].Model < out.Models[j].Model })
	return out
}

func deriveProm(name string, mm *promAcc) ModelStatPayload {
	p := ModelStatPayload{
		Model: name, Success: mm.success, Failed: mm.failed, Streaming: mm.streaming,
		CompletionTokens: mm.compTok, Credit: mm.credit,
		CacheHitTokens: mm.cacheHit, CacheMissTokens: mm.cacheMiss,
	}
	if mm.ttfbN > 0 {
		p.AvgTTFBMS = mm.ttfbSum / float64(mm.ttfbN)
	}
	if mm.requests > 0 {
		p.AvgLatencyMS = mm.latSum / float64(mm.requests)
	}
	if mm.genSec > 0 {
		p.TokensPerSec = float64(mm.genTok) / mm.genSec
	}
	return p
}

// promRealm 指标里的域标签（"" = 全部）。
var promRealms = []struct{ label, realm string }{{"cn", "cn"}, {"global", "global"}, {"all", ""}}

var promPoolStates = []string{
	"healthy", "cooling", "disabled", "paused", "in_flight_full", "breaker", "degraded", "model_cooled",
}

// promPoolCounts 单个域的账号状态计数。
type promPoolCounts struct {
	total, inFlight            int
	healthy, cooling, disabled int
	paused, inFlightFull       int
	breaker, degraded          int
	modelCooled                int
}

// aggregatePool 从 Status 列表聚合（realm 为空 = 不过滤）。
func aggregatePool(list []pool.Status, realm string) promPoolCounts {
	var c promPoolCounts
	for _, st := range list {
		if realm != "" && st.Realm != realm {
			continue
		}
		c.total++
		c.inFlight += st.InFlight
		switch {
		case st.Disabled:
			c.disabled++
		case st.Paused:
			c.paused++
		case st.Cooling:
			c.cooling++
		default:
			c.healthy++
		}
		if !st.BreakerUntil.IsZero() && st.BreakerUntil.After(time.Now()) {
			c.breaker++
		}
		if !st.DegradeUntil.IsZero() && st.DegradeUntil.After(time.Now()) {
			c.degraded++
		}
		if len(st.RateLimitedModels) > 0 {
			c.modelCooled++
		}
	}
	return c
}

func (c promPoolCounts) stateValue(state string) int {
	switch state {
	case "healthy":
		return c.healthy
	case "cooling":
		return c.cooling
	case "disabled":
		return c.disabled
	case "paused":
		return c.paused
	case "in_flight_full":
		return c.inFlightFull
	case "breaker":
		return c.breaker
	case "degraded":
		return c.degraded
	case "model_cooled":
		return c.modelCooled
	}
	return 0
}

type promWriter struct {
	sb   strings.Builder
	seen map[string]bool
}

func newPromWriter() *promWriter { return &promWriter{seen: map[string]bool{}} }

func (w *promWriter) family(name, help, typ string) {
	if w.seen[name] {
		return
	}
	w.seen[name] = true
	w.sb.WriteString("# HELP ")
	w.sb.WriteString(name)
	w.sb.WriteByte(' ')
	w.sb.WriteString(strings.ReplaceAll(strings.ReplaceAll(help, `\`, `\\`), "\n", `\n`))
	w.sb.WriteByte('\n')
	w.sb.WriteString("# TYPE ")
	w.sb.WriteString(name)
	w.sb.WriteByte(' ')
	w.sb.WriteString(typ)
	w.sb.WriteByte('\n')
}

func (w *promWriter) sample(name string, value float64, labels ...string) {
	w.sb.WriteString(name)
	if len(labels) > 0 {
		w.sb.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				w.sb.WriteByte(',')
			}
			w.sb.WriteString(labels[i])
			w.sb.WriteString(`="`)
			v := strings.ReplaceAll(labels[i+1], `\`, `\\`)
			v = strings.ReplaceAll(v, `"`, `\"`)
			v = strings.ReplaceAll(v, "\n", `\n`)
			w.sb.WriteString(v)
			w.sb.WriteByte('"')
		}
		w.sb.WriteByte('}')
	}
	w.sb.WriteByte(' ')
	w.sb.WriteString(formatPromValue(value))
	w.sb.WriteByte('\n')
}

func formatPromValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func writePromMetrics(snap MetricsSnapshot, health map[string]promPoolCounts, sticky int, explore int64, wafActive bool) string {
	w := newPromWriter()
	w.family("wb2api_pool_accounts_total", "Accounts in the pool.", "gauge")
	for _, rl := range promRealms {
		w.sample("wb2api_pool_accounts_total", float64(health[rl.label].total), "realm", rl.label)
	}
	w.family("wb2api_pool_accounts", "Accounts by realm and state.", "gauge")
	for _, rl := range promRealms {
		c := health[rl.label]
		for _, st := range promPoolStates {
			w.sample("wb2api_pool_accounts", float64(c.stateValue(st)), "realm", rl.label, "state", st)
		}
	}
	w.family("wb2api_pool_in_flight", "In-flight requests.", "gauge")
	for _, rl := range promRealms {
		w.sample("wb2api_pool_in_flight", float64(health[rl.label].inFlight), "realm", rl.label)
	}
	w.family("wb2api_sticky_sessions", "Sticky sessions.", "gauge")
	w.sample("wb2api_sticky_sessions", float64(sticky))
	w.family("wb2api_cost_explore_events_total", "costTier explore events.", "counter")
	w.sample("wb2api_cost_explore_events_total", float64(explore))
	w.family("wb2api_waf_ip_block_active", "1 when IP-level WAF block is active.", "gauge")
	if wafActive {
		w.sample("wb2api_waf_ip_block_active", 1)
	} else {
		w.sample("wb2api_waf_ip_block_active", 0)
	}
	since := 0.0
	if !snap.Since.IsZero() {
		since = float64(snap.Since.Unix())
	}
	w.family("wb2api_stats_since_timestamp_seconds", "Stats window start, unix seconds.", "gauge")
	w.sample("wb2api_stats_since_timestamp_seconds", since)
	w.family("wb2api_requests_total", "Chat requests.", "counter")
	w.sample("wb2api_requests_total", float64(snap.Total.Success), "status", "success")
	w.sample("wb2api_requests_total", float64(snap.Total.Failed), "status", "failed")
	w.family("wb2api_model_requests_total", "Chat requests by model.", "counter")
	for _, m := range snap.Models {
		w.sample("wb2api_model_requests_total", float64(m.Success), "model", m.Model, "status", "success")
		w.sample("wb2api_model_requests_total", float64(m.Failed), "model", m.Model, "status", "failed")
	}
	w.family("wb2api_model_credit_total", "Observed upstream credit by model.", "counter")
	for _, m := range snap.Models {
		w.sample("wb2api_model_credit_total", m.Credit, "model", m.Model)
	}
	w.family("wb2api_model_requests_streaming_total", "Streaming chat requests by model.", "counter")
	for _, m := range snap.Models {
		w.sample("wb2api_model_requests_streaming_total", float64(m.Streaming), "model", m.Model)
	}
	w.family("wb2api_model_tokens_total", "Completion tokens by model.", "counter")
	for _, m := range snap.Models {
		w.sample("wb2api_model_tokens_total", float64(m.CompletionTokens), "model", m.Model, "type", "completion")
	}
	w.family("wb2api_model_prompt_cache_tokens_total", "Upstream prompt cache tokens by model. Hit rate = hit/(hit+miss).", "counter")
	for _, m := range snap.Models {
		w.sample("wb2api_model_prompt_cache_tokens_total", float64(m.CacheHitTokens), "model", m.Model, "type", "hit")
		w.sample("wb2api_model_prompt_cache_tokens_total", float64(m.CacheMissTokens), "model", m.Model, "type", "miss")
	}
	w.family("wb2api_model_avg_ttfb_seconds", "Average TTFB by model.", "gauge")
	for _, m := range snap.Models {
		w.sample("wb2api_model_avg_ttfb_seconds", m.AvgTTFBMS/1000, "model", m.Model)
	}
	w.family("wb2api_model_avg_latency_seconds", "Average latency by model.", "gauge")
	for _, m := range snap.Models {
		w.sample("wb2api_model_avg_latency_seconds", m.AvgLatencyMS/1000, "model", m.Model)
	}
	w.family("wb2api_model_tokens_per_second", "Completion tokens per second by model.", "gauge")
	for _, m := range snap.Models {
		w.sample("wb2api_model_tokens_per_second", m.TokensPerSec, "model", m.Model)
	}
	return w.sb.String()
}

// promMetrics 渲染 /metrics。
func (h *Handler) promMetrics(w http.ResponseWriter, r *http.Request) {
	snap := MetricsSnapshotOf()
	health := map[string]promPoolCounts{}
	var explore int64
	if h.cfg.Pool != nil {
		list := h.cfg.Pool.List()
		for _, rl := range promRealms {
			health[rl.label] = aggregatePool(list, rl.realm)
		}
		explore, _ = h.cfg.Pool.CostExploreStatus()
	} else {
		for _, rl := range promRealms {
			health[rl.label] = promPoolCounts{}
		}
	}
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(writePromMetrics(snap, health, sticky, explore, h.wafIP.active())))
}

// WAFActive 透出 IP 级 WAF 拦截状态（供面板/诊断读取）。
func (h *Handler) WAFActive() bool { return h.wafIP.active() }
