package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
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

// SendMessage 持久化并尽力发送一条单聊消息。
//
// 「入库成功才算发送」这个顺序由本方法显式编排 —— 事件总线不承载需要事务的路径。
func (a *ChatApp) SendMessage(ctx context.Context, peerID identity.NodeID, text string) (message.Message, error) {
	now := a.clk.Now()
	msg := message.Message{
		MsgID:     message.NewID(),
		ConvID:    message.DirectConvID(a.self, peerID),
		SenderID:  a.self,
		Direction: message.DirectionOut,
		Content:   text,
		MsgType:   message.MsgTypeText,
		SentAt:    now,
		State:     message.StatePending,
	}
	if err := a.store.AppendOutgoing(msg, now); err != nil {
		return msg, fmt.Errorf("chat: persist outgoing: %w", err)
	}
	if err := a.trySend(ctx, peerID, msg); err != nil && a.lg != nil {
		a.lg.Info("chat: initial send deferred to outbox", "msg_id", msg.MsgID, "err", err)
	}
	return msg, nil
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

	m := message.Message{
		MsgID:     c.MsgID,
		ConvID:    convID,
		SenderID:  senderID,
		Direction: message.DirectionIn,
		Content:   c.Content,
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
	entries, err := a.store.ListByConv(message.DirectConvID(a.self, peerID))
	if err != nil {
		return
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

// Start 订阅 peer.online 以触发 outbox 冲刷。
func (a *ChatApp) Start(ctx context.Context) {
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
		go a.FlushTo(ctx, id)
	})
}

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
