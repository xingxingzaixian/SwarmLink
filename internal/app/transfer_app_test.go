package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// stubTransferRepo 只记录落库，够本测试断言「失败也要留痕」。
//
// 必须自带互斥：传输是「一个任务一个 goroutine」的，而测试往往在另一个
// goroutine 里轮询状态 —— 裸 map 在 -race 下就是数据竞争。
type stubTransferRepo struct {
	mu   sync.Mutex
	jobs map[string]transfer.Job
}

func (s *stubTransferRepo) UpsertJob(j transfer.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs == nil {
		s.jobs = map[string]transfer.Job{}
	}
	s.jobs[j.JobID] = j
	return nil
}

func (s *stubTransferRepo) GetJob(jobID string) (transfer.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return transfer.Job{}, errors.New("not found")
	}
	return j, nil
}

// FindResumable 实现【真实语义】而不是恒返回 false。
//
// 恒 false 会让「批次内同 hash 的两个文件互相覆盖」这类用例变成空测：
// 没有命中的可能，断言当然通过。这里照抄端口契约（含 rel_path 维度），
// 让用例验证的是调用方有没有把 rel_path 传对。
func (s *stubTransferRepo) FindResumable(peerID identity.NodeID, fileHash, relPath string) (transfer.Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.PeerID != peerID || j.FileHash != fileHash {
			continue
		}
		if j.Status == transfer.StateDone || j.Status == transfer.StateCancelled {
			continue
		}
		if relPath != "" && j.RelPath != relPath {
			continue
		}
		return j, true
	}
	return transfer.Job{}, false
}

func (s *stubTransferRepo) SaveBitmap(string, transfer.ChunkBitmap, int64) error { return nil }

// all 返回 stub 里按 job_id 排序的全部任务。
//
// 两个列表方法都刻意【不】按领域语义过滤：测试要能塞进任意状态的任务，
// 由被测代码自己判断该不该动它 —— 过滤写在 stub 里等于把被测逻辑
// 抄一遍，测试就变成自己验证自己。
func (s *stubTransferRepo) all() []transfer.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]transfer.Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		if j.JobID == "" {
			continue
		}
		out = append(out, j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].JobID < out[k].JobID })
	return out
}

func (s *stubTransferRepo) ListActive() ([]transfer.Job, error)  { return s.all(), nil }
func (s *stubTransferRepo) ListWaiting() ([]transfer.Job, error) { return s.all(), nil }

// ListByBatch 刻意【不】像上面两个那样返回全部。
//
// 整体取消的语义完全依赖「只动这一批」：一个什么都返回的替身，会让
// 「取消 b1 却把 b2 的任务也停了」这种错误在测试里照样通过 ——
// 那种测试比没有测试更糟。
func (s *stubTransferRepo) ListByBatch(batchID string) ([]transfer.Job, error) {
	if batchID == "" {
		// 与两个真实实现同一条守卫：替身不镜像它，Task 8 的 CancelBatch("")
		// 用例就会拿到「返回全部单文件任务」的假绿灯。
		return nil, fmt.Errorf("%w: stub", ports.ErrEmptyBatchID)
	}
	out := make([]transfer.Job, 0)
	for _, j := range s.all() {
		if j.BatchID == batchID {
			out = append(out, j)
		}
	}
	return out, nil
}

func (s *stubTransferRepo) ListRecent(limit int) ([]transfer.Job, error) {
	return s.all(), nil
}

func (s *stubTransferRepo) PurgeFinished(keep int, olderThan time.Time) (int, error) {
	return 0, nil
}

// 锁定「发送失败必须给出反馈」：任何失败路径都要发出 transfer:error，
// 且携带调用方（TransferService.SendFile）已经交给界面的那个 job_id。
//
// 修复前只有 runSender 的错误会发事件；像「文件打不开」这种在建任务【之前】
// 就失败的路径是裸 return，而调用方又把错误丢给了 `_` ——
// 界面只建了一条「排队中」的任务，然后永远等不到任何反馈，
// 用户看到的就是「点了发送文件，实际并没有发送」。
func TestSendFileAsPublishesErrorOnEarlyFailure(t *testing.T) {
	bus := eventbus.New()
	got := make(chan eventbus.TransferError, 1)
	bus.Subscribe(eventbus.TopicTransferError, func(p any) {
		ev, ok := p.(eventbus.TransferError)
		if !ok {
			return
		}
		select {
		case got <- ev:
		default:
		}
	})

	repo := &stubTransferRepo{}
	a := NewTransferApp(identity.NodeID{}, repo, nil, nil, nil, bus, clock.New(), nil)

	const jobID = "job-early-failure"
	// 文件不存在 → 本地检查（os.Stat）就失败，连可达性判定与哈希都走不到
	if _, err := a.SendFileAs(context.Background(), identity.NodeID{}, "/definitely/not/here.bin", jobID); err == nil {
		t.Fatal("expected an error for a missing file")
	}

	select {
	case ev := <-got:
		if ev.JobID != jobID {
			t.Fatalf("error event carries wrong job id: %q", ev.JobID)
		}
		if ev.Err == nil {
			t.Fatal("error event must carry the underlying error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("失败路径没有发出 transfer:error —— 界面会一直卡在「排队中」")
	}

	// 失败也要落库，否则刷新列表后任务凭空消失
	j, err := repo.GetJob(jobID)
	if err != nil {
		t.Fatalf("failed job not persisted: %v", err)
	}
	if j.Status != transfer.StateFailed {
		t.Fatalf("status = %q, want %q", j.Status, transfer.StateFailed)
	}
}

// 成功路径不能误发 error 事件（统一出口只在 err != nil 时发）。
func TestSendFileAsPublishesNoErrorWhenJobIDMissing(t *testing.T) {
	bus := eventbus.New()
	bus.Subscribe(eventbus.TopicTransferError, func(any) {
		t.Error("must not publish transfer.error when job_id is empty")
	})

	a := NewTransferApp(identity.NodeID{}, &stubTransferRepo{}, nil, nil, nil, bus, clock.New(), nil)
	// job_id 为空在函数最前面就被拒绝，早于统一出口注册，因此不应发事件
	if _, err := a.SendFileAs(context.Background(), identity.NodeID{}, "/whatever", ""); err == nil {
		t.Fatal("expected an error for empty job_id")
	}
}
