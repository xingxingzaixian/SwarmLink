package wails

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/app"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
)

// 本文件只验证【服务层往下传了什么】，因此全部依赖都用最小 fake：
// 真实网络与落盘不是这里的关注点，把它们拉进来只会让测试变慢变脆。

// NodeID 是 8 字节指纹（16 个 hex 字符），见 identity.nodeIDLen。
const (
	testSelfHex = "1122334455667788"
	testPeerHex = "aabbccddeeff0011"
)

var errNoSession = errors.New("test: 没有可用会话")

// ---------------------------------------------------------------------------
// fake
// ---------------------------------------------------------------------------

type memTransferRepo struct {
	mu   sync.Mutex
	jobs map[string]transfer.Job
}

func newMemTransferRepo() *memTransferRepo {
	return &memTransferRepo{jobs: make(map[string]transfer.Job)}
}

func (r *memTransferRepo) UpsertJob(j transfer.Job) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs[j.JobID] = j
	return nil
}

func (r *memTransferRepo) GetJob(jobID string) (transfer.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[jobID]
	if !ok {
		return transfer.Job{}, os.ErrNotExist
	}
	return j, nil
}

func (r *memTransferRepo) get(jobID string) (transfer.Job, bool) {
	j, err := r.GetJob(jobID)
	return j, err == nil
}

func (r *memTransferRepo) FindResumable(identity.NodeID, string) (transfer.Job, bool) {
	return transfer.Job{}, false
}

func (r *memTransferRepo) SaveBitmap(string, transfer.ChunkBitmap, int64) error { return nil }

func (r *memTransferRepo) ListActive() ([]transfer.Job, error)                 { return nil, nil }
func (r *memTransferRepo) ListRecent(limit int) ([]transfer.Job, error)         { return nil, nil }
func (r *memTransferRepo) PurgeFinished(keep int, olderThan time.Time) (int, error) {
	return 0, nil
}

// stubConns 永远没有可用会话：发送会在「任务已建好之后」失败，
// 这正好让我们观察到任务是否用【正确的 job_id】落了库。
type stubConns struct{}

func (stubConns) Dial(context.Context, identity.NodeID, string) (ports.Session, error) {
	return nil, errNoSession
}
func (stubConns) Accept(context.Context) (ports.Session, error)      { return nil, errNoSession }
func (stubConns) SessionOf(identity.NodeID) (ports.Session, bool)    { return nil, false }
func (stubConns) Broadcast(protocol.Frame, ...identity.NodeID) error { return nil }
func (stubConns) Close(identity.NodeID) error                        { return nil }

type emptyDir struct{}

func (emptyDir) Upsert(peer.Peer) error                           { return nil }
func (emptyDir) Get(identity.NodeID) (peer.Peer, bool)            { return peer.Peer{}, false }
func (emptyDir) List(ports.PeerFilter) []peer.Peer                { return nil }
func (emptyDir) MarkOffline(identity.NodeID, time.Time) error     { return nil }
func (emptyDir) SeenRecently(identity.NodeID, time.Duration) bool { return false }

func selfID(t *testing.T) identity.NodeID {
	t.Helper()
	id, err := identity.ParseNodeID(testSelfHex)
	if err != nil {
		t.Fatalf("解析自身 NodeID: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// SendFile 的返回值契约
// ---------------------------------------------------------------------------

// TestSendFileReturnsTheRealJobID 锁住服务层的核心契约：
// 返回的 job_id 必须【就是】任务落库所用的那个 id。
//
// 这是界面能否显示进度的前提 —— 前端拿这个 id 建任务条目，
// 再靠 transfer:progress / done / error 事件里的 job_id 匹配更新。
// 一旦服务层另造一个占位 id，任务条目就永远等不到自己的事件，
// 表现为「文件发出去了但进度条永远不动」。
func TestSendFileReturnsTheRealJobID(t *testing.T) {
	repo := newMemTransferRepo()
	tapp := app.NewTransferApp(selfID(t), repo, nil, stubConns{}, emptyDir{}, nil, clock.New(), nil)
	svc := &TransferService{d: Deps{Transfer: tapp}}

	path := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(path, []byte("swarmlink payload"), 0o644); err != nil {
		t.Fatalf("写测试文件: %v", err)
	}

	jobID, err := svc.SendFile(testPeerHex, path)
	if err != nil {
		t.Fatalf("SendFile: %v", err)
	}
	if jobID == "" {
		t.Fatal("SendFile 必须返回 job_id")
	}

	// 任务在后台 goroutine 里创建，轮询等它出现
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := repo.get(jobID); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("返回的 job_id %q 从未落库：界面拿到的 id 与真实任务对不上", jobID)
		}
		time.Sleep(10 * time.Millisecond)
	}

	job, _ := repo.get(jobID)
	if job.FileName != "payload.bin" {
		t.Errorf("job.FileName = %q, 期望 payload.bin", job.FileName)
	}
	if got := job.PeerID.String(); got != testPeerHex {
		t.Errorf("job.PeerID = %q, 期望 %q", got, testPeerHex)
	}
	if job.Direction != transfer.DirectionSend {
		t.Errorf("job.Direction = %q, 期望 %q", job.Direction, transfer.DirectionSend)
	}
	if job.FileSize != int64(len("swarmlink payload")) {
		t.Errorf("job.FileSize = %d", job.FileSize)
	}
}

// TestSendFileRejectsBadPathSynchronously 保证「路径打错 / 选到目录 / 空文件」
// 这三类失误在 RPC 返回前就报错。
//
// 否则它们会变成一个永远停在「排队中」的任务，用户只能靠猜。
// 注意 Deps 是零值：这些错误都必须在触碰 app 层之前返回，
// 真的走到 app 层会因 nil 解引用崩溃 —— 崩溃本身就是失败信号。
func TestSendFileRejectsBadPathSynchronously(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.bin")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatalf("写测试文件: %v", err)
	}

	cases := []struct {
		name string
		peer string
		path string
	}{
		{"文件不存在", testPeerHex, filepath.Join(dir, "nope.bin")},
		{"路径是目录", testPeerHex, dir},
		{"空文件", testPeerHex, empty},
		{"空路径", testPeerHex, ""},
		{"非法 NodeID", "not-a-node-id", empty},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &TransferService{}
			if _, err := svc.SendFile(tc.peer, tc.path); err == nil {
				t.Fatal("必须同步返回错误，而不是留一个跑不起来的任务")
			}
		})
	}
}
