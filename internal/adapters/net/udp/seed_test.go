package udp

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
)

func TestRegistryPickReportsBackoff(t *testing.T) {
	fc := clock.NewFake(time.Unix(0, 0))
	s1 := peer.SeedAddr{IP: "10.0.0.1", UDPPort: 2425}
	s2 := peer.SeedAddr{IP: "10.0.0.2", UDPPort: 2425}
	r := NewRegistry([]peer.SeedAddr{s1, s2}, fc)
	r.BaseBackoff = time.Second
	r.MaxBackoff = 10 * time.Minute

	if got := r.Pick(2); len(got) != 2 {
		t.Fatalf("want 2 seeds got %v", got)
	}

	// 一颗失败 → 进入退避
	r.Report(s1, nil, errors.New("timeout"))
	got := r.Pick(5)
	if len(got) != 1 || got[0] != s2 {
		t.Fatalf("failed seed must be skipped, got %v", got)
	}

	// 退避未过：仍被跳过
	fc.Advance(time.Second)
	if got := r.Pick(5); len(got) != 1 {
		t.Fatalf("seed still in backoff, got %v", got)
	}

	// 退避过期（failCnt=1 → 2^1 * 1s = 2s）
	fc.Advance(2 * time.Second)
	if got := r.Pick(5); len(got) != 2 {
		t.Fatalf("seed should be available again, got %v", got)
	}

	// 成功后 failCnt 归零并记录学到的事实
	kp, _ := identity.GenerateKeyPair()
	r.Report(s1, &peer.Announcement{NodeID: kp.NodeID(), TCPPort: 4242, Subnet: "10.0.0.0/24"}, nil)
	snap := r.Snapshot()
	var found bool
	for _, s := range snap {
		if s.Addr == s1 {
			found = true
			if s.FailCnt != 0 || s.TCPPort != 4242 || s.NodeID != kp.NodeID().String() {
				t.Fatalf("report not applied: %+v", s)
			}
		}
	}
	if !found {
		t.Fatal("seed missing from snapshot")
	}
}

func TestRegistryPickReturnsNilWhenAllBackingOff(t *testing.T) {
	fc := clock.NewFake(time.Unix(0, 0))
	s1 := peer.SeedAddr{IP: "10.0.0.1", UDPPort: 2425}
	r := NewRegistry([]peer.SeedAddr{s1}, fc)
	r.Report(s1, nil, errors.New("down"))
	if got := r.Pick(2); got != nil {
		t.Fatalf("all seeds backing off must yield nil, got %v", got)
	}
}

func TestRegistrySkipsZeroAddrs(t *testing.T) {
	fc := clock.NewFake(time.Unix(0, 0))
	r := NewRegistry([]peer.SeedAddr{{}, {IP: "10.0.0.1", UDPPort: 2425}}, fc)
	if r.Len() != 1 {
		t.Fatalf("zero seed addr must be skipped, got %d", r.Len())
	}
}

func TestProbeRetrievesSignedAnnounce(t *testing.T) {
	clk := clock.New()
	kpB, _ := identity.GenerateKeyPair()

	cfg := DefaultConfig()
	cfg.DisplayName = "bob"
	cfg.TCPPort = 2425
	cfg.SeedOnly = true // 关闭广播循环，只验证探测应答路径

	b := NewBroadcaster(kpB, cfg, clk, nil)
	lo := net.ParseIP("127.0.0.1").To4()
	b.SetInterfaces([]Interface{{Name: "lo0", IP: lo, Mask: net.CIDRMask(8, 32), Broadcast: lo}})

	if err := b.Start(context.Background(), func(peer.Announcement) {}); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()

	p := &Prober{Timeout: 2 * time.Second}
	a, err := p.Probe(context.Background(), peer.SeedAddr{IP: "127.0.0.1", UDPPort: uint16(b.UDPPort())})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if a.NodeID != kpB.NodeID() {
		t.Fatalf("wrong seed identity: %s", a.NodeID)
	}
	if a.DisplayName != "bob" {
		t.Fatalf("wrong display name %q", a.DisplayName)
	}
	if a.TCPPort != 2425 {
		t.Fatalf("wrong tcp port %d", a.TCPPort)
	}
	if !identity.Verify(a.PublicKey, a.SigningBytes(), a.Sig) {
		t.Fatal("seed announce must be signed and verifiable from the first hop")
	}
	// subnet 必须被填充（P-2 地址重叠检测依赖它）
	if a.Subnet != "127.0.0.1/8" {
		t.Fatalf("subnet not filled: %q", a.Subnet)
	}
}

func TestProbeTimesOutOnDeadSeed(t *testing.T) {
	// 选一个很可能没人监听的端口
	p := &Prober{Timeout: 300 * time.Millisecond}
	_, err := p.Probe(context.Background(), peer.SeedAddr{IP: "127.0.0.1", UDPPort: 1})
	if err == nil {
		t.Fatal("expected probe failure")
	}
}
