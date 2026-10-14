package sqlite

import (
	"database/sql"

	"github.com/swarmlink/swarmlink/internal/domain/group"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
)

// Groups 实现 ports.GroupRepo。
type Groups struct{ db *DB }

var _ ports.GroupRepo = (*Groups)(nil)

// NewGroups 构造群仓储。
func NewGroups(db *DB) *Groups { return &Groups{db: db} }

// Upsert 单事务写入群与成员表。
func (g *Groups) Upsert(in group.Group) error {
	// 先复制再刷新派生缓存：Upsert 不该改动调用方传进来的切片
	// （Members 是共享底层数组，就地改 State 会顺带改掉调用方的 Group）。
	in.Members = append([]group.Member(nil), in.Members...)
	in.SyncDerivedState()

	return g.db.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
INSERT INTO groups(group_id, name, owner_id, epoch, state, state_sig, mint_seq, created_at)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(group_id) DO UPDATE SET
  name       = excluded.name,
  owner_id   = excluded.owner_id,
  epoch      = excluded.epoch,
  state      = excluded.state,
  state_sig  = excluded.state_sig,
  mint_seq   = excluded.mint_seq`,
			in.ID, in.Name, in.OwnerID.String(), in.Epoch, string(in.State.StateOrActive()),
			b64(in.RosterSig), in.MintSeq, msOf(in.CreatedAt),
		); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM group_members WHERE group_id = ?`, in.ID); err != nil {
			return err
		}
		for _, m := range in.Members {
			role := m.Role
			if role == "" {
				role = group.RoleMember
			}
			state := m.State
			if state == "" {
				state = group.MemberActive
			}
			if _, err := tx.Exec(`
INSERT INTO group_members(
  group_id, node_id, display_name, role, state, joined_at,
  admit_seq, kick_seq, left_seq, left_sig)
VALUES(?,?,?,?,?,?,?,?,?,?)`,
				in.ID, m.NodeID.String(), m.DisplayName, string(role), string(state), msOf(m.JoinedAt),
				m.AdmitSeq, m.KickSeq, m.LeftSeq, b64(m.LeftSig),
			); err != nil {
				return err
			}
		}
		return nil
	})
}

const groupCols = `group_id, name, owner_id, epoch, state, state_sig, mint_seq, created_at`

// Get 读取群及其成员。
func (g *Groups) Get(groupID string) (group.Group, bool) {
	out, err := g.loadOne(groupID)
	if err != nil {
		return group.Group{}, false
	}
	return out, true
}

// List 返回全部群。
func (g *Groups) List() []group.Group {
	rows, err := g.db.Query(`SELECT ` + groupCols + ` FROM groups ORDER BY created_at`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []group.Group
	for rows.Next() {
		item, err := scanGroup(rows)
		if err != nil {
			return out
		}
		item.Members = g.membersOf(item.ID)
		out = append(out, item)
	}
	return out
}

func (g *Groups) loadOne(groupID string) (group.Group, error) {
	row := g.db.QueryRow(`SELECT `+groupCols+` FROM groups WHERE group_id = ?`, groupID)
	out, err := scanGroup(row)
	if err != nil {
		return out, err
	}
	out.Members = g.membersOf(groupID)
	return out, nil
}

func (g *Groups) membersOf(groupID string) []group.Member {
	rows, err := g.db.Query(`
SELECT node_id, display_name, role, state, joined_at,
       admit_seq, kick_seq, left_seq, left_sig
FROM group_members WHERE group_id = ?`, groupID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []group.Member
	for rows.Next() {
		var (
			m        group.Member
			nodeID   string
			dispName sql.NullString
			role     string
			state    string
			joinedAt int64
			leftSig  sql.NullString
		)
		if err := rows.Scan(&nodeID, &dispName, &role, &state, &joinedAt,
			&m.AdmitSeq, &m.KickSeq, &m.LeftSeq, &leftSig); err != nil {
			return out
		}
		id, err := identity.ParseNodeID(nodeID)
		if err != nil {
			continue
		}
		m.NodeID = id
		m.DisplayName = nullString(dispName)
		m.Role = group.MemberRole(role)
		// state 列只是派生缓存；判定一律走 Active()（票号）。
		m.State = group.MemberState(state)
		m.JoinedAt = timeOf(joinedAt)
		m.LeftSig = unb64(nullString(leftSig))
		out = append(out, m)
	}
	return out
}

func scanGroup(s rowScanner) (group.Group, error) {
	var (
		out       group.Group
		ownerID   string
		state     string
		stateSig  sql.NullString
		createdAt int64
	)
	if err := s.Scan(&out.ID, &out.Name, &ownerID, &out.Epoch, &state, &stateSig,
		&out.MintSeq, &createdAt); err != nil {
		return out, err
	}
	if id, err := identity.ParseNodeID(ownerID); err == nil {
		out.OwnerID = id
	}
	out.State = group.GroupState(state).StateOrActive()
	out.RosterSig = unb64(nullString(stateSig))
	out.CreatedAt = timeOf(createdAt)
	return out, nil
}
