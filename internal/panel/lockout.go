package panel

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// authLockDuration 面板鉴权失败后的锁定时长。
//
// 口径：**输错一次即锁**（不是累计 N 次才锁）。面板密钥是长随机串，正常用户
// 不会反复输错；一次就锁能彻底封死在线爆破，代价是手滑要等一小时——可接受。
const authLockDuration = time.Hour

// authLockMaxEntries 锁定表条目上限。失败 IP 来自可伪造的请求头，若不设上限，
// 攻击者可以轮换地址让 map 无限增长（内存放大）；到达上限先清理过期项，
// 仍然满则淘汰任意一条——误伤一把锁好过整块内存被吃光。
const authLockMaxEntries = 4096

// authLockout 按客户端 IP 记录面板鉴权失败的锁定状态。
//
// 锁定期间即使携带正确密钥也拒绝：否则爆破者仍有每小时内的无限次试探机会，
// 锁定就失去意义。状态仅存内存（进程重启即清空）——面板是常驻进程，
// 且失败一次即锁，条目数受真实失败过的 IP 数约束，无需落盘与淘汰。
type authLockout struct {
	lockFor time.Duration
	now     func() time.Time // 测试注入；生产恒为 time.Now

	mu    sync.Mutex
	until map[string]time.Time
}

func newAuthLockout(lockFor time.Duration) *authLockout {
	return &authLockout{
		lockFor: lockFor,
		now:     time.Now,
		until:   map[string]time.Time{},
	}
}

// locked 返回该 IP 的剩余锁定时间；未锁定（或已到期）返回 ok=false。
func (l *authLockout) locked(ip string) (time.Duration, bool) {
	if l == nil || ip == "" {
		return 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	until, ok := l.until[ip]
	if !ok {
		return 0, false
	}
	now := l.now()
	if !now.Before(until) {
		delete(l.until, ip)
		return 0, false
	}
	return until.Sub(now), true
}

// fail 记录一次鉴权失败：从此刻起锁定 lockFor。
func (l *authLockout) fail(ip string) {
	if l == nil || ip == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.until) >= authLockMaxEntries {
		for k, until := range l.until {
			if !now.Before(until) {
				delete(l.until, k)
			}
		}
		for k := range l.until {
			if len(l.until) < authLockMaxEntries {
				break
			}
			delete(l.until, k)
		}
	}
	l.until[ip] = now.Add(l.lockFor)
}

// clientIPForLock 提取客户端 IP：CF-Connecting-IP → X-Real-IP →
// X-Forwarded-For 首段 → TCP 对端。
//
// 与请求记录（X-Forwarded-For 首段优先）的差别：锁定是安全边界，优先取
// Cloudflare 隧道写入的 CF-Connecting-IP（客户端无法用自带 XFF 覆盖它），
// 其次才是反代常用的 X-Real-IP / XFF。必须说明：这些都是客户端可伪造的
// 请求头，直连暴露时换头即可绕过锁定——面板的正确部署方式是只监听私网
// 或置于反代/隧道之后，锁定是纵深防御，不是唯一屏障。
func clientIPForLock(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); v != "" {
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		if v != "" {
			return v
		}
	}
	remote := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return host
	}
	return remote
}
