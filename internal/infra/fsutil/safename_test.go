package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeFileNameRejectsUnsafeNames(t *testing.T) {
	// 这些必须【直接拒绝】：无法通过丢弃目录成分变成安全名字
	rejected := []string{
		`..\..\Startup\evil.exe`, // Windows 风格路径：Unix 上 Base 不会拆 "\"，由分隔符检查拦下
		"a\x00b",                 // 空字节截断
		"..",
		".",
		"",
		"...", // 全是点，TrimRight 后为空
		"CON", // Windows 保留设备名
		"nul", // 大小写不敏感
		"COM1",
		"lpt9.txt", // 保留名带扩展名
	}
	for _, a := range rejected {
		if got, err := SanitizeFileName(a); err == nil {
			t.Fatalf("unsafe name %q must be rejected, got %q", a, got)
		}
	}
}

func TestSanitizeFileNameNeutralizesDirectoryComponents(t *testing.T) {
	// 纯 "/" 目录成分与绝对路径由 Base 中和（架构书步骤 1）：
	// 不是拒绝，而是丢弃目录成分 —— 这正是修复「目录穿越写任意路径」的方式。
	cases := map[string]string{
		"docs/report.pdf":                          "report.pdf",
		"/etc/launchd.conf":                        "launchd.conf",
		"../../../../Users/x/.ssh/authorized_keys": "authorized_keys",
	}
	for in, want := range cases {
		got, err := SanitizeFileName(in)
		if err != nil {
			t.Fatalf("%q: unexpected error %v", in, err)
		}
		if got != want {
			t.Fatalf("%q: want %q got %q", in, want, got)
		}
		// 结果不含任何分隔符，因此后续 SecureJoin 必然落在目标目录内
		if strings.ContainsAny(got, `/\`) {
			t.Fatalf("%q produced a name with separators: %q", in, got)
		}
	}
}

func TestSanitizeFileNameAcceptsLegitNames(t *testing.T) {
	cases := map[string]string{
		"report.pdf":   "report.pdf",
		"我的文档.docx":    "我的文档.docx",
		"a b c.tar.gz": "a b c.tar.gz",
		"trailing. ":   "trailing",
		"file.":        "file",
	}
	for in, want := range cases {
		got, err := SanitizeFileName(in)
		if err != nil {
			t.Fatalf("%q: unexpected error %v", in, err)
		}
		if got != want {
			t.Fatalf("%q: want %q got %q", in, want, got)
		}
	}
}

func TestSanitizeFileNameTruncatesAtUTF8Boundary(t *testing.T) {
	// 200 个 3 字节汉字 = 600 字节，必须截断到 <=255 且不切坏字符
	name := strings.Repeat("文", 200)
	got, err := SanitizeFileName(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > maxNameBytes {
		t.Fatalf("too long: %d bytes", len(got))
	}
	if !strings.HasPrefix(name, got) {
		t.Fatal("truncation must keep a prefix of the original")
	}
	for _, r := range got {
		if r == '\uFFFD' {
			t.Fatal("truncation broke a multibyte character")
		}
	}
}

func TestSecureJoinBlocksEscape(t *testing.T) {
	dir := t.TempDir()
	if _, err := SecureJoin(dir, "../outside.txt"); !errors.Is(err, ErrEscape) {
		t.Fatalf("want ErrEscape got %v", err)
	}
	ok, err := SecureJoin(dir, "inside.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ok, dir) {
		t.Fatal("expected path inside dir")
	}
}

func TestUniquePathAvoidsCollision(t *testing.T) {
	dir := t.TempDir()
	first, err := UniquePath(dir, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := UniquePath(dir, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("expected a different path")
	}
	if filepath.Base(second) != "a (1).txt" {
		t.Fatalf("want 'a (1).txt' got %q", filepath.Base(second))
	}
}

func TestTempPartNameIsSafe(t *testing.T) {
	got := TempPartName("../../evil")
	if strings.ContainsAny(got, `/\`) {
		t.Fatalf("temp name must be safe: %q", got)
	}
	if !strings.HasPrefix(got, ".swarmlink.") || !strings.HasSuffix(got, ".part") {
		t.Fatalf("unexpected temp name %q", got)
	}
}

func TestSanitizeToken(t *testing.T) {
	if got := SanitizeToken("ab/cd\\e f"); got != "abcdef" {
		t.Fatalf("want abcdef got %q", got)
	}
	if got := SanitizeToken("!!!"); got != "job" {
		t.Fatalf("want job got %q", got)
	}
}
