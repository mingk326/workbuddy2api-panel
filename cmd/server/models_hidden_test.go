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
