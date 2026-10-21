package wails

import (
	"encoding/base64"
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
	// EventTransferState 是任务状态变化（载荷：jobId / status / reason）。
	//
	// 与 progress 分开的原因：progress 的语义是「进度在动」，而"进入等待
	// 对方上线"恰恰是停下来了；用进度事件表达状态变化迟早会撞上
	// 「已结束的任务不再接受进度更新」那条前端护栏。
	EventTransferState = "transfer:state"
	EventNetError      = "net:error"
	EventConfigChanged = "config:changed"
	// EventShareUpdated 是【本机】共享清单变化（发布 / 取消 / 刷新索引）。
	// 载荷：{ shareId: string, action: "published"|"unpublished"|"refreshed" }。
	//
	// 他人共享的变化不走这里：那是目录条目的变化，已经由 peer:updated 承担 ——
	// 再发一次只会让界面在同一件事上刷新两遍。
	EventShareUpdated = "share:updated"
	// EventDebug 把原始总线事件整体送到前端「调试面板」。
	//
	// P2P 排障极难，这个面板的投入产出比极高（架构书 3.5）。
	EventDebug = "debug:event"
	// EventFilesDropped 转发「拖入窗口的文件路径」。
	//
	// 它不是领域事件，因此不经过 EventBus —— WebView 里 drop 出来的 File 对象
	// 没有本地路径，路径只有窗口层（Go）拿得到，必须由外壳转发。
	// 载荷：{ paths: []string, target: string }，target 是命中的
	// data-file-drop-target 属性值，由前端决定「这次投放算发给谁」。
	EventFilesDropped = "file:dropped"
	// EventSecondInstanceLaunched 表示有第二个实例尝试启动（单实例模式下）。
	//
	// 载荷：{ args: []string, workingDir: string }
	EventSecondInstanceLaunched = "second-instance:launched"
	// EventSystemNotifyActivated 表示用户点击了一条系统通知。
	//
	// 与 EventFilesDropped 同类：它不是领域事件（点击发生在操作系统侧，
	// 已经离开本进程的边界），因此同样由外壳直接发往前端，不过事件总线。
	// 窗口已经由外壳叫到前台，这里只负责让前端切到对应会话。
	// 载荷：{ convId: string }，取不到会话时为空串。
	EventSystemNotifyActivated = "notify:activated"
	// EventScreenshotReady 表示「覆盖层里选好并标注完的截图」已经就绪。
	//
	// 它由覆盖层窗口所在的会话发出，而【要送到的是主窗口】——
	// 主窗口据此把图放进聊天输入框上方的草稿行。
	// 载荷：ScreenshotDTO。
	EventScreenshotReady = "screenshot:ready"
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
		// 刻意【不带 rttMs】：通告这条路径不知道延迟，而它每几秒就推一次；
		// 若跟着带一个 0，就会把已知的真实延迟反复抹成「无数据」，
		// 信号卡会在真值与测量中之间来回跳。
		b.emitter.Emit(EventPeerUpdated, peerUpdate(pp.NodeID.String(), pp.DisplayName, string(pp.State), pp.LastAddr))
	})

	b.on(eventbus.TopicPeerOnline, func(p any) {
		ev, ok := p.(eventbus.PeerOnline)
		if !ok {
			return
		}
		b.debug(eventbus.TopicPeerOnline, p)
		update := peerUpdate(ev.NodeID, "", string(peer.StateOnline), ev.Addr)
		// 握手成功这一刻就把 RTT 带上（PeerOnline 载荷里本来就有，之前被丢掉了），
		// 前端不必等下一次 List() 才画得出连接质量。
		// 刚建连时它是 0 —— 心跳还没跑完一轮，界面按「测量中」显示。
		update["rttMs"] = int64(ev.RTT / time.Millisecond)
		b.emitter.Emit(EventPeerUpdated, update)
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
		// 会话没了 ⇒ 延迟也就没有数据了，必须显式清零。
		// 否则信号卡会一直挂着上一个会话的旧数字，看起来像「还连着」。
		update["rttMs"] = int64(0)
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
			"state":   string(g.State.StateOrActive()),
			// digest 让前端能判断「这条事件是否真的改变了名册」：
			// 摘要一致就跳过重新拉取，避免重复投递引发一轮刷新风暴。
			"digest": base64.StdEncoding.EncodeToString(g.RosterDigest()),
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
		// 只发「易变」的进度：文件名/大小等元数据由 TransferService.List() 提供。
		// 高频事件里塞不变的字段，会让 4 Hz 的通道背负无谓的序列化成本。
		payload := map[string]any{
			"jobId":   ev.JobID,
			"peerId":  ev.PeerID,
			"percent": ev.Percent,
			"speed":   ev.Speed,              // 字节/秒
			"eta":     ev.ETA.Milliseconds(), // 由前端格式化成「剩余 1 分 20 秒」
			"status":  ev.Status,
		}
		// 唯一的例外，且只在真的续传过时出现：续传起点定在 FILE_META_ACK 之后，
		// 那一刻没有别的事件可用，而它又不是「重读整行」能安全拿到的（发送侧的
		// 进度不落库，重读会把界面上的百分比盖回去）。理由详见
		// eventbus.TransferProgress.ResumeFromPercent。
		//
		// 不续传的任务（绝大多数帧）一个字节都不多花。
		if ev.ResumeFromPercent > 0 {
			payload["resumeFromPercent"] = ev.ResumeFromPercent
		}
		b.emitter.Emit(EventTransferProgress, payload)
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

	b.on(eventbus.TopicTransferState, func(p any) {
		ev, ok := p.(eventbus.TransferState)
		if !ok {
			return
		}
		b.debug(eventbus.TopicTransferState, p)
		b.emitter.Emit(EventTransferState, map[string]any{
			"jobId":  ev.JobID,
			"status": ev.To,
			"reason": ev.Reason,
		})
	})

	b.on(eventbus.TopicShareUpdated, func(p any) {
		ev, ok := p.(eventbus.ShareUpdated)
		if !ok {
			return
		}
		b.debug(eventbus.TopicShareUpdated, p)
		b.emitter.Emit(EventShareUpdated, map[string]any{
			"shareId": ev.ShareID,
			"action":  ev.Action,
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
