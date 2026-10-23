// features_config_test.go features.sanitize_brand_scope 配置测试：
// 缺省 all / 大小写归一 / 非法值 fail fast。
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSanitizeBrandScopeDefault 键缺席 → all（保持旧行为：所有角色都净化品牌词）。
func TestSanitizeBrandScopeDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Features.SanitizeBrandScope != "all" {
		t.Errorf("sanitize_brand_scope=%q want all", c.Features.SanitizeBrandScope)
	}
}

// TestSanitizeBrandScopeIdentity 显式 identity（含大小写/空白）归一为小写。
func TestSanitizeBrandScopeIdentity(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"features":{"sanitize_brand_scope":" Identity "}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Features.SanitizeBrandScope != "identity" {
		t.Errorf("sanitize_brand_scope=%q want identity", c.Features.SanitizeBrandScope)
	}
}

// TestSanitizeBrandScopeInvalid 非法值启动报错（静默回落会让用户误以为收窄已生效）。
func TestSanitizeBrandScopeInvalid(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"features":{"sanitize_brand_scope":"system-only"}}`), 0o600)
	_, err := Load(fp)
	if err == nil || !strings.Contains(err.Error(), "sanitize_brand_scope") {
		t.Fatalf("want sanitize_brand_scope error, got %v", err)
	}
}
