package pool

import "testing"

func TestClassifyPlan(t *testing.T) {
	cases := []struct {
		name string
		pkgs []string
		want PlanKind
	}{
		{"空列表不猜", nil, PlanUnknown},
		{"免费版强信号", []string{"Free Plan 免费额度"}, PlanFree},
		{"体验版", []string{"体验版 100 积分"}, PlanTrial},
		{"标准版", []string{"标准版月度订阅"}, PlanStandard},
		{"订阅优先于活动包", []string{"标准版订阅", "拉新活动赠送包"}, PlanStandard},
		{"全部活动包", []string{"拉新奖励包", "邀请赠送包"}, PlanActivity},
		{"混合无法判定", []string{"拉新奖励包", "未知包名"}, PlanUnknown},
		{"完全未知", []string{"something else"}, PlanUnknown},
		{"英文 bonus", []string{"Bonus Pack Q4"}, PlanActivity},
	}
	for _, c := range cases {
		got, ev := ClassifyPlan(c.pkgs)
		if got != c.want {
			t.Errorf("%s: ClassifyPlan(%v)=%s（证据 %q）want %s", c.name, c.pkgs, got, ev, c.want)
		}
		if got != PlanUnknown && ev == "" {
			t.Errorf("%s: 非 unknown 时应给出证据，got 空", c.name)
		}
	}
}
