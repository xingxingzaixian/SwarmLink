package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// 分类规则表：主判据是「对端可达性」，错误文本只做兜底。
//
// 反例比正例更重要 —— 把真失败误判成"等对方上线"会让用户永远等一个
// 不可能成功的任务，比当场报错糟糕得多。
func TestShouldWaitForPeer(t *testing.T) {
	dialErr := errors.New(`tcp: dial 192.168.1.9:41641: connectex: No connection could be made`)

	cases := []struct {
		name        string
		reachable   bool
		sessionLive bool
		err         error
		want        bool
	}{
		{"对端不在目录里 → 等", false, false, errors.New("transfer: no known address for peer abc"), true},
		{"对端不在目录里，哪怕错误看不懂 → 等", false, false, errors.New("some brand new error"), true},
		{"会话没了（对方掉线） → 等", true, false, errors.New("write tcp: connection reset by peer"), true},
		{"看起来可达但发不出去（拨号失败） → 等", true, true, dialErr, true},
		{"等 FILE_META_ACK 超时 → 等", true, true, errors.New("transfer: timeout waiting for FILE_META_ACK"), true},
		{"等 FILE_DONE_ACK 超时 → 等", true, true, errors.New("transfer: timeout waiting for FILE_DONE_ACK"), true},

		{"文件不存在 → 真失败", true, true, errors.New("transfer: open /tmp/gone.bin: no such file or directory"), false},
		{"校验失败 → 真失败", true, true, errors.New("transfer: peer verify failed: bad chunk 3"), false},
		{"元数据非法 → 真失败", true, true, errors.New("transfer: invalid meta (total_chunks=0)"), false},
		{"空文件 → 真失败", true, true, errors.New("transfer: empty file"), false},
		{"ctx 取消 → 真失败", true, true, fmt.Errorf("transfer: hash /tmp/a.bin: %w", errors.New("context canceled")), false},
		// 本地读文件读到一半文件被截断：看起来像 EOF，但绝不能当成"等对方上线"
		{"读文件时 unexpected EOF → 真失败", true, true, errors.New("read /tmp/a.bin: unexpected EOF"), false},
		{"nil 错误 → 不算等待（调用方只在 err != nil 时问）", false, false, nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldWaitForPeer(failureEvidence{
				PeerReachable: tc.reachable,
				SessionAlive:  tc.sessionLive,
				Err:           tc.err,
			})
			if got != tc.want {
				t.Errorf("shouldWaitForPeer = %v, want %v", got, tc.want)
			}
		})
	}
}

// 对方离线时点发送：立刻落一条「等待对方上线」的任务，不报错。
//
// 这里用 noConns + 空目录，对端一律不可达；「不哈希」由这份用例间接锁定：
// 等待路径根本不碰文件内容，因此 file_hash 必须还是空的。
func TestSendFileWhilePeerOfflineWaitsWithoutHashing(t *testing.T) {
	repo := &stubTransferRepo{}
	bus := eventbus.New()
	states := make(chan eventbus.TransferState, 4)
	bus.Subscribe(eventbus.TopicTransferState, func(p any) {
		if ev, ok := p.(eventbus.TransferState); ok {
			states <- ev
		}
	})
	bus.Subscribe(eventbus.TopicTransferError, func(p any) {
		ev, _ := p.(eventbus.TransferError)
		t.Errorf("等待不是失败，不该发 transfer:error（%v）", ev.Err)
	})

	a := NewTransferApp(identity.NodeID{}, repo, nil, noConns{}, nil, bus, clock.New(), nil)
	a.ChunkSize = 1024

	peerID := mustNodeID(t, testPeerHex)
	path := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte{7}, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	const jobID = "job-offline-send"
	job, err := a.SendFileAs(context.Background(), peerID, path, jobID)
	if err != nil {
		t.Fatalf("等待不该返回错误，得到 %v", err)
	}
	if job.Status != transfer.StateIdle {
		t.Errorf("status = %q, want %q（Idle 就是「还没开始」）", job.Status, transfer.StateIdle)
	}
	if job.WaitReason != transfer.WaitPeerOffline {
		t.Errorf("waitReason = %q, want %q", job.WaitReason, transfer.WaitPeerOffline)
	}
	if job.FileSize != 4096 {
		t.Errorf("fileSize = %d, want 4096（等待也要能显示进度口径）", job.FileSize)
	}
	if job.TotalChunks != 4 {
		t.Errorf("totalChunks = %d, want 4", job.TotalChunks)
	}
	if job.FileHash != "" {
		t.Errorf("还没开始传就不该有 file_hash，得到 %q（说明等待期读了文件内容）", job.FileHash)
	}
	if job.NextTryAt.IsZero() {
		t.Error("下次重试时间必须落下，否则 sweep 不会拾取它")
	}

	stored, err := repo.GetJob(jobID)
	if err != nil {
		t.Fatalf("等待任务必须落库（否则重启后丢失）: %v", err)
	}
	if stored.WaitReason != transfer.WaitPeerOffline {
		t.Errorf("落库的 waitReason = %q", stored.WaitReason)
	}

	select {
	case ev := <-states:
		if ev.To != string(transfer.StateIdle) || ev.Reason != string(transfer.WaitPeerOffline) {
			t.Errorf("state 事件 = %+v，期望 idle/peer_offline", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("进入等待必须发 transfer.state，否则界面永远显示「协商中」")
	}
}

// 真失败仍然必须是失败：本地文件不存在不该被"等待"吞掉。
func TestSendFileMissingFileStillFails(t *testing.T) {
	repo := &stubTransferRepo{}
	bus := eventbus.New()
	got := make(chan eventbus.TransferError, 1)
	bus.Subscribe(eventbus.TopicTransferError, func(p any) {
		if ev, ok := p.(eventbus.TransferError); ok {
			got <- ev
		}
	})
	a := NewTransferApp(identity.NodeID{}, repo, nil, noConns{}, nil, bus, clock.New(), nil)

	missing := filepath.Join(t.TempDir(), "nope.bin")
	if _, err := a.SendFileAs(context.Background(), mustNodeID(t, testPeerHex), missing, "job-missing"); err == nil {
		t.Fatal("文件不存在必须是错误")
	}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("失败路径必须发 transfer:error")
	}
	j, err := repo.GetJob("job-missing")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != transfer.StateFailed {
		t.Errorf("status = %q, want failed", j.Status)
	}
}
