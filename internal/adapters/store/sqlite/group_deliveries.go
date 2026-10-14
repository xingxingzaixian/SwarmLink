package sqlite

import (
	"database/sql"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
)

// GroupDeliveries 实现 ports.GroupDeliveryRepo（群消息的按成员投递跟踪）。
type GroupDeliveries struct{ db *DB }

var _ ports.GroupDeliveryRepo = (*GroupDeliveries)(nil)

// NewGroupDeliveries 构造投递跟踪仓储。
func NewGroupDeliveries(db *DB) *GroupDeliveries { return &GroupDeliveries{db: db} }

// EnsureDeliveries 为一条群消息登记全体目标成员（幂等；已存在的行保持原状）。
//
// 用 DO NOTHING 而不是 DO UPDATE：一次补投或重启恢复会重复调用本方法，
// 若覆盖 acked_at，就把「已经送到的人」重新标成未送达 —— 那会引发
// 对同一条消息的无限重发。
func (g *GroupDeliveries) EnsureDeliveries(msgID, groupID string, members []identity.NodeID) error {
	if len(members) == 0 {
		return nil
	}
	return g.db.Write(func(tx *sql.Tx) error {
		for _, m := range members {
			if m.IsZero() {
				continue
			}
			if _, err := tx.Exec(`
INSERT INTO group_msg_deliveries(msg_id, group_id, member_id, acked_at)
VALUES(?,?,?,NULL)
ON CONFLICT(msg_id, member_id) DO NOTHING`,
				msgID, groupID, m.String(),
			); err != nil {
				return err
			}
		}
		return nil
	})
}

// AckDelivery 标记某成员已送达（幂等）。成员不在记录里时是一条空操作。
func (g *GroupDeliveries) AckDelivery(msgID string, memberID identity.NodeID) error {
	return g.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE group_msg_deliveries SET acked_at = ?
			 WHERE msg_id = ? AND member_id = ? AND acked_at IS NULL`,
			time.Now().UnixMilli(), msgID, memberID.String(),
		)
		return err
	})
}

// PendingMembers 返回尚未送达的成员。
func (g *GroupDeliveries) PendingMembers(msgID string) ([]identity.NodeID, error) {
	rows, err := g.db.Query(
		`SELECT member_id FROM group_msg_deliveries WHERE msg_id = ? AND acked_at IS NULL`,
		msgID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []identity.NodeID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return out, err
		}
		if id, err := identity.ParseNodeID(raw); err == nil {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// StatsByConv 返回会话内每条群消息的 (已送达, 成员总数)。
func (g *GroupDeliveries) StatsByConv(convID string) (map[string]ports.DeliveryStat, error) {
	// group_id 即群会话的 conv_id（群 ID 是纯 hex，与单聊含冒号的形态不相交）。
	rows, err := g.db.Query(`
SELECT msg_id,
       COUNT(*)                                        AS total,
       SUM(CASE WHEN acked_at IS NOT NULL THEN 1 ELSE 0 END) AS acked
FROM group_msg_deliveries WHERE group_id = ? GROUP BY msg_id`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]ports.DeliveryStat)
	for rows.Next() {
		var (
			msgID string
			total int
			acked sql.NullInt64
		)
		if err := rows.Scan(&msgID, &total, &acked); err != nil {
			return out, err
		}
		out[msgID] = ports.DeliveryStat{Acked: int(acked.Int64), Total: total}
	}
	return out, rows.Err()
}

// DeleteByMsg 清掉一条消息的全部投递记录。
func (g *GroupDeliveries) DeleteByMsg(msgID string) error {
	return g.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM group_msg_deliveries WHERE msg_id = ?`, msgID)
		return err
	})
}

// DeleteByGroup 清掉某群的全部投递记录（解散时停止一切补投）。
func (g *GroupDeliveries) DeleteByGroup(groupID string) error {
	return g.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM group_msg_deliveries WHERE group_id = ?`, groupID)
		return err
	})
}

// DeleteGroupMember 清掉某群某成员的全部投递记录（成员被移出群时用）。
func (g *GroupDeliveries) DeleteGroupMember(groupID string, memberID identity.NodeID) error {
	return g.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`DELETE FROM group_msg_deliveries WHERE group_id = ? AND member_id = ?`,
			groupID, memberID.String(),
		)
		return err
	})
}
