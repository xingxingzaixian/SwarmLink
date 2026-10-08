package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/store/mem"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// 启动收尾：崩溃残留的 active 任务按方向分别处理 ——
// 发送侧转入等待（随后会自动续传），接收侧标失败。
//
// 为什么不把接收侧也转入等待：接收侧的续传起点由【发送方】的下一轮
// FILE_META 驱动（FindResumable 只排除 done/cancelled，failed 行照样被继承），
// 主动去连对方反而多一条没必要的状态机分支。
func TestRecoverHandlesLeftoverActiveJobs(t *testing.T) {
	peerID := mustNodeID(t, testPeerHex)
	repo := &stubTransferRepo{}
	bus := eventbus.New()
	errs := make(chan eventbus.TransferError, 4)
	bus.Subscribe(eventbus.TopicTransferError, func(p any) {
		if ev, ok := p.(eventbus.TransferError); ok {
			errs <- ev
		}
	})

	// noConns + 空目录：发送侧转入等待后立刻重试也会马上回到等待
	a := NewTransferApp(identity.NodeID{}, repo, nil, noConns{}, nil, bus, clock.New(), nil)
	a.ChunkSize = 1024

	p := filepath.Join(t.TempDir(), "zombie.bin")
	if err := os.WriteFile(p, bytes.Repeat([]byte{5}, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	sendJob := transfer.Job{
		JobID: "job-send-zombie", PeerID: peerID, Direction: transfer.DirectionSend,
		Status: transfer.StateTransferring, LocalPath: p, FileName: "zombie.bin",
		ChunkSize: 1024, TotalChunks: 4, FileSize: 4096, RetryCount: 2,
	}
	recvJob := transfer.Job{
		JobID: "job-recv-zombie", PeerID: peerID, Direction: transfer.DirectionRecv,
		Status: transfer.StateTransferring, LocalPath: "/tmp/in.bin", FileName: "in.bin",
		ChunkSize: 1024, TotalChunks: 4, FileSize: 4096,
	}
	for _, j := range []transfer.Job{sendJob, recvJob} {
		if err := repo.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}

	a.Recover(context.Background())

	gotSend, err := repo.GetJob("job-send-zombie")
	if err != nil {
		t.Fatal(err)
	}
	if gotSend.Status != transfer.StateIdle || gotSend.WaitReason != transfer.WaitPeerOffline {
		t.Errorf("发送侧残留 = %q/%q，期望 idle/peer_offline", gotSend.Status, gotSend.WaitReason)
	}
	if gotSend.RetryCount <= sendJob.RetryCount {
		t.Errorf("发送侧残留的尝试次数没有递增：%d → %d", sendJob.RetryCount, gotSend.RetryCount)
	}

	gotRecv, err := repo.GetJob("job-recv-zombie")
	if err != nil {
		t.Fatal(err)
	}
	if gotRecv.Status != transfer.StateFailed {
		t.Errorf("接收侧残留 = %q，期望 failed", gotRecv.Status)
	}
	if gotRecv.Error == "" {
		t.Error("接收侧残留要写清原因，否则界面只有一条光秃秃的「失败」")
	}

	select {
	case <-errs:
	case <-time.After(time.Second):
		t.Fatal("接收侧残留必须有失败反馈（否则界面还停在「传输中」）")
	}
}

// 重启后等待中的任务要立刻试一轮：上次退出期间对端可能已经上线，
// 干等下一个事件（或下一个节拍）是没必要的延迟。
func TestRecoverStartsWaitingJobs(t *testing.T) {
	peerID := mustNodeID(t, testPeerHex)
	repo := &stubTransferRepo{}
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))

	peers := mem.NewPeers(fc)
	if err := peers.Upsert(peer.Peer{
		NodeID: peerID, State: peer.StateOnline, LastAddr: "127.0.0.1:9", LastSeen: fc.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	sess := &brokenSession{
		peer: peerID, failFrom: 1, err: errors.New("use of closed network connection"),
	}
	a := NewTransferApp(identity.NodeID{}, repo, nil, deadConns{sess: sess}, peers,
		eventbus.New(), fc, nil)
	a.ChunkSize = 1024

	p := filepath.Join(t.TempDir(), "w.bin")
	if err := os.WriteFile(p, bytes.Repeat([]byte{3}, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	j := a.newWaitingJob("job-wait-recover", peerID, p, 2048)
	j.NextTryAt = fc.Now().Add(time.Hour) // 退避还很久：重启也该立刻试一次
	if err := repo.UpsertJob(j); err != nil {
		t.Fatal(err)
	}

	a.Recover(context.Background())
	waitUntil(t, 2*time.Second, "恢复后立刻发起一次尝试", func() bool {
		return countAttempted(repo) == 1
	})
}
