package panel

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

// testKeysPanel 构建一个带临时 config.json 的面板；SaveConfig 直接落盘模拟 main 的
// 校验+热应用链路（校验逻辑本身在 cmd/server 测试里覆盖）。
func testKeysPanel(t *testing.T, initial string) (*Panel, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(Config{
		Version:    "test",
		APIKey:     "sk-legacy",
		ConfigPath: path,
		SaveConfig: func(raw []byte) ([]string, error) {
			return nil, os.WriteFile(path, raw, 0o600)
		},
	})
	return p, path
}

func doPanel(p *Panel, method, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// TestPanelOnlyAdminKey 面板只认 admin 密钥；普通密钥 401（不泄露"密钥有效但无权"）。
func TestPanelOnlyAdminKey(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{APIKeys: []httpauth.Key{
		{ID: "admin", Secret: "sk-admin", Admin: true},
		{ID: "user", Secret: "sk-user"},
	}})
	p := New(Config{Version: "test", Live: live})
	cases := []struct {
		key  string
		want int
	}{
		{"sk-admin", 200},
		{"sk-user", 401},
		{"sk-wrong", 401},
		{"", 401},
	}
	for _, c := range cases {
		if got := doPanel(p, "GET", "/panel/api/logs", c.key, "").Code; got != c.want {
			t.Errorf("key=%q code=%d want %d", c.key, got, c.want)
		}
	}
}

// TestKeysCRUD 列表脱敏、创建回显一次、编辑、删除与 legacy 只读保护。
func TestKeysCRUD(t *testing.T) {
	p, path := testKeysPanel(t, `{"api_key":"sk-legacy"}`)

	// 列表：只有 legacy，且不回传完整密钥。
	rec := doPanel(p, "GET", "/panel/api/keys", "sk-legacy", "")
	if rec.Code != 200 {
		t.Fatalf("list code=%d body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		Keys []keyView `json:"keys"`
	}
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Keys) != 1 || !list.Keys[0].Legacy || list.Keys[0].Masked == "sk-legacy" {
		t.Fatalf("legacy list = %+v", list.Keys)
	}

	// 创建：留空密钥自动生成，完整值只在响应里出现一次。
	rec = doPanel(p, "POST", "/panel/api/keys", "sk-legacy",
		`{"name":"codex","thinking_default_effort":"max","thinking_per_model":{"glm-5.2":"low"}}`)
	if rec.Code != 200 {
		t.Fatalf("create code=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	json.Unmarshal(rec.Body.Bytes(), &created)
	if !strings.HasPrefix(created.Key, "sk-") || created.ID == "" {
		t.Fatalf("created = %+v", created)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), created.Key) {
		t.Fatalf("新密钥未落盘: %s", raw)
	}

	// 列表：新密钥 + legacy；新密钥默认档位与模型覆盖可读。
	rec = doPanel(p, "GET", "/panel/api/keys", "sk-legacy", "")
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Keys) != 2 || list.Keys[0].DefaultEffort != "max" || list.Keys[0].PerModel["glm-5.2"] != "low" {
		t.Fatalf("list after create = %+v", list.Keys)
	}

	// 编辑：改名 + 提升为管理员。
	rec = doPanel(p, "POST", "/panel/api/keys/update", "sk-legacy",
		`{"id":"`+created.ID+`","name":"codex2","admin":true}`)
	if rec.Code != 200 {
		t.Fatalf("update code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doPanel(p, "GET", "/panel/api/keys", "sk-legacy", "")
	json.Unmarshal(rec.Body.Bytes(), &list)
	if list.Keys[0].Name != "codex2" || !list.Keys[0].Admin {
		t.Fatalf("update not applied: %+v", list.Keys[0])
	}

	// legacy 不可删。
	if rec := doPanel(p, "POST", "/panel/api/keys/delete", "sk-legacy", `{"id":"legacy"}`); rec.Code != 400 {
		t.Fatalf("delete legacy code=%d want 400", rec.Code)
	}

	// 删除新密钥：只剩 legacy。
	if rec := doPanel(p, "POST", "/panel/api/keys/delete", "sk-legacy", `{"id":"`+created.ID+`"}`); rec.Code != 200 {
		t.Fatalf("delete code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = doPanel(p, "GET", "/panel/api/keys", "sk-legacy", "")
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Keys) != 1 || !list.Keys[0].Legacy {
		t.Fatalf("list after delete = %+v", list.Keys)
	}
}

// TestKeysDeleteGuards 禁止删到一把不剩、禁止删掉最后一把管理员。
func TestKeysDeleteGuards(t *testing.T) {
	// 没有 legacy：删掉唯一密钥会让网关变无鉴权 → 拒绝。
	p, _ := testKeysPanel(t, `{"api_keys":[{"id":"only","key":"sk-onlyonly","admin":true}]}`)
	if rec := doPanel(p, "POST", "/panel/api/keys/delete", "sk-legacy", `{"id":"only"}`); rec.Code != 400 {
		t.Fatalf("delete last key code=%d want 400", rec.Code)
	}

	// 有普通密钥兜底但删的是最后一把 admin → 拒绝。
	p2, _ := testKeysPanel(t, `{"api_keys":[
		{"id":"adm","key":"sk-adminadmin","admin":true},
		{"id":"usr","key":"sk-useruser","admin":false}
	]}`)
	if rec := doPanel(p2, "POST", "/panel/api/keys/delete", "sk-legacy", `{"id":"adm"}`); rec.Code != 400 {
		t.Fatalf("delete last admin code=%d want 400", rec.Code)
	}
	if rec := doPanel(p2, "POST", "/panel/api/keys/delete", "sk-legacy", `{"id":"usr"}`); rec.Code != 200 {
		t.Fatalf("delete normal key code=%d body=%s", rec.Code, rec.Body.String())
	}
}
