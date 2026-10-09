package filesink

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/swarmlink/swarmlink/internal/domain/ports"
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

// 批次内的文件必须落到同一个根目录下，并还原出相对路径的层次。
func TestCreateInRestoresTree(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	h1, err := s.CreateIn(ports.SinkRequest{JobID: "j1", BatchID: "b1", RelPath: "照片/2024/a.jpg"})
	if err != nil {
		t.Fatalf("createIn: %v", err)
	}
	if want := filepath.Join(dir, "照片", "2024", "a.jpg"); h1.FinalPath != want {
		t.Errorf("final = %q，期望 %q", h1.FinalPath, want)
	}
	// 临时文件必须与最终文件【同目录】：跨目录 os.Rename 不保证原子（ADR-005）
	if filepath.Dir(h1.TempPath) != filepath.Dir(h1.FinalPath) {
		t.Errorf("临时文件目录 = %q，必须与最终目录 %q 相同", filepath.Dir(h1.TempPath), filepath.Dir(h1.FinalPath))
	}
	if _, err := os.Stat(filepath.Dir(h1.TempPath)); err != nil {
		t.Errorf("父目录应已创建: %v", err)
	}

	// 同批第二个文件必须在【同一个根】下，不能又建一个「照片 (1)」
	h2, err := s.CreateIn(ports.SinkRequest{JobID: "j2", BatchID: "b1", RelPath: "照片/b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "照片", "b.txt"); h2.FinalPath != want {
		t.Errorf("同批第二个文件 final = %q，期望 %q", h2.FinalPath, want)
	}
}

// 目标目录已存在同名文件夹：整批退到「照片 (1)」，两份内容不混合。
func TestCreateInRenamesExistingBatchRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "照片"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, _ := New(dir)

	h, err := s.CreateIn(ports.SinkRequest{JobID: "j1", BatchID: "b1", RelPath: "照片/a.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "照片 (1)", "a.jpg"); h.FinalPath != want {
		t.Errorf("final = %q，期望改名为 %q", h.FinalPath, want)
	}
}

// 不同批次用同一个文件夹名：必须各分配各的根，不能共用一个。
func TestCreateInDifferentBatchesGetSeparateRoots(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	h1, err := s.CreateIn(ports.SinkRequest{JobID: "j1", BatchID: "b1", RelPath: "照片/a.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := s.CreateIn(ports.SinkRequest{JobID: "j2", BatchID: "b2", RelPath: "照片/a.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if h1.FinalPath == h2.FinalPath {
		t.Fatalf("两个批次不该共用根目录：%s", h1.FinalPath)
	}
	if filepath.Dir(h2.FinalPath) != filepath.Join(dir, "照片 (1)") {
		t.Errorf("第二批根目录 = %q，期望 %q", filepath.Dir(h2.FinalPath), filepath.Join(dir, "照片 (1)"))
	}
}

// 重启后认领已有的根目录：否则续传会把一个文件夹拆成「照片」和「照片 (1)」两份。
func TestAdoptRootKeepsSameRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "照片"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, _ := New(dir)
	s.AdoptRoot("b1", filepath.Join(dir, "照片"))

	h, err := s.CreateIn(ports.SinkRequest{JobID: "j1", BatchID: "b1", RelPath: "照片/b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "照片", "b.txt"); h.FinalPath != want {
		t.Errorf("认领后 final = %q，期望 %q（不能重新分配）", h.FinalPath, want)
	}
}

// 目录条目：只建目录（幂等），不建临时文件。
func TestCreateInDir(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	h, err := s.CreateIn(ports.SinkRequest{JobID: "j1", BatchID: "b1", RelPath: "照片/空目录", IsDir: true})
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(h.FinalPath)
	if err != nil || !st.IsDir() {
		t.Fatalf("目录未创建: %v", err)
	}
	if h.TempPath != "" {
		t.Errorf("目录条目不应当有临时文件，得到 %q", h.TempPath)
	}

	// 同一目录再来一次（空目录与它下面的文件都会走到这里）不能报错、也不能改名
	again, err := s.CreateIn(ports.SinkRequest{JobID: "j2", BatchID: "b1", RelPath: "照片/空目录", IsDir: true})
	if err != nil {
		t.Fatalf("重复建目录必须幂等: %v", err)
	}
	if again.FinalPath != h.FinalPath {
		t.Errorf("重复建目录被改名了：%q ≠ %q", again.FinalPath, h.FinalPath)
	}
}

// 恶意相对路径必须被拒，且不能在上级目录留下任何东西。
func TestCreateInRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	for _, rel := range []string{"../../evil.txt", "/tmp/evil.txt", "照片/../../evil.txt", "", `C:\evil.txt`} {
		if _, err := s.CreateIn(ports.SinkRequest{JobID: "j", BatchID: "b", RelPath: rel}); err == nil {
			t.Errorf("rel_path = %q 必须被拒绝", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "evil.txt")); !os.IsNotExist(err) {
		t.Error("拒绝的路径不应在上级目录留下文件")
	}
}

// 空 batch_id 必须拒绝：它意味着「不是批次」，而 CreateIn 专门服务批次。
func TestCreateInRejectsEmptyBatchID(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	if _, err := s.CreateIn(ports.SinkRequest{JobID: "j", RelPath: "照片/a.txt"}); err == nil {
		t.Fatal("空 batch_id 必须被拒绝")
	}
}

// 单文件路径（Create）的行为不能被这次改动碰到。
func TestCreateStillFlatAndDeduped(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.txt"), "existing")
	s, _ := New(dir)

	h, err := s.Create("job-x", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(h.FinalPath) != dir {
		t.Fatalf("单文件必须仍落在接收目录里: %s", h.FinalPath)
	}
	if filepath.Base(h.FinalPath) != "a (1).txt" {
		t.Fatalf("单文件去重行为变了: %q", filepath.Base(h.FinalPath))
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
