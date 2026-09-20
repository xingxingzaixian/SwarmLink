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

	mu        sync.Mutex
	lastEpoch map[identity.NodeID]uint64
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
		lastEpoch:       make(map[identity.NodeID]uint64),
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

	epoch := d.epochOf(an.NodeID)
	payload, err := protocol.EncodeJSON(protocol.PeerListReqWire{ListEpoch: epoch})
	if err != nil {
		return
	}
	_ = sess.Send(protocol.New(protocol.TypePeerListReq, payload))
}

// HandlePeerListReq 响应目录请求：epoch 相同则回空响应（省流）。
func (d *DiscoveryApp) HandlePeerListReq(sess ports.Session, f protocol.Frame) error {
	var req protocol.PeerListReqWire
	_ = protocol.DecodeJSON(f.Payload, &req)

	local := uint64(0)
	if d.LocalEpoch != nil {
		local = d.LocalEpoch()
	}
	if req.ListEpoch != 0 && req.ListEpoch == local {
		return d.sendListResp(sess, protocol.PeerListRespWire{Epoch: local})
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
	d.setEpoch(sess.PeerID(), resp.Epoch)

	now := d.clk.Now()
	for _, e := range resp.Entries {
		id, err := identity.ParseNodeID(e.NodeID)
		if err != nil || id == d.self {
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
		if p.State == peer.StateBlocked {
			continue
		}
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

func (d *DiscoveryApp) epochOf(id identity.NodeID) uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastEpoch[id]
}

func (d *DiscoveryApp) setEpoch(id identity.NodeID, epoch uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastEpoch[id] = epoch
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
