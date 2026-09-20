package tcp

import (
	"context"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
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
	kp, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	bus := eventbus.New()
	mgr := NewManager(kp, testCfg("n-"+kp.NodeID().String()[:4], 0), DefaultPolicy(), clk, nil, bus, handler)
	if _, err := mgr.Start(context.Background(), "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Stop)
	return &node{kp: kp, mgr: mgr, addr: mgr.LocalAddr(), bus: bus}
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
