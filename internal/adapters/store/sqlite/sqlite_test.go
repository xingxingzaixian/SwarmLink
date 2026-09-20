package sqlite

import (
	"bytes"
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
		"group_members", "peers", "transfer_jobs", "seed_nodes"} {
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
