package panel

import (
	"os"
	"strings"
	"testing"
)

// TestWorkBuddyValorantRegistered issue #228：新增任务的判据链与 Expert_lighthouse
// 同构，但两侧注册缺一不可——后端 autoActions 缺了则任务中心无「一键完成」，
// 前端 AUTO_TASKS 缺了则按钮不出现，两种情况都不会报错，只能在面板上肉眼发现。
func TestWorkBuddyValorantRegistered(t *testing.T) {
	const code = "WorkBuddy_Valorant"

	act := autoActionFor(code)
	if act == nil {
		t.Fatalf("autoActions 未注册 %s", code)
	}
	if act.run == nil {
		t.Fatalf("%s 未绑定 run 实现", code)
	}
	if !act.Attempt {
		t.Errorf("%s 判据尚未实测点亮，必须标记 Attempt（否则面板会显示成已验证）", code)
	}

	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "'"+code+"':") {
		t.Fatalf("app.js AUTO_TASKS 未注册 %s（任务中心不会出现一键完成按钮）", code)
	}
}
