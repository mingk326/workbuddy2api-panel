package upstream

import (
	"strings"
	"testing"
)

// TestRequestedEffort 客户端原始档位读取：snake 优先、camel 兜底、未携带为空。
func TestRequestedEffort(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"snake", `{"model":"m","reasoning_effort":"max"}`, "max"},
		{"camel", `{"model":"m","reasoningEffort":"HIGH"}`, "high"},
		{"both (snake wins)", `{"reasoning_effort":"low","reasoningEffort":"max"}`, "low"},
		{"missing", `{"model":"m"}`, ""},
		{"empty", `{"reasoning_effort":""}`, ""},
		{"non-string", `{"reasoning_effort":3}`, ""},
		{"invalid json", `{`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RequestedEffort([]byte(c.body)); got != c.want {
				t.Errorf("RequestedEffort(%s)=%q want %q", c.body, got, c.want)
			}
		})
	}
}

// TestPrepareBodyEffortTrace 档位溯源：in 在降级前、eff 在降级后；客户端未携带时
// in 为空（deepseek 补的默认档只体现在 eff）。
func TestPrepareBodyEffortTrace(t *testing.T) {
	efforts := map[string][]string{"ds-model": {"low", "high"}}

	// 客户端显式 max，模型只支持 low/high → in=max、eff=high。
	out, in, eff := PrepareBodyWithEffortTrace(
		[]byte(`{"model":"ds-model","reasoning_effort":"max","messages":[{"role":"user","content":"hi"}]}`),
		false, efforts, nil)
	if in != "max" || eff != "high" {
		t.Fatalf("trace=(%q,%q) want (max,high)", in, eff)
	}
	if !strings.Contains(string(out), `"reasoning_effort":"high"`) {
		t.Errorf("出站应为降级后的 high: %s", out)
	}

	// 客户端未携带 + 非 deepseek 模型 → 两段皆空（不编造档位）。
	_, in, eff = PrepareBodyWithEffortTrace(
		[]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
		false, nil, nil)
	if in != "" || eff != "" {
		t.Fatalf("未携带档位 trace=(%q,%q) want 空", in, eff)
	}

	// deepseek 未携带：网关补默认档 high → in 空（不是客户端发的）、eff=high。
	_, in, eff = PrepareBodyWithEffortTrace(
		[]byte(`{"model":"deepseek-v4","messages":[{"role":"user","content":"hi"}]}`),
		false, map[string][]string{"deepseek-v4": {"low", "high", "max"}}, nil)
	if in != "" || eff != "high" {
		t.Fatalf("deepseek 补档 trace=(%q,%q) want (\"\",high)", in, eff)
	}

	// 显式档位被模型支持 → 两段一致。
	_, in, eff = PrepareBodyWithEffortTrace(
		[]byte(`{"model":"ds-model","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`),
		false, efforts, nil)
	if in != "low" || eff != "low" {
		t.Fatalf("显式支持档 trace=(%q,%q) want (low,low)", in, eff)
	}
}
