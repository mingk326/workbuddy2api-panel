// growth.go 成长任务队列的即时入口（POST /admin/tasks/growth/run 用）。
//
// 成长队列的真实执行体在 panel.RunGrowthQueueOnce（通过 SetGrowthHook 延迟挂载），
// 这里只做门控 + 转发：排程到点与手动补跑走同一条链路，行为不分叉。
package scheduler

// RunGrowthNow 立即执行一轮成长任务队列（扫描全部账号 → 执行可自动化任务）。
// GrowthHook 未挂载（panel 尚未构造 / 单测裸用）或已显式关闭时静默跳过。
func (s *Scheduler) RunGrowthNow() {
	if s.cfg.GrowthDisabled {
		return
	}
	if fn := s.cfg.GrowthHook; fn != nil {
		fn()
	}
}
