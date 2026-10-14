package filesink

import (
	"errors"
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

// 目标目录已存在同名文件夹：直接【复用】那个目录，不改名成「照片 (1)」。
func TestCreateInReusesExistingBatchRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "照片"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 目录里已有的文件必须原样保留：是合并，不是另起一份。
	write(t, filepath.Join(dir, "照片", "keep.txt"), "keep")
	s, _ := New(dir)

	h, err := s.CreateIn(ports.SinkRequest{JobID: "j1", BatchID: "b1", RelPath: "照片/a.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "照片", "a.jpg"); h.FinalPath != want {
		t.Errorf("final = %q，期望复用已有目录 %q", h.FinalPath, want)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "照片", "keep.txt")); err != nil || string(b) != "keep" {
		t.Errorf("目录里原有的文件被动了：%v %q", err, b)
	}
}

// 同名【文件】挡在根目录的位置（无法合并成目录）：只能退到唯一名，
// 否则这一批根本没有地方可落。
func TestCreateInRootBlockedByFileFallsBack(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "照片"), "i am a file")
	s, _ := New(dir)

	h, err := s.CreateIn(ports.SinkRequest{JobID: "j1", BatchID: "b1", RelPath: "照片/a.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "照片 (1)", "a.jpg"); h.FinalPath != want {
		t.Errorf("final = %q，期望退到 %q", h.FinalPath, want)
	}
}

// 不同批次用同一个文件夹名：落到【同一个目录】里。
//
// 「同名目录就是同一个目录」：再开一个「照片 (1)」会把同一份内容分成两份
// （重复下载同一个共享时最刺眼 —— 下三个文件就得到三个目录）。
func TestCreateInBatchesShareExistingRoot(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	h1, err := s.CreateIn(ports.SinkRequest{JobID: "j1", BatchID: "b1", RelPath: "照片/a.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := s.CreateIn(ports.SinkRequest{JobID: "j2", BatchID: "b2", RelPath: "照片/b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "照片")
	for _, h := range []ports.SinkHandle{h1, h2} {
		if filepath.Dir(h.FinalPath) != want {
			t.Errorf("落点 = %q，期望同在 %q 下", h.FinalPath, want)
		}
	}
}

// 同名同路径的文件是「同一份文件的再次到达」：就地覆盖，不留 (1) 副本。
func TestCreateInOverwritesExistingFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "照片"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "照片", "a.jpg"), "old")
	s, _ := New(dir)

	h, err := s.CreateIn(ports.SinkRequest{JobID: "j2", BatchID: "b2", RelPath: "照片/a.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "照片", "a.jpg"); h.FinalPath != want {
		t.Fatalf("final = %q，期望就地覆盖 %q", h.FinalPath, want)
	}
	if err := os.WriteFile(h.TempPath, []byte("new-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(h); err != nil {
		t.Fatal(err)
	}

	if b, err := os.ReadFile(h.FinalPath); err != nil || string(b) != "new-content" {
		t.Fatalf("文件内容 = %q（err=%v），期望被覆盖为 new-content", b, err)
	}
	ents, err := os.ReadDir(filepath.Join(dir, "照片"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != "a.jpg" {
			t.Errorf("目录里多出了 %s（重复下载不该产生副本）", e.Name())
		}
	}
}

// 覆盖语义的第二半：同一条路径【此刻正被另一个任务写】时退到唯一名，
// 免得两份并发的内容互相把对方覆盖掉；占位在 Commit 后释放，于是
// 「先后到达」的重复下载又回到覆盖。
//
// 这两条必须一起成立：只做后者会让覆盖在「同一进程内的第二次下载」失效
// （占位从没释放过），只做前者会留下并发互相覆盖的窗口。
func TestCreateInMergesSequentialButNotConcurrent(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	h1, err := s.CreateIn(ports.SinkRequest{JobID: "j1", BatchID: "b1", RelPath: "照片/a.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	// 并发到达的同名文件：让开一步（h1 还在写这条路径）。
	h2, err := s.CreateIn(ports.SinkRequest{JobID: "j2", BatchID: "b2", RelPath: "照片/a.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "照片", "a (1).jpg"); h2.FinalPath != want {
		t.Errorf("并发落点 = %q，期望 %q", h2.FinalPath, want)
	}

	// h1 写完 → 占位释放 → 再来的同名文件回到覆盖语义。
	if _, err := s.Commit(h1); err != nil {
		t.Fatal(err)
	}
	h3, err := s.CreateIn(ports.SinkRequest{JobID: "j3", BatchID: "b3", RelPath: "照片/a.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "照片", "a.jpg"); h3.FinalPath != want {
		t.Errorf("顺序落点 = %q，期望覆盖原文件 %q", h3.FinalPath, want)
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
	_, err := s.CreateIn(ports.SinkRequest{JobID: "j", RelPath: "照片/a.txt"})
	if err == nil {
		t.Fatal("空 batch_id 必须被拒绝")
	}
	// 必须可判别：调用方要把「传了空 id」与「磁盘写不了」分开处理
	if !errors.Is(err, ports.ErrEmptyBatchID) {
		t.Errorf("错误应可用 errors.Is(ports.ErrEmptyBatchID) 判别，得到 %v", err)
	}
}

// 单段 rel_path = 「文件夹本身」这一条，且【绝不能 panic】。
//
// 这条输入不是畸形数据，而是我们自己的发送端在「空文件夹」时必然发出的
// （设计 3.3：整个文件夹为空时，为根目录本身发一条）。第一版实现里
// segs[1:len(segs)-1] 会越界 panic，而且 panic 跳过了手写的 Unlock ——
// 整个 Sink 的锁就此永久占用，之后每条接收任务都卡死。
func TestCreateInSingleSegmentIsRootDirOnly(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	h, err := s.CreateIn(ports.SinkRequest{JobID: "j1", BatchID: "b1", RelPath: "空的", IsDir: true})
	if err != nil {
		t.Fatalf("空文件夹的根目录条目必须被接受: %v", err)
	}
	if want := filepath.Join(dir, "空的"); h.FinalPath != want {
		t.Errorf("final = %q，期望 %q", h.FinalPath, want)
	}
	if h.TempPath != "" {
		t.Errorf("目录条目不该有临时文件，得到 %q", h.TempPath)
	}

	// 幂等：重复到达不能改名，也不能 panic（panic 会让锁死掉）
	h2, err := s.CreateIn(ports.SinkRequest{JobID: "j2", BatchID: "b1", RelPath: "空的", IsDir: true})
	if err != nil {
		t.Fatalf("重复的根目录条目必须幂等: %v", err)
	}
	if h2.FinalPath != h.FinalPath {
		t.Errorf("重复的根目录条目被改名了：%q ≠ %q", h2.FinalPath, h.FinalPath)
	}

	// 同批文件仍然落在【同一个根】下（不是「空的/空的」）
	h3, err := s.CreateIn(ports.SinkRequest{JobID: "j3", BatchID: "b1", RelPath: "空的/a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Dir(h3.FinalPath); got != filepath.Join(dir, "空的") {
		t.Errorf("同批文件落在 %q，期望 %q", got, filepath.Join(dir, "空的"))
	}

	// 单段但声称是文件：必须明确报错，不能悄悄落成根目录
	if _, err := s.CreateIn(ports.SinkRequest{JobID: "j4", BatchID: "b1", RelPath: "a.txt"}); err == nil {
		t.Error("单段文件路径必须被拒绝")
	}
}

// 同一批次里 rel_path 的首段名必须一致。
//
// 不一致就是发送端有 bug 或者被伪造；按已分配的根静默落盘会把两个文件夹
// 合并成一个 —— 用户看到的是「传了两次，结果只有一个」。
func TestCreateInRejectsInconsistentRootName(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	if _, err := s.CreateIn(ports.SinkRequest{JobID: "j1", BatchID: "b1", RelPath: "甲/a.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateIn(ports.SinkRequest{JobID: "j2", BatchID: "b1", RelPath: "乙/b.txt"}); err == nil {
		t.Fatal("同批首段名不一致必须被拒绝")
	}
}

// 认领一个接收目录之外的根目录必须被忽略。
//
// 认领等于「把整批文件的落点交给调用方」，而调用方（重启恢复）手里同时有
// 发送侧与接收侧的任务 —— 少一行方向过滤就会把接收数据写进用户自己的
// 源文件夹。这里把安全性收在 Sink 内部，而不是指望调用方不犯这个错。
func TestAdoptRootIgnoresPathOutsideDownloadDir(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	s, _ := New(dir)

	s.AdoptRoot("b1", outside)

	h, err := s.CreateIn(ports.SinkRequest{JobID: "j1", BatchID: "b1", RelPath: "照片/a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(h.FinalPath) != filepath.Join(dir, "照片") {
		t.Fatalf("越界认领被采纳了：final = %q（应重新分配到接收目录内）", h.FinalPath)
	}
	if _, err := os.Stat(filepath.Join(outside, "照片", "a.txt")); !os.IsNotExist(err) {
		t.Error("不该在接收目录之外创建任何路径")
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
