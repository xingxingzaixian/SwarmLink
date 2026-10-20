package app

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/domain/share"
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

	// reload 用来叫醒刷新循环：设置页保存新的种子清单/刷新周期后，
	// 循环可能正睡在旧周期的 ticker 上。不叫醒的话「保存即时生效」只是
	// 半句话 —— 用户要等旧周期到点才看到变化（最长 5 分钟）。
	reload chan struct{}
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
		reload:          make(chan struct{}, 1),
	}
}

// seedSnapshot 一次性取走刷新循环需要的可变状态（全部在 d.mu 下）。
//
// 为什么不在循环里直接读字段：ApplySeeds 会在运行中改它们，
// 无锁读写在 -race 下是明确的 data race，而这类竞态的表现是
// 「偶尔用旧周期再跑一轮」，最难复现。
func (d *DiscoveryApp) seedSnapshot() (ports.SeedRegistry, ports.SeedProber, int, time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.registry, d.prober, d.SeedsPerRefresh, d.RefreshInterval
}

// Start 启动种子刷新循环（跨网段目录刷新周期，同时也是可见性收敛上界）。
func (d *DiscoveryApp) Start(ctx context.Context) {
	registry, prober, _, interval := d.seedSnapshot()
	if registry == nil || prober == nil || interval <= 0 {
		return
	}
	go d.loop(ctx)
}

// loop 是刷新循环。
//
// 每轮都【重建 ticker】而不是复用一个：只有这样才能让「改刷新周期」
// 立刻生效（ApplySeeds 通过 reload 通道把它叫醒）。代价是每次多一个
// 定时器对象，而它 5 分钟才出现一次。
func (d *DiscoveryApp) loop(ctx context.Context) {
	d.RefreshSeeds(ctx)

	for {
		_, _, _, interval := d.seedSnapshot()
		if interval <= 0 {
			interval = 300 * time.Second
		}

		t := d.clk.NewTicker(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-d.reload:
			// 保存了新清单：立刻按新配置拉一轮，别等下一次到点。
			t.Stop()
			d.RefreshSeeds(ctx)
		case <-t.C():
			t.Stop()
			d.RefreshSeeds(ctx)
		}
	}
}

// ApplySeeds 热更新种子配置（设置页保存后由服务层调用）。
//
// 做了三件事，缺一件「即时生效」都是假的：
//  1. 替换注册表内容（保留同址种子的失败计数与退避，见 udp.Registry.Replace）；
//  2. 更新每轮拉取数与刷新周期；
//  3. 叫醒刷新循环 —— 它可能正睡在旧周期的 ticker 上。
func (d *DiscoveryApp) ApplySeeds(addrs []peer.SeedAddr, perRefresh int, interval time.Duration) {
	d.mu.Lock()
	registry := d.registry
	if perRefresh > 0 {
		d.SeedsPerRefresh = perRefresh
	}
	if interval > 0 {
		d.RefreshInterval = interval
	}
	reload := d.reload
	d.mu.Unlock()

	if registry != nil {
		registry.Replace(addrs)
	}
	if reload != nil {
		select {
		case reload <- struct{}{}:
		default:
			// 已经有一个待处理的唤醒信号，丢掉这次即可（同一次保存不需要两次）
		}
	}
}

// RefreshSeeds 每轮随机挑 N 颗可达种子：UDP 探测 → 学 ANNOUNCE → TCP 拉全量目录。
//
// 随机 2 颗而非全部：既保证单颗种子宕机仍能拿到跨网段目录，又天然分散负载。
func (d *DiscoveryApp) RefreshSeeds(ctx context.Context) {
	registry, prober, n, _ := d.seedSnapshot()
	if registry == nil || prober == nil {
		return
	}
	if n <= 0 {
		n = 2
	}
	for _, addr := range registry.Pick(n) {
		an, err := prober.Probe(ctx, addr)
		if err != nil {
			registry.Report(addr, nil, err)
			if d.lg != nil {
				d.lg.Debug("seed probe failed", "seed", addr.String(), "err", err)
			}
			continue
		}
		registry.Report(addr, an, nil)

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
			// 转述来的摘要照单全收（与其它字段同一条路径）：
			// 它只用于展示「去哪儿问」，真伪由共享方在收到请求时校验。
			Shares: share.NormalizeSummaries(e.Shares),
			// 版本同样是转述：只用于候选排序，真值以会话内 UPDATE_INFO_RESP 为准。
			AppVersion: e.AppVersion,
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
			Shares:      p.Shares,
			AppVersion:  p.AppVersion,
		}
		if len(p.PublicKey) == ed25519.PublicKeySize {
			entry.PublicKey = identity.MarshalPublic(p.PublicKey)
		}
		out = append(out, entry)
	}
	return out
}

// SeedSnapshot 暴露「配置了哪些种子」（自检与诊断用）。
//
// 用 List 而不是 Pick：Pick 会跳过正在退避的种子，于是「配了两颗、都连不上」
// 会表现为「一颗都没配」，而这两者的修法完全不同。
func (d *DiscoveryApp) SeedSnapshot() []peer.SeedAddr {
	registry, _, _, _ := d.seedSnapshot()
	if registry == nil {
		return nil
	}
	return registry.List()
}

// SetRegistry 替换种子注册表。
//
// 用途：种子地址在配置里是静态的，但集成测试需要在节点启动拿到端口后
// 才能构造 SeedAddr，因此提供这一注入点。
func (d *DiscoveryApp) SetRegistry(r ports.SeedRegistry) {
	d.mu.Lock()
	d.registry = r
	reload := d.reload
	d.mu.Unlock()
	if reload != nil {
		select {
		case reload <- struct{}{}:
		default:
		}
	}
}

// SeedTestResult 是「测试一颗种子」的结果（设置页逐颗测试用）。
type SeedTestResult struct {
	// Addr 是规范化后的地址（"ip:udp_port"）。
	Addr string
	// UDPOK：种子应答了探测，且签名与 node_id 自洽。
	UDPOK bool
	// TCPOK：探测学到的 TCP 端口能建立会话（证明三层可达）。
	TCPOK bool
	// RTT 是 UDP 探测往返耗时。
	RTT         time.Duration
	NodeID      string
	DisplayName string
	Subnet      string
	TCPPort     uint16
	// Err 是给用户看的中文失败原因（空串 = 全通）。
	//
	// 分开描述 UDP / TCP 两跳是刻意的：用户最常撞到的是「UDP 通、TCP 被防火墙挡」，
	// 一句笼统的「连接失败」会把他引到错误的方向去改。
	Err string
}

// TestSeed 对单颗种子做一次真实的双跳探测（UDP 通告 + TCP 目录拉取）。
//
// 与自检（SelfCheck）的区别：自检只看「整批能不能学到东西」，它无法回答
// 「这一颗到底通不通」；而部署现场最需要的就是这个定位能力。
//
// 副作用与正常刷新一致：探测成功会 Report 并把该节点 Upsert 进目录
// （测试通过即可在列表里看到它），失败会累计退避 —— 测试不该伪造一个
// 与真实运行不同的世界。
func (d *DiscoveryApp) TestSeed(ctx context.Context, raw string) SeedTestResult {
	res := SeedTestResult{Addr: strings.TrimSpace(raw)}

	addr, err := peer.ParseSeedAddr(raw)
	if err != nil {
		res.Err = "地址格式不对，应写成 IP:UDP端口（例如 192.168.2.10:2425）"
		return res
	}
	res.Addr = addr.String()

	registry, prober, _, _ := d.seedSnapshot()
	if prober == nil {
		res.Err = "发现服务未启动"
		return res
	}

	start := d.clk.Now()
	an, err := prober.Probe(ctx, addr)
	res.RTT = d.clk.Now().Sub(start)
	if err != nil {
		if registry != nil {
			registry.Report(addr, nil, err)
		}
		res.Err = "UDP 探测无应答：" + err.Error()
		return res
	}
	if registry != nil {
		registry.Report(addr, an, nil)
	}
	res.UDPOK = true
	res.NodeID = an.NodeID.String()
	res.DisplayName = an.DisplayName
	res.Subnet = an.Subnet
	res.TCPPort = an.TCPPort

	if d.peers != nil {
		if err := d.peers.OnAnnouncement(*an); err != nil && d.lg != nil {
			d.lg.Warn("seed test upsert failed", "seed", addr.String(), "err", err)
		}
	}

	port := an.TCPPort
	if port == 0 {
		port = d.SeedTCPPortFallback
	}
	if port == 0 || d.conns == nil {
		res.Err = "对方没有公布 TCP 端口，无法建立会话"
		return res
	}
	target := net.JoinHostPort(addr.IP, strconv.Itoa(int(port)))
	if _, err := d.conns.Dial(ctx, an.NodeID, target); err != nil {
		res.Err = "UDP 通了，但 TCP " + target + " 连不上：跨网段目录拉不到，检查防火墙与路由"
		return res
	}
	res.TCPOK = true
	return res
}
