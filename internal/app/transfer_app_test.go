package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// stubTransferRepo 只记录落库，够本测试断言「失败也要留痕」。
type stubTransferRepo struct{ jobs map[string]transfer.Job }

func (s *stubTransferRepo) UpsertJob(j transfer.Job) error {
	if s.jobs == nil {
		s.jobs = map[string]transfer.Job{}
	}
	s.jobs[j.JobID] = j
	return nil
}

func (s *stubTransferRepo) GetJob(jobID string) (transfer.Job, error) {
	j, ok := s.jobs[jobID]
	if !ok {
		return transfer.Job{}, errors.New("not found")
	}
	return j, nil
}

func (s *stubTransferRepo) FindResumable(identity.NodeID, string) (transfer.Job, bool) {
	return transfer.Job{}, false
}

func (s *stubTransferRepo) SaveBitmap(string, transfer.ChunkBitmap, int64) error { return nil }

func (s *stubTransferRepo) ListActive() ([]transfer.Job, error)         { return nil, nil }
func (s *stubTransferRepo) ListRecent(limit int) ([]transfer.Job, error) { return nil, nil }

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
	// 文件不存在 → hashFileChunks 在建任务之前就失败（连 session 都没走到）
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
