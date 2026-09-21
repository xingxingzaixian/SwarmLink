package app

import (
	"context"
	"fmt"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
)

// SelfCheckResult 是部署前置条件（P-1 / P-2）的自检结果。
//
// 为什么必须做成可执行的一次自检：这两条前提一旦不成立，现象是「跨网段发现不到人」
// 而不是报错 —— 用户会以为是软件坏了，很难联想到是网段规划问题。
// 因此把判断固化成代码，而不是留在文档里靠人读。
type SelfCheckResult struct {
	// Performed 为 false 表示未配置种子，自检跳过（单网段部署本就不需要）。
	Performed bool
	SeedCount int
	// Learned 是本轮通过种子学到的跨网段目录条数。
	Learned  int
	P1OK     bool
	P1Detail string
	// P2Overlap 为真表示学到的条目与本机处于同一地址段，地址段可能重叠。
	P2Overlap bool
	P2Detail  string
}

// SelfCheckInput 是自检所需输入，全部由组合根注入。
type SelfCheckInput struct {
	Discovery  *DiscoveryApp
	Peers      ports.PeerDirectory
	SelfSubnet string
}

// SelfCheck 执行 P-1 / P-2 自检。
//
// 判定口径与部署文档一致：
//   - P-1：任意两网段的种子 IP 之间可 TCP 直连（三层路由可达，非 NAT 隔离）
//   - P-2：各网段使用不重叠的地址段（不得都是 192.168.1.0/24）
func SelfCheck(ctx context.Context, in SelfCheckInput) SelfCheckResult {
	seeds := in.Discovery.SeedSnapshot()
	if len(seeds) == 0 {
		return SelfCheckResult{P1Detail: "未配置种子，跳过（单网段部署无需自检）"}
	}

	res := SelfCheckResult{Performed: true, SeedCount: len(seeds)}

	in.Discovery.RefreshSeeds(ctx)

	// 目录拉取是异步的（UDP 探测 → TCP 全量拉取），给它一点时间落地。
	learned := seedSourced(in.Peers)
	for i := 0; i < 30 && len(learned) == 0; i++ {
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
		learned = seedSourced(in.Peers)
	}

	res.Learned = len(learned)
	if res.Learned > 0 {
		res.P1OK = true
		res.P1Detail = fmt.Sprintf("已通过种子探通并拉取到 %d 条跨网段目录", res.Learned)
	} else {
		res.P1Detail = "无法通过种子获取目录 —— 跨网段将完全失效。" +
			"请确认任意两网段的种子 IP 之间可 TCP 直连（三层路由可达，非 NAT 隔离）"
	}

	res.P2Overlap, res.P2Detail = checkOverlap(learned, in.SelfSubnet)
	return res
}

// seedSourced 返回目录里由种子学到的条目（SourceSeed）。
func seedSourced(dir ports.PeerDirectory) []peer.Peer {
	if dir == nil {
		return nil
	}
	return dir.List(ports.PeerFilter{Source: peer.SourceSeed})
}

// checkOverlap 判断「学到的条目与本机同地址段」，即 P-2 可能不成立。
//
// 它只是一个警告而非硬失败：同地址段也可能确实就是同一网段，
// 只有当用户明确自己处于多网段环境时才说明规划有问题。
func checkOverlap(learned []peer.Peer, selfSubnet string) (bool, string) {
	if selfSubnet == "" {
		return false, "未获取本机网段，跳过 P-2 检查"
	}
	n := 0
	for _, p := range learned {
		if p.Subnet != "" && p.Subnet == selfSubnet {
			n++
		}
	}
	if n == 0 {
		return false, "未观察到地址段重叠的迹象"
	}
	return true, fmt.Sprintf("通过种子学到的条目中有 %d 个与本机同处 %s。"+
		"若它们实际位于不同物理网段，说明各网段地址段重叠，跨网段寻址会崩溃且现象隐蔽", n, selfSubnet)
}
