package app

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// DiscoveryApp 编排跨网段发现（ADR-011）：多种子 + 单跳目录拉取。
//
// 种子【零特权】：它与普通节点跑完全相同的 PEER_LIST_REQ/RESP，
// 「特殊性」完全外置在配置文件里。
type DiscoveryApp struct {
	self     identity.NodeID
	peers    *PeerApp
	dir      ports.PeerDirectory
	conns    ports.ConnManager
	registry ports.SeedRegistry
	prober   ports.SeedProber
	bus      ports.EventBus
	clk      ports.Clock
	lg       *slog.Logger

	// LocalEpoch 返回本地目录 epoch（由发现适配器维护）。
	LocalEpoch      func() uint64
	RefreshInterval time.Duration
	// SeedsPerRefresh 每轮随机拉取的种子数（v1.0 取简法：2）。
	SeedsPerRefresh int
	MaxEntries      int
	// SeedTCPPortFallback 是种子的兜底 TCP 端口（探测未学到 tcp_port 时使用）。
	SeedTCPPortFallback uint16

	mu sync.Mutex
}

// NewDiscoveryApp 构造发现应用。
func NewDiscoveryApp(
	self identity.NodeID,
	peers *PeerApp,
	dir ports.PeerDirectory,
	conns ports.ConnManager,
	registry ports.SeedRegistry,
	prober ports.SeedProber,
	bus ports.EventBus,
	clk ports.Clock,
	lg *slog.Logger,
) *DiscoveryApp {
	return &DiscoveryApp{
		self:            self,
		peers:           peers,
		dir:             dir,
		conns:           conns,
		registry:        registry,
		prober:          prober,
		bus:             bus,
		clk:             clk,
		lg:              lg,
		RefreshInterval: 300 * time.Second,
		SeedsPerRefresh: 2,
		MaxEntries:      256,
	}
}

// Start 启动种子刷新循环（跨网段目录刷新周期，同时也是可见性收敛上界）。
func (d *DiscoveryApp) Start(ctx context.Context) {
	if d.registry == nil || d.prober == nil || d.RefreshInterval <= 0 {
		return
	}
	go func() {
		d.RefreshSeeds(ctx)

		t := d.clk.NewTicker(d.RefreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C():
				d.RefreshSeeds(ctx)
			}
		}
	}()
}

// RefreshSeeds 每轮随机挑 N 颗可达种子：UDP 探测 → 学 ANNOUNCE → TCP 拉全量目录。
//
// 随机 2 颗而非全部：既保证单颗种子宕机仍能拿到跨网段目录，又天然分散负载。
func (d *DiscoveryApp) RefreshSeeds(ctx context.Context) {
	n := d.SeedsPerRefresh
	if n <= 0 {
		n = 2
	}
	for _, addr := range d.registry.Pick(n) {
		an, err := d.prober.Probe(ctx, addr)
		if err != nil {
			d.registry.Report(addr, nil, err)
			if d.lg != nil {
				d.lg.Debug("seed probe failed", "seed", addr.String(), "err", err)
			}
			continue
		}
		d.registry.Report(addr, an, nil)

		// 种子产出的就是普通 Announcement，与广播完全同构 → 同一条 Upsert 路径。
		if d.peers != nil {
			if err := d.peers.OnAnnouncement(*an); err != nil && d.lg != nil {
				d.lg.Warn("seed announce upsert failed", "err", err)
			}
		}
		// 第二跳：TCP 单播拉取该种子手里的全量目录（含它从别的网段学来的条目）。
		go d.pullFromSeed(ctx, addr, *an)
	}
}

func (d *DiscoveryApp) pullFromSeed(ctx context.Context, addr peer.SeedAddr, an peer.Announcement) {
	port := an.TCPPort
	if port == 0 {
		port = d.SeedTCPPortFallback
	}
	if port == 0 {
		return
	}
	target := net.JoinHostPort(addr.IP, strconv.Itoa(int(port)))

	sess, err := d.conns.Dial(ctx, an.NodeID, target)
	if err != nil {
		if d.lg != nil {
			d.lg.Debug("seed dial failed", "seed", addr.String(), "err", err)
		}
		return
	}

	// 请求体刻意不带 list_epoch：服务端无条件返回全量目录（见 HandlePeerListReq）。
	// 早先这里会回填「上一轮学到的 epoch」，但那个值不再影响任何判定 —— 留着一个
	// 看起来在跟踪、实际决定不了任何事的字段，正是本仓反复吃亏的形态。
	payload, err := protocol.EncodeJSON(protocol.PeerListReqWire{})
	if err != nil {
		return
	}
	_ = sess.Send(protocol.New(protocol.TypePeerListReq, payload))
}

// HandlePeerListReq 响应目录请求：无条件返回全量目录。
//
// 为什么不再做「epoch 相同就回空响应」的省流优化（原实现如此，已确认是缺陷）：
// 本地 epoch 恒为 1 —— Broadcaster.BumpEpoch 在全仓没有任何调用点 —— 而客户端会把
// 上一轮响应里的 epoch 原样记下来、下次请求时带回来，于是 req.ListEpoch == local
// 从第二轮起永远成立。结果是同一对 (本机, 种子) 只会同步一次，种子后来学到的
// 节点再也传不过来，直到本机重启；而同网段广播不受影响，所以现象是「跨网段的新
// 节点一直看不见」而不是报错。详见《发现协议设计书》§10.3 / §15.2。
//
// 取简法的成本（§15.2 的核算）：200 节点下一次全量响应约 12.8 KB、每节点 5 分钟
// 一次 ⇒ 单种子出站 < 2 KB/s，可以忽略。换来的是「恒正确」，而不是一条依赖调用者
// 自觉去 bump epoch 的优化 —— 后者正是本仓反复吃过亏的「多处真相」形态。
//
// req.ListEpoch 仍然解析（线上格式保持不变，不破坏兼容），但当前不参与判定。
// 将来若真的要做增量，必须同时定下「谁负责 bump」的单一真相，否则就会退回今天这个状态。
func (d *DiscoveryApp) HandlePeerListReq(sess ports.Session, f protocol.Frame) error {
	var req protocol.PeerListReqWire
	_ = protocol.DecodeJSON(f.Payload, &req)

	local := uint64(0)
	if d.LocalEpoch != nil {
		local = d.LocalEpoch()
	}
	return d.sendListResp(sess, protocol.PeerListRespWire{Epoch: local, Entries: d.entries()})
}

// HandlePeerListResp 合并对端返回的全量目录。
//
// 防环：条目按 node_id 去重（Upsert 天然保证）；LastSeen 用【本地到达时间】，
// 因此不需要 via / hop_count / 来源标记。
func (d *DiscoveryApp) HandlePeerListResp(sess ports.Session, f protocol.Frame) error {
	var resp protocol.PeerListRespWire
	if err := protocol.DecodeJSON(f.Payload, &resp); err != nil {
		return err
	}

	now := d.clk.Now()
	for _, e := range resp.Entries {
		id, err := identity.ParseNodeID(e.NodeID)
		if err != nil || id == d.self {
			continue
		}
		// 被本地移除的节点：整条跳过，而不是"让它 Upsert 后被粘性吃掉"。
		//
		// 这里的显式判断不能省：仓储的粘性保住的是【落库状态】，而下面的循环会把
		// p（State=discovered）发到事件总线上 —— 事件桥照发，前端 peers store 对未知
		// nodeId 会 push 回列表（于是删掉的节点每隔一个通告周期闪现一次），
		// 连接层与传输等待也会据此去拨号一个用户已经移除的节点。
		//
		// 本路径【不触发恢复】：目录条目是第三方转述、没有签名（设计 §5.3 ③）。
		if prev, ok := d.dir.Get(id); ok && prev.State == peer.StateBlocked {
			continue
		}
		p := peer.Peer{
			NodeID:      id,
			DisplayName: e.DisplayName,
			LastAddr:    e.Addr,
			Subnet:      e.Subnet,
			Caps:        peer.Caps(e.Caps),
			ProtoVer:    e.ProtoVer,
			LastSeen:    now,
			State:       peer.StateDiscovered,
			Source:      peer.SourceSeed,
		}
		if e.PublicKey != "" {
			if pub, err := identity.ParsePublic(e.PublicKey); err == nil {
				p.PublicKey = pub
			}
		}
		if err := d.dir.Upsert(p); err != nil {
			continue
		}
		if d.bus != nil {
			d.bus.Publish(eventbus.TopicPeerDiscovered, p)
		}
	}
	return nil
}

func (d *DiscoveryApp) sendListResp(sess ports.Session, resp protocol.PeerListRespWire) error {
	payload, err := protocol.EncodeJSON(resp)
	if err != nil {
		return err
	}
	return sess.Send(protocol.New(protocol.TypePeerListResp, payload))
}

func (d *DiscoveryApp) entries() []protocol.PeerListEntry {
	limit := d.MaxEntries
	if limit <= 0 {
		limit = 256
	}
	ps := d.dir.List(ports.PeerFilter{Limit: limit})

	out := make([]protocol.PeerListEntry, 0, len(ps))
	for _, p := range ps {
		// 被本地移除的节点不会走到这里：List 的默认语义已经排除 blocked
		// （ports.PeerFilter.IncludeBlocked）。此处不再重复过滤 —— 过滤规则只留一处真相。
		entry := protocol.PeerListEntry{
			NodeID:      p.NodeID.String(),
			DisplayName: p.DisplayName,
			Addr:        p.LastAddr,
			Subnet:      p.Subnet,
			Caps:        uint32(p.Caps),
			ProtoVer:    p.ProtoVer,
		}
		if len(p.PublicKey) == ed25519.PublicKeySize {
			entry.PublicKey = identity.MarshalPublic(p.PublicKey)
		}
		out = append(out, entry)
	}
	return out
}

// SeedSnapshot 暴露种子状态（诊断面板用）。
func (d *DiscoveryApp) SeedSnapshot() []peer.SeedAddr {
	if d.registry == nil {
		return nil
	}
	return d.registry.Pick(1 << 10)
}

// SetRegistry 替换种子注册表。
//
// 用途：种子地址在配置里是静态的，但集成测试需要在节点启动拿到端口后
// 才能构造 SeedAddr，因此提供这一注入点。
func (d *DiscoveryApp) SetRegistry(r ports.SeedRegistry) {
	d.mu.Lock()
	d.registry = r
	d.mu.Unlock()
}
