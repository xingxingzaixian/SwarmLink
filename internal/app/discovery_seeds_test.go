package app

import (
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
)

// fakeRegistry 记录 Replace 收到的清单，并可以选择性地模拟「全部在退避」。
type fakeRegistry struct {
	addrs    []peer.SeedAddr
	pickable []peer.SeedAddr
	replaced int
}

func (f *fakeRegistry) Pick(int) []peer.SeedAddr                        { return f.pickable }
func (f *fakeRegistry) List() []peer.SeedAddr                           { return f.addrs }
func (f *fakeRegistry) Report(peer.SeedAddr, *peer.Announcement, error) {}

func (f *fakeRegistry) Replace(addrs []peer.SeedAddr) {
	f.addrs = append([]peer.SeedAddr(nil), addrs...)
	f.pickable = f.addrs
	f.replaced++
}

func newDiscoveryForTest(reg *fakeRegistry) *DiscoveryApp {
	return NewDiscoveryApp(identity.NodeID{}, nil, nil, nil, reg, nil, nil, clock.New(), nil)
}

// 保存设置后必须：替换清单、更新两个参数、并且叫醒刷新循环。
// 少最后一步时「保存即时生效」只是半句话 —— 用户要等旧周期到点（最长 5 分钟）。
func TestApplySeedsReplacesAndSignals(t *testing.T) {
	reg := &fakeRegistry{}
	d := newDiscoveryForTest(reg)

	want := []peer.SeedAddr{{IP: "10.0.0.1", UDPPort: 2425}}
	d.ApplySeeds(want, 3, 42*time.Second)

	if reg.replaced != 1 || len(reg.addrs) != 1 || reg.addrs[0] != want[0] {
		t.Fatalf("清单没有被替换：replaced=%d addrs=%v", reg.replaced, reg.addrs)
	}
	if d.SeedsPerRefresh != 3 {
		t.Fatalf("SeedsPerRefresh=%d want 3", d.SeedsPerRefresh)
	}
	if d.RefreshInterval != 42*time.Second {
		t.Fatalf("RefreshInterval=%v want 42s", d.RefreshInterval)
	}

	select {
	case <-d.reload:
	default:
		t.Fatal("没有叫醒刷新循环：新的刷新周期要等旧 ticker 到点才生效")
	}
}

// 非法参数（0 / 负数）不应该把已经配好的值改坏：设置页传 0 的语义是「没填」。
func TestApplySeedsIgnoresZeroValues(t *testing.T) {
	d := newDiscoveryForTest(&fakeRegistry{})
	d.SeedsPerRefresh = 2
	d.RefreshInterval = 5 * time.Minute

	d.ApplySeeds(nil, 0, 0)

	if d.SeedsPerRefresh != 2 || d.RefreshInterval != 5*time.Minute {
		t.Fatalf("零值不该覆盖现有配置：per=%d interval=%v", d.SeedsPerRefresh, d.RefreshInterval)
	}
}

// SeedSnapshot 供自检与诊断使用，必须反映【配置了几颗】。
// 用 Pick 会跳过正在退避的种子 → 「配了两颗都连不上」被读成「一颗都没配」，
// 而这两种故障的修法完全不同。
func TestSeedSnapshotUsesListNotPick(t *testing.T) {
	two := []peer.SeedAddr{{IP: "10.0.0.1", UDPPort: 2425}, {IP: "10.0.0.2", UDPPort: 2425}}
	d := newDiscoveryForTest(&fakeRegistry{addrs: two, pickable: nil})

	got := d.SeedSnapshot()
	if len(got) != 2 {
		t.Fatalf("SeedSnapshot 返回 %d 颗，want 2（退避中也不能从清单消失）", len(got))
	}
}
