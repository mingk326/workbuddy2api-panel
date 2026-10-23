// api_keys_models_test.go api_keys[].allowed_models 配置归一化与运行期转换。
package main

import (
	"reflect"
	"testing"
)

// TestAPIKeysAllowedModelsNormalized 白名单去空白、去重、保序，并透传到运行期密钥表。
func TestAPIKeysAllowedModelsNormalized(t *testing.T) {
	c, err := ParseConfig([]byte(`{"api_keys":[{"key":"sk-a","admin":true,
		"allowed_models":[" glm-5.3 ","glm-5.3","","global:hy-image-3.5"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"glm-5.3", "global:hy-image-3.5"}
	if got := c.APIKeys[0].AllowedModels; !reflect.DeepEqual(got, want) {
		t.Fatalf("allowed_models=%v want %v", got, want)
	}
	keys := c.EffectiveKeys()
	if len(keys) != 1 {
		t.Fatalf("effective keys=%d", len(keys))
	}
	if !keys[0].AllowsModel("cn", "glm-5.3") || !keys[0].AllowsModel("global", "glm-5.3") {
		t.Error("裸名应对两个域都生效")
	}
	if keys[0].AllowsModel("cn", "hy-image-3.5") {
		t.Error("global: 前缀不应允许 cn 域")
	}
	if !keys[0].AllowsModel("global", "hy-image-3.5") {
		t.Error("global: 前缀应允许 global 域")
	}
}

// TestLegacyKeyUnrestricted 旧版 api_key 没有白名单 → 不限制（零回归）。
func TestLegacyKeyUnrestricted(t *testing.T) {
	c, err := ParseConfig([]byte(`{"api_key":"sk-legacy"}`))
	if err != nil {
		t.Fatal(err)
	}
	keys := c.EffectiveKeys()
	if len(keys) != 1 || keys[0].ID != "legacy" {
		t.Fatalf("effective keys=%+v", keys)
	}
	if !keys[0].AllowsModel("global", "any-model") {
		t.Error("legacy 密钥不应受模型白名单限制")
	}
}
