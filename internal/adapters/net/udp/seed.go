package udp

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
)

// ---------------------------------------------------------------------------
// 探测（第一跳）
// ---------------------------------------------------------------------------

// Prober 执行种子第一跳：UDP 探测 → 种子单播回 ANNOUNCE（含签名）。
//
// 为什么第一跳走 UDP 而不是直接 TCP：被选为种子的机器必须固定 udp_port
// （不允许自动回退），否则跨网段节点拿着配置里的端口发第一个报文就石沉大海。
// 只有这一个端口需要固定；tcp_port 照样可以回退，由回传的 ANNOUNCE 学到。
type Prober struct {
	Timeout time.Duration
	// LocalDialer 可选：用于把探测源绑定到指定接口。
	LocalAddr *net.UDPAddr
}

// Probe 探测一颗种子并返回其（已验签的）ANNOUNCE。
func (p *Prober) Probe(ctx context.Context, addr peer.SeedAddr) (*peer.Announcement, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	raddr := &net.UDPAddr{IP: net.ParseIP(addr.IP), Port: int(addr.UDPPort)}
	if raddr.IP == nil {
		return nil, fmt.Errorf("udp: invalid seed ip %q", addr.IP)
	}

	// DialUDP 建立「已连接」socket：只接收来自该地址的回复，天然过滤伪造源。
	conn, err := net.DialUDP("udp4", p.LocalAddr, raddr)
	if err != nil {
		return nil, fmt.Errorf("udp: probe dial %s: %w", addr, err)
	}
	defer conn.Close()

	probe, err := protocol.EncodeUDP(protocol.UDPTypeSeedProbe, protocol.SeedProbeWire{})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(probe); err != nil {
		return nil, fmt.Errorf("udp: probe write %s: %w", addr, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))

	buf := make([]byte, 65535)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := conn.Read(buf)
		if err != nil {
			return nil, fmt.Errorf("udp: probe read %s: %w", addr, err)
		}
		typ, body, err := protocol.DecodeUDP(buf[:n])
		if err != nil || typ != protocol.UDPTypeAnnounce {
			continue
		}
		var w protocol.AnnounceWire
		if protocol.DecodeJSON(body, &w) != nil {
			continue
		}
		a, err := w.Announcement()
		if err != nil {
			continue
		}
		// 种子身份从第一跳起即可验证，不给中间人留口子。
		if !identity.Verify(a.PublicKey, a.SigningBytes(), a.Sig) {
			continue
		}
		// 身份自洽：与 broadcast.go 的 decodeAnnounce 同一口径（发现协议设计书 §15.1）。
		// 种子是跨网段唯一的入口，这里放行一条自相矛盾的条目，被污染的不只是本机目录。
		if identity.Fingerprint(a.PublicKey) != a.NodeID {
			continue
		}
		a.Source = peer.SourceSeed
		a.ObservedIP = addr.IP
		return &a, nil
	}
}

// ---------------------------------------------------------------------------
// 注册表
// ---------------------------------------------------------------------------

type seedState struct {
	addr      peer.SeedAddr
	failCnt   int
	nextTry   time.Time
	lastSeen  time.Time
	nodeID    string
	tcpPort   uint16
	subnet    string
	lastEpoch uint64
}

// SeedState 是种子的只读快照（用于持久化与诊断面板）。
type SeedState struct {
	Addr      peer.SeedAddr
	FailCnt   int
	LastSeen  time.Time
	NodeID    string
	TCPPort   uint16
	Subnet    string
	LastEpoch uint64
}

// Available 报告这颗种子当前是否可用（状态灯的 N）。
//
// 「未失败即可用」：每轮只随机探 SeedsPerRefresh 颗，因此刚填进清单的种子
// 在轮到它之前 LastSeen 还是零值。若把「还没探过」算成不可用，用户刚配完
// 就会看到「0/4」并把这条配置当成错的 —— 而实际上什么都没坏。
//
// 反过来，一次探测失败就立刻算不可用：那是有证据的失败，此时把种子显示成
// 健康会让「跨网段断了但灯是绿的」成为最难排查的状态。
func (s SeedState) Available() bool { return s.FailCnt == 0 }

// Registry 实现 ports.SeedRegistry。
//
// 关键：种子【不拥有任何特权接口】。它产出的就是普通的 peer.Announcement，
// 与广播产出的结构完全同构 → 走同一条 Upsert 路径进入 PeerDirectory。
type Registry struct {
	mu    sync.Mutex
	clk   ports.Clock
	seeds []*seedState
	rnd   *rand.Rand

	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

var _ ports.SeedRegistry = (*Registry)(nil)

// NewRegistry 由配置清单构造注册表。
func NewRegistry(addrs []peer.SeedAddr, clk ports.Clock) *Registry {
	r := &Registry{
		clk:         clk,
		rnd:         rand.New(rand.NewSource(time.Now().UnixNano())),
		BaseBackoff: time.Second,
		MaxBackoff:  10 * time.Minute,
	}
	for _, a := range addrs {
		if a.IsZero() {
			continue
		}
		r.seeds = append(r.seeds, &seedState{addr: a})
	}
	return r
}

// Len 返回种子总数。
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seeds)
}

// List 返回全部配置的种子（含正在退避的），按配置顺序。
func (r *Registry) List() []peer.SeedAddr {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]peer.SeedAddr, 0, len(r.seeds))
	for _, s := range r.seeds {
		out = append(out, s.addr)
	}
	return out
}

// Pick 返回本轮该问的 N 颗种子：先过滤仍在退避中的，再随机取。
// 随机而非固定，避免 200 个节点集中打同一颗。
func (r *Registry) Pick(n int) []peer.SeedAddr {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.clk.Now()
	cand := make([]*seedState, 0, len(r.seeds))
	for _, s := range r.seeds {
		if s.failCnt > 0 && now.Before(s.nextTry) {
			continue
		}
		cand = append(cand, s)
	}
	// 全部在退避中 → 本轮不拉，避免反复探测已下线的种子
	if len(cand) == 0 {
		return nil
	}

	r.rnd.Shuffle(len(cand), func(i, j int) { cand[i], cand[j] = cand[j], cand[i] })
	if n > len(cand) {
		n = len(cand)
	}
	out := make([]peer.SeedAddr, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, cand[i].addr)
	}
	return out
}

// Report 回填探测/拉取结果。learned == nil 表示失败，据此累计 fail_cnt 并指数退避。
func (r *Registry) Report(addr peer.SeedAddr, learned *peer.Announcement, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	s := r.find(addr)
	if s == nil {
		return
	}

	if err != nil || learned == nil {
		s.failCnt++
		exp := s.failCnt
		if exp > 10 {
			exp = 10
		}
		d := r.BaseBackoff * time.Duration(1<<exp)
		if d > r.MaxBackoff {
			d = r.MaxBackoff
		}
		s.nextTry = r.clk.Now().Add(d)
		return
	}

	s.failCnt = 0
	s.nextTry = time.Time{}
	s.lastSeen = r.clk.Now()
	s.nodeID = learned.NodeID.String()
	s.tcpPort = learned.TCPPort
	s.subnet = learned.Subnet
}

// SetLastEpoch 记录最近一次目录响应的 list_epoch（相同则下次不发请求）。
func (r *Registry) SetLastEpoch(addr peer.SeedAddr, epoch uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.find(addr); s != nil {
		s.lastEpoch = epoch
	}
}

// LastEpoch 返回已知的目录 epoch。
func (r *Registry) LastEpoch(addr peer.SeedAddr) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.find(addr); s != nil {
		return s.lastEpoch
	}
	return 0
}

// Snapshot 返回全部种子的只读快照。
func (r *Registry) Snapshot() []SeedState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SeedState, 0, len(r.seeds))
	for _, s := range r.seeds {
		out = append(out, SeedState{
			Addr:      s.addr,
			FailCnt:   s.failCnt,
			LastSeen:  s.lastSeen,
			NodeID:    s.nodeID,
			TCPPort:   s.tcpPort,
			Subnet:    s.subnet,
			LastEpoch: s.lastEpoch,
		})
	}
	return out
}

// Replace 用新清单替换种子集合（设置页保存后热生效）。
//
// 保留同一地址的历史状态：用户改的是「清单」，不是「这些种子的健康状况」。
// 若整体重建，所有种子会同时被立刻重探一遍（退避被清零），
// 现象是「点保存之后网络抖了一下」，还会让本来在退避的坏种子立刻再打一次。
func (r *Registry) Replace(addrs []peer.SeedAddr) {
	r.mu.Lock()
	defer r.mu.Unlock()

	prev := make(map[peer.SeedAddr]*seedState, len(r.seeds))
	for _, s := range r.seeds {
		prev[s.addr] = s
	}

	next := make([]*seedState, 0, len(addrs))
	seen := make(map[peer.SeedAddr]struct{}, len(addrs))
	for _, a := range addrs {
		if a.IsZero() {
			continue
		}
		// 去重：同一地址写两遍会让 Report 永远回填到第一条，
		// 第二份状态从此再也不会更新 —— 界面上就是「同一颗种子，两种状态」。
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}

		if s, ok := prev[a]; ok {
			next = append(next, s)
			continue
		}
		next = append(next, &seedState{addr: a})
	}
	r.seeds = next
}

func (r *Registry) find(addr peer.SeedAddr) *seedState {
	for _, s := range r.seeds {
		if s.addr == addr {
			return s
		}
	}
	return nil
}
