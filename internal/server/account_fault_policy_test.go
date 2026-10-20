package server

import (
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// issue #197 / PR #198 回归：上游对「渠道校验失败」返回 400 + code 11-128 /
// msg "request illegal"，其 displayTips 明示这是可重试的请求级拦截，不是账号
// 封禁。历史实现在 ErrAccountFault 分支直接 Disable（永久踢出池子、需人工解冻），
// 把仍可用的账号误杀。此测试钉住：命中该文案时只做账号级软冷却（可自动恢复、
// 可轮换），绝不置 Disabled。
func TestApplyErrorPolicyRequestIllegalCooldownsNotDisables(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	body := `{"code":"11-128","msg":"request illegal","displayMsg":{"zh":"请求被拦截，请重新发送"},"displayTips":{"zh":"渠道校验失败，可能不是从官方客户端发出的，请用官方客户端重新发送"}}`
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body, "", nil)

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号不在池中")
	}
	if st.Disabled {
		t.Fatalf("request illegal 不应永久禁用账号（issue #197 回归）: %+v", st)
	}
	if !st.Cooling {
		t.Fatalf("request illegal 应进入软冷却并轮换: %+v", st)
	}
	if st.CoolKind != "soft_rate" {
		t.Fatalf("cool_kind=%q, want soft_rate", st.CoolKind)
	}
	if st.CoolRemaining <= 0 {
		t.Fatalf("cool_remaining_sec=%d, want > 0", st.CoolRemaining)
	}
}

// 同一文案 "request illegal" 下，11140（账号级授权风控，需申诉）与 11-128
// （渠道校验拦截，请求级可重试）必须分开处置：前者硬禁用并提示申诉，后者软冷却。
// 把两者合并成「都软冷却」会让 11140 的号每轮冷却到期后被反复选中试错；
// 合并成「都禁用」则会误杀仍可用的 11-128 账号（issue #197）。
func TestApplyErrorPolicyRequestIllegalSplitsByCode(t *testing.T) {
	body11140 := `{"code":11140,"msg":"request illegal"}`
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body11140, "", nil)

	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("11140 是账号级授权风控，应禁用并提示申诉: %+v", st)
	}
	if !strings.Contains(st.DisabledReason, "appeal") {
		t.Errorf("disabled_reason=%q 应提示去官方申诉", st.DisabledReason)
	}

	// 嵌套信封（error.data.code）同样要识别。
	p2 := pool.New("")
	p2.Add(&auth.Auth{UID: "u2"})
	h2 := NewHandler(Config{Pool: p2, SoftCooldown: 600 * time.Second})
	h2.applyErrorPolicy("u2", upstream.ErrAccountFault,
		`{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, "", nil)
	if st2, _ := p2.Status("u2"); !st2.Disabled {
		t.Fatalf("嵌套信封里的 11140 也应禁用: %+v", st2)
	}
}

// 信封里没有可辨识 code 时宁可软冷却也不误杀账号。
func TestApplyErrorPolicyRequestIllegalUnknownCodeStaysSoft(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, `{"msg":"request illegal"}`, "", nil)

	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("无可辨识 code 时不得禁用账号: %+v", st)
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("应软冷却: %+v", st)
	}
}

// 同一分支的 14017（trial not activated）仍走软冷却——补完 register 后自愈，
// 禁用会让用户补完后仍无法使用。此测试防止后续改动把两条子分支合并成 Disable。
func TestApplyErrorPolicyTrialNotActivatedStaysSoft(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	h.applyErrorPolicy("u1", upstream.ErrAccountFault, `{"code":14017,"msg":"trial not activated"}`, "", nil)

	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("14017 不应永久禁用账号: %+v", st)
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("14017 应为 soft_rate 冷却: %+v", st)
	}
}
