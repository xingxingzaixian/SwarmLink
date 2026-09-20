package wails

import (
	"sync"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/group"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// Emitter 是「向前端推送事件」的最小抽象。
//
// 抽象存在的理由：Wails 的具体 API 在装配文件里适配，
// 本包因此可以在没有 Wails 工具链的环境下编译与单测。
type Emitter interface {
	Emit(name string, data any)
}

// 前端事件名。
const (
	EventPeerUpdated      = "peer:updated"
	EventChatNewMessage   = "chat:new-message"
	EventChatDelivered    = "chat:delivered"
	EventGroupUpdated     = "group:updated"
	EventTransferProgress = "transfer:progress"
	EventTransferDone     = "transfer:done"
	EventTransferError    = "transfer:error"
	EventNetError         = "net:error"
	EventConfigChanged    = "config:changed"
	// EventDebug 把原始总线事件整体送到前端「调试面板」。
	//
	// P2P 排障极难，这个面板的投入产出比极高（架构书 3.5）。
	EventDebug = "debug:event"
)

// ProgressThrottle 是进度事件的最小间隔。
//
// 为什么要节流：1 GB 文件 = 2048 个块，若不节流会在千兆网下 0.2 秒内
// 把 2048 个事件全部推向 WebView，直接打满渲染主线程（P1-10）。
const ProgressThrottle = 250 * time.Millisecond // 4 Hz

// Bridge 把领域事件映射为前端事件。
type Bridge struct {
	bus     ports.EventBus
	clk     ports.Clock
	emitter Emitter

	mu       sync.Mutex
	lastEmit map[string]time.Time

	unsubs []func()
}

// NewBridge 创建事件桥。
func NewBridge(bus ports.EventBus, clk ports.Clock, emitter Emitter) *Bridge {
	return &Bridge{
		bus:      bus,
		clk:      clk,
		emitter:  emitter,
		lastEmit: make(map[string]time.Time),
	}
}

// Start 订阅全部领域主题。
func (b *Bridge) Start() {
	if b.bus == nil || b.emitter == nil {
		return
	}

	// 节点事件只推「变化了的那一条」的精简信息；前端据此局部更新，
	// 需要全量列表时再调 PeerService.List() —— 避免把领域类型泄漏给前端。
	b.on(eventbus.TopicPeerDiscovered, func(p any) {
		pp, ok := p.(peer.Peer)
		if !ok {
			return
		}
		b.debug(eventbus.TopicPeerDiscovered, p)
		b.emitter.Emit(EventPeerUpdated, peerUpdate(pp.NodeID.String(), pp.DisplayName, string(pp.State), pp.LastAddr))
	})

	b.on(eventbus.TopicPeerOnline, func(p any) {
		ev, ok := p.(eventbus.PeerOnline)
		if !ok {
			return
		}
		b.debug(eventbus.TopicPeerOnline, p)
		b.emitter.Emit(EventPeerUpdated, peerUpdate(ev.NodeID, "", string(peer.StateOnline), ev.Addr))
	})

	b.on(eventbus.TopicPeerOffline, func(p any) {
		ev, ok := p.(eventbus.PeerOffline)
		if !ok {
			return
		}
		b.debug(eventbus.TopicPeerOffline, p)
		state := peer.StateOffline
		if ev.Reason == "idle_timeout" {
			// 连接不再可用 ≠ 节点离线：仍会广播 announce。
			state = peer.StateDiscovered
		}
		update := peerUpdate(ev.NodeID, "", string(state), "")
		update["reason"] = ev.Reason
		b.emitter.Emit(EventPeerUpdated, update)
	})

	b.on(eventbus.TopicChatReceived, func(p any) {
		m, ok := p.(message.Message)
		if !ok {
			return
		}
		b.debug(eventbus.TopicChatReceived, p)
		b.emitter.Emit(EventChatNewMessage, ToMessageDTO(m))
	})

	b.on(eventbus.TopicChatDelivered, func(p any) {
		ack, ok := p.(protocol.ChatAck)
		if !ok {
			return
		}
		b.debug(eventbus.TopicChatDelivered, p)
		b.emitter.Emit(EventChatDelivered, map[string]any{"msgId": ack.MsgID})
	})

	b.on(eventbus.TopicGroupUpdated, func(p any) {
		g, ok := p.(group.Group)
		if !ok {
			return
		}
		b.debug(eventbus.TopicGroupUpdated, p)
		b.emitter.Emit(EventGroupUpdated, map[string]any{
			"groupId": g.ID,
			"name":    g.Name,
			"epoch":   g.Epoch,
		})
	})

	// 进度事件按 job 节流：这是唯一需要节流的事件（P1-10）。
	b.on(eventbus.TopicTransferProgress, func(p any) {
		ev, ok := p.(eventbus.TransferProgress)
		if !ok {
			return
		}
		if !b.allow(ev.JobID, ProgressThrottle) {
			return
		}
		b.emitter.Emit(EventTransferProgress, map[string]any{
			"jobId":   ev.JobID,
			"peerId":  ev.PeerID,
			"percent": ev.Percent,
			"speed":   ev.Speed,
			"status":  ev.Status,
		})
	})

	b.on(eventbus.TopicTransferDone, func(p any) {
		ev, ok := p.(eventbus.TransferDone)
		if !ok {
			return
		}
		b.debug(eventbus.TopicTransferDone, p)
		b.forget(ev.JobID)
		b.emitter.Emit(EventTransferDone, map[string]any{
			"jobId":  ev.JobID,
			"peerId": ev.PeerID,
			"path":   ev.Path,
		})
	})

	b.on(eventbus.TopicTransferError, func(p any) {
		ev, ok := p.(eventbus.TransferError)
		if !ok {
			return
		}
		b.debug(eventbus.TopicTransferError, p)
		b.forget(ev.JobID)
		msg := ""
		if ev.Err != nil {
			msg = ev.Err.Error()
		}
		b.emitter.Emit(EventTransferError, map[string]any{
			"jobId": ev.JobID,
			"error": msg,
		})
	})

	b.on(eventbus.TopicNetError, func(p any) {
		ev, ok := p.(eventbus.NetError)
		if !ok {
			return
		}
		msg := ""
		if ev.Err != nil {
			msg = ev.Err.Error()
		}
		b.emitter.Emit(EventNetError, map[string]any{
			"op":     ev.Op,
			"peerId": ev.PeerID,
			"error":  msg,
		})
	})
}

// Stop 退订全部主题。
func (b *Bridge) Stop() {
	for _, un := range b.unsubs {
		un()
	}
	b.unsubs = nil
}

func (b *Bridge) on(topic string, fn func(any)) {
	b.unsubs = append(b.unsubs, b.bus.Subscribe(topic, fn))
}

// debug 把原始事件送进前端调试面板。
func (b *Bridge) debug(topic string, payload any) {
	b.emitter.Emit(EventDebug, map[string]any{
		"topic": topic,
		"at":    b.clk.Now().UnixMilli(),
	})
}

// allow 判断是否允许在节流窗口内发送（按 key 独立计时）。
func (b *Bridge) allow(key string, window time.Duration) bool {
	now := b.clk.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if last, ok := b.lastEmit[key]; ok && now.Sub(last) < window {
		return false
	}
	b.lastEmit[key] = now
	return true
}

func (b *Bridge) forget(key string) {
	b.mu.Lock()
	delete(b.lastEmit, key)
	b.mu.Unlock()
}

// peerUpdate 构造前端侧的节点变更载荷。
func peerUpdate(nodeID, displayName, state, addr string) map[string]any {
	short := nodeID
	if len(short) > 8 {
		short = short[:8]
	}
	return map[string]any{
		"nodeId":      nodeID,
		"shortId":     short,
		"displayName": displayName,
		"state":       state,
		"lastAddr":    addr,
	}
}
