package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// newCancelFixture 造一个「一拨就通」的发送环境。
//
// ackEagerSession 对文件帧只返回 nil（相当于发出去了），于是 sendFile 会按
// 「发完 META → 等 FILE_META_ACK」的正常节奏停下来 —— 这正是我们要取消的
// 状态：已经进入网络阶段、但还没传完。
func newCancelFixture(t *testing.T) (*TransferApp, *stubTransferRepo, identity.NodeID) {
	t.Helper()
	repo := &stubTransferRepo{}
	peerID := mustNodeID(t, testPeerHex)
	conns := newAckEagerConns(peerID)
	a := NewTransferApp(identity.NodeID{}, repo, nil, conns, nil, eventbus.New(), clock.New(), nil)
	a.ChunkSize = 1024
	return a, repo, peerID
}

func mkPayload(t *testing.T, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte{7}, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// 正在传输的任务被取消：必须是 cancelled，【不能】掉进「转等待」。
//
// 这是这个功能最不能出的问题：用户点了取消、对方一上线文件又自己跑起来。
func TestCancelRunningJobDoesNotBecomeWaiting(t *testing.T) {
	a, repo, peerID := newCancelFixture(t)
	path := mkPayload(t, 4096)
	const jobID = "job-cancel-running"

	done := make(chan transfer.Job, 1)
	go func() {
		j, _ := a.SendFileAs(context.Background(), peerID, path, jobID)
		done <- j
	}()

	waitUntil(t, 3*time.Second, "发送进入网络阶段（已登记 sender）", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.senders[jobID] != nil
	})

	if err := a.CancelJob(jobID); err != nil {
		t.Fatalf("正在传输的任务也必须能取消: %v", err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("取消后发送必须尽快退出（否则界面上的任务永远停在传输中）")
	}

	stored, err := repo.GetJob(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != transfer.StateCancelled {
		t.Errorf("status = %s，期望 cancelled（绝不能是 idle/failed）", stored.Status)
	}
	if stored.WaitReason != transfer.WaitNone {
		t.Errorf("waitReason = %q，期望空：取消过的任务绝不能被重新拉起", stored.WaitReason)
	}
	if !stored.NextTryAt.IsZero() {
		t.Errorf("nextTryAt = %v，期望零值（不再退避重试）", stored.NextTryAt)
	}

	// 对端上线也不能把它拉起来
	a.WakePeer(context.Background(), peerID)
	if j, err := repo.GetJob(jobID); err == nil && j.Status != transfer.StateCancelled {
		t.Errorf("取消过的任务被 WakePeer 拉起了：%s", j.Status)
	}
}

// 整体取消：排队的与在传的都要停，已结束的不动。
func TestCancelBatchCancelsAllUnfinished(t *testing.T) {
	a, repo, peerID := newCancelFixture(t)
	base := time.Unix(1, 0)

	mk := func(jobID, rel string, st transfer.State, reason transfer.WaitReason) transfer.Job {
		return transfer.Job{
			JobID: jobID, PeerID: peerID, ChunkSize: 1024, Direction: transfer.DirectionSend,
			Status: st, WaitReason: reason, BatchID: "b1", RelPath: rel,
			CreatedAt: base, UpdatedAt: base,
		}
	}
	queued := mk("q1", "照片/q1.txt", transfer.StateIdle, transfer.WaitQueued)
	finished := mk("f1", "照片/f1.txt", transfer.StateDone, transfer.WaitNone)
	// 在传的那条：先落一行（带上批次字段），再用同一个 job_id 真的发起发送
	// —— 这条路径同时验证了「批次字段在开始时被承接」。
	path := mkPayload(t, 4096)
	running := mk("r1", "照片/r1.txt", transfer.StateIdle, transfer.WaitQueued)
	running.LocalPath = path

	for _, j := range []transfer.Job{queued, finished, running} {
		if err := repo.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan transfer.Job, 1)
	go func() {
		j, _ := a.SendFileAs(context.Background(), peerID, path, "r1")
		done <- j
	}()
	waitUntil(t, 3*time.Second, "在传任务进入网络阶段", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.senders["r1"] != nil
	})

	n, err := a.CancelBatch("b1")
	if err != nil {
		t.Fatalf("CancelBatch: %v", err)
	}
	if n != 2 {
		t.Errorf("取消条数 = %d，期望 2（排队中的 + 在传的；已完成的不能算）", n)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("整体取消必须把在传的那条也停下来")
	}

	for _, tc := range []struct {
		jobID string
		want  transfer.State
	}{
		{"q1", transfer.StateCancelled},
		{"r1", transfer.StateCancelled},
		{"f1", transfer.StateDone}, // 已结束的不许被改写
	} {
		j, err := repo.GetJob(tc.jobID)
		if err != nil {
			t.Fatalf("%s 读不回来: %v", tc.jobID, err)
		}
		if j.Status != tc.want {
			t.Errorf("%s 状态 = %s，期望 %s", tc.jobID, j.Status, tc.want)
		}
	}
}

// 取消一个已经结束的批次：不报错、返回 0（卡片上的按钮可能被连点）。
func TestCancelBatchOnFinishedBatchIsNoop(t *testing.T) {
	a, repo, peerID := newCancelFixture(t)
	if err := repo.UpsertJob(transfer.Job{
		JobID: "done-1", PeerID: peerID, ChunkSize: 1024, Direction: transfer.DirectionSend,
		Status: transfer.StateDone, BatchID: "b-done", RelPath: "照片/a.txt",
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}

	n, err := a.CancelBatch("b-done")
	if err != nil {
		t.Fatalf("已结束的批次不该报错: %v", err)
	}
	if n != 0 {
		t.Errorf("取消条数 = %d，期望 0", n)
	}
	if j, _ := repo.GetJob("done-1"); j.Status != transfer.StateDone {
		t.Errorf("已完成的任务被改写了：%s", j.Status)
	}

	// 空 batch_id 必须被拒（否则会退化成「取消本机全部单文件任务」）
	if _, err := a.CancelBatch(""); err == nil {
		t.Error("空 batch_id 必须被拒绝")
	}
}
