package panel

import (
	"encoding/json"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestAccountModelOffAPI 面板开关接口：带前缀输入按裸名存储，on 幂等解除。
func TestAccountModelOffAPI(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1"})
	pl.SetCredits("u1", 100, 100)
	pn := New(Config{Version: "test", APIKey: "sk-legacy", Pool: pl})

	rec := doPanel(pn, "POST", "/panel/api/accounts/u1/model_off", "sk-legacy", `{"model":"cn:glm-5.2","note":"报错"}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	offs := pl.ModelOffOf("u1")
	if len(offs) != 1 || offs[0].Model != "glm-5.2" || offs[0].Note != "报错" {
		t.Fatalf("ModelOffOf=%+v", offs)
	}

	rec = doPanel(pn, "POST", "/panel/api/accounts/u1/model_on", "sk-legacy", `{"model":"glm-5.2"}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(pl.ModelOffOf("u1")) != 0 {
		t.Fatal("model_on 后应清空")
	}
	// 幂等：再开一次仍 200。
	rec = doPanel(pn, "POST", "/panel/api/accounts/u1/model_on", "sk-legacy", `{"model":"glm-5.2"}`)
	if rec.Code != 200 {
		t.Fatalf("幂等 on code=%d", rec.Code)
	}
	// 未知账号 404。
	rec = doPanel(pn, "POST", "/panel/api/accounts/nope/model_off", "sk-legacy", `{"model":"x"}`)
	if rec.Code != 404 {
		t.Fatalf("未知账号 code=%d want 404", rec.Code)
	}
}

// TestAccountModelOffInList 账号列表透出 model_off，供前端徽标与编辑使用。
func TestAccountModelOffInList(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "n1"})
	pl.SetCredits("u1", 100, 100)
	pl.SetModelOff("u1", "glm-5.2", "")
	pn := New(Config{Version: "test", APIKey: "sk-legacy", Pool: pl})
	// overview 是账号列表入口；直接验证 JSON 结构。
	rec := doPanel(pn, "GET", "/panel/api/overview", "sk-legacy", "")
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var d struct {
		Accounts []struct {
			ModelOff []struct {
				Model string `json:"model"`
			} `json:"model_off"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range d.Accounts {
		for _, o := range a.ModelOff {
			if o.Model == "glm-5.2" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("overview 应透出 model_off: %s", rec.Body.String())
	}
}
