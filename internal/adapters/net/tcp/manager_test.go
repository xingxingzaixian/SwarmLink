package tcp

import (
	"context"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

type node struct {
	kp   *identity.KeyPair
	mgr  *Manager
	addr string
	bus  *eventbus.Bus
}

func newTestNode(t *testing.T, clk ports.Clock, handler FrameHandler) *node {
	t.Helper()
	return newTestNodeWithCaps(t, clk, 0, handler)
}

// newTestNodeWithCaps 造一个显式声明能力位的节点。
func newTestNodeWithCaps(t *testing.T, clk ports.Clock, caps peer.Caps, handler FrameHandler) *node {
	t.Helper()
	kp, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	bus := eventbus.New()
	mgr := NewManager(kp, testCfg("n-"+kp.NodeID().String()[:4], caps), DefaultPolicy(), clk, nil, bus, handler)
	if _, err := mgr.Start(context.Background(), "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Stop)
	return &node{kp: kp, mgr: mgr, addr: mgr.LocalAddr(), bus: bus}
}

// 会话建立必须把协商出的能力位随 peer.online 事件发出去。
//
// 适配器不许写目录（依赖方向 R2），这条事件是协商结果抵达目录的唯一通道；
// 漏掉它，接收端就只能看到 caps = 0，任何文件夹传输都会被判成「对方不支持」。
func TestPeerOnlineCarriesNegotiatedCaps(t *testing.T) {
	ctx := context.Background()
	caps := peer.CapsChatReliable | peer.CapsFolderTransfer
	a := newTestNodeWithCaps(t, clock.New(), caps, nil)
	b := newTestNodeWithCaps(t, clock.New(), caps, nil)

	online := make([]eventbus.PeerOnline, 0, 1)
	a.bus.Subscribe(eventbus.TopicPeerOnline, func(payload any) {
		if ev, ok := payload.(eventbus.PeerOnline); ok {
			online = append(online, ev)
		}
	})

	sess, err := a.mgr.Dial(ctx, b.kp.NodeID(), b.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if len(online) != 1 {
		t.Fatalf("期望 1 条 peer.online，得到 %d 条", len(online))
	}
	ev := online[0]
	if !ev.Caps.Has(peer.CapsFolderTransfer) {
		t.Errorf("事件必须带上协商能力位（含 CapsFolderTransfer），得到 %b", uint32(ev.Caps))
	}
	// ports.Session 上没有 Caps()：能力位跨不出适配器层，这本身就是原缺陷的一部分
	// —— 因此这里断言事件与会话内部字段一致，而不是通过端口读。
	s, ok := sess.(*session)
	if !ok {
		t.Fatalf("会话类型异常：%T", sess)
	}
	if ev.Caps != s.caps {
		t.Errorf("事件能力位 %b 与会话 %b 不一致", uint32(ev.Caps), uint32(s.caps))
	}
	if ev.ProtoVer != s.protoVersion {
		t.Errorf("事件协议版本 %d 与会话 %d 不一致", ev.ProtoVer, s.protoVersion)
	}
}

func TestManagerDialAndExchangeFrames(t *testing.T) {
	ctx := context.Background()
	got := make(chan protocol.Frame, 1)

	b := newTestNode(t, clock.New(), func(_ ports.Session, f protocol.Frame) { got <- f })
	a := newTestNode(t, clock.New(), nil)

	sess, err := a.mgr.Dial(ctx, b.kp.NodeID(), b.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if sess.PeerID() != b.kp.NodeID() {
		t.Fatal("wrong peer id after dial")
	}

	payload, _ := protocol.EncodeJSON(protocol.Chat{MsgID: "m1", ConvID: "c1", Content: "hi"})
	if err := sess.Send(protocol.New(protocol.TypeChat, payload)); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case f := <-got:
		if f.Type != protocol.TypeChat {
			t.Fatalf("want CHAT got %s", protocol.TypeName(f.Type))
		}
		var chat protocol.Chat
		if err := protocol.DecodeJSON(f.Payload, &chat); err != nil {
			t.Fatal(err)
		}
		if chat.Content != "hi" {
			t.Fatalf("want hi got %q", chat.Content)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for frame")
	}

	if n := a.mgr.SessionCount(); n != 1 {
		t.Fatalf("A sessions want 1 got %d", n)
	}
	if n := b.mgr.SessionCount(); n != 1 {
		t.Fatalf("B sessions want 1 got %d", n)
	}
}

func TestSimultaneousDialLeavesSingleSession(t *testing.T) {
	ctx := context.Background()
	a := newTestNode(t, clock.New(), nil)
	b := newTestNode(t, clock.New(), nil)

	done := make(chan error, 2)
	go func() { _, err := a.mgr.Dial(ctx, b.kp.NodeID(), b.addr); done <- err }()
	go func() { _, err := b.mgr.Dial(ctx, a.kp.NodeID(), a.addr); done <- err }()

	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("concurrent dial: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout on concurrent dial")
		}
	}

	// 等待去重收敛
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a.mgr.SessionCount() == 1 && b.mgr.SessionCount() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := a.mgr.SessionCount(); n != 1 {
		t.Fatalf("A sessions want 1 got %d", n)
	}
	if n := b.mgr.SessionCount(); n != 1 {
		t.Fatalf("B sessions want 1 got %d", n)
	}
}

func TestIdleTimeoutReclaimsSession(t *testing.T) {
	ctx := context.Background()
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))

	b := newTestNode(t, clock.New(), nil)

	// A 使用 FakeClock，并设置很短的空闲阈值
	kpA, _ := identity.GenerateKeyPair()
	mgrA := NewManager(kpA, testCfg("a", 0), Policy{
		MaxDialConcurrency: 4,
		MaxActiveConns:     8,
		IdleTimeout:        100 * time.Millisecond,
		HeartbeatInterval:  time.Hour,
		HeartbeatTimeout:   time.Hour,
	}, fc, nil, eventbus.New(), nil)
	if _, err := mgrA.Start(ctx, "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer mgrA.Stop()

	if _, err := mgrA.Dial(ctx, b.kp.NodeID(), b.addr); err != nil {
		t.Fatal(err)
	}
	if n := mgrA.SessionCount(); n != 1 {
		t.Fatalf("want 1 session got %d", n)
	}

	// 推进虚拟时间越过空闲阈值后回收
	fc.Advance(200 * time.Millisecond)
	mgrA.Sweep()

	if n := mgrA.SessionCount(); n != 0 {
		t.Fatalf("idle session must be reclaimed, still have %d", n)
	}
}

func TestDialReusesExistingSession(t *testing.T) {
	ctx := context.Background()
	a := newTestNode(t, clock.New(), nil)
	b := newTestNode(t, clock.New(), nil)

	s1, err := a.mgr.Dial(ctx, b.kp.NodeID(), b.addr)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := a.mgr.Dial(ctx, b.kp.NodeID(), b.addr)
	if err != nil {
		t.Fatal(err)
	}
	if s1 != s2 {
		t.Fatal("second Dial must reuse the existing session")
	}
	if n := b.mgr.SessionCount(); n != 1 {
		t.Fatalf("B must not see two sessions, got %d", n)
	}
}
