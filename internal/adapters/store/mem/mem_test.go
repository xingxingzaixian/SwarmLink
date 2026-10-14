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

	// 状态过滤：OnlineOnly 含 online 与 discovered ——「正在广播但还没握手」也算「在」。
	// 与 sqlite 实现一致：这里原先只认 online，是评审发现的跨实现分歧（分组扇出正是
	// 靠这个集合决定「值得拨号的对象」，把 discovered 排除会让它们收不到群消息）。
	_ = p.Upsert(peer.Peer{NodeID: mkID(2), State: peer.StateDiscovered})
	online := map[identity.NodeID]bool{}
	for _, got := range p.List(ports.PeerFilter{OnlineOnly: true}) {
		online[got.NodeID] = true
	}
	// 断言集合而不是顺序：两条的 last_seen 相同，排序是未定义的
	if len(online) != 2 || !online[mkID(1)] || !online[mkID(2)] {
		t.Fatalf("OnlineOnly 应返回 {1,2}（online + discovered），得到 %+v", online)
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

// 启动清理：只降级 online，保留 last_seen，不动 discovered/offline/blocked
// （与 sqlite 实现必须一致，两者互为替身）。
func TestPeersResetLivenessClearsOnlyOnline(t *testing.T) {
	fc := clock.NewFake(time.Unix(4000, 0))
	p := NewPeers(fc)

	_ = p.Upsert(peer.Peer{NodeID: mkID(1), State: peer.StateOnline, LastSeen: time.Unix(3000, 0)})
	_ = p.Upsert(peer.Peer{NodeID: mkID(2), State: peer.StateDiscovered, LastSeen: time.Unix(3000, 0)})
	_ = p.Upsert(peer.Peer{NodeID: mkID(3), State: peer.StateOffline, LastSeen: time.Unix(3000, 0)})
	_ = p.Upsert(peer.Peer{NodeID: mkID(4), State: peer.StateOnline, LastSeen: time.Unix(3000, 0)})
	_ = p.SetSuppressed(mkID(4), true, time.Unix(3500, 0))

	n, err := p.ResetLiveness()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应只降级 1 条 online，得到 %d", n)
	}
	got, _ := p.Get(mkID(1))
	if got.State != peer.StateDiscovered {
		t.Fatalf("online 应降级为 discovered，得到 %q", got.State)
	}
	// last_seen 必须保留：TTL 兜底据此判断条目是否已过期。写成 now 会把失联伪装成刚见过。
	if !got.LastSeen.Equal(time.Unix(3000, 0)) {
		t.Fatalf("last_seen 必须保留，得到 %v", got.LastSeen)
	}
	if g, _ := p.Get(mkID(2)); g.State != peer.StateDiscovered {
		t.Fatalf("discovered 不该被动，得到 %q", g.State)
	}
	if g, _ := p.Get(mkID(3)); g.State != peer.StateOffline {
		t.Fatalf("offline 不该被动，得到 %q", g.State)
	}
	if g, _ := p.Get(mkID(4)); g.State != peer.StateBlocked {
		t.Fatalf("blocked 是粘性状态，不该被动，得到 %q", g.State)
	}

	// 幂等：没有 online 了
	if n2, _ := p.ResetLiveness(); n2 != 0 {
		t.Fatalf("第二次应返回 0，得到 %d", n2)
	}
}

// 与 sqlite 实现保持同一份语义：可打可解、不存在的行是空操作。
// 两个实现的行为必须一致 —— 它们互作替身（R2 允许测试跨引适配器正是为此）。
func TestSetSuppressedRoundTrip(t *testing.T) {
	fc := clock.NewFake(time.Unix(4000, 0))
	p := NewPeers(fc)

	id := mkID(7)
	if err := p.Upsert(peer.Peer{NodeID: id, DisplayName: "ghost", State: peer.StateOffline}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetSuppressed(id, true, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	got, ok := p.Get(id)
	if !ok || got.State != peer.StateBlocked {
		t.Fatalf("期望 blocked，得到 %+v", got)
	}
	if got.DisplayName != "ghost" {
		t.Fatalf("标记不该丢名字：%+v", got)
	}
	if !got.LastSeen.Equal(time.Unix(1000, 0)) {
		t.Fatalf("last_seen 应记下标记时刻，得到 %v", got.LastSeen)
	}
	if err := p.SetSuppressed(id, false, time.Unix(2000, 0)); err != nil {
		t.Fatal(err)
	}
	got, _ = p.Get(id)
	if got.State == peer.StateBlocked {
		t.Fatal("解除后不该还是 blocked")
	}
	if !got.LastSeen.Equal(time.Unix(2000, 0)) {
		t.Fatalf("解除应刷新 last_seen，得到 %v", got.LastSeen)
	}

	// 与 sqlite 实现一致：解除只对「确实被移除」的行生效，不降级在线节点
	if err := p.Upsert(peer.Peer{NodeID: id, DisplayName: "ghost", State: peer.StateOnline, LastSeen: time.Unix(3000, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetSuppressed(id, false, time.Unix(3100, 0)); err != nil {
		t.Fatal(err)
	}
	got, _ = p.Get(id)
	if got.State != peer.StateOnline {
		t.Fatalf("解除不该动一个本来就不是 blocked 的行：%+v", got)
	}
	if !got.LastSeen.Equal(time.Unix(3000, 0)) {
		t.Fatalf("非 blocked 行的解除必须是彻底空操作（last_seen 也不该动），得到 %v", got.LastSeen)
	}

	if err := p.SetSuppressed(mkID(9), true, time.Unix(1, 0)); err != nil {
		t.Fatalf("对不存在的行不该报错：%v", err)
	}
	if _, ok := p.Get(mkID(9)); ok {
		t.Fatal("不该凭空造出一行")
	}
}

// 粘性：被移除的节点不能被任何写入路径「复活」（与 sqlite 实现同一份语义）。
func TestBlockedStateIsStickyAcrossWrites(t *testing.T) {
	fc := clock.NewFake(time.Unix(4000, 0))
	p := NewPeers(fc)

	id := mkID(3)
	if err := p.Upsert(peer.Peer{NodeID: id, State: peer.StateOffline}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetSuppressed(id, true, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}

	if err := p.Upsert(peer.Peer{NodeID: id, DisplayName: "ghost", State: peer.StateDiscovered}); err != nil {
		t.Fatal(err)
	}
	got, _ := p.Get(id)
	if got.State != peer.StateBlocked {
		t.Fatalf("Upsert 覆盖了移除标记：%+v", got)
	}
	if got.DisplayName != "ghost" {
		t.Fatalf("非状态字段应当照常更新：%+v", got)
	}

	if err := p.MarkOffline(id, time.Unix(1200, 0)); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Get(id); got.State != peer.StateBlocked {
		t.Fatalf("MarkOffline 覆盖了移除标记：%+v", got)
	}
}

// 默认 List 不返回被移除的节点；显式 IncludeBlocked / 显式 State 查询仍能看到。
func TestListExcludesBlockedByDefault(t *testing.T) {
	fc := clock.NewFake(time.Unix(4000, 0))
	p := NewPeers(fc)

	kept, gone := mkID(4), mkID(5)
	for _, id := range []identity.NodeID{kept, gone} {
		if err := p.Upsert(peer.Peer{NodeID: id, State: peer.StateOffline}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.SetSuppressed(gone, true, fc.Now()); err != nil {
		t.Fatal(err)
	}

	all := p.List(ports.PeerFilter{})
	if len(all) != 1 || all[0].NodeID != kept {
		t.Fatalf("默认 List 应只剩 1 条，得到 %+v", all)
	}
	if with := p.List(ports.PeerFilter{IncludeBlocked: true}); len(with) != 2 {
		t.Fatalf("IncludeBlocked 应返回 2 条，得到 %d", len(with))
	}
	if only := p.List(ports.PeerFilter{State: peer.StateBlocked}); len(only) != 1 || only[0].NodeID != gone {
		t.Fatalf("显式按 blocked 查询应返回 1 条，得到 %+v", only)
	}
}

// OnlineOnly 的语义：online 与 discovered 都算「在」（与 sqlite 实现必须一致）。
func TestListOnlineOnlyIncludesDiscovered(t *testing.T) {
	fc := clock.NewFake(time.Unix(4000, 0))
	p := NewPeers(fc)

	for i, st := range []peer.State{peer.StateOnline, peer.StateDiscovered, peer.StateOffline} {
		if err := p.Upsert(peer.Peer{NodeID: mkID(byte(i + 1)), State: st}); err != nil {
			t.Fatal(err)
		}
	}

	got := p.List(ports.PeerFilter{OnlineOnly: true})
	if len(got) != 2 {
		t.Fatalf("OnlineOnly 应包含 online 与 discovered 共 2 条，得到 %d: %+v", len(got), got)
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

// 票号与自签的往返 + State 派生缓存刷新。
//
// 与 sqlite 侧的 TestGroupMemberTokensRoundTrip 是镜像：两边必须同时保留
// admit/kick/left 与 left_sig，并同样按票号刷新 State —— 少做一样，
// 就会出现「mem 跑测试绿、sqlite 跑生产错」这类最难查的漂移。
func TestGroupsRepoTokensAndDerivedState(t *testing.T) {
	g := NewGroups()
	owner, m2 := mkID(1), mkID(2)
	grp := group.Group{
		ID: "g1", Name: "群", OwnerID: owner, Epoch: 7, MintSeq: 13,
		Members: []group.Member{
			{NodeID: owner, Role: group.RoleOwner, AdmitSeq: 1},
			// 13 > 9 ⇒ 在群（退出后被重新拉回）；State 故意留空，应由票号推出。
			{NodeID: m2, Role: group.RoleMember, AdmitSeq: 13, LeftSeq: 9, LeftSig: []byte{9, 9}},
		},
	}
	if err := g.Upsert(grp); err != nil {
		t.Fatal(err)
	}
	back, _ := g.Get("g1")
	if back.MintSeq != 13 {
		t.Errorf("MintSeq 未往返：%d", back.MintSeq)
	}
	m, ok := back.Member(m2)
	if !ok {
		t.Fatal("成员记录丢失")
	}
	if m.AdmitSeq != 13 || m.LeftSeq != 9 || len(m.LeftSig) != 2 {
		t.Errorf("票号未往返：%+v", m)
	}
	if !m.Active() {
		t.Error("admit=13 > left=9 ⇒ 应是在群")
	}
	if m.State != group.MemberActive {
		t.Errorf("State 派生缓存 = %q，期望 active", m.State)
	}

	// 撕到同一号 ⇒ 不在群，且缓存应变成 left。
	grp.Members[1].AdmitSeq = 9
	if err := g.Upsert(grp); err != nil {
		t.Fatal(err)
	}
	back, _ = g.Get("g1")
	m, _ = back.Member(m2)
	if m.Active() {
		t.Error("admit == left ⇒ 必须是不在群")
	}
	if m.State != group.MemberLeft {
		t.Errorf("State 派生缓存 = %q，期望 left", m.State)
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
