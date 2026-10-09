package mem

import (
	"errors"
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

// ListWaiting 只认「发送方向 + 可自动拉起的原因」，且按创建时间返回。
//
// 这条守卫的意义：sweep 拿它去自动重试，多返回一条就是把不该动的任务
// 一起拉起来；顺序错了则慢来的文件插队，用户看到的传输次序与发起次序不符。
//
// 这里与 sqlite 侧的 TestListWaitingCoversEveryAutoStartableReason 是镜像，
// 两边必须同时包含 WaitPeerOffline 与 WaitQueued —— 一个实现漏了，
// 就会出现「只在其中一种存储下，排队中的文件永远不动」。
func TestListWaitingFiltersByReasonAndDirection(t *testing.T) {
	m := NewMessages(clock.New())
	peerID := mkID(7)

	mk := func(id string, st transfer.State, reason transfer.WaitReason, dir transfer.Direction, at int64) transfer.Job {
		j := newJob(id, peerID, "hash-"+id)
		j.Status, j.WaitReason, j.Direction = st, reason, dir
		j.CreatedAt, j.UpdatedAt = time.Unix(at, 0), time.Unix(at, 0)
		return j
	}

	for _, j := range []transfer.Job{
		mk("wait-first", transfer.StateIdle, transfer.WaitPeerOffline, transfer.DirectionSend, 10),
		mk("wait-second", transfer.StateIdle, transfer.WaitPeerOffline, transfer.DirectionSend, 20),
		mk("wait-queued", transfer.StateIdle, transfer.WaitQueued, transfer.DirectionSend, 25),
		mk("idle-no-reason", transfer.StateIdle, transfer.WaitNone, transfer.DirectionSend, 30),
		mk("running", transfer.StateTransferring, transfer.WaitNone, transfer.DirectionSend, 40),
		mk("wait-recv", transfer.StateIdle, transfer.WaitPeerOffline, transfer.DirectionRecv, 50),
		mk("failed", transfer.StateFailed, transfer.WaitNone, transfer.DirectionSend, 60),
	} {
		if err := m.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}

	got, err := m.ListWaiting()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("ListWaiting 返回 %d 条，want 3：%+v", len(got), got)
	}
	if got[0].JobID != "wait-first" || got[1].JobID != "wait-second" || got[2].JobID != "wait-queued" {
		t.Fatalf("ListWaiting 顺序 = %v，want wait-first/wait-second/wait-queued（按创建时间）", jobIDsOf(got))
	}
}

// jobIDsOf 便于断言任务顺序（与 sqlite 侧的同名辅助分开定义：两者是不同包）。
func jobIDsOf(jobs []transfer.Job) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.JobID)
	}
	return out
}

// 同刻按目录树顺序（镜像 sqlite 侧）：刻意让 job_id 顺序与 rel_path 顺序相反。
//
// map 迭代顺序是随机的，没有第二/第三排序键时这条用例会随机红 ——
// 它同时守着「内存实现与 sqlite 给出同一种顺序」这条对等性。
func TestListByBatchOrdersByRelPath(t *testing.T) {
	m := NewMessages(clock.New())
	peerID := mkID(9)
	same := time.Unix(7, 0)
	for _, spec := range []struct{ id, rel string }{
		{"jz", "照片/a.txt"}, {"ja", "照片/m.txt"}, {"jm", "照片/z.txt"},
	} {
		j := newJob(spec.id, peerID, "hash-"+spec.id)
		j.BatchID, j.RelPath = "b-tie", spec.rel
		j.CreatedAt, j.UpdatedAt = same, same
		if err := m.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}
	got, err := m.ListByBatch("b-tie")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"照片/a.txt", "照片/m.txt", "照片/z.txt"}
	if len(got) != len(want) {
		t.Fatalf("条数 = %d，期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i].RelPath != want[i] {
			t.Fatalf("同刻顺序异常，期望按 rel_path（目录树）%v", want)
		}
	}
}

// ListByBatch 与 sqlite 侧镜像：只返回本批、包含已结束的、空 id 报错。
func TestListByBatchFiltersAndKeepsFinished(t *testing.T) {
	m := NewMessages(clock.New())
	peerID := mkID(8)

	mk := func(id, batch string, st transfer.State, rel string) transfer.Job {
		j := newJob(id, peerID, "hash-"+id)
		j.BatchID, j.Status, j.RelPath = batch, st, rel
		return j
	}
	for _, j := range []transfer.Job{
		mk("b1-a", "b1", transfer.StateDone, "照片/a.txt"),
		mk("b1-b", "b1", transfer.StateIdle, "照片/b.txt"),
		mk("b2-a", "b2", transfer.StateIdle, "别的/a.txt"),
		mk("solo", "", transfer.StateIdle, ""),
	} {
		if err := m.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}

	got, err := m.ListByBatch("b1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("返回 %d 条，want 2（不能带上别的批或单文件）：%v", len(got), jobIDsOf(got))
	}
	hasDone := false
	for _, j := range got {
		if j.Status == transfer.StateDone {
			hasDone = true
		}
	}
	if !hasDone {
		t.Error("批次必须包含已结束的任务，否则卡片上的 N 会永远对不上")
	}

	// 空 batch_id 必须报错：否则 CancelBatch("") 会把本机全部单文件传输一起停掉
	if _, err := m.ListByBatch(""); err == nil {
		t.Fatal("空 batch_id 必须报错")
	} else if !errors.Is(err, ports.ErrEmptyBatchID) {
		t.Errorf("错误应可用 errors.Is(ports.ErrEmptyBatchID) 判别（与 sqlite 实现一致），得到 %v", err)
	}
}

// 同刻并列时按目录树排（镜像 sqlite 侧），并顺带钉住 ListActive 用的同一套顺序。
//
// job_id 顺序与 rel_path 顺序刻意【相反】：同向的话删掉第二键也照样绿，
// 那等于没有测试 —— 本用例第一版正是这么写错的（同刻数据全取 a、z）。
func TestListWaitingAndActiveOrderByTreePathWhenSameTick(t *testing.T) {
	m := NewMessages(clock.New())
	peerID := mkID(11)
	same := time.Unix(11, 0)
	for _, spec := range []struct{ id, rel string }{
		{"j1", "照片/z.txt"}, // job_id 更小，树序在后
		{"j2", "照片/a.txt"}, // job_id 更大，树序在前
	} {
		j := newJob(spec.id, peerID, "hash-"+spec.id)
		j.Status, j.WaitReason, j.Direction = transfer.StateIdle, transfer.WaitQueued, transfer.DirectionSend
		j.BatchID, j.RelPath = "b-order", spec.rel
		j.CreatedAt, j.UpdatedAt = same, same
		if err := m.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}

	waiting, err := m.ListWaiting()
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 2 || waiting[0].JobID != "j2" || waiting[1].JobID != "j1" {
		t.Fatalf("ListWaiting 顺序 = %v，期望 j2,j1（同刻按目录树）", jobIDsOf(waiting))
	}

	active, err := m.ListActive()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 || active[0].JobID != "j2" || active[1].JobID != "j1" {
		t.Fatalf("ListActive 顺序 = %v，期望与 ListWaiting 同一套顺序 j2,j1", jobIDsOf(active))
	}
}

func TestTransferRepoResumable(t *testing.T) {
	m := NewMessages(clock.New())
	peerID := mkID(7)
	job := newJob("j1", peerID, "hash1")
	if err := m.UpsertJob(job); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.FindResumable(peerID, "hash1", ""); !ok {
		t.Fatal("should find resumable job")
	}
	if _, ok := m.FindResumable(peerID, "other", ""); ok {
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
	if _, ok := m.FindResumable(peerID, "hash1", ""); ok {
		t.Fatal("completed job must not be resumable")
	}
	if list, _ := m.ListActive(); len(list) != 0 {
		t.Fatalf("no active jobs expected, got %d", len(list))
	}
}

// 批次内同 hash 的两个文件必须互不命中（镜像 sqlite 侧的 TestFindResumableSeparatesByRelPath）。
//
// 独立一个用例而不是塞进上面那个：上面的用例结尾断言 ListActive 为空，
// 塞进去的批次任务会让那条断言变成噪声。
func TestFindResumableSeparatesByRelPath(t *testing.T) {
	m := NewMessages(clock.New())
	peerID := mkID(12)
	for _, spec := range []struct{ id, rel string }{
		{"j2", "照片/备份/a.txt"}, {"j3", "照片/a.txt"},
	} {
		j := newJob(spec.id, peerID, "same-hash")
		j.BatchID, j.RelPath, j.TempPath = "b1", spec.rel, "/tmp/"+spec.id+".part"
		if err := m.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}

	got, ok := m.FindResumable(peerID, "same-hash", "照片/备份/a.txt")
	if !ok || got.JobID != "j2" {
		t.Fatalf("续传命中 = %s ok=%v，期望 j2（必须按 rel_path 区分）", got.JobID, ok)
	}
	if _, ok := m.FindResumable(peerID, "same-hash", "照片/没有这个.txt"); ok {
		t.Error("不同的 rel_path 不该命中")
	}
	// relPath 为空（单文件）保持 (peer, hash) 的旧语义
	if _, ok := m.FindResumable(peerID, "same-hash", ""); !ok {
		t.Error("relPath 为空时应保持旧语义")
	}
}

// 与 sqlite 侧同一组断言：两个实现必须对等，否则 app 层测试（用 mem）通过
// 而真实运行（用 sqlite）出错，是最难查的一类漂移。
func TestConvClearRoundTripAndOverwrite(t *testing.T) {
	m := NewMessages(clock.New())

	if _, ok, err := m.ConvClearedAt("c1"); err != nil || ok {
		t.Fatalf("未清空时应 ok=false，得到 ok=%v err=%v", ok, err)
	}

	if err := m.SetConvCleared("c1", time.UnixMilli(1_000)); err != nil {
		t.Fatalf("SetConvCleared: %v", err)
	}
	got, ok, err := m.ConvClearedAt("c1")
	if err != nil || !ok || got.UnixMilli() != 1_000 {
		t.Fatalf("读回 = %v ok=%v err=%v，期望 1000/true", got, ok, err)
	}

	if err := m.SetConvCleared("c1", time.UnixMilli(2_000)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := m.ConvClearedAt("c1"); got.UnixMilli() != 2_000 {
		t.Fatalf("二次清空未覆盖，仍是 %d", got.UnixMilli())
	}

	if _, ok, _ := m.ConvClearedAt("c2"); ok {
		t.Fatal("清空 c1 不应影响 c2")
	}

	for i := 0; i < 2; i++ {
		if err := m.RemoveConvClear("c1"); err != nil {
			t.Fatalf("RemoveConvClear(第 %d 次): %v", i+1, err)
		}
	}
	if _, ok, _ := m.ConvClearedAt("c1"); ok {
		t.Fatal("撤销后仍读到清空点")
	}
}
