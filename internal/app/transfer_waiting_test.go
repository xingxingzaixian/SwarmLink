package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/store/mem"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
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

// ---------- 「传一半掉线」用的假连接与轮询辅助 ----------

// brokenSession 从第 failFrom 帧起开始失败：用来模拟「传一半对方掉线」。
//
// 为什么不复用现成的 stubSession：它永远成功，而这条用例要的正是
// 「走到发块循环之后才开始失败」—— 元数据阶段就失败是另一条路径
// （等 FILE_META_ACK 超时），覆盖不到 runSender 里的写失败。
type brokenSession struct {
	peer     identity.NodeID
	failFrom int
	err      error

	mu    sync.Mutex
	sends int
}

func (s *brokenSession) PeerID() identity.NodeID { return s.peer }

func (s *brokenSession) Send(protocol.Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends++
	if s.sends >= s.failFrom {
		return s.err
	}
	return nil
}

func (s *brokenSession) Recv() (protocol.Frame, error) {
	return protocol.Frame{}, errors.New("test: 不读")
}
func (s *brokenSession) RTT() time.Duration { return 0 }
func (s *brokenSession) Close() error       { return nil }

func (s *brokenSession) sent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sends
}

// deadConns：Dial 仍能给出会话（第一次拨号是成功的），但 SessionOf 恒 false ——
// 等价于「会话已经不在管理器里了」，也就是对方掉线之后的样子。
type deadConns struct{ sess ports.Session }

func (c deadConns) Dial(context.Context, identity.NodeID, string) (ports.Session, error) {
	return c.sess, nil
}
func (c deadConns) Accept(context.Context) (ports.Session, error) { return c.sess, nil }
func (c deadConns) SessionOf(identity.NodeID) (ports.Session, bool) {
	return nil, false
}
func (c deadConns) Broadcast(protocol.Frame, ...identity.NodeID) error { return nil }
func (c deadConns) Close(identity.NodeID) error                        { return nil }

// waitUntil 轮询直到条件成立；超时给出可读的失败原因（不靠 sleep 猜时序）。
func waitUntil(t *testing.T, d time.Duration, desc string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", desc)
}

// 传一半对方掉线：任务必须转入等待（而不是 failed），且不报错。
func TestMidTransferDisconnectWaitsForPeer(t *testing.T) {
	peerID := mustNodeID(t, testPeerHex)
	repo := &stubTransferRepo{}
	bus := eventbus.New()
	states := make(chan eventbus.TransferState, 8)
	bus.Subscribe(eventbus.TopicTransferState, func(p any) {
		if ev, ok := p.(eventbus.TransferState); ok {
			states <- ev
		}
	})
	bus.Subscribe(eventbus.TopicTransferError, func(p any) {
		ev, _ := p.(eventbus.TransferError)
		t.Errorf("等待不是失败，不该发 transfer:error（%v）", ev.Err)
	})

	clk := clock.New()
	// 目录里对端仍显示在线：模拟的正是「目录还没更新，但连接已经断了」
	peers := mem.NewPeers(clk)
	err := peers.Upsert(peer.Peer{
		NodeID: peerID, State: peer.StateOnline, LastAddr: "127.0.0.1:9", LastSeen: clk.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	sess := &brokenSession{
		peer:     peerID,
		failFrom: 3, // 第 1 帧 FILE_META 通过，之后开始失败
		err:      errors.New("write tcp: use of closed network connection"),
	}

	a := NewTransferApp(identity.NodeID{}, repo, nil, deadConns{sess: sess}, peers, bus, clk, nil)
	a.ChunkSize = 1024

	path := filepath.Join(t.TempDir(), "half.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte{9}, 8*1024), 0o644); err != nil {
		t.Fatal(err)
	}

	const jobID = "job-mid-drop"
	go func() {
		_, _ = a.SendFileAs(context.Background(), peerID, path, jobID)
	}()

	// 等发送侧发出 FILE_META 并登记任务，再喂一个空位图 ACK 让它进入发块循环
	waitUntil(t, 2*time.Second, "发送侧登记并发出 FILE_META", func() bool {
		return a.getSender(jobID) != nil
	})
	bm := transfer.NewChunkBitmap(transfer.ChunkCount(8*1024, a.ChunkSize))
	payload, err := protocol.EncodeJSON(protocol.FileMetaAck{
		JobID: jobID, CompletedBitmap: bm.EncodeBase64(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.HandleFileMetaAck(sess, protocol.New(protocol.TypeFileMetaAck, payload)); err != nil {
		t.Fatal(err)
	}

	// 发块循环里写失败 → 转入等待
	waitUntil(t, 3*time.Second, "任务转入等待对方上线", func() bool {
		j, err := repo.GetJob(jobID)
		return err == nil && j.Status == transfer.StateIdle && j.WaitReason == transfer.WaitPeerOffline
	})
	if got := sess.sent(); got < 3 {
		t.Errorf("只发了 %d 帧就被判定掉线，说明没有真的走到发块阶段", got)
	}

	select {
	case ev := <-states:
		if ev.Reason != string(transfer.WaitPeerOffline) {
			t.Errorf("state 事件 reason = %q，期望 peer_offline", ev.Reason)
		}
	default:
		t.Fatal("转入等待必须发 transfer.state")
	}
}

// countAttempted 数一数「真的尝试过」的任务。
//
// 观测点选 RetryCount 而不是 Status：尝试次数只在 markWaiting 里递增
// （newWaitingJob 从 0 起步），因此 >=1 就说明这次尝试真的发生过。
// 而 Status 在尝试期间是 StateMetaExchange，跑完又会回到 StateIdle ——
// 拿它当判据要跟时序赛跑。
func countAttempted(repo *stubTransferRepo) int {
	n := 0
	for _, j := range repo.all() {
		if j.RetryCount >= 1 {
			n++
		}
	}
	return n
}

// sweep 只挑「到点」的等待任务，且同一对端每轮只启动一个。
//
// 后半句是硬要求：对方一上线，10 个等待任务同时开始整文件哈希，
// 磁盘会先崩。
func TestSweepWaitingRespectsBackoffAndPerPeerSerialization(t *testing.T) {
	peerID := mustNodeID(t, testPeerHex)
	repo := &stubTransferRepo{}
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))

	// 目录里对端显示在线（所以「值得一试」），但会话查不到：
	// 于是尝试会真的走到网络阶段，然后因写失败回到等待。
	peers := mem.NewPeers(fc)
	if err := peers.Upsert(peer.Peer{
		NodeID: peerID, State: peer.StateOnline, LastAddr: "127.0.0.1:9", LastSeen: fc.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	sess := &brokenSession{
		peer: peerID, failFrom: 1, err: errors.New("use of closed network connection"),
	}
	a := NewTransferApp(identity.NodeID{}, repo, nil, deadConns{sess: sess}, peers, eventbus.New(), fc, nil)
	a.ChunkSize = 1024
	a.BaseBackoff = time.Second

	dir := t.TempDir()
	for i, id := range []string{"job-1", "job-2", "job-3"} {
		p := filepath.Join(dir, id+".bin")
		if err := os.WriteFile(p, bytes.Repeat([]byte{1}, 4*1024), 0o644); err != nil {
			t.Fatal(err)
		}
		j := a.newWaitingJob(id, peerID, p, 4096)
		j.CreatedAt = fc.Now().Add(time.Duration(i) * time.Second)
		if err := repo.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}

	// 退避未到点：一条都不该动，也不该发出任何帧
	a.SweepWaiting(context.Background())
	if n := countAttempted(repo); n != 0 {
		t.Fatalf("退避未到点就启动了 %d 条任务", n)
	}
	if got := sess.sent(); got != 0 {
		t.Fatalf("退避未到点就发了 %d 帧", got)
	}

	// 到点：同一对端这一轮只启动一条
	fc.Advance(2 * time.Second)
	a.SweepWaiting(context.Background())
	waitUntil(t, 2*time.Second, "有一条任务被启动", func() bool {
		return countAttempted(repo) == 1
	})
	// 给「误启动第二条」留出犯错窗口：真出问题时它会在这一小段时间里出现
	time.Sleep(50 * time.Millisecond)
	if n := countAttempted(repo); n != 1 {
		t.Fatalf("同一对端在同一轮启动了 %d 条（want 1）—— 对方一上线就会同时哈希多个文件", n)
	}
}

// 连续失败必须让退避递增。
//
// 这条守卫来自一个真实缺陷：尝试时任务是从零构造的，若不把历史次数带回来，
// 退避会永远停在第一档 —— 对端离线时变成每秒一次无谓唤醒，
// 而且永远到不了 5 分钟上限。
func TestWaitingBackoffEscalates(t *testing.T) {
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
	a := NewTransferApp(identity.NodeID{}, repo, nil, deadConns{sess: sess}, peers, eventbus.New(), fc, nil)
	a.ChunkSize = 1024

	p := filepath.Join(t.TempDir(), "b.bin")
	if err := os.WriteFile(p, bytes.Repeat([]byte{1}, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	j := a.newWaitingJob("job-backoff", peerID, p, 4096)
	j.RetryCount = 1
	j.NextTryAt = fc.Now()
	if err := repo.UpsertJob(j); err != nil {
		t.Fatal(err)
	}

	// 第一次尝试失败 → 计数 2，下一次间隔 backoff(2) = 2s
	a.SweepWaiting(context.Background())
	waitUntil(t, 2*time.Second, "第一次尝试失败并重新入等待", func() bool {
		got, err := repo.GetJob("job-backoff")
		return err == nil && got.RetryCount == 2
	})
	if got, _ := repo.GetJob("job-backoff"); !got.NextTryAt.Equal(fc.Now().Add(2 * time.Second)) {
		t.Fatalf("第一次重试间隔 = %v，期望 2s（退避没有递增）", got.NextTryAt.Sub(fc.Now()))
	}

	// 第二次尝试失败 → 计数 3，下一次间隔 backoff(3) = 4s
	fc.Advance(2 * time.Second)
	a.SweepWaiting(context.Background())
	waitUntil(t, 2*time.Second, "第二次尝试失败并重新入等待", func() bool {
		got, err := repo.GetJob("job-backoff")
		return err == nil && got.RetryCount == 3
	})
	if got, _ := repo.GetJob("job-backoff"); !got.NextTryAt.Equal(fc.Now().Add(4 * time.Second)) {
		t.Fatalf("第二次重试间隔 = %v，期望 4s（退避没有递增）", got.NextTryAt.Sub(fc.Now()))
	}
}

// 早退路径（对端看起来离线，连哈希都不做）的退避同样必须递增。
//
// 这条守卫来自一个真实缺陷：早退路径每次都 newWaitingJob，而它把 RetryCount
// 从 1 重新开始 —— 于是"每秒重试一次"永远停不下来，也永远到不了上限。
func TestWaitingBackoffEscalatesOnOfflineShortcut(t *testing.T) {
	peerID := mustNodeID(t, testPeerHex)
	repo := &stubTransferRepo{}
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))

	// noConns + 空目录：永远走"对端离线"的早退路径
	a := NewTransferApp(identity.NodeID{}, repo, nil, noConns{}, nil, eventbus.New(), fc, nil)
	a.ChunkSize = 1024

	p := filepath.Join(t.TempDir(), "o.bin")
	if err := os.WriteFile(p, bytes.Repeat([]byte{4}, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	j := a.newWaitingJob("job-offline-backoff", peerID, p, 4096)
	j.RetryCount = 3
	j.NextTryAt = fc.Now()
	if err := repo.UpsertJob(j); err != nil {
		t.Fatal(err)
	}

	a.SweepWaiting(context.Background())
	waitUntil(t, 2*time.Second, "早退路径重新入等待", func() bool {
		got, err := repo.GetJob("job-offline-backoff")
		return err == nil && got.RetryCount == 4
	})
	got, _ := repo.GetJob("job-offline-backoff")
	if want := fc.Now().Add(8 * time.Second); !got.NextTryAt.Equal(want) {
		t.Fatalf("下一次重试时间 = %v（间隔 %v），期望 %v（backoff(4)=8s）",
			got.NextTryAt, got.NextTryAt.Sub(fc.Now()), want)
	}
}

// 取消与启动之间的竞态：抢占到 goroutine 之后必须复读一次库。
//
// 时序（真实可发生）：CancelJob 通过检查并写入 cancelled → 随后那条已经
// 抢到 starting 的 goroutine 才跑到 sendFile。若不复读，它会用
// StateMetaExchange 把任务写回去，取消就成了假的 —— 而「取消之后对方上线
// 仍然自动传输」正是这个功能明令禁止的事。
func TestStartAfterCancelDoesNotResurrectJob(t *testing.T) {
	peerID := mustNodeID(t, testPeerHex)
	repo := &stubTransferRepo{}
	a := NewTransferApp(identity.NodeID{}, repo, nil, noConns{}, nil, eventbus.New(), clock.New(), nil)
	a.ChunkSize = 1024

	p := filepath.Join(t.TempDir(), "race.bin")
	if err := os.WriteFile(p, bytes.Repeat([]byte{8}, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	cancelled := a.newWaitingJob("job-race", peerID, p, 4096)
	cancelled.Status = transfer.StateCancelled
	cancelled.WaitReason = transfer.WaitNone
	cancelled.NextTryAt = time.Time{}
	if err := repo.UpsertJob(cancelled); err != nil {
		t.Fatal(err)
	}

	if _, err := a.sendFile(context.Background(), peerID, p, "job-race", true); err == nil {
		t.Fatal("已取消的任务不该被启动")
	}
	got, err := repo.GetJob("job-race")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != transfer.StateCancelled {
		t.Errorf("status = %q，want cancelled（取消被启动流程覆盖了）", got.Status)
	}
	if got.WaitReason != transfer.WaitNone {
		t.Errorf("waitReason = %q，取消的任务不该被重新标记为等待", got.WaitReason)
	}
}

// 取消：任务变 cancelled、清掉等待标记，于是 sweep 再也拾取不到它。
func TestCancelJobStopsWaiting(t *testing.T) {
	peerID := mustNodeID(t, testPeerHex)
	repo := &stubTransferRepo{}
	a := NewTransferApp(identity.NodeID{}, repo, nil, noConns{}, nil, eventbus.New(), clock.New(), nil)

	p := filepath.Join(t.TempDir(), "x.bin")
	if err := os.WriteFile(p, bytes.Repeat([]byte{1}, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	j := a.newWaitingJob("job-cancel", peerID, p, 4096)
	if err := repo.UpsertJob(j); err != nil {
		t.Fatal(err)
	}

	if err := a.CancelJob("job-cancel"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got, err := repo.GetJob("job-cancel")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != transfer.StateCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
	if got.WaitReason != transfer.WaitNone || !got.NextTryAt.IsZero() {
		t.Errorf("取消后必须清掉等待标记：%q / %v", got.WaitReason, got.NextTryAt)
	}

	// 取消之后 sweep 必须落空。
	//
	// 这里【不】断言 repo.ListWaiting() 为空：测试替身刻意不做领域过滤，
	// 要验的正是"即便仓储把取消过的任务也递上来，调度层也不会动它"。
	// 判据用尝试次数有没有变（起点由 newWaitingJob 决定，不写死具体值）。
	attemptsBefore := j.RetryCount
	a.SweepWaiting(context.Background())
	time.Sleep(50 * time.Millisecond)
	if got, _ := repo.GetJob("job-cancel"); got.RetryCount != attemptsBefore {
		t.Errorf("取消后仍被自动重试（尝试次数 %d → %d）", attemptsBefore, got.RetryCount)
	}
}

// 不能取消已经在传的任务：当前 sender 没有中断点，假取消比拒绝更糟。
func TestCancelJobRejectsRunningTransfer(t *testing.T) {
	peerID := mustNodeID(t, testPeerHex)
	repo := &stubTransferRepo{}
	a := NewTransferApp(identity.NodeID{}, repo, nil, noConns{}, nil, eventbus.New(), clock.New(), nil)

	running := transfer.Job{
		JobID: "job-running", PeerID: peerID, Direction: transfer.DirectionSend,
		Status: transfer.StateTransferring, ChunkSize: 1024, TotalChunks: 4, FileSize: 4096,
	}
	if err := repo.UpsertJob(running); err != nil {
		t.Fatal(err)
	}
	if err := a.CancelJob("job-running"); err == nil {
		t.Fatal("已在传输的任务必须拒绝取消")
	}

	// 「已经交给 goroutine、但状态还没写成 active」这个窗口同样必须拦住 ——
	// 放行的后果是那个 goroutine 随后把状态写回"传输中"，取消变成假的。
	waiting := a.newWaitingJob("job-claimed", peerID, "/tmp/claimed.bin", 4096)
	if err := repo.UpsertJob(waiting); err != nil {
		t.Fatal(err)
	}
	if !a.claimStarting("job-claimed") {
		t.Fatal("抢占失败，测试前提不成立")
	}
	defer a.releaseStarting("job-claimed")
	if err := a.CancelJob("job-claimed"); err == nil {
		t.Fatal("已被交给 goroutine 的任务必须拒绝取消")
	}
}

// 立即重试：把退避推到当下，并真的发起一次尝试（force 绕过"目录说离线"）。
func TestRetryNowStartsAttempt(t *testing.T) {
	peerID := mustNodeID(t, testPeerHex)
	repo := &stubTransferRepo{}
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))

	// 目录为空（对端看起来离线）：force 必须照样真的拨一次
	sess := &brokenSession{
		peer: peerID, failFrom: 1, err: errors.New("use of closed network connection"),
	}
	a := NewTransferApp(identity.NodeID{}, repo, nil, deadConns{sess: sess}, nil, eventbus.New(), fc, nil)
	a.ChunkSize = 1024

	p := filepath.Join(t.TempDir(), "r.bin")
	if err := os.WriteFile(p, bytes.Repeat([]byte{2}, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	j := a.newWaitingJob("job-retry", peerID, p, 2048)
	j.RetryCount = 5
	j.NextTryAt = fc.Now().Add(time.Hour) // 退避还很久
	if err := repo.UpsertJob(j); err != nil {
		t.Fatal(err)
	}

	if err := a.RetryNow(context.Background(), "job-retry"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	waitUntil(t, 2*time.Second, "立即重试真的发起了一次尝试", func() bool {
		return countAttempted(repo) == 1
	})
	if got, _ := repo.GetJob("job-retry"); got.NextTryAt.After(fc.Now()) {
		t.Errorf("下次重试时间没被推到当下：%v", got.NextTryAt.Sub(fc.Now()))
	}
}
