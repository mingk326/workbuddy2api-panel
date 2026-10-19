package upstream

import (
	"testing"
	"time"
)

func TestQueueRetryAfter(t *testing.T) {
	cases := []struct {
		body string
		want time.Duration
	}{
		{`{"code":6020,"msg":"queue.waiting.title","data":{"retry_after":40}}`, 40 * time.Second},
		{`{"code":6021,"msg":"queue.full.title","data":{"retry_after":5}}`, 5 * time.Second},
		{`{"code":6020,"msg":"queue.waiting.title"}`, 0},
		{`{"code":6020,"data":{"retry_after":0}}`, 0},
		{`{"code":6020,"data":{"retry_after":9999}}`, 0},
		{`not json`, 0},
	}
	for _, c := range cases {
		if got := QueueRetryAfter(c.body); got != c.want {
			t.Errorf("QueueRetryAfter(%s)=%v want %v", c.body, got, c.want)
		}
	}
}
