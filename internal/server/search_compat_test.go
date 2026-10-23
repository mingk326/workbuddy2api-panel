package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDeepSeekSearchClient UA 判定只认 DSH，其余客户端保持原文透传。
func TestDeepSeekSearchClient(t *testing.T) {
	if !deepSeekSearchClient("deepseek-harness/0.2.1-rc.2 (+https://github.com/deepseek-ai/deepseek-harness)") {
		t.Error("DSH UA 应命中")
	}
	if !deepSeekSearchClient("DeepSeek-Harness/1.0") {
		t.Error("大小写不敏感")
	}
	for _, ua := range []string{"", "curl/8.0", "OpenAI/Python 1.30.0", "WorkBuddy/5.8.3"} {
		if deepSeekSearchClient(ua) {
			t.Errorf("非 DSH UA 不应命中: %q", ua)
		}
	}
}

// TestCompatDeepSeekSearch 补齐 date:null 与 title/snippet 字符串，其余字段不动。
func TestCompatDeepSeekSearch(t *testing.T) {
	raw := `{"query":"q","results":[` +
		`{"title":"A","url":"https://a","snippet":"sa"},` +
		`{"title":null,"url":"https://b","snippet":null},` +
		`{"title":"C","url":"https://c","snippet":"sc","date":"2026-01-01"}` +
		`],"total_results":3}`
	out := compatDeepSeekSearch([]byte(raw))

	var obj struct {
		Query        string `json:"query"`
		TotalResults int    `json:"total_results"`
		Results      []struct {
			Title   *string `json:"title"`
			URL     string  `json:"url"`
			Snippet *string `json:"snippet"`
			Date    *string `json:"date"`
			HasDate bool
		} `json:"results"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, out)
	}
	if obj.Query != "q" || obj.TotalResults != 3 {
		t.Errorf("其他字段被改动: %+v", obj)
	}
	if len(obj.Results) != 3 {
		t.Fatalf("results len=%d", len(obj.Results))
	}
	// 第一项：缺 date → 必须显式出现且为 null。
	var m map[string]json.RawMessage
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(m["results"], &items)
	if string(items[0]["date"]) != "null" {
		t.Errorf("缺失 date 应补 null，实际 %q", items[0]["date"])
	}
	// 第二项：title/snippet 为 null → 空串。
	if string(items[1]["title"]) != `""` || string(items[1]["snippet"]) != `""` {
		t.Errorf("null title/snippet 应补空串: %s", items[1])
	}
	// 第三项：已有 date 原样保留。
	if string(items[2]["date"]) != `"2026-01-01"` {
		t.Errorf("已有 date 不应改写: %q", items[2]["date"])
	}

	// 畸形 / 无 results：原样返回（不做二次包装）。
	for _, bad := range []string{`{`, `{"query":"q"}`, `{"results":"x"}`} {
		if got := string(compatDeepSeekSearch([]byte(bad))); got != bad {
			t.Errorf("畸形输入应原样返回: in=%q out=%q", bad, got)
		}
	}
}

// TestCompatDeepSeekSearchNoResults 空结果集不补字段、不报错。
func TestCompatDeepSeekSearchNoResults(t *testing.T) {
	raw := `{"query":"q","results":[]}`
	if got := string(compatDeepSeekSearch([]byte(raw))); got != raw {
		t.Errorf("空 results 应原样返回: %s", got)
	}
	if !strings.Contains(string(compatDeepSeekSearch([]byte(raw))), "results") {
		t.Error("输出丢失 results")
	}
}
