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

// 确保测试里用到的 identity 类型不会被误删（helper 返回它）。
var _ = identity.NodeID{}
