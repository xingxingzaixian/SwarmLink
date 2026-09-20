package mem

import (
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/group"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
)

func mkID(b byte) identity.NodeID {
	var n identity.NodeID
	n[7] = b
	return n
}

func newJob(id string, peerID identity.NodeID, hash string) transfer.Job {
	const chunk = int64(512 * 1024)
	size := chunk*4 + 10
	total := transfer.ChunkCount(size, chunk)
	return transfer.Job{
		JobID:       id,
		PeerID:      peerID,
		FileName:    "f.bin",
		FileSize:    size,
		FileHash:    hash,
		ChunkSize:   chunk,
		TotalChunks: total,
		Direction:   transfer.DirectionSend,
		Bitmap:      transfer.NewChunkBitmap(total),
		Status:      transfer.StateTransferring,
		CreatedAt:   time.Unix(0, 0),
	}
}

func TestMessagesAppendIfAbsentAndLatest(t *testing.T) {
	m := NewMessages(clock.New())
	base := time.Unix(1000, 0)

	for i, id := range []string{"m1", "m2", "m3"} {
		inserted, err := m.AppendIfAbsent(message.Message{
			MsgID: id, ConvID: "c1", Direction: message.DirectionIn,
			Content: id, SentAt: base.Add(time.Duration(i) * time.Second),
		})
		if err != nil || !inserted {
			t.Fatalf("%s: inserted=%v err=%v", id, inserted, err)
		}
	}
	// 幂等：重复写入不插入
	if inserted, _ := m.AppendIfAbsent(message.Message{MsgID: "m1", ConvID: "c1"}); inserted {
		t.Fatal("duplicate msg_id must not insert")
	}
	if m.Count() != 3 {
		t.Fatalf("want 3 messages got %d", m.Count())
	}

	got, err := m.Latest("c1", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].MsgID != "m3" || got[1].MsgID != "m2" {
		t.Fatalf("Latest must return newest first, got %+v", got)
	}
}

func TestAppendOutgoingAndDeliverAreAtomic(t *testing.T) {
	m := NewMessages(clock.New())
	now := time.Unix(2000, 0)

	msg := message.Message{MsgID: "o1", ConvID: "c1", Direction: message.DirectionOut, Content: "hi", SentAt: now}
	if err := m.AppendOutgoing(msg, now); err != nil {
		t.Fatal(err)
	}
	if m.OutboxLen() != 1 {
		t.Fatal("outbox must contain the pending message")
	}
	stored, _ := m.Get("o1")
	if stored.State != message.StatePending {
		t.Fatalf("state must be pending, got %s", stored.State)
	}

	due, err := m.Due(now, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("due want 1 got %v err=%v", due, err)
	}

	if err := m.Deliver("o1"); err != nil {
		t.Fatal(err)
	}
	if m.OutboxLen() != 0 {
		t.Fatal("outbox must be cleared after delivery")
	}
	stored, _ = m.Get("o1")
	if stored.State != message.StateDelivered {
		t.Fatalf("state must be delivered, got %s", stored.State)
	}
}

func TestOutboxBackoff(t *testing.T) {
	m := NewMessages(clock.New())
	now := time.Unix(3000, 0)
	_ = m.Enqueue("m1", "c1", now)

	due, _ := m.Due(now, 10)
	if len(due) != 1 || due[0].Attempts != 0 {
		t.Fatalf("unexpected due: %+v", due)
	}

	later := now.Add(10 * time.Second)
	_ = m.BumpAttempt("m1", later)

	if due, _ := m.Due(now, 10); len(due) != 0 {
		t.Fatal("must not be due before next_try_at")
	}
	due, _ = m.Due(later, 10)
	if len(due) != 1 || due[0].Attempts != 1 {
		t.Fatalf("want attempts=1 got %+v", due)
	}
}

func TestPeersDirectory(t *testing.T) {
	fc := clock.NewFake(time.Unix(4000, 0))
	p := NewPeers(fc)

	if err := p.Upsert(peer.Peer{NodeID: mkID(1), DisplayName: "alice", State: peer.StateOnline, Source: peer.SourceBroadcast}); err != nil {
		t.Fatal(err)
	}
	got, ok := p.Get(mkID(1))
	if !ok || got.DisplayName != "alice" {
		t.Fatalf("get failed: %+v", got)
	}
	if !p.SeenRecently(mkID(1), time.Minute) {
		t.Fatal("should be seen recently")
	}

	// TTL：推进超过窗口后不再算「最近见到」
	fc.Advance(2 * time.Minute)
	if p.SeenRecently(mkID(1), time.Minute) {
		t.Fatal("window expired")
	}

	// 状态过滤
	_ = p.Upsert(peer.Peer{NodeID: mkID(2), State: peer.StateDiscovered})
	if list := p.List(ports.PeerFilter{OnlineOnly: true}); len(list) != 1 || list[0].NodeID != mkID(1) {
		t.Fatalf("online filter wrong: %+v", list)
	}
	if list := p.List(ports.PeerFilter{Source: peer.SourceBroadcast}); len(list) != 1 {
		t.Fatalf("source filter wrong: %+v", list)
	}

	// 离线标记保留条目
	if err := p.MarkOffline(mkID(1), fc.Now()); err != nil {
		t.Fatal(err)
	}
	got, ok = p.Get(mkID(1))
	if !ok || got.State != peer.StateOffline {
		t.Fatalf("mark offline failed: %+v", got)
	}
}

func TestGroupsRepo(t *testing.T) {
	g := NewGroups()
	owner := mkID(1)
	grp := group.Group{
		ID: "g1", Name: "team", OwnerID: owner, Epoch: 1, CreatedAt: time.Unix(0, 0),
		Members: []group.Member{{NodeID: owner, Role: group.RoleOwner, State: group.MemberActive}},
	}
	if err := g.Upsert(grp); err != nil {
		t.Fatal(err)
	}
	back, ok := g.Get("g1")
	if !ok || back.Name != "team" || len(back.Members) != 1 {
		t.Fatalf("get wrong: %+v", back)
	}
	// 返回的副本不得与内部共享切片
	back.Members[0].DisplayName = "mutated"
	again, _ := g.Get("g1")
	if again.Members[0].DisplayName == "mutated" {
		t.Fatal("Groups must return deep copies")
	}
	if len(g.List()) != 1 {
		t.Fatal("list wrong")
	}
}

func TestTransferRepoResumable(t *testing.T) {
	m := NewMessages(clock.New())
	peerID := mkID(7)
	job := newJob("j1", peerID, "hash1")
	if err := m.UpsertJob(job); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.FindResumable(peerID, "hash1"); !ok {
		t.Fatal("should find resumable job")
	}
	if _, ok := m.FindResumable(peerID, "other"); ok {
		t.Fatal("must not match different hash")
	}

	// 位图落盘后读取一致
	bm := job.Bitmap
	bm.Set(0)
	if err := m.SaveBitmap("j1", bm, 1); err != nil {
		t.Fatal(err)
	}
	back, err := m.GetJob("j1")
	if err != nil {
		t.Fatal(err)
	}
	if !back.Bitmap.IsSet(0) || back.Completed != 1 {
		t.Fatalf("bitmap not persisted: %+v", back)
	}

	// 完成后不再可续传
	job.Status = transfer.StateDone
	_ = m.UpsertJob(job)
	if _, ok := m.FindResumable(peerID, "hash1"); ok {
		t.Fatal("completed job must not be resumable")
	}
	if list, _ := m.ListActive(); len(list) != 0 {
		t.Fatalf("no active jobs expected, got %d", len(list))
	}
}
