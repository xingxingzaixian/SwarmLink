package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/group"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// GroupApp 编排群聊（ADR-003：全互联单播扇出 + 群状态 epoch 签名）。
//
// v1.0 上限 20 人。群消息做「按成员」投递跟踪（离线成员上线后补投，见 2026-10-11 设计）：
// 过去用「单条 outbox + 任一成员 ACK 即出队」，群里第一个回 ACK 的人就让它停止重发，
// 离线成员永远收不到 —— 现在由 deliveries 记录「还差谁」。
type GroupApp struct {
	self       identity.NodeID
	kp         *identity.KeyPair
	groups     ports.GroupRepo
	store      ports.ChatStore
	deliveries ports.GroupDeliveryRepo
	conns      ports.ConnManager
	dir        ports.PeerDirectory
	bus        ports.EventBus
	clk        ports.Clock
	lg         *slog.Logger

	// MaxDialConcurrency 是扇出时的并发拨号上限，避免瞬间 SYN 密集。
	MaxDialConcurrency int
	MaxBackoff         time.Duration
	// SyncInterval 是名册摘要抽查的间隔（0 = 默认 60s）。
	//
	// 抽查是「推送与拉取都静默丢失」时的兜底：局域网里丢包罕见但并非不可能，
	// 而成员表不一致的表现是「有人永远收不到群消息」，没有兜底就只能等下次变更。
	SyncInterval time.Duration
}

// defaultSyncInterval 是名册摘要抽查的默认间隔。
const defaultSyncInterval = 60 * time.Second

// NewGroupApp 构造群聊应用。deliveries 为 nil 时退回旧语义（任一 ACK 即出队），
// 便于不关心离线补投的测试省略它。
func NewGroupApp(
	self identity.NodeID,
	kp *identity.KeyPair,
	groups ports.GroupRepo,
	store ports.ChatStore,
	deliveries ports.GroupDeliveryRepo,
	conns ports.ConnManager,
	dir ports.PeerDirectory,
	bus ports.EventBus,
	clk ports.Clock,
	lg *slog.Logger,
) *GroupApp {
	return &GroupApp{
		self: self, kp: kp, groups: groups, store: store, deliveries: deliveries,
		conns: conns, dir: dir, bus: bus, clk: clk, lg: lg,
		MaxDialConcurrency: 8,
		MaxBackoff:         5 * time.Minute,
		SyncInterval:       defaultSyncInterval,
	}
}

// Start 订阅节点上下线，做「推送 + 拉取」并启动低频抽查。
//
// 这是本设计修掉 G-2 的地方：原实现里成员变更只做一次尽力而为的单播，
// 离线成员错过即永久错过 —— 被移出者仍以为自己在群里，被加入者连群都不存在。
// 原来 GroupApp 连 Start 都没有，因此没有任何「上线后对齐」的触发点。
func (a *GroupApp) Start(ctx context.Context) {
	if a.bus == nil {
		return
	}
	// 推送与拉取都放到独立 goroutine 里：它们可能触发拨号（见 pushGroupsTo 的说明），
	// 而事件是【同步派发】的 —— 在订阅者里做网络 I/O 会把发布方（连接管理的读循环）
	// 一起拖住。与 ChatApp 的 outbox 冲刷同一套做法。
	a.bus.Subscribe(eventbus.TopicPeerOnline, func(payload any) {
		ev, ok := payload.(eventbus.PeerOnline)
		if !ok {
			return
		}
		id, err := identity.ParseNodeID(ev.NodeID)
		if err != nil {
			return
		}
		go func() {
			// 推：他可能压根不知道这个群（刚被拉进来），或名册已落后。
			a.pushGroupsTo(ctx, id)
			// 拉：他可能比我知道得多（比如他刚退出/被踢，而我没收到那条增量）。
			a.requestSyncFrom(ctx, id)
		}()
	})
	a.bus.Subscribe(eventbus.TopicPeerDiscovered, func(payload any) {
		p, ok := payload.(peer.Peer)
		if !ok {
			return
		}
		go a.requestSyncFrom(ctx, p.NodeID)
	})

	go a.loopSync(ctx)
}

// loopSync 按 SyncInterval 周期抽查；ctx 取消即退出。
func (a *GroupApp) loopSync(ctx context.Context) {
	interval := a.SyncInterval
	if interval <= 0 {
		interval = defaultSyncInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.SyncOnce(ctx)
		}
	}
}

// SyncOnce 对每个群挑一个在线成员发一次摘要比对请求（不一致时对方回全量）。
//
// 独立成公开方法是为了可测：把「60 秒一次」写死在 goroutine 里，这条兜底
// 就永远测不到，而它恰恰是「推送与拉取都静默丢失」时的唯一出口。
func (a *GroupApp) SyncOnce(ctx context.Context) {
	_ = ctx
	for _, g := range a.groups.List() {
		for _, id := range a.onlineMembers(g, a.self) {
			sess, ok := a.conns.SessionOf(id)
			if !ok {
				continue // 不拨号：抽查不值得为一次摘要比对建立连接
			}
			payload, err := protocol.EncodeJSON(protocol.GroupMembersReq{
				GroupID:     g.ID,
				KnownEpoch:  g.Epoch,
				KnownDigest: encodeBase64(g.RosterDigest()),
			})
			if err != nil {
				continue
			}
			_ = sess.Send(protocol.New(protocol.TypeGroupMembersReq, payload))
			break // 每个群抽查一个成员即可：摘要是全局一致的
		}
	}
}

// sharedGroups 返回「名册里含该节点」的全部群。
func (a *GroupApp) sharedGroups(id identity.NodeID) []group.Group {
	out := make([]group.Group, 0, 2)
	for _, g := range a.groups.List() {
		if _, member := g.Member(id); member {
			out = append(out, g)
		}
	}
	return out
}

// pushGroupsTo 把该节点【在我名册里】的每个群推一次全量名册。
//
// 任一成员都能推，不要求是群主 —— 这正是「新成员本地没有这个群、无从发起拉取」
// 的解法：他不需要发起，别人会推给他。
//
// 这里【按需拨号】（session 而非 SessionOf）：刚被拉进群的人本机连这个群都不知道，
// 更不会有现成连接 —— 只查现有会话就等于「永远推不到他」，那正是本次要修的缺口。
func (a *GroupApp) pushGroupsTo(ctx context.Context, id identity.NodeID) {
	shared := a.sharedGroups(id)
	if len(shared) == 0 {
		return // 无关节点：不拨号、不推送（推送只会是打扰与信息泄露）
	}
	sess, err := a.session(ctx, id)
	if err != nil {
		return
	}
	for _, g := range shared {
		payload, err := protocol.EncodeJSON(encodeGroupMeta(g))
		if err != nil {
			continue
		}
		_ = sess.Send(protocol.New(protocol.TypeGroupMeta, payload))
	}
}

// requestSyncFrom 向该节点请求「我名册里含他的那些群」的最新快照。
func (a *GroupApp) requestSyncFrom(ctx context.Context, id identity.NodeID) {
	shared := a.sharedGroups(id)
	if len(shared) == 0 {
		return
	}
	sess, err := a.session(ctx, id)
	if err != nil {
		return
	}
	for _, g := range shared {
		payload, err := protocol.EncodeJSON(protocol.GroupMembersReq{
			GroupID:     g.ID,
			KnownEpoch:  g.Epoch,
			KnownDigest: encodeBase64(g.RosterDigest()),
		})
		if err != nil {
			continue
		}
		_ = sess.Send(protocol.New(protocol.TypeGroupMembersReq, payload))
	}
}

// onlineMembers 返回当前在群且在线的成员（可排除某人），上限 MaxMembers。
func (a *GroupApp) onlineMembers(g group.Group, except identity.NodeID) []identity.NodeID {
	online := make(map[identity.NodeID]bool)
	for _, p := range a.dir.List(ports.PeerFilter{OnlineOnly: true}) {
		online[p.NodeID] = true
	}
	return g.FanoutPlan(except, online)
}

// CreateGroup 新建群（本方为 owner），签名后落库并广播群元数据。
func (a *GroupApp) CreateGroup(ctx context.Context, name string, memberIDs []identity.NodeID) (group.Group, error) {
	now := a.clk.Now()
	g := group.Group{
		ID:        group.NewID(a.self, now, name),
		Name:      name,
		OwnerID:   a.self,
		Epoch:     1,
		MintSeq:   1, // 群主自己是 1 号票的持有者
		State:     group.StateActive,
		CreatedAt: now,
	}
	g.Members = append(g.Members, group.Member{
		NodeID: a.self, DisplayName: "me", Role: group.RoleOwner,
		JoinedAt: now, State: group.MemberActive, AdmitSeq: 1,
	})
	for _, id := range memberIDs {
		if id == a.self || id.IsZero() {
			continue
		}
		// 每个初始成员各发一张票（MintSeq 随之递增），保证「新票恒大于旧票」。
		if err := g.Admit(id, now); err != nil {
			return g, err
		}
		if p, ok := a.dir.Get(id); ok {
			g.SetDisplayName(id, p.DisplayName)
		}
	}
	return a.commitGroup(ctx, g)
}

// commitGroup 是所有「群状态变更」的唯一收口：epoch+1 → owner 重新签名 → 落库 → 广播。
//
// 所有变更都必须走这里。否则会出现「成员变了但 epoch 没动」的状态，
// 而对端是按 epoch 判断新旧并静默丢弃的 —— 那种 bug 极难排查。
//
// 注意 MintSeq 不在这里递增：票号由 Admit 在「发票」那一刻分配，
// 放在这里会让「没发票的变更」（改名/解散）也空烧一个票号。
func (a *GroupApp) commitGroup(ctx context.Context, g group.Group) (group.Group, error) {
	// 已存在的群才是「变更」，新建群沿用调用方给定的 epoch。
	if _, exists := a.groups.Get(g.ID); exists {
		g.Epoch++
	}
	g.SyncDerivedState()
	g.RosterSig = a.kp.Sign(g.RosterSigningBytes())

	if err := a.groups.Upsert(g); err != nil {
		return g, err
	}
	a.publishGroup(g)
	go a.broadcastMeta(ctx, g)
	return g, nil
}

// publishGroup 广播一次群状态变化（UI 据此刷新面板与会话标题）。
func (a *GroupApp) publishGroup(g group.Group) {
	if a.bus != nil {
		a.bus.Publish(eventbus.TopicGroupUpdated, g)
	}
}

// AddMember 由群主把成员加入群（发票 + epoch+1 并广播 GROUP_META）。
//
// 「已存在但被踢/已退出」的人也走这条路径：Admit 会发一张【更高的新票】，
// 因此拉回必然生效，不需要知道对方撕到几号（ADR-012 Decision §2）。
func (a *GroupApp) AddMember(ctx context.Context, groupID string, memberID identity.NodeID) (group.Group, error) {
	g, ok := a.groups.Get(groupID)
	if !ok {
		return group.Group{}, fmt.Errorf("group: unknown group %s", groupID)
	}
	if g.OwnerID != a.self {
		return g, fmt.Errorf("group: only the owner can change membership")
	}
	if memberID.IsZero() || memberID == a.self {
		return g, fmt.Errorf("group: invalid member")
	}

	if err := g.Admit(memberID, a.clk.Now()); err != nil {
		if errors.Is(err, group.ErrMemberLimit) {
			return g, fmt.Errorf("group: 成员上限 %d（v1.0）", group.MaxMembers)
		}
		return g, err
	}
	if p, ok := a.dir.Get(memberID); ok {
		g.SetDisplayName(memberID, p.DisplayName)
	}
	return a.commitGroup(ctx, g)
}

// SetMemberState 由群主撕票（移除成员），仅群主可操作。
//
// state 参数只为兼容旧调用（left / kicked）：在票号模型里两者是同一个动作
// —— 撕掉当前那张活票；「是谁撕的」才决定它是退出还是被踢。
func (a *GroupApp) SetMemberState(ctx context.Context, groupID string, memberID identity.NodeID, state group.MemberState) (group.Group, error) {
	g, ok := a.groups.Get(groupID)
	if !ok {
		return group.Group{}, fmt.Errorf("group: unknown group %s", groupID)
	}
	if g.OwnerID != a.self {
		return g, fmt.Errorf("group: only the owner can change membership")
	}
	if memberID == g.OwnerID {
		// 群主不能被自己移除：Validate 要求 owner 必须在成员里，移除它会让
		// 群进入一个无法验签/无法变更的死状态。
		return g, fmt.Errorf("group: 不能移除群主")
	}
	_ = state
	g.Kick(memberID)
	// 该成员待投递的消息记录也要一并清掉：否则那些消息永远凑不齐「全员送达」，
	// 会一直留在 outbox 里空转、界面一直显示「还有人没收到」。
	// 无条件清理：本函数只会被「移除」路径调用（state 恒为 left/kicked）。
	if a.deliveries != nil {
		_ = a.deliveries.DeleteGroupMember(groupID, memberID)
	}
	return a.commitGroup(ctx, g)
}

// RemoveMember 由群主把成员移出群（状态置为 kicked，epoch+1 并广播）。
//
// 与 AddMember 对称的显式入口：调用方读起来就是「踢人」，不必自造 MemberState。
func (a *GroupApp) RemoveMember(ctx context.Context, groupID string, memberID identity.NodeID) (group.Group, error) {
	if memberID.IsZero() {
		return group.Group{}, fmt.Errorf("group: invalid member")
	}
	return a.SetMemberState(ctx, groupID, memberID, group.MemberKicked)
}

// Rename 由群主改名（epoch+1 并广播）。
//
// 改名不单独签名：它走 commitGroup，epoch 递增后对端要用 owner 对
// (id|epoch|state|members) 的签名才能接受 —— 没有私钥就造不出这个 epoch 的签名，
// 因此名字同样不可伪造。
func (a *GroupApp) Rename(ctx context.Context, groupID, name string) (group.Group, error) {
	g, ok := a.groups.Get(groupID)
	if !ok {
		return group.Group{}, fmt.Errorf("group: unknown group %s", groupID)
	}
	if g.OwnerID != a.self {
		return g, fmt.Errorf("group: only the owner can rename the group")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return g, fmt.Errorf("group: 群名不能为空")
	}
	if name == g.Name {
		return g, nil // 幂等：名字没变就不制造一次无意义的 epoch。
	}
	g.Name = name
	return a.commitGroup(ctx, g)
}

// Dissolve 由群主解散群：置 state=dissolved，epoch+1 并广播。
//
// 不删成员记录：对端要靠它验签与维持 epoch 一致。真正停止路由的是这个
// 群级状态（见 send 的守卫与 fanout 的收敛）。同时清掉该群的投递记录，
// 让仍在等待的补投立即停止。
func (a *GroupApp) Dissolve(ctx context.Context, groupID string) (group.Group, error) {
	g, ok := a.groups.Get(groupID)
	if !ok {
		return group.Group{}, fmt.Errorf("group: unknown group %s", groupID)
	}
	if g.OwnerID != a.self {
		return g, fmt.Errorf("group: only the owner can dissolve the group")
	}
	if g.IsDissolved() {
		return g, nil // 幂等
	}
	g.State = group.StateDissolved
	if a.deliveries != nil {
		_ = a.deliveries.DeleteByGroup(groupID)
	}
	return a.commitGroup(ctx, g)
}

// Resign 由群主重签一次名册（不改成员）。
//
// 唯一用途：覆盖 ADR-012 的 R-3 —— RosterSig 覆盖的字节在本次改造中变了，
// 升级前已存在的群其旧签名在新代码下验不过。启动时由群主自动重签一次即可，
// 既不需要用户操作，也不需要留一版兼容代码。
func (a *GroupApp) Resign(ctx context.Context, groupID string) (group.Group, error) {
	g, ok := a.groups.Get(groupID)
	if !ok {
		return group.Group{}, fmt.Errorf("group: unknown group %s", groupID)
	}
	if g.OwnerID != a.self {
		return g, fmt.Errorf("group: only the owner can sign the roster")
	}
	return a.commitGroup(ctx, g)
}

// Leave 由本人退出：本地立即撕票生效，然后尽力通知在线成员。
//
// 为什么本地立即生效：退出权属于本人（ADR-012），把它绑在「群主在线」上
// 正是原设计的硬伤。通知失败只意味着别人晚一点知道，本机状态已是最终态。
//
// 群主不能退出：Validate 要求 owner 是 active 成员，退出会让群进入
// 「无人能变更」的死状态 —— 群主想走就该解散。
func (a *GroupApp) Leave(ctx context.Context, groupID string) (group.Group, error) {
	g, ok := a.groups.Get(groupID)
	if !ok {
		return group.Group{}, fmt.Errorf("group: unknown group %s", groupID)
	}
	if g.OwnerID == a.self {
		return g, fmt.Errorf("group: 群主不能退出，请改为解散群")
	}
	me, ok := g.Member(a.self)
	if !ok {
		return g, fmt.Errorf("group: 你不在这个群里")
	}
	if !me.Active() {
		return g, nil // 已经退出：幂等
	}

	leftSeq := me.AdmitSeq // 撕掉当前那张活票
	sig := a.kp.Sign(group.LeftSigningBytes(groupID, a.self, leftSeq))
	if !g.MergeLeft(a.self, leftSeq, sig) {
		return g, nil
	}

	// 该成员待投递的群消息记录一并清掉：否则那些消息永远凑不齐「全员送达」，
	// 会一直留在 outbox 里空转、界面一直显示「还有人没收到」。
	if a.deliveries != nil {
		_ = a.deliveries.DeleteGroupMember(groupID, a.self)
	}

	// 刻意【不重签】名册：退出只改 LeftSeq，而它不在群主签名域内
	// （RosterSigningBytes 不含 left）—— 所以群主那份签名依然有效。
	// 若在这里用自己的密钥重签，就会产出一份「用成员私钥签的名册」，
	// 对端拿群主公钥验签必然失败，整条消息会被当成伪造丢弃。
	g.SyncDerivedState()
	if err := a.groups.Upsert(g); err != nil {
		return g, err
	}
	a.publishGroup(g)
	go a.broadcastLeave(ctx, g, leftSeq, sig)
	return g, nil
}

// broadcastLeave 把本人退出通知给在线的其他成员。
//
// 只发给「在线的 active 成员」：不可达的人上线后会由 Start 的推送/拉取补上
// （名册摘要不同 ⇒ 对方会来拉全量，其中就带着这条 left）。
func (a *GroupApp) broadcastLeave(ctx context.Context, g group.Group, leftSeq int64, sig []byte) {
	payload, err := protocol.EncodeJSON(protocol.GroupLeave{
		GroupID: g.ID,
		NodeID:  a.self.String(),
		LeftSeq: leftSeq,
		Sig:     encodeBase64(sig),
	})
	if err != nil {
		return
	}
	for _, id := range a.onlineMembers(g, a.self) {
		sess, err := a.session(ctx, id)
		if err != nil {
			continue
		}
		_ = sess.Send(protocol.New(protocol.TypeGroupLeave, payload))
	}
}

// Local 读取本地群快照。
func (a *GroupApp) Local(groupID string) (group.Group, bool) { return a.groups.Get(groupID) }

// List 返回全部群。
func (a *GroupApp) List() []group.Group { return a.groups.List() }

// ApplyMeta 应用对端传播的群状态（两块独立合并）。
//
// 规则：
//  1. 必须用 OwnerID 对应公钥验签（群主签名块是整体，不能被拼装）；
//  2. 群主块按 Epoch 原子替换（见 Group.MergeOwnerSnapshot）；
//  3. left 覆盖逐成员取 max（自带本人签名，独立演进）；
//  4. 只有【名册真的变了】才落库与广播 —— 摘要一致的重复投递不该触发刷新风暴。
func (a *GroupApp) ApplyMeta(g group.Group) error {
	if err := g.Validate(); err != nil {
		return err
	}
	pub, ok := a.ownerPublicKey(g.OwnerID)
	if !ok {
		// 还没见过群主（跨网段冷启动）：名册留着下次再拉。
		return fmt.Errorf("group: unknown owner public key for %s", g.OwnerID)
	}
	if !identity.Verify(pub, g.RosterSigningBytes(), g.RosterSig) {
		return fmt.Errorf("group: owner signature invalid for %s", g.ID)
	}

	// 每条 left 必须由【该成员本人】签：否则任何人伪造一条 left 就能把别人踢出群，
	// 而本设计里没有服务端可以拦这种事。
	//
	// 验不过的一律【丢弃这条 left】（其余字段照常合并），方向是刻意选的：
	// 忽略一条验不过的退出，后果只是「某人晚一点才退出」；反过来照单全收，
	// 后果是任何人都能伪造他人的退出 —— 前者的代价小得多。
	//
	// 顺带说明为什么是「丢弃」而不是「清零」：清零会把一个我们还不认识公钥的
	// 成员（跨网段冷启动）当成从未退出过，而他一旦被认识，下一次同步会再带上
	// 那条自签 —— 丢弃是可自愈的。
	verified := make([]group.Member, 0, len(g.Members))
	for _, m := range g.Members {
		if m.LeftSeq > 0 {
			mpub, ok := a.peerPublicKey(m.NodeID)
			if !ok || !identity.Verify(mpub, group.LeftSigningBytes(g.ID, m.NodeID, m.LeftSeq), m.LeftSig) {
				m.LeftSeq, m.LeftSig = 0, nil
			}
		}
		verified = append(verified, m)
	}
	// 复制一份再改 Members：g.Members 与调用方共享底层数组，就地改会改到调用方。
	sanitized := g
	sanitized.Members = verified

	cur, exists := a.groups.Get(g.ID)
	changed := false
	if !exists {
		// 本机还不认识这个群（典型：刚被拉进来）—— 直接采纳。
		sanitized.SyncDerivedState()
		if err := a.groups.Upsert(sanitized); err != nil {
			return err
		}
		a.publishGroup(sanitized)
		return nil
	}

	// 群主块：更高 epoch 才整块替换；替换时会把本地 left 覆盖带过去。
	if cur.MergeOwnerSnapshot(sanitized) {
		changed = true
	}
	// left 覆盖：独立于 epoch 合并 —— 对方可能只带来了「某人退出了」这一条增量，
	// 而群主块一点没变（退出不经群主，epoch 不动）。
	for _, m := range verified {
		if m.LeftSeq <= 0 {
			continue
		}
		if cur.MergeLeft(m.NodeID, m.LeftSeq, m.LeftSig) {
			changed = true
		}
	}
	if !changed {
		return nil
	}

	cur.SyncDerivedState()
	if err := a.groups.Upsert(cur); err != nil {
		return err
	}
	a.publishGroup(cur)
	return nil
}

// SendGroupMessage 持久化并扇出到所有在线成员。
func (a *GroupApp) SendGroupMessage(ctx context.Context, groupID, text string) (message.Message, error) {
	return a.send(ctx, groupID, text, message.MsgTypeText)
}

// SendGroupImage 发送群聊图片消息（content 为内联图片 JSON 信封）。
//
// 与文本走同一条扇出链路：群里的图片没有额外的传输通道，
// 因此也不会出现在传输列表里。
func (a *GroupApp) SendGroupImage(ctx context.Context, groupID, content string) (message.Message, error) {
	return a.send(ctx, groupID, content, message.MsgTypeImage)
}

// SendGroupFile 发送群聊文件消息（content 为 job_id 和文件名的 JSON）。
//
// 这条消息只是让群里出现一张文件卡片；文件本体由发送端对每个在线成员
// 各自发起点对点传输（见前端 useFileSend），群消息不承载字节。
func (a *GroupApp) SendGroupFile(ctx context.Context, groupID, jobID, fileName string) (message.Message, error) {
	payload, err := protocol.EncodeJSON(message.FileMessageContent{JobID: jobID, FileName: fileName})
	if err != nil {
		return message.Message{}, fmt.Errorf("group: encode file content: %w", err)
	}
	return a.send(ctx, groupID, string(payload), message.MsgTypeFile)
}

// SendGroupFolder 发送群聊文件夹消息（content 为一次性聚合卡片的 JSON）。
func (a *GroupApp) SendGroupFolder(
	ctx context.Context,
	groupID, batchID, folderName string,
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
		return message.Message{}, fmt.Errorf("group: encode folder content: %w", err)
	}
	return a.send(ctx, groupID, string(payload), message.MsgTypeFolder)
}

// send 是群文本 / 图片 / 文件卡片共用的发送实现。
func (a *GroupApp) send(ctx context.Context, groupID, content string, msgType message.MsgType) (message.Message, error) {
	g, ok := a.groups.Get(groupID)
	if !ok {
		return message.Message{}, fmt.Errorf("group: unknown group %s", groupID)
	}
	if g.IsDissolved() {
		return message.Message{}, fmt.Errorf("group: 群已解散，无法发送")
	}
	// 自己不在群里就别发了：消息到了对端也会被成员资格校验丢掉（白走一趟网络），
	// 而且界面上会先出现一条永远送不达的「发送中」。
	//
	// 这道守卫与接收侧的校验是配套的：发送侧少发、接收侧兜底 ——
	// 后者才是防伪造的那一道（发送侧只依赖本地名册，随时可能落后）。
	if me, ok := g.Member(a.self); ok && !me.Active() {
		return message.Message{}, fmt.Errorf("group: 你不在这个群里，无法发送")
	}
	now := a.clk.Now()
	msg := message.Message{
		MsgID:     message.NewID(),
		ConvID:    message.GroupConvID(groupID),
		SenderID:  a.self,
		Direction: message.DirectionOut,
		Content:   content,
		MsgType:   msgType,
		SentAt:    now,
		State:     message.StatePending,
	}
	if err := a.store.AppendOutgoing(msg, now); err != nil {
		return msg, err
	}
	// 登记「该消息要送到哪些成员」：这是离线补投与「N 人未送达」的唯一依据。
	// 必须在 AppendOutgoing 之后立即完成，否则 fanout 看不到待投名单。
	targets := a.activeOthers(g)
	if err := a.ensureDeliveries(msg.MsgID, groupID, targets); err != nil && a.lg != nil {
		a.lg.Warn("group: ensure deliveries", "msg_id", msg.MsgID, "err", err)
	}
	if len(targets) == 0 {
		// 群里只有自己：没有收件人，直接算送达，别让它在 outbox 里空转。
		_ = a.store.Deliver(msg.MsgID)
		msg.State = message.StateDelivered
		return msg, nil
	}
	go a.fanout(ctx, g, msg)
	return msg, nil
}

// activeOthers 返回除自己以外的 active 成员（判据是票号，不是 State 缓存）。
func (a *GroupApp) activeOthers(g group.Group) []identity.NodeID {
	out := make([]identity.NodeID, 0, len(g.Members))
	for _, m := range g.Members {
		if m.Active() && m.NodeID != a.self {
			out = append(out, m.NodeID)
		}
	}
	return out
}

// ensureDeliveries 登记投递记录（deliveries 为 nil 时退回旧语义：跳过）。
func (a *GroupApp) ensureDeliveries(msgID, groupID string, members []identity.NodeID) error {
	if a.deliveries == nil {
		return nil
	}
	return a.deliveries.EnsureDeliveries(msgID, groupID, members)
}

// pendingSet 返回尚未送达的成员集合；没有投递跟踪时返回 nil（表示不筛选）。
func (a *GroupApp) pendingSet(msgID string) map[identity.NodeID]bool {
	if a.deliveries == nil {
		return nil
	}
	pending, err := a.deliveries.PendingMembers(msgID)
	if err != nil {
		return nil
	}
	set := make(map[identity.NodeID]bool, len(pending))
	for _, id := range pending {
		set[id] = true
	}
	return set
}

// pendingActive 返回「尚未送达 且 当前仍是 active 成员」的集合。
//
// 必须再与 active 成员求交：投递记录是【发送时】登记的，之后成员可能被移出。
// 被移出的人不会再收到消息，因此不能让他一直挡着「全员送达」。
func (a *GroupApp) pendingActive(g group.Group, msgID string) []identity.NodeID {
	if a.deliveries == nil {
		return nil
	}
	pending, err := a.deliveries.PendingMembers(msgID)
	if err != nil {
		return nil
	}
	active := make(map[identity.NodeID]bool, len(g.Members))
	for _, m := range g.Members {
		if m.Active() {
			active[m.NodeID] = true
		}
	}
	out := pending[:0]
	for _, id := range pending {
		if active[id] {
			out = append(out, id)
		}
	}
	return out
}

// 说明：active 判据用 Member.Active()（票号）而不是 State 字段 ——
// State 只是派生缓存，用它判定会让「退出 / 被踢」在缓存没刷新时说谎。

// HandleGroupAck 处理某成员对一条群消息的送达确认。
//
// 由 ChatApp.HandleChatAck 在识别出「这是一条群消息」后委派过来 ——
// 群与单聊共用 TypeChatAck，但送达语义不同：单聊是一对一（一条 ACK 即完成），
// 群聊要按成员记账，只有全员确认才算送达。
func (a *GroupApp) HandleGroupAck(from identity.NodeID, msgID string) error {
	if a.deliveries == nil {
		// 没有投递跟踪：退回旧语义（任一 ACK 即出队）。
		return a.store.Deliver(msgID)
	}
	if err := a.deliveries.AckDelivery(msgID, from); err != nil {
		return err
	}
	msg, ok := a.store.Get(msgID)
	if !ok {
		return nil
	}
	g, ok := a.groups.Get(msg.ConvID)
	if !ok {
		return nil
	}
	if len(a.pendingActive(g, msgID)) > 0 {
		return nil // 还有人在等待（可能离线），消息留在 outbox 里由 sweep 补投。
	}
	if err := a.store.Deliver(msgID); err != nil {
		return err
	}
	// 消息已完成：清掉投递记录（界面上不再显示「N 人未送达」，也避免无限增长）。
	_ = a.deliveries.DeleteByMsg(msgID)
	if a.bus != nil {
		a.bus.Publish(eventbus.TopicChatDelivered, protocol.ChatAck{MsgID: msgID})
	}
	return nil
}

// fanout 向「未送达 且 active 且在线」的成员并发单播（并发上限 MaxDialConcurrency）。
//
// 两个筛选缺一不可：未送达避免重复发送已确认的人；在线避免对离线成员
// 做无意义的拨号（他们由 SweepOutbox 在上线后补投）。
func (a *GroupApp) fanout(ctx context.Context, g group.Group, msg message.Message) {
	online := make(map[identity.NodeID]bool)
	for _, p := range a.dir.List(ports.PeerFilter{OnlineOnly: true}) {
		online[p.NodeID] = true
	}
	targets := g.FanoutPlan(a.self, online)
	if pending := a.pendingSet(msg.MsgID); pending != nil {
		filtered := targets[:0]
		for _, id := range targets {
			if pending[id] {
				filtered = append(filtered, id)
			}
		}
		targets = filtered
	}
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
		MsgType:  string(msg.MsgType), // 群里也要声明类型，否则图片到达后变成空白文字气泡
		FileID:   msg.FileID,
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

	msgType := message.MsgType(gm.MsgType)
	if msgType == "" {
		msgType = message.MsgTypeText
	}

	// 成员资格校验：非当前在群成员发来的群消息丢弃，但【仍回 ACK】——
	// 不回 ACK 会让发送方按 outbox 策略无限重发同一条。
	//
	// 判据用【当前】名册而不是「发送时的名册」：成员资格是现在的事实。
	// 少了这道校验，一个已被移出、本地名册还没更新的人仍会被正常收下消息
	// 并显示出来（这是原实现里最实的一处漏洞）。
	if g, ok := a.groups.Get(gm.GroupID); ok {
		if m, ok := g.Member(senderID); !ok || !m.Active() {
			return a.sendAck(sess, gm.MsgID)
		}
	}

	content := gm.Content
	if msgType == message.MsgTypeImage {
		sanitized, ok := sanitizeImageContent(content)
		if !ok {
			// 与单聊同策略：坏图片消息丢弃，但仍回 ACK，避免发送方无限重发。
			return a.sendAck(sess, gm.MsgID)
		}
		content = sanitized
	}
	// 文件消息的 content 是 JSON，不需要特殊处理，直接存储即可

	m := message.Message{
		MsgID:     gm.MsgID,
		ConvID:    message.GroupConvID(gm.GroupID),
		SenderID:  senderID,
		Direction: message.DirectionIn,
		Content:   content,
		MsgType:   msgType,
		FileID:    gm.FileID,
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

// HandleMembersReq 响应成员快照请求。
//
// 判据是【名册摘要】而不是 epoch：退出不经群主、epoch 不动，
// 只看 epoch 会让「别人退出了」这条增量永远同步不出去（那是左迁移）。
func (a *GroupApp) HandleMembersReq(sess ports.Session, f protocol.Frame) error {
	var req protocol.GroupMembersReq
	if err := protocol.DecodeJSON(f.Payload, &req); err != nil {
		return err
	}
	g, ok := a.groups.Get(req.GroupID)
	if !ok {
		return nil
	}
	// 摘要一致 ⇒ 静默：省掉一次「其实什么都没变」的往返，也避免摘要一致的
	// 重复投递触发一轮 UI 刷新风暴。
	if req.KnownDigest != "" && req.KnownDigest == encodeBase64(g.RosterDigest()) {
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
			if a.deliveries != nil {
				_ = a.deliveries.DeleteByMsg(e.MsgID)
			}
			continue
		}
		// 群已解散：停止补投，直接把消息标为送达并出队（成员记录仍保留）。
		if g.IsDissolved() {
			_ = a.store.Deliver(e.MsgID)
			if a.deliveries != nil {
				_ = a.deliveries.DeleteByMsg(e.MsgID)
			}
			continue
		}
		// 全员已送达（或被移出的成员已不再计入）：出队，别再退避空转；投递记录
		// 也随之清掉（消息已完成，界面上不再需要「N 人未送达」，留着只会无限增长）。
		if a.deliveries != nil {
			if len(a.pendingActive(g, e.MsgID)) == 0 {
				_ = a.store.Deliver(e.MsgID)
				_ = a.deliveries.DeleteByMsg(e.MsgID)
				continue
			}
		}
		a.fanout(ctx, g, msg)
		_ = a.store.BumpAttempt(e.MsgID, now.Add(backoff))
	}
}

// broadcastMeta 把群名册通知给在线的相关成员。
//
// 受众用 MetaAudience 而【不是】FanoutPlan：名册变更必须也发给刚被移出的人，
// 否则他收不到自己被移出的那条，会一直以为自己还在群里。
// 这个差别不是理论问题 —— 只按 FanoutPlan 广播时，「在线被踢的人收不到自己被踢」
// （他已不在 active 集合里），而「离线被踢的人反而能靠上线推送得知」。
func (a *GroupApp) broadcastMeta(ctx context.Context, g group.Group) {
	payload, err := protocol.EncodeJSON(encodeGroupMeta(g))
	if err != nil {
		return
	}
	for _, id := range a.metaAudience(g) {
		sess, err := a.session(ctx, id)
		if err != nil {
			continue
		}
		_ = sess.Send(protocol.New(protocol.TypeGroupMeta, payload))
	}
}

// metaAudience 返回名册广播的在线受众（含已退出 / 已移出的人）。
func (a *GroupApp) metaAudience(g group.Group) []identity.NodeID {
	online := make(map[identity.NodeID]bool)
	for _, p := range a.dir.List(ports.PeerFilter{OnlineOnly: true}) {
		online[p.NodeID] = true
	}
	return g.MetaAudience(a.self, online)
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
	return a.peerPublicKey(owner)
}

// peerPublicKey 取某节点已认证的公钥（本机直接给，其他从节点目录里取）。
//
// 公钥来源必须是【签名通告或握手】认证过的身份 —— 目录里那些由第三方转述的
// 条目不带签名，拿它验签等于把「谁能冒充谁」交给转发者决定。
func (a *GroupApp) peerPublicKey(id identity.NodeID) ([]byte, bool) {
	if id == a.self {
		return a.kp.PublicKey(), true
	}
	p, ok := a.dir.Get(id)
	if !ok || len(p.PublicKey) == 0 {
		return nil, false
	}
	return p.PublicKey, true
}

// HandleGroupLeave 应用别人的退出记录。
//
// 接受条件三条全满足：认识该群、自签有效（且必须由【该成员本人】签）、票号更高。
// 少任何一条都会出问题：不验签则任何人能替你退出；不比票号则一条旧记录能把
// 你「退回去」（左迁移）。
func (a *GroupApp) HandleGroupLeave(_ ports.Session, f protocol.Frame) error {
	var lv protocol.GroupLeave
	if err := protocol.DecodeJSON(f.Payload, &lv); err != nil {
		return err
	}
	if lv.GroupID == "" || lv.NodeID == "" || lv.LeftSeq <= 0 {
		return nil
	}
	id, err := identity.ParseNodeID(lv.NodeID)
	if err != nil {
		return nil
	}
	g, ok := a.groups.Get(lv.GroupID)
	if !ok {
		// 我还不认识这个群：留给群主名册去补（那时才知道他是不是成员）。
		return nil
	}
	pub, ok := a.peerPublicKey(id)
	if !ok {
		return nil
	}
	sig := decodeBase64(lv.Sig)
	if !identity.Verify(pub, group.LeftSigningBytes(lv.GroupID, id, lv.LeftSeq), sig) {
		return fmt.Errorf("group: invalid leave signature from %s", id)
	}
	if !g.MergeLeft(id, lv.LeftSeq, sig) {
		return nil // 幂等 / 单调：旧记录或重复投递
	}
	g.SyncDerivedState()
	if err := a.groups.Upsert(g); err != nil {
		return err
	}
	a.publishGroup(g)
	return nil
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
			AdmitSeq:    m.AdmitSeq,
			KickSeq:     m.KickSeq,
			LeftSeq:     m.LeftSeq,
			LeftSig:     encodeBase64(m.LeftSig),
		})
	}
	return protocol.GroupMeta{
		GroupID:      g.ID,
		Name:         g.Name,
		OwnerID:      g.OwnerID.String(),
		Epoch:        g.Epoch,
		MintSeq:      g.MintSeq,
		State:        string(g.State.StateOrActive()),
		RosterSig:    encodeBase64(g.RosterSig),
		Members:      ms,
		CreatedAt:    g.CreatedAt.UnixMilli(),
		RosterDigest: encodeBase64(g.RosterDigest()),
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
		MintSeq:   meta.MintSeq,
		State:     group.GroupState(meta.State).StateOrActive(),
		RosterSig: decodeBase64(meta.RosterSig),
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
			// State 只是派生缓存：解码端不信任它，判定一律走票号。
			State:    group.MemberState(m.State),
			JoinedAt: time.UnixMilli(m.JoinedAt),
			AdmitSeq: m.AdmitSeq,
			KickSeq:  m.KickSeq,
			LeftSeq:  m.LeftSeq,
			LeftSig:  decodeBase64(m.LeftSig),
		})
	}
	return g
}
