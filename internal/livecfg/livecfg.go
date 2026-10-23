// Package livecfg 运行期可变配置的并发安全持有者。
//
// 背景：进程启动时读入的配置是普通字段（读多写零），但管理面板允许在线改配置，
// 于是少量"可热生效"的字段需要有并发安全的读写点。此处用不可变快照 + atomic 指针：
// 读方 Load 拿到一致视图，写方 Store 整体替换，无锁无数据竞争。
//
// 只承载**读路径深、热改需求强**的少数字段；池参数/排程参数等各有既有 setter
// （pool.SetBreaker、scheduler.Reconfigure 等），不重复收编到这里。
package livecfg

import (
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
)

// Snapshot 一次读取的不可变配置视图。
type Snapshot struct {
	// APIKey 旧版单一密钥（保留供面板展示/兼容）；鉴权一律走 APIKeys。
	APIKey string
	// APIKeys 运行期密钥表（api_keys + 旧版 api_key）。空 = 不鉴权。
	APIKeys []httpauth.Key

	SoftCooldown         time.Duration // 429 软冷却基数（<=0 时调用方回退内置默认）
	SanitizeFingerprints bool          // 出站请求体指纹脱敏
	RecordClientInfo     bool          // 请求日志是否记录调用来源（客户端 IP / UA）

	// 积分闸（0 = 关闭对应闸）。面板保存配置后下一请求生效，无需重启。
	BudgetDailyCreditLimit float64 // 当日累计 credit 上限
	BudgetBurstCreditLimit float64 // 5 分钟窗口内累计 credit 上限
	BudgetCacheLowRate     float64 // 连续低缓存率触发阈值（百分比）

	// ModelFallback 模型降级路由表（pool.model_fallback）：6004 模型级限流时
	// 改用的等价模型（裸名 → 裸名）。nil = 不降级。
	ModelFallback map[string]string
}

// Keys 返回生效密钥表：优先 APIKeys；为空时把旧版 APIKey 合成为 admin 身份。
// 这层兜底保证「只填了 APIKey、没填 APIKeys」的部分构造场景不会静默变成无鉴权。
func (s Snapshot) Keys() []httpauth.Key {
	if len(s.APIKeys) > 0 {
		return s.APIKeys
	}
	if s.APIKey == "" {
		return nil
	}
	return []httpauth.Key{{ID: "legacy", Secret: s.APIKey, Name: "api_key", Admin: true}}
}

// Holder 原子持有当前快照。
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// New 以初始快照构建。
func New(s Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)
	return h
}

// Load 返回当前快照（Holder 为 nil 或从未 Store 时返回零值快照，调用方无需判空）。
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	if s := h.p.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

// Store 整体替换快照。
//
// 密钥表做深拷贝：Store 后调用方可能继续复用/修改传入的 map（例如配置对象被
// 再次 normalize），若共享底层 PerModel，读方会在无锁状态读到并发写。
func (h *Holder) Store(s Snapshot) {
	s.APIKeys = cloneKeys(s.APIKeys)
	if len(s.ModelFallback) > 0 {
		m := make(map[string]string, len(s.ModelFallback))
		for k, v := range s.ModelFallback {
			m[k] = v
		}
		s.ModelFallback = m
	}
	h.p.Store(&s)
}

// cloneKeys 复制密钥表（含 per-model 档位 map），保证快照不可变。
func cloneKeys(in []httpauth.Key) []httpauth.Key {
	if len(in) == 0 {
		return nil
	}
	out := make([]httpauth.Key, len(in))
	for i, k := range in {
		if len(k.PerModel) > 0 {
			m := make(map[string]string, len(k.PerModel))
			for mk, mv := range k.PerModel {
				m[mk] = mv
			}
			k.PerModel = m
		}
		if len(k.AllowedModels) > 0 {
			k.AllowedModels = append([]string(nil), k.AllowedModels...)
		}
		out[i] = k
	}
	return out
}
