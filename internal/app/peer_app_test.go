package app

import (
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/store/mem"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// 收到 BYE（对方正常退出）必须立即把节点标记为离线并推事件 ——
// 这是「关闭客户端后别人还显示你在线」这个问题的正面修复。
func TestOnAnnouncementLeavingMarksOffline(t *testing.T) {
	clk := clock.New()
	dir := mem.NewPeers(clk)
	bus := eventbus.New()
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)

	offline := make([]eventbus.PeerOffline, 0, 1)
	bus.Subscribe(eventbus.TopicPeerOffline, func(payload any) {
		if ev, ok := payload.(eventbus.PeerOffline); ok {
			offline = append(offline, ev)
		}
	})

	app := NewPeerApp(self, dir, bus, clk, 5*time.Minute, nil)

	// 先正常上线一次
	if err := app.OnAnnouncement(peer.Announcement{
		NodeID: peerID, Subnet: "10.0.0.0/24", TCPPort: 2425, ObservedIP: "10.0.0.9",
	}); err != nil {
		t.Fatalf("上线的通告: %v", err)
	}
	got, ok := dir.Get(peerID)
	if !ok || got.State == peer.StateOffline {
		t.Fatalf("上线后状态 = %q，期望非离线", got.State)
	}

	// 对方退出
	if err := app.OnAnnouncement(peer.Announcement{
		NodeID: peerID, ObservedIP: "10.0.0.9", Leaving: true,
	}); err != nil {
		t.Fatalf("BYE: %v", err)
	}

	got, ok = dir.Get(peerID)
	if !ok {
		t.Fatal("BYE 不应删除条目：UI 需要区分「从未见过」与「离线」")
	}
	if got.State != peer.StateOffline {
		t.Errorf("BYE 后状态 = %q，期望 offline", got.State)
	}
	if len(offline) != 1 {
		t.Fatalf("peer.offline 事件数 = %d，期望 1", len(offline))
	}
	if offline[0].Reason != "leave" {
		t.Errorf("reason = %q，期望 leave（TCP 断开与正常退出要能区分）", offline[0].Reason)
	}
	if offline[0].NodeID != peerID.String() {
		t.Errorf("事件 node_id = %q，期望 %q", offline[0].NodeID, peerID.String())
	}
}

// BYE 不得把对方的条目「刷新成在线」：它携带的地址等信息依然有效，
// 但状态必须是离线 —— 否则告别反而变成了上线通知。
func TestOnAnnouncementLeavingDoesNotReviveOnlineState(t *testing.T) {
	clk := clock.New()
	dir := mem.NewPeers(clk)
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)

	app := NewPeerApp(self, dir, nil, clk, 5*time.Minute, nil)

	if err := dir.Upsert(peer.Peer{
		NodeID: peerID, DisplayName: "对方", State: peer.StateOnline, LastSeen: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := app.OnAnnouncement(peer.Announcement{
		NodeID: peerID, DisplayName: "对方", ObservedIP: "10.0.0.9", Leaving: true,
	}); err != nil {
		t.Fatal(err)
	}

	got, _ := dir.Get(peerID)
	if got.State != peer.StateOffline {
		t.Fatalf("状态 = %q，期望 offline", got.State)
	}
	// 目录条目里的展示信息要保留，避免列表里突然变成一串十六进制
	if got.DisplayName != "对方" {
		t.Errorf("display_name = %q，期望保留", got.DisplayName)
	}
}

// 收到一个「从没见过的节点」发来的 BYE 应当是空操作。
//
// 这不是疏忽：MarkOffline 是「标记」而不是「写入」，对一个从未出现在目录里的
// 节点，唯一正确的动作是什么都不做 —— 否则子网上任何节点退出，
// 都会在所有邻居的列表里凭空多出一条从未上线过的离线条目。
func TestOnAnnouncementLeavingForUnknownPeerIsNoop(t *testing.T) {
	clk := clock.New()
	dir := mem.NewPeers(clk)
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)

	app := NewPeerApp(self, dir, nil, clk, 5*time.Minute, nil)
	if err := app.OnAnnouncement(peer.Announcement{
		NodeID: peerID, ObservedIP: "10.0.0.9", Leaving: true,
	}); err != nil {
		t.Fatal(err)
	}

	if _, ok := dir.Get(peerID); ok {
		t.Error("未知节点的 BYE 不应在目录里创建条目")
	}
	if list := dir.List(ports.PeerFilter{OnlineOnly: true}); len(list) != 0 {
		t.Errorf("不应有任何在线节点: %+v", list)
	}
}

// 有效签名通告 = 该身份仍在世 ⇒ 自动解除本地移除（设计 D7）。
//
// 能走到 OnAnnouncement 就说明 UDP 层已经验签，并校验过 node_id == Fingerprint(pub)
// （udp/broadcast.go 的 decodeAnnounce、udp/seed.go 的种子回包）—— 这正是它
// 与「目录交换的第三方转述」的本质区别，后者必须继续抑制。
func TestSignedAnnouncementRestoresSuppressedPeer(t *testing.T) {
	clk := clock.New()
	dir := mem.NewPeers(clk)
	bus := eventbus.New()
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)

	if err := dir.Upsert(peer.Peer{NodeID: peerID, DisplayName: "幽灵", State: peer.StateDiscovered}); err != nil {
		t.Fatal(err)
	}
	if err := dir.SetSuppressed(peerID, true, clk.Now()); err != nil {
		t.Fatal(err)
	}

	published := 0
	bus.Subscribe(eventbus.TopicPeerDiscovered, func(any) { published++ })

	app := NewPeerApp(self, dir, bus, clk, 5*time.Minute, nil)
	if err := app.OnAnnouncement(peer.Announcement{
		NodeID: peerID, DisplayName: "幽灵", ObservedIP: "10.0.0.9", TCPPort: 2425,
	}); err != nil {
		t.Fatal(err)
	}

	got, _ := dir.Get(peerID)
	if got.State == peer.StateBlocked {
		t.Fatal("有效签名通告必须解除移除标记")
	}
	// 恢复的是「它在世」这件事；状态按 announce 的口径是 discovered，不直接写 online
	// （ADR-009：在线状态由 announce 维护、连接可用由 TCP 维护）
	if got.State != peer.StateDiscovered {
		t.Fatalf("状态 = %q，期望 discovered", got.State)
	}
	if published != 1 {
		t.Fatalf("恢复后应当发布一次 peer.discovered，发了 %d 条", published)
	}
	if list := dir.List(ports.PeerFilter{}); len(list) != 1 {
		t.Fatalf("恢复后应当重新出现在默认列表里，得到 %+v", list)
	}
}

// 签名有效的 BYE 同样是「它在世」的证据（只是要走了）：先解除移除标记，
// 再把「离线」正常告知 UI —— 顺序反了就会发布一条状态自相矛盾的事件。
func TestSignedByeRestoresSuppressedPeerThenMarksOffline(t *testing.T) {
	clk := clock.New()
	dir := mem.NewPeers(clk)
	bus := eventbus.New()
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)

	if err := dir.Upsert(peer.Peer{NodeID: peerID, DisplayName: "幽灵", State: peer.StateDiscovered}); err != nil {
		t.Fatal(err)
	}
	if err := dir.SetSuppressed(peerID, true, clk.Now()); err != nil {
		t.Fatal(err)
	}

	offline := make([]eventbus.PeerOffline, 0, 1)
	bus.Subscribe(eventbus.TopicPeerOffline, func(payload any) {
		if ev, ok := payload.(eventbus.PeerOffline); ok {
			offline = append(offline, ev)
		}
	})

	app := NewPeerApp(self, dir, bus, clk, 5*time.Minute, nil)
	if err := app.OnAnnouncement(peer.Announcement{
		NodeID: peerID, ObservedIP: "10.0.0.9", Leaving: true,
	}); err != nil {
		t.Fatal(err)
	}

	got, _ := dir.Get(peerID)
	if got.State != peer.StateOffline {
		t.Fatalf("状态 = %q，期望 offline（BYE 应先解除标记再标离线）", got.State)
	}
	if len(offline) != 1 || offline[0].Reason != "leave" {
		t.Fatalf("应当发布一次 reason=leave 的 peer.offline，得到 %+v", offline)
	}
}

// 握手成功是更强的在世证明（三段 Ed25519 认证都过了）⇒ 同样解除移除标记。
//
// 这条同时是「TCP 适配器不需要知道目录」的依据：恢复点在这里，事件发布在适配器，
// 两者由事件总线连接（R2 的依赖方向因此不被破坏）。
func TestConnectionRestoresSuppressedPeer(t *testing.T) {
	clk := clock.New()
	dir := mem.NewPeers(clk)
	bus := eventbus.New()
	self := mustNodeID(t, testSelfHex)
	peerID := mustNodeID(t, testPeerHex)

	if err := dir.Upsert(peer.Peer{NodeID: peerID, DisplayName: "幽灵", State: peer.StateDiscovered}); err != nil {
		t.Fatal(err)
	}
	if err := dir.SetSuppressed(peerID, true, clk.Now()); err != nil {
		t.Fatal(err)
	}

	app := NewPeerApp(self, dir, bus, clk, 5*time.Minute, nil)
	app.Start()

	bus.Publish(eventbus.TopicPeerOnline, eventbus.PeerOnline{
		NodeID: peerID.String(), Addr: "10.0.0.9:2425",
	})

	got, _ := dir.Get(peerID)
	if got.State != peer.StateOnline {
		t.Fatalf("状态 = %q，期望 online", got.State)
	}
	if list := dir.List(ports.PeerFilter{}); len(list) != 1 {
		t.Fatalf("恢复后应当重新出现在默认列表里，得到 %+v", list)
	}
}

// 确保测试里用到的 identity 类型不会被误删（helper 返回它）。
var _ = identity.NodeID{}
