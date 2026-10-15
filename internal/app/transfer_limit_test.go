package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// metaAckRecorder 记录接收端回出去的每一帧。
//
// 既有的 stubSession 把帧直接丢掉，而「拒收必须带上理由」这件事只能从
// 回出去的 FILE_META_ACK 里看到，所以这里单独造一个会记账的会话。
type metaAckRecorder struct {
	peer   identity.NodeID
	frames []protocol.Frame
}

func (s *metaAckRecorder) PeerID() identity.NodeID     { return s.peer }
func (s *metaAckRecorder) Send(f protocol.Frame) error { s.frames = append(s.frames, f); return nil }
func (s *metaAckRecorder) Recv() (protocol.Frame, error) {
	return protocol.Frame{}, errors.New("test: 不读")
}
func (s *metaAckRecorder) RTT() time.Duration { return 0 }
func (s *metaAckRecorder) Close() error       { return nil }

// lastMetaAck 取出本端最后一条 FILE_META_ACK。
func (s *metaAckRecorder) lastMetaAck(t *testing.T) protocol.FileMetaAck {
	t.Helper()
	for i := len(s.frames) - 1; i >= 0; i-- {
		if s.frames[i].Type != protocol.TypeFileMetaAck {
			continue
		}
		var ack protocol.FileMetaAck
		if err := protocol.DecodeJSON(s.frames[i].Payload, &ack); err != nil {
			t.Fatalf("FILE_META_ACK 解不开: %v", err)
		}
		return ack
	}
	t.Fatal("没有回 FILE_META_ACK")
	return protocol.FileMetaAck{}
}

// 超过接收上限的文件必须被【拒收】并回明确原因，且不建任务、不在接收目录留下任何东西。
//
// 「什么都没有留下」和「回了原因」同等重要：不落库是为了让列表里不出现一条
// 永远传不完的任务；不回原因则发送端只能干等到 10 秒超时，用户看到的是
// 「传输超时」而不是「对方设了上限」。
func TestHandleFileMetaRejectsFileOverLimit(t *testing.T) {
	a, repo, dir, peerID := newRecvFixture(t)
	a.MaxFileSize = 4096
	sess := &metaAckRecorder{peer: peerID}

	meta := protocol.FileMeta{
		JobID: "big", FileID: "h-big", FileName: "big.bin", FileSize: 8192,
		ChunkSize: 1024, TotalChunks: 8,
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err != nil {
		t.Fatalf("拒收不是协议错误，必须正常返回: %v", err)
	}

	ack := sess.lastMetaAck(t)
	if ack.Reject == "" {
		t.Fatal("超限文件必须回带 reject 的 FILE_META_ACK")
	}
	if !strings.Contains(ack.Reject, "上限") {
		t.Errorf("拒收理由要说清是上限问题，实际 = %q", ack.Reject)
	}

	if _, err := repo.GetJob("big"); err == nil {
		t.Error("被拒收的文件不该落库")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("被拒收的文件不该在接收目录留下任何痕迹：%v", entries)
	}
}

// 边界：正好等于上限的文件必须收下。
//
// 写成「>」而不是「>=」的理由：用户把上限设成 10GB，期待的是「可以发 10GB 的文件」，
// 而不是「10GB 差一个字节也发不了」。
func TestHandleFileMetaAcceptsFileAtLimit(t *testing.T) {
	a, repo, _, peerID := newRecvFixture(t)
	a.MaxFileSize = 4096
	sess := &metaAckRecorder{peer: peerID}

	meta := protocol.FileMeta{
		JobID: "edge", FileID: "h-edge", FileName: "edge.bin", FileSize: 4096,
		ChunkSize: 1024, TotalChunks: 4,
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err != nil {
		t.Fatalf("HandleFileMeta: %v", err)
	}
	if ack := sess.lastMetaAck(t); ack.Reject != "" {
		t.Errorf("正好等于上限的文件被拒了：%q", ack.Reject)
	}
	if _, err := repo.GetJob("edge"); err != nil {
		t.Errorf("接受的文件必须落库: %v", err)
	}
}

// 上限为 0（或未配置）表示不限制：老配置怎么跑，现在还是怎么跑。
func TestHandleFileMetaWithoutLimitAcceptsAnything(t *testing.T) {
	a, repo, _, peerID := newRecvFixture(t)
	sess := &metaAckRecorder{peer: peerID}

	meta := protocol.FileMeta{
		JobID: "nolimit", FileID: "h-nl", FileName: "huge.bin", FileSize: 1 << 40,
		ChunkSize: 1024, TotalChunks: transfer.ChunkCount(1<<40, 1024),
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err != nil {
		t.Fatalf("未配置上限时必须放行: %v", err)
	}
	if _, err := repo.GetJob("nolimit"); err != nil {
		t.Errorf("未配置上限时的文件必须落库: %v", err)
	}
}

// 对端拒收 → 发送侧必须【直接失败并说明原因】，绝不能转成「等待对方上线」。
//
// 转等待的后果有两个，都不能接受：界面上是一条永远转圈的任务；
// 而且每次退避到点都会把同一个注定被拒的文件重发一遍。
func TestSendFileAsFailsWhenPeerRejects(t *testing.T) {
	repo := &stubTransferRepo{}
	peerID := mustNodeID(t, testPeerHex)
	bus := eventbus.New()
	caught := make(chan eventbus.TransferError, 1)
	bus.Subscribe(eventbus.TopicTransferError, func(p any) {
		if ev, ok := p.(eventbus.TransferError); ok {
			select {
			case caught <- ev:
			default:
			}
		}
	})

	a := NewTransferApp(identity.NodeID{}, repo, nil, newAckEagerConns(peerID), nil, bus, clock.New(), nil)
	a.ChunkSize = 1024

	path := mkPayload(t, 4096)
	const jobID = "job-rejected"

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

	const reason = "文件超过接收上限（4.0 KB）"
	payload, err := protocol.EncodeJSON(protocol.FileMetaAck{JobID: jobID, Reject: reason})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.HandleFileMetaAck(stubSession{peer: peerID}, protocol.New(protocol.TypeFileMetaAck, payload)); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("拒收后发送必须尽快退出（否则界面要停到 10 秒超时）")
	}

	stored, err := repo.GetJob(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != transfer.StateFailed {
		t.Errorf("status = %s，期望 failed（被拒收不是「等一会儿」）", stored.Status)
	}
	if stored.WaitReason != transfer.WaitNone {
		t.Errorf("waitReason = %q，期望空：被拒收的任务绝不能被自动拉起", stored.WaitReason)
	}
	if !strings.Contains(stored.Error, "上限") {
		t.Errorf("失败原因要带上对端给的理由，实际 = %q", stored.Error)
	}

	select {
	case ev := <-caught:
		if ev.JobID != jobID {
			t.Errorf("transfer:error 的 jobId = %q，期望 %q", ev.JobID, jobID)
		}
	case <-time.After(time.Second):
		t.Fatal("拒收必须有 transfer:error 反馈，否则界面没有任何提示")
	}
}

// formatBytes 只服务人眼可读的拒绝理由，必须稳、不能出现 "0 B" 之外的怪值。
func TestFormatBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{4096, "4.0 KB"},
		{10 << 30, "10.0 GB"},
		{1536, "1.5 KB"},
	}
	for _, c := range cases {
		if got := formatBytes(c.in); got != c.want {
			t.Errorf("formatBytes(%d) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// 目标盘空间不足时必须【在落盘之前】拒收，并说清是空间问题。
//
// 「落盘之前」不是洁癖：先建 .part 再拒，接收目录里就多出一个永远不会被
// 写入的半个文件，而用户在传输列表里什么都看不到。
func TestHandleFileMetaRejectsWhenDiskIsFull(t *testing.T) {
	a, repo, dir, peerID := newRecvFixture(t)
	a.DiskFree = func(string) (uint64, error) { return 1024, nil }
	sess := &metaAckRecorder{peer: peerID}

	meta := protocol.FileMeta{
		JobID: "full", FileID: "h-full", FileName: "big.bin", FileSize: 8192,
		ChunkSize: 1024, TotalChunks: 8,
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err != nil {
		t.Fatalf("拒收不是协议错误，必须正常返回: %v", err)
	}

	ack := sess.lastMetaAck(t)
	if !strings.Contains(ack.Reject, "空间不足") {
		t.Fatalf("必须说清是空间不足，实际 = %q", ack.Reject)
	}
	if _, err := repo.GetJob("full"); err == nil {
		t.Error("空间不足被拒的文件不该落库")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("被拒的文件不该在接收目录留下任何痕迹：%v", entries)
	}
}

// 预检是「提前止损」，不是新的拒绝理由来源：探不到（网络盘、权限、路径不对）
// 一律放行。宁可让它传下去由写入失败报错，也不能因为一次探测失败就拒收。
func TestSpaceCheckFailsOpen(t *testing.T) {
	cases := []struct {
		name string
		free func(string) (uint64, error)
		want string
	}{
		{"空间充足", func(string) (uint64, error) { return 1 << 40, nil }, ""},
		{"探测失败", func(string) (uint64, error) { return 0, errors.New("探不到") }, ""},
		{"没接探测函数", nil, ""},
		{"空间不足", func(string) (uint64, error) { return 1, nil }, "空间不足"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, repo, _, peerID := newRecvFixture(t)
			a.DiskFree = c.free
			sess := &metaAckRecorder{peer: peerID}

			meta := protocol.FileMeta{
				JobID: "j1", FileID: "h1", FileName: "f.bin", FileSize: 4096,
				ChunkSize: 1024, TotalChunks: 4,
			}
			if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err != nil {
				t.Fatalf("HandleFileMeta: %v", err)
			}
			ack := sess.lastMetaAck(t)
			switch {
			case c.want == "" && ack.Reject != "":
				t.Errorf("本该放行却被拒：%q", ack.Reject)
			case c.want != "" && !strings.Contains(ack.Reject, c.want):
				t.Errorf("拒绝理由 = %q，期望含 %q", ack.Reject, c.want)
			}
			if c.want == "" {
				if _, err := repo.GetJob("j1"); err != nil {
					t.Errorf("放行的文件必须落库: %v", err)
				}
			}
		})
	}
}

// 续传只需要补剩下的部分。
//
// 已经躺在盘上的那 4KB 不能再算进「需要多少空间」，否则磁盘刚好够补完时
// 续传会被自己误判成空间不足 —— 而它恰恰是最该成功的那种情况。
func TestSpaceCheckCountsOnlyRemainingBytes(t *testing.T) {
	a, repo, dir, peerID := newRecvFixture(t)
	sess := &metaAckRecorder{peer: peerID}

	bm := transfer.NewChunkBitmap(8)
	for i := 0; i < 4; i++ {
		bm.Set(i)
	}
	if err := repo.UpsertJob(transfer.Job{
		JobID: "old", PeerID: peerID, FileHash: "h-resume", Direction: transfer.DirectionRecv,
		Status: transfer.StateTransferring, TempPath: filepath.Join(dir, "resume.bin.part"),
		LocalPath: filepath.Join(dir, "resume.bin"), ChunkSize: 1024, TotalChunks: 8,
		FileSize: 8192, Bitmap: bm, Completed: 4096,
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}

	// 盘上正好剩下 4KB 可用：够补完剩下的一半，必须放行
	a.DiskFree = func(string) (uint64, error) { return 4096, nil }
	meta := protocol.FileMeta{
		JobID: "resume", FileID: "h-resume", FileName: "resume.bin", FileSize: 8192,
		ChunkSize: 1024, TotalChunks: 8,
	}
	if err := a.HandleFileMeta(sess, metaFrame(t, meta)); err != nil {
		t.Fatalf("HandleFileMeta: %v", err)
	}
	if ack := sess.lastMetaAck(t); ack.Reject != "" {
		t.Errorf("续传只需补剩余的 4KB，不该被拒：%q", ack.Reject)
	}
}
