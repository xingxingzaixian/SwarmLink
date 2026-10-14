// Package group 定义群组聚合、成员、epoch 与扇出计划（ADR-003）。
package group

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
)

// MaxMembers 是 v1.0 群成员硬上限（架构书 4.6）。
const MaxMembers = 20

// ErrMemberLimit 表示群成员已达上限。
//
// 用哨兵错误而不是字符串比较：调用方要据此换成用户可读的提示，
// 而字符串一旦被改动，比较会静默失效（表现为「上限提示消失、只报一句英文」）。
var ErrMemberLimit = fmt.Errorf("group: member limit %d exceeded", MaxMembers)

// MemberRole 是成员角色。
type MemberRole string

const (
	RoleOwner  MemberRole = "owner"
	RoleAdmin  MemberRole = "admin"
	RoleMember MemberRole = "member"
)

// MemberState 是成员状态。
type MemberState string

const (
	MemberActive MemberState = "active"
	MemberLeft   MemberState = "left"
	MemberKicked MemberState = "kicked"
)

// StateOrActive 把零值归一为 active（与 GroupState.StateOrActive 同一理由：
// 历史数据与「只构造了成员、没填状态」的构造点都该被当成在群）。
func (s MemberState) StateOrActive() MemberState {
	if s == "" {
		return MemberActive
	}
	return s
}

// GroupState 是群的整体状态（PRD 10.1：活跃 / 解散）。
//
// 解散不是「把成员删光」：成员记录要保留以便审计与 epoch 一致，真正停止
// 路由的是这个群级状态。它进入签名域（见 SigningBytes），否则一条合法的
// (id, epoch, members) 签名可以被原地篡改成「已解散」而无人能察觉。
type GroupState string

const (
	StateActive    GroupState = "active"
	StateDissolved GroupState = "dissolved"
)

// StateOrActive 把零值归一为 active：历史数据与「只填了成员没填状态」的
// 构造点都应被当成活跃群，而不是一个非法状态。
func (s GroupState) StateOrActive() GroupState {
	if s == "" {
		return StateActive
	}
	return s
}

// Valid 报告状态是否为已知取值。
func (s GroupState) Valid() bool {
	switch s.StateOrActive() {
	case StateActive, StateDissolved:
		return true
	default:
		return false
	}
}

// Member 是群成员。
//
// 成员资格由「票号」决定（见 Active）：群主发票（AdmitSeq）、群主撕票（KickSeq）、
// 本人撕票（LeftSeq）。State 退化为【派生缓存】—— 落库前用 SyncDerivedState
// 按 Active() 写回；判定不再以它为准（见 ADR-012）。
type Member struct {
	NodeID      identity.NodeID
	DisplayName string // 非权威提示：不进签名域，冲突时以本地节点目录为准
	Role        MemberRole
	JoinedAt    time.Time
	State       MemberState // 派生缓存；判据见 Active()

	// AdmitSeq 是最近一次「发票」的票号（0 = 从未被准入）。
	AdmitSeq int64
	// KickSeq 是群主最近一次「撕票」的票号（0 = 未被踢过）。
	KickSeq int64
	// LeftSeq 是本人最近一次「撕票」的票号（0 = 未退出过）。
	LeftSeq int64
	// LeftSig 是本人对 LeftSigningBytes(group_id, node_id, left_seq) 的签名；
	// LeftSeq == 0 时为空。
	LeftSig []byte
}

// Active 报告该成员当前是否在群里。
//
// 【必须是严格大于】踢人 / 退出之后 KickSeq（或 LeftSeq）恰好等于 AdmitSeq
// —— 撕的就是当前那张活票；写成 >= 会让踢人当场失效。
//
// 【兼容回退】三个票号全为 0 时退回看 State：升级前的存量数据、以及测试里直接
// 构造的成员都走这条路径，避免一次模型升级让所有群瞬间空掉。
func (m Member) Active() bool {
	if m.AdmitSeq == 0 && m.KickSeq == 0 && m.LeftSeq == 0 {
		return m.State.StateOrActive() == MemberActive
	}
	return m.AdmitSeq > m.KickSeq && m.AdmitSeq > m.LeftSeq
}

// Group 是群聚合。
//
// 名册分两块【独立认证】的内容（见 ADR-012）：
//   - 群主签名块：ID/Name/State/Epoch/MintSeq + 每成员的 AdmitSeq/KickSeq，按 Epoch 原子替换；
//   - left 覆盖：每成员自签的 LeftSeq/LeftSig，按成员取 max。
//
// 两块各自单调且互不覆盖，因此可以独立传播、独立合并。绝不可把两块揉成一份
// 再当作群主签名转发出去 —— 那会以群主之名散布一份没人签过的名册。
type Group struct {
	ID      string
	Name    string
	OwnerID identity.NodeID
	// Epoch 是群主签名块的序号：只在群主侧递增，用于「整块替换」的先后判定。
	Epoch int64
	// MintSeq 是群主的【群级】发票计数器，单调递增（ADR-012 Decision §2）。
	//
	// 为什么必须是群级而不是每用户各自一个：「新票恒大于此前发过的所有票号」
	// 是「群主无需知道对方撕到几号就能把成员拉回来」的全部理由。
	MintSeq int64
	// State 是群整体状态（零值视为 active）。
	State GroupState
	// RosterSig 是群主对 RosterSigningBytes() 的签名。
	RosterSig []byte
	Members   []Member
	CreatedAt time.Time
}

// IsDissolved 报告群是否已解散。
func (g *Group) IsDissolved() bool { return g.State.StateOrActive() == StateDissolved }

// NewID 由群主派生群 ID：SHA-256(ownerID || createdAt || name)[:16]。
func NewID(owner identity.NodeID, createdAt time.Time, name string) string {
	h := sha256.New()
	h.Write([]byte(owner.String()))
	h.Write([]byte{0})
	h.Write([]byte(createdAt.UTC().Format(time.RFC3339Nano)))
	h.Write([]byte{0})
	h.Write([]byte(name))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// RosterSigningBytes 返回群主待签名的规范字节。
//
// 覆盖：group_id | name | state | epoch | mint_seq | Σ(nodeID, admitSeq, kickSeq)
//
// 【不含】LeftSeq/LeftSig（本人签名域 —— 纳入会让「成员一退出」就得劳烦群主重签一次）、
// DisplayName（非权威提示，纳入会让改昵称触发全表重签）。
// 成员按 NodeID 升序排列，保证同一名册产生同一签名（确定性）。
func (g *Group) RosterSigningBytes() []byte {
	members := append([]Member(nil), g.Members...)
	sort.SliceStable(members, func(i, j int) bool { return members[i].NodeID.Less(members[j].NodeID) })

	var tmp [8]byte
	buf := make([]byte, 0, 96+len(members)*48)
	buf = append(buf, g.ID...)
	buf = append(buf, '|')
	buf = append(buf, g.Name...)
	buf = append(buf, '|')
	// state 必须在签名域内：否则「把活跃群篡改成已解散」可以用一条合法的名册签名完成。
	buf = append(buf, string(g.State.StateOrActive())...)
	buf = append(buf, '|')
	binary.BigEndian.PutUint64(tmp[:], uint64(g.Epoch))
	buf = append(buf, tmp[:]...)
	binary.BigEndian.PutUint64(tmp[:], uint64(g.MintSeq))
	buf = append(buf, tmp[:]...)
	for i, m := range members {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, m.NodeID[:]...)
		binary.BigEndian.PutUint64(tmp[:], uint64(m.AdmitSeq))
		buf = append(buf, tmp[:]...)
		binary.BigEndian.PutUint64(tmp[:], uint64(m.KickSeq))
		buf = append(buf, tmp[:]...)
	}
	sum := sha256.Sum256(buf)
	return sum[:]
}

// LeftSigningBytes 返回成员本人待签名的规范字节：H(group_id | node_id | left_seq)。
//
// 放在领域层是为了让「签名什么」只有一份真相 —— 签发与验签各写一遍迟早会分叉，
// 而那会表现为「别人替你退出，你却验不出这是伪造的」。
func LeftSigningBytes(groupID string, nodeID identity.NodeID, leftSeq int64) []byte {
	var tmp [8]byte
	buf := make([]byte, 0, len(groupID)+len(nodeID)+16)
	buf = append(buf, groupID...)
	buf = append(buf, '|')
	buf = append(buf, nodeID[:]...)
	binary.BigEndian.PutUint64(tmp[:], uint64(leftSeq))
	buf = append(buf, tmp[:]...)
	sum := sha256.Sum256(buf)
	return sum[:]
}

// ActiveMembers 返回当前在群的成员。
//
// 判据是 Active()（票号）而不是 State 字段：后者只是派生缓存，
// 用它判定会让「退出 / 被踢」在缓存没刷新时说谎。
func (g *Group) ActiveMembers() []Member {
	out := make([]Member, 0, len(g.Members))
	for _, m := range g.Members {
		if m.Active() {
			out = append(out, m)
		}
	}
	return out
}

// SetDisplayName 写入某成员的显示名（非权威提示，仅在本地节点目录里查得到时用）。
//
// 单独一个方法而不是让 Admit 接收名字：DisplayName 不在群主签名域内，它不该
// 混进「发票」这条受签名保护的操作里 —— 混进去会让「改昵称」看起来像一次成员变更。
func (g *Group) SetDisplayName(id identity.NodeID, name string) {
	if name == "" {
		return
	}
	for i := range g.Members {
		if g.Members[i].NodeID == id {
			g.Members[i].DisplayName = name
			return
		}
	}
}

// Member 按 NodeID 查找成员。
func (g *Group) Member(id identity.NodeID) (Member, bool) {
	for _, m := range g.Members {
		if m.NodeID == id {
			return m, true
		}
	}
	return Member{}, false
}

// Admit 由群主发票：递增群级票号，并把它发给该成员。
//
// 票号取自群级计数器 MintSeq，因此「新票恒大于此前发过的所有票号」——
// 群主【无需知道】对方撕到几号就能把成员拉回来。这是本设计的关键性质，
// 一旦改成「每用户各自的计数器」，拉回就会随机失效（ADR-012 Decision §2）。
//
// 不修改 Epoch：由调用方（commitGroup）负责递增与重签。
func (g *Group) Admit(id identity.NodeID, now time.Time) error {
	for i := range g.Members {
		if g.Members[i].NodeID != id {
			continue
		}
		if g.Members[i].Active() {
			// 已在群里：只把票号推新（等价于「重新确认」），不占新名额。
			g.MintSeq++
			g.Members[i].AdmitSeq = g.MintSeq
			return nil
		}
		// 被踢 / 已退出的成员重新拉回：同样要占一个名额。
		if len(g.ActiveMembers()) >= MaxMembers {
			return ErrMemberLimit
		}
		g.MintSeq++
		g.Members[i].AdmitSeq = g.MintSeq
		g.Members[i].State = MemberActive
		return nil
	}

	if len(g.ActiveMembers()) >= MaxMembers {
		return ErrMemberLimit
	}
	g.MintSeq++
	g.Members = append(g.Members, Member{
		NodeID:   id,
		Role:     RoleMember,
		JoinedAt: now,
		State:    MemberActive,
		AdmitSeq: g.MintSeq,
	})
	return nil
}

// Kick 由群主撕票：把该成员当前的票作废。
//
// 群主只能撕票、不能发电「负票」，因此本人无法撤销 —— 这正是「踢人不可自撤销」
// 在票号模型里的表达（撕票号严格等于票号 ⇒ Active() 为 false）。
func (g *Group) Kick(id identity.NodeID) {
	for i := range g.Members {
		if g.Members[i].NodeID == id {
			g.Members[i].KickSeq = g.Members[i].AdmitSeq
			g.Members[i].State = MemberKicked
			return
		}
	}
}

// MergeLeft 合并一条「本人撕票」（退出）记录：逐成员取 max。
//
// 返回是否带来了实质变化 —— 调用方据此决定要不要落库与广播事件：
// 摘要一致的重复投递不该触发一次 UI 刷新风暴。
//
// 名册里没有这个人时返回 false：那说明我比对方旧，该由 owner 快照去补他，
// 而不是凭空插一条没有 admit 记录的成员。
func (g *Group) MergeLeft(id identity.NodeID, leftSeq int64, sig []byte) bool {
	for i := range g.Members {
		if g.Members[i].NodeID != id {
			continue
		}
		if leftSeq <= g.Members[i].LeftSeq {
			return false // 幂等 + 单调：更低或相等的票号一律忽略
		}
		g.Members[i].LeftSeq = leftSeq
		g.Members[i].LeftSig = sig
		return true
	}
	return false
}

// MergeOwnerSnapshot 用一份【群主签名】的快照整块替换本地群级字段与成员票号。
//
// 只在 in.Epoch 更高时采纳，且是【整块】替换：若逐字段取 max，拼出来的名册
// 不对应任何一份合法签名，而本机还会把它当中继转发出去 —— 那等于以群主之名
// 散布一份没人签过的名册（ADR-012 Decision §5.2）。
//
// left 覆盖由本人签名、独立演进，替换 owner 块时【不能】丢：丢掉会让「成员退出」
// 在一次正常的名册同步之后被静默抹掉。
//
// 调用方必须先验签（本层不持有公钥，保持 domain 零 I/O 与纯函数）。
func (g *Group) MergeOwnerSnapshot(in Group) bool {
	if in.Epoch <= g.Epoch {
		return false
	}

	overrides := make(map[identity.NodeID]leftOverride, len(g.Members))
	for _, m := range g.Members {
		if m.LeftSeq > 0 {
			overrides[m.NodeID] = leftOverride{seq: m.LeftSeq, sig: m.LeftSig}
		}
	}

	g.Name, g.State, g.Epoch, g.MintSeq, g.RosterSig = in.Name, in.State, in.Epoch, in.MintSeq, in.RosterSig
	g.Members = append([]Member(nil), in.Members...)
	for i := range g.Members {
		if o, ok := overrides[g.Members[i].NodeID]; ok && o.seq > g.Members[i].LeftSeq {
			g.Members[i].LeftSeq, g.Members[i].LeftSig = o.seq, o.sig
		}
	}
	return true
}

type leftOverride struct {
	seq int64
	sig []byte
}

// SyncDerivedState 刷新 State 派生缓存（落库前调用）。
//
// 判据始终是 Active()；缓存只是为了让 state 列与旧查询不至于说谎 ——
// 升级期的读路径与报表仍然会读它。
func (g *Group) SyncDerivedState() {
	for i := range g.Members {
		switch {
		case g.Members[i].Active():
			g.Members[i].State = MemberActive
		case g.Members[i].KickSeq >= g.Members[i].AdmitSeq:
			g.Members[i].State = MemberKicked
		default:
			g.Members[i].State = MemberLeft
		}
	}
}

// RosterDigest 是名册完整状态的摘要：两块内容都参与。
//
// 用途只有一个 —— 回答「我是不是拿全了」。摘要一致 ⇒ 不落库、不触发刷新；
// 不一致 ⇒ 触发一次 GROUP_MEMBERS_REQ。
// 不含 DisplayName（非权威提示，改了不该触发同步）。
func (g *Group) RosterDigest() []byte {
	members := append([]Member(nil), g.Members...)
	sort.SliceStable(members, func(i, j int) bool { return members[i].NodeID.Less(members[j].NodeID) })

	var tmp [8]byte
	buf := make([]byte, 0, 64+len(members)*40)
	buf = append(buf, g.ID...)
	buf = append(buf, '|')
	buf = append(buf, string(g.State.StateOrActive())...)
	binary.BigEndian.PutUint64(tmp[:], uint64(g.Epoch))
	buf = append(buf, tmp[:]...)
	for _, m := range members {
		buf = append(buf, m.NodeID[:]...)
		binary.BigEndian.PutUint64(tmp[:], uint64(m.AdmitSeq))
		buf = append(buf, tmp[:]...)
		binary.BigEndian.PutUint64(tmp[:], uint64(m.KickSeq))
		buf = append(buf, tmp[:]...)
		binary.BigEndian.PutUint64(tmp[:], uint64(m.LeftSeq))
		buf = append(buf, tmp[:]...)
	}
	sum := sha256.Sum256(buf)
	return sum[:]
}

// MetaAudience 返回「名册广播」的受众：名册里出现过的**所有人**（含已退出 / 已移出）
// ∩ 在线，不含自己。
//
// 与 FanoutPlan 的差别是刻意的，而且是必须的：
//   - 群消息只发给在群成员（FanoutPlan）；
//   - 名册变更必须【也发给刚被移出的人】—— 否则他会一直以为自己还在群里。
//
// 这个差别不是理论问题：只按 FanoutPlan 广播时，「在线被踢的人收不到自己被踢」
// （他已不在 active 集合里），而「离线被踢的人反而能靠上线推送得知」——
// 同一个动作的两种结果取决于当时在不在线，属于最难解释的那类 bug。
func (g *Group) MetaAudience(self identity.NodeID, online map[identity.NodeID]bool) []identity.NodeID {
	out := make([]identity.NodeID, 0, len(g.Members))
	for _, m := range g.Members {
		if m.NodeID == self {
			continue
		}
		if online != nil && !online[m.NodeID] {
			continue
		}
		out = append(out, m.NodeID)
		if len(out) >= MaxMembers {
			break
		}
	}
	return out
}

// FanoutPlan 返回扇出目标：当前在群且在线的成员（不含自己），上限 MaxMembers。
func (g *Group) FanoutPlan(self identity.NodeID, online map[identity.NodeID]bool) []identity.NodeID {
	out := make([]identity.NodeID, 0, len(g.Members))
	for _, m := range g.Members {
		if !m.Active() || m.NodeID == self {
			continue
		}
		if online != nil && !online[m.NodeID] {
			continue
		}
		out = append(out, m.NodeID)
		if len(out) >= MaxMembers {
			break
		}
	}
	return out
}

// Validate 校验群的基本不变量。
func (g *Group) Validate() error {
	if g.ID == "" {
		return fmt.Errorf("group: empty id")
	}
	if !g.State.Valid() {
		return fmt.Errorf("group: invalid state %q", g.State)
	}
	if g.OwnerID.IsZero() {
		return fmt.Errorf("group: empty owner")
	}
	owner, ok := g.Member(g.OwnerID)
	if !ok || owner.Role != RoleOwner {
		return fmt.Errorf("group: owner %s missing or not owner role", g.OwnerID)
	}
	// 群主必须自己也在群里：被踢/已退出的 owner 会让群进入「无人能变更」的死状态
	// （后续所有成员变更都要求 OwnerID == self，而 owner 又不在名册里）。
	if !owner.Active() {
		return fmt.Errorf("group: owner %s is not active", g.OwnerID)
	}
	if len(g.ActiveMembers()) > MaxMembers {
		return fmt.Errorf("group: active members exceed %d", MaxMembers)
	}
	return nil
}
