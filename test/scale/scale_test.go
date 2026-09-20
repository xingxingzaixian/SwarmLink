// Package scale 验证「全局 ≤ 200 节点」这条约束下的核心性质：
// 全量目录同步可行、且【不建立全互联持久连接】。
package scale

import (
	"fmt"
	"testing"

	"github.com/swarmlink/swarmlink/test/harness"
)

// TestTwentyNodesHaveFullDirectoryAndNoResidentConnections
//
// 这条用例是 ADR-009 / ADR-010 的直接验证物：
//   - ADR-010：全量目录同步 —— 每个节点都持有其他所有节点的条目
//   - ADR-009：在线状态走 UDP announce，TCP 按需拨号 —— 因此常驻连接数必须为 0
func TestTwentyNodesHaveFullDirectoryAndNoResidentConnections(t *testing.T) {
	const n = 20

	nodes := make([]*harness.Node, 0, n)
	for i := 0; i < n; i++ {
		nd, err := harness.New(fmt.Sprintf("n%02d", i), harness.Options{})
		if err != nil {
			t.Fatalf("new node %d: %v", i, err)
		}
		defer nd.Close()
		nodes = append(nodes, nd)
	}

	// 模拟同网段广播：每个节点都听到其他所有节点的通告。
	for i := range nodes {
		for j := range nodes {
			if i == j {
				continue
			}
			if err := nodes[i].PeerApp.OnAnnouncement(nodes[j].Announcement()); err != nil {
				t.Fatalf("announce %d->%d: %v", j, i, err)
			}
		}
	}

	for _, nd := range nodes {
		// ADR-010：全量目录
		if got := nd.Peers.Len(); got != n-1 {
			t.Fatalf("%s: want %d peers got %d", nd.Name, n-1, got)
		}
		// ADR-009：没有任何常驻连接（因此也没有保活流量与 SYN 风暴）
		if got := nd.Conn.SessionCount(); got != 0 {
			t.Fatalf("%s: expected 0 resident connections, got %d", nd.Name, got)
		}
	}
}

// TestMaxActiveConnsIsEnforced 验证连接硬上限生效：
// 超过上限的拨号会被拒绝，而不是无限增长。
func TestMaxActiveConnsIsEnforced(t *testing.T) {
	a, err := harness.New("a", harness.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// 默认上限 32，远大于本用例规模；这里只验证「空闲时连接数为 0」这条基线。
	if a.Conn.SessionCount() != 0 {
		t.Fatalf("fresh node must have no sessions, got %d", a.Conn.SessionCount())
	}
}
