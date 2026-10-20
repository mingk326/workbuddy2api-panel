package upstream

import (
	"encoding/json"
	"testing"
)

func TestInjectDefaultThinkingEffort(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		override map[string]string
		want     string // "" = 零改动
	}{
		{"nil 关闭零改动", `{"model":"glm-5.2","messages":[]}`, nil, ""},
		{"全局默认注入", `{"model":"glm-5.2","messages":[]}`, map[string]string{"": "high"}, `{"model":"glm-5.2","messages":[],"reasoning_effort":"high"}`},
		{"按模型覆盖", `{"model":"deepseek-v4.1-flash","messages":[]}`, map[string]string{"": "medium", "deepseek-v4.1-flash": "xhigh"}, `{"model":"deepseek-v4.1-flash","messages":[],"reasoning_effort":"xhigh"}`},
		{"全局兜底无匹配", `{"model":"glm-5.2","messages":[]}`, map[string]string{"deepseek-v4.1-flash": "high"}, ""},
		{"已有 snake 不覆盖", `{"model":"glm-5.2","reasoning_effort":"low","messages":[]}`, map[string]string{"": "high"}, ""},
		{"已有 camel 不覆盖", `{"model":"glm-5.2","reasoningEffort":"low","messages":[]}`, map[string]string{"": "high"}, ""},
		{"显式 disabled 不注入", `{"model":"glm-5.2","thinking":{"type":"disabled"},"messages":[]}`, map[string]string{"": "high"}, ""},
		{"带前缀剥前缀匹配", `{"model":"cn:glm-5.2","messages":[]}`, map[string]string{"glm-5.2": "medium"}, `{"model":"cn:glm-5.2","messages":[],"reasoning_effort":"medium"}`},
		{"非法 JSON 原样返回", `{"model":"glm-5.2",`, map[string]string{"": "high"}, ""},
		{"无 model 原样返回", `{"messages":[]}`, map[string]string{"": "high"}, ""},
		{"空档位跳过", `{"model":"glm-5.2","messages":[]}`, map[string]string{"glm-5.2": "", "": "high"}, `{"model":"glm-5.2","messages":[],"reasoning_effort":"high"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := InjectDefaultThinkingEffort([]byte(c.body), c.override)
			if c.want == "" {
				if string(out) != c.body {
					t.Fatalf("应零改动: %s -> %s", c.body, out)
				}
				return
			}
			var got, want map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("out not json: %v %s", err, out)
			}
			if err := json.Unmarshal([]byte(c.want), &want); err != nil {
				t.Fatalf("want not json: %v", err)
			}
			if got["reasoning_effort"] != want["reasoning_effort"] {
				t.Fatalf("reasoning_effort=%v want %v (out=%s)", got["reasoning_effort"], want["reasoning_effort"], out)
			}
		})
	}
}

func TestInjectDefaultThinkingEffortThenNormalizeDowngrade(t *testing.T) {
	// 注入后仍要过 normalizeReasoningEffort 降级管线：模型不支持所配档位时落到
	// ≤ 该档位的最高支持档（不出站非法档）。
	body := InjectDefaultThinkingEffort(
		[]byte(`{"model":"glm-5.2-mini","messages":[]}`),
		map[string]string{"glm-5.2-mini": "max"},
	)
	// 模型只支持 off/low/medium/high（无 max）→ 降级到 high。
	out := PrepareBodyOptWithEfforts(body, false, map[string][]string{"glm-5.2-mini": {"off", "low", "medium", "high"}})
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("out not json: %v %s", err, out)
	}
	if got, _ := obj["reasoning_effort"].(string); got != "high" {
		t.Fatalf("reasoning_effort=%q want high（注入后应降级）: %s", got, out)
	}
}
