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
	"strings"
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

	// HiddenModels 精确隐藏的模型 id（已归一化为 "cn:x"/"global:x" 全量形式）；
	// nil = 不隐藏。models.hidden 热生效：面板保存后下一请求即应用。
	HiddenModels map[string]struct{}
	// AllowedModels 模型白名单（config models.allowed，issue #68）：已归一化为
	// "cn:x"/"global:x" 全量形式的精确 id 集合；nil = 未启用（默认放行所有模型）。
	// 与 HiddenModels 完全对称：Hidden 是「可见集合再剔除」，Allowed 是「可能可见」集合。
	AllowedModels map[string]struct{}
	// AllowlistEnabled 白名单是否启用（归一化结果非空即启用）；false = 全部放行。
	AllowlistEnabled bool
	// ModelAliases 客户端模型名 → 真实模型名（键已归一化为小写 "cn:x"/"global:x"）；
	// nil = 不映射。models.aliases，issue #30。
	ModelAliases map[string]string

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
	if len(s.HiddenModels) > 0 {
		m := make(map[string]struct{}, len(s.HiddenModels))
		for k := range s.HiddenModels {
			m[k] = struct{}{}
		}
		s.HiddenModels = m
	}
	if len(s.ModelAliases) > 0 {
		m := make(map[string]string, len(s.ModelAliases))
		for k, v := range s.ModelAliases {
			m[k] = v
		}
		s.ModelAliases = m
	}
	h.p.Store(&s)
}

// NormalizeModelAliases 把 models.aliases 条目（"客户端名=真实名"）归一化为查找表。
//
//   - 按第一个 "=" 拆键值；键/值都 Trim，键小写存储（查找端同样小写）。
//   - 键带合法 realm 前缀 → 只在该域生效；裸名键同时登记 cn/global 两域。
//   - 值原样保留（可带 realm 前缀实现跨域指向）。
//   - 无 "="、空键、空值、键==值（大小写不敏感）→ 丢弃。
func NormalizeModelAliases(in []string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in)*2)
	for _, item := range in {
		k, v, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if k == "" || v == "" || strings.EqualFold(k, v) {
			continue
		}
		if prefix, bare, ok := strings.Cut(k, ":"); ok && (prefix == "cn" || prefix == "global") && bare != "" {
			out[prefix+":"+bare] = v
		} else {
			out["cn:"+k] = v
			out["global:"+k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeHiddenModels 把用户配置的 models.hidden 条目归一化为精确 id 集合。
//
// 条目带合法 "cn:"/"global:" 前缀 → 只隐藏该域；裸名 → 两域都隐藏。
// 大小写敏感（与 server.resolveModel 的前缀规则对称：取第一个 ":"，前段恰为
// "cn"/"global" 才剥离）。返回的 map 只读共享，调用方不得修改。
func NormalizeHiddenModels(hidden []string) map[string]struct{} {
	if len(hidden) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(hidden)*2)
	for _, m := range hidden {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if prefix, bare, ok := strings.Cut(m, ":"); ok && (prefix == "cn" || prefix == "global") && bare != "" {
			out[prefix+":"+bare] = struct{}{}
		} else {
			out["cn:"+m] = struct{}{}
			out["global:"+m] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeAllowedModels 把用户配置的 allowed 条目归一化为精确 id 集合。
//
// 条目语法与 NormalizeHiddenModels 完全一致：带合法 "cn:"/"global:" 前缀 →
// 只在该域允许；裸名 → 两域都允许。大小写敏感。返回 enabled 表示白名单是否
// 启用：入参为空或全部为无效条目（Trim 后为空）→ false，调用方据此走
// 「默认放行所有模型」的向后兼容路径。返回的 map 只读共享，调用方不得修改。
func NormalizeAllowedModels(in []string) (map[string]struct{}, bool) {
	if len(in) == 0 {
		return nil, false
	}
	out := make(map[string]struct{}, len(in)*2)
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if prefix, bare, ok := strings.Cut(m, ":"); ok && (prefix == "cn" || prefix == "global") {
			out[prefix+":"+bare] = struct{}{}
		} else {
			out["cn:"+m] = struct{}{}
			out["global:"+m] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
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
