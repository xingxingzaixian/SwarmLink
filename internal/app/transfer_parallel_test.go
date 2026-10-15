package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/store/mem"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// 同对端并行传输（D-11）的用例。
//
// 观测手法刻意统一：会话「发得出去、但永不回 ACK」，于是被启动的任务会停在
// 「等 FILE_META_ACK」并一直占着并发额度 —— 这样「同时有几条在跑」是确定性
// 可数的，不用跟 goroutine 的退出时机赛跑。

// newParallelFixture 造一个同一对端可并行的观测环境。budget<=0 表示用默认额度。
func newParallelFixture(t *testing.T, budget int) (*TransferApp, *stubTransferRepo, *brokenSession, identity.NodeID, *clock.Fake) {
	t.Helper()
	peerID := mustNodeID(t, testPeerHex)
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))

	// 目录里对端在线（值得一试），但会话查不到 → 走按需拨号那条路，
	// 拿到的就是我们这个「只收不回」的会话。
	peers := mem.NewPeers(fc)
	if err := peers.Upsert(peer.Peer{
		NodeID: peerID, State: peer.StateOnline, LastAddr: "127.0.0.1:9", LastSeen: fc.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	sess := &brokenSession{peer: peerID, failFrom: 1 << 30} // 从不失败，只记账
	repo := &stubTransferRepo{}
	a := NewTransferApp(identity.NodeID{}, repo, nil, deadConns{sess: sess}, peers, eventbus.New(), fc, nil)
	a.ChunkSize = 1024
	if budget > 0 {
		a.sendSem = make(chan struct{}, budget)
	}
	return a, repo, sess, peerID, fc
}

// queueJobs 造 n 条【同一对端】的到点等待任务（真文件，能走过本地检查）。
func queueJobs(t *testing.T, a *TransferApp, repo *stubTransferRepo, peerID identity.NodeID, ids ...string) []transfer.Job {
	t.Helper()
	dir := t.TempDir()
	jobs := make([]transfer.Job, 0, len(ids))
	for i, id := range ids {
		p := filepath.Join(dir, id+".bin")
		if err := os.WriteFile(p, bytes.Repeat([]byte{1}, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
		j := a.newWaitingJob(id, peerID, p, 4096)
		j.CreatedAt = a.clk.Now().Add(time.Duration(i) * time.Second)
		j.NextTryAt = a.clk.Now() // 立刻到点：本用例不考退避
		if err := repo.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, j)
	}
	return jobs
}

// drainAttempts 把停在「等 FILE_META_ACK」的尝试推过 10 秒超时并等它们退出。
//
// 不做这一步，它们手里的文件句柄会卡住 t.TempDir 的清理（Windows 上必现）。
// 顺带这也是「额度会随尝试结束归还」的观测点。
func drainAttempts(t *testing.T, a *TransferApp, fc *clock.Fake) {
	t.Helper()
	fc.Advance(11 * time.Second)
	waitUntil(t, 3*time.Second, "所有尝试超时退出并释放文件句柄", func() bool {
		return a.ActiveJobs() == 0
	})
}

// D-11 的核心行为：同一对端的多条任务可以同时跑 —— 上限是【全局额度】，
// 不再有「每对端一条」。
//
// 对照组就是这条用例本身：改动之前这里只会有一条进入网络阶段。
func TestStartJobsRunsSamePeerInParallel(t *testing.T) {
	a, repo, _, peerID, fc := newParallelFixture(t, 3)
	jobs := queueJobs(t, a, repo, peerID, "p-1", "p-2", "p-3")

	a.startJobs(context.Background(), jobs)

	waitUntil(t, 3*time.Second, "三条同对端任务同时进入网络阶段", func() bool {
		for _, j := range jobs {
			if a.getSender(j.JobID) == nil {
				return false
			}
		}
		return true
	})
	if n := a.ActiveJobs(); n != 3 {
		t.Errorf("活跃任务 = %d，期望 3", n)
	}
	drainAttempts(t, a, fc)
}

// 额度必须真的被归还。
//
// 一个泄漏的额度会让并发数一路掉到 0，表现是「前 N 条传完之后队列再也不动」
// —— 而且它只在真实使用中才暴露（单测若不数活跃数根本看不见）。
func TestSendSlotIsReturnedAfterAttempt(t *testing.T) {
	a, repo, _, peerID, fc := newParallelFixture(t, 2)
	jobs := queueJobs(t, a, repo, peerID, "r-1", "r-2", "r-3")

	a.startJobs(context.Background(), jobs)
	waitUntil(t, 3*time.Second, "额度 2 → 起两条", func() bool {
		return a.getSender("r-1") != nil && a.getSender("r-2") != nil
	})
	if a.getSender("r-3") != nil {
		t.Fatal("额度已满却起了第三条")
	}

	drainAttempts(t, a, fc) // 两条都超时退出 → 归还额度

	a.startJobs(context.Background(), jobs[2:])
	waitUntil(t, 3*time.Second, "归还后的额度能被下一条用上", func() bool {
		return a.getSender("r-3") != nil
	})
	drainAttempts(t, a, fc)
}

// 同一会话上多条任务的 ACK 必须各归各。
//
// 并行之后帧交错是常态：收件方按「500ms 或 32 块」的节奏回位图，两条任务的
// 位图会在同一条 TCP 流上互相穿插。路由靠的是帧里的 job_id，这条用例钉住它。
func TestChunkAcksRouteByJobID(t *testing.T) {
	peerID := mustNodeID(t, testPeerHex)
	a := NewTransferApp(identity.NodeID{}, &stubTransferRepo{}, nil, noConns{}, nil, eventbus.New(), clock.New(), nil)

	mkSender := func(jobID string) *sendJob {
		return &sendJob{
			job:     transfer.Job{JobID: jobID, PeerID: peerID},
			ackCh:   make(chan chunkAck, 4),
			stopped: make(chan struct{}),
		}
	}
	sjA, sjB := mkSender("job-a"), mkSender("job-b")
	a.putSender(sjA)
	a.putSender(sjB)

	bmA := transfer.NewChunkBitmap(8)
	bmA.Set(0)
	bmB := transfer.NewChunkBitmap(8)
	for i := 0; i < 4; i++ {
		bmB.Set(i)
	}

	deliver := func(jobID string, bm transfer.ChunkBitmap) {
		t.Helper()
		payload, err := protocol.EncodeJSON(protocol.FileChunkAck{
			JobID: jobID, Bitmap: bm.EncodeBase64(), CompletedChunks: bm.CompletedCount(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.HandleFileChunkAck(stubSession{peer: peerID},
			protocol.New(protocol.TypeFileChunkAck, payload)); err != nil {
			t.Fatalf("HandleFileChunkAck: %v", err)
		}
	}
	// 交错投递：先 a 后 b，再 a —— 模拟真实到达顺序
	deliver("job-a", bmA)
	deliver("job-b", bmB)
	deliver("job-a", bmA)

	if got := len(sjA.ackCh); got != 2 {
		t.Fatalf("job-a 收到 %d 帧，期望 2（帧按 job_id 分派）", got)
	}
	if got := len(sjB.ackCh); got != 1 {
		t.Fatalf("job-b 收到 %d 帧，期望 1", got)
	}
	if ck := <-sjA.ackCh; ck.bitmap.CompletedCount() != 1 {
		t.Errorf("job-a 的位图 = %d 块，期望 1", ck.bitmap.CompletedCount())
	}
	if ck := <-sjB.ackCh; ck.bitmap.CompletedCount() != 4 {
		t.Errorf("job-b 的位图 = %d 块，期望 4", ck.bitmap.CompletedCount())
	}
}

// 一条已经退出的任务不能把会话读循环卡住。
//
// 这条不是洁癖：读循环是整条会话共用的，卡住它等于把该对端上所有其它任务、
// 心跳与聊天一起堵死。并行之后同一会话上任务变多，这条路径才真正会被走到。
func TestAckDeliveryDoesNotBlockAfterSenderStops(t *testing.T) {
	peerID := mustNodeID(t, testPeerHex)
	a := NewTransferApp(identity.NodeID{}, &stubTransferRepo{}, nil, noConns{}, nil, eventbus.New(), clock.New(), nil)

	sj := &sendJob{
		job:     transfer.Job{JobID: "job-dead", PeerID: peerID},
		ackCh:   make(chan chunkAck, 1),
		stopped: make(chan struct{}),
	}
	sj.ackCh <- chunkAck{} // 缓冲区已满：消费者已经不在了
	sj.markStopped()
	a.putSender(sj) // 仍在表里（真实窗口：读循环已经拿到了它）

	// 位图必须是合法 base64：空位图会让处理函数在解码处就返回，
	// 那样这条用例就成了假绿灯（根本没走到投递那一步）。
	bm := transfer.NewChunkBitmap(8)
	bm.Set(0)
	payload, err := protocol.EncodeJSON(protocol.FileChunkAck{
		JobID: "job-dead", Bitmap: bm.EncodeBase64(), CompletedChunks: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.HandleFileChunkAck(stubSession{peer: peerID},
			protocol.New(protocol.TypeFileChunkAck, payload))
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("已退出任务的 ACK 投递卡住了调用方（会话读循环）")
	}
}

// 用户取消与调度之间有一个真实窗口：任务已经交给调度、还没跑到 sendFile 的
// 取消检查点。这条窗口里取消必须由发送路径落地，且【不能】顺手把整行抹掉 ——
// 文件名、批次 id 与相对路径都还在旧行里。
func TestCancelledPreStartJobKeepsRowFields(t *testing.T) {
	a, repo, sess, peerID, _ := newParallelFixture(t, 1)

	p := filepath.Join(t.TempDir(), "cancelled.bin")
	if err := os.WriteFile(p, bytes.Repeat([]byte{9}, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	j := a.newWaitingJob("job-cancel-pre-start", peerID, p, 4096)
	j.BatchID = "batch-cancel"
	j.RelPath = "照片/cancelled.bin"
	j.CreatedAt = a.clk.Now()
	j.NextTryAt = a.clk.Now()
	if err := repo.UpsertJob(j); err != nil {
		t.Fatal(err)
	}
	a.markCancelling(j.JobID)

	a.startJobs(context.Background(), []transfer.Job{j})

	waitUntil(t, 3*time.Second, "取消标记被兑现成终态行", func() bool {
		got, err := repo.GetJob(j.JobID)
		return err == nil && got.Status == transfer.StateCancelled
	})
	got, err := repo.GetJob(j.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.WaitReason != transfer.WaitNone {
		t.Errorf("waitReason = %q，取消的任务不该留在可自动拉起的状态", got.WaitReason)
	}
	if got.FileName == "" || got.BatchID == "" || got.RelPath == "" {
		t.Errorf("取消把行内字段抹掉了：fileName=%q batchID=%q relPath=%q",
			got.FileName, got.BatchID, got.RelPath)
	}
	// 取消检查点在哈希之前：被取消的任务一个字节都不该读、一帧都不该发
	if n := sess.sent(); n != 0 {
		t.Errorf("被取消的任务仍然发了 %d 帧", n)
	}
	if n := a.ActiveJobs(); n != 0 {
		t.Errorf("被取消的任务仍占着名额：ActiveJobs = %d", n)
	}
}

// 重试时源文件已经不在：这是真失败，但【不能】用一份零值行覆盖它 ——
// 那会让文件夹卡片失去相对路径、列表里只剩一个空文件名。
func TestFailedAttemptKeepsRowFieldsWhenLocalFileGone(t *testing.T) {
	peerID := mustNodeID(t, testPeerHex)
	repo := &stubTransferRepo{}
	a := NewTransferApp(identity.NodeID{}, repo, nil, noConns{}, nil, eventbus.New(), clock.New(), nil)
	a.ChunkSize = 1024

	missing := filepath.Join(t.TempDir(), "已经被删掉了.pdf")
	j := transfer.Job{
		JobID: "job-file-gone", PeerID: peerID, Direction: transfer.DirectionSend,
		FileName: "季度报告.pdf", BatchID: "batch-gone", RelPath: "照片/季度报告.pdf",
		FileSize: 4096, ChunkSize: 1024, TotalChunks: 4,
		Bitmap: transfer.NewChunkBitmap(4), LocalPath: missing,
		Status: transfer.StateIdle, WaitReason: transfer.WaitQueued,
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
	}
	if err := repo.UpsertJob(j); err != nil {
		t.Fatal(err)
	}

	if _, err := a.SendFileAs(context.Background(), peerID, missing, j.JobID); err == nil {
		t.Fatal("源文件不存在必须失败")
	}
	got, err := repo.GetJob(j.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != transfer.StateFailed {
		t.Errorf("status = %s，期望 failed", got.Status)
	}
	if got.Error == "" {
		t.Error("失败原因必须落库（界面靠它显示文案）")
	}
	if got.FileName == "" || got.BatchID == "" || got.RelPath == "" {
		t.Errorf("失败出口把行内字段抹掉了：fileName=%q batchID=%q relPath=%q",
			got.FileName, got.BatchID, got.RelPath)
	}
}
