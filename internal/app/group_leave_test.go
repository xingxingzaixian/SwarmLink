package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/store/mem"
	"github.com/swarmlink/swarmlink/internal/domain/group"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// frameRecorder 记录本机发出的帧 —— 用来断言「有没有推快照 / 有没有回 ACK」这类出站行为。
type frameRecorder struct {
	mu   sync.Mutex
	sent []protocol.Frame
}

func (r *frameRecorder) PeerID() identity.NodeID { return identity.NodeID{} }
func (r *frameRecorder) Send(f protocol.Frame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, f)
	return nil
}
func (r *frameRecorder) Recv() (protocol.Frame, error) { return protocol.Frame{}, errNoSession }
func (r *frameRecorder) RTT() time.Duration            { return 0 }
func (r *frameRecorder) Close() error                  { return nil }

func (r *frameRecorder) count(t byte) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, f := range r.sent {
		if f.Type == t {
			n++
		}
	}
	return n
}

func (r *frameRecorder) first(t byte) (protocol.Frame, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.sent {
		if f.Type == t {
			return f, true
		}
	}
	return protocol.Frame{}, false
}

// 说明：本文件用 transfer_waiting_test.go 里既有的 waitUntil 做轮询等待。
//
// 推送与拉取跑在独立 goroutine 里（事件是同步派发的，在订阅者里做网络 I/O
// 会拖住发布方），因此不能对它们的产物做同步断言 —— 普通跑碰巧能过，
// 但 `-race` 下调度一变就红，属于典型的「测试时序假设写错了」。

// sessionConns 让 SessionOf 直接命中同一个 recorder：推送路径刻意不拨号
// （对方刚上线时不该为一次推送发起连接），所以测试里必须有现成会话。
type sessionConns struct{ sess ports.Session }

func (c sessionConns) Dial(context.Context, identity.NodeID, string) (ports.Session, error) {
	return c.sess, nil
}
func (c sessionConns) Accept(context.Context) (ports.Session, error) { return c.sess, nil }
func (c sessionConns) SessionOf(identity.NodeID) (ports.Session, bool) {
	return c.sess, true
}
func (c sessionConns) Broadcast(protocol.Frame, ...identity.NodeID) error { return nil }
func (c sessionConns) Close(identity.NodeID) error                        { return nil }

// groupFixture 造一个「本机是普通成员（非群主）」的群环境。
//
// 必须是真密钥：Leave 要给自己的退出签名，kp 为 nil 会直接 panic ——
// 那正是「退出权属于本人」这条改动在测试里的直接体现。
func groupFixture(t *testing.T, fc *clock.Fake) (*GroupApp, *mem.Peers, identity.NodeID, identity.NodeID, *identity.KeyPair) {
	t.Helper()
	kp, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	owner := mustNodeID(t, testPeerHex)
	self := kp.NodeID()
	dir := mem.NewPeers(fc)

	groups := mem.NewGroups()
	if err := groups.Upsert(group.Group{
		ID: "g1", Name: "群", OwnerID: owner, Epoch: 1, MintSeq: 2, State: group.StateActive,
		Members: []group.Member{
			{NodeID: owner, Role: group.RoleOwner, AdmitSeq: 1},
			{NodeID: self, Role: group.RoleMember, AdmitSeq: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	gapp := NewGroupApp(self, kp, groups, mem.NewMessages(fc), mem.NewGroupDeliveries(),
		noConns{}, dir, eventbus.New(), fc, nil)
	return gapp, dir, owner, self, kp
}

// 本人退出必须【本地立即生效】，且不依赖群主在线。
//
// 这是本次改造的唯一动因：原设计把整张成员表压在一个群主签名下，
// 于是「退出」这个属于本人的权威无处安放。
func TestLeaveTakesEffectLocally(t *testing.T) {
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	gapp, _, _, self, _ := groupFixture(t, fc)

	g, err := gapp.Leave(context.Background(), "g1")
	if err != nil {
		t.Fatalf("Leave: %v", err)
	}
	m, ok := g.Member(self)
	if !ok {
		t.Fatal("退出后成员记录必须保留（审计与 epoch 一致都要它）")
	}
	if m.Active() {
		t.Error("退出后本机必须立刻视为不在群")
	}
	if len(m.LeftSig) == 0 {
		t.Error("退出必须带上自签，否则别人无法在乱序下判定这条是谁发的")
	}
	if m.LeftSeq != m.AdmitSeq {
		t.Errorf("撕票号 = %d，期望等于票号 %d（撕的就是当前那张活票）", m.LeftSeq, m.AdmitSeq)
	}

	// 幂等：再退一次不报错、状态不变。
	if _, err := gapp.Leave(context.Background(), "g1"); err != nil {
		t.Fatalf("重复退出应当幂等，得到 %v", err)
	}
}

// 群主不能退出：Validate 要求 owner 是 active 成员，退出会让群进入
// 「无人能变更」的死状态。给一句明确的提示，指向「解散群」。
func TestLeaveRejectsOwner(t *testing.T) {
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	kp, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	self := kp.NodeID()
	groups := mem.NewGroups()
	if err := groups.Upsert(group.Group{
		ID: "g1", Name: "群", OwnerID: self, Epoch: 1, MintSeq: 1, State: group.StateActive,
		Members: []group.Member{{NodeID: self, Role: group.RoleOwner, AdmitSeq: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	gapp := NewGroupApp(self, kp, groups, mem.NewMessages(fc), mem.NewGroupDeliveries(),
		noConns{}, mem.NewPeers(fc), eventbus.New(), fc, nil)

	if _, err := gapp.Leave(context.Background(), "g1"); err == nil {
		t.Fatal("群主退出必须被拒绝")
	}
}

// 非成员发来的群消息必须丢弃，但【仍要回 ACK】——
// 不回 ACK 会让发送方按 outbox 策略无限重发同一条。
func TestGroupMsgFromNonMemberDroppedButAcked(t *testing.T) {
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	gapp, _, _, _, _ := groupFixture(t, fc)
	stranger := mustNodeID(t, "0172030405060708")

	sess := &recordingSession{peer: stranger}
	frame := mustGroupFrame(t, protocol.GroupMsg{
		MsgID: "m-stranger", GroupID: "g1", SenderID: stranger.String(), Content: "我不在这个群",
	})
	if err := gapp.HandleGroupMsg(sess, frame); err != nil {
		t.Fatalf("HandleGroupMsg: %v", err)
	}
	if _, ok := gapp.store.Get("m-stranger"); ok {
		t.Error("非成员的消息不该落库")
	}
	if !sess.acked {
		t.Error("非成员的消息也必须回 ACK，否则发送方无限重发")
	}
}

// 已退出的成员再发消息同样不是成员：名单变了，判据用的是【当前】名册。
func TestGroupMsgFromLeftMemberDropped(t *testing.T) {
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	gapp, _, _, self, _ := groupFixture(t, fc)
	if _, err := gapp.Leave(context.Background(), "g1"); err != nil {
		t.Fatal(err)
	}

	sess := &recordingSession{peer: self}
	frame := mustGroupFrame(t, protocol.GroupMsg{
		MsgID: "m-after-leave", GroupID: "g1", SenderID: self.String(), Content: "我退出了",
	})
	if err := gapp.HandleGroupMsg(sess, frame); err != nil {
		t.Fatal(err)
	}
	if _, ok := gapp.store.Get("m-after-leave"); ok {
		t.Error("已退出成员的消息不该落库")
	}
	if !sess.acked {
		t.Error("仍必须回 ACK")
	}
}

// 有人上线时把「他可能不知道或已落后」的群推给他。
//
// 这是「刚被拉进群、本机压根没有这个群」的唯一解法：他无从发起拉取，
// 因此必须由别人推 —— 而且任一成员都能推，不需要群主在线。
func TestStartPushesGroupSnapshotOnPeerOnline(t *testing.T) {
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	kp, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	self := kp.NodeID()
	peerID := mustNodeID(t, testPeerHex)
	rec := &frameRecorder{}
	bus := eventbus.New()

	groups := mem.NewGroups()
	if err := groups.Upsert(group.Group{
		ID: "g1", Name: "群", OwnerID: self, Epoch: 3, MintSeq: 5, State: group.StateActive,
		Members: []group.Member{
			{NodeID: self, Role: group.RoleOwner, AdmitSeq: 1},
			{NodeID: peerID, Role: group.RoleMember, AdmitSeq: 5},
		},
	}); err != nil {
		t.Fatal(err)
	}
	gapp := NewGroupApp(self, kp, groups, mem.NewMessages(fc), mem.NewGroupDeliveries(),
		sessionConns{sess: rec}, mem.NewPeers(fc), bus, fc, nil)
	gapp.Start(context.Background())

	bus.Publish(eventbus.TopicPeerOnline, eventbus.PeerOnline{
		NodeID: peerID.String(), Addr: "127.0.0.1:2425",
	})

	waitUntil(t, 2*time.Second, "peer 上线后收到全量名册推送", func() bool {
		return rec.count(protocol.TypeGroupMeta) == 1
	})
	f, _ := rec.first(protocol.TypeGroupMeta)
	meta, err := decodeGroupMeta(f.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Epoch != 3 || meta.MintSeq != 5 || len(meta.Members) != 2 {
		t.Errorf("推送的名册不对：epoch=%d mint=%d members=%d", meta.Epoch, meta.MintSeq, len(meta.Members))
	}
}

// 不在我名册里的节点上线不该被推任何群：那是无关节点，推送就是打扰与信息泄露。
func TestStartDoesNotPushToUnrelatedPeer(t *testing.T) {
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	kp, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	self := kp.NodeID()
	rec := &frameRecorder{}
	bus := eventbus.New()
	groups := mem.NewGroups()
	if err := groups.Upsert(group.Group{
		ID: "g1", Name: "群", OwnerID: self, Epoch: 1, MintSeq: 1, State: group.StateActive,
		Members: []group.Member{{NodeID: self, Role: group.RoleOwner, AdmitSeq: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	gapp := NewGroupApp(self, kp, groups, mem.NewMessages(fc), mem.NewGroupDeliveries(),
		sessionConns{sess: rec}, mem.NewPeers(fc), bus, fc, nil)
	gapp.Start(context.Background())

	bus.Publish(eventbus.TopicPeerOnline, eventbus.PeerOnline{
		NodeID: mustNodeID(t, "0172030405060708").String(), Addr: "127.0.0.1:2425",
	})
	// 「不该发生」的断言只能靠等：给 goroutine 一个必然执行完的窗口再断言 0。
	time.Sleep(200 * time.Millisecond)
	if n := rec.count(protocol.TypeGroupMeta); n != 0 {
		t.Errorf("无关节点上线不该收到推送，实际 %d 次", n)
	}
}

// 抽查：对名册里的在线成员发一次摘要比对请求（不一致时对方会回全量）。
func TestSyncOnceRequestsSnapshot(t *testing.T) {
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	kp, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	self := kp.NodeID()
	peerID := mustNodeID(t, testPeerHex)
	rec := &frameRecorder{}
	dir := mem.NewPeers(fc)
	if err := dir.Upsert(mkOnlinePeer(peerID, fc.Now())); err != nil {
		t.Fatal(err)
	}
	groups := mem.NewGroups()
	if err := groups.Upsert(group.Group{
		ID: "g1", Name: "群", OwnerID: self, Epoch: 1, MintSeq: 1, State: group.StateActive,
		Members: []group.Member{
			{NodeID: self, Role: group.RoleOwner, AdmitSeq: 1},
			{NodeID: peerID, Role: group.RoleMember, AdmitSeq: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	gapp := NewGroupApp(self, kp, groups, mem.NewMessages(fc), mem.NewGroupDeliveries(),
		sessionConns{sess: rec}, dir, eventbus.New(), fc, nil)

	gapp.SyncOnce(context.Background())
	if rec.count(protocol.TypeGroupMembersReq) != 1 {
		t.Fatalf("抽查应当发 1 次 GROUP_MEMBERS_REQ，实际 %d 次", rec.count(protocol.TypeGroupMembersReq))
	}
	f, _ := rec.first(protocol.TypeGroupMembersReq)
	var req protocol.GroupMembersReq
	if err := protocol.DecodeJSON(f.Payload, &req); err != nil {
		t.Fatal(err)
	}
	if req.GroupID != "g1" || req.KnownDigest == "" {
		t.Errorf("摘要比对请求应当带上已知摘要，得到 %+v", req)
	}
}

// 摘要一致时【不回】全量：省掉一次「其实什么都没变」的往返，
// 也顺带避免了摘要一致的重复投递触发 UI 刷新风暴。
func TestHandleMembersReqSkipsWhenDigestMatches(t *testing.T) {
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	kp, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	self := kp.NodeID()
	peerID := mustNodeID(t, testPeerHex)
	groups := mem.NewGroups()
	g := group.Group{
		ID: "g1", Name: "群", OwnerID: self, Epoch: 1, MintSeq: 1, State: group.StateActive,
		Members: []group.Member{
			{NodeID: self, Role: group.RoleOwner, AdmitSeq: 1},
			{NodeID: peerID, Role: group.RoleMember, AdmitSeq: 2},
		},
	}
	if err := groups.Upsert(g); err != nil {
		t.Fatal(err)
	}
	gapp := NewGroupApp(self, kp, groups, mem.NewMessages(fc), mem.NewGroupDeliveries(),
		noConns{}, mem.NewPeers(fc), eventbus.New(), fc, nil)

	rec := &frameRecorder{}
	same := g.RosterDigest()

	// 摘要一致 → 静默。
	payload, err := protocol.EncodeJSON(protocol.GroupMembersReq{
		GroupID: "g1", KnownEpoch: 1, KnownDigest: encodeBase64(same),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gapp.HandleMembersReq(rec, protocol.New(protocol.TypeGroupMembersReq, payload)); err != nil {
		t.Fatal(err)
	}
	if n := rec.count(protocol.TypeGroupMembersRes); n != 0 {
		t.Fatalf("摘要一致时不该回全量，实际回了 %d 次", n)
	}

	// 摘要不同 → 回全量。
	payload, err = protocol.EncodeJSON(protocol.GroupMembersReq{
		GroupID: "g1", KnownEpoch: 1, KnownDigest: "AAAA",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gapp.HandleMembersReq(rec, protocol.New(protocol.TypeGroupMembersReq, payload)); err != nil {
		t.Fatal(err)
	}
	if n := rec.count(protocol.TypeGroupMembersRes); n != 1 {
		t.Fatalf("摘要不同时必须回全量，实际 %d 次", n)
	}
}
