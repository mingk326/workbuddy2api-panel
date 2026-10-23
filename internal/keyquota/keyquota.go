// Package keyquota 按 API 密钥统计并限制每日用量（Token / 请求数）。
//
// 口径：
//   - 日界为 CST 自然日 00:00（与 server 的积分闸、scheduler 的日任务一致）；
//   - Token 只累计**上游观测到的** usage（total_tokens，缺失时由调用方回落
//     prompt+completion）；上游没回 usage 的请求不计 token（请求数仍然计）；
//   - 请求数在通过配额检查时 +1：被上游拒绝的请求也算，符合"请求数"直觉；
//   - 限额 <=0 = 不限制，但计数照常累计（面板可看今日用量）。
//
// 落盘：state 同目录 key-usage.json，防抖 15s + Close 时强制刷盘，原子替换。
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

// Decision 一次配额检查结果。
type Decision struct {
	OK         bool
	Reason     string // "token" / "request"（OK=false 时有效）
	Used       int64
	Limit      int64
	RetryAfter time.Duration // 到次日 00:00 CST
}

// Tracker 并发安全的按密钥日用量统计器。
type Tracker struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
	day  string
	keys map[string]*counter
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
		path: path,
		now:  time.Now,
		day:  cstDay(time.Now()),
		keys: make(map[string]*counter),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	t.load()
	if path != "" {
		go t.loop()
	} else {
		close(t.done)
	}
	return t
}

// Admit 检查密钥是否还能发起一次请求；通过时请求数 +1。
//
// tokenLimit / requestLimit <=0 表示该维度不限制，但计数照常进行。
func (t *Tracker) Admit(id string, tokenLimit, requestLimit int64) Decision {
	if t == nil {
		return Decision{OK: true}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollLocked()
	if id == "" {
		return Decision{OK: true}
	}
	c := t.counterLocked(id)
	if tokenLimit > 0 && c.Tokens >= tokenLimit {
		return Decision{Reason: "token", Used: c.Tokens, Limit: tokenLimit, RetryAfter: untilMidnight(t.now())}
	}
	if requestLimit > 0 && c.Requests >= requestLimit {
		return Decision{Reason: "request", Used: c.Requests, Limit: requestLimit, RetryAfter: untilMidnight(t.now())}
	}
	c.Requests++
	t.gen++
	return Decision{OK: true}
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
