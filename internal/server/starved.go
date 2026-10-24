// starved.go 「思考吃光预算」的救援重发。
//
// 现象（上游实测，cn:deepseek-v4.1-flash）：上游把 reasoning 与正文字节算在
// **同一个** max_tokens 预算里，思考没有独立上限字段。客户端给一个偏小的预算时，
// 思考可以独吞整份额度——结束于 finish_reason=length、正文零字节、没有 tool_calls，
// 客户端看到的就是「空回复」。关掉 thinking 后同预算重发，正文正常输出。
//
// 救援动作是「**同预算、关思考**重发一次」：不放大客户端要的额度（超发是对客户端
// 的语义违约，也是新的可判别形态），只把预算从思考手里拿回来。只在「这一轮什么都
// 没产出」时触发——正常回复、有正文的正常截断都不碰。
//
// 与续写的分工：客户端显式限额（limitSet）时续写分支本就不接力，这一形态由本文件
// 接走；没有限额时这一形态同样优先走救援（思考-only 没有正文可续）。
package server

import (
	"bufio"
	"encoding/json"
	"log"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// minStarvedBudget 低于此额度不救援：官方客户端用 max_tokens=1 做 key 探活，
// 那种请求按原样失败即可，不值得为它多发一次上游请求。
const minStarvedBudget = 16

// starvedRetryBody 把请求体改写成救援形态：thinking 关掉、删掉思考档位，其余
// （含 max_tokens / tools / messages）原样保留。
//
// 返回 false 的情形都是「不猜」：没有 thinking 开关的模型（不知道拿什么关思考）、
// 已经关着思考的、额度小到不值得重发的、body 不可解析。
func starvedRetryBody(body []byte) ([]byte, bool) {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return nil, false
	}
	if bt := outputBudget(obj); bt > 0 && bt < minStarvedBudget {
		return nil, false
	}
	th, ok := obj["thinking"].(map[string]any)
	if !ok {
		// 客户端通常不自己带 thinking（档位/开关由上游管线注入），这里按**模型能力**
		// 判断能不能关：拿不到开关的模型不猜别的手段。
		model, _ := obj["model"].(string)
		if !upstream.ThinkingSwitchSupported(model) {
			return nil, false
		}
		th = map[string]any{}
	}
	if t, _ := th["type"].(string); strings.EqualFold(strings.TrimSpace(t), "disabled") {
		return nil, false
	}
	th["type"] = "disabled"
	obj["thinking"] = th
	// 关思考就别再带档位：上游对 disabled + effort 的组合语义未取证，
	// 而思考管线的 disabled 分支本来就要求两者不共存（见 upstream/thinking.go）。
	delete(obj, "reasoning_effort")
	delete(obj, "reasoningEffort")
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// outputBudget 读请求体里的输出限额（三个别名），未携带 → 0。
func outputBudget(obj map[string]any) int {
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		switch v := obj[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case int64:
			return int(v)
		}
	}
	return 0
}

// tryStarvedRetry 空截断救援：本段预算全被思考吃掉——正文零字节、无工具分片、
// finish_reason=length。同预算重发一次、把 thinking 关掉，让正文落进客户端要的额度里。
//
// 每请求一次，失败就按原路降级——不救援时客户端行为与接线前完全一致。
func (r *continueReader) tryStarvedRetry() bool {
	switch {
	case r.starvedTried, r.errSeen, r.toolSeen:
		return false
	case r.finish != "length":
		return false
	case r.text.Len() > 0:
		// 有正文的截断是额度语义的正常结果，不救。
		return false
	case r.seg >= maxContinueSegments-1:
		return false
	}
	body, ok := starvedRetryBody(r.body)
	if !ok {
		return false
	}
	r.starvedTried = true
	rc, status, _, err := r.up.ChatStreamContext(r.ctx, r.acct, body, r.clientIP, r.meta, nil)
	if err != nil || status >= 400 || rc == nil {
		if rc != nil {
			rc.Close()
		}
		log.Printf("WARN: [starved] acct=%s seg=%d 关思考重发失败（status=%d err=%v）→ 降级为截断终态",
			logfmt.Label(r.acct.UID, r.acct.Nickname), r.seg+1, status, err)
		return false
	}
	r.seg++
	r.cur, r.br = rc, bufio.NewReaderSize(rc, 64*1024)
	r.finish, r.tail = "", nil
	// 上一段外泄给客户端的思考（正文尚未开始，见 frame() 的接缝策略）已经发出去、
	// 收不回来；救援段关思考，把本地累积清掉，免得续写前缀里混进两轮思考。
	r.reason.Reset()
	log.Printf("[starved] acct=%s model=%s seg=%d 预算被思考吃光（正文 0B）→ 同额度关思考重发",
		logfmt.Label(r.acct.UID, r.acct.Nickname), bareModelOf(r.body), r.seg)
	return true
}
