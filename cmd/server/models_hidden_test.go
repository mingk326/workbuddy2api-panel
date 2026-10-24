// models_hidden_test.go config models.hidden 的归一化。
package main

import (
	"reflect"
	"testing"
)

func TestModelsHiddenNormalized(t *testing.T) {
	c, err := ParseConfig([]byte(`{"models":{"hidden":[" glm-5.2 ","glm-5.2","","cn:kimi-k3-1"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"glm-5.2", "cn:kimi-k3-1"}
	if got := c.Models.Hidden; !reflect.DeepEqual(got, want) {
		t.Fatalf("hidden=%v want %v", got, want)
	}
}

// TestModelsAliasesReserveAndCNBase 别名去重、保留积分解析、CN base 去尾斜杠。
func TestModelsAliasesReserveAndCNBase(t *testing.T) {
	c, err := ParseConfig([]byte(`{
		"models":{"aliases":[" claude=glm-5.2 ","claude=glm-5.2"]},
		"pool":{"reserve_credits":7},
		"cn":{"chat_base":"https://cn.example/"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Models.Aliases) != 1 || c.Models.Aliases[0] != "claude=glm-5.2" {
		t.Errorf("aliases=%v", c.Models.Aliases)
	}
	if c.Pool.ReserveCredits != 7 {
		t.Errorf("reserve_credits=%d want 7", c.Pool.ReserveCredits)
	}
	if c.CN.ChatBase != "https://cn.example" {
		t.Errorf("cn.chat_base=%q", c.CN.ChatBase)
	}
}
