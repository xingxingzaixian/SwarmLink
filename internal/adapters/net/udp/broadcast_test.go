package udp

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
)

// 锁定「udp_port=0 时两台机器互相发现不了」的根因。
//
// 修复前的行为：发送方把 ANNOUNCE 发到【自己绑定】的那个端口
// （sendAnnounces 里的 Port: b.port）。两台机器各自让系统随机分配端口，
// 几乎必然错开 —— A 发到 A 的端口，B 在听 B 的端口，谁也收不到谁。
//
// 修复后：发送方按 cfg.AnnouncePorts 覆盖候选端口区间。本测试让 A 绑定
// 自己的随机端口，却把 AnnouncePorts 指到 B 的端口，断言 B 能收到
// A 的签名 ANNOUNCE —— 这正是「端口错开也能发现」的语义。
func TestAnnounceReachesPeerOnDifferentPort(t *testing.T) {
	clk := clock.New()
	lo := net.ParseIP("127.0.0.1").To4()
	iface := []Interface{{Name: "lo0", IP: lo, Mask: net.CIDRMask(8, 32), Broadcast: lo}}

	// 接收方：SeedOnly 关掉发送循环，只收。
	kpB, _ := identity.GenerateKeyPair()
	cfgB := DefaultConfig()
	cfgB.SeedOnly = true
	b := NewBroadcaster(kpB, cfgB, clk, nil)
	b.SetInterfaces(iface)

	got := make(chan peer.Announcement, 1)
	if err := b.Start(context.Background(), func(a peer.Announcement) {
		select {
		case got <- a:
		default:
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()

	portB := b.UDPPort()
	if portB <= 0 {
		t.Fatalf("receiver must have bound a real port, got %d", portB)
	}

	// 发送方：绑定自己的随机端口，但广播目标指向 B 的端口。
	kpA, _ := identity.GenerateKeyPair()
	cfgA := DefaultConfig()
	cfgA.AnnounceInterval = 20 * time.Millisecond
	cfgA.JitterRatio = 0
	a := NewBroadcaster(kpA, cfgA, clk, nil)
	a.SetInterfaces(iface)
	a.SetAnnouncePorts([]int{portB})
	if err := a.Start(context.Background(), func(peer.Announcement) {}); err != nil {
		t.Fatal(err)
	}
	defer a.Stop()

	if a.UDPPort() == portB {
		t.Skip("发送方恰好绑到与接收方相同的端口，未构成端口错开的场景")
	}

	select {
	case an := <-got:
		if an.NodeID != kpA.NodeID() {
			t.Fatalf("wrong sender: %s", an.NodeID)
		}
		if !identity.Verify(an.PublicKey, an.SigningBytes(), an.Sig) {
			t.Fatal("announce signature invalid")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("接收方(端口 %d)始终没收到发送方(端口 %d)的 ANNOUNCE —— AnnouncePorts 未生效",
			portB, a.UDPPort())
	}
}
