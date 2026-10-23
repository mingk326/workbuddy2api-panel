// Package httpauth 网关与面板共用的 Bearer 鉴权原语。
//
// 单独成包的原因：server（/v1/*、/status）与 panel（/panel/api/*）两处鉴权
// 必须完全同口径——此前各自复制了一份"字符串直接比较"的实现，既容易漂移，
// 又都带计时侧信道。统一到这里后，口径只有一份，且天然常量时间比较。
package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// bearerPrefix 认证方案前缀（大小写敏感，与 HTTP 规范及既有实现一致）。
const bearerPrefix = "Bearer "

// Key 一把 API 密钥及其身份（多密钥体系）。
//
// 除密钥本体外还承载两类身份信息：
//   - Admin 是否可访问管理面板（/panel/api/*）；网关 /v1/* 不限。
//   - DefaultEffort / PerModel 该密钥专属的默认思考档注入表（可为空）。
//
// 零值 Key 表示"未启用鉴权"（密钥表为空时的放行身份）。
type Key struct {
	ID            string
	Secret        string
	Name          string
	Admin         bool
	DefaultEffort string
	PerModel      map[string]string
	// AllowedModels 可用模型白名单（见 models.go）：空 = 不限制；
	// 裸名对所有域生效，带 cn:/global: 前缀只限定该域。
	AllowedModels []string
	// DailyTokenLimit / DailyRequestLimit 密钥级每日配额（CST 自然日）；
	// 0 = 不限制。由 server 的 keyquota 统计器执行。
	DailyTokenLimit   int64
	DailyRequestLimit int64
	// RPMLimit 每分钟请求数上限（滑动 60 秒窗口，含出图）；0 = 不限制。
	RPMLimit int
	// MaxConcurrency 同时在途请求上限（流式到最后一帧才算结束，含出图）；
	// 0 = 不限制。
	MaxConcurrency int
}

// Authenticate 在密钥表中查找请求携带的 Bearer 密钥，返回命中的身份。
//
// 语义与 VerifyBearer 对齐：keys 为空 = 未启用鉴权，返回零值 Key 且 ok=true。
// 比较仍是 SHA-256 摘要 + subtle.ConstantTimeCompare，且**遍历全表不提前返回**：
// 命中位置不同会改变循环次数，提前 break 等于把"第几把密钥"通过耗时泄露出去。
func Authenticate(r *http.Request, keys []Key) (Key, bool) {
	if len(keys) == 0 {
		return Key{}, true
	}
	tok := bearerToken(r)
	var hit Key
	matched := 0
	for _, k := range keys {
		if subtle.ConstantTimeCompare(digest(tok), digest(k.Secret)) == 1 {
			hit = k
			matched = 1
		}
	}
	if matched == 0 {
		return Key{}, false
	}
	return hit, true
}

// bearerToken 取 Authorization 头里的 Bearer 令牌；缺头/方案不符返回空串。
func bearerToken(r *http.Request) string {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		return ""
	}
	return authz[len(bearerPrefix):]
}

// VerifyBearer 校验请求头是否携带正确的 Bearer 密钥。
//
// key 为空表示"未启用鉴权"，恒返回 true（调用方据此放行）。
// 比较用 SHA-256 摘要 + subtle.ConstantTimeCompare：
//   - 常量时间，不因前缀匹配长度而泄露信息；
//   - 先摘要再比较，长度差异被吸收进摘要（不会因长度不同提前返回）；
//   - 摘要本身不可逆，即便有侧信道也拿不到密钥原文。
func VerifyBearer(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	// 缺头/方案不对时 bearerToken 返回空串，仍走一次摘要比较，保持耗时形状一致。
	return subtle.ConstantTimeCompare(digest(bearerToken(r)), digest(key)) == 1
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
