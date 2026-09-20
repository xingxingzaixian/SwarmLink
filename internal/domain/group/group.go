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

// Member 是群成员。
type Member struct {
	NodeID      identity.NodeID
	DisplayName string
	Role        MemberRole
	JoinedAt    time.Time
	State       MemberState
}

// Group 是群聚合。任何成员变更都会让 Epoch+1，并由 owner 重新签名。
type Group struct {
	ID        string
	Name      string
	OwnerID   identity.NodeID
	Epoch     int64
	StateSig  []byte
	Members   []Member
	CreatedAt time.Time
}

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

// SigningBytes 返回 owner 待签名的规范字节：H(group_id|epoch|members)。
// 成员按 NodeID 升序排列，保证同一成员集合产生同一签名（确定性）。
func (g *Group) SigningBytes() []byte {
	members := append([]Member(nil), g.Members...)
	sort.SliceStable(members, func(i, j int) bool { return members[i].NodeID.Less(members[j].NodeID) })

	var tmp [8]byte
	buf := make([]byte, 0, 64+len(members)*48)
	buf = append(buf, g.ID...)
	buf = append(buf, '|')
	binary.BigEndian.PutUint64(tmp[:], uint64(g.Epoch))
	buf = append(buf, tmp[:]...)
	buf = append(buf, '|')
	for i, m := range members {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, m.NodeID[:]...)
		buf = append(buf, ':')
		buf = append(buf, string(m.Role)...)
		buf = append(buf, ':')
		buf = append(buf, string(m.State)...)
	}
	sum := sha256.Sum256(buf)
	return sum[:]
}

// ActiveMembers 返回 state=active 的成员。
func (g *Group) ActiveMembers() []Member {
	out := make([]Member, 0, len(g.Members))
	for _, m := range g.Members {
		if m.State == MemberActive {
			out = append(out, m)
		}
	}
	return out
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

// AddMember 追加成员（若已存在则置回 active）。不修改 Epoch，由调用方负责递增与重签。
func (g *Group) AddMember(m Member) error {
	if _, ok := g.Member(m.NodeID); ok {
		for i := range g.Members {
			if g.Members[i].NodeID == m.NodeID {
				g.Members[i].State = MemberActive
				return nil
			}
		}
	}
	if len(g.ActiveMembers()) >= MaxMembers {
		return fmt.Errorf("group: member limit %d exceeded", MaxMembers)
	}
	g.Members = append(g.Members, m)
	return nil
}

// RemoveMember 将成员置为 left/kicked（保留记录以便审计与 epoch 一致）。
func (g *Group) RemoveMember(id identity.NodeID, st MemberState) {
	for i := range g.Members {
		if g.Members[i].NodeID == id {
			g.Members[i].State = st
			return
		}
	}
}

// FanoutPlan 返回扇出目标：state=active 且在线的成员（不含自己），上限 MaxMembers。
func (g *Group) FanoutPlan(self identity.NodeID, online map[identity.NodeID]bool) []identity.NodeID {
	out := make([]identity.NodeID, 0, len(g.Members))
	for _, m := range g.Members {
		if m.State != MemberActive || m.NodeID == self {
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
	if g.OwnerID.IsZero() {
		return fmt.Errorf("group: empty owner")
	}
	owner, ok := g.Member(g.OwnerID)
	if !ok || owner.Role != RoleOwner {
		return fmt.Errorf("group: owner %s missing or not owner role", g.OwnerID)
	}
	if len(g.ActiveMembers()) > MaxMembers {
		return fmt.Errorf("group: active members exceed %d", MaxMembers)
	}
	return nil
}
