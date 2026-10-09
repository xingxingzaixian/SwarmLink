package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 会话「清空点」的持久化。
//
// 只记一行「早于这个时刻的消息不显示」—— 消息本身一条不动，撤销清空
// 就是把这一行删掉。因此这里的写错方式只有两种：读成 0（消息全回来）
// 或读成未来（消息全不见）。前者是"清空没生效"，后者严重得多，
// 所以 ConvClearedAt 对「没有记录」与「读失败」区分得很清楚。

// SetConvCleared 写入/覆盖会话清空点（毫秒时间戳）。
//
// 覆盖是必须的语义：用户清空后又聊几句再清空一次，边界要往前推。
func (m *Messages) SetConvCleared(convID string, at time.Time) error {
	return m.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
INSERT INTO conv_clears(conv_id, cleared_at) VALUES(?,?)
ON CONFLICT(conv_id) DO UPDATE SET cleared_at = excluded.cleared_at`,
			convID, at.UnixMilli())
		if err != nil {
			return fmt.Errorf("sqlite: set conv cleared: %w", err)
		}
		return nil
	})
}

// ConvClearedAt 读取清空点；从未清空时返回 ok=false（这不是错误）。
func (m *Messages) ConvClearedAt(convID string) (time.Time, bool, error) {
	var ms int64
	err := m.db.QueryRow(`SELECT cleared_at FROM conv_clears WHERE conv_id = ?`, convID).Scan(&ms)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("sqlite: read conv cleared: %w", err)
	}
	return time.UnixMilli(ms), true, nil
}

// RemoveConvClear 撤销清空（幂等：没有记录也返回 nil）。
func (m *Messages) RemoveConvClear(convID string) error {
	return m.db.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM conv_clears WHERE conv_id = ?`, convID); err != nil {
			return fmt.Errorf("sqlite: remove conv clear: %w", err)
		}
		return nil
	})
}
