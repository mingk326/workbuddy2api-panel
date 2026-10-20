package server

import (
	"strings"
	"testing"
	"time"
)

// 日上限：严格小于才放行；到顶后拒绝且 lastReject=daily。
func TestDailyBudgetStopsAtLimit(t *testing.T) {
	b := newDailyBudget(1)
	if ok, _ := b.admit(); !ok {
		t.Fatal("空账本应放行")
	}
	b.add(0.6, true)
	if ok, _ := b.admit(); !ok {
		t.Fatal("未到上限应放行")
	}
	b.add(0.4, true) // 累计 1.0 == limit
	if ok, _ := b.admit(); ok {
		t.Fatal("达到上限后应拒绝")
	}
	used, limit, rejected, last := b.snapshot()
	if used != 1.0 || limit != 1.0 || rejected == 0 || last != "daily" {
		t.Fatalf("snapshot used=%v limit=%v rejected=%d last=%q", used, limit, rejected, last)
	}
}

// 未观测到的 credit 不计入（缺失 ≠ 0）：否则闸门形同虚设。
func TestDailyBudgetIgnoresUnobservedCredit(t *testing.T) {
	b := newDailyBudget(1)
	b.add(5, false) // 未观测
	if used, _, _, _ := b.snapshot(); used != 0 {
		t.Fatalf("未观测的 credit 不应累计，used=%v", used)
	}
}

// 突发闸：5 分钟窗口内累计达阈值即拒；窗口外的旧样本被裁掉。
func TestDailyBudgetBurstGuard(t *testing.T) {
	b := newDailyBudget(0) // 日上限关闭，只开突发闸
	b.applyConfig(0, 30, 0)

	b.add(20, true)
	if ok, _ := b.admit(); !ok {
		t.Fatal("未达突发阈值应放行")
	}
	b.add(15, true) // 窗口内累计 35 >= 30
	ok, why := b.admit()
	if ok {
		t.Fatal("突发阈值达到后应拒绝")
	}
	if !strings.Contains(why, "burst") {
		t.Fatalf("拒因应说明 burst，got %q", why)
	}

	// 把窗口内样本手工挪到 6 分钟前 → 应被裁掉，重新放行。
	b.mu.Lock()
	for i := range b.burst {
		b.burst[i].at = time.Now().Add(-6 * time.Minute)
	}
	b.mu.Unlock()
	if ok, _ := b.admit(); !ok {
		t.Fatal("窗口外的旧样本不应继续计数")
	}
}

// 缓存率闸：连续 5 轮低命中触发 60s 冷却；恢复一轮即清零。
func TestDailyBudgetCacheGuard(t *testing.T) {
	b := newDailyBudget(0)
	b.applyConfig(0, 0, 50)

	// 未给缓存字段的轮次不参与判定。
	b.observeCache(0, 100, false)
	if b.cacheStreak != 0 {
		t.Fatalf("has=false 不应累计连败，got %d", b.cacheStreak)
	}
	for i := 0; i < 5; i++ {
		b.observeCache(1, 99, true) // 1% < 50%
	}
	if ok, why := b.admit(); ok {
		t.Fatal("连续 5 轮低缓存率后应冷却")
	} else if !strings.Contains(why, "cache") {
		t.Fatalf("拒因应说明 cache，got %q", why)
	}

	// 恢复一轮高命中 → 连败清零；但冷却窗未过前仍拒绝（防止抖动）。
	b.observeCache(90, 10, true)
	if b.cacheStreak != 0 {
		t.Fatalf("高命中应清零连败，got %d", b.cacheStreak)
	}

	// 冷却窗过期后自动放行。
	b.mu.Lock()
	b.cacheCoolTill = time.Now().Add(-time.Second)
	b.mu.Unlock()
	if ok, _ := b.admit(); !ok {
		t.Fatal("冷却窗过期后应放行")
	}
}

// 三道闸缺省全关：行为与旧版一致。
func TestDailyBudgetGuardsOffByDefault(t *testing.T) {
	b := newDailyBudget(0)
	b.add(1000, true)
	b.observeCache(0, 1000, true)
	if ok, _ := b.admit(); !ok {
		t.Fatal("三道闸缺省关闭时不得拦截")
	}
}

// 日界按 CST：跨日归零。
func TestDailyBudgetRollsAtCSTMidnight(t *testing.T) {
	b := newDailyBudget(10)
	b.add(10, true)
	if ok, _ := b.admit(); ok {
		t.Fatal("到顶应拒绝")
	}
	// 伪造「昨天」的账本 → 下一次访问惰性归零。
	b.mu.Lock()
	b.day = "2000-01-01"
	b.mu.Unlock()
	if ok, _ := b.admit(); !ok {
		t.Fatal("跨日应自动归零并放行")
	}
	if used, _, _, _ := b.snapshot(); used != 0 {
		t.Fatalf("跨日后 used 应归零，got %v", used)
	}
}
