package app

import (
	"errors"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/store/mem"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
)

// captureSession 记录本端发出的帧，供断言 PEER_LIST_RESP 的内容。
type captureSession struct {
	peer identity.NodeID
	sent []protocol.Frame
}

func (s *captureSession) PeerID() identity.NodeID     { return s.peer }
func (s *captureSession) Send(f protocol.Frame) error { s.sent = append(s.sent, f); return nil }
func (s *captureSession) RTT() time.Duration          { return 0 }
func (s *captureSession) Close() error                { return nil }
func (s *captureSession) Recv() (protocol.Frame, error) {
	return protocol.Frame{}, errors.New("test: 不读")
}

// lastPeerListResp 取出最近一条 PEER_LIST_RESP。
func lastPeerListResp(t *testing.T, s *captureSession) protocol.PeerListRespWire {
	t.Helper()
	for i := len(s.sent) - 1; i >= 0; i-- {
		if s.sent[i].Type != protocol.TypePeerListResp {
			continue
		}
		var resp protocol.PeerListRespWire
		if err := protocol.DecodeJSON(s.sent[i].Payload, &resp); err != nil {
			t.Fatalf("解码 PEER_LIST_RESP: %v", err)
		}
		return resp
	}
	t.Fatal("本端没有发出任何 PEER_LIST_RESP")
	return protocol.PeerListRespWire{}
}

func hasEntry(resp protocol.PeerListRespWire, id identity.NodeID) bool {
	for _, e := range resp.Entries {
		if e.NodeID == id.String() {
			return true
		}
	}
	return false
}

// requestPeerList 走一次完整的「请求 → 响应」：把 list_epoch 写进请求帧，
// 交给 HandlePeerListReq，再取回本端发出的响应。
func requestPeerList(t *testing.T, d *DiscoveryApp, sess *captureSession, listEpoch uint64) protocol.PeerListRespWire {
	t.Helper()
	payload, err := protocol.EncodeJSON(protocol.PeerListReqWire{ListEpoch: listEpoch})
	if err != nil {
		t.Fatalf("编码 PEER_LIST_REQ: %v", err)
	}
	if err := d.HandlePeerListReq(sess, protocol.New(protocol.TypePeerListReq, payload)); err != nil {
		t.Fatalf("HandlePeerListReq: %v", err)
	}
	return lastPeerListResp(t, sess)
}

// TestPeerListRefreshSecondRoundStillReturnsNewNodes 钉住跨网段刷新的核心承诺：
// 「新节点 ≤ 1 个刷新周期可见」。
//
// 背景（《发现协议设计书》§10.3 与 §15.2）：Broadcaster.BumpEpoch() 在全仓没有
// 任何调用点，epoch 只在 Start 时置 1 就再也不变。而客户端会把上一轮响应里的
// epoch 原样记下来、下次请求时带回去。服务端只要「epoch 相同就回空响应」，
// 同一对 (本机, 种子) 从第二轮起就永远拿到空目录 —— 种子那边后来学到的节点
// 再也不会传过来，直到本机重启。同网段广播不受影响，所以现象是「跨网段的新
// 节点一直看不见」，而不是报错。
//
// 这个用例按【客户端的真实行为】构造：第二轮带回来的 list_epoch 正是第一轮
// 响应里给出的那个值。现有 e2e（TestCrossSubnetConvergenceViaSeed）抓不到它，
// 因为它每次都构造全新的 (本机, 种子) 组合，恰好永远走「第 1 次」。
func TestPeerListRefreshSecondRoundStillReturnsNewNodes(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	dir := mem.NewPeers(clk)
	self := mustNodeID(t, testSelfHex)

	mkPeer := func(hex, name, addr string) peer.Peer {
		return peer.Peer{
			NodeID:      mustNodeID(t, hex),
			DisplayName: name,
			LastAddr:    addr,
			LastSeen:    clk.Now(),
			State:       peer.StateDiscovered,
			Source:      peer.SourceSeed,
		}
	}

	first := mkPeer("0102030405060708", "甲网段节点", "10.1.1.5:2425")
	if err := dir.Upsert(first); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// LocalEpoch 模拟真实广播器：Start 之后恒为 1，因为 BumpEpoch 从不被调用。
	// 这里刻意用常量而不是递增计数器 —— 递增就会掩盖掉这个缺陷。
	d := NewDiscoveryApp(self, nil, dir, nil, nil, nil, nil, clk, nil)
	d.LocalEpoch = func() uint64 { return 1 }

	sess := &captureSession{peer: mustNodeID(t, testPeerHex)}

	// 第一轮：对端从没拉过（list_epoch = 0）→ 必须拿到全量。
	round1 := requestPeerList(t, d, sess, 0)
	if !hasEntry(round1, first.NodeID) {
		t.Fatalf("第一轮响应里没有 %s，目录拉取从第一步就不对", first.DisplayName)
	}

	// 第二轮之前，种子那边学到一个新节点。
	second := mkPeer("1112131415161718", "乙网段新节点", "10.2.2.9:2425")
	if err := dir.Upsert(second); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// 第二轮：对端带回上一轮学到的 epoch —— 这是客户端的真实行为。
	round2 := requestPeerList(t, d, sess, round1.Epoch)
	if !hasEntry(round2, second.NodeID) {
		t.Fatalf("第二轮响应里没有新加入的 %s（entries=%d，请求带的 list_epoch=%d）："+
			"同一个 (本机, 种子) 从第二轮起拿不到新节点，跨网段可见性承诺不成立",
			second.DisplayName, len(round2.Entries), round1.Epoch)
	}

	// 顺带钉住「回空」这种退化：目录非空时，响应就不该是空的。
	if len(round2.Entries) == 0 {
		t.Fatal("目录非空，但第二轮响应是空的")
	}
}
