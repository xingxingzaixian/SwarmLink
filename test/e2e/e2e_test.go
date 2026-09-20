// Package e2e 在单进程内跑多节点端到端用例。
//
// 覆盖：发现 → 握手 → 单聊（含幂等去重）→ 文件传输 → 断点续传 → 跨网段目录收敛。
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
	"github.com/swarmlink/swarmlink/test/harness"
)

func newNode(t *testing.T, name string, opts harness.Options) *harness.Node {
	t.Helper()
	n, err := harness.New(name, opts)
	if err != nil {
		t.Fatalf("new node %s: %v", name, err)
	}
	t.Cleanup(n.Close)
	return n
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool, diag ...func() string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(diag) > 0 && diag[0] != nil {
		t.Fatalf("timeout waiting for %s | %s", what, diag[0]())
	}
	t.Fatalf("timeout waiting for %s", what)
}

// directConv 是测试侧的唯一 conv_id 真相来源：必须与实现一致（对称）。
func directConv(x, y *harness.Node) string {
	return message.DirectConvID(x.ID(), y.ID())
}

func randomFile(t *testing.T, size int) (string, []byte) {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, data
}

func TestThreeNodesDiscoveryAndChat(t *testing.T) {
	a := newNode(t, "alice", harness.Options{})
	b := newNode(t, "bob", harness.Options{})
	c := newNode(t, "carol", harness.Options{})

	if err := harness.Link(a, b); err != nil {
		t.Fatal(err)
	}
	if err := harness.Link(b, c); err != nil {
		t.Fatal(err)
	}
	if err := harness.Link(a, c); err != nil {
		t.Fatal(err)
	}

	for _, n := range []*harness.Node{a, b, c} {
		if n.Peers.Len() != 2 {
			t.Fatalf("%s: want 2 peers got %d", n.Name, n.Peers.Len())
		}
	}

	ctx := context.Background()
	msg, err := a.ChatApp.SendMessage(ctx, b.ID(), "hello bob")
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	conv := directConv(a, b)

	waitFor(t, 5*time.Second, "bob to receive the message", func() bool {
		ms, _ := b.Messages.Latest(conv, 10, 0)
		return len(ms) == 1 && ms[0].Content == "hello bob" && ms[0].Direction == message.DirectionIn
	}, func() string {
		ms, _ := b.Messages.Latest(conv, 10, 0)
		return fmt.Sprintf("convID=%s b.latest=%d b.total=%d a.outbox=%d a.sessions=%d b.sessions=%d",
			conv, len(ms), b.Messages.Count(), a.Messages.OutboxLen(),
			a.Conn.SessionCount(), b.Conn.SessionCount())
	})

	waitFor(t, 5*time.Second, "alice to receive CHAT_ACK", func() bool {
		m, ok := a.Messages.Get(msg.MsgID)
		return ok && m.State == message.StateDelivered
	})

	if n := a.Messages.OutboxLen(); n != 0 {
		t.Fatalf("outbox should be empty after ACK, got %d", n)
	}

	// 发送侧的会话 ID 必须与接收侧一致（同一段会话）
	if msg.ConvID != conv {
		t.Fatalf("sender conv id %q != expected %q", msg.ConvID, conv)
	}
}

func TestDuplicateChatIsDedupedButStillAcked(t *testing.T) {
	a := newNode(t, "alice", harness.Options{})
	b := newNode(t, "bob", harness.Options{})
	if err := harness.Link(a, b); err != nil {
		t.Fatal(err)
	}

	var uiEvents int
	b.Bus.Subscribe(eventbus.TopicChatReceived, func(any) { uiEvents++ })

	ctx := context.Background()
	msg, err := a.ChatApp.SendMessage(ctx, b.ID(), "only once")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "bob to receive", func() bool {
		return b.Messages.Count() == 1
	})

	// 模拟「ACK 丢失导致发送方重发」：重新入队并立即扫描
	if err := a.Messages.Enqueue(msg.MsgID, msg.ConvID, time.Now()); err != nil {
		t.Fatal(err)
	}
	a.ChatApp.SweepOutbox(ctx)

	// 重复到达必须被幂等吸收：不重复展示、不重复入库，但仍要回 ACK
	waitFor(t, 5*time.Second, "duplicate to be absorbed and acked", func() bool {
		m, ok := a.Messages.Get(msg.MsgID)
		return ok && m.State == message.StateDelivered
	})
	if got := b.Messages.Count(); got != 1 {
		t.Fatalf("duplicate must not create a second row, got %d", got)
	}
	if uiEvents != 1 {
		t.Fatalf("UI must be notified exactly once, got %d", uiEvents)
	}
}

func TestFileTransferEndToEnd(t *testing.T) {
	a := newNode(t, "alice", harness.Options{ChunkSize: 64 * 1024})
	b := newNode(t, "bob", harness.Options{ChunkSize: 64 * 1024})
	if err := harness.Link(a, b); err != nil {
		t.Fatal(err)
	}

	src, data := randomFile(t, 1<<20+1234) // 约 1 MB，尾块不整

	job, err := a.TransferApp.SendFile(context.Background(), b.ID(), src)
	if err != nil {
		t.Fatalf("send file: %v", err)
	}
	if job.Status != transfer.StateDone {
		t.Fatalf("job status want done got %s (err=%s)", job.Status, job.Error)
	}

	dst := filepath.Join(b.DownloadDir, "payload.bin")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read received file: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("received content mismatch: want %d bytes got %d", len(data), len(got))
	}

	if jobs, err := b.Messages.ListActive(); err == nil && len(jobs) != 0 {
		t.Fatalf("no active jobs expected on receiver, got %d", len(jobs))
	}
}

func TestFileTransferResumesAfterInterruption(t *testing.T) {
	a := newNode(t, "alice", harness.Options{ChunkSize: 64 * 1024})
	b := newNode(t, "bob", harness.Options{ChunkSize: 64 * 1024})
	if err := harness.Link(a, b); err != nil {
		t.Fatal(err)
	}

	// 让接收侧更频繁地回 ACK（ACK 时顺带落盘位图），
	// 这样中断后一定存在可续传的位图。
	b.TransferApp.AckEveryChunks = 4
	b.TransferApp.AckInterval = 5 * time.Millisecond
	b.TransferApp.FlushEveryChunks = 4

	// 4 MB = 64 块：窗口 W=8、每轮最多推进 8 块，
	// 因此 100 ms 中断时一定只完成了一部分。
	src, data := randomFile(t, 4<<20)
	const total = 64

	ctx1, cancel1 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, _ = a.TransferApp.SendFile(ctx1, b.ID(), src)
	cancel1()

	partial := int64(0)
	if jobs, err := b.Messages.ListActive(); err == nil {
		for _, j := range jobs {
			if j.Direction == transfer.DirectionRecv {
				partial = j.Completed
			}
		}
	}
	if partial == 0 {
		t.Skip("first attempt persisted nothing (scheduler too fast); resume path not exercised")
	}
	t.Logf("first attempt persisted %d/%d chunks before interruption", partial, total)

	// 第二次：同一文件（同一 file_hash）必须续传并最终校验通过
	job, err := a.TransferApp.SendFile(context.Background(), b.ID(), src)
	if err != nil {
		t.Fatalf("resume send: %v", err)
	}
	if job.Status != transfer.StateDone {
		t.Fatalf("resume job status want done got %s (err=%s)", job.Status, job.Error)
	}

	got, err := os.ReadFile(filepath.Join(b.DownloadDir, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("content mismatch after resume")
	}
}

// TestCrossSubnetConvergenceViaSeed 是「跨网段能力进 v1.0」这个决定的验收物。
//
// 构造两个互不广播的域（A 域 {a1}，B 域 {b1,b2}）与一颗同时可达两域的种子；
// 只允许通过种子的单跳目录交换收敛，不允许任何转发/中继。
func TestCrossSubnetConvergenceViaSeed(t *testing.T) {
	seed := newNode(t, "seed", harness.Options{})
	a1 := newNode(t, "a1", harness.Options{})
	b1 := newNode(t, "b1", harness.Options{})
	b2 := newNode(t, "b2", harness.Options{})

	// 域内互相发现（等价于同网段广播）：种子属于 A 域；B 域内部互通。
	// 注意：a1 与 b1/b2 之间【没有】Link —— 它们互相看不见。
	if err := harness.Link(a1, seed); err != nil {
		t.Fatal(err)
	}
	if err := harness.Link(b1, b2); err != nil {
		t.Fatal(err)
	}

	// 种子清单：种子拉取两个域；各域节点只拉种子。
	seed.SetSeeds([]peer.SeedAddr{a1.SeedAddr(), b1.SeedAddr()})
	a1.SetSeeds([]peer.SeedAddr{seed.SeedAddr()})
	b1.SetSeeds([]peer.SeedAddr{seed.SeedAddr()})

	ctx := context.Background()

	// 第一轮：种子从两个域各拉一次目录
	seed.DiscApp.RefreshSeeds(ctx)
	waitFor(t, 5*time.Second, "seed to learn b1", func() bool {
		_, ok := seed.Peers.Get(b1.ID())
		return ok
	})
	waitFor(t, 5*time.Second, "seed to learn b2 (via b1's directory)", func() bool {
		_, ok := seed.Peers.Get(b2.ID())
		return ok
	})

	// 第二轮：两个域的节点向种子拉取 → 目录双向收敛
	a1.DiscApp.RefreshSeeds(ctx)
	b1.DiscApp.RefreshSeeds(ctx)

	waitFor(t, 5*time.Second, "a1 to learn b1", func() bool {
		_, ok := a1.Peers.Get(b1.ID())
		return ok
	})
	waitFor(t, 5*time.Second, "a1 to learn b2", func() bool {
		_, ok := a1.Peers.Get(b2.ID())
		return ok
	})
	waitFor(t, 5*time.Second, "b1 to learn a1", func() bool {
		_, ok := b1.Peers.Get(a1.ID())
		return ok
	})

	// 学到的条目必须带可拨号地址（跨网段首条消息直接拨号，零跳数）
	p, ok := a1.Peers.Get(b1.ID())
	if !ok || p.LastAddr == "" {
		t.Fatalf("cross-subnet entry must carry a dialable address: %+v", p)
	}
	if p.Source != peer.SourceSeed {
		t.Fatalf("cross-subnet entry source want %q got %q", peer.SourceSeed, p.Source)
	}
	// P-2：subnet 必须被填充（地址重叠检测依赖它）
	if p.Subnet == "" {
		t.Fatal("subnet must be populated in ANNOUNCE (P-2 detection)")
	}

	// 跨网段首条消息：直接拨号即可送达
	conv := directConv(a1, b1)
	msg, err := a1.ChatApp.SendMessage(ctx, b1.ID(), "cross-subnet hello")
	if err != nil {
		t.Fatalf("cross-subnet send: %v", err)
	}
	waitFor(t, 5*time.Second, "b1 to receive cross-subnet message", func() bool {
		ms, _ := b1.Messages.Latest(conv, 10, 0)
		return len(ms) == 1 && ms[0].Content == "cross-subnet hello"
	})
	waitFor(t, 5*time.Second, "a1 to get ACK across subnet boundary", func() bool {
		m, ok := a1.Messages.Get(msg.MsgID)
		return ok && m.State == message.StateDelivered
	})
}
