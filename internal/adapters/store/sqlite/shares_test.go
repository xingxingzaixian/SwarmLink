package sqlite

import (
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/share"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
)

func mkShare(id, name, root string, vis share.Visibility, items []share.Item) share.Share {
	return share.Share{
		ID:         id,
		Name:       name,
		Owner:      "aabbccddeeff0011",
		OwnerName:  "alice",
		RootPath:   root,
		IsDir:      true,
		Visibility: vis,
		Items:      items,
		TotalSize:  100,
		CreatedAt:  time.UnixMilli(1_700_000_000_000),
		UpdatedAt:  time.UnixMilli(1_700_000_000_000),
	}
}

func TestSharesRoundTrip(t *testing.T) {
	db, _ := openTest(t)
	r := NewShares(db)

	items := []share.Item{
		{Path: "b/second.txt", Name: "second.txt", Size: 60},
		{Path: "empty", Name: "empty", IsDir: true},
		{Path: "a.txt", Name: "a.txt", Size: 40, MTime: 1_696_828_800},
	}
	in := mkShare("s1", "周报模板", `D:\共享\周报模板`, share.Visibility{
		Type:    share.VisibilityUsers,
		Allowed: []string{"bbbbbbbbbbbbbbbb", "cccccccccccccccc"},
	}, items)

	if err := r.Upsert(in); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, ok := r.Get("s1")
	if !ok {
		t.Fatal("Get 未命中刚写入的共享")
	}
	if got.Name != in.Name || got.RootPath != in.RootPath || got.TotalSize != in.TotalSize {
		t.Fatalf("元数据不一致：%#v", got)
	}
	if got.Visibility.Type != share.VisibilityUsers || len(got.Visibility.Allowed) != 2 {
		t.Fatalf("可见性未正确往返：%#v", got.Visibility)
	}
	if len(got.Items) != 3 {
		t.Fatalf("清单条数 = %d，期望 3", len(got.Items))
	}
	// 清单按 path 升序（目录树顺序）：界面与「整份替换」的语义都依赖这个次序。
	wantOrder := []string{"a.txt", "b/second.txt", "empty"}
	for i, want := range wantOrder {
		if got.Items[i].Path != want {
			t.Fatalf("清单次序 = %v，期望 %v", itemPaths(got.Items), wantOrder)
		}
	}
	if !got.Items[2].IsDir {
		t.Fatal("空目录占位项的 IsDir 丢了")
	}
	if got.CreatedAt.UnixMilli() != in.CreatedAt.UnixMilli() {
		t.Fatalf("created_at 往返错误：%v", got.CreatedAt)
	}

	// 未命中的 id 返回 (零值, false)，而不是哨兵错误（见 ports.ShareRepo 的契约）。
	if _, ok := r.Get("nope"); ok {
		t.Fatal("不存在的共享不该命中")
	}
	if n := len(r.List()); n != 1 {
		t.Fatalf("List 条数 = %d，期望 1", n)
	}
}

// 清单必须是【整份替换】：一次刷新要能删掉源目录里已经不存在的文件。
// 增量合并做不到这件事 —— 它只会越并越多。
func TestSharesUpsertReplacesItems(t *testing.T) {
	db, _ := openTest(t)
	r := NewShares(db)

	first := mkShare("s1", "包", "/tmp/pkg", share.Visibility{Type: share.VisibilityAll},
		[]share.Item{{Path: "a.txt", Name: "a.txt", Size: 1}, {Path: "b.txt", Name: "b.txt", Size: 2}})
	if err := r.Upsert(first); err != nil {
		t.Fatal(err)
	}

	second := first
	second.Name = "包v2"
	second.Items = []share.Item{{Path: "b.txt", Name: "b.txt", Size: 2, MTime: 5}}
	second.UpdatedAt = time.UnixMilli(1_800_000_000_000)
	if err := r.Upsert(second); err != nil {
		t.Fatal(err)
	}

	got, _ := r.Get("s1")
	if len(got.Items) != 1 || got.Items[0].Path != "b.txt" || got.Items[0].MTime != 5 {
		t.Fatalf("清单未被整份替换：%#v", got.Items)
	}
	if got.Name != "包v2" {
		t.Fatalf("元数据未更新：%q", got.Name)
	}
	// created_at 不被覆盖：它是「这条共享什么时候建的」，重刷索引不该改它。
	if got.CreatedAt.UnixMilli() != first.CreatedAt.UnixMilli() {
		t.Fatalf("created_at 被覆盖了：%v", got.CreatedAt)
	}
	if got.UpdatedAt.UnixMilli() != second.UpdatedAt.UnixMilli() {
		t.Fatalf("updated_at 未更新：%v", got.UpdatedAt)
	}
}

func TestSharesDelete(t *testing.T) {
	db, _ := openTest(t)
	r := NewShares(db)

	if err := r.Upsert(mkShare("s1", "包", "/tmp/pkg", share.Visibility{Type: share.VisibilityAll},
		[]share.Item{{Path: "a.txt", Name: "a.txt"}})); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete("s1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("s1"); ok {
		t.Fatal("删除后仍能读到")
	}
	// 清单必须跟着一起走：留着孤儿行会让「重建同 id 的共享」读到上一份清单。
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM share_items WHERE share_id = 's1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("删除共享后残留 %d 条清单行", n)
	}
	// 幂等：删不存在的 id 不报错。
	if err := r.Delete("s1"); err != nil {
		t.Fatalf("重复删除应当幂等：%v", err)
	}
}

func TestSharesListOrder(t *testing.T) {
	db, _ := openTest(t)
	r := NewShares(db)

	for _, spec := range []struct {
		id   string
		at   time.Time
		name string
	}{
		{"s2", time.UnixMilli(2000), "b"},
		{"s1", time.UnixMilli(1000), "a"},
		{"s3", time.UnixMilli(3000), "c"},
	} {
		s := mkShare(spec.id, spec.name, "/tmp/"+spec.name, share.Visibility{Type: share.VisibilityAll}, nil)
		s.CreatedAt, s.UpdatedAt = spec.at, spec.at
		if err := r.Upsert(s); err != nil {
			t.Fatal(err)
		}
	}

	got := r.List()
	want := []string{"s1", "s2", "s3"}
	if len(got) != len(want) {
		t.Fatalf("条数 = %d，期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("次序 = %v，期望按 created_at 升序 %v", shareIDs(got), want)
		}
	}
}

// peers.shares 的语义与同一张表其它列【刻意不同】：它是「最近一次通告的
// 全量状态」，因此空值也必须覆盖。沿用「空值不覆盖」会让取消掉的共享一直
// 挂在别人列表里，直到 TTL 过期（用户点了就提示「共享不存在」）。
func TestPeersSharesColumnOverwritesWithEmpty(t *testing.T) {
	db, _ := openTest(t)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	p := NewPeers(db, clk)

	id := mkID(7)
	withShares := peer.Peer{
		NodeID:   id,
		LastAddr: "10.0.0.2:2425",
		LastSeen: clk.Now(),
		State:    peer.StateOnline,
		Source:   peer.SourceBroadcast,
		Shares: []share.Summary{{
			ShareID:   "s1",
			Name:      "包",
			OwnerID:   id.String(),
			FileCount: 3,
			TotalSize: 100,
			Visibility: share.Visibility{
				Type: share.VisibilityAll,
			},
		}},
	}
	if err := p.Upsert(withShares); err != nil {
		t.Fatal(err)
	}
	got, ok := p.Get(id)
	if !ok || len(got.Shares) != 1 || got.Shares[0].ShareID != "s1" {
		t.Fatalf("摘要未往返：%#v", got.Shares)
	}
	if got.Shares[0].FileCount != 3 || got.Shares[0].TotalSize != 100 {
		t.Fatalf("摘要字段丢失：%#v", got.Shares[0])
	}

	// 对方取消发布：下一次通告里没有摘要，落库必须跟着清空。
	cleared := withShares
	cleared.Shares = nil
	cleared.LastSeen = clk.Now().Add(time.Second)
	if err := p.Upsert(cleared); err != nil {
		t.Fatal(err)
	}
	got, ok = p.Get(id)
	if !ok {
		t.Fatal("条目本身不该消失")
	}
	if len(got.Shares) != 0 {
		t.Fatalf("空摘要必须覆盖旧值（否则取消的共享会挂到 TTL 过期）：%#v", got.Shares)
	}
	// 其它字段照旧遵循「空值不覆盖」：LastAddr 不该被空值抹掉。
	if got.LastAddr != "10.0.0.2:2425" {
		t.Fatalf("LastAddr 不该被空值覆盖：%q", got.LastAddr)
	}
}

// 存储层必须容忍坏 JSON：摘要只是可丢的展示数据，为它让整条节点记录
// 读不出来（进而让该节点从列表里消失）是完全不成比例的代价。
func TestPeersSharesBadJSONIsDropped(t *testing.T) {
	db, _ := openTest(t)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	p := NewPeers(db, clk)

	id := mkID(9)
	if err := p.Upsert(peer.Peer{
		NodeID: id, LastSeen: clk.Now(), State: peer.StateOnline, Source: peer.SourceBroadcast,
	}); err != nil {
		t.Fatal(err)
	}
	// 直接写入一段坏 JSON，模拟「磁盘被别人改过 / 半个写入」。
	if _, err := db.db.Exec(`UPDATE peers SET shares = ? WHERE node_id = ?`, "{not json", id.String()); err != nil {
		t.Fatal(err)
	}

	got, ok := p.Get(id)
	if !ok {
		t.Fatal("坏摘要不该让条目读不出来")
	}
	if len(got.Shares) != 0 {
		t.Fatalf("坏 JSON 应当被当作空摘要：%#v", got.Shares)
	}
}

func itemPaths(items []share.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Path)
	}
	return out
}

func shareIDs(list []share.Share) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.ID)
	}
	return out
}
