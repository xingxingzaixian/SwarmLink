package app

// DiscoveryState 是顶部「发现状态灯」的结论（规格 1.2）。
//
// 为什么由后端判定而不是前端拼：同一组数字在诊断面板、状态灯、自检结果
// 三处出现，阈值口径散在前端组件里就一定会分叉。这里只做一次判定，
// 前端负责把它翻译成人话（NETWORK LIVE / PARTIAL / LIMITED）。
type DiscoveryState string

const (
	// DiscoveryLive：同网段有邻居，且「若配了种子，至少一颗可用」。
	// 单网段部署（没配种子）落在这里才是对的 —— 它本来就不需要种子。
	DiscoveryLive DiscoveryState = "live"

	// DiscoveryPartial：还能发现到人，但存在明确缺口 ——
	// 要么同网段通而种子全挂（跨网段已经断了），要么只有种子可用而同网段没邻居。
	DiscoveryPartial DiscoveryState = "partial"

	// DiscoveryLimited：既没有同网段邻居，也没有可用种子。
	// 单机、网卡配错、防火墙全挡，都会落到这里。
	DiscoveryLimited DiscoveryState = "limited"
)

// DiscoveryHealth 是发现层的聚合健康度（状态灯的唯一数据源）。
type DiscoveryHealth struct {
	// SameSubnet 是同网段、且发得出消息的邻居数（只计广播/组播来源）。
	SameSubnet int
	// SeedTotal 是配置的种子总数（M）。
	SeedTotal int
	// SeedAvailable 是当前可用的种子数（N）：连续失败 0 次即算可用。
	SeedAvailable int
	// State 是判定结论。
	State DiscoveryState
}

// NewDiscoveryHealth 组装并判定。计数非法时向「更保守」的一侧收敛：
// 可用数不会超过总数，负数一律按 0 处理 —— 状态灯宁可少报可用，
// 也不能在种子全挂时还显示绿色。
func NewDiscoveryHealth(sameSubnet, seedTotal, seedAvailable int) DiscoveryHealth {
	h := DiscoveryHealth{SameSubnet: max0(sameSubnet), SeedTotal: max0(seedTotal)}
	h.SeedAvailable = min(max0(seedAvailable), h.SeedTotal)
	h.State = ClassifyDiscovery(h.SameSubnet, h.SeedTotal, h.SeedAvailable)
	return h
}

// ClassifyDiscovery 是判定规则本身（纯函数，便于表驱动测试）。
//
// 注意「配了种子但一颗都没探过」的情形：可用数按「未失败即可用」计
// （见 udp.SeedState.Available），因此刚填完种子不会立刻显示 0/M。
// 那会让人以为配错了，而实际上只是每轮只随机探 N 颗、还没轮到它。
func ClassifyDiscovery(sameSubnet, seedTotal, seedAvailable int) DiscoveryState {
	switch {
	case sameSubnet <= 0 && seedAvailable <= 0:
		return DiscoveryLimited
	case sameSubnet > 0 && (seedTotal <= 0 || seedAvailable > 0):
		return DiscoveryLive
	default:
		return DiscoveryPartial
	}
}

func max0(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
