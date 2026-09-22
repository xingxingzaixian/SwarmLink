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

// 关闭节点必须主动宣告下线：对端据此立即判离线，而不是等 TTL（默认 5 分钟）。
//
// 这条链路此前完全不存在 —— 用户关掉客户端后，其他客户端因为「只在广播
// 里见过它」而一直显示在线（discovered 也算可达），最长要等 5 分钟才消失。
func TestStopBroadcastsBye(t *testing.T) {
	clk := clock.New()
	lo := net.ParseIP("127.0.0.1").To4()
	iface := []Interface{{Name: "lo0", IP: lo, Mask: net.CIDRMask(8, 32), Broadcast: lo}}

	// 接收方：SeedOnly 关掉自己的发送循环，只收。
	kpB, _ := identity.GenerateKeyPair()
	cfgB := DefaultConfig()
	cfgB.SeedOnly = true
	receiver := NewBroadcaster(kpB, cfgB, clk, nil)
	receiver.SetInterfaces(iface)

	got := make(chan peer.Announcement, 8)
	if err := receiver.Start(context.Background(), func(a peer.Announcement) {
		select {
		case got <- a:
		default:
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer receiver.Stop()

	// 发送方：绑定自己的端口，广播目标指向接收方。
	kpA, _ := identity.GenerateKeyPair()
	cfgA := DefaultConfig()
	cfgA.AnnounceInterval = time.Hour // 不要它自己发 ANNOUNCE 干扰断言
	cfgA.JitterRatio = 0
	sender := NewBroadcaster(kpA, cfgA, clk, nil)
	sender.SetInterfaces(iface)
	sender.SetAnnouncePorts([]int{receiver.UDPPort()})
	if err := sender.Start(context.Background(), func(peer.Announcement) {}); err != nil {
		t.Fatal(err)
	}
	if sender.UDPPort() == receiver.UDPPort() {
		t.Skip("发送方恰好绑到与接收方相同的端口，未构成可区分的场景")
	}

	// 关闭：应当发出 BYE
	if err := sender.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case an := <-got:
			if an.NodeID != kpA.NodeID() {
				t.Fatalf("收到的是别的节点的通告: %s", an.NodeID)
			}
			if !an.Leaving {
				t.Fatal("关闭时收到的不是 BYE（Leaving 必须为 true）")
			}
			if !identity.Verify(an.PublicKey, an.SigningBytes(), an.Sig) {
				t.Fatal("BYE 也必须带有效签名，否则任何人都能伪造别人的下线")
			}
			return // 通过
		case <-deadline:
			t.Fatal("Stop 没有广播 BYE：对端只能等 TTL 过期才认为该节点离线")
		}
	}
}

// 反复 Stop 不应重复发 BYE：退出后还在广播「我走了」是纯粹的噪声。
func TestStopIsIdempotent(t *testing.T) {
	clk := clock.New()
	lo := net.ParseIP("127.0.0.1").To4()
	iface := []Interface{{Name: "lo0", IP: lo, Mask: net.CIDRMask(8, 32), Broadcast: lo}}

	kp, _ := identity.GenerateKeyPair()
	cfg := DefaultConfig()
	cfg.SeedOnly = true // 不参与广播的节点没有下线可宣告
	b := NewBroadcaster(kp, cfg, clk, nil)
	b.SetInterfaces(iface)
	if err := b.Start(context.Background(), func(peer.Announcement) {}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if err := b.Stop(); err != nil {
			t.Fatalf("第 %d 次 Stop: %v", i+1, err)
		}
	}
}

// 默认通告周期必须落在 30~60s：它是「保活流量」与「新节点被发现的延迟」
// 之间的取舍点，写过一次 5s（200 节点 ≈ 40 pkt/s 常驻噪声）就够难受了。
func TestDefaultAnnounceIntervalWithinWindow(t *testing.T) {
	cfg := DefaultConfig()

	lo := time.Duration(float64(cfg.AnnounceInterval) * (1 - cfg.JitterRatio))
	hi := time.Duration(float64(cfg.AnnounceInterval) * (1 + cfg.JitterRatio))

	if lo < 29*time.Second || hi > 61*time.Second {
		t.Fatalf("抖动区间 %s~%s 超出 30~60s", lo.Round(time.Second), hi.Round(time.Second))
	}
	if cfg.JitterRatio <= 0 {
		t.Fatal("多节点同时重启需要抖动，否则会形成同步风暴")
	}
}
