package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestReserveCreditsBlocksAllModels 已知余额 <= 阈值时停止接单（含免费模型）。
func TestReserveCreditsBlocksAllModels(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetReserveCredits(10)
	p.Add(&auth.Auth{UID: "low"})
	p.SetCredits("low", 10, 100) // 已知余额，恰好等于阈值
	if a := p.PickExcludingForRealm(nil, "any-model", ""); a != nil {
		t.Fatalf("保留积分触底号不应接单: %s", a.UID)
	}
	// 充值到阈值以上自动恢复。
	p.SetCredits("low", 11, 100)
	if a := p.PickExcludingForRealm(nil, "any-model", ""); a == nil {
		t.Fatal("余额恢复后应重新可选")
	}
}

// TestReserveCreditsUnknownBalanceAllowed 余额从未查过的账号不受影响。
func TestReserveCreditsUnknownBalanceAllowed(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetReserveCredits(10)
	p.Add(&auth.Auth{UID: "fresh"}) // 未 SetCredits → 余额未知
	if a := p.PickExcludingForRealm(nil, "m", ""); a == nil {
		t.Fatal("余额未知的账号不应被保留积分拦截")
	}
}

// TestReserveCreditsStickyPath 粘性路径同样受保留积分约束。
func TestReserveCreditsStickyPath(t *testing.T) {
	p := New("")
	p.SetReserveCredits(10)
	p.Add(&auth.Auth{UID: "low"})
	p.SetCredits("low", 5, 100)
	if a := p.PickByUIDForModel("low", "m"); a != nil {
		t.Fatal("粘性路径不应选中保留积分触底号")
	}
}

// TestModelOffManualWins 手动关闭优先：正常 / 粘性选号都避开，开启后恢复。
func TestModelOffManualWins(t *testing.T) {
	p := New("")
	p.SetCostExploreInterval(0)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1000, 1000)

	if !p.SetModelOff("u1", "glm-5.2", "上游总报错") {
		t.Fatal("SetModelOff 应成功")
	}
	if a := p.PickExcludingForRealm(nil, "glm-5.2", ""); a != nil {
		t.Fatalf("手动关闭的模型不应被选中: %s", a.UID)
	}
	if a := p.PickByUIDForModel("u1", "glm-5.2"); a != nil {
		t.Fatal("粘性路径不应选中手动关闭的组合")
	}
	// 其他模型不受影响。
	if a := p.PickExcludingForRealm(nil, "other", ""); a == nil {
		t.Fatal("其他模型应照常可选")
	}
	st, ok := p.Status("u1")
	if !ok || len(st.ModelOff) != 1 || st.ModelOff[0].Model != "glm-5.2" || st.ModelOff[0].Note != "上游总报错" {
		t.Fatalf("Status.ModelOff=%+v", st.ModelOff)
	}

	if !p.SetModelOn("u1", "glm-5.2") {
		t.Fatal("SetModelOn 应成功")
	}
	if a := p.PickByUIDForModel("u1", "glm-5.2"); a == nil {
		t.Fatal("手动开启后应恢复可选")
	}
	if st, _ := p.Status("u1"); len(st.ModelOff) != 0 {
		t.Fatalf("开启后 ModelOff 应为空: %+v", st.ModelOff)
	}
}
