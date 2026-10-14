package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/store/mem"
	"github.com/swarmlink/swarmlink/internal/domain/group"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// recordingConns 记录向哪些节点拨过号，用来证明「补投只发给未送达的在线成员」。
type recordingConns struct {
	mu     sync.Mutex
	dialed []identity.NodeID
}

func (c *recordingConns) Dial(_ context.Context, id identity.NodeID, _ string) (ports.Session, error) {
	c.mu.Lock()
	c.dialed = append(c.dialed, id)
	c.mu.Unlock()
	return stubSession{peer: id}, nil
}
func (c *recordingConns) Accept(context.Context) (ports.Session, error) { return nil, errNoSession }
func (c *recordingConns) SessionOf(identity.NodeID) (ports.Session, bool) {
	return nil, false
}
func (c *recordingConns) Broadcast(protocol.Frame, ...identity.NodeID) error { return nil }
func (c *recordingConns) Close(identity.NodeID) error                        { return nil }

func (c *recordingConns) snapshot() []identity.NodeID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]identity.NodeID(nil), c.dialed...)
}

// has 报告是否向该节点拨过号。
//
// 刻意不做「拨号列表恰好等于 [...]」的断言：send 的首次扇出跑在独立 goroutine 里，
// 它什么时候执行是不确定的（可能晚于测试把对端改成在线）。断言「谁被拨过/谁没被拨过」
// 才是稳定的，且足以表达意图（离线的人永远不该被拨号）。
func (c *recordingConns) has(id identity.NodeID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.dialed {
		if d == id {
			return true
		}
	}
	return false
}

// deliveryFixture 造一个「本机 + 两名成员」的群；成员在线状态由 dir 控制。
func deliveryFixture(t *testing.T, fc *clock.Fake) (*GroupApp, *mem.Messages, *mem.GroupDeliveries, *mem.Peers, *recordingConns) {
	t.Helper()
	store := mem.NewMessages(fc)
	groups := mem.NewGroups()
	deliveries := mem.NewGroupDeliveries()
	dir := mem.NewPeers(fc)
	conns := &recordingConns{}

	self := mustNodeID(t, testSelfHex)
	p1 := mustNodeID(t, testPeerHex)
	p2 := mustNodeID(t, "0102030405060708")

	if err := groups.Upsert(group.Group{
		ID: "g1", Name: "测试群", OwnerID: self, Epoch: 1, State: group.StateActive,
		Members: []group.Member{
			{NodeID: self, Role: group.RoleOwner, State: group.MemberActive},
			{NodeID: p1, Role: group.RoleMember, State: group.MemberActive},
			{NodeID: p2, Role: group.RoleMember, State: group.MemberActive},
		},
	}); err != nil {
		t.Fatalf("建群: %v", err)
	}

	gapp := NewGroupApp(self, nil, groups, store, deliveries, conns, dir, eventbus.New(), fc, nil)
	return gapp, store, deliveries, dir, conns
}

func mkOnlinePeer(id identity.NodeID, now time.Time) peer.Peer {
	// LastAddr 必须有值：扇出要先拨号，没有地址的节点等同于不可达。
	return peer.Peer{
		NodeID:   id,
		State:    peer.StateOnline,
		LastAddr: "127.0.0.1:2425",
		LastSeen: now,
	}
}

// 群消息的送达必须【按成员】记账：一个人确认不等于全员送达。
// 这正是过去「任一 ACK 即出队」导致离线成员永久漏收的根因。
func TestGroupDeliveryIsPerMember(t *testing.T) {
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	gapp, store, deliveries, _, _ := deliveryFixture(t, fc)
	p1 := mustNodeID(t, testPeerHex)
	p2 := mustNodeID(t, "0102030405060708")

	msg, err := gapp.SendGroupMessage(context.Background(), "g1", "大家好")
	if err != nil {
		t.Fatalf("SendGroupMessage: %v", err)
	}

	stats, err := deliveries.StatsByConv("g1")
	if err != nil {
		t.Fatalf("StatsByConv: %v", err)
	}
	if st := stats[msg.MsgID]; st.Total != 2 || st.Acked != 0 || st.Pending() != 2 {
		t.Fatalf("初始投递统计 = %+v，期望 total=2 acked=0", st)
	}

	// 只有一人确认：消息必须仍是 pending，outbox 里也还在。
	if err := gapp.HandleGroupAck(p1, msg.MsgID); err != nil {
		t.Fatalf("HandleGroupAck(p1): %v", err)
	}
	if got, _ := store.Get(msg.MsgID); got.State != message.StatePending {
		t.Errorf("还有成员未确认，消息状态 = %q，期望 pending", got.State)
	}
	if n := store.OutboxLen(); n != 1 {
		t.Errorf("还有成员未确认，outbox 应仍有 1 条，实际 %d", n)
	}

	// 第二人确认：全员完成 → 送达 + 出队 + 投递记录清理。
	if err := gapp.HandleGroupAck(p2, msg.MsgID); err != nil {
		t.Fatalf("HandleGroupAck(p2): %v", err)
	}
	if got, _ := store.Get(msg.MsgID); got.State != message.StateDelivered {
		t.Errorf("全员确认后消息状态 = %q，期望 delivered", got.State)
	}
	if n := store.OutboxLen(); n != 0 {
		t.Errorf("全员确认后 outbox 应为空，实际 %d", n)
	}
	stats, _ = deliveries.StatsByConv("g1")
	if _, ok := stats[msg.MsgID]; ok {
		t.Error("完成的消息投递记录应被清理（否则统计与存储都会无限增长）")
	}
}

// 离线成员不当时发送；其上线后由 SweepOutbox 补投，且只发给未送达的人。
func TestGroupOfflineMemberGetsBackfilled(t *testing.T) {
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	gapp, store, deliveries, dir, conns := deliveryFixture(t, fc)
	p1 := mustNodeID(t, testPeerHex)
	p2 := mustNodeID(t, "0102030405060708")

	// 发送时两名成员都不在线：扇出没有目标，因此不会有任何拨号。
	msg, err := gapp.SendGroupMessage(context.Background(), "g1", "给还没上线的人")
	if err != nil {
		t.Fatalf("SendGroupMessage: %v", err)
	}
	if pending, _ := deliveries.PendingMembers(msg.MsgID); len(pending) != 2 {
		t.Fatalf("待送达成员应为 2，得到 %v", pending)
	}

	// 只有 p2 上线，且退避时间已过：补投应只拨给 p2。
	if err := dir.Upsert(mkOnlinePeer(p2, fc.Now())); err != nil {
		t.Fatal(err)
	}
	fc.Advance(10 * time.Minute)
	gapp.SweepOutbox(context.Background())

	if !conns.has(p2) {
		t.Error("p2 上线后应当被补投")
	}
	if conns.has(p1) {
		t.Error("离线（未送达）的 p1 不该被拨号")
	}
	if got, _ := store.Get(msg.MsgID); got.State != message.StatePending {
		t.Error("仍有成员未送达，消息不该提前标为送达")
	}

	// p1 与 p2 都确认后完成。
	if err := gapp.HandleGroupAck(p1, msg.MsgID); err != nil {
		t.Fatal(err)
	}
	if pending, _ := deliveries.PendingMembers(msg.MsgID); len(pending) != 1 || pending[0] != p2 {
		t.Fatalf("p1 确认后待送达 = %v，期望仅 p2", pending)
	}
	if err := gapp.HandleGroupAck(p2, msg.MsgID); err != nil {
		t.Fatal(err)
	}
	if pending, _ := deliveries.PendingMembers(msg.MsgID); len(pending) != 0 {
		t.Fatalf("全员确认后仍待送达 %v", pending)
	}
	if got, _ := store.Get(msg.MsgID); got.State != message.StateDelivered {
		t.Errorf("全员确认后状态 = %q，期望 delivered", got.State)
	}
}

// 改名 / 移除成员 / 解散都必须在 epoch 上留下痕迹，并把群状态改对。
func TestGroupOwnerOperationsBumpEpoch(t *testing.T) {
	kp, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	store := mem.NewMessages(fc)
	groups := mem.NewGroups()
	self := kp.NodeID()
	peerID := mustNodeID(t, testPeerHex)
	if err := groups.Upsert(group.Group{
		ID: "g1", Name: "原名", OwnerID: self, Epoch: 1, State: group.StateActive,
		Members: []group.Member{
			{NodeID: self, Role: group.RoleOwner, State: group.MemberActive},
			{NodeID: peerID, Role: group.RoleMember, State: group.MemberActive},
		},
	}); err != nil {
		t.Fatal(err)
	}
	gapp := NewGroupApp(self, kp, groups, store, mem.NewGroupDeliveries(), noConns{}, mem.NewPeers(fc), eventbus.New(), fc, nil)

	renamed, err := gapp.Rename(context.Background(), "g1", "新名")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.Name != "新名" || renamed.Epoch != 2 {
		t.Errorf("改名后 name=%q epoch=%d，期望 新名/2", renamed.Name, renamed.Epoch)
	}
	if len(renamed.RosterSig) == 0 {
		t.Error("改名后必须重新签名")
	}

	removed, err := gapp.RemoveMember(context.Background(), "g1", peerID)
	if err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if m, _ := removed.Member(peerID); m.State != group.MemberKicked {
		t.Errorf("被移除成员 state = %q，期望 kicked", m.State)
	}
	if removed.Epoch != 3 {
		t.Errorf("移除后 epoch = %d，期望 3", removed.Epoch)
	}

	dis, err := gapp.Dissolve(context.Background(), "g1")
	if err != nil {
		t.Fatalf("Dissolve: %v", err)
	}
	if !dis.IsDissolved() || dis.Epoch != 4 {
		t.Errorf("解散后 state=%q epoch=%d，期望 dissolved/4", dis.State, dis.Epoch)
	}
	if _, err := gapp.SendGroupMessage(context.Background(), "g1", "还能发吗"); err == nil {
		t.Error("解散后发送必须被拒绝")
	}
}

// 待投递期间被移出的成员不能把消息永远卡住。
//
// 这是真实会发生的组合：离线成员还没确认 -> 群主把他踢了。若不清理他的投递记录，
// 这条消息永远凑不齐「全员送达」，会一直留在 outbox 里空转、界面一直显示未送达。
func TestRemovingMemberUnblocksDelivery(t *testing.T) {
	kp, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	store := mem.NewMessages(fc)
	groups := mem.NewGroups()
	deliveries := mem.NewGroupDeliveries()
	self := kp.NodeID()
	p1 := mustNodeID(t, testPeerHex)
	p2 := mustNodeID(t, "0102030405060708")
	if err := groups.Upsert(group.Group{
		ID: "g1", Name: "群", OwnerID: self, Epoch: 1, State: group.StateActive,
		Members: []group.Member{
			{NodeID: self, Role: group.RoleOwner, State: group.MemberActive},
			{NodeID: p1, Role: group.RoleMember, State: group.MemberActive},
			{NodeID: p2, Role: group.RoleMember, State: group.MemberActive},
		},
	}); err != nil {
		t.Fatal(err)
	}
	gapp := NewGroupApp(self, kp, groups, store, deliveries, noConns{}, mem.NewPeers(fc), eventbus.New(), fc, nil)

	msg, err := gapp.SendGroupMessage(context.Background(), "g1", "有人会被踢")
	if err != nil {
		t.Fatal(err)
	}
	// p1 确认，p2 一直没确认（离线）。
	if err := gapp.HandleGroupAck(p1, msg.MsgID); err != nil {
		t.Fatal(err)
	}
	if pending, _ := deliveries.PendingMembers(msg.MsgID); len(pending) != 1 {
		t.Fatalf("此时待送达 = %v，期望仅 p2", pending)
	}

	// 群主移除 p2：他的投递记录应被清掉，消息随之完成。
	if _, err := gapp.RemoveMember(context.Background(), "g1", p2); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	fc.Advance(time.Minute)
	gapp.SweepOutbox(context.Background())

	if got, _ := store.Get(msg.MsgID); got.State != message.StateDelivered {
		t.Errorf("被移出成员不再挡路后，消息状态 = %q，期望 delivered", got.State)
	}
	if n := store.OutboxLen(); n != 0 {
		t.Errorf("消息应已出队，outbox 仍有 %d 条", n)
	}
}

// 群主不能被移除：那会让群落进「owner 不在成员里」的死状态。
func TestRemoveMemberRejectsOwner(t *testing.T) {
	kp, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	groups := mem.NewGroups()
	self := kp.NodeID()
	if err := groups.Upsert(group.Group{
		ID: "g1", Name: "群", OwnerID: self, Epoch: 1, State: group.StateActive,
		Members: []group.Member{{NodeID: self, Role: group.RoleOwner, State: group.MemberActive}},
	}); err != nil {
		t.Fatal(err)
	}
	gapp := NewGroupApp(self, kp, groups, mem.NewMessages(fc), mem.NewGroupDeliveries(), noConns{}, mem.NewPeers(fc), eventbus.New(), fc, nil)
	if _, err := gapp.RemoveMember(context.Background(), "g1", self); err == nil {
		t.Fatal("移除群主必须报错")
	}
}

// 单聊的 ACK 路径不能被群委派带偏：直连消息必须仍然走 Deliver。
func TestDirectChatAckStillDelivers(t *testing.T) {
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	store := mem.NewMessages(fc)
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)
	capp := NewChatApp(self, store, noConns{}, mem.NewPeers(fc), eventbus.New(), fc, nil)
	// 故意接上群委派：即使它存在，直连消息也不该被它拦下。
	capp.GroupAck = func(identity.NodeID, string) error {
		return errGroupAckUnexpected
	}

	msg, err := capp.SendMessage(context.Background(), peerID, "单聊")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if err := capp.HandleChatAck(stubSession{peer: peerID}, ackFrame(t, msg.MsgID)); err != nil {
		t.Fatalf("HandleChatAck: %v", err)
	}
	if got, _ := store.Get(msg.MsgID); got.State != message.StateDelivered {
		t.Errorf("单聊 ACK 后状态 = %q，期望 delivered", got.State)
	}
}

var errGroupAckUnexpected = errUnexpectedGroupAck{}

type errUnexpectedGroupAck struct{}

func (errUnexpectedGroupAck) Error() string { return "直连消息不该走群 ACK 委派" }

func ackFrame(t *testing.T, msgID string) protocol.Frame {
	t.Helper()
	payload, err := protocol.EncodeJSON(protocol.ChatAck{MsgID: msgID})
	if err != nil {
		t.Fatal(err)
	}
	return protocol.New(protocol.TypeChatAck, payload)
}
