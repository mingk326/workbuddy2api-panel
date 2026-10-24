package upstream

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreamHintModelEcho WithModelEcho 把透传帧的 model 覆写回客户端原发名。
func TestStreamHintModelEcho(t *testing.T) {
	raw := `data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil, WithModelEcho("claude-opus-4")); err != nil {
		t.Fatal(err)
	}
	s := rec.Body.String()
	if !strings.Contains(s, `"model":"claude-opus-4"`) {
		t.Fatalf("model 未回显: %s", s)
	}
	if strings.Contains(s, `"model":"glm-5.2"`) {
		t.Fatalf("真实模型名不应外泄: %s", s)
	}
	// 空 echo = 零改写。
	rec2 := httptest.NewRecorder()
	if err := StreamHint(rec2, strings.NewReader(raw), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec2.Body.String(), `"model":"glm-5.2"`) {
		t.Fatalf("未接线时应保持原文: %s", rec2.Body.String())
	}
}
