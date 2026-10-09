package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/filesink"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// batchRootOf 从落库任务反推批次根目录：local_path 去掉 rel_path 的段数。
func TestBatchRootOf(t *testing.T) {
	cases := []struct {
		name   string
		job    transfer.Job
		want   string
		wantOK bool
	}{
		{
			name: "三层相对路径：首段就是批次根名，只退两层",
			job: transfer.Job{
				RelPath:   "照片/2024/a.jpg",
				LocalPath: filepath.Join("C:", "dl", "照片", "2024", "a.jpg"),
			},
			want:   filepath.Join("C:", "dl", "照片"),
			wantOK: true,
		},
		{
			name: "根目录条目（单段）：一层都不用退，local_path 本身就是根",
			job: transfer.Job{
				RelPath:   "空的",
				LocalPath: filepath.Join("C:", "dl", "空的"),
			},
			want:   filepath.Join("C:", "dl", "空的"),
			wantOK: true,
		},
		{
			name:   "单文件没有 rel_path → 推不出来",
			job:    transfer.Job{LocalPath: filepath.Join("C:", "dl", "a.txt")},
			wantOK: false,
		},
		{
			name:   "rel_path 非法（穿越）→ 推不出来，不能拿它当根",
			job:    transfer.Job{RelPath: "../../evil", LocalPath: filepath.Join("C:", "dl", "evil")},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := batchRootOf(tc.job)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v，期望 %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("root = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// 重启续传不能把同一个文件夹拆成两份：Recover 必须把已有的根目录认领回来。
func TestRecoverAdoptsBatchRoots(t *testing.T) {
	root := t.TempDir()
	sink, err := filesink.New(root)
	if err != nil {
		t.Fatal(err)
	}
	repo := &stubTransferRepo{}
	peerID := mustNodeID(t, testPeerHex)
	a := NewTransferApp(identity.NodeID{}, repo, sink, noConns{}, nil, eventbus.New(), clock.New(), nil)
	a.ChunkSize = 1024

	// 上一轮运行留下的痕迹：<root>/照片/a.jpg 已存在，库里有一条同批的接收任务
	if err := os.MkdirAll(filepath.Join(root, "照片"), 0o755); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(root, "照片", "a.jpg")
	if err := repo.UpsertJob(transfer.Job{
		JobID: "recv-1", PeerID: peerID, Direction: transfer.DirectionRecv,
		Status: transfer.StateTransferring, BatchID: "b1", RelPath: "照片/a.jpg",
		LocalPath: local, TempPath: local + ".part", ChunkSize: 1024, TotalChunks: 1,
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}

	a.Recover(context.Background())

	// 认领之后再收一个同批文件：必须落到同一个根，而不是新分配「照片 (1)」
	h, err := sink.CreateIn(ports.SinkRequest{JobID: "j2", BatchID: "b1", RelPath: "照片/b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "照片", "b.txt"); h.FinalPath != want {
		t.Errorf("认领后同批文件落在 %q，期望 %q", h.FinalPath, want)
	}
}

// 发送侧的任务【不能】被拿来认领：它的 local_path 是用户自己的源文件路径。
//
// 认领了就等于「把接收数据的落点指向用户源文件夹」，第二道防线是
// filesink.AdoptRoot 的包含性校验。这条用例把第一道也钉住。
func TestRecoverIgnoresSendSideRoots(t *testing.T) {
	root := t.TempDir()
	sink, err := filesink.New(root)
	if err != nil {
		t.Fatal(err)
	}
	repo := &stubTransferRepo{}
	peerID := mustNodeID(t, testPeerHex)
	a := NewTransferApp(identity.NodeID{}, repo, sink, noConns{}, nil, eventbus.New(), clock.New(), nil)

	// 一个发送侧任务，rel_path/local_path 指向用户自己的目录
	sourceRoot := t.TempDir()
	if err := repo.UpsertJob(transfer.Job{
		JobID: "send-1", PeerID: peerID, Direction: transfer.DirectionSend,
		Status: transfer.StateTransferring, BatchID: "b-send", RelPath: "照片/a.jpg",
		LocalPath: filepath.Join(sourceRoot, "照片", "a.jpg"),
		ChunkSize: 1024, TotalChunks: 1,
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}

	a.Recover(context.Background())

	// 同批再来一个文件：必须重新分配到接收目录里，而不是写到 sourceRoot
	h, err := sink.CreateIn(ports.SinkRequest{JobID: "j2", BatchID: "b-send", RelPath: "照片/b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(filepath.Dir(h.FinalPath)) != root {
		t.Fatalf("落点 %q 不在接收目录 %q 之下", h.FinalPath, root)
	}
	if _, err := os.Stat(filepath.Join(sourceRoot, "照片", "b.txt")); !os.IsNotExist(err) {
		t.Error("不该往发送侧的源目录里写任何东西")
	}
}
