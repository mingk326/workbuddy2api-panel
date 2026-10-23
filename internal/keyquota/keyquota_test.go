package keyquota

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAdmitTokenAndRequestLimits 两类阈值：token 累计到上限后拒绝，
// 请求数达到上限后拒绝；<=0 的维度不限制但计数照常。
func TestAdmitTokenAndRequestLimits(t *testing.T) {
	tr := New("")
	defer tr.Close()

	if d, _ := tr.Admit("k1", Limits{DailyTokens: 10, DailyRequests: 2}); !d.OK {
		t.Fatalf("first admit denied: %+v", d)
	}
	if d, _ := tr.Admit("k1", Limits{DailyTokens: 10, DailyRequests: 2}); !d.OK {
		t.Fatalf("second admit denied: %+v", d)
	}
	d, _ := tr.Admit("k1", Limits{DailyTokens: 10, DailyRequests: 2})
	if d.OK || d.Reason != "request" || d.Used != 2 || d.Limit != 2 {
		t.Fatalf("request limit decision = %+v", d)
	}
	if d.RetryAfter <= 0 {
		t.Errorf("RetryAfter=%v want >0", d.RetryAfter)
	}

	tr.AddTokens("k1", 10)
	d, _ = tr.Admit("k1", Limits{DailyTokens: 10}) // 请求数不限制，token 已到上限
	if d.OK || d.Reason != "token" || d.Used != 10 || d.Limit != 10 {
		t.Fatalf("token limit decision = %+v", d)
	}

	// 另一个密钥不受影响；且无限制维度也会累计用量。
	if d, _ := tr.Admit("k2", Limits{DailyTokens: 10, DailyRequests: 2}); !d.OK {
		t.Fatalf("other key denied: %+v", d)
	}
	if tok, req := tr.Snapshot("k2"); tok != 0 || req != 1 {
		t.Errorf("k2 snapshot = (%d,%d) want (0,1)", tok, req)
	}
	if tok, req := tr.Snapshot("k1"); tok != 10 || req != 2 {
		t.Errorf("k1 snapshot = (%d,%d) want (10,2)", tok, req)
	}
}

// TestAdmitRPMSlidingWindow 滑动 60 秒窗口：到限即拒、窗口滑出后恢复，
// 且被拒的请求不消耗日请求数。
func TestAdmitRPMSlidingWindow(t *testing.T) {
	tr := New("")
	defer tr.Close()
	base := time.Now()
	now := base
	tr.now = func() time.Time { return now }

	lim := Limits{RPM: 2}
	if d, _ := tr.Admit("k1", lim); !d.OK {
		t.Fatalf("first deny: %+v", d)
	}
	now = base.Add(30 * time.Second)
	if d, _ := tr.Admit("k1", lim); !d.OK {
		t.Fatalf("second deny: %+v", d)
	}
	now = base.Add(59 * time.Second)
	d, _ := tr.Admit("k1", lim)
	if d.OK || d.Reason != "rpm" || d.Used != 2 || d.Limit != 2 {
		t.Fatalf("rpm decision = %+v", d)
	}
	if d.RetryAfter <= 0 || d.RetryAfter > time.Minute {
		t.Errorf("RetryAfter=%v want (0,60s]", d.RetryAfter)
	}
	if _, req := tr.Snapshot("k1"); req != 2 {
		t.Errorf("rejected rpm must not consume daily request: req=%d want 2", req)
	}
	if rpm, _ := tr.Live("k1"); rpm != 2 {
		t.Errorf("live rpm=%d want 2", rpm)
	}

	// 61s 时第一条（base+0s）滑出窗口，恢复准入。
	now = base.Add(61 * time.Second)
	if d, _ := tr.Admit("k1", lim); !d.OK {
		t.Fatalf("after window slide deny: %+v", d)
	}
}

// TestConcurrencyLease 并发上限：lease 未归还原地拒绝，归还后恢复；Release 幂等。
func TestConcurrencyLease(t *testing.T) {
	tr := New("")
	defer tr.Close()

	lim := Limits{MaxConcurrency: 1}
	d, lease := tr.Admit("k1", lim)
	if !d.OK || lease == nil {
		t.Fatalf("first admit = %+v lease=%v", d, lease)
	}
	if _, in := tr.Live("k1"); in != 1 {
		t.Fatalf("live inflight=%d want 1", in)
	}
	d, lease2 := tr.Admit("k1", lim)
	if d.OK || d.Reason != "concurrency" || d.Used != 1 || d.Limit != 1 || lease2 != nil {
		t.Fatalf("concurrency decision = %+v lease=%v", d, lease2)
	}
	if d.RetryAfter <= 0 {
		t.Errorf("RetryAfter=%v want >0", d.RetryAfter)
	}
	// 被拒的并发请求不消耗日请求数。
	if _, req := tr.Snapshot("k1"); req != 1 {
		t.Errorf("rejected concurrency must not consume daily request: req=%d want 1", req)
	}

	lease.Release()
	lease.Release() // 幂等
	if _, in := tr.Live("k1"); in != 0 {
		t.Fatalf("live inflight=%d want 0", in)
	}
	d, lease = tr.Admit("k1", lim)
	if !d.OK || lease == nil {
		t.Fatalf("after release admit = %+v lease=%v", d, lease)
	}
	lease.Release()
}

// TestUnlimitedLimitsNoTracking 未配置 RPM/并发时不记录瞬时状态，且不拒绝。
func TestUnlimitedLimitsNoTracking(t *testing.T) {
	tr := New("")
	defer tr.Close()
	for i := 0; i < 5; i++ {
		d, lease := tr.Admit("k1", Limits{})
		if !d.OK || lease != nil {
			t.Fatalf("admit %d = %+v lease=%v", i, d, lease)
		}
	}
	if rpm, in := tr.Live("k1"); rpm != 0 || in != 0 {
		t.Fatalf("live=(%d,%d) want (0,0)", rpm, in)
	}
}

// TestDayRoll 跨 CST 自然日后计数归零。
func TestDayRoll(t *testing.T) {
	tr := New("")
	defer tr.Close()
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, cstZone)
	tr.now = func() time.Time { return base }
	tr.day = cstDay(base)

	tr.Admit("k1", Limits{DailyTokens: 100, DailyRequests: 10})
	tr.AddTokens("k1", 42)
	if tok, req := tr.Snapshot("k1"); tok != 42 || req != 1 {
		t.Fatalf("before roll = (%d,%d)", tok, req)
	}

	tr.now = func() time.Time { return base.Add(24 * time.Hour) }
	if tok, req := tr.Snapshot("k1"); tok != 0 || req != 0 {
		t.Fatalf("after roll = (%d,%d) want (0,0)", tok, req)
	}
	if d, _ := tr.Admit("k1", Limits{DailyTokens: 100, DailyRequests: 10}); !d.OK {
		t.Fatalf("admit after roll denied: %+v", d)
	}
}

// TestPersistence 计数落盘并在重启后读回。
func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key-usage.json")
	tr := New(path)
	tr.Admit("key-ef797c81", Limits{})
	tr.Admit("key-ef797c81", Limits{})
	tr.AddTokens("key-ef797c81", 1234)
	tr.Close()

	tr2 := New(path)
	defer tr2.Close()
	tok, req := tr2.Snapshot("key-ef797c81")
	if tok != 1234 || req != 2 {
		t.Fatalf("reloaded = (%d,%d) want (1234,2)", tok, req)
	}
}

// TestPersistenceDayMismatch 落盘日期不是今天 → 计数从零开始（不沿用旧日数据）。
func TestPersistenceDayMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key-usage.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"day":"2000-01-01","keys":{"k1":{"tokens":9,"requests":9}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := New(path)
	defer tr.Close()
	if tok, req := tr.Snapshot("k1"); tok != 0 || req != 0 {
		t.Fatalf("stale day loaded: (%d,%d)", tok, req)
	}
}

// TestNilTracker nil 安全（未接线场景）。
func TestNilTracker(t *testing.T) {
	var tr *Tracker
	if d, lease := tr.Admit("k", Limits{DailyTokens: 1, DailyRequests: 1}); !d.OK || lease != nil {
		t.Errorf("nil tracker should allow: %+v lease=%v", d, lease)
	}
	tr.AddTokens("k", 10)
	if tok, req := tr.Snapshot("k"); tok != 0 || req != 0 {
		t.Errorf("nil snapshot = (%d,%d)", tok, req)
	}
	if rpm, in := tr.Live("k"); rpm != 0 || in != 0 {
		t.Errorf("nil live = (%d,%d)", rpm, in)
	}
	tr.Close()
}
