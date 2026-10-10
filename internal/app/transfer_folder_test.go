package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/store/mem"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// newFolderFixture 造一个可枚举文件夹的 TransferApp。
//
// 对端能力位用它控制；刻意【不给地址】（LastAddr 为空）—— 那样 peerReachable
// 为 false，被拉起的任务会在哈希之前就退回等待，枚举期的断言因此是确定的
// （没有 goroutine 在读文件、也不会把 file_hash 写进去）。
func newFolderFixture(t *testing.T, caps peer.Caps) (*TransferApp, *stubTransferRepo, identity.NodeID) {
	t.Helper()
	repo := &stubTransferRepo{}
	clk := clock.New()
	dir := mem.NewPeers(clk)

	peerID := identity.NodeID{}
	peerID[len(peerID)-1] = 42
	if err := dir.Upsert(peer.Peer{NodeID: peerID, Caps: caps, DisplayName: "bob"}); err != nil {
		t.Fatal(err)
	}

	a := NewTransferApp(identity.NodeID{}, repo, nil, noConns{}, dir, eventbus.New(), clk, nil)
	a.ChunkSize = 1024
	return a, repo, peerID
}

func mkFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkDir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// 枚举：真实文件 + 空目录各一条，rel_path 含根名，枚举期不哈希。
func TestSendFolderEnumeratesTree(t *testing.T) {
	root := t.TempDir()
	mkFile(t, filepath.Join(root, "照片", "a.jpg"), 100)
	mkFile(t, filepath.Join(root, "照片", "2024", "b.jpg"), 200)
	mkDir(t, filepath.Join(root, "照片", "空目录"))

	a, repo, peerID := newFolderFixture(t, peer.CapsFolderTransfer)

	sum, err := a.SendFolderAs(context.Background(), peerID, filepath.Join(root, "照片"), "batch-1")
	if err != nil {
		t.Fatalf("SendFolderAs: %v", err)
	}
	if sum.FileCount != 3 {
		t.Errorf("file_count = %d，期望 3（2 个文件 + 1 个空目录）", sum.FileCount)
	}
	if sum.TotalSize != 300 {
		t.Errorf("total_size = %d，期望 300（目录条目算 0）", sum.TotalSize)
	}
	if sum.Skipped != 0 {
		t.Errorf("skipped = %d，期望 0", sum.Skipped)
	}
	if sum.FolderName != "照片" {
		t.Errorf("folder_name = %q，期望 照片", sum.FolderName)
	}

	jobs, err := repo.ListByBatch("batch-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("落库任务数 = %d，期望 3", len(jobs))
	}
	for _, j := range jobs {
		// 只断言「还在 Idle 且是可自动拉起的」：SendFolderAs 结尾会立刻试一轮，
		// 那个 goroutine 可能已把 WaitQueued 改写成 WaitPeerOffline（对端不可达），
		// 两者都是合法的、都必须是可拉起的。
		if j.Status != transfer.StateIdle {
			t.Errorf("任务 %s 状态 = %s，期望 idle", j.JobID, j.Status)
		}
		if !j.WaitReason.AutoStartable() {
			t.Errorf("任务 %s 的 waitReason = %q，必须可自动拉起", j.JobID, j.WaitReason)
		}
		if j.FileHash != "" {
			t.Errorf("任务 %s 在枚举阶段不该有哈希（大目录会被卡死）", j.JobID)
		}
		if j.BatchID != "batch-1" {
			t.Errorf("任务 %s 的 batch_id = %q", j.JobID, j.BatchID)
		}
		if !strings.HasPrefix(j.RelPath, "照片/") {
			t.Errorf("任务 %s 的 rel_path = %q，必须含根名", j.JobID, j.RelPath)
		}
		if filepath.Separator == '\\' && strings.Contains(j.RelPath, `\`) {
			t.Errorf("任务 %s 的 rel_path = %q，必须是 ToSlash 之后的形式", j.JobID, j.RelPath)
		}
	}
}

// 空文件夹：为根目录本身发一条目录条目，否则接收端建不出这个空目录。
func TestSendFolderEmptyRoot(t *testing.T) {
	root := t.TempDir()
	mkDir(t, filepath.Join(root, "空的"))

	a, repo, peerID := newFolderFixture(t, peer.CapsFolderTransfer)

	sum, err := a.SendFolderAs(context.Background(), peerID, filepath.Join(root, "空的"), "batch-empty")
	if err != nil {
		t.Fatalf("SendFolderAs: %v", err)
	}
	if sum.FileCount != 1 || sum.TotalSize != 0 {
		t.Errorf("summary = %+v，期望 1 条零字节条目", sum)
	}

	jobs, _ := repo.ListByBatch("batch-empty")
	if len(jobs) != 1 {
		t.Fatalf("任务数 = %d，期望 1", len(jobs))
	}
	if !jobs[0].IsDir {
		t.Error("空文件夹的根条目必须是目录条目")
	}
	if jobs[0].RelPath != "空的" {
		t.Errorf("rel_path = %q，期望单段「空的」", jobs[0].RelPath)
	}
}

// 符号链接不跟随（避免循环与越权读取），但必须如实计数。
func TestSendFolderSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	mkFile(t, filepath.Join(root, "照片", "a.jpg"), 10)
	outside := filepath.Join(root, "外面的机密.txt")
	mkFile(t, outside, 10)

	if err := os.Symlink(outside, filepath.Join(root, "照片", "链接.txt")); err != nil {
		t.Skipf("本环境不支持创建符号链接（Windows 需要开发者模式或管理员）: %v", err)
	}

	a, repo, peerID := newFolderFixture(t, peer.CapsFolderTransfer)
	sum, err := a.SendFolderAs(context.Background(), peerID, filepath.Join(root, "照片"), "batch-link")
	if err != nil {
		t.Fatalf("SendFolderAs: %v", err)
	}
	if sum.Skipped != 1 {
		t.Errorf("skipped = %d，期望 1（必须如实告诉用户跳过了几项）", sum.Skipped)
	}
	if sum.FileCount != 1 {
		t.Errorf("file_count = %d，期望 1（链接不算）", sum.FileCount)
	}
	jobs, _ := repo.ListByBatch("batch-link")
	for _, j := range jobs {
		if strings.Contains(j.RelPath, "链接") {
			t.Errorf("符号链接不该被枚举: %+v", j)
		}
	}
}

// 超过条目上限：整批拒绝，一条任务都不许留下。
func TestSendFolderRejectsTooManyEntries(t *testing.T) {
	root := t.TempDir()
	mkFile(t, filepath.Join(root, "照片", "a.txt"), 1)
	mkFile(t, filepath.Join(root, "照片", "b.txt"), 1)

	a, repo, peerID := newFolderFixture(t, peer.CapsFolderTransfer)
	a.MaxFolderEntries = 1 // 调小上限，避免真造两万个文件

	_, err := a.SendFolderAs(context.Background(), peerID, filepath.Join(root, "照片"), "batch-big")
	if err == nil {
		t.Fatal("超过条目上限必须整批拒绝")
	}
	jobs, _ := repo.ListByBatch("batch-big")
	if len(jobs) != 0 {
		t.Errorf("拒绝时不该留下任何任务，实际 %d 条", len(jobs))
	}
}

// 对端不支持目录传输：明确拒绝，而不是偷偷打包或平铺过去。
func TestSendFolderRequiresCapability(t *testing.T) {
	root := t.TempDir()
	mkFile(t, filepath.Join(root, "照片", "a.txt"), 1)

	a, repo, peerID := newFolderFixture(t, peer.CapsChatReliable) // 没有 CapsFolderTransfer

	_, err := a.SendFolderAs(context.Background(), peerID, filepath.Join(root, "照片"), "batch-nocap")
	if err == nil {
		t.Fatal("对端不支持目录传输时必须拒绝发送")
	}
	if !strings.Contains(err.Error(), "不支持") {
		t.Errorf("错误信息要说清原因，得到 %q", err)
	}
	jobs, _ := repo.ListByBatch("batch-nocap")
	if len(jobs) != 0 {
		t.Errorf("拒绝时不该留下任务，实际 %d 条", len(jobs))
	}
}

// 能力位必须能真的从握手协商走到目录 —— 这是「对方版本不支持文件夹传输」
// 这个误报的正面回归。
//
// 缺陷成因：目录里的 caps 只会被「目录交换的第三方转述」写，而 announce 不携带
// 能力位、session.Caps() 全仓无人调用 —— 于是无论对端升不升级，本机目录里的
// caps 永远是 0，所有文件夹传输都被判成不支持。本用例走完整接缝：
// conn 层发 peer.online（带 HELLO 协商出的 caps_common）→ PeerApp 写目录 →
// SendFolderAs 放行。
func TestFolderCapabilityLearnedFromHandshake(t *testing.T) {
	root := t.TempDir()
	mkFile(t, filepath.Join(root, "照片", "a.txt"), 1)

	clk := clock.New()
	dir := mem.NewPeers(clk)
	bus := eventbus.New()

	peerID := identity.NodeID{}
	peerID[len(peerID)-1] = 7
	// 只被 announce 发现过：announce 不携带能力位，条目里因此没有 caps。
	if err := dir.Upsert(peer.Peer{NodeID: peerID, DisplayName: "bob"}); err != nil {
		t.Fatal(err)
	}

	a := NewTransferApp(identity.NodeID{}, &stubTransferRepo{}, nil, noConns{}, dir, bus, clk, nil)
	a.ChunkSize = 1024

	// 协商前：能力位未知 ⇒ 必须拒绝（原有行为，不得退化）。
	if _, err := a.SendFolderAs(context.Background(), peerID, filepath.Join(root, "照片"), "batch-before"); err == nil {
		t.Fatal("协商前不该放行文件夹发送")
	}

	// 连接建立：连接层按 HELLO 的 caps_common 发 peer.online。
	NewPeerApp(identity.NodeID{}, dir, bus, clk, time.Minute, nil).Start()
	bus.Publish(eventbus.TopicPeerOnline, eventbus.PeerOnline{
		NodeID:   peerID.String(),
		Caps:     peer.CapsChatReliable | peer.CapsFolderTransfer,
		ProtoVer: 2,
	})

	got, ok := dir.Get(peerID)
	if !ok || !got.Caps.Has(peer.CapsFolderTransfer) {
		t.Fatalf("协商出的能力位没有落到目录：%+v", got)
	}
	if got.ProtoVer != 2 {
		t.Errorf("proto_ver = %d，期望 2", got.ProtoVer)
	}

	// 协商后：放行。
	if _, err := a.SendFolderAs(context.Background(), peerID, filepath.Join(root, "照片"), "batch-after"); err != nil {
		t.Fatalf("协商后必须放行，得到 %v", err)
	}
}

// 传的文件不是一个目录：立即报错。
func TestSendFolderRejectsNonDirectory(t *testing.T) {
	root := t.TempDir()
	mkFile(t, filepath.Join(root, "a.txt"), 1)

	a, _, peerID := newFolderFixture(t, peer.CapsFolderTransfer)
	if _, err := a.SendFolderAs(context.Background(), peerID, filepath.Join(root, "a.txt"), "b"); err == nil {
		t.Fatal("传文件而不是目录时必须报错")
	}
	if _, err := a.SendFolderAs(context.Background(), peerID, filepath.Join(root, "不存在"), "b"); err == nil {
		t.Fatal("路径不存在时必须报错")
	}
}

// 批次字段必须像 RetryCount 一样「承接历史」。
//
// sendFile / newWaitingJob 都是从零构造 Job 再 Upsert，漏掉这里的话，
// 文件真正开始传的那一刻批次字段会被抹成零值 —— 表现为传输列表里的
// 相对路径没了、文件夹卡片的进度永远算不出来。
func TestCarryOverBatchFields(t *testing.T) {
	prev := transfer.Job{
		JobID: "j1", BatchID: "b1", RelPath: "照片/a.jpg", IsDir: true,
	}
	got := carryOverBatch(prev, transfer.Job{JobID: "j1", RetryCount: 2})
	if got.BatchID != "b1" || got.RelPath != "照片/a.jpg" || !got.IsDir {
		t.Errorf("批次字段丢失：%+v", got)
	}
	if got.RetryCount != 2 {
		t.Errorf("不该动其它字段：%+v", got)
	}
}

// 枚举出来的任务必须真的会被拉起（不是躺在库里）。
//
// 这条是 Task 2 与 Task 6 的接缝：Task 2 让 queued 可被拉起，
// 这里确认 SendFolderAs 结尾真的触发了一轮 —— 对端不可达，因此被拉起后
// 会立刻退回「等待对方上线」，这正是期望的终局。
func TestSendFolderStartsFirstJob(t *testing.T) {
	root := t.TempDir()
	mkFile(t, filepath.Join(root, "照片", "a.txt"), 1)

	a, repo, peerID := newFolderFixture(t, peer.CapsFolderTransfer)

	if _, err := a.SendFolderAs(context.Background(), peerID, filepath.Join(root, "照片"), "batch-start"); err != nil {
		t.Fatalf("SendFolderAs: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		jobs, _ := repo.ListByBatch("batch-start")
		if len(jobs) == 1 && jobs[0].WaitReason == transfer.WaitPeerOffline {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	jobs, _ := repo.ListByBatch("batch-start")
	t.Fatalf("任务没有被拉起过（waitReason 应已由 batch_queued 转成 peer_offline）：%+v", jobs)
}
