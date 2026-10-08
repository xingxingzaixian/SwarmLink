package sqlite

import (
	"bytes"
	"database/sql"
	"path/filepath"
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

func openAt(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	return db
}

func openTest(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.db")
	db := openAt(t, path)
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func TestMigrationsAreReentrant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")

	db1 := openAt(t, path)
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}
	// 第二次打开同一文件：迁移必须被识别为「已应用」并跳过
	db2 := openAt(t, path)
	defer func() { _ = db2.Close() }()

	var n int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(migrations) {
		t.Fatalf("want %d applied migrations got %d", len(migrations), n)
	}

	// 表都建好了
	for _, table := range []string{"conversations", "messages", "outbox", "groups",
		"group_members", "peers", "transfer_jobs", "seed_nodes", "conv_clears"} {
		var name string
		err := db2.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
}

func TestForeignKeysAreEnabled(t *testing.T) {
	db, _ := openTest(t)
	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatal("foreign_keys must be ON (Go driver defaults to OFF)")
	}
}

func TestAppendIfAbsentIsIdempotent(t *testing.T) {
	db, _ := openTest(t)
	m := NewMessages(db)

	msg := message.Message{
		MsgID: "m1", ConvID: message.DirectConvID(mkID(1), mkID(2)),
		SenderID: mkID(1), Direction: message.DirectionIn, Content: "hi",
		MsgType: message.MsgTypeText, SentAt: time.Unix(1000, 0), State: message.StateDelivered,
	}
	inserted, err := m.AppendIfAbsent(msg)
	if err != nil || !inserted {
		t.Fatalf("first insert: inserted=%v err=%v", inserted, err)
	}
	inserted, err = m.AppendIfAbsent(msg)
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("duplicate msg_id must not insert")
	}

	got, ok := m.Get("m1")
	if !ok || got.Content != "hi" || got.Direction != message.DirectionIn {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	items, err := m.Latest(msg.ConvID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 message got %d", len(items))
	}
}

func TestAppendOutgoingAndDeliverAreAtomic(t *testing.T) {
	db, _ := openTest(t)
	m := NewMessages(db)
	now := time.Unix(2000, 0)

	msg := message.Message{
		MsgID: "o1", ConvID: message.DirectConvID(mkID(1), mkID(2)),
		SenderID: mkID(1), Direction: message.DirectionOut, Content: "hey",
		MsgType: message.MsgTypeText, SentAt: now, State: message.StatePending,
	}
	if err := m.AppendOutgoing(msg, now); err != nil {
		t.Fatal(err)
	}

	due, err := m.Due(now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].MsgID != "o1" || due[0].Attempts != 0 {
		t.Fatalf("unexpected outbox: %+v", due)
	}

	if err := m.Deliver("o1"); err != nil {
		t.Fatal(err)
	}
	if due, _ := m.Due(now.Add(time.Hour), 10); len(due) != 0 {
		t.Fatalf("outbox must be cleared after delivery, got %+v", due)
	}
	stored, _ := m.Get("o1")
	if stored.State != message.StateDelivered {
		t.Fatalf("state want delivered got %s", stored.State)
	}
}

func TestOutboxBackoffSurvives(t *testing.T) {
	db, _ := openTest(t)
	m := NewMessages(db)
	now := time.Unix(3000, 0)

	if err := m.Enqueue("x1", "conv", now); err != nil {
		t.Fatal(err)
	}
	later := now.Add(30 * time.Second)
	if err := m.BumpAttempt("x1", later); err != nil {
		t.Fatal(err)
	}

	if due, _ := m.Due(now, 10); len(due) != 0 {
		t.Fatal("must not be due before next_try_at")
	}
	due, err := m.Due(later, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Attempts != 1 {
		t.Fatalf("want attempts=1 got %+v", due)
	}

	byConv, err := m.ListByConv("conv")
	if err != nil || len(byConv) != 1 {
		t.Fatalf("ListByConv failed: %+v err=%v", byConv, err)
	}
	if err := m.Delete("x1"); err != nil {
		t.Fatal(err)
	}
	if byConv, _ := m.ListByConv("conv"); len(byConv) != 0 {
		t.Fatal("delete failed")
	}
}

func TestPeersUpsertMergeSemantics(t *testing.T) {
	db, _ := openTest(t)
	p := NewPeers(db, clock.New())

	// 第一次：只有 announce 能提供的信息（没有 caps / addr）
	if err := p.Upsert(peer.Peer{
		NodeID: mkID(1), DisplayName: "alice", State: peer.StateDiscovered,
		Source: peer.SourceBroadcast, LastSeen: time.Unix(100, 0), Subnet: "10.0.0.0/24",
	}); err != nil {
		t.Fatal(err)
	}

	// 第二次：握手后才有的信息（caps / addr），不应抹掉 display_name
	if err := p.Upsert(peer.Peer{
		NodeID: mkID(1), LastAddr: "10.0.0.5:2425", Caps: peer.CapsGroupChat,
		ProtoVer: 2, State: peer.StateOnline, Source: peer.SourceBroadcast,
		LastSeen: time.Unix(200, 0),
	}); err != nil {
		t.Fatal(err)
	}

	got, ok := p.Get(mkID(1))
	if !ok {
		t.Fatal("peer missing")
	}
	if got.DisplayName != "alice" {
		t.Fatalf("display name must be preserved, got %q", got.DisplayName)
	}
	if got.LastAddr != "10.0.0.5:2425" || !got.Caps.Has(peer.CapsGroupChat) || got.ProtoVer != 2 {
		t.Fatalf("handshake fields not merged: %+v", got)
	}
	if got.State != peer.StateOnline {
		t.Fatalf("state want online got %s", got.State)
	}
	if got.Subnet != "10.0.0.0/24" {
		t.Fatalf("subnet must be preserved, got %q", got.Subnet)
	}

	// 过滤与离线
	if list := p.List(ports.PeerFilter{OnlineOnly: true}); len(list) != 1 {
		t.Fatalf("online filter wrong: %d", len(list))
	}
	if err := p.MarkOffline(mkID(1), time.Unix(300, 0)); err != nil {
		t.Fatal(err)
	}
	got, _ = p.Get(mkID(1))
	if got.State != peer.StateOffline {
		t.Fatalf("want offline got %s", got.State)
	}
}

func TestTransferBitmapPersistsAndResumes(t *testing.T) {
	db, _ := openTest(t)
	tr := NewTransfers(db)
	peerID := mkID(9)

	const chunk = int64(512 * 1024)
	size := chunk*8 + 3
	total := transfer.ChunkCount(size, chunk)

	job := transfer.Job{
		JobID: "j1", PeerID: peerID, FileName: "big.bin", FileSize: size, FileHash: "h1",
		ChunkSize: chunk, TotalChunks: total, Direction: transfer.DirectionRecv,
		LocalPath: "/tmp/big.bin", TempPath: "/tmp/.swarmlink.j1.part",
		Bitmap: transfer.NewChunkBitmap(total), Status: transfer.StateTransferring,
		WindowSize: 8, CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
	}
	if err := tr.UpsertJob(job); err != nil {
		t.Fatal(err)
	}

	bm := job.Bitmap
	bm.Set(0)
	bm.Set(3)
	bm.Set(total - 1)
	if err := tr.SaveBitmap("j1", bm, 3); err != nil {
		t.Fatal(err)
	}

	// 续传查找：同 peer + 同 file_hash 必须命中
	found, ok := tr.FindResumable(peerID, "h1")
	if !ok {
		t.Fatal("resumable job not found")
	}
	if found.Completed != 3 || !found.Bitmap.IsSet(3) || found.Bitmap.IsSet(1) {
		t.Fatalf("bitmap not restored: completed=%d", found.Completed)
	}
	if found.Bitmap.Len() < total {
		t.Fatalf("bitmap length not restored: %d < %d", found.Bitmap.Len(), total)
	}

	// 位图内容按字节一致（往返无损）
	roundTrip := found.Bitmap.Bytes()
	if !bytes.Equal(roundTrip, bm.Bytes()) {
		t.Fatal("bitmap bytes mismatch after roundtrip")
	}

	// 未结束的任务出现在 ListActive
	active, err := tr.ListActive()
	if err != nil || len(active) != 1 {
		t.Fatalf("ListActive want 1 got %d err=%v", len(active), err)
	}

	// 完成后不再可续传
	job.Status = transfer.StateDone
	if err := tr.UpsertJob(job); err != nil {
		t.Fatal(err)
	}
	if _, ok := tr.FindResumable(peerID, "h1"); ok {
		t.Fatal("done job must not be resumable")
	}
	if active, _ := tr.ListActive(); len(active) != 0 {
		t.Fatalf("no active jobs expected, got %d", len(active))
	}
}

// 等待态与退避时间必须能落库再读回来：重启恢复、sweep 是否到点全靠这两列。
func TestJobWaitingFieldsRoundTrip(t *testing.T) {
	db, _ := openTest(t)
	tr := NewTransfers(db)

	peerID := mkID(7)
	next := time.UnixMilli(1_760_000_000_000)
	job := transfer.Job{
		JobID: "job-wait-1", PeerID: peerID, FileName: "报告.pdf", FileSize: 4096,
		ChunkSize: 1024, TotalChunks: 4, Direction: transfer.DirectionSend,
		LocalPath: `C:\tmp\报告.pdf`, Bitmap: transfer.NewChunkBitmap(4),
		Status: transfer.StateIdle, WaitReason: transfer.WaitPeerOffline,
		NextTryAt: next, RetryCount: 3,
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(2, 0),
	}
	if err := tr.UpsertJob(job); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := tr.GetJob(job.JobID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.WaitReason != transfer.WaitPeerOffline {
		t.Errorf("wait_reason = %q, want %q", got.WaitReason, transfer.WaitPeerOffline)
	}
	if !got.NextTryAt.Equal(next) {
		t.Errorf("next_try_at = %v, want %v", got.NextTryAt, next)
	}
	if got.RetryCount != 3 {
		t.Errorf("retry_count = %d, want 3", got.RetryCount)
	}

	// 零值必须干净地回到零值（不能变成 1970 之前的负数）
	job.NextTryAt = time.Time{}
	job.WaitReason = transfer.WaitNone
	if err := tr.UpsertJob(job); err != nil {
		t.Fatalf("upsert zero: %v", err)
	}
	got, err = tr.GetJob(job.JobID)
	if err != nil {
		t.Fatalf("get zero: %v", err)
	}
	if !got.NextTryAt.IsZero() {
		t.Errorf("next_try_at = %v, want zero", got.NextTryAt)
	}
	if got.WaitReason != transfer.WaitNone {
		t.Errorf("wait_reason = %q, want 空串", got.WaitReason)
	}
}

// 批次字段必须能落库再读回来：目录树还原、批次聚合进度、整体取消都靠它们。
func TestJobBatchFieldsRoundTrip(t *testing.T) {
	db, _ := openTest(t)
	tr := NewTransfers(db)

	peerID := mkID(9)
	job := transfer.Job{
		JobID: "job-batch-1", PeerID: peerID, FileName: "a.jpg", FileSize: 1024,
		ChunkSize: 512, TotalChunks: 2, Direction: transfer.DirectionSend,
		LocalPath: `C:\照片\2024\a.jpg`, Bitmap: transfer.NewChunkBitmap(2),
		Status: transfer.StateIdle, WaitReason: transfer.WaitQueued,
		BatchID: "batch-1", RelPath: "照片/2024/a.jpg",
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(2, 0),
	}
	if err := tr.UpsertJob(job); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := tr.GetJob(job.JobID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.BatchID != "batch-1" {
		t.Errorf("batch_id = %q, want batch-1", got.BatchID)
	}
	if got.RelPath != "照片/2024/a.jpg" {
		t.Errorf("rel_path = %q, want 原样读回", got.RelPath)
	}
	if got.IsDir {
		t.Error("普通文件的 is_dir 必须为 false")
	}

	// 空目录条目：is_dir 必须真的存成 1，否则接收端不会建目录
	dirJob := job
	dirJob.JobID = "job-batch-2"
	dirJob.FileName = "空的"
	dirJob.FileSize = 0
	dirJob.TotalChunks = 0
	dirJob.RelPath = "照片/空的"
	dirJob.IsDir = true
	if err := tr.UpsertJob(dirJob); err != nil {
		t.Fatalf("upsert dir: %v", err)
	}
	gotDir, err := tr.GetJob("job-batch-2")
	if err != nil {
		t.Fatalf("get dir: %v", err)
	}
	if !gotDir.IsDir {
		t.Error("is_dir 读回为 false：空目录会在接收端丢失")
	}

	// 更新回零值也必须干净（与 wait_reason 同一条零值往返要求）
	dirJob.IsDir = false
	dirJob.BatchID = ""
	dirJob.RelPath = ""
	if err := tr.UpsertJob(dirJob); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	back, err := tr.GetJob("job-batch-2")
	if err != nil {
		t.Fatalf("get back: %v", err)
	}
	if back.IsDir || back.BatchID != "" || back.RelPath != "" {
		t.Errorf("零值往返失败：isDir=%v batch=%q rel=%q", back.IsDir, back.BatchID, back.RelPath)
	}
}

// ListByBatch 是批次聚合与整体取消的唯一查询入口。
func TestListByBatch(t *testing.T) {
	db, _ := openTest(t)
	tr := NewTransfers(db)
	peerID := mkID(10)

	base := transfer.Job{
		PeerID: peerID, ChunkSize: 512, Direction: transfer.DirectionSend,
		Status: transfer.StateIdle, WaitReason: transfer.WaitQueued,
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
	}
	// CreatedAt 刻意各不相同：三条同时间戳会让 ORDER BY created_at 出现并列，
	// 返回顺序取决于 SQLite 未定义的稳定性 —— 那样「按创建顺序」这条要求
	// 即使被改成 DESC 也照样通过，断言就成了摆设。
	for i, spec := range []struct{ jobID, rel string }{
		{"j1", "照片/a.txt"}, {"j2", "照片/b.txt"}, {"j3", "照片/子/c.txt"},
	} {
		j := base
		j.JobID, j.BatchID, j.RelPath = spec.jobID, "b1", spec.rel
		j.CreatedAt = time.Unix(int64(i+1), 0)
		if err := tr.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}
	other := base
	other.JobID, other.BatchID, other.RelPath = "j9", "b2", "别的/x.txt"
	if err := tr.UpsertJob(other); err != nil {
		t.Fatal(err)
	}
	single := base
	single.JobID = "j10" // 单文件：BatchID/RelPath 都是空串
	if err := tr.UpsertJob(single); err != nil {
		t.Fatal(err)
	}

	got, err := tr.ListByBatch("b1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("条数 = %d，期望 3（不能把别的批或单文件算进来）", len(got))
	}
	if got[0].RelPath != "照片/a.txt" {
		t.Errorf("首条 rel_path = %q，期望按创建顺序", got[0].RelPath)
	}
}

// 批次排队任务必须被 ListWaiting 一起返回，否则对方上线后整批不会自动开始；
// 而「用户取消过」的任务一个都不能进来。
func TestListWaitingIncludesQueued(t *testing.T) {
	db, _ := openTest(t)
	tr := NewTransfers(db)
	peerID := mkID(11)

	mk := func(jobID string, reason transfer.WaitReason) {
		if err := tr.UpsertJob(transfer.Job{
			JobID: jobID, PeerID: peerID, ChunkSize: 512, Direction: transfer.DirectionSend,
			Status: transfer.StateIdle, WaitReason: reason,
			CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("w1", transfer.WaitPeerOffline)
	mk("w2", transfer.WaitQueued)
	mk("w3", transfer.WaitNone) // 用户取消过 → 绝不能被自动拉起

	got, err := tr.ListWaiting()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, j := range got {
		ids[j.JobID] = true
	}
	if !ids["w1"] || !ids["w2"] {
		t.Errorf("ListWaiting 漏了可自动拉起的任务：%v", ids)
	}
	if ids["w3"] {
		t.Error("WaitNone（用户取消）的任务不能被 ListWaiting 返回")
	}
}

// 清理策略必须放过「等待对方上线」的任务。
//
// 它们的状态是 queued，不在 done/failed/cancelled 里。这条用例把它钉住：
// 一旦有人图省事把 queued 也塞进清理集合，等待中的文件会在 TTL 之后被
// 静默删掉 —— 用户什么都没做，任务却消失了。
func TestPurgeFinishedKeepsWaitingJobs(t *testing.T) {
	db, _ := openTest(t)
	tr := NewTransfers(db)
	peerID := mkID(8)

	old := time.Now().Add(-30 * 24 * time.Hour) // 比 TTL 还旧
	mk := func(id string, st transfer.State, reason transfer.WaitReason) transfer.Job {
		return transfer.Job{
			JobID: id, PeerID: peerID, FileName: id + ".bin", FileSize: 4096,
			ChunkSize: 1024, TotalChunks: 4, Direction: transfer.DirectionSend,
			LocalPath: "/tmp/" + id, Bitmap: transfer.NewChunkBitmap(4),
			Status: st, WaitReason: reason, CreatedAt: old, UpdatedAt: old,
		}
	}
	for _, j := range []transfer.Job{
		mk("job-waiting-old", transfer.StateIdle, transfer.WaitPeerOffline),
		mk("job-done-old", transfer.StateDone, transfer.WaitNone),
	} {
		if err := tr.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := tr.PurgeFinished(0, time.Now().Add(-7*24*time.Hour)); err != nil {
		t.Fatalf("purge: %v", err)
	}

	if _, err := tr.GetJob("job-waiting-old"); err != nil {
		t.Errorf("等待中的任务被清理掉了（%v）", err)
	}
	if _, err := tr.GetJob("job-done-old"); err == nil {
		t.Error("过期的已结束任务应当被清理")
	}
}

// 与 mem 侧同一组断言：两个实现必须对等，否则 app 层测试（用 mem）通过
// 而真实运行（用 sqlite）出错，是最难查的一类漂移。
func TestListWaitingFiltersByReasonAndDirection(t *testing.T) {
	db, _ := openTest(t)
	tr := NewTransfers(db)
	peerID := mkID(7)

	base := transfer.Job{
		PeerID: peerID, FileName: "f.bin", FileSize: 4096, ChunkSize: 1024,
		TotalChunks: 4, LocalPath: "/tmp/f.bin", Bitmap: transfer.NewChunkBitmap(4),
	}
	mk := func(id string, st transfer.State, reason transfer.WaitReason, dir transfer.Direction, at int64) transfer.Job {
		j := base
		j.JobID, j.Status, j.WaitReason, j.Direction = id, st, reason, dir
		j.CreatedAt, j.UpdatedAt = time.Unix(at, 0), time.Unix(at, 0)
		return j
	}

	for _, j := range []transfer.Job{
		mk("wait-first", transfer.StateIdle, transfer.WaitPeerOffline, transfer.DirectionSend, 10),
		mk("wait-second", transfer.StateIdle, transfer.WaitPeerOffline, transfer.DirectionSend, 20),
		mk("idle-no-reason", transfer.StateIdle, transfer.WaitNone, transfer.DirectionSend, 30),
		mk("running", transfer.StateTransferring, transfer.WaitNone, transfer.DirectionSend, 40),
		mk("wait-recv", transfer.StateIdle, transfer.WaitPeerOffline, transfer.DirectionRecv, 50),
		mk("failed", transfer.StateFailed, transfer.WaitNone, transfer.DirectionSend, 60),
	} {
		if err := tr.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}

	got, err := tr.ListWaiting()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListWaiting 返回 %d 条，want 2：%+v", len(got), got)
	}
	if got[0].JobID != "wait-first" || got[1].JobID != "wait-second" {
		t.Fatalf("ListWaiting 顺序 = %q/%q，want wait-first/wait-second（按 created_at）", got[0].JobID, got[1].JobID)
	}
}

func TestGroupsRoundTrip(t *testing.T) {
	db, _ := openTest(t)
	g := NewGroups(db)
	owner := mkID(1)

	in := group.Group{
		ID: "g1", Name: "team", OwnerID: owner, Epoch: 3,
		StateSig: []byte{1, 2, 3}, CreatedAt: time.Unix(10, 0),
		Members: []group.Member{
			{NodeID: owner, DisplayName: "owner", Role: group.RoleOwner, State: group.MemberActive, JoinedAt: time.Unix(10, 0)},
			{NodeID: mkID(2), DisplayName: "bob", Role: group.RoleMember, State: group.MemberActive, JoinedAt: time.Unix(11, 0)},
		},
	}
	if err := g.Upsert(in); err != nil {
		t.Fatal(err)
	}

	got, ok := g.Get("g1")
	if !ok {
		t.Fatal("group missing")
	}
	if got.Name != "team" || got.Epoch != 3 || len(got.Members) != 2 {
		t.Fatalf("group roundtrip mismatch: %+v", got)
	}
	if !bytes.Equal(got.StateSig, in.StateSig) {
		t.Fatal("state signature mismatch")
	}

	// 成员变更后覆盖（epoch 递增且成员表被替换，不残留旧成员）
	in.Epoch = 4
	in.Members = in.Members[:1]
	if err := g.Upsert(in); err != nil {
		t.Fatal(err)
	}
	got, _ = g.Get("g1")
	if got.Epoch != 4 || len(got.Members) != 1 {
		t.Fatalf("member replacement failed: epoch=%d members=%d", got.Epoch, len(got.Members))
	}
	if len(g.List()) != 1 {
		t.Fatal("list failed")
	}
}

// 老库升级：只应用过 v1 的数据库再次打开时，必须自动补上 v2。
//
// 这是最容易在真机上炸、却最容易在测试里漏的一条路径 —— 开发者的库
// 早已是 v1，用户升级到本版本时走的正是这里（要么补建表，要么因为
// checksum / 表已存在而启动失败）。
func TestMigrateFromV1AppliesLaterVersions(t *testing.T) {
	db, path := openTest(t)

	// 把库退回「只应用过 v1」的样子：删掉 v2 的记录与它的表
	if err := db.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DROP TABLE conv_clears`); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM schema_migrations WHERE version = 2`)
		return err
	}); err != nil {
		t.Fatalf("退回 v1: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// 重新打开：v1 的 checksum 必须仍然匹配（证明 v1 没被改过），
	// v2 必须被重新应用
	reopened := openAt(t, path)
	defer func() { _ = reopened.Close() }()

	var n int
	if err := reopened.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 2`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("v2 没有被重新应用")
	}
	var name string
	if err := reopened.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='conv_clears'`).Scan(&name); err != nil {
		t.Fatalf("conv_clears 表没有补建: %v", err)
	}
}

// 清空点必须能覆盖（用户清空 → 又聊了几句 → 再清空一次，边界要往前推），
// 且各会话互不影响。
//
// 它是「软清空」的全部持久化状态：读错一次就会表现成「消息凭空消失」
// 或「清空了却又回来」，因此逐条钉住。
func TestConvClearRoundTripAndOverwrite(t *testing.T) {
	db, _ := openTest(t)
	m := NewMessages(db)

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

	// 撤销要幂等：连删两次都不报错
	for i := 0; i < 2; i++ {
		if err := m.RemoveConvClear("c1"); err != nil {
			t.Fatalf("RemoveConvClear(第 %d 次): %v", i+1, err)
		}
	}
	if _, ok, _ := m.ConvClearedAt("c1"); ok {
		t.Fatal("撤销后仍读到清空点")
	}
}
