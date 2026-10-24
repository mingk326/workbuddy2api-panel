package scheduler

import (
	"path/filepath"
	"testing"
	"time"
)

func stampScheduler(path string) *Scheduler {
	return New(Config{
		CheckinHours:      []int{9},
		TravelHours:       []int{10},
		ActivityHours:     []int{11},
		KeepaliveHours:    []int{22},
		BlackcatHours:     []int{23},
		GrowthHours:       []int{1},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		BlackcatDisabled:  true,
		GrowthDisabled:    true,
		RunStampFile:      path,
	})
}

// TestMissedTodayRunStamps 今天的时点已过 → 补跑一次；落盘后同日重启不再补。
func TestMissedTodayRunStamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-runs.json")
	sch := stampScheduler(path)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.Local)

	missed := sch.missedToday(now)
	if len(missed) != 1 || missed[0] != taskCheckin {
		t.Fatalf("missed=%v want [checkin]", missed)
	}
	sch.markRan(missed, now)
	if got := sch.missedToday(now); len(got) != 0 {
		t.Fatalf("markRan 后 missed=%v want empty", got)
	}

	// 模拟重启：新实例读同一文件，同日不再补。
	sch2 := stampScheduler(path)
	if got := sch2.missedToday(now); len(got) != 0 {
		t.Fatalf("重启后同日 missed=%v want empty", got)
	}
	// 次日旧标记失效，重新补。
	if got := sch2.missedToday(now.Add(24 * time.Hour)); len(got) != 1 || got[0] != taskCheckin {
		t.Fatalf("次日 missed=%v want [checkin]", got)
	}
}

// TestMissedTodayFutureSlotNotMissed 时点还没到 → 不补跑。
func TestMissedTodayFutureSlotNotMissed(t *testing.T) {
	sch := stampScheduler("")
	now := time.Date(2026, 10, 23, 8, 0, 0, 0, time.Local)
	if got := sch.missedToday(now); len(got) != 0 {
		t.Fatalf("8:00 时 9 点签到未到，missed=%v want empty", got)
	}
}

// TestRunStampDisabled 未配置 RunStampFile → 恒不认为已补跑（旧行为）。
func TestRunStampDisabled(t *testing.T) {
	sch := stampScheduler("")
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.Local)
	if sch.ranToday(taskCheckin, now) {
		t.Error("无文件时应恒 false")
	}
	sch.markRan([]taskKind{taskCheckin}, now) // 不应 panic / 写文件
	if sch.ranToday(taskCheckin, now) {
		t.Error("无文件时 markRan 不应改变状态")
	}
}
