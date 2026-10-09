package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/swarmlink/swarmlink/internal/adapters/filesink"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// newRecvFixture 造一个接收侧环境：真 filesink（临时目录）+ 内存仓储 + 哑会话。
func newRecvFixture(t *testing.T) (*TransferApp, *stubTransferRepo, string, identity.NodeID) {
	t.Helper()
	repo := &stubTransferRepo{}
	dir := t.TempDir()
	sink, err := filesink.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := NewTransferApp(identity.NodeID{}, repo, sink, noConns{}, nil, eventbus.New(), clock.New(), nil)
	a.ChunkSize = 1024

	// 接收端在传输期间【刻意】持有临时文件句柄（直到 FILE_DONE 才关）。
	// Windows 上删不掉被打开的文件，因此要在 t.TempDir 的清理之前把句柄关掉
	// —— t.Cleanup 是后进先出，这里注册的会先跑。
	t.Cleanup(func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, rj := range a.receivers {
			if rj.f != nil {
				_ = rj.f.Close()
			}
		}
	})

	return a, repo, dir, mustNodeID(t, testPeerHex)
}

func metaFrame(t *testing.T, meta protocol.FileMeta) protocol.Frame {
	t.Helper()
	payload, err := protocol.EncodeJSON(meta)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.New(protocol.TypeFileMeta, payload)
}

// 带 rel_path 的 META：落到子目录、批次字段落库、临时文件与目标同目录。
func TestHandleFileMetaWithRelPath(t *testing.T) {
	a, repo, dir, peerID := newRecvFixture(t)
	sess := stubSession{peer: peerID}

	meta := protocol.FileMeta{
		JobID: "j1", FileID: "h1", FileName: "a.jpg", FileSize: 2048,
		ChunkSize: 1024, TotalChunks: 2,
		BatchID: "b1", RelPath: "照片/2024/a.jpg",
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err != nil {
		t.Fatalf("HandleFileMeta: %v", err)
	}

	job, err := repo.GetJob("j1")
	if err != nil {
		t.Fatal(err)
	}
	if job.BatchID != "b1" || job.RelPath != "照片/2024/a.jpg" || job.IsDir {
		t.Errorf("落库的批次字段不对：%+v", job)
	}
	if want := filepath.Join(dir, "照片", "2024", "a.jpg"); job.LocalPath != want {
		t.Errorf("local_path = %q，期望 %q", job.LocalPath, want)
	}
	if filepath.Dir(job.TempPath) != filepath.Dir(job.LocalPath) {
		t.Errorf("临时文件必须与目标同目录：%q vs %q", job.TempPath, job.LocalPath)
	}
}

// 目录条目：total_chunks=0 必须放行（空目录没有分块），只建目录；
// 随后的 FILE_DONE 直接收尾，不做校验。
func TestHandleFileMetaDirEntry(t *testing.T) {
	a, repo, dir, peerID := newRecvFixture(t)
	sess := stubSession{peer: peerID}

	meta := protocol.FileMeta{
		JobID: "d1", FileName: "空的", ChunkSize: 1024,
		BatchID: "b1", RelPath: "照片/空的", IsDir: true,
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err != nil {
		t.Fatalf("目录条目必须被接受: %v", err)
	}

	st, err := os.Stat(filepath.Join(dir, "照片", "空的"))
	if err != nil || !st.IsDir() {
		t.Fatalf("目录没建出来: %v", err)
	}
	if job, err := repo.GetJob("d1"); err != nil || !job.IsDir {
		t.Errorf("落库的目录条目不对：%+v err=%v", job, err)
	}

	// DONE：目录没有块可校验，不能去 Close 一个 nil 文件句柄
	payload, err := protocol.EncodeJSON(protocol.FileDone{JobID: "d1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.HandleFileDone(sess, protocol.New(protocol.TypeFileDone, payload)); err != nil {
		t.Fatalf("目录条目的 FILE_DONE 不该报错: %v", err)
	}
	job, err := repo.GetJob("d1")
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != transfer.StateDone {
		t.Errorf("目录条目状态 = %s，期望 done", job.Status)
	}
}

// 回归：非目录条目的空文件仍然是协议非法 —— 这条校验不能被放宽。
func TestHandleFileMetaStillRejectsEmptyFile(t *testing.T) {
	a, _, _, peerID := newRecvFixture(t)
	sess := stubSession{peer: peerID}

	meta := protocol.FileMeta{
		JobID: "j0", FileID: "h", FileName: "empty.txt", ChunkSize: 1024,
		BatchID: "b1", RelPath: "照片/empty.txt",
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err == nil {
		t.Fatal("非目录条目且 total_chunks=0 必须被拒绝")
	}
}

// 恶意 rel_path 只让这一条失败：不写任何文件到接收目录之外，后续正常任务照收。
func TestHandleFileMetaBadRelPathFailsOnlyThatJob(t *testing.T) {
	a, repo, dir, peerID := newRecvFixture(t)
	sess := stubSession{peer: peerID}

	bad := protocol.FileMeta{
		JobID: "bad", FileID: "h", FileName: "evil.txt", FileSize: 1024,
		ChunkSize: 1024, TotalChunks: 1,
		BatchID: "b1", RelPath: "../../evil.txt",
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, bad)); err == nil {
		t.Fatal("穿越路径必须被拒绝")
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "evil.txt")); !os.IsNotExist(err) {
		t.Error("拒绝的路径不该在上级目录留下文件")
	}

	// 同一个批次里的下一条正常任务不受影响
	good := protocol.FileMeta{
		JobID: "good", FileID: "h2", FileName: "ok.txt", FileSize: 1024,
		ChunkSize: 1024, TotalChunks: 1,
		BatchID: "b1", RelPath: "照片/ok.txt",
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, good)); err != nil {
		t.Fatalf("同批的下一条正常任务必须不受影响: %v", err)
	}
	if _, err := repo.GetJob("good"); err != nil {
		t.Errorf("正常任务未落库: %v", err)
	}
}

// 同一批次里两个内容相同的文件必须各有各的落点。
//
// 续传键原本是 (peer, file_hash)：一张文件夹里两份一样的图/文档 file_hash
// 也相同，第二条会命中第一条并【继承它的临时文件与最终路径】—— 结果是
// 目录里少一个文件、另一个内容错位。
func TestHandleFileMetaSameHashInBatchKeepsSeparateTargets(t *testing.T) {
	a, repo, dir, peerID := newRecvFixture(t)
	sess := stubSession{peer: peerID}

	first := protocol.FileMeta{
		JobID: "dup-1", FileID: "same-hash", FileName: "a.txt", FileSize: 1024,
		ChunkSize: 1024, TotalChunks: 1,
		BatchID: "b1", RelPath: "照片/a.txt",
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, first)); err != nil {
		t.Fatal(err)
	}
	second := first
	second.JobID, second.RelPath = "dup-2", "照片/备份/a.txt"
	if err := a.HandleFileMeta(sess, metaFrame(t, second)); err != nil {
		t.Fatal(err)
	}

	j1, err := repo.GetJob("dup-1")
	if err != nil {
		t.Fatal(err)
	}
	j2, err := repo.GetJob("dup-2")
	if err != nil {
		t.Fatal(err)
	}
	if j1.LocalPath == j2.LocalPath {
		t.Fatalf("同 hash 的两个文件拿到了同一个落点：%q", j1.LocalPath)
	}
	if j2.TempPath == j1.TempPath {
		t.Errorf("第二个文件继承了第一个的临时文件：%q", j2.TempPath)
	}
	if want := filepath.Join(dir, "照片", "备份", "a.txt"); j2.LocalPath != want {
		t.Errorf("第二个文件落点 = %q，期望 %q", j2.LocalPath, want)
	}
}

// 目录条目不该参与分块处理：收到块只能忽略，不能 panic。
func TestHandleFileChunkIgnoresDirJob(t *testing.T) {
	a, _, _, peerID := newRecvFixture(t)
	sess := stubSession{peer: peerID}

	meta := protocol.FileMeta{
		JobID: "d9", FileName: "空的", ChunkSize: 1024,
		BatchID: "b1", RelPath: "照片/空的", IsDir: true,
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err != nil {
		t.Fatal(err)
	}

	chunk := protocol.FileChunk{JobID: "d9", Index: 0, Offset: 0, Data: []byte("x")}
	payload, err := protocol.EncodeFileChunk(chunk)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.HandleFileChunk(sess, protocol.New(protocol.TypeFileChunk, payload)); err != nil {
		t.Fatalf("目录条目收到块应当被忽略而不是报错: %v", err)
	}
}

// 单文件（没有批次字段）仍走老路径：扁平落盘 + 同名去重。
func TestHandleFileMetaSingleFileStillFlat(t *testing.T) {
	a, repo, dir, peerID := newRecvFixture(t)
	sess := stubSession{peer: peerID}

	meta := protocol.FileMeta{
		JobID: "s1", FileID: "h", FileName: "report.pdf", FileSize: 1024,
		ChunkSize: 1024, TotalChunks: 1,
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err != nil {
		t.Fatal(err)
	}
	job, err := repo.GetJob("s1")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(job.LocalPath) != dir {
		t.Errorf("单文件必须直接落在接收目录里：%q", job.LocalPath)
	}
	if job.BatchID != "" || job.RelPath != "" || job.IsDir {
		t.Errorf("单文件不该带批次字段：%+v", job)
	}
}
