// Package keyquota 按 API 密钥统计并限制用量（每日 Token / 请求数 + 每分钟请求数 / 并发）。
//
// 口径：
//   - 日界为 CST 自然日 00:00（与 server 的积分闸、scheduler 的日任务一致）；
//   - Token 只累计**上游观测到的** usage（total_tokens，缺失时由调用方回落
//     prompt+completion）；上游没回 usage 的请求不计 token（请求数仍然计）；
//   - 请求数在通过准入时 +1：被上游拒绝的请求也算，符合"请求数"直觉；
//   - RPM 为滑动 60 秒窗口，只统计通过准入的客户端请求（内部重试/降级不重复计）；
//   - 并发从准入成功持续到响应结束（流式到最后一帧），由 Lease.Release 归还；
//   - 限额 <=0 = 不限制，但日用量照常累计（面板可看今日用量）。
//
// 落盘：仅日用量写 state 同目录 key-usage.json，防抖 15s + Close 时强制刷盘，
// 原子替换。RPM 窗口与并发计数是瞬时状态，不落盘（重启清零）。
// 单进程假设：多进程共用同一文件会互相覆盖（与全局积分闸的进程内口径一致）。
package keyquota

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var cstZone = time.FixedZone("CST", 8*60*60)

func cstDay(t time.Time) string { return t.In(cstZone).Format("2006-01-02") }

const (
	fileVersion   = 1
	flushInterval = 15 * time.Second
	rpmWindow     = time.Minute
)

// counter 单个密钥当天累计量。
type counter struct {
	Tokens   int64 `json:"tokens"`
	Requests int64 `json:"requests"`
}

// fileData 落盘结构。
type fileData struct {
	Version int                 `json:"version"`
	Saved   string              `json:"saved"`
	Day     string              `json:"day"`
	Keys    map[string]*counter `json:"keys"`
}

// Limits 一次准入的限额；各维度 <=0 = 不限制。
type Limits struct {
	DailyTokens    int64
	DailyRequests  int64
	RPM            int
	MaxConcurrency int
}

// Decision 一次准入结果。
type Decision struct {
	OK     bool
	Reason string // "token" / "request" / "rpm" / "concurrency"（OK=false 时有效）
	Used   int64
	Limit  int64
	// RetryAfter 建议的重试等待：日配额到次日 00:00 CST，RPM 到窗口腾出，
	// 并发固定 1s（等在途请求结束）。
	RetryAfter time.Duration
}

// Lease 一次并发配额占用；响应结束（含流式最后一帧）时 Release 归还。
// Release 幂等，可安全地在多个 defer 中调用。
type Lease struct {
	t    *Tracker
	id   string
	once sync.Once
}

// Release 归还在途并发计数；lease 为 nil 时无操作。
func (l *Lease) Release() {
	if l == nil || l.t == nil || l.id == "" {
		return
	}
	l.once.Do(func() {
		t := l.t
		t.mu.Lock()
		if n := t.inflight[l.id]; n > 1 {
			t.inflight[l.id] = n - 1
		} else {
			delete(t.inflight, l.id)
		}
		t.mu.Unlock()
	})
}

// Tracker 并发安全的按密钥用量统计器。
type Tracker struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
	day  string
	keys map[string]*counter
	// rpm 每个密钥最近一分钟内的准入时间戳（仅在设置 RPM 上限时记录）。
	rpm map[string][]time.Time
	// inflight 每个密钥在途请求数（仅在设置并发上限时记录）。
	inflight map[string]int
	// gen / writtenGen 用于防抖刷盘：写盘期间若有新累计，下一轮继续写。
	gen        uint64
	writtenGen uint64

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// New 创建统计器并加载已有计数；path 为空 = 纯内存（测试/禁用落盘）。
func New(path string) *Tracker {
	t := &Tracker{
		path:     path,
		now:      time.Now,
		day:      cstDay(time.Now()),
		keys:     make(map[string]*counter),
		rpm:      make(map[string][]time.Time),
		inflight: make(map[string]int),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	t.load()
	if path != "" {
		go t.loop()
	} else {
		close(t.done)
	}
	return t
}

// Admit 检查一次请求能否准入；通过时累计日请求数与 RPM，并（若配置了并发上限）
// 返回一个必须在响应结束时 Release 的 Lease。
//
// 日配额被拒的请求不消耗任何计数；RPM/并发被拒同样不计日请求数。
func (t *Tracker) Admit(id string, lim Limits) (Decision, *Lease) {
	if t == nil {
		return Decision{OK: true}, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollLocked()
	if id == "" {
		return Decision{OK: true}, nil
	}
	c := t.counterLocked(id)
	if lim.DailyTokens > 0 && c.Tokens >= lim.DailyTokens {
		return Decision{Reason: "token", Used: c.Tokens, Limit: lim.DailyTokens, RetryAfter: untilMidnight(t.now())}, nil
	}
	if lim.DailyRequests > 0 && c.Requests >= lim.DailyRequests {
		return Decision{Reason: "request", Used: c.Requests, Limit: lim.DailyRequests, RetryAfter: untilMidnight(t.now())}, nil
	}
	now := t.now()
	// 并发先做只读预检，避免"RPM 已记了一笔、并发却被拒"的错账。
	if lim.MaxConcurrency > 0 && t.inflight[id] >= lim.MaxConcurrency {
		return Decision{Reason: "concurrency", Used: int64(t.inflight[id]), Limit: int64(lim.MaxConcurrency),
			RetryAfter: time.Second}, nil
	}
	var win []time.Time
	if lim.RPM > 0 {
		win = t.purgeRPM(id, now)
		if len(win) >= lim.RPM {
			retry := win[0].Add(rpmWindow).Sub(now)
			if retry < time.Second {
				retry = time.Second
			}
			return Decision{Reason: "rpm", Used: int64(len(win)), Limit: int64(lim.RPM), RetryAfter: retry}, nil
		}
	}
	// 提交：全部维度都通过后才落计数。
	if lim.RPM > 0 {
		win = append(win, now)
		t.rpm[id] = win
	}
	var lease *Lease
	if lim.MaxConcurrency > 0 {
		t.inflight[id]++
		lease = &Lease{t: t, id: id}
	}
	c.Requests++
	t.gen++
	return Decision{OK: true}, lease
}

// AddTokens 累计一次已观测的 token 用量（<=0 不计）。
func (t *Tracker) AddTokens(id string, tokens int64) {
	if t == nil || id == "" || tokens <= 0 {
		return
	}
	t.mu.Lock()
	t.rollLocked()
	t.counterLocked(id).Tokens += tokens
	t.gen++
	t.mu.Unlock()
}

// Snapshot 返回该密钥今天的 (tokens, requests)。
func (t *Tracker) Snapshot(id string) (tokens, requests int64) {
	if t == nil || id == "" {
		return 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollLocked()
	if c := t.keys[id]; c != nil {
		return c.Tokens, c.Requests
	}
	return 0, 0
}

// Live 返回该密钥当前滑动一分钟内的准入请求数与在途并发数。
// 仅当对应维度配置了上限时才有计数；未设置时返回 0（面板显示 —）。
func (t *Tracker) Live(id string) (rpm, inflight int) {
	if t == nil || id == "" {
		return 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.purgeRPM(id, t.now())), t.inflight[id]
}

// purgeRPM 清掉滑出 60 秒窗口的时间戳并返回剩余窗口（调用方持锁）。
func (t *Tracker) purgeRPM(id string, now time.Time) []time.Time {
	win := t.rpm[id]
	cut := now.Add(-rpmWindow)
	i := 0
	for i < len(win) && !win[i].After(cut) {
		i++
	}
	if i == 0 {
		return win
	}
	win = win[i:]
	if len(win) == 0 {
		delete(t.rpm, id)
		return nil
	}
	t.rpm[id] = win
	return win
}

// Close 停止后台刷盘并做最后一次落盘。
func (t *Tracker) Close() {
	if t == nil || t.path == "" {
		return
	}
	t.closeOnce.Do(func() {
		close(t.stop)
		<-t.done
		t.flush()
	})
}

func (t *Tracker) counterLocked(id string) *counter {
	c := t.keys[id]
	if c == nil {
		c = &counter{}
		t.keys[id] = c
	}
	return c
}

// rollLocked 跨日清零（惰性，任何入口先调用）。
func (t *Tracker) rollLocked() {
	today := cstDay(t.now())
	if today == t.day {
		return
	}
	t.day = today
	t.keys = make(map[string]*counter)
	t.gen++
}

func (t *Tracker) loop() {
	defer close(t.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.flush()
		case <-t.stop:
			return
		}
	}
}

func (t *Tracker) load() {
	if t.path == "" {
		return
	}
	raw, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	var d fileData
	if json.Unmarshal(raw, &d) != nil || d.Version != fileVersion {
		return
	}
	if d.Day != t.day || len(d.Keys) == 0 {
		return
	}
	for id, c := range d.Keys {
		if c != nil {
			t.keys[id] = &counter{Tokens: c.Tokens, Requests: c.Requests}
		}
	}
}

// flush 把当前计数原子写入磁盘；无变化时跳过。
func (t *Tracker) flush() {
	if t.path == "" {
		return
	}
	t.mu.Lock()
	if t.gen == t.writtenGen {
		t.mu.Unlock()
		return
	}
	gen := t.gen
	data := fileData{
		Version: fileVersion,
		Saved:   t.now().In(cstZone).Format(time.RFC3339),
		Day:     t.day,
		Keys:    make(map[string]*counter, len(t.keys)),
	}
	for id, c := range t.keys {
		data.Keys[id] = &counter{Tokens: c.Tokens, Requests: c.Requests}
	}
	t.mu.Unlock()

	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	dir := filepath.Dir(t.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, t.path); err != nil {
		_ = os.Remove(tmp)
		return
	}
	t.mu.Lock()
	if t.writtenGen < gen {
		t.writtenGen = gen
	}
	t.mu.Unlock()
}

// untilMidnight 返回到下一个 CST 00:00 的时长（至少 1s）。
func untilMidnight(now time.Time) time.Duration {
	n := now.In(cstZone)
	next := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, cstZone).Add(24 * time.Hour)
	d := next.Sub(n)
	if d < time.Second {
		d = time.Second
	}
	return d
}
