// Package app 是 L3 应用层：编排领域对象与端口，界定事务边界，发布领域事件。
//
// 依赖方向：app → domain（ports 与领域类型）+ infra（eventbus）。
// app 不得 import 任何 adapters（红线 R3 由 CI 检查）。
package app

import (
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/share"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// ConnLookup 只回答「此刻本机是否与该节点存在活动 TCP 会话」。
//
// PeerApp 用它把 TTL 兜底（Sweep）从 discovered 扩展到 online：online 由握手置位
// （ADR-009），一旦会话消失且 announce 也停了，它就该像 discovered 一样过期。
// 判会话是为了不误伤「连接仍在、只是 announce 恰好断档」的节点。
type ConnLookup interface {
	SessionOf(id identity.NodeID) (ports.Session, bool)
}

// PeerApp 维护节点目录：把发现层产出的 Announcement 归一化为 Peer 并落目录。
type PeerApp struct {
	self identity.NodeID
	dir  ports.PeerDirectory
	bus  ports.EventBus
	clk  ports.Clock
	ttl  time.Duration
	lg   *slog.Logger
	// conns 让 Sweep 判断「是否仍有活动会话」（可选，见 SetConnLookup）。
	conns ConnLookup
}

// SetConnLookup 注入连接层（可选）。未注入时 Sweep 把 online 一律按「无会话」处理。
func (a *PeerApp) SetConnLookup(c ConnLookup) { a.conns = c }

// NewPeerApp 构造节点目录应用。
func NewPeerApp(
	self identity.NodeID,
	dir ports.PeerDirectory,
	bus ports.EventBus,
	clk ports.Clock,
	ttl time.Duration,
	lg *slog.Logger,
) *PeerApp {
	if ttl <= 0 {
		ttl = 300 * time.Second
	}
	return &PeerApp{self: self, dir: dir, bus: bus, clk: clk, ttl: ttl, lg: lg}
}

// AddrOf 返回可拨号地址 "ip:tcp_port"。
func AddrOf(an peer.Announcement) string {
	if an.ObservedIP == "" {
		return ""
	}
	if an.TCPPort == 0 {
		return an.ObservedIP
	}
	return net.JoinHostPort(an.ObservedIP, strconv.Itoa(int(an.TCPPort)))
}

// OnAnnouncement 归一化处理来自任意发现策略（广播 / 种子 / 组播）的通告。
//
// 关键：广播与种子产出的结构完全同构，因此走【同一条】路径。
// LastSeen 记为本地到达时间，TTL 只与它比较（架构书 ADR-011）。
func (a *PeerApp) OnAnnouncement(an peer.Announcement) error {
	if an.NodeID.IsZero() || an.NodeID == a.self {
		return nil
	}
	now := a.clk.Now()

	// 本地已移除的节点再次被【有效签名通告】验证到 ⇒ 它没死，自动恢复（设计 D7）。
	//
	// 能走到这里就说明 UDP 层已经验签，并校验过 node_id == Fingerprint(pub)
	// （udp/broadcast.go 的 decodeAnnounce、udp/seed.go 的种子回包）。
	// 携带该 node_id 的第三方转述（目录交换）不具备这个性质 ——
	// HandlePeerListResp 那条路径必须继续抑制，不要在这里找对称性。
	//
	// 放在 BYE 分支之前：告别报文同样是签名有效的在世证明（它只是要走了），
	// 先解除标记，下面才会把「离线」正常告知 UI 而不是被闸门吞掉。
	if prev, ok := a.dir.Get(an.NodeID); ok && prev.State == peer.StateBlocked {
		if err := a.dir.SetSuppressed(an.NodeID, false, now); err != nil {
			return err
		}
		if a.lg != nil {
			a.lg.Info("peer un-suppressed by signed announce/bye", "peer_id", an.NodeID.String())
		}
	}

	// 告别报文（BYE）：对方正在正常退出，立即判离线，不等 TTL。
	// 这是「优雅退出」（秒级可见）与「崩溃/断电」（等 TTL）的区别所在。
	if an.Leaving {
		if err := a.dir.MarkOffline(an.NodeID, now); err != nil {
			return err
		}
		// 被本地移除的节点连「告别」也不该产生事件：MarkOffline 对 blocked 是空操作，
		// 但事件桥会把 peer:offline 转成 peer:updated，前端 peers store 对未知 nodeId
		// 会 push 回列表 —— 于是删掉的节点在它优雅退出时又闪现一次。
		//
		// 与下面非 BYE 分支同一口径；B3 落地后，签名有效的 BYE 会先解除标记再走到这里。
		if stored, ok := a.dir.Get(an.NodeID); ok && stored.State == peer.StateBlocked {
			return nil
		}
		if a.lg != nil {
			// 用 peer_id 而不是 node_id：logger 的上下文字段里已经有自己的 node_id 了
			a.lg.Info("peer left", "peer_id", an.NodeID.String())
		}
		if a.bus != nil {
			a.bus.Publish(eventbus.TopicPeerOffline, eventbus.PeerOffline{
				NodeID: an.NodeID.String(),
				Reason: "leave",
			})
		}
		return nil
	}

	// 重读一次：地址/名字的兜底要用当前字段。这里【不必】区分 blocked ——
	// 上面刚解除时它是 discovered，未解除时下面算出的状态同样是 discovered
	// （仓储的粘性会保证这次 Upsert 改不掉 blocked）。
	prev, exists := a.dir.Get(an.NodeID)

	// 「节点在线」由 announce 维护；「连接可用」由 tcp 层维护，两者互不覆盖。
	state := peer.StateDiscovered
	if exists && prev.State == peer.StateOnline {
		state = peer.StateOnline
	}

	source := an.Source
	if source == "" {
		source = peer.SourceBroadcast
	}

	// 从目录条目学到的条目可能没有可拨号地址，此时保留旧地址。
	addr := AddrOf(an)
	if addr == "" && exists {
		addr = prev.LastAddr
	}

	p := peer.Peer{
		NodeID:      an.NodeID,
		DisplayName: an.DisplayName,
		PublicKey:   an.PublicKey,
		LastAddr:    addr,
		// caps / proto_ver 由 HELLO 协商得出，announce 不携带，保留旧值。
		Caps:     prev.Caps,
		ProtoVer: prev.ProtoVer,
		LastSeen: now,
		Subnet:   an.Subnet,
		State:    state,
		Source:   source,
		// 共享摘要随通告刷新。刻意【不做】「空值不覆盖」：通告是周期性的
		// 全量状态，对方取消发布后摘要就该立刻变空 —— 沿用空值保留规则
		// 会让取消掉的共享在别人列表里挂到 TTL 过期。
		Shares: share.NormalizeSummaries(an.Shares),
		// 应用版本：旧版本客户端不带这个字段（空串），此时保留上一次学到的值。
		// 与 Shares 相反 —— 版本不会「取消」，空只表示「这条通告没说」。
		AppVersion: an.AppVersion,
	}
	if exists {
		if p.AppVersion == "" {
			p.AppVersion = prev.AppVersion
		}
		if p.DisplayName == "" {
			p.DisplayName = prev.DisplayName
		}
		if len(p.PublicKey) == 0 {
			p.PublicKey = prev.PublicKey
		}
		if p.Subnet == "" {
			p.Subnet = prev.Subnet
		}
	}

	if err := a.dir.Upsert(p); err != nil {
		return err
	}
	// 发布前以【落库后的状态】为准：仓储对 blocked 有粘性，上面的 p 可能是
	// discovered/online，而目录里仍是 blocked（该节点已被本地移除）。
	// 把 p 直接发出去会让 UI 把它推回列表、并让连接/传输层去拨号它。
	//
	// 注意本判断在「有效签名通告触发恢复」之后必然为假：那条路径会先
	// SetSuppressed(false) 再走到这里（设计 §5.3 ③），此时发布 discovered 是正确的。
	// 留着它是给任何仍能带着 blocked 行走到这里的路径兜底。
	if stored, ok := a.dir.Get(an.NodeID); ok && stored.State == peer.StateBlocked {
		return nil
	}
	if a.bus != nil {
		a.bus.Publish(eventbus.TopicPeerDiscovered, p)
	}
	return nil
}

// Start 订阅连接层事件，维护目录中的节点状态。
func (a *PeerApp) Start() {
	if a.bus == nil {
		return
	}

	a.bus.Subscribe(eventbus.TopicPeerOnline, func(payload any) {
		ev, ok := payload.(eventbus.PeerOnline)
		if !ok {
			return
		}
		id, err := identity.ParseNodeID(ev.NodeID)
		if err != nil {
			return
		}
		now := a.clk.Now()
		p, exists := a.dir.Get(id)
		if !exists {
			p = peer.Peer{NodeID: id}
		}
		// 三段 Ed25519 握手已通过 ⇒ 该身份确实在世，清除移除标记（设计 D7）。
		//
		// 必须显式解除：仓储 Upsert 对 blocked 是粘性的，不解除的话下面的
		// p.State = online 会被静默吃掉，节点永远不会回到列表。
		if exists && p.State == peer.StateBlocked {
			if err := a.dir.SetSuppressed(id, false, now); err != nil {
				// 与下面 Upsert 失败同样记 Warn：这条路径失败会让节点无声地保持「已移除」
				if a.lg != nil {
					a.lg.Warn("un-suppress failed", "err", err, "peer_id", id.String())
				}
				return
			}
		}
		p.State = peer.StateOnline
		p.LastSeen = now
		if ev.Addr != "" {
			p.LastAddr = ev.Addr
		}
		// caps / proto_ver 是 HELLO 协商结果，announce 不携带，只能从这条事件学。
		// 零值不覆盖：与 sqlite 仓储的粘性规则同一口径（「没协商出能力」不成信息，
		// 不该抹掉已知的能力位）。
		if ev.Caps != 0 {
			p.Caps = ev.Caps
		}
		if ev.ProtoVer != 0 {
			p.ProtoVer = ev.ProtoVer
		}
		if err := a.dir.Upsert(p); err != nil && a.lg != nil {
			a.lg.Warn("peer directory upsert failed", "err", err)
		}
		if a.lg != nil {
			a.lg.Info("peer: marked online from connection", "peer_id", id.String(), "addr", ev.Addr)
		}
	})

	a.bus.Subscribe(eventbus.TopicPeerOffline, func(payload any) {
		ev, ok := payload.(eventbus.PeerOffline)
		if !ok {
			return
		}
		id, err := identity.ParseNodeID(ev.NodeID)
		if err != nil {
			return
		}
		// 空闲回收只表示「连接不再可用」，不代表「节点离线」——
		// 它可能仍在广播 ANNOUNCE。UI 必须区分这两种状态。
		if ev.Reason == "idle_timeout" {
			if p, ok := a.dir.Get(id); ok && p.State == peer.StateOnline {
				p.State = peer.StateDiscovered
				p.LastSeen = a.clk.Now()
				_ = a.dir.Upsert(p)
			}
			return
		}
		if err := a.dir.MarkOffline(id, a.clk.Now()); err != nil && a.lg != nil {
			a.lg.Warn("mark offline failed", "err", err)
		}
	})
}

// Sweep 做过期兜底：
//   - discovered：announce 停发超过 TTL ⇒ 判离线（原有语义）；
//   - online：连接层已无该会话【且】announce 也停了超过 TTL ⇒ 判离线。
//
// online 由握手置位（ADR-009），一旦会话消失却没有收到离线事件（进程重启后残留、
// 或离线事件在对方不在线期间丢失），它就会永久卡在 online —— 现象正是
// 「对方明明不在线，列表却一直显示在线」。这一层兜底让 online 也能被 TTL 收回。
// 先判会话：有活动会话的节点即便 announce 恰好断档也不该被判离线。
func (a *PeerApp) Sweep() {
	now := a.clk.Now()
	for _, p := range a.dir.List(ports.PeerFilter{}) {
		switch p.State {
		case peer.StateDiscovered:
			// announce 维护的状态，照常按 TTL 过期。
		case peer.StateOnline:
			// 有活动会话 ⇒ 它确实连着，只是这次没等到 announce，不判离线。
			if a.conns != nil {
				if _, ok := a.conns.SessionOf(p.NodeID); ok {
					continue
				}
			}
		default:
			continue
		}
		if !p.Expired(now, a.ttl) {
			continue
		}
		_ = a.dir.MarkOffline(p.NodeID, now)
		if a.bus != nil {
			a.bus.Publish(eventbus.TopicPeerOffline, eventbus.PeerOffline{
				NodeID: p.NodeID.String(),
				Reason: "ttl_expired",
			})
		}
	}
}

// List 返回节点目录（供 UI 与目录交换使用）。
func (a *PeerApp) List(filter ports.PeerFilter) []peer.Peer { return a.dir.List(filter) }

// Get 读取单个节点。
func (a *PeerApp) Get(id identity.NodeID) (peer.Peer, bool) { return a.dir.Get(id) }
