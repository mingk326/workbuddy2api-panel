package scheduler

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestNewAccountSpreadDeterministic 偏移当天确定性（重启不重排）、落在窗口内、
// 相邻账号至少 minGap。
func TestNewAccountSpreadDeterministic(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, cstZone)
	uids := []string{"a", "b", "c", "d", "e"}
	window := 30 * time.Minute
	gap := 60 * time.Second

	p1 := newAccountSpread(taskCheckin, uids, window, gap, now)
	p2 := newAccountSpread(taskCheckin, uids, window, gap, now)
	if p1 == nil || p2 == nil {
		t.Fatal("计划不应为空")
	}
	var offs []time.Duration
	for _, u := range uids {
		at1, ok1 := p1.at[u]
		at2, ok2 := p2.at[u]
		if !ok1 || !ok2 {
			t.Fatalf("uid %s 不在计划里", u)
		}
		if !at1.Equal(at2) {
			t.Errorf("同一天两次生成偏移不一致：%s %v vs %v", u, at1, at2)
		}
		d := at1.Sub(now)
		if d < 0 || d >= window {
			t.Errorf("uid %s 偏移 %v 越界 [0,%v)", u, d, window)
		}
		offs = append(offs, d)
	}
	// 排序后除末尾钳制外，相邻间隔 >= gap。
	for i := 1; i < len(offs); i++ {
		for j := i; j > 0 && offs[j] < offs[j-1]; j-- {
			offs[j], offs[j-1] = offs[j-1], offs[j]
		}
	}
	for i := 1; i < len(offs); i++ {
		if offs[i]-offs[i-1] < gap && offs[i] != window-time.Second {
			t.Errorf("相邻偏移 %v / %v 小于 minGap %v", offs[i-1], offs[i], gap)
		}
	}
}

// TestNewAccountSpreadDisabled 窗口为 0 或单账号时不开摊开。
func TestNewAccountSpreadDisabled(t *testing.T) {
	now := time.Now()
	if p := newAccountSpread(taskCheckin, []string{"a", "b"}, 0, time.Second, now); p != nil {
		t.Error("窗口为 0 应关闭摊开")
	}
	if p := newAccountSpread(taskCheckin, []string{"a"}, 30*time.Minute, time.Second, now); p != nil {
		t.Error("单账号无需摊开")
	}
}

// TestAccountSpreadOrder 按计划时刻升序遍历（issue #45 塌缩修复）：
// uid 序与计划序不一致时，不能按 uid 序阻塞执行。
func TestAccountSpreadOrder(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, cstZone)
	p := newAccountSpread(taskCheckin, []string{"a", "b", "c"}, 30*time.Minute, 60*time.Second, now)
	if p == nil {
		t.Fatal("计划不应为空")
	}
	list := []pool.Status{{UID: "c"}, {UID: "a"}, {UID: "b"}}
	got := p.order(list)
	if len(got) != 3 {
		t.Fatalf("len=%d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if p.at[got[i].UID].Before(p.at[got[i-1].UID]) {
			t.Fatalf("顺序未按计划时刻升序: %v", []string{got[0].UID, got[1].UID, got[2].UID})
		}
	}
	// 计划为 nil（未启用 / 手动触发）时原样返回。
	var nilPlan *accountSpread
	if got := nilPlan.order(list); len(got) != 3 || got[0].UID != "c" {
		t.Errorf("nil plan 应原样返回: %+v", got)
	}
}

// TestSpreadPlanManualSkips 手动触发不摊开；排程触发但池为空也不产生计划。
func TestSpreadPlanManualSkips(t *testing.T) {
	s := New(Config{Pool: pool.New(""), AccountSpreadMinutes: 30, AccountSpreadMinGapSeconds: 60})
	if p := s.spreadPlan(taskCheckin, false, time.Now()); p != nil {
		t.Error("手动触发不应摊开")
	}
	if p := s.spreadPlan(taskCheckin, true, time.Now()); p != nil {
		t.Error("空池不应产生计划")
	}
}

// TestSetAccountSpread 热改参数：负窗口钳 0，非法间隔回落 30。
func TestSetAccountSpread(t *testing.T) {
	s := New(Config{})
	s.SetAccountSpread(-5, 0)
	w, g := s.spreadParams()
	if w != 0 {
		t.Errorf("window=%v want 0", w)
	}
	if g != 30*time.Second {
		t.Errorf("gap=%v want 30s", g)
	}
	s.SetAccountSpread(15, 45)
	w, g = s.spreadParams()
	if w != 15*time.Minute || g != 45*time.Second {
		t.Errorf("params=(%v,%v) want (15m,45s)", w, g)
	}
}
