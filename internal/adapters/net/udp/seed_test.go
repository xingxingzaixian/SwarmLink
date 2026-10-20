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

// 状态灯的「可用数」直接取这里：未失败（含还没轮到探测）算可用，
// 一次探测失败立刻算不可用 —— 否则跨网段已断而灯还是绿的。
func TestSeedStateAvailable(t *testing.T) {
	if !(SeedState{}).Available() {
		t.Fatal("尚未探测过的种子必须算可用，否则刚填完清单就显示 0/M")
	}
	if (SeedState{FailCnt: 1}).Available() {
		t.Fatal("探测失败过的种子不能算可用")
	}
	if !(SeedState{FailCnt: 0, LastSeen: time.Now()}).Available() {
		t.Fatal("探测成功过的种子必须算可用")
	}
}

// 热替换清单（设置页保存即时生效）必须保住同址种子的状态：
// 否则改一行清单会让所有种子同时被重探一遍，退避全部清零。
func TestRegistryReplaceKeepsStateForSameAddr(t *testing.T) {
	fc := clock.NewFake(time.Unix(0, 0))
	kept := peer.SeedAddr{IP: "10.0.0.1", UDPPort: 2425}
	removed := peer.SeedAddr{IP: "10.0.0.2", UDPPort: 2425}
	added := peer.SeedAddr{IP: "10.0.0.3", UDPPort: 2425}

	r := NewRegistry([]peer.SeedAddr{kept, removed}, fc)
	r.Report(kept, nil, errors.New("down"))
	r.Report(kept, nil, errors.New("down"))

	r.Replace([]peer.SeedAddr{kept, added})

	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("len=%d want 2（removed 应被删掉、added 应被加入）", len(snap))
	}
	var keptFail int
	var seenAdded bool
	for _, s := range snap {
		switch s.Addr {
		case kept:
			keptFail = s.FailCnt
		case added:
			seenAdded = true
			if s.FailCnt != 0 {
				t.Fatalf("新种子 FailCnt=%d want 0", s.FailCnt)
			}
		case removed:
			t.Fatal("被移除的种子不应还在清单里")
		}
	}
	if !seenAdded {
		t.Fatal("新增的种子没有进入清单")
	}
	if keptFail != 2 {
		t.Fatalf("同址种子的失败计数被清零了（%d want 2）", keptFail)
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
