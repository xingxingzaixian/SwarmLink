package sqlite

import (
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/group"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
)

// 群状态必须原样往返：解散态存成 active 会让「已解散」的群在重启后复活。
func TestGroupStateRoundTrip(t *testing.T) {
	db, _ := openTest(t)
	groups := NewGroups(db)

	dis := group.Group{
		ID: "g-dissolved", Name: "散伙", OwnerID: mkID(1), Epoch: 2, State: group.StateDissolved,
		CreatedAt: time.Unix(1700000000, 0),
		Members:   []group.Member{{NodeID: mkID(1), Role: group.RoleOwner, State: group.MemberActive}},
	}
	if err := groups.Upsert(dis); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, ok := groups.Get("g-dissolved")
	if !ok || got.State != group.StateDissolved {
		t.Fatalf("读取的群状态 = %q，期望 dissolved", got.State)
	}

	// 未显式设状态时按 active 落库并可读回。
	act := group.Group{
		ID: "g-active", Name: "活跃", OwnerID: mkID(1), Epoch: 1,
		CreatedAt: time.Unix(1700000001, 0),
		Members:   []group.Member{{NodeID: mkID(1), Role: group.RoleOwner, State: group.MemberActive}},
	}
	if err := groups.Upsert(act); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if got, _ := groups.Get("g-active"); got.State != group.StateActive {
		t.Fatalf("缺省状态 = %q，期望 active", got.State)
	}
}

// 按成员投递跟踪的完整生命周期：登记 → 部分确认 → 全员确认 → 统计与清理。
func TestGroupDeliveriesLifecycle(t *testing.T) {
	db, _ := openTest(t)
	d := NewGroupDeliveries(db)
	p1, p2 := mkID(2), mkID(3)

	if err := d.EnsureDeliveries("m1", "g1", []identity.NodeID{p1, p2}); err != nil {
		t.Fatalf("EnsureDeliveries: %v", err)
	}
	// 幂等：重复登记不得把已确认的成员改回未送达，也不能报错。
	if err := d.EnsureDeliveries("m1", "g1", []identity.NodeID{p1, p2}); err != nil {
		t.Fatalf("重复 EnsureDeliveries: %v", err)
	}

	pending, err := d.PendingMembers("m1")
	if err != nil || len(pending) != 2 {
		t.Fatalf("初始待送达 = %v (err=%v)，期望 2 人", pending, err)
	}

	if err := d.AckDelivery("m1", p1); err != nil {
		t.Fatalf("AckDelivery: %v", err)
	}
	// 重复确认必须幂等（迟到/重复的 ACK 是常态）。
	if err := d.AckDelivery("m1", p1); err != nil {
		t.Fatalf("重复 AckDelivery: %v", err)
	}
	pending, _ = d.PendingMembers("m1")
	if len(pending) != 1 || pending[0] != p2 {
		t.Fatalf("p1 确认后待送达 = %v，期望仅 p2", pending)
	}

	stats, err := d.StatsByConv("g1")
	if err != nil {
		t.Fatalf("StatsByConv: %v", err)
	}
	if st := stats["m1"]; st.Total != 2 || st.Acked != 1 || st.Pending() != 1 {
		t.Fatalf("统计 = %+v，期望 total=2 acked=1", st)
	}

	if err := d.DeleteByMsg("m1"); err != nil {
		t.Fatalf("DeleteByMsg: %v", err)
	}
	if stats, _ := d.StatsByConv("g1"); len(stats) != 0 {
		t.Fatalf("DeleteByMsg 后统计应为空，得到 %+v", stats)
	}
}

// DeleteByGroup 必须清掉该群全部消息的投递记录（解散时停止一切补投）。
func TestGroupDeliveriesDeleteByGroup(t *testing.T) {
	db, _ := openTest(t)
	d := NewGroupDeliveries(db)
	ids := []identity.NodeID{mkID(2)}

	if err := d.EnsureDeliveries("m1", "g1", ids); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureDeliveries("m2", "g1", ids); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureDeliveries("m3", "g2", ids); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteByGroup("g1"); err != nil {
		t.Fatalf("DeleteByGroup: %v", err)
	}
	if stats, _ := d.StatsByConv("g1"); len(stats) != 0 {
		t.Fatalf("g1 的记录应被清空，得到 %+v", stats)
	}
	if stats, _ := d.StatsByConv("g2"); len(stats) != 1 {
		t.Fatalf("g2 的记录不该被牵连，得到 %+v", stats)
	}
}

// DeleteGroupMember 只清掉那一个人（成员被移出群时用）：
// 清多了会让还在等待的成员凭空消失，清少了会让消息永远无法完成。
func TestGroupDeliveriesDeleteMember(t *testing.T) {
	db, _ := openTest(t)
	d := NewGroupDeliveries(db)
	p1, p2 := mkID(2), mkID(3)

	for _, msgID := range []string{"m1", "m2"} {
		if err := d.EnsureDeliveries(msgID, "g1", []identity.NodeID{p1, p2}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.DeleteGroupMember("g1", p1); err != nil {
		t.Fatalf("DeleteGroupMember: %v", err)
	}
	for _, msgID := range []string{"m1", "m2"} {
		pending, _ := d.PendingMembers(msgID)
		if len(pending) != 1 || pending[0] != p2 {
			t.Fatalf("%s 待送达 = %v，期望仅 p2", msgID, pending)
		}
		stats, _ := d.StatsByConv("g1")
		if st := stats[msgID]; st.Total != 1 {
			t.Fatalf("%s 统计 = %+v，期望 total=1", msgID, st)
		}
	}
}
