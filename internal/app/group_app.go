package app

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/group"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// GroupApp 编排群聊（ADR-003：全互联单播扇出 + 群状态 epoch 签名）。
//
// v1.0 上限 20 人，不做离线消息暂存转发 —— 那等价于引入弱中心，
// 与「无中心」直接冲突。UI 必须诚实标注「N 人未送达」。
type GroupApp struct {
	self   identity.NodeID
	kp     *identity.KeyPair
	groups ports.GroupRepo
	store  ports.ChatStore
	conns  ports.ConnManager
	dir    ports.PeerDirectory
	bus    ports.EventBus
	clk    ports.Clock
	lg     *slog.Logger

	// MaxDialConcurrency 是扇出时的并发拨号上限，避免瞬间 SYN 密集。
	MaxDialConcurrency int
	MaxBackoff         time.Duration
}

// NewGroupApp 构造群聊应用。
func NewGroupApp(
	self identity.NodeID,
	kp *identity.KeyPair,
	groups ports.GroupRepo,
	store ports.ChatStore,
	conns ports.ConnManager,
	dir ports.PeerDirectory,
	bus ports.EventBus,
	clk ports.Clock,
	lg *slog.Logger,
) *GroupApp {
	return &GroupApp{
		self: self, kp: kp, groups: groups, store: store, conns: conns,
		dir: dir, bus: bus, clk: clk, lg: lg,
		MaxDialConcurrency: 8,
		MaxBackoff:         5 * time.Minute,
	}
}

// CreateGroup 新建群（本方为 owner），签名后落库并广播群元数据。
func (a *GroupApp) CreateGroup(ctx context.Context, name string, memberIDs []identity.NodeID) (group.Group, error) {
	now := a.clk.Now()
	g := group.Group{
		ID:        group.NewID(a.self, now, name),
		Name:      name,
		OwnerID:   a.self,
		Epoch:     1,
		CreatedAt: now,
	}
	_ = g.AddMember(group.Member{
		NodeID: a.self, DisplayName: "me", Role: group.RoleOwner,
		JoinedAt: now, State: group.MemberActive,
	})
	for _, id := range memberIDs {
		if id == a.self || id.IsZero() {
			continue
		}
		display := ""
		if p, ok := a.dir.Get(id); ok {
			display = p.DisplayName
		}
		if err := g.AddMember(group.Member{
			NodeID: id, DisplayName: display, Role: group.RoleMember,
			JoinedAt: now, State: group.MemberActive,
		}); err != nil {
			return g, err
		}
	}
	g.StateSig = a.kp.Sign(g.SigningBytes())

	if err := a.groups.Upsert(g); err != nil {
		return g, err
	}
	if a.bus != nil {
		a.bus.Publish(eventbus.TopicGroupUpdated, g)
	}
	go a.broadcastMeta(ctx, g)
	return g, nil
}

// Local 读取本地群快照。
func (a *GroupApp) Local(groupID string) (group.Group, bool) { return a.groups.Get(groupID) }

// List 返回全部群。
func (a *GroupApp) List() []group.Group { return a.groups.List() }

// ApplyMeta 应用对端传播的群状态。
//
// 规则：必须用 OwnerID 对应公钥验签；epoch 更高才接受；
// 同 epoch 冲突时以 owner 签名为准（取字节序较大者，保证各节点独立算出同一结论）。
func (a *GroupApp) ApplyMeta(g group.Group) error {
	if err := g.Validate(); err != nil {
		return err
	}
	pub, ok := a.ownerPublicKey(g.OwnerID)
	if !ok {
		return fmt.Errorf("group: unknown owner public key for %s", g.OwnerID)
	}
	if !identity.Verify(pub, g.SigningBytes(), g.StateSig) {
		return fmt.Errorf("group: owner signature invalid for %s", g.ID)
	}

	cur, exists := a.groups.Get(g.ID)
	if exists {
		if g.Epoch < cur.Epoch {
			return nil
		}
		if g.Epoch == cur.Epoch {
			if bytes.Equal(g.StateSig, cur.StateSig) {
				return nil
			}
			// epoch 冲突：以 owner 签名为准，且双方独立算出同一胜者
			if bytes.Compare(g.StateSig, cur.StateSig) <= 0 {
				return nil
			}
		}
	}

	if err := a.groups.Upsert(g); err != nil {
		return err
	}
	if a.bus != nil {
		a.bus.Publish(eventbus.TopicGroupUpdated, g)
	}
	return nil
}

// SendGroupMessage 持久化并扇出到所有在线成员。
func (a *GroupApp) SendGroupMessage(ctx context.Context, groupID, text string) (message.Message, error) {
	g, ok := a.groups.Get(groupID)
	if !ok {
		return message.Message{}, fmt.Errorf("group: unknown group %s", groupID)
	}
	now := a.clk.Now()
	msg := message.Message{
		MsgID:     message.NewID(),
		ConvID:    message.GroupConvID(groupID),
		SenderID:  a.self,
		Direction: message.DirectionOut,
		Content:   text,
		MsgType:   message.MsgTypeText,
		SentAt:    now,
		State:     message.StatePending,
	}
	if err := a.store.AppendOutgoing(msg, now); err != nil {
		return msg, err
	}
	go a.fanout(ctx, g, msg)
	return msg, nil
}

// fanout 向 active 且在线、且非自己的成员并发单播（并发上限 MaxDialConcurrency）。
func (a *GroupApp) fanout(ctx context.Context, g group.Group, msg message.Message) {
	online := make(map[identity.NodeID]bool)
	for _, p := range a.dir.List(ports.PeerFilter{OnlineOnly: true}) {
		online[p.NodeID] = true
	}
	targets := g.FanoutPlan(a.self, online)
	if len(targets) == 0 {
		return
	}

	limit := a.MaxDialConcurrency
	if limit <= 0 {
		limit = 8
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for _, id := range targets {
		id := id
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := a.sendGroupMsgTo(ctx, id, msg); err != nil && a.lg != nil {
				a.lg.Debug("group: fanout to peer failed", "peer_id", id.String(), "err", err)
			}
		}()
	}
	wg.Wait()
}

func (a *GroupApp) sendGroupMsgTo(ctx context.Context, peerID identity.NodeID, msg message.Message) error {
	sess, err := a.session(ctx, peerID)
	if err != nil {
		return err
	}
	payload, err := protocol.EncodeJSON(protocol.GroupMsg{
		MsgID:    msg.MsgID,
		GroupID:  msg.ConvID,
		SenderID: msg.SenderID.String(),
		Content:  msg.Content,
		SentAt:   msg.SentAt.UnixMilli(),
	})
	if err != nil {
		return err
	}
	return sess.Send(protocol.New(protocol.TypeGroupMsg, payload))
}

// HandleGroupMsg 处理入站群消息：幂等入库 + 无条件回 ACK。
func (a *GroupApp) HandleGroupMsg(sess ports.Session, f protocol.Frame) error {
	var gm protocol.GroupMsg
	if err := protocol.DecodeJSON(f.Payload, &gm); err != nil {
		return err
	}
	if gm.MsgID == "" || gm.GroupID == "" {
		return nil
	}

	senderID := sess.PeerID()
	if gm.SenderID != "" {
		if id, err := identity.ParseNodeID(gm.SenderID); err == nil {
			senderID = id
		}
	}
	sentAt := a.clk.Now()
	if gm.SentAt > 0 {
		sentAt = time.UnixMilli(gm.SentAt)
	}

	m := message.Message{
		MsgID:     gm.MsgID,
		ConvID:    message.GroupConvID(gm.GroupID),
		SenderID:  senderID,
		Direction: message.DirectionIn,
		Content:   gm.Content,
		MsgType:   message.MsgTypeText,
		SentAt:    sentAt,
		RecvAt:    a.clk.Now(),
		State:     message.StateDelivered,
	}

	inserted, err := a.store.AppendIfAbsent(m)
	if err != nil {
		return err
	}
	if inserted && a.bus != nil {
		a.bus.Publish(eventbus.TopicChatReceived, m)
	}
	// 同单聊：重复消息也必须回 ACK。
	return a.sendAck(sess, gm.MsgID)
}

// HandleGroupMeta 处理群元数据传播。
func (a *GroupApp) HandleGroupMeta(_ ports.Session, f protocol.Frame) error {
	g, err := decodeGroupMeta(f.Payload)
	if err != nil {
		return err
	}
	return a.ApplyMeta(g)
}

// HandleMembersReq 响应成员快照请求（known_epoch 落后时返回全量）。
func (a *GroupApp) HandleMembersReq(sess ports.Session, f protocol.Frame) error {
	var req protocol.GroupMembersReq
	if err := protocol.DecodeJSON(f.Payload, &req); err != nil {
		return err
	}
	g, ok := a.groups.Get(req.GroupID)
	if !ok {
		return nil
	}
	if req.KnownEpoch >= g.Epoch {
		return nil
	}
	payload, err := protocol.EncodeJSON(protocol.GroupMembersResp{Meta: encodeGroupMeta(g)})
	if err != nil {
		return err
	}
	return sess.Send(protocol.New(protocol.TypeGroupMembersRes, payload))
}

// HandleMembersResp 处理成员快照响应。
func (a *GroupApp) HandleMembersResp(_ ports.Session, f protocol.Frame) error {
	var resp protocol.GroupMembersResp
	if err := protocol.DecodeJSON(f.Payload, &resp); err != nil {
		return err
	}
	return a.ApplyMeta(decodeGroupMetaValue(resp.Meta))
}

// SweepOutbox 重发未确认的群消息。
func (a *GroupApp) SweepOutbox(ctx context.Context) {
	now := a.clk.Now()
	due, err := a.store.Due(now, 64)
	if err != nil {
		return
	}
	backoff := a.MaxBackoff
	if backoff <= 0 {
		backoff = 5 * time.Minute
	}
	for _, e := range due {
		g, ok := a.groups.Get(e.ConvID)
		if !ok {
			continue // 不是群消息（单聊由 ChatApp 负责）
		}
		msg, ok := a.store.Get(e.MsgID)
		if !ok {
			_ = a.store.Delete(e.MsgID)
			continue
		}
		a.fanout(ctx, g, msg)
		_ = a.store.BumpAttempt(e.MsgID, now.Add(backoff))
	}
}

func (a *GroupApp) broadcastMeta(ctx context.Context, g group.Group) {
	online := make(map[identity.NodeID]bool)
	for _, p := range a.dir.List(ports.PeerFilter{OnlineOnly: true}) {
		online[p.NodeID] = true
	}
	payload, err := protocol.EncodeJSON(encodeGroupMeta(g))
	if err != nil {
		return
	}
	for _, id := range g.FanoutPlan(a.self, online) {
		sess, err := a.session(ctx, id)
		if err != nil {
			continue
		}
		_ = sess.Send(protocol.New(protocol.TypeGroupMeta, payload))
	}
}

func (a *GroupApp) sendAck(sess ports.Session, msgID string) error {
	payload, err := protocol.EncodeJSON(protocol.ChatAck{MsgID: msgID})
	if err != nil {
		return err
	}
	return sess.Send(protocol.New(protocol.TypeChatAck, payload))
}

func (a *GroupApp) session(ctx context.Context, peerID identity.NodeID) (ports.Session, error) {
	if s, ok := a.conns.SessionOf(peerID); ok {
		return s, nil
	}
	p, ok := a.dir.Get(peerID)
	if !ok || p.LastAddr == "" {
		return nil, fmt.Errorf("group: no known address for peer %s", peerID)
	}
	return a.conns.Dial(ctx, peerID, p.LastAddr)
}

func (a *GroupApp) ownerPublicKey(owner identity.NodeID) ([]byte, bool) {
	if owner == a.self {
		return a.kp.PublicKey(), true
	}
	p, ok := a.dir.Get(owner)
	if !ok || len(p.PublicKey) == 0 {
		return nil, false
	}
	return p.PublicKey, true
}

// ---------------------------------------------------------------------------
// 线上 <-> 领域 转换
// ---------------------------------------------------------------------------

func encodeGroupMeta(g group.Group) protocol.GroupMeta {
	ms := make([]protocol.GroupMemberWire, 0, len(g.Members))
	for _, m := range g.Members {
		ms = append(ms, protocol.GroupMemberWire{
			NodeID:      m.NodeID.String(),
			DisplayName: m.DisplayName,
			Role:        string(m.Role),
			State:       string(m.State),
			JoinedAt:    m.JoinedAt.UnixMilli(),
		})
	}
	return protocol.GroupMeta{
		GroupID:   g.ID,
		Name:      g.Name,
		OwnerID:   g.OwnerID.String(),
		Epoch:     g.Epoch,
		StateSig:  encodeBase64(g.StateSig),
		Members:   ms,
		CreatedAt: g.CreatedAt.UnixMilli(),
	}
}

func decodeGroupMeta(payload []byte) (group.Group, error) {
	var meta protocol.GroupMeta
	if err := protocol.DecodeJSON(payload, &meta); err != nil {
		return group.Group{}, err
	}
	return decodeGroupMetaValue(meta), nil
}

func decodeGroupMetaValue(meta protocol.GroupMeta) group.Group {
	g := group.Group{
		ID:        meta.GroupID,
		Name:      meta.Name,
		Epoch:     meta.Epoch,
		StateSig:  decodeBase64(meta.StateSig),
		CreatedAt: time.UnixMilli(meta.CreatedAt),
	}
	if id, err := identity.ParseNodeID(meta.OwnerID); err == nil {
		g.OwnerID = id
	}
	for _, m := range meta.Members {
		id, err := identity.ParseNodeID(m.NodeID)
		if err != nil {
			continue
		}
		g.Members = append(g.Members, group.Member{
			NodeID:      id,
			DisplayName: m.DisplayName,
			Role:        group.MemberRole(m.Role),
			State:       group.MemberState(m.State),
			JoinedAt:    time.UnixMilli(m.JoinedAt),
		})
	}
	return g
}
