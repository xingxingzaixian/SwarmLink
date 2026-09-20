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
	return g.db.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
INSERT INTO groups(group_id, name, owner_id, epoch, state_sig, created_at)
VALUES(?,?,?,?,?,?)
ON CONFLICT(group_id) DO UPDATE SET
  name       = excluded.name,
  owner_id   = excluded.owner_id,
  epoch      = excluded.epoch,
  state_sig  = excluded.state_sig`,
			in.ID, in.Name, in.OwnerID.String(), in.Epoch, b64(in.StateSig), msOf(in.CreatedAt),
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
INSERT INTO group_members(group_id, node_id, display_name, role, state, joined_at)
VALUES(?,?,?,?,?,?)`,
				in.ID, m.NodeID.String(), m.DisplayName, string(role), string(state), msOf(m.JoinedAt),
			); err != nil {
				return err
			}
		}
		return nil
	})
}

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
	rows, err := g.db.Query(`SELECT group_id, name, owner_id, epoch, state_sig, created_at FROM groups ORDER BY created_at`)
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
	row := g.db.QueryRow(
		`SELECT group_id, name, owner_id, epoch, state_sig, created_at FROM groups WHERE group_id = ?`,
		groupID,
	)
	out, err := scanGroup(row)
	if err != nil {
		return out, err
	}
	out.Members = g.membersOf(groupID)
	return out, nil
}

func (g *Groups) membersOf(groupID string) []group.Member {
	rows, err := g.db.Query(`
SELECT node_id, display_name, role, state, joined_at FROM group_members WHERE group_id = ?`, groupID)
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
		)
		if err := rows.Scan(&nodeID, &dispName, &role, &state, &joinedAt); err != nil {
			return out
		}
		id, err := identity.ParseNodeID(nodeID)
		if err != nil {
			continue
		}
		m.NodeID = id
		m.DisplayName = nullString(dispName)
		m.Role = group.MemberRole(role)
		m.State = group.MemberState(state)
		m.JoinedAt = timeOf(joinedAt)
		out = append(out, m)
	}
	return out
}

func scanGroup(s rowScanner) (group.Group, error) {
	var (
		out       group.Group
		ownerID   string
		stateSig  sql.NullString
		createdAt int64
	)
	if err := s.Scan(&out.ID, &out.Name, &ownerID, &out.Epoch, &stateSig, &createdAt); err != nil {
		return out, err
	}
	if id, err := identity.ParseNodeID(ownerID); err == nil {
		out.OwnerID = id
	}
	out.StateSig = unb64(nullString(stateSig))
	out.CreatedAt = timeOf(createdAt)
	return out, nil
}
