package server

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// seg1ThinkingOnly 预算被思考吃光：只有 reasoning、finish=length、无正文/工具。
const seg1ThinkingOnly = `data: {"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"想很久"}}]}` + "\n\n" +
	`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18}}` + "\n\n" +
	"data: [DONE]\n\n"

// seg2StarvedOK 救援段：关思考后正文正常输出并自然收尾。
const seg2StarvedOK = `data: {"id":"c2","choices":[{"index":0,"delta":{"content":"正文来了"}}]}` + "\n\n" +
	`data: {"id":"c2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}` + "\n\n" +
	"data: [DONE]\n\n"

// TestStarvedRetryBody 救援请求体：关思考、删档位、保留 max_tokens 与消息。
func TestStarvedRetryBody(t *testing.T) {
	body := []byte(`{"model":"cn:deepseek-v4.1-flash","max_tokens":128,"reasoning_effort":"high",
		"messages":[{"role":"user","content":"hi"}]}`)
	out, ok := starvedRetryBody(body)
	if !ok {
		t.Fatal("deepseek + 显式限额应可救援")
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	th, _ := obj["thinking"].(map[string]any)
	if th["type"] != "disabled" {
		t.Fatalf("thinking=%v want disabled", obj["thinking"])
	}
	if _, has := obj["reasoning_effort"]; has {
		t.Errorf("关思考后不应保留 reasoning_effort: %s", out)
	}
	if obj["max_tokens"] != float64(128) {
		t.Errorf("max_tokens=%v want 128（不放大客户端额度）", obj["max_tokens"])
	}
	if len(obj["messages"].([]any)) != 1 {
		t.Errorf("messages 应原样保留: %s", out)
	}

	// 非 DeepSeek：不知道拿什么关思考，不猜。
	if _, ok := starvedRetryBody([]byte(`{"model":"glm-5.3","max_tokens":128}`)); ok {
		t.Error("glm 模型不应救援")
	}
	// 探活级额度：不值得为它多发一次上游请求。
	if _, ok := starvedRetryBody([]byte(`{"model":"deepseek-v4.1-flash","max_tokens":1}`)); ok {
		t.Error("max_tokens=1 不应救援")
	}
	// 已经关着思考：无动作。
	if _, ok := starvedRetryBody([]byte(`{"model":"deepseek-v4.1-flash","max_tokens":128,"thinking":{"type":"disabled"}}`)); ok {
		t.Error("已 disabled 不应重复救援")
	}
}

// TestContinueReaderStarvedRetry 思考吃光预算 → 同额度关思考重发，客户端拿到正文。
func TestContinueReaderStarvedRetry(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, seg2StarvedOK })
	body := `{"model":"cn:deepseek-v4.1-flash","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`
	r := newTestContinueReader(t, up, body, io.NopCloser(strings.NewReader(seg1ThinkingOnly)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("救援请求数=%d want 1", len(*reqs))
	}
	req := (*reqs)[0]
	if !strings.Contains(req, `"thinking":{"type":"disabled"}`) {
		t.Fatalf("救援请求未关思考:\n%s", req)
	}
	if strings.Contains(req, "reasoning_effort") {
		t.Fatalf("救援请求不应带档位:\n%s", req)
	}
	if !strings.Contains(req, `"max_tokens":128`) {
		t.Fatalf("救援请求不应放大额度:\n%s", req)
	}
	s := string(out)
	if !strings.Contains(s, "正文来了") {
		t.Fatalf("客户端应收到救援段正文:\n%s", s)
	}
	if strings.Contains(s, `"finish_reason":"length"`) {
		t.Fatalf("救援成功后 length 终态不应外泄:\n%s", s)
	}
}

// TestContinueReaderStarvedOnlyOnce 救援失败后降级：不无限重发。
func TestContinueReaderStarvedOnlyOnce(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 500, "boom" })
	body := `{"model":"deepseek-v4.1-flash","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`
	r := newTestContinueReader(t, up, body, io.NopCloser(strings.NewReader(seg1ThinkingOnly)))
	_, _ = io.ReadAll(r)
	if len(*reqs) != 1 {
		t.Fatalf("救援失败也不应重试第二次: reqs=%d", len(*reqs))
	}
	if !r.starvedTried {
		t.Error("starvedTried 应置位")
	}
}
