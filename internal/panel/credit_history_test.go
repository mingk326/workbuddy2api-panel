package panel

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/credithist"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// creditHistoryPanel 造一个仅带积分账本的面板（纯内存账本，不落盘）。
func creditHistoryPanel(t *testing.T) (*Panel, *credithist.Ledger) {
	t.Helper()
	led := credithist.New("", 0)
	pn := New(Config{
		Version:    "test",
		Pool:       pool.New(""),
		CreditHist: led,
	})
	return pn, led
}

func getCreditHistory(t *testing.T, pn *Panel, query string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	pn.creditHistory(rec, httptest.NewRequest("GET", "/panel/api/credit_history"+query, nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v %s", err, rec.Body)
	}
	return rec.Code, out
}

// TestCreditHistoryBaselineAndDelta 首次观测只建基线不留痕；余额变化才追加流水。
func TestCreditHistoryBaselineAndDelta(t *testing.T) {
	pn, led := creditHistoryPanel(t)
	led.Observe("u1", 100) // 基线
	led.Observe("u1", 100) // 未变化：不记
	led.Observe("u1", 160) // +60
	led.Observe("u2", 50)  // 基线
	led.Observe("u2", 30)  // -20

	code, out := getCreditHistory(t, pn, "")
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	entries, _ := out["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("流水条数=%d want 2: %v", len(entries), out)
	}
	// 新的在前：u2 的 -20 在前，u1 的 +60 在后。
	first, _ := entries[0].(map[string]any)
	if first["uid"] != "u2" || first["delta"] != float64(-20) || first["after"] != float64(30) {
		t.Fatalf("首条流水不符: %v", first)
	}
}

// TestCreditHistoryUIDFilterAndLimit uid 过滤必须先于截断：limit=1 时仍能取到
// 指定账号的流水，而不是被其他账号的最近记录挤掉。
func TestCreditHistoryUIDFilterAndLimit(t *testing.T) {
	pn, led := creditHistoryPanel(t)
	led.Observe("a", 10)
	led.Observe("a", 20)
	led.Observe("b", 100)
	led.Observe("b", 200)
	led.Observe("b", 300) // b 的最新两条更靠后

	code, out := getCreditHistory(t, pn, "?uid=a&limit=1")
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	entries, _ := out["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("条数=%d want 1: %v", len(entries), out)
	}
	e, _ := entries[0].(map[string]any)
	if e["uid"] != "a" || e["delta"] != float64(10) {
		t.Fatalf("过滤/limit 结果不符: %v", e)
	}
	if out["limit"] != float64(1) {
		t.Fatalf("limit=%v want 1", out["limit"])
	}
}

// TestCreditHistoryEmptyAndUnavailable 空账本返回 [] 而不是 null；未注入账本返回 501。
func TestCreditHistoryEmptyAndUnavailable(t *testing.T) {
	pn, _ := creditHistoryPanel(t)
	code, out := getCreditHistory(t, pn, "")
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	if entries, ok := out["entries"].([]any); !ok || len(entries) != 0 {
		t.Fatalf("空账本 entries 应为 []: %v", out)
	}

	bare := New(Config{Version: "test", Pool: pool.New("")})
	rec := httptest.NewRecorder()
	bare.creditHistory(rec, httptest.NewRequest("GET", "/panel/api/credit_history", nil))
	if rec.Code != 501 {
		t.Fatalf("未注入账本 code=%d want 501", rec.Code)
	}
}
