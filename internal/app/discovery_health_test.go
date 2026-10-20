package app

import "testing"

// 状态灯的判定必须能覆盖「跨网段断了但同网段还通」这个最容易被忽略的形态：
// 只看「有没有节点」的话，它会被判成 live，绿色灯亮着而用户永远收不到
// 隔壁网段的消息。
func TestClassifyDiscovery(t *testing.T) {
	cases := []struct {
		name       string
		sameSubnet int
		seedTotal  int
		available  int
		want       DiscoveryState
	}{
		{"单机无种子", 0, 0, 0, DiscoveryLimited},
		{"单网段有邻居", 3, 0, 0, DiscoveryLive},
		{"同网段通且种子可用", 2, 4, 3, DiscoveryLive},
		{"同网段通但种子全挂", 2, 4, 0, DiscoveryPartial},
		{"同网段无邻居但种子可用", 0, 4, 1, DiscoveryPartial},
		{"同网段无邻居且种子全挂", 0, 4, 0, DiscoveryLimited},
		{"没配种子也没邻居", 0, 0, 0, DiscoveryLimited},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyDiscovery(c.sameSubnet, c.seedTotal, c.available); got != c.want {
				t.Fatalf("ClassifyDiscovery(%d,%d,%d)=%q want %q",
					c.sameSubnet, c.seedTotal, c.available, got, c.want)
			}
		})
	}
}

// 上游把可用数算多了（例如快照里混进了别的网段的种子）时，
// 状态灯不能显示 "5/4" 这种不存在的数字。
func TestNewDiscoveryHealthClampsCounts(t *testing.T) {
	h := NewDiscoveryHealth(-1, 4, 9)
	if h.SameSubnet != 0 {
		t.Fatalf("sameSubnet=%d want 0", h.SameSubnet)
	}
	if h.SeedAvailable != 4 || h.SeedTotal != 4 {
		t.Fatalf("available=%d total=%d want 4/4", h.SeedAvailable, h.SeedTotal)
	}
	if h.State != DiscoveryPartial {
		t.Fatalf("state=%q want partial（同网段没邻居）", h.State)
	}
}
