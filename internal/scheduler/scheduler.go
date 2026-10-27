// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive 四类独立排程。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/credithist"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即四类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client
	// CreditHist 积分变动流水（nil = 不记账）。只在**真实查到**余额后 Observe，
	// 不喂池内缓存值；首次观测只建基线不留痕。
	CreditHist     *credithist.Ledger
	CheckinHours   []int // 默认 [9, 21]
	TravelHours    []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours  []int // 默认 [10]
	KeepaliveHours []int // 默认 [22]
	BlackcatHours  []int // 默认 [23]：夜猫子（23:00–08:00 计数窗口）
	GrowthHours    []int // 默认 [1]：成长任务队列（Sequential 族每日零点解锁一环，
	// 01:00 自动扫描+执行；避开零点整防解锁竞态）

	// ExpiringSoonWindow 快过期积分窗口：签到/余额刷新查余额时，把到期时间
	// <= now+window 的套餐余额标记为"快过期"（pool 据此做最早到期优先，见
	// entry.creditsEarliestExpiry）。<=0 时不做路由门槛；展示仍使用完整逐包数据。
	ExpiringSoonWindow time.Duration

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// BlackcatDisabled 显式关闭夜猫子排程（schedule.blackcat_enabled=false）。
	BlackcatDisabled bool
	// GrowthDisabled 显式关闭成长任务自动排程（schedule.growth_enabled=false）。
	GrowthDisabled bool

	// IncludeDisabledInTasks 让「保号类」四任务（签到 / 活跃上报 / token 保活 / 余额刷新）
	// 对已禁用（disabled）的账号也执行（schedule.include_disabled_in_tasks）。
	// 缺省 false = 保持「禁用的跳过」既有语义；打开后禁用号照常签到保号，但**仍不参与
	// 选号**——pool.pick 侧的 disabled 过滤与本开关无关。
	IncludeDisabledInTasks bool

	// AccountSpreadMinutes 账号级摊开窗口（分钟，0 = 关闭）：排程触发的账号遍历型
	// 任务（签到 / 旅行 / 活跃上报 / 保活）每号在窗口内自己的偏移上执行，
	// 不再同一秒打全池。手动触发不受影响。见 spread.go。
	AccountSpreadMinutes int
	// AccountSpreadMinGapSeconds 摊开时相邻账号的最小间隔（秒）；<=0 回落 30。
	AccountSpreadMinGapSeconds int

	// GrowthHook 成长任务队列执行回调（panel.RunGrowthQueueOnce：扫描全部账号
	// 待办并执行，与面板「执行全部待办」按钮同管线）。调度器只管时点不管实现——
	// panel 在 scheduler 之后构造，用 SetGrowthHook 事后挂载；nil 时到点跳过。
	GrowthHook func()

	// TaskSweepHook 任务补领清扫回调（panel.RunTaskSweepOnce：把「已达标未领」
	// 的任务奖励领掉，不跑任何行为链路）。与 GrowthHook 同款延迟挂载；nil 时
	// ticker 到点跳过。见 issue #174：成长队列与「一键完成」都不负责这条补领链路。
	TaskSweepHook func()

	// RunStampFile 启动补跑去重记录的落盘路径（见 runstamps.go）。空 = 不持久化，
	// 每次重启都补一轮；配置后同一类任务当天只补一次。
	RunStampFile string
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// stampMu 串行化补跑标记文件的读写（见 runstamps.go）。
	stampMu sync.Mutex

	// schedMu 保护排程参数（时点/开关）；Reconfigure 可在运行期热改（面板保存配置时调用）。
	// rearmSchedule/rearmBalance/rearmSweep 是「排程已变，立即重算」通知：Run 与各周期
	// 循环各自消费，分别用独立 channel（同 channel 被多个 select 消费会丢信号）。
	schedMu       sync.Mutex
	rearmSchedule chan struct{}
	rearmBalance  chan struct{}
	rearmSweep    chan struct{}

	// balanceInterval 余额刷新间隔（纳秒，0=暂停）。atomic 读写：执行循环每轮读当前值，
	// SetBalanceInterval 可任意时刻热改（面板保存配置）。
	balanceInterval atomic.Int64

	// sweepInterval 任务补领清扫间隔（纳秒，0=暂停）。语义同 balanceInterval：
	// SetTaskSweepInterval 可热改（面板保存配置后下一轮生效）。
	sweepInterval atomic.Int64

	// balanceJitter/sweepJitter 周期抖动比例（千分比 0-500 = 0-50%，atomic 读写）。
	// >0 时余额刷新/补领清扫的下一轮间隔在 ±frac×interval 内随机偏移（见 humanize.go），
	// 不再精确固定间隔打卡。余额刷新另把账号遍历摊开（spreadStaggerDelay）。
	balanceJitter atomic.Int64
	sweepJitter   atomic.Int64

	// nightAccountCap 夜间任务参与上限（>0 = 只让积分最高的前 N 个号参加夜猫子）。
	// atomic 读写，Reconfigure 可热改。0 = 不限（旧行为）。
	nightAccountCap atomic.Int64

	// authHealMu/authHealAt 401→refresh 求证的退避记账（uid → 上次求证时刻）。
	// 见 allowAuthHeal：降级 token 会让每轮探测都 401，不设退避就是每 5 分钟轮换一次 RT。
	authHealMu sync.Mutex
	authHealAt map[string]time.Time

	// spreadMinutes/spreadGapSeconds 账号级摊开参数（分钟 / 秒，0 = 关闭）。
	// atomic 读写：面板保存配置后可热改，下一轮排程任务立即生效。
	spreadMinutes    atomic.Int64
	spreadGapSeconds atomic.Int64

	// runCtxMu/baseCtx 主循环 ctx（Run 启动时保存）：摊开等待据此在优雅停机时
	// 立即退出，不必等睡满窗口。
	runCtxMu sync.RWMutex
	baseCtx  context.Context
}

// authHealBackoff 同一账号 401→refresh 求证的最小间隔。
// 降级 token（billing 恒 401、chat 可用，issue #215 评论实测）每轮余额探测都会命中
// 401；若每轮都 refresh，一天会轮换 288 次 RT——既换不来可用凭据，又平白放大上游
// 风控与并发竞态。窗口内只记录日志、不再刷新；真死号走 refresh 12153 分支（不吃退避，
// 首次即判死），因此退避不影响「15 分钟禁用」的主路径。
const authHealBackoff = 30 * time.Minute

// allowAuthHeal 退避窗口判定：窗口内已「成功求证」过就不再刷新。
// 只记录成功（refresh 返回 nil）——真死号每轮 refresh 都回 12153 且不轮换 RT，
// 必须每轮都试，连续计数才能按预期在 3 次（≈15 分钟）内判死；把失败也记进退避
// 会让死号永远停在 1/3。
func (s *Scheduler) allowAuthHeal(uid string) bool {
	s.authHealMu.Lock()
	defer s.authHealMu.Unlock()
	if s.authHealAt == nil {
		s.authHealAt = make(map[string]time.Time)
	}
	if t, ok := s.authHealAt[uid]; ok && time.Since(t) < authHealBackoff {
		return false
	}
	return true
}

// noteAuthHeal 记录一次「refresh 成功」的求证时刻（供 allowAuthHeal 退避）。
func (s *Scheduler) noteAuthHeal(uid string) {
	s.authHealMu.Lock()
	defer s.authHealMu.Unlock()
	if s.authHealAt == nil {
		s.authHealAt = make(map[string]time.Time)
	}
	s.authHealAt[uid] = time.Now()
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.BlackcatHours) == 0 {
		cfg.BlackcatHours = []int{23}
	}
	s := &Scheduler{
		cfg:           cfg,
		adoptTried:    make(map[string]string),
		rearmSchedule: make(chan struct{}, 1),
		rearmBalance:  make(chan struct{}, 1),
		rearmSweep:    make(chan struct{}, 1),
	}
	s.SetAccountSpread(cfg.AccountSpreadMinutes, cfg.AccountSpreadMinGapSeconds)
	return s
}

// ExpiringSoonWindow 返回当前快过期路由窗口（读取时与热配置写在 schedMu 下同步）。
func (s *Scheduler) ExpiringSoonWindow() time.Duration {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.ExpiringSoonWindow
}

// SetExpiringSoonWindow 热更新快过期路由窗口。窗口变化时清空池内旧快照，避免在下一轮
// 余额刷新覆盖前，继续用旧窗口得出的最早到期顺序选号。
func (s *Scheduler) SetExpiringSoonWindow(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.schedMu.Lock()
	changed := s.cfg.ExpiringSoonWindow != d
	s.cfg.ExpiringSoonWindow = d
	poolRef := s.cfg.Pool
	s.schedMu.Unlock()
	if changed && poolRef != nil {
		poolRef.ClearExpiringSnapshots()
	}
}

// Reconfigure 热更新排程参数（面板保存配置后调用）：改时点/开关并通知运行中的循环重算。
// 空 hours 视为「未配置」保留原值（与 config.normalize 的回落语义一致）。
// SetGrowthHook 挂载/替换成长任务队列回调（panel 构造晚于 scheduler，事后接线）。
func (s *Scheduler) SetGrowthHook(fn func()) {
	s.schedMu.Lock()
	s.cfg.GrowthHook = fn
	s.schedMu.Unlock()
}

// includeDisabledInTasks 返回保号类任务是否应覆盖禁用账号（配置快照，供循环开头取一次）。
func (s *Scheduler) includeDisabledInTasks() bool {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.IncludeDisabledInTasks
}

// SetIncludeDisabledInTasks 热更新「保号类任务是否覆盖禁用账号」
// （schedule.include_disabled_in_tasks）。
// 用独立 setter 而非并入 Reconfigure：后者已有 12 个位置参数，继续追加会让调用点难以
// 校对；本开关语义独立（只影响账号过滤，不影响时点），单独设值更清晰。
func (s *Scheduler) SetIncludeDisabledInTasks(v bool) {
	s.schedMu.Lock()
	s.cfg.IncludeDisabledInTasks = v
	s.schedMu.Unlock()
}

func (s *Scheduler) Reconfigure(checkinHours, travelHours, activityHours, keepaliveHours, blackcatHours, growthHours []int,
	checkinDisabled, travelDisabled, activityDisabled, keepaliveDisabled, blackcatDisabled, growthDisabled bool) {
	s.schedMu.Lock()
	if len(checkinHours) > 0 {
		s.cfg.CheckinHours = checkinHours
	}
	if len(travelHours) > 0 {
		s.cfg.TravelHours = travelHours
	}
	if len(activityHours) > 0 {
		s.cfg.ActivityHours = activityHours
	}
	if len(keepaliveHours) > 0 {
		s.cfg.KeepaliveHours = keepaliveHours
	}
	if len(blackcatHours) > 0 {
		s.cfg.BlackcatHours = blackcatHours
	}
	if len(growthHours) > 0 {
		s.cfg.GrowthHours = growthHours
	}
	s.cfg.CheckinDisabled = checkinDisabled
	s.cfg.TravelDisabled = travelDisabled
	s.cfg.ActivityDisabled = activityDisabled
	s.cfg.KeepaliveDisabled = keepaliveDisabled
	s.cfg.BlackcatDisabled = blackcatDisabled
	s.cfg.GrowthDisabled = growthDisabled
	s.schedMu.Unlock()
	poke(s.rearmSchedule)
	poke(s.rearmBalance)
	poke(s.rearmSweep)
}

// poke 非阻塞发一次唤醒信号（已有待处理信号则忽略，语义等价）。
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskBlackcat
	taskGrowth
)

// slot 单个任务的计划触发时刻。
type slot struct {
	at   time.Time
	kind taskKind
}

// enabledSlots 返回各类任务的下一次触发时点（本地时区整点）。
// 已显式禁用的任务不进候选。排程参数在 schedMu 下快照，与 Reconfigure 的并发写隔离。
func (s *Scheduler) enabledSlots(now time.Time) []slot {
	s.schedMu.Lock()
	checkinHours, keepaliveHours, blackcatHours := s.cfg.CheckinHours, s.cfg.KeepaliveHours, s.cfg.BlackcatHours
	travelHours, activityHours := s.cfg.TravelHours, s.cfg.ActivityHours
	checkinOff, keepaliveOff, blackcatOff := s.cfg.CheckinDisabled, s.cfg.KeepaliveDisabled, s.cfg.BlackcatDisabled
	growthHours, growthOff := s.cfg.GrowthHours, s.cfg.GrowthDisabled
	travelOff, activityOff := s.cfg.TravelDisabled, s.cfg.ActivityDisabled
	s.schedMu.Unlock()

	var slots []slot
	if !checkinOff {
		slots = append(slots, slot{nextFire(now, checkinHours), taskCheckin})
	}
	if !travelOff {
		slots = append(slots, slot{nextFire(now, travelHours), taskTravel})
	}
	if !activityOff {
		slots = append(slots, slot{nextFire(now, activityHours), taskActivity})
	}
	if !keepaliveOff {
		slots = append(slots, slot{nextFire(now, keepaliveHours), taskKeepalive})
	}
	if !blackcatOff {
		slots = append(slots, slot{nextFire(now, blackcatHours), taskBlackcat})
	}
	if !growthOff {
		slots = append(slots, slot{nextFire(now, growthHours), taskGrowth})
	}
	return slots
}

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	slots := s.enabledSlots(now)
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// wakeupGraceDelay 迟到唤醒补跑的派发前网络宽限：Windows Modern Standby exit 后
// 网络栈/DNS 1-2s 才恢复（issue #152 实测 dial tcp lookup no such host 与
// Kernel-Power 507 standby exit ≤1s 重合），宽限 5s 覆盖 90%+ 唤醒场景。
// 只对迟到补跑生效（准点触发零延迟），零配置。测试可缩短（与
// travelAccountDelay「测试可置 0」同口径）。
var wakeupGraceDelay = 5 * time.Second

// wakeupLateThreshold 迟到判定阈值：now 晚于槽位计划时刻超过 1s 才算迟到补跑。
// 毫秒级抖动（timer 正常触发的偏移量级）不算，避免准点触发被误宽限。
const wakeupLateThreshold = 1 * time.Second

// awaitWakeupGrace 迟到唤醒补跑派发前的网络宽限：槽位时刻已过点超过阈值
// （机器刚从睡眠唤醒）时先等满 wakeupGraceDelay 让网络栈/DNS 就绪再派发。
// 准点/阈值内抖动零延迟直接放行。ctx 取消立即返回 false（优雅停机不等宽限
// 睡满，本批放弃，下轮 nextWake 照旧从"现在"起算）。返回是否继续派发。
func awaitWakeupGrace(ctx context.Context, planned time.Time) bool {
	if late := time.Since(planned); late <= wakeupLateThreshold {
		return ctx.Err() == nil // 准点触发：零延迟放行
	}
	log.Printf("wakeup grace %s: late catch-up for slot %s", wakeupGraceDelay, planned.Format("15:04"))
	return sleepCtx(ctx, wakeupGraceDelay)
}

// wallclockCheckStep 墙钟校验段长：等待槽位时单次 timer 的最大时长，每段醒来用
// 墙钟重判是否到点。值是「时点精度」与「空闲唤醒频率」的折中——60s 段内时点
// 偏差上限 60s，对签到/保活类任务足够。
const wallclockCheckStep = time.Minute

// slotWake waitSlot 的三态结果。
type slotWake int

const (
	slotFired  slotWake = iota // 墙钟已到达计划时点：补跑本批
	slotRearm                  // 排程已变（Reconfigure）：上层重算下一次唤醒
	slotCancel                 // ctx 取消：上层优雅退出
)

// waitSlot 分段等待到 next 的**墙钟**时刻（next 由 nextFire 用 time.Date 构造、
// 不携带单调读数，time.Until 对它是纯墙钟差）。
//
// 为什么不一把 time.NewTimer(time.Until(next)) 睡到底：timer 的等待基于单调时钟，
// macOS / Windows Modern Standby 睡眠会冻结它——睡眠时长不足整个等待时，fire
// 被顺延「睡眠时长」（墙钟已过点、timer 还要继续等），时点被错过且不会立即补跑；
// 睡眠时长超过整个等待时倒是无害的（唤醒瞬间 timer 到期，awaitWakeupGrace 补跑）。
// 分段睡、每段醒来用墙钟重判，把冻结的影响限制在一段之内：睡眠结束后的第一段
// 末尾必然发现「墙钟已越过时点」并立即补跑，偏差上限 = step + 睡眠落段余量。
//
// ctx 取消 / rearmSchedule（在线改配置重排）在每段的 select 里随时返回，段长
// 不影响两者响应性。返回三态见 slotWake。
func (s *Scheduler) waitSlot(ctx context.Context, next time.Time, step time.Duration) slotWake {
	for {
		wallRemain := time.Until(next)
		if wallRemain <= 0 {
			return slotFired
		}
		d := wallRemain
		if d > step {
			d = step
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return slotCancel
		case <-s.rearmSchedule:
			timer.Stop()
			return slotRearm
		case <-timer.C:
			// 段末回到循环顶用墙钟重判：正常推进时若干段后到点；单调时钟被
			// 睡眠冻结时，墙钟大幅前进，至多一段之后即到点补跑。
		}
	}
}

// Run 主循环，阻塞直到 ctx 取消。
// Reconfigure 触发 rearmSchedule 时提前唤醒重算（新时点/开关立即生效）。
func (s *Scheduler) Run(ctx context.Context) {
	// 摊开等待读取该 ctx：优雅停机时立即放弃剩余账号，不等睡满窗口。
	s.setRunCtx(ctx)
	// 启动补跑：进程停机（部署 / 重启 / 崩溃）跨过今天的时点，迟到唤醒补跑救不了
	// ——它只管进程活着时 timer 被睡眠冻结。这里补一轮：各任务按天幂等，窗口类
	// （夜猫子）自带守卫；runstamps 保证同一天重启多次也只补一次。
	if missed := s.missedToday(time.Now()); len(missed) > 0 {
		log.Printf("启动补跑：%v（今天的时点已过）", missed)
		if !sleepCtx(ctx, wakeupGraceDelay) {
			return // 开机瞬间网络/DNS 可能未就绪（同 issue #152 的宽限口径）
		}
		s.runBatch(ctx, missed)
		s.markRan(missed, time.Now())
	}
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// 四类任务全部禁用：不空转，等重排通知（在线改配置重新启用）或退出信号。
			select {
			case <-ctx.Done():
				return
			case <-s.rearmSchedule:
				continue
			}
		}
		switch s.waitSlot(ctx, next, wallclockCheckStep) {
		case slotCancel:
			return
		case slotRearm:
			continue // 排程已变：重算下一次唤醒
		case slotFired:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			// 迟到唤醒（睡眠跨过槽位时刻，timer 在唤醒瞬间才到期）先等网络宽限：
			// 唤醒瞬间 DNS 未就绪，零宽限派发等于把唯一一次补跑机会打在注定失败
			// 的窗口里（issue #152）；准点触发零延迟不受影响。
			if !awaitWakeupGrace(ctx, next) {
				return // ctx 取消：放弃本批，优雅退出
			}
			// 唤醒时全部并行派发：每类一个 goroutine，慢任务族（如活跃上报
			// 多号 × 间隔 ≈ 数分钟睡眠）不再阻塞同槽其他任务族；返回前等全部
			// 任务收尾（下一轮 nextWake 照旧从"现在"起算，多轮重叠的风险与
			// 串行版相同——nextWake 只挑现在之后的时点）。
			s.runBatch(ctx, kinds)
		}
	}
}

// runBatch 并行派发一批任务（同一唤醒时刻的多类任务），等全部完成返回。
// ctx 取消时由各任务内部的可取消等待快速收尾。
func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			switch k {
			case taskCheckin:
				s.runCheckin(true)
			case taskTravel:
				s.runTravel(true)
			case taskActivity:
				s.runActivity(ctx, true)
			case taskKeepalive:
				s.runKeepalive(true)
			case taskBlackcat:
				s.RunBlackcatNow()
			case taskGrowth:
				// 成长任务队列：回调在 panel 侧异步启动（返回不等执行完），nil 未挂载则跳过。
				s.schedMu.Lock()
				hook := s.cfg.GrowthHook
				s.schedMu.Unlock()
				if hook != nil {
					hook()
				}
			}
		}(k)
	}
	wg.Wait()
}

// sleepCtx 可取消的等待：ctx 取消立即返回 false（优雅停机不必等限速睡醒），
// 等满返回 true。d<=0 立即放行。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用账号默认跳过——
// 若 schedule.include_disabled_in_tasks 打开则一并执行（禁用只关选号，不停保号）。
// 旅行已从签到剥离为独立排程（travel_hours），不再搭签到便车。
// 末尾追加连登管家（streak.go）：可兑换档位自动兑换 + 抽奖次数自动抽完——
// 连登兑换按天数解锁，挂在每日签到后即「到天数那天自动完成兑换→抽奖闭环」。
func (s *Scheduler) RunCheckinNow() {
	s.runCheckin(false)
}

// runCheckin 签到遍历实现。scheduled=true（排程触发）时按 schedule.account_spread_minutes
// 摊开；手动触发不摊开，立刻跑完。
func (s *Scheduler) runCheckin(scheduled bool) {
	plan := s.spreadPlan(taskCheckin, scheduled, time.Now())
	expiringSoon := s.ExpiringSoonWindow()
	includeDisabled := s.includeDisabledInTasks()
	for _, st := range plan.order(s.cfg.Pool.List()) {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// D4 门控：realm=global 账号无签到体系，直接跳过（不发起任何上游调用，避免风控）。
		// 经 auth.Realm() 统一判定：逃生门（global.enabled=false）下 global 账号被降级为 cn、
		// 按 CN 处理——这是逃生门的刻意语义（纯 CN 部署锁死一切 global），与引用处一致。
		if a.IsGlobal() {
			continue
		}
		// 账号级摊开：等到本号今日的偏移时刻（未启用时立即放行）；ctx 取消即放弃剩余账号。
		if !plan.wait(s.runCtx(), st.UID) {
			return
		}
		// 企业版无签到体系：上游对 POST /v2/billing/meter/daily-checkin 直接
		// 400 code 10001「企业账号不支持该操作」。跳过签到，但**不 continue**——
		// 下方余额查询照常跑（企业额度走 get-enterprise-user-usage 口径，且是
		// credit_floor 判定与面板额度展示的唯一数据源）。
		if a.SupportsPersonalEndpoints() {
			// 先查活动状态（只读）：本机 checkin_done 只记录我们自己发起的签到，用户从
			// 手机 / 官方客户端签过就看不到；上游 today_checked_in 才是权威事实。已签则
			// 直接记账跳过（省掉 21:00 那一轮注定被幂等拒绝的写调用），顺带把连登与
			// 活动截止落进日志；active=false 说明活动已结束/未开始，提前显形。
			// **查询失败 fail-open**——照常签到（签到自身的幂等错误标记仍是兜底判据）。
			label := logfmt.Label(st.UID, st.Nickname)
			act, actErr := s.cfg.Upstream.GetCheckinActivityStatus(a)
			switch {
			case actErr != nil:
				log.Printf("WARN: checkin %s: 活动状态查询失败（照常签到）: %v", label, actErr)
			case !act.Active:
				log.Printf("WARN: checkin %s: 上游签到活动当前不生效（截止 %s），仍尝试签到", label, act.EndTime)
			}
			if actErr == nil && act.TodayCheckedIn {
				s.cfg.Pool.NoteCheckinDone(st.UID)
				log.Printf("checkin %s: 上游显示今天已签到（连登 %d 天，活动至 %s）", label, act.StreakDays, act.EndTime)
			} else if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
				// "今天已签到"是幂等成功（上游对重复签到返回 code!=0），不再当失败打 error 行。
				if upstream.IsAlreadyCheckin(err) {
					s.cfg.Pool.NoteCheckinDone(st.UID)
					log.Printf("checkin %s: 今天已签到（幂等）", label)
				} else {
					log.Printf("WARN: checkin %s: %v", label, err)
				}
				// 其余业务错误也继续走余额查询
			} else {
				// 首次签到成功此前静默——排查「签到到底跑没跑」时无迹可循（幂等行只在
				// 重复触发时出现），成功也落一行。
				s.cfg.Pool.NoteCheckinDone(st.UID)
				log.Printf("checkin %s: 签到成功", label)
			}
		}
		// 分桶查余额：配置窗口内的积分单独标记，同时记录最早未来到期批次。
		remain, total, expiring, earliestAt, earliestRemaining, err := s.cfg.Upstream.UserResourceDetailedWithExpiry(a, expiringSoon)
		if err != nil {
			log.Printf("user-resource %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain, total)
		s.cfg.Pool.SetCreditsDetailed(st.UID, remain, total, expiring, earliestAt, earliestRemaining)
		s.cfg.CreditHist.Observe(st.UID, remain)
	}
	s.RunStreakBonusNow()
}

// RunActivityNow 立即对池内所有可用账号执行一次对话活跃上报。
// 禁用账号默认跳过（schedule.include_disabled_in_tasks 打开时一并上报）；
// 无 AccessToken 的跳过；账号间限速 activityAccountDelay。
// CN 与 global 账号**都上报**（PR #45 实测国际版 /v2/report 在 workbuddy.ai 上
// code=0 OK，点亮连登）；一条上报同时点亮 growth 连登 + 解锁 first_buddy 任务。
// 上报成功后续跑 streak 自检（checkActivityStreak）：回读连登天数，发现
// 「上报 200 但 streak 没涨」的静默丢弃（只读 oracle，不做重试）。
// RunActivityNow 是无 ctx 的外部入口（面板/测试一次性触发）；排程主循环走
// runActivity（ctx 取消时立即放弃剩余账号，不等限速睡满）。
func (s *Scheduler) RunActivityNow() {
	s.runActivity(context.Background(), false)
}

// runActivity 活跃上报遍历，随 ctx 取消立即退出。scheduled=true 时按账号摊开。
func (s *Scheduler) runActivity(ctx context.Context, scheduled bool) {
	plan := s.spreadPlan(taskActivity, scheduled, time.Now())
	first := true
	includeDisabled := s.includeDisabledInTasks()
	for _, st := range plan.order(s.cfg.Pool.List()) {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		// 企业版跳过活跃上报：/v2/report 本身返回 200（不报错），但它唯一的作用是
		// 点亮 growth 连登天数 / 解锁 first_buddy 领养——这两者对企业号都是 403
		// （「growth system is only available for personal users」），即**零收益**。
		// 该类"静默无效"最易被忽略（日志无异常），故显式跳过，不给上游多发一次请求。
		if !a.SupportsPersonalGrowth() {
			continue
		}
		// 账号级摊开（仅排程触发；手动触发 plan 为 nil 立即放行）。
		if !plan.wait(ctx, st.UID) {
			return
		}
		// global 账号同样上报（PR #45 实测国际版 /v2/report 在 workbuddy.ai 上 code=0 OK，
		// 点亮连登）；realmBase 路由/头由 upstream.billingJSON/BillingHeaders 按 realm 切，
		// 无需改动 upstream。此处曾按「D4 门控：global 无活跃体系」跳过 global，实测该
		// 判断不成立——国际版 /v2/report 可用，跳过即国际版账号永远点不亮连登（上游
		// a190252 同口径修复）。checkin/travel 的 global 门控不受影响，仍跳过。
		if !first {
			if !sleepCtx(ctx, activityAccountDelay) {
				return // 优雅停机：不等限速睡满，剩余账号下轮再报
			}
		}
		first = false
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		if err := s.cfg.Upstream.ReportChatActivity(a, cid, ""); err != nil {
			log.Printf("activity %s: %v", logfmt.Label(a.UID, a.Nickname), err)
			continue
		}
		s.checkActivityStreak(a) // 上报成功 → 回读 streak 自检
	}
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("activity %s: streak check failed (report OK): %v", logfmt.Label(a.UID, a.Nickname), err)
		return true
	}
	if days == 0 {
		log.Printf("activity %s: report OK but streak.days=0 (silent drop?)", logfmt.Label(a.UID, a.Nickname))
		return true
	}
	log.Printf("activity %s: streak days=%d", logfmt.Label(a.UID, a.Nickname), days)
	return false
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
// 禁用账号默认跳过；若 schedule.include_disabled_in_tasks 打开，禁用号也会续期 token
// （这是轮换用法下把闲置号保持可用的关键），但**不再对已禁用的号重复计数 12153**——
// 它已经是终态，再计一次只会打出一行「— 禁用」的误导日志。
// 12153 禁用走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不再立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
func (s *Scheduler) RunKeepaliveNow() {
	s.runKeepalive(false)
}

// runKeepalive 保活遍历实现。scheduled=true（排程触发）时按账号摊开。
func (s *Scheduler) runKeepalive(scheduled bool) {
	plan := s.spreadPlan(taskKeepalive, scheduled, time.Now())
	includeDisabled := s.includeDisabledInTasks()
	for _, st := range plan.order(s.cfg.Pool.List()) {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if !plan.wait(s.runCtx(), st.UID) {
			return
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead && !st.Disabled {
				// refresh token 失效 ≠ 账号不可用（issue #83）：AT 未过期时上游 chat
				// 仍正常。这种号计死会在连续 3 轮保活后永久禁用一个完全健康的账号
				//（面板一片橙色「疑似登录失效」，对话却秒回）。
				if upstream.SessionDeadButUsable(a) {
					log.Printf("keepalive %s: refresh 判死但 AT 仍有效（会话仍可用），不计 session dead",
						logfmt.Label(st.UID, st.Nickname))
					continue
				}
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("keepalive %s: 连续 %d 次 12153 session dead — 禁用", logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // 刷新成功清误判计数，失败不该累计
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
		}
	}
}

// RunBalanceRefreshNow 并发查询余额并更新池内 credits。
// 账号范围：默认跳过禁用账号；schedule.include_disabled_in_tasks 打开时一并刷新
// （轮换用法下据此判断下一个该启用谁）。注意 ReenableIfCredits 对 disabled 是
// no-op，所以本开关**不会**导致禁用号被自动解冻——它只让已有账号的积分保持新鲜。
// 解冻语义与签到一致（ReenableIfCredits：余额 > 0 的冷却账号自动解冻），
// 但不做签到、不刷新 token——只让"积分"这个观测量保持新鲜。
// 供两类入口复用：后台周期任务（StartBalanceRefresh）与面板手动全量刷新。
func (s *Scheduler) RunBalanceRefreshNow() {
	var wg sync.WaitGroup
	expiringSoon := s.ExpiringSoonWindow()
	includeDisabled := s.includeDisabledInTasks()
	// 账号级起跑错峰（人味化，humanize.go）：并发齐射改为窗口内逐号错开，消除
	// 「同一秒全池同动作」的时间指纹。窗口量级 = frac×interval（周期抖动关闭时为 0
	// = 旧行为全并发）；偏移逐号、逐轮独立。手动全量刷新也走这里。
	stagger := spreadIntervalStagger(time.Duration(s.balanceInterval.Load()), s.periodicJitterFor(false))
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth, uid string, wait time.Duration) {
			defer wg.Done()
			if wait > 0 && !sleepCtx(context.Background(), wait) {
				return
			}
			remain, total, expiring, earliestAt, earliestRemaining, err := s.cfg.Upstream.UserResourceDetailedWithExpiry(a, expiringSoon)
			if err != nil {
				log.Printf("balance %s: %v", logfmt.Label(uid, a.Nickname), err)
				s.noteBalanceAuthFailure(a, uid, err)
				return
			}
			s.cfg.Pool.ReenableIfCredits(uid, remain, total)
			s.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring, earliestAt, earliestRemaining)
			s.cfg.CreditHist.Observe(uid, remain)
		}(a, st.UID, spreadStaggerDelay(stagger, st.UID))
	}
	wg.Wait()
}

// noteBalanceAuthFailure 余额探测拿到 401 时的「登录态求证 + 计罚」。
//
// 为什么需要它（issue #215 复盘）：余额刷新是池内唯一的分钟级健康观测量，但此前
// 拿到错误只写一行日志（RunBalanceRefreshNow 的 err 分支），从不触碰池状态。上游
// 批量注销会话那天，号在 billing/chat 两侧都已 401，面板却继续显示「可用」，要等
// 每日 22:00 的保活连续失败 3 次（≈3 天）才转 disabled——期间运维看不到任何信号。
//
// 为什么不能把 401 直接当 ErrSessionDead 计罚：401 有两种成因，
//  1. 会话被上游注销（Keycloak session 没了，refresh 回 12153）→ 需重登，应计罚；
//  2. 渠道换回「降级 token」（issue #215 评论实测：billing/growth 恒 401，chat 仍可用）
//     → 号还能干活，计罚会误杀。
//
// 用 refresh 做一次权威求证即可分开：refresh 成功 = 会话还活着（降级 token 场景，
// 顺手换成新 token 并清计数，下一轮探测复核）；refresh 返回 12153 = 会话真死，交给
// NoteSessionDead 的连续计数（3 次 × 5 分钟 ≈ 15 分钟，远快于每日保活）。
//
// 只对 401 生效：其余错误（5xx/限流/传输层）各有既有惩罚路径，不在此重复计罚
// （两机制并存取更长者，不叠加——同 degrade.go 的纪律）。
func (s *Scheduler) noteBalanceAuthFailure(a *auth.Auth, uid string, probeErr error) {
	var ue *upstream.Error
	if !errors.As(probeErr, &ue) || ue.Status != http.StatusUnauthorized {
		return
	}
	// 没有 RT 就无法求证（静态 key / 企业号等形态）：保持「只记日志」的旧语义。
	if a.RefreshTokenValue() == "" {
		return
	}
	label := logfmt.Label(uid, a.Nickname)
	if !s.allowAuthHeal(uid) {
		// 退避窗口内已求证过：仍 401 说明是降级 token（refresh 换不回可用凭据），
		// 本轮不刷新，避免每 5 分钟轮换一次 RT。
		log.Printf("balance %s: 401（%s 内已求证过登录态，本轮跳过 refresh）", label, authHealBackoff)
		return
	}
	rerr := s.cfg.Upstream.RefreshToken(a)
	if rerr == nil {
		s.noteAuthHeal(uid)
		if serr := a.SaveAtomic(); serr != nil {
			log.Printf("balance %s: 401 后刷新成功但落盘失败: %v", label, serr)
		}
		s.cfg.Pool.ClearSessionDead(uid)
		log.Printf("balance %s: 401 经 refresh 求证为降级/漂移 token（会话仍活），已换新 token 并清 session dead 计数", label)
		return
	}
	var re *upstream.Error
	if errors.As(rerr, &re) && re.Kind == upstream.ErrSessionDead {
		// refresh token 失效 ≠ 账号不可用（issue #83）：AT 未过期时上游 chat / billing
		// 仍可能正常。这种号计死会在 3 次探测后永久禁用一个健康账号。
		if upstream.SessionDeadButUsable(a) {
			log.Printf("balance %s: refresh 判死但 AT 仍有效（会话仍可用），不计 session dead", label)
			return
		}
		if s.cfg.Pool.NoteSessionDead(uid) {
			log.Printf("balance %s: 连续 %d 次 401 且 refresh 判死 — 禁用（需重新登录）", label, pool.SessionDeadThreshold())
			return
		}
		if st, ok := s.cfg.Pool.Status(uid); ok {
			log.Printf("balance %s: 401 且 refresh 判死（session dead 计数 %d/%d）", label, st.SessionDeadFails, pool.SessionDeadThreshold())
		}
		return
	}
	log.Printf("balance %s: 401 但 refresh 失败（非 session dead，不计入禁用）: %v", label, rerr)
}

// StartBalanceRefresh 后台周期性余额刷新（独立 ticker goroutine，ctx 取消即停）。
// interval<=0 不启动（schedule.balance_refresh_enabled=false 时 main 不调用即可）。
// 独立于 Run 的小时制排程：余额是分钟级观测量，不值得为它扩展 nextFire 的粒度。
// 运行期可用 SetBalanceInterval 热改间隔（下一轮生效）。
func (s *Scheduler) StartBalanceRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.balanceInterval.Store(int64(interval))
	go func() {
		var logged time.Duration
		for {
			cur := time.Duration(s.balanceInterval.Load())
			if cur != logged {
				log.Printf("scheduler: 余额后台刷新每 %s（暂停中显示 0s）", cur)
				logged = cur
			}
			if cur <= 0 {
				// 被热改暂停：等重排通知（重新启用时唤醒）或退出。
				select {
				case <-ctx.Done():
					return
				case <-s.rearmBalance:
					continue
				}
			}
			// 人味化抖动：间隔在 ±frac×interval 内偏移（每轮重取，进程种子稳定），
			// 不再精确固定间隔打卡。基准间隔不变（热改/暂停语义不受影响）。
			delay := cur + periodicJitter(cur, s.periodicJitterFor(false), "bal")
			if delay < 0 {
				delay = 0
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmBalance:
				timer.Stop() // 间隔已变：立刻按新值重算
			case <-timer.C:
				s.RunBalanceRefreshNow()
			}
		}
	}()
}

// SetBalanceInterval 热改余额刷新间隔；<=0 表示暂停循环（面板关闭该开关时）。
func (s *Scheduler) SetBalanceInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.balanceInterval.Store(int64(d))
	poke(s.rearmBalance)
}

// StartTaskSweep 后台周期性任务补领清扫（独立 ticker goroutine，ctx 取消即停）。
// 与 StartBalanceRefresh 同款结构；interval<=0 不启动（面板关闭开关时 main 不
// 调用即可），运行期可用 SetTaskSweepInterval 热改间隔（下一轮生效）。
//
// 与成长队列（GrowthHook，小时制）的区别：队列执行的是**行为链路**（真实对话、
// 事件上报）成本高；清扫只做 accept/claim 两条幂等读改请求，分钟级跑也几乎零
// 负担，却能补上「达标的任务没人领奖」这条长期缺失的链路（issue #174）。
func (s *Scheduler) StartTaskSweep(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.sweepInterval.Store(int64(interval))
	go func() {
		// 启动即跑一轮：升级/重启后立刻补领，不必等满一个间隔。清扫幂等
		// （accept/claim 重复调用无副作用），首轮与后续 ticker 共用同一 hook。
		s.runTaskSweepHook()
		var logged time.Duration
		for {
			cur := time.Duration(s.sweepInterval.Load())
			if cur != logged {
				log.Printf("scheduler: 任务补领清扫每 %s（暂停中显示 0s）", cur)
				logged = cur
			}
			if cur <= 0 {
				// 被热改暂停：等重排通知（重新启用时唤醒）或退出。
				select {
				case <-ctx.Done():
					return
				case <-s.rearmSweep:
					continue
				}
			}
			// 人味化抖动：间隔在 ±frac×interval 内偏移（每轮重取），不再精确固定
			// 间隔打卡。基准间隔不变（热改/暂停语义不受影响）。见 humanize.go。
			delay := cur + periodicJitter(cur, s.periodicJitterFor(true), "sweep")
			if delay < 0 {
				delay = 0
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmSweep:
				timer.Stop() // 间隔已变：立刻按新值重算
			case <-timer.C:
				s.runTaskSweepHook()
			}
		}
	}()
}

// RunTaskSweepNow 立刻跑一轮任务补领清扫（手动触发/测试用）。
func (s *Scheduler) RunTaskSweepNow() {
	s.runTaskSweepHook()
}

// runTaskSweepHook 取当前 hook 并执行；hook 为 nil（panel 尚未接线）时跳过。
// 启动首轮、ticker 每轮与手动触发共用同一入口，避免三处各写一遍加锁逻辑。
func (s *Scheduler) runTaskSweepHook() {
	s.schedMu.Lock()
	hook := s.cfg.TaskSweepHook
	s.schedMu.Unlock()
	if hook != nil {
		hook()
	}
}

// SetTaskSweepHook 挂载/替换任务补领清扫回调（panel 构造晚于 scheduler，事后接线）。
func (s *Scheduler) SetTaskSweepHook(fn func()) {
	s.schedMu.Lock()
	s.cfg.TaskSweepHook = fn
	s.schedMu.Unlock()
}

// SetTaskSweepInterval 热改任务补领清扫间隔；<=0 表示暂停循环（面板关闭该开关时）。
func (s *Scheduler) SetTaskSweepInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.sweepInterval.Store(int64(d))
	poke(s.rearmSweep)
}

// SetJitter 热改周期抖动比例（千分比；<=0 关闭）。balance 与 sweep 各自独立。
func (s *Scheduler) SetJitter(balancePermill, sweepPermill int) {
	if balancePermill < 0 {
		balancePermill = 0
	}
	if sweepPermill < 0 {
		sweepPermill = 0
	}
	s.balanceJitter.Store(int64(balancePermill))
	s.sweepJitter.Store(int64(sweepPermill))
}

// SetNightAccountCap 热改夜间参与上限；<0 视为 0（不限）。
func (s *Scheduler) SetNightAccountCap(n int) {
	if n < 0 {
		n = 0
	}
	s.nightAccountCap.Store(int64(n))
}

// NightAccountCap 当前夜间参与上限。
func (s *Scheduler) NightAccountCap() int { return int(s.nightAccountCap.Load()) }

// capByCredits 夜间任务候选收窄：按积分降序取前 cap 个（余额未知/相同的顺序不稳定，
// 但候选集每天基本不变，无需额外确定性）。cap<=0 或候选不超上限时原样返回。
func capByCredits(list []pool.Status, cap int) []pool.Status {
	if cap <= 0 || len(list) <= cap {
		return list
	}
	sorted := append([]pool.Status(nil), list...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].Credits > sorted[j-1].Credits; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	return sorted[:cap]
}

// periodicJitterFor 当前生效的周期抖动（千分比 → 比例）。
func (s *Scheduler) periodicJitterFor(sweep bool) float64 {
	if sweep {
		return float64(s.sweepJitter.Load()) / 1000
	}
	return float64(s.balanceJitter.Load()) / 1000
}
