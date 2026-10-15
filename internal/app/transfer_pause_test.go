package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/filesink"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// ---------------------------------------------------------------------------
// 可控的对端
// ---------------------------------------------------------------------------

// pauseTestSession 是一个「可开关」的对端会话。
//
// 它会记录收到的每一帧，并在开着的时候把 META / 块 / DONE 全部确认掉。
// 之所以必须能关：只有先把它关掉，发送端才会停在「等 FILE_META_ACK」，
// 我们才有一个「正在传但没传完」的任务可以暂停；恢复之后又要它能确认，
// 才能断言「恢复 = 重走 META 后继续传完」。
type pauseTestSession struct {
	peer identity.NodeID

	mu      sync.Mutex
	app     *TransferApp // 两段式回填（会话要先于 TransferApp 构造）
	ackMeta bool
	frames  []protocol.Frame
	total   int
}

func (s *pauseTestSession) PeerID() identity.NodeID { return s.peer }
func (s *pauseTestSession) Recv() (protocol.Frame, error) {
	return protocol.Frame{}, errors.New("test: 不读")
}
func (s *pauseTestSession) RTT() time.Duration { return 0 }
func (s *pauseTestSession) Close() error       { return nil }

func (s *pauseTestSession) setAckMeta(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ackMeta = v
}

// count 数一数收到过多少条某类型的帧（判「恢复有没有真的重走 META」）。
func (s *pauseTestSession) count(typ byte) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, f := range s.frames {
		if f.Type == typ {
			n++
		}
	}
	return n
}

func (s *pauseTestSession) Send(f protocol.Frame) error {
	s.mu.Lock()
	s.frames = append(s.frames, f)
	switch f.Type {
	case protocol.TypeFileMeta:
		var meta protocol.FileMeta
		if err := protocol.DecodeJSON(f.Payload, &meta); err != nil {
			s.mu.Unlock()
			return err
		}
		s.total = meta.TotalChunks
		break
	case protocol.TypeFileChunk:
		// 逐块确认：位图里只有这一块，发送端据此判定「传完了」。
		// 块走的是二进制编码（不是 JSON），必须用配套的解码器。
		ck, err := protocol.DecodeFileChunk(f.Payload)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		bm := transfer.NewChunkBitmap(s.total)
		bm.Set(ck.Index)
		payload, err := protocol.EncodeJSON(protocol.FileChunkAck{
			JobID: ck.JobID, Bitmap: bm.EncodeBase64(), CompletedChunks: 1,
		})
		s.mu.Unlock()
		if err != nil {
			return err
		}
		return s.app.HandleFileChunkAck(s, protocol.New(protocol.TypeFileChunkAck, payload))
	}
	ackMeta := s.ackMeta
	s.mu.Unlock()

	switch f.Type {
	case protocol.TypeFileMeta:
		if !ackMeta {
			return nil // 吞掉：发送端停在「等 FILE_META_ACK」
		}
		var meta protocol.FileMeta
		if err := protocol.DecodeJSON(f.Payload, &meta); err != nil {
			return err
		}
		bm := transfer.NewChunkBitmap(meta.TotalChunks)
		payload, err := protocol.EncodeJSON(protocol.FileMetaAck{
			JobID: meta.JobID, CompletedBitmap: bm.EncodeBase64(),
		})
		if err != nil {
			return err
		}
		return s.app.HandleFileMetaAck(s, protocol.New(protocol.TypeFileMetaAck, payload))
	case protocol.TypeFileDone:
		var d protocol.FileDone
		if err := protocol.DecodeJSON(f.Payload, &d); err != nil {
			return err
		}
		payload, err := protocol.EncodeJSON(protocol.FileDoneAck{JobID: d.JobID, OK: true})
		if err != nil {
			return err
		}
		return s.app.HandleFileDoneAck(s, protocol.New(protocol.TypeFileDoneAck, payload))
	}
	return nil
}

// pauseTestConns 把同一个会话交给所有拨号/查表请求（单对端环境）。
type pauseTestConns struct{ sess ports.Session }

func (c pauseTestConns) Dial(context.Context, identity.NodeID, string) (ports.Session, error) {
	return c.sess, nil
}
func (c pauseTestConns) Accept(context.Context) (ports.Session, error) { return c.sess, nil }
func (c pauseTestConns) SessionOf(identity.NodeID) (ports.Session, bool) {
	return c.sess, true
}
func (c pauseTestConns) Broadcast(protocol.Frame, ...identity.NodeID) error { return nil }
func (c pauseTestConns) Close(identity.NodeID) error                        { return nil }

// newPauseSendFixture 造一个「可控对端」的发送环境（文件 4KB / 1KB 分块 = 4 块）。
func newPauseSendFixture(t *testing.T) (*TransferApp, *stubTransferRepo, *pauseTestSession, string) {
	t.Helper()
	repo := &stubTransferRepo{}
	peerID := mustNodeID(t, testPeerHex)
	sess := &pauseTestSession{peer: peerID}
	a := NewTransferApp(identity.NodeID{}, repo, nil, pauseTestConns{sess: sess}, nil,
		eventbus.New(), clock.New(), nil)
	a.ChunkSize = 1024
	sess.app = a

	path := filepath.Join(t.TempDir(), "pausable.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte{3}, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	return a, repo, sess, path
}

func mustFrame(t *testing.T, typ byte, v any) protocol.Frame {
	t.Helper()
	payload, err := protocol.EncodeJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.New(typ, payload)
}

// waitStatus 轮询到库里的状态变成 want。
//
// 自己写循环而不是复用 waitUntil：超时时必须把真实状态、错误与重试次数一起
// 打出来 —— 只报「等待超时」的话，排查者还得回去改测试才能知道卡在哪一步。
func waitStatus(t *testing.T, repo *stubTransferRepo, jobID string, want transfer.State, desc string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last transfer.Job
	for time.Now().Before(deadline) {
		if j, err := repo.GetJob(jobID); err == nil {
			last = j
			if j.Status == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s：状态 = %s（err=%q waitReason=%s retry=%d），期望 %s",
		desc, last.Status, last.Error, last.WaitReason, last.RetryCount, want)
}

// ---------------------------------------------------------------------------
// 发送侧：本机用户暂停 / 恢复
// ---------------------------------------------------------------------------

// 本机用户暂停一条正在发送的任务，再恢复：暂停必须真的停下（且不是失败），
// 恢复必须重走 META 之后把文件传完。
func TestPauseRunningSendThenResumeCompletes(t *testing.T) {
	a, repo, sess, path := newPauseSendFixture(t)
	errs := make(chan eventbus.TransferError, 4)
	a.bus.Subscribe(eventbus.TopicTransferError, func(p any) {
		if ev, ok := p.(eventbus.TransferError); ok {
			errs <- ev
		}
	})
	const jobID = "job-pause-resume"

	sess.setAckMeta(false) // 对端吞掉 META → 发送端停在网络阶段
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.SendFileAs(context.Background(), mustNodeID(t, testPeerHex), path, jobID)
	}()
	waitUntil(t, 3*time.Second, "发送进入网络阶段（已登记 sender）", func() bool {
		return a.getSender(jobID) != nil
	})

	if err := a.PauseJob(jobID); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	waitUntil(t, 3*time.Second, "暂停后发送 goroutine 干净退出", func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})

	stored, err := repo.GetJob(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != transfer.StatePaused {
		t.Errorf("status = %s，期望 paused", stored.Status)
	}
	if stored.WaitReason != transfer.WaitNone {
		t.Errorf("waitReason = %q，期望空：暂停的任务绝不能被自动拉起", stored.WaitReason)
	}
	if !stored.NextTryAt.IsZero() {
		t.Errorf("nextTryAt = %v，期望零值（暂停不参与退避重试）", stored.NextTryAt)
	}
	// 暂停不是失败：整行标红是错的反馈
	select {
	case ev := <-errs:
		t.Fatalf("暂停不该发 transfer:error（%v）", ev.Err)
	default:
	}
	// 但必须发状态事件，否则界面一直停在「传输中」
	if !sess.sentType(t, protocol.TypeFilePause) {
		t.Error("本机暂停必须通知对端（FILE_PAUSE）")
	}

	// 恢复：对端这次愿意确认
	sess.setAckMeta(true)
	metasBefore := sess.count(protocol.TypeFileMeta)
	if err := a.ResumeJob(context.Background(), jobID); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	if !sess.sentType(t, protocol.TypeFileResume) {
		t.Error("本机恢复必须通知对端（FILE_RESUME）")
	}
	waitStatus(t, repo, jobID, transfer.StateDone, "恢复后把文件传完")
	// 架构书 3.6：暂停后必须重新交换位图，不能拿旧位图接着发
	if n := sess.count(protocol.TypeFileMeta); n <= metasBefore {
		t.Errorf("恢复只发了 %d 次 FILE_META（暂停前 %d 次），必须重新协商位图",
			n, metasBefore)
	}
	// 终态要清掉暂停意图，否则同一个 job_id 的下一次尝试会被误判
	if a.isPausedByUser(jobID) || a.isPausedByPeer(jobID) {
		t.Error("任务完成后必须清掉暂停意图")
	}
}

// sentType 报告对端是否收到过某类型的帧。
func (s *pauseTestSession) sentType(t *testing.T, typ byte) bool {
	t.Helper()
	return s.count(typ) > 0
}

// 只能暂停「正在传」的任务：等待态与终态都必须被拒（否则与既有等待态语义打架）。
func TestPauseJobRefusesNonRunningJobs(t *testing.T) {
	repo := &stubTransferRepo{}
	peer := mustNodeID(t, testPeerHex)
	a := NewTransferApp(identity.NodeID{}, repo, nil, noConns{}, nil, eventbus.New(), clock.New(), nil)

	for _, tc := range []struct {
		name string
		st   transfer.State
	}{
		{"等待对方上线", transfer.StateIdle},
		{"批次排队中", transfer.StateIdle},
		{"已完成", transfer.StateDone},
		{"已取消", transfer.StateCancelled},
		{"校验中", transfer.StateVerifying},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobID := "job-" + tc.name
			if err := repo.UpsertJob(transfer.Job{
				JobID: jobID, PeerID: peer, Direction: transfer.DirectionSend,
				Status: tc.st, ChunkSize: 1024,
			}); err != nil {
				t.Fatal(err)
			}
			if err := a.PauseJob(jobID); err == nil {
				t.Fatalf("状态 %s 不该允许暂停", tc.st)
			}
		})
	}
	if err := a.PauseJob("不存在的任务"); err == nil {
		t.Error("不存在的任务必须报错")
	}
}

// 暂停的任务必须能取消：否则用户按下暂停之后就把这条任务永远留在列表里。
func TestCancelPausedSendJobSucceeds(t *testing.T) {
	a, repo, _, path := newPauseSendFixture(t)
	const jobID = "job-pause-then-cancel"

	if err := repo.UpsertJob(transfer.Job{
		JobID: jobID, PeerID: mustNodeID(t, testPeerHex), Direction: transfer.DirectionSend,
		Status: transfer.StatePaused, ChunkSize: 1024, LocalPath: path,
	}); err != nil {
		t.Fatal(err)
	}
	a.markPauseUser(jobID)

	if err := a.CancelJob(jobID); err != nil {
		t.Fatalf("暂停中的任务必须能取消: %v", err)
	}
	j, err := repo.GetJob(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != transfer.StateCancelled {
		t.Errorf("status = %s，期望 cancelled", j.Status)
	}
	if a.isPausedByUser(jobID) || a.isPausedByPeer(jobID) {
		t.Error("取消必须一并清掉暂停意图")
	}
}

// ---------------------------------------------------------------------------
// 发送侧：对端发起的暂停 / 恢复
// ---------------------------------------------------------------------------

// 接收方用户按了暂停：发送方必须停下，且本机用户此时点恢复要被拒绝
// （对方还按着，单方面恢复只会再被按一次）。
// 对方恢复后，本机必须自动继续 —— 用户不需要在两边各点一次。
func TestPeerPauseStopsSenderAndPeerResumeAutoContinues(t *testing.T) {
	a, repo, sess, path := newPauseSendFixture(t)
	const jobID = "job-peer-pause"

	sess.setAckMeta(false)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.SendFileAs(context.Background(), mustNodeID(t, testPeerHex), path, jobID)
	}()
	waitUntil(t, 3*time.Second, "发送进入网络阶段", func() bool {
		return a.getSender(jobID) != nil
	})

	// 对端（接收方）发来 FILE_PAUSE
	if err := a.HandleFilePause(sess, mustFrame(t, protocol.TypeFilePause, protocol.FilePause{JobID: jobID})); err != nil {
		t.Fatalf("HandleFilePause: %v", err)
	}
	waitUntil(t, 3*time.Second, "对端暂停后发送必须停下", func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	waitStatus(t, repo, jobID, transfer.StatePaused, "落成已暂停")

	if err := a.ResumeJob(context.Background(), jobID); err == nil {
		t.Error("对方还按着的时候，本机恢复必须被拒绝")
	}

	// 对端恢复 → 本机自动继续
	sess.setAckMeta(true)
	if err := a.HandleFileResume(sess, mustFrame(t, protocol.TypeFileResume, protocol.FileResume{JobID: jobID})); err != nil {
		t.Fatalf("HandleFileResume: %v", err)
	}
	waitStatus(t, repo, jobID, transfer.StateDone, "对端恢复后自动继续并传完")
}

// 对端在【算哈希】那段窗口里按下暂停：那时还没有 sendJob 可以收信号，
// 意图只能记在 pausedByPeer 上 —— 但哈希结束后、META 之前必须兑现它。
func TestPeerPauseDuringHashIsHonored(t *testing.T) {
	a, repo, sess, path := newPauseSendFixture(t)
	const jobID = "job-pause-during-hash"

	// 模拟「对端的 FILE_PAUSE 先到、而本机还在算哈希」：直接记意图
	a.markPausePeer(jobID)
	sess.setAckMeta(true) // 对端本来会确认：不兑现暂停的话这次会一路传完

	_, _ = a.SendFileAs(context.Background(), mustNodeID(t, testPeerHex), path, jobID)

	j, err := repo.GetJob(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != transfer.StatePaused {
		t.Errorf("status = %s，期望 paused（哈希期间的对端暂停必须被兑现）", j.Status)
	}
	if n := sess.count(protocol.TypeFileMeta); n != 0 {
		t.Errorf("被暂停的任务不该发出 FILE_META，实际发了 %d 条", n)
	}
}

// ---------------------------------------------------------------------------
// 接收侧
// ---------------------------------------------------------------------------

// newPauseRecvFixture 造接收侧环境：真 filesink（临时目录）+ 记账会话。
func newPauseRecvFixture(t *testing.T) (*TransferApp, *stubTransferRepo, *metaAckRecorder, identity.NodeID) {
	t.Helper()
	repo := &stubTransferRepo{}
	dir := t.TempDir()
	sink, err := filesink.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	peerID := mustNodeID(t, testPeerHex)
	sess := &metaAckRecorder{peer: peerID}
	// 会话必须挂在连接管理器上：接收侧的暂停/恢复信令要发给发送方，
	// 真实环境里那条会话一定在（帧刚从它上面到达），fixture 照做。
	a := NewTransferApp(identity.NodeID{}, repo, sink, pauseTestConns{sess: sess}, nil,
		eventbus.New(), clock.New(), nil)
	a.ChunkSize = 1024
	// 每块都回 ACK：控制信令里「暂停期间不许回 ACK」这条只有让 ACK 必然出现
	// 才有意义（默认每 32 块才回一次，小文件上永远看不到 ACK，断言会空过）。
	a.AckEveryChunks = 1
	// 接收端在传输期间持有临时文件句柄（Windows 上不关掉就删不了临时目录）
	t.Cleanup(func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, rj := range a.receivers {
			if rj.f != nil {
				_ = rj.f.Close()
			}
		}
	})
	return a, repo, sess, peerID
}

// feedChunk 喂一个块进接收路径。
func feedChunk(t *testing.T, a *TransferApp, sess ports.Session, jobID string, index int) {
	t.Helper()
	payload, err := protocol.EncodeFileChunk(protocol.FileChunk{
		JobID: jobID, Index: index, Offset: int64(index) * 1024, Data: make([]byte, 1024),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.HandleFileChunk(sess, protocol.New(protocol.TypeFileChunk, payload)); err != nil {
		t.Fatalf("HandleFileChunk(%d): %v", index, err)
	}
}

func hasType(frames []protocol.Frame, typ byte) bool {
	for _, f := range frames {
		if f.Type == typ {
			return true
		}
	}
	return false
}

// 暂停一条接收中的任务：状态落 paused、通知发送方停发、
// 而且【暂停期间到达的块与 DONE 都要丢掉】—— 不丢的话位图会在界面写着
// 「已暂停」的时候悄悄前进。
func TestPauseRecvJobDropsChunksAndNotifiesSender(t *testing.T) {
	a, repo, sess, _ := newPauseRecvFixture(t)
	const jobID = "recv-pause"
	meta := protocol.FileMeta{
		JobID: jobID, FileID: "h-recv-pause", FileName: "f.bin",
		FileSize: 4096, ChunkSize: 1024, TotalChunks: 4,
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err != nil {
		t.Fatalf("HandleFileMeta: %v", err)
	}

	// 前置条件：正常路径能收块
	feedChunk(t, a, sess, jobID, 0)
	rj := a.getReceiver(jobID)
	if rj == nil || rj.bitmap.CompletedCount() != 1 {
		t.Fatalf("前置条件不成立：第一块应已写入，实际 %v", rj)
	}

	if err := a.PauseJob(jobID); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	if j, err := repo.GetJob(jobID); err != nil || j.Status != transfer.StatePaused {
		t.Fatalf("status = %v，期望 paused", j.Status)
	}
	if !hasType(sess.frames, protocol.TypeFilePause) {
		t.Error("暂停入站传输必须给发送方发 FILE_PAUSE")
	}

	framesBefore := len(sess.frames)
	feedChunk(t, a, sess, jobID, 1)
	if rj.bitmap.CompletedCount() != 1 {
		t.Errorf("暂停期间不该写入新块，位图已完成 %d 块", rj.bitmap.CompletedCount())
	}
	// 暂停期间也不该回 ACK：回 ACK 就是告诉发送方「我收下了」
	if hasType(sess.frames[framesBefore:], protocol.TypeFileChunkAck) {
		t.Error("暂停期间不该回 FILE_CHUNK_ACK")
	}
	// 暂停期间收到 FILE_DONE：忽略，否则会拿半截文件去校验
	if err := a.HandleFileDone(sess, mustFrame(t, protocol.TypeFileDone, protocol.FileDone{JobID: jobID})); err != nil {
		t.Fatalf("HandleFileDone: %v", err)
	}
	if j, _ := repo.GetJob(jobID); j.Status != transfer.StatePaused {
		t.Errorf("暂停期间收到 FILE_DONE 不该改动状态，实际 %s", j.Status)
	}
}

// 暂停时必须把内存里的进度一起带进库。
//
// 接收侧的进度有【两条写入路径】：SaveBitmap（位图 + 已完成数）与
// UpsertJob（整行）。暂停只改状态，若直接 UpsertJob(rj.job)，就会用
// rj.job 里那份「要到 DONE 才填满」的快照把进度覆盖成零 ——
// 用户按下暂停，进度条先归零。这条是从 e2e 抓回来的回归。
func TestPauseRecvJobKeepsProgress(t *testing.T) {
	a, repo, sess, _ := newPauseRecvFixture(t)
	const jobID = "recv-keep-progress"
	meta := protocol.FileMeta{
		JobID: jobID, FileID: "h-keep-progress", FileName: "f.bin",
		FileSize: 4096, ChunkSize: 1024, TotalChunks: 4,
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err != nil {
		t.Fatal(err)
	}
	feedChunk(t, a, sess, jobID, 0)
	feedChunk(t, a, sess, jobID, 1)

	if err := a.PauseJob(jobID); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	j, err := repo.GetJob(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != transfer.StatePaused {
		t.Fatalf("status = %s，期望 paused", j.Status)
	}
	if j.Completed != 2 {
		t.Errorf("暂停把已完成块数冲掉了：%d，期望 2", j.Completed)
	}
	if got := j.Bitmap.CompletedCount(); got != 2 {
		t.Errorf("暂停把位图冲掉了：%d 块，期望 2（下次续传要靠它）", got)
	}

	// 恢复同样不能把进度冲掉
	if err := a.ResumeJob(context.Background(), jobID); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	if j, _ := repo.GetJob(jobID); j.Completed != 2 || j.Bitmap.CompletedCount() != 2 {
		t.Errorf("恢复把进度冲掉了：completed=%d bitmap=%d，期望都是 2",
			j.Completed, j.Bitmap.CompletedCount())
	}
}

// 恢复接收中的任务：重新收块、并通知发送方继续。
func TestResumeRecvJobAcceptsChunksAgain(t *testing.T) {
	a, repo, sess, _ := newPauseRecvFixture(t)
	const jobID = "recv-resume"
	meta := protocol.FileMeta{
		JobID: jobID, FileID: "h-recv-resume", FileName: "f.bin",
		FileSize: 4096, ChunkSize: 1024, TotalChunks: 4,
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err != nil {
		t.Fatal(err)
	}
	feedChunk(t, a, sess, jobID, 0)
	if err := a.PauseJob(jobID); err != nil {
		t.Fatal(err)
	}

	if err := a.ResumeJob(context.Background(), jobID); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	if j, _ := repo.GetJob(jobID); j.Status != transfer.StateTransferring {
		t.Errorf("status = %s，期望 transferring（接收侧恢复不需要重走 META）", j.Status)
	}
	if !hasType(sess.frames, protocol.TypeFileResume) {
		t.Error("恢复入站传输必须给发送方发 FILE_RESUME")
	}

	framesBefore := len(sess.frames)
	feedChunk(t, a, sess, jobID, 1)
	rj := a.getReceiver(jobID)
	if rj.bitmap.CompletedCount() != 2 {
		t.Errorf("恢复后必须继续写盘，位图已完成 %d 块，期望 2", rj.bitmap.CompletedCount())
	}
	if !hasType(sess.frames[framesBefore:], protocol.TypeFileChunkAck) {
		t.Error("恢复后必须恢复回 ACK")
	}
}

// 发送方在算哈希期间（我们这边已按暂停）又发来 META：不能把用户的暂停复活。
//
// 这是最容易出错的一条：META 走的是「重建接收任务」的正常路径，若不显式
// 保留暂停状态，用户按下暂停之后会看到文件自己开始下载。
func TestHandleFileMetaKeepsUserPausedReceiver(t *testing.T) {
	a, repo, sess, _ := newPauseRecvFixture(t)
	const jobID = "recv-meta-while-paused"
	meta := protocol.FileMeta{
		JobID: jobID, FileID: "h-keep", FileName: "f.bin",
		FileSize: 4096, ChunkSize: 1024, TotalChunks: 4,
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err != nil {
		t.Fatal(err)
	}
	feedChunk(t, a, sess, jobID, 0)
	if err := a.PauseJob(jobID); err != nil {
		t.Fatal(err)
	}

	// 发送方（它的用户点了恢复）重走 META
	framesBefore := len(sess.frames)
	if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err != nil {
		t.Fatalf("HandleFileMeta（暂停期间）: %v", err)
	}

	if j, _ := repo.GetJob(jobID); j.Status != transfer.StatePaused {
		t.Errorf("status = %s，期望仍是 paused（META 不该复活用户的暂停）", j.Status)
	}
	sent := sess.frames[framesBefore:]
	if !hasType(sent, protocol.TypeFileMetaAck) {
		t.Error("仍必须回答 FILE_META_ACK：发送方要靠它定位续传起点")
	}
	if !hasType(sent, protocol.TypeFilePause) {
		t.Error("必须补一条 FILE_PAUSE，让发送方一起停下")
	}
	// 位图不能被重建清零
	rj := a.getReceiver(jobID)
	if rj == nil || rj.bitmap.CompletedCount() != 1 {
		t.Errorf("暂停期间重建了接收任务，位图进度丢了")
	}
}

// 发送方恢复（FILE_RESUME）而本机用户也按着暂停：只有用户自己能解。
func TestPeerResumeDoesNotOverrideLocalPause(t *testing.T) {
	a, repo, sess, _ := newPauseRecvFixture(t)
	const jobID = "recv-local-wins"
	meta := protocol.FileMeta{
		JobID: jobID, FileID: "h-local-wins", FileName: "f.bin",
		FileSize: 4096, ChunkSize: 1024, TotalChunks: 4,
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err != nil {
		t.Fatal(err)
	}
	if err := a.PauseJob(jobID); err != nil {
		t.Fatal(err)
	}

	if err := a.HandleFileResume(sess, mustFrame(t, protocol.TypeFileResume, protocol.FileResume{JobID: jobID})); err != nil {
		t.Fatal(err)
	}
	if j, _ := repo.GetJob(jobID); j.Status != transfer.StatePaused {
		t.Errorf("status = %s，期望仍是 paused（对方的恢复不能解本机用户的暂停）", j.Status)
	}

	// 用户自己点恢复 → 才真的回到传输中
	if err := a.ResumeJob(context.Background(), jobID); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	if j, _ := repo.GetJob(jobID); j.Status != transfer.StateTransferring {
		t.Errorf("status = %s，期望 transferring", j.Status)
	}
}

// 对端发来暂停（发送方用户按的）：接收侧必须跟着停下，且不能覆盖本机用户的暂停。
func TestPeerPausePausesReceiver(t *testing.T) {
	a, repo, sess, _ := newPauseRecvFixture(t)
	const jobID = "recv-peer-pause"
	meta := protocol.FileMeta{
		JobID: jobID, FileID: "h-peer-pause", FileName: "f.bin",
		FileSize: 4096, ChunkSize: 1024, TotalChunks: 4,
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err != nil {
		t.Fatal(err)
	}

	if err := a.HandleFilePause(sess, mustFrame(t, protocol.TypeFilePause, protocol.FilePause{JobID: jobID})); err != nil {
		t.Fatalf("HandleFilePause: %v", err)
	}
	if j, _ := repo.GetJob(jobID); j.Status != transfer.StatePaused {
		t.Errorf("status = %s，期望 paused", j.Status)
	}
	rj := a.getReceiver(jobID)
	feedChunk(t, a, sess, jobID, 0)
	if rj.bitmap.CompletedCount() != 0 {
		t.Error("对端暂停后不该继续写盘")
	}

	// 本机用户此时点恢复：对端还按着 → 必须被拒，而不是偷偷传起来
	if err := a.ResumeJob(context.Background(), jobID); err == nil {
		t.Error("对方暂停时本机恢复必须被拒绝")
	}

	// 对端恢复 → 自动继续
	if err := a.HandleFileResume(sess, mustFrame(t, protocol.TypeFileResume, protocol.FileResume{JobID: jobID})); err != nil {
		t.Fatal(err)
	}
	if j, _ := repo.GetJob(jobID); j.Status != transfer.StateTransferring {
		t.Errorf("status = %s，期望 transferring（对端恢复后自动继续）", j.Status)
	}
}

// 恢复一个【已经不在传输中】的任务必须被拒：状态守卫不能被绕过。
func TestResumeJobRefusesNonPausedJobs(t *testing.T) {
	repo := &stubTransferRepo{}
	peer := mustNodeID(t, testPeerHex)
	a := NewTransferApp(identity.NodeID{}, repo, nil, noConns{}, nil, eventbus.New(), clock.New(), nil)
	for _, st := range []transfer.State{transfer.StateIdle, transfer.StateTransferring, transfer.StateDone, transfer.StateCancelled} {
		jobID := "job-resume-" + string(st)
		if err := repo.UpsertJob(transfer.Job{
			JobID: jobID, PeerID: peer, Direction: transfer.DirectionSend,
			Status: st, ChunkSize: 1024, LocalPath: "x.bin",
		}); err != nil {
			t.Fatal(err)
		}
		if err := a.ResumeJob(context.Background(), jobID); err == nil {
			t.Errorf("状态 %s 不该允许恢复", st)
		}
	}
}
