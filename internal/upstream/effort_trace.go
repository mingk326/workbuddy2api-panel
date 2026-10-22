// effort_trace.go 思考档位溯源：区分「客户端原始档位 / 出站意图档位 / 最终生效档位」。
//
// 背景（对齐 misakano7545 issue #43）：流水行只报生效档位时，「客户端设了 max、日志
// 显示 high」无法判断是客户端根本没发出去，还是被网关按模型档位表降级了。三段分开
// 记录后，日志能自证：
//
//   - Client：客户端请求里的原始档位（handler 在**任何注入之前**抓取；未携带 = 空）
//   - In：出站管线降级前的档位（可能是客户端值，也可能是密钥 / 全局 thinking.* 注入值）
//   - Out：降级 / floor 之后的最终出站档位
//
// 与上游参考实现（只分 Client/Effort 两段）的差异：我们能对多密钥注入的档位做来源
// 标记（日志渲染为 gw:max->xhigh），否则客户端没发 max、网关注入 max 再被降级时，
// 日志会长得和「客户端发了 max」一模一样。
package upstream

import (
	"encoding/json"
	"strings"
)

// EffortTrace 一次 chat 请求的档位溯源（可选出参；nil 表示不采集、零开销）。
// 字段只在单次请求内由调用链写入，不跨请求复用。
type EffortTrace struct {
	Client string // 客户端原始档位（handler 注入前填入；upstream 不覆盖）
	In     string // 出站管线降级前（客户端显式或网关注入）
	Out    string // 最终出站档位（降级/floor 后）
}

// RequestedEffort 从 JSON 请求体读取思考档位（snake 优先，camel 兜底），
// 不做任何改写与默认补档。解析失败 / 未携带 / 非字符串 / 空串 → ""。
func RequestedEffort(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return ""
	}
	return effortOf(obj)
}

// effortOf 从已解析的请求体对象读档位（内部复用，口径与 RequestedEffort 一致）。
func effortOf(obj map[string]any) string {
	for _, key := range [...]string{"reasoning_effort", "reasoningEffort"} {
		if v, ok := obj[key].(string); ok {
			if s := strings.ToLower(strings.TrimSpace(v)); s != "" {
				return s
			}
		}
	}
	return ""
}
