package filesink

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRejectsUnsafeName(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 无法被清洗为安全名字的输入必须直接拒绝
	if _, err := s.Create("j1", `..\..\evil.exe`); err == nil {
		t.Fatal("windows-style traversal must be rejected")
	}
	if _, err := s.Create("j1", "a\x00b"); err == nil {
		t.Fatal("NUL byte must be rejected")
	}
	if _, err := s.Create("j1", ".."); err == nil {
		t.Fatal("parent dir must be rejected")
	}
	if _, err := s.Create("j1", "CON"); err == nil {
		t.Fatal("reserved device name must be rejected")
	}
}

func TestCreateNeutralizesTraversalIntoDir(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 纯 "/" 路径被 Base 中和：不是拒绝，而是丢弃目录成分并安全落在 dir 内
	h, err := s.Create("j1", "../../../.ssh/authorized_keys")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if filepath.Dir(h.FinalPath) != dir {
		t.Fatalf("final path escaped dir: %s", h.FinalPath)
	}
	if filepath.Base(h.FinalPath) != "authorized_keys" {
		t.Fatalf("unexpected basename %q", filepath.Base(h.FinalPath))
	}
}

func TestCreateCommitKeepsFileInsideDir(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Create("job-1", "report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(h.TempPath) != dir || filepath.Dir(h.FinalPath) != dir {
		t.Fatalf("paths must stay in dir: %+v", h)
	}
	write(t, h.TempPath, "hello")

	final, err := s.Commit(h)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(final) != "report.pdf" {
		t.Fatalf("unexpected final name %q", final)
	}
	b, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Fatalf("content mismatch: %q", b)
	}
}

func TestCreateDedupesExistingFile(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.txt"), "existing")

	s, _ := New(dir)
	h, err := s.Create("job-2", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(h.FinalPath) != "a (1).txt" {
		t.Fatalf("expected dedupe suffix, got %q", filepath.Base(h.FinalPath))
	}
}

func TestCreateDedupesWithinConcurrentBatch(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	h1, err := s.Create("j1", "same.txt")
	if err != nil {
		t.Fatal(err)
	}
	h2, err := s.Create("j2", "same.txt")
	if err != nil {
		t.Fatal(err)
	}
	if h1.FinalPath == h2.FinalPath {
		t.Fatalf("two jobs must not reserve the same final path: %s", h1.FinalPath)
	}
}

func TestAbortRemovesTemp(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	h, err := s.Create("job-3", "x.bin")
	if err != nil {
		t.Fatal(err)
	}
	write(t, h.TempPath, "partial")
	if err := s.Abort(h); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.TempPath); !os.IsNotExist(err) {
		t.Fatal("temp file must be removed")
	}
	// 二次 Abort 不应报错
	if err := s.Abort(h); err != nil {
		t.Fatal(err)
	}
}

func TestOpenTempSupportsRandomWrite(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	h, err := s.Create("job-4", "r.bin")
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.OpenTemp(h)
	if err != nil {
		t.Fatal(err)
	}
	// 乱序写入：先写 offset 4，再写 offset 0，最后写 offset 8
	if _, err := f.WriteAt([]byte("BBBB"), 4); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("AAAA"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("CCCC"), 8); err != nil {
		t.Fatal(err)
	}
	f.Close()

	b, _ := os.ReadFile(h.TempPath)
	if string(b) != "AAAABBBBCCCC" {
		t.Fatalf("random write broken: %q", b)
	}
}
