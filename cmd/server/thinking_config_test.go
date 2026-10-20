package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestThinkingConfigNormalize thinking.* 解析与归一化：小写化、非法档位 fail fast。
func TestThinkingConfigNormalize(t *testing.T) {
	// 合法：大小写混合归一为小写。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"thinking":{"default_effort":" HIGH ","per_model":{"deepseek-v4.1-flash":"XHigh"}}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Thinking.DefaultEffort != "high" {
		t.Fatalf("default_effort=%q want high", c.Thinking.DefaultEffort)
	}
	if c.Thinking.PerModel["deepseek-v4.1-flash"] != "xhigh" {
		t.Fatalf("per_model=%v", c.Thinking.PerModel)
	}

	// 非法全局档位：启动报错（不悄悄回落）。
	os.WriteFile(fp, []byte(`{"thinking":{"default_effort":"ultra"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("非法档位应报错")
	}

	// 非法 per_model 档位：同样 fail fast。
	os.WriteFile(fp, []byte(`{"thinking":{"per_model":{"glm-5.2":"turbo"}}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("per_model 非法档位应报错")
	}

	// 缺省零值：关闭注入（旧行为）。
	os.WriteFile(fp, []byte(`{}`), 0o600)
	c, err = Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Thinking.DefaultEffort != "" || len(c.Thinking.PerModel) != 0 {
		t.Fatalf("缺省应为空: %+v", c.Thinking)
	}
}
