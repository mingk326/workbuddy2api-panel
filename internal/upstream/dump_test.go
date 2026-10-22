package upstream

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestDumpUpstreamWritesFile 开启 dump 后：请求体与响应字节都落盘，Close 关文件。
func TestDumpUpstreamWritesFile(t *testing.T) {
	dir := t.TempDir()
	c := &Client{DumpDir: dir}
	src := io.NopCloser(strings.NewReader("data: {\"choices\":[]}\n\n"))
	rc := c.dumpUpstream(src, []byte(`{"model":"glm-5.2","messages":[]}`), &auth.Auth{UID: "abcdefgh1234"})
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("dump files=%d want 1", len(entries))
	}
	raw, err := os.ReadFile(dir + string(os.PathSeparator) + entries[0].Name())
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{"=== request ===", `"model":"glm-5.2"`, "=== response ===", `data: {"choices":[]}`} {
		if !strings.Contains(body, want) {
			t.Errorf("dump 内容缺 %q：\n%s", want, body)
		}
	}
	// 文件名只带 UID 前 8 位，不落完整 UID。
	if strings.Contains(entries[0].Name(), "abcdefgh1234") {
		t.Errorf("dump 文件名不应含完整 UID: %s", entries[0].Name())
	}
}

// TestDumpUpstreamDisabled 未配置目录时原样返回（零开销、零行为变化）。
func TestDumpUpstreamDisabled(t *testing.T) {
	c := &Client{}
	src := io.NopCloser(strings.NewReader("x"))
	if got := c.dumpUpstream(src, []byte("{}"), nil); got != src {
		t.Fatal("DumpDir 为空时必须原样返回 rc")
	}
}

// TestDumpUpstreamRequestMax 请求体片段按上限截断（响应仍完整记录）。
func TestDumpUpstreamRequestMax(t *testing.T) {
	dir := t.TempDir()
	c := &Client{DumpDir: dir, DumpRequestMax: 8}
	rc := c.dumpUpstream(io.NopCloser(strings.NewReader("R")), []byte("0123456789ABCDEF"), nil)
	io.ReadAll(rc)
	rc.Close()

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("dump files=%d", len(entries))
	}
	raw, _ := os.ReadFile(dir + string(os.PathSeparator) + entries[0].Name())
	if !strings.Contains(string(raw), "01234567") || strings.Contains(string(raw), "89ABCDEF") {
		t.Errorf("请求体应截断到 8 字节：\n%s", raw)
	}
}
