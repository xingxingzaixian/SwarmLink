package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// ChatApp 编排单聊：at-least-once 投递 + 接收端幂等去重（ADR-004）。
type ChatApp struct {
	self  identity.NodeID
	store ports.ChatStore
	conns ports.ConnManager
	dir   ports.PeerDirectory
	bus   ports.EventBus
	clk   ports.Clock
	lg    *slog.Logger

	// BaseBackoff / MaxBackoff 控制 outbox 重发退避（1s → 2s → … → 5min）。
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

// NewChatApp 构造单聊应用。
func NewChatApp(
	self identity.NodeID,
	store ports.ChatStore,
	conns ports.ConnManager,
	dir ports.PeerDirectory,
	bus ports.EventBus,
	clk ports.Clock,
	lg *slog.Logger,
) *ChatApp {
	return &ChatApp{
		self:        self,
		store:       store,
		conns:       conns,
		dir:         dir,
		bus:         bus,
		clk:         clk,
		lg:          lg,
		BaseBackoff: time.Second,
		MaxBackoff:  5 * time.Minute,
	}
}

// SendMessage 持久化并尽力发送一条单聊文本消息。
func (a *ChatApp) SendMessage(ctx context.Context, peerID identity.NodeID, text string) (message.Message, error) {
	return a.send(ctx, peerID, text, message.MsgTypeText)
}

// SendImage 发送单聊图片消息。
//
// content 是 message.EncodeInlineImage 产出的 JSON 信封（不是裸 base64）：
// 图片在发送前已被压到 300KB 量级，因此直接内联进消息体，
// 走的是与文本完全相同的「入库 → trySend → outbox 重发 → ACK」链路。
func (a *ChatApp) SendImage(ctx context.Context, peerID identity.NodeID, content string) (message.Message, error) {
	return a.send(ctx, peerID, content, message.MsgTypeImage)
}

// SendFile 发送单聊文件消息。
//
// content 只带 job_id 与文件名：这条消息不携带文件本身，只是让两端的
// 聊天框都出现一张文件卡片（实际传输由 TransferApp 的独立通道完成）。
//
// 用 EncodeJSON 而不是手写 fmt.Sprintf：文件名里的一个英文引号就能把
// 拼出来的 JSON 变成坏数据，而接收方解析失败只表现为「文件名不见了」。
func (a *ChatApp) SendFile(ctx context.Context, peerID identity.NodeID, jobID string, fileName string) (message.Message, error) {
	payload, err := protocol.EncodeJSON(message.FileMessageContent{
		JobID:    jobID,
		FileName: fileName,
	})
	if err != nil {
		return message.Message{}, fmt.Errorf("chat: encode file content: %w", err)
	}
	return a.send(ctx, peerID, string(payload), message.MsgTypeFile)
}

// SendFolder 发送单聊文件夹消息。
//
// 与 SendFile 同一套路：content 只是让两端聊天框出现一张聚合卡片，
// 真正的传输由 TransferApp 按 batch_id 自己跑。
//
// 一次文件夹只发【一条】消息：几百个文件发几百条消息会把聊天记录刷屏，
// 也会让 outbox 里排上几百条待确认记录。
func (a *ChatApp) SendFolder(
	ctx context.Context,
	peerID identity.NodeID,
	batchID, folderName string,
	fileCount int,
	totalSize int64,
	skipped int,
) (message.Message, error) {
	payload, err := protocol.EncodeJSON(message.FolderMessageContent{
		BatchID:    batchID,
		FolderName: folderName,
		FileCount:  fileCount,
		TotalSize:  totalSize,
		Skipped:    skipped,
	})
	if err != nil {
		return message.Message{}, fmt.Errorf("chat: encode folder content: %w", err)
	}
	return a.send(ctx, peerID, string(payload), message.MsgTypeFolder)
}

// send 是文本与图片共用的发送实现。
//
// 「入库成功就算发送完成」：网络连接建立与实际发送在后台异步进行，
// 失败时由 outbox 重发机制兜底。这样即使对方不在线，消息也能立即显示。
func (a *ChatApp) send(ctx context.Context, peerID identity.NodeID, content string, msgType message.MsgType) (message.Message, error) {
	now := a.clk.Now()
	msg := message.Message{
		MsgID:     message.NewID(),
		ConvID:    message.DirectConvID(a.self, peerID),
		SenderID:  a.self,
		Direction: message.DirectionOut,
		Content:   content,
		MsgType:   msgType,
		SentAt:    now,
		State:     message.StatePending,
	}
	if err := a.store.AppendOutgoing(msg, now); err != nil {
		return msg, fmt.Errorf("chat: persist outgoing: %w", err)
	}
	// 异步尝试发送：即使连接失败也不阻塞响应，消息会进入 outbox 重发
	go func() {
		if err := a.trySend(context.Background(), peerID, msg); err != nil && a.lg != nil {
			a.lg.Info("chat: initial send deferred to outbox", "msg_id", msg.MsgID, "err", err)
		}
	}()
	return msg, nil
}

// ClearConversation 记录会话的清空点：早于它的消息不再出现在聊天区。
//
// 【不删数据】：消息仍在库里，RestoreConversation 之后原样回来。
// 时间取本机时钟 —— 清空点是本机视图偏好，不需要与其他节点对齐。
func (a *ChatApp) ClearConversation(ctx context.Context, convID string) error {
	if convID == "" {
		return fmt.Errorf("chat: conv id 不能为空")
	}
	return a.store.SetConvCleared(convID, a.clk.Now())
}

// RestoreConversation 撤销清空点（幂等：没清空过也返回 nil）。
func (a *ChatApp) RestoreConversation(ctx context.Context, convID string) error {
	if convID == "" {
		return fmt.Errorf("chat: conv id 不能为空")
	}
	return a.store.RemoveConvClear(convID)
}

// ClearedAt 返回会话清空点的毫秒时间戳；0 表示未清空。
//
// 读失败一律按「未清空」返回 0：宁可多显示消息，也不要让用户的
// 聊天记录因为一次读错误而凭空消失。
func (a *ChatApp) ClearedAt(ctx context.Context, convID string) (int64, error) {
	if convID == "" {
		return 0, nil
	}
	at, ok, err := a.store.ConvClearedAt(convID)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	return at.UnixMilli(), nil
}

// Message 按 msg_id 取一条消息（「另存为」等按 id 回查的入口用）。
func (a *ChatApp) Message(msgID string) (message.Message, bool) {
	if a.store == nil {
		return message.Message{}, false
	}
	return a.store.Get(msgID)
}

// History 返回会话历史（新→旧）。
func (a *ChatApp) History(convID string, limit int, before int64) ([]message.Message, error) {
	return a.store.Latest(convID, limit, before)
}

// HandleChat 处理入站消息：幂等入库 + 无条件回 ACK。
func (a *ChatApp) HandleChat(sess ports.Session, f protocol.Frame) error {
	var c protocol.Chat
	if err := protocol.DecodeJSON(f.Payload, &c); err != nil {
		return err
	}
	if c.MsgID == "" {
		return nil
	}

	senderID := sess.PeerID()
	if c.SenderID != "" {
		if id, err := identity.ParseNodeID(c.SenderID); err == nil {
			senderID = id
		}
	}
	// 单聊 conv_id 必须由双方【对称】算出：
	// 既不能直接用对端发来的 c.ConvID（可能是它自己视角的 ID），
	// 也不能用对端 NodeID 本身 —— 否则两端会各存半条会话。
	convID := message.DirectConvID(a.self, senderID)
	sentAt := a.clk.Now()
	if c.SentAt > 0 {
		sentAt = time.UnixMilli(c.SentAt)
	}
	msgType := message.MsgType(c.MsgType)
	if msgType == "" {
		msgType = message.MsgTypeText
	}

	content := c.Content
	if msgType == message.MsgTypeImage {
		sanitized, ok := sanitizeImageContent(content)
		if !ok {
			// 坏消息的处置：不落库、不通知 UI，但【照常回 ACK】。
			// 不回 ACK 会让发送方按 outbox 策略无限重发同一条垃圾消息。
			return a.sendAck(sess, c.MsgID)
		}
		content = sanitized
	}
	// 文件消息的 content 是 JSON，不需要特殊处理，直接存储即可

	m := message.Message{
		MsgID:     c.MsgID,
		ConvID:    convID,
		SenderID:  senderID,
		Direction: message.DirectionIn,
		Content:   content,
		MsgType:   msgType,
		FileID:    c.FileID,
		SentAt:    sentAt,
		RecvAt:    a.clk.Now(),
		State:     message.StateDelivered,
	}

	inserted, err := a.store.AppendIfAbsent(m)
	if err != nil {
		return fmt.Errorf("chat: persist inbound: %w", err)
	}
	// 只有「首次插入」才通知 UI；重复消息不重复展示。
	if inserted && a.bus != nil {
		a.bus.Publish(eventbus.TopicChatReceived, m)
	}

	// 关键：重复消息【也必须回 ACK】，否则发送方永远收不到确认、无限重发。
	return a.sendAck(sess, c.MsgID)
}

// maxImageEdge 是接收侧允许声明的最大边长。
//
// 尺寸字段来自网络，只用于前端占位；钳制它的意义是防止伪造的
// w/h 让接收端按天文数字撑开占位框。真实的图片尺寸由浏览器的解码结果决定。
const maxImageEdge = 20000

// sanitizeImageContent 校验并规范化入站的内联图片 content。
//
// 校验通过且尺寸合规时原样返回，避免对正常消息做无意义的重新编码。
func sanitizeImageContent(content string) (string, bool) {
	img, err := message.DecodeInlineImage(content)
	if err != nil {
		return "", false
	}
	if _, err := img.Bytes(); err != nil { // base64 合法性 + 体积上限
		return "", false
	}
	if img.W < 0 || img.H < 0 || img.W > maxImageEdge || img.H > maxImageEdge {
		img.W = clampEdge(img.W)
		img.H = clampEdge(img.H)
		out, err := message.EncodeInlineImage(img)
		if err != nil {
			return "", false
		}
		return out, true
	}
	return content, true
}

func clampEdge(v int) int {
	if v < 0 {
		return 0
	}
	if v > maxImageEdge {
		return maxImageEdge
	}
	return v
}

func (a *ChatApp) sendAck(sess ports.Session, msgID string) error {
	payload, err := protocol.EncodeJSON(protocol.ChatAck{MsgID: msgID})
	if err != nil {
		return err
	}
	return sess.Send(protocol.New(protocol.TypeChatAck, payload))
}

// HandleChatAck 处理送达确认。
func (a *ChatApp) HandleChatAck(_ ports.Session, f protocol.Frame) error {
	var ack protocol.ChatAck
	if err := protocol.DecodeJSON(f.Payload, &ack); err != nil {
		return err
	}
	if ack.MsgID == "" {
		return nil
	}
	if err := a.store.Deliver(ack.MsgID); err != nil {
		return err
	}
	if a.bus != nil {
		a.bus.Publish(eventbus.TopicChatDelivered, ack)
	}
	return nil
}

// SweepOutbox 扫描未确认消息并重发（指数退避）。
func (a *ChatApp) SweepOutbox(ctx context.Context) {
	now := a.clk.Now()
	due, err := a.store.Due(now, 64)
	if err != nil {
		return
	}
	for _, e := range due {
		msg, ok := a.store.Get(e.MsgID)
		if !ok {
			_ = a.store.Delete(e.MsgID)
			continue
		}
		peerID, ok := message.DirectPeer(msg.ConvID, a.self)
		if !ok {
			// 群消息的 conv_id 是 group_id，由 GroupApp 负责重发。
			continue
		}
		if err := a.trySend(ctx, peerID, msg); err != nil {
			_ = a.store.BumpAttempt(e.MsgID, now.Add(backoff(e.Attempts+1, a.BaseBackoff, a.MaxBackoff)))
			continue
		}
		// 已发出，等待 ACK；把下次重试推远，避免重复发送。
		_ = a.store.BumpAttempt(e.MsgID, now.Add(a.MaxBackoff))
	}
}

// FlushTo 对端上线后立即冲刷其所有待确认消息。
func (a *ChatApp) FlushTo(ctx context.Context, peerID identity.NodeID) {
	if a.lg != nil {
		a.lg.Info("chat: flushing outbox to peer", "peer_id", peerID.String())
	}
	entries, err := a.store.ListByConv(message.DirectConvID(a.self, peerID))
	if err != nil {
		if a.lg != nil {
			a.lg.Warn("chat: list by conv failed", "conv_id", message.DirectConvID(a.self, peerID), "err", err)
		}
		return
	}
	if a.lg != nil {
		a.lg.Info("chat: found pending messages", "peer_id", peerID.String(), "count", len(entries))
	}
	for _, e := range entries {
		msg, ok := a.store.Get(e.MsgID)
		if !ok {
			continue
		}
		if err := a.trySend(ctx, peerID, msg); err != nil && a.lg != nil {
			a.lg.Debug("chat: flush deferred", "msg_id", msg.MsgID, "err", err)
		}
	}
}

// MaybeConnectAndFlush 检查是否有待发送消息，如果有则主动建立连接并发送
func (a *ChatApp) MaybeConnectAndFlush(ctx context.Context, peerID identity.NodeID) {
	entries, err := a.store.ListByConv(message.DirectConvID(a.self, peerID))
	if err != nil {
		if a.lg != nil {
			a.lg.Warn("chat: list by conv failed", "conv_id", message.DirectConvID(a.self, peerID), "err", err)
		}
		return
	}
	// 没有待发送消息，不需要主动连接
	if len(entries) == 0 {
		if a.lg != nil {
			a.lg.Debug("chat: no pending messages, skip connect", "peer_id", peerID.String())
		}
		return
	}
	if a.lg != nil {
		a.lg.Info("chat: found pending messages, establishing connection", "peer_id", peerID.String(), "count", len(entries))
	}
	// 主动建立连接（这会触发 FlushTo）
	go a.FlushTo(ctx, peerID)
}

// Start 订阅 peer.online 以触发 outbox 冲刷。
func (a *ChatApp) Start(ctx context.Context) {
	if a.bus == nil {
		if a.lg != nil {
			a.lg.Warn("chat: event bus is nil, cannot subscribe to peer online")
		}
		return
	}
	if a.lg != nil {
		a.lg.Info("chat: starting, subscribing to peer online events")
	}

	// 订阅节点上线事件（TCP 连接建立时）
	a.bus.Subscribe(eventbus.TopicPeerOnline, func(payload any) {
		ev, ok := payload.(eventbus.PeerOnline)
		if !ok {
			if a.lg != nil {
				a.lg.Warn("chat: peer online event type mismatch", "type", fmt.Sprintf("%T", payload))
			}
			return
		}
		id, err := identity.ParseNodeID(ev.NodeID)
		if err != nil {
			if a.lg != nil {
				a.lg.Warn("chat: parse node id failed", "node_id", ev.NodeID, "err", err)
			}
			return
		}
		if a.lg != nil {
			a.lg.Info("chat: peer online (TCP connected), flushing outbox", "peer_id", id.String())
		}
		go a.FlushTo(ctx, id)
	})

	// 订阅节点发现事件（UDP 广播收到通告时）
	// 用于处理：对方启动后即使没有建立TCP连接，也要主动连接并发送离线消息
	a.bus.Subscribe(eventbus.TopicPeerDiscovered, func(payload any) {
		p, ok := payload.(peer.Peer)
		if !ok {
			if a.lg != nil {
				a.lg.Warn("chat: peer discovered event type mismatch", "type", fmt.Sprintf("%T", payload))
			}
			return
		}
		// 只有对方重新上线（之前是离线状态）才主动连接
		if p.State != peer.StateOnline && p.State != peer.StateDiscovered {
			return
		}
		if a.lg != nil {
			a.lg.Info("chat: peer discovered (UDP announce), checking for pending messages", "peer_id", p.NodeID.String())
		}
		go a.MaybeConnectAndFlush(ctx, p.NodeID)
	})
	if a.lg != nil {
		a.lg.Info("chat: subscribed to peer online and discovered events")
	}
}

// trySend 把一条消息发给对端。
//
// 文件消息【不再】有特殊分支：它和文本、图片走同一条投递路径
// （outbox 重试 → ACK → 出队）。曾经这里把 file 消息转给本机的
// TransferApp.HandleFileMessage「先检查文件还在不在」，结果是：
//   - 消息从不经过 sess.Send，对端的聊天框永远看不到文件卡片；
//   - 它永远拿不到 ACK，outbox 于是一直重试，每次重试都再产生一条
//     假的「文件已被删除或移动」消息，源源不断发给对端。
//
// 文件是否还在，由 TransferApp 在真正传输时判断（失败会发 transfer:error
// 并在传输面板标红），不该由聊天消息来前置检查。
func (a *ChatApp) trySend(ctx context.Context, peerID identity.NodeID, msg message.Message) error {
	sess, err := a.session(ctx, peerID)
	if err != nil {
		return err
	}
	payload, err := protocol.EncodeJSON(protocol.Chat{
		MsgID:    msg.MsgID,
		ConvID:   msg.ConvID,
		SenderID: msg.SenderID.String(),
		Content:  msg.Content,
		MsgType:  string(msg.MsgType),
		FileID:   msg.FileID,
		SentAt:   msg.SentAt.UnixMilli(),
	})
	if err != nil {
		return err
	}
	return sess.Send(protocol.New(protocol.TypeChat, payload))
}

// session 复用既有会话，否则按目录地址按需拨号（ADR-009）。
func (a *ChatApp) session(ctx context.Context, peerID identity.NodeID) (ports.Session, error) {
	if s, ok := a.conns.SessionOf(peerID); ok {
		return s, nil
	}
	p, ok := a.dir.Get(peerID)
	if !ok || p.LastAddr == "" {
		return nil, fmt.Errorf("chat: no known address for peer %s", peerID)
	}
	return a.conns.Dial(ctx, peerID, p.LastAddr)
}

// backoff 计算第 attempt 次重试的等待时间（1s,2s,4s…上限 max）。
func backoff(attempt int, base, max time.Duration) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if max <= 0 {
		max = 5 * time.Minute
	}
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 20 {
		attempt = 20
	}
	d := base * time.Duration(int64(1)<<uint(attempt-1))
	if d <= 0 || d > max {
		d = max
	}
	return d
}
