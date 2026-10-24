// runstamps.go 启动补跑去重标记。
//
// 排程只活在内存里时，进程重启会被 missedToday 判成「今天的时点全过了、还没跑」，
// 于是每次启动都把签到 / 旅行 / 活跃上报对每个账号各打一遍；一天重开三次就是三倍
// 无意义请求（账号越多、被上游风控盯上的概率越高）。本文件把「今天已经补跑过哪几类
// 任务」落成一个小文件，让重启只在当天补一次。
//
// 只记日期不记时刻：精度到「天」就够（missedToday 的判据本来也是按天），文件里
// 最多几行，旧日期写过就丢，不需要清理策略。
package scheduler

import (
	"encoding/json"
	"log"
	"os"
	"time"
)

// String 补跑标记里的任务名（落盘键；改动会影响旧文件的识别，保持稳定）。
func (k taskKind) String() string {
	switch k {
	case taskCheckin:
		return "checkin"
	case taskTravel:
		return "travel"
	case taskActivity:
		return "activity"
	case taskKeepalive:
		return "keepalive"
	case taskBlackcat:
		return "blackcat"
	case taskGrowth:
		return "growth"
	}
	return "unknown"
}

// dayKey 补跑标记的日期键（本地时区，与排程整点同口径）。
func dayKey(now time.Time) string { return now.Format("2006-01-02") }

// missedToday 返回「今天时点已过、但今天还没补跑过」的任务类型。
//
// 判据与 nextFire 对称：某类任务的 nextFire 已滚到明天，说明今天的时点全部过去了。
// 只看下一次触发是否落在今天——跨零点窗口（夜猫子 23:00–08:00）若在 00:00–08:00
// 之间重启，昨夜那一轮不会被补；真有这种重启再升级判据。
func (s *Scheduler) missedToday(now time.Time) []taskKind {
	var out []taskKind
	for _, sl := range s.enabledSlots(now) {
		if sl.at.IsZero() {
			continue
		}
		if sl.at.Year() != now.Year() || sl.at.YearDay() != now.YearDay() {
			if s.ranToday(sl.kind, now) {
				continue // 今天已补跑过一轮：重启不该再把同样的请求打一遍
			}
			out = append(out, sl.kind)
		}
	}
	return out
}

// ranToday 报告 kind 今天是否已经补跑过。未配置 RunStampFile 时恒 false（旧行为）。
func (s *Scheduler) ranToday(kind taskKind, now time.Time) bool {
	if s.cfg.RunStampFile == "" {
		return false
	}
	s.stampMu.Lock()
	defer s.stampMu.Unlock()
	return s.loadStamps()[kind.String()] == dayKey(now)
}

// markRan 把 kinds 记成「今天已补跑」。落盘失败只记日志：下次重启多补一轮，
// 比让签到状态机因为写盘错误而停摆好。
func (s *Scheduler) markRan(kinds []taskKind, now time.Time) {
	if s.cfg.RunStampFile == "" || len(kinds) == 0 {
		return
	}
	s.stampMu.Lock()
	defer s.stampMu.Unlock()
	day := dayKey(now)
	st := s.loadStamps()
	for k, v := range st {
		if v != day {
			delete(st, k) // 只留当天，文件不会越长越大
		}
	}
	for _, k := range kinds {
		st[k.String()] = day
	}
	raw, err := json.Marshal(st)
	if err == nil {
		// 非关键小文件：tmp + rename 足够；损坏时 loadStamps 当空表（最多多补一轮）。
		tmp := s.cfg.RunStampFile + ".tmp"
		if err = os.WriteFile(tmp, raw, 0o600); err == nil {
			err = os.Rename(tmp, s.cfg.RunStampFile)
		}
	}
	if err != nil {
		log.Printf("scheduler: 补跑标记落盘失败（下次重启会再补一轮）: %v", err)
	}
}

// loadStamps 读标记；文件不在/坏了都当空表（宁可多补一轮，不因此不跑）。
func (s *Scheduler) loadStamps() map[string]string {
	raw, err := os.ReadFile(s.cfg.RunStampFile)
	if err != nil {
		return map[string]string{}
	}
	st := map[string]string{}
	if json.Unmarshal(raw, &st) != nil {
		return map[string]string{}
	}
	return st
}
