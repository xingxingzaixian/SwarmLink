//go:build wails

// 这个文件必须带 wails tag：根目录刻意保持「不带该 tag 时没有可编译文件」，
// 而测试文件无法被 go build 忽略 —— 不带 tag 时它会让 `go test ./...` 编译失败。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 首选位置可写时必须用它，并且不报任何原因（否则对话框会平白多一句解释）。
func TestOpenFirstWritableUsesPrimary(t *testing.T) {
	primary := filepath.Join(t.TempDir(), "logs", "swarmlink.log")

	f, got, err := openFirstWritable([]string{primary})
	if f == nil {
		t.Fatalf("首选可写时不该失败：%v", err)
	}
	defer func() { _ = f.Close() }()

	if got != primary {
		t.Fatalf("落点 = %q，期望 %q", got, primary)
	}
	if err != nil {
		t.Fatalf("落在首选位置时不该返回原因，得到 %v", err)
	}
}

// 首选位置写不了时必须退到备用位置 —— 这是「日志可取证」这条承诺的兜底。
//
// 为什么值得一条测试：本项目真实遇到过「数据目录不可写」的启动失败，而那种
// 情况下日志恰恰是最需要的证据。这个分支无法靠启动产物来验证（受限环境里
// 连临时目录也可能写不了），所以在这里用确定性的方式钉住它。
func TestOpenFirstWritableFallsBackWhenPrimaryUnwritable(t *testing.T) {
	dir := t.TempDir()

	// 拿一个【普通文件】当父目录：MkdirAll 必然失败，且与平台无关。
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(blocker, "sub", "swarmlink.log")
	good := filepath.Join(dir, "fallback", "swarmlink.log")

	f, got, err := openFirstWritable([]string{bad, good})
	if f == nil {
		t.Fatalf("首选不可写时应退到备用位置，却整个失败了：%v", err)
	}
	defer func() { _ = f.Close() }()

	if got != good {
		t.Fatalf("落点 = %q，期望 %q", got, good)
	}
	// 退到备用位置时【必须】带回首选失败的原因：对话框要据此说明
	// 「日志为什么不在数据目录里」，否则用户会去数据目录里找一个不存在的文件。
	if err == nil {
		t.Fatal("退到备用位置时必须返回首选失败的原因")
	}
	if _, statErr := os.Stat(good); statErr != nil {
		t.Fatalf("备用位置的日志文件应当已被创建：%v", statErr)
	}
}

// 全部候选都不可写时必须明确失败，而不是返回一个 nil 句柄却让调用方以为成功了。
func TestOpenFirstWritableReportsAllReasons(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad1 := filepath.Join(blocker, "a", "swarmlink.log")
	bad2 := filepath.Join(blocker, "b", "swarmlink.log")

	f, got, err := openFirstWritable([]string{bad1, bad2})
	if f != nil {
		_ = f.Close()
		t.Fatal("两处都不可写时不该返回句柄")
	}
	if err == nil {
		t.Fatal("两处都不可写时必须返回原因")
	}
	if got != bad1 {
		t.Fatalf("失败时应回报首选路径 %q，得到 %q", bad1, got)
	}
}
