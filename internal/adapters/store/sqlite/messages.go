package sqlite

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
)

// Messages 实现 ports.ChatStore（消息 + outbox 的原子写入）。
type Messages struct{ db *DB }

var _ ports.ChatStore = (*Messages)(nil)

// NewMessages 构造消息仓储。
func NewMessages(db *DB) *Messages { return &Messages{db: db} }

const messageColumns = `msg_id, conv_id, sender_id, direction, content, msg_type,
                        file_id, sent_at, recv_at, state`

// Append 追加消息（不判重；仍保证会话行存在以满足外键）。
func (m *Messages) Append(msg message.Message) error {
	return m.db.Write(func(tx *sql.Tx) error {
		if err := upsertConversation(tx, msg); err != nil {
			return err
		}
		return insertMessage(tx, msg)
	})
}

// AppendIfAbsent 幂等写入：msg_id 已存在则 inserted=false。
func (m *Messages) AppendIfAbsent(msg message.Message) (bool, error) {
	var inserted bool
	err := m.db.Write(func(tx *sql.Tx) error {
		if err := upsertConversation(tx, msg); err != nil {
			return err
		}
		res, err := tx.Exec(`
INSERT INTO messages(msg_id, conv_id, sender_id, direction, content, msg_type, file_id, sent_at, recv_at, state)
VALUES(?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(msg_id) DO NOTHING`,
			msg.MsgID, msg.ConvID, msg.SenderID.String(), string(msg.Direction), msg.Content,
			msgTypeOf(msg), nullFileID(msg), msOf(msg.SentAt), nullableMs(msg.RecvAt), string(msg.State),
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		inserted = n > 0
		return nil
	})
	return inserted, err
}

// Get 按 msg_id 读取消息。
func (m *Messages) Get(msgID string) (message.Message, bool) {
	row := m.db.QueryRow(`SELECT `+messageColumns+` FROM messages WHERE msg_id = ?`, msgID)
	out, err := scanMessage(row)
	if err != nil {
		return message.Message{}, false
	}
	return out, true
}

// Latest 按会话返回最近 limit 条（新→旧）；before 为毫秒时间戳游标。
func (m *Messages) Latest(convID string, limit int, before int64) ([]message.Message, error) {
	q := `SELECT ` + messageColumns + ` FROM messages WHERE conv_id = ?`
	args := []any{convID}
	if before > 0 {
		q += " AND sent_at < ?"
		args = append(args, before)
	}
	q += " ORDER BY sent_at DESC, id DESC"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := m.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: latest messages: %w", err)
	}
	defer rows.Close()

	out := make([]message.Message, 0, 16)
	for rows.Next() {
		item, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// MarkDelivered 置为已送达（不删 outbox；Deliver 会同时处理）。
func (m *Messages) MarkDelivered(msgID string) error {
	return m.db.Write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE messages SET state = 'delivered' WHERE msg_id = ?`, msgID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return fmt.Errorf("sqlite: message %s not found", msgID)
		}
		return nil
	})
}

// Enqueue 入队一条待确认消息。
func (m *Messages) Enqueue(msgID, convID string, nextTry time.Time) error {
	return m.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
INSERT INTO outbox(msg_id, conv_id, attempts, next_try_at, created_at)
VALUES(?,?,0,?,?)
ON CONFLICT(msg_id) DO UPDATE SET next_try_at = excluded.next_try_at`,
			msgID, convID, msOf(nextTry), time.Now().UnixMilli())
		return err
	})
}

// Due 返回到期的 outbox 记录。
func (m *Messages) Due(now time.Time, limit int) ([]ports.OutboxEntry, error) {
	q := `SELECT msg_id, conv_id, attempts, next_try_at FROM outbox
	      WHERE next_try_at <= ? ORDER BY next_try_at`
	args := []any{msOf(now)}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	return m.queryOutbox(q, args...)
}

// Delete 删除 outbox 记录。
func (m *Messages) Delete(msgID string) error {
	return m.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM outbox WHERE msg_id = ?`, msgID)
		return err
	})
}

// BumpAttempt 增加重试次数并设置下次重试时间。
func (m *Messages) BumpAttempt(msgID string, nextTry time.Time) error {
	return m.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE outbox SET attempts = attempts + 1, next_try_at = ? WHERE msg_id = ?`,
			msOf(nextTry), msgID,
		)
		return err
	})
}

// ListByConv 返回某会话的全部待确认消息。
func (m *Messages) ListByConv(convID string) ([]ports.OutboxEntry, error) {
	return m.queryOutbox(
		`SELECT msg_id, conv_id, attempts, next_try_at FROM outbox WHERE conv_id = ? ORDER BY next_try_at`,
		convID,
	)
}

// AppendOutgoing 单事务写入出站消息与 outbox（ADR-004 的硬要求）。
func (m *Messages) AppendOutgoing(msg message.Message, nextTry time.Time) error {
	if msg.State == "" {
		msg.State = message.StatePending
	}
	return m.db.Write(func(tx *sql.Tx) error {
		if err := upsertConversation(tx, msg); err != nil {
			return err
		}
		if _, err := tx.Exec(`
INSERT INTO messages(msg_id, conv_id, sender_id, direction, content, msg_type, file_id, sent_at, recv_at, state)
VALUES(?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(msg_id) DO NOTHING`,
			msg.MsgID, msg.ConvID, msg.SenderID.String(), string(msg.Direction), msg.Content,
			msgTypeOf(msg), nullFileID(msg), msOf(msg.SentAt), nullableMs(msg.RecvAt), string(msg.State),
		); err != nil {
			return err
		}
		_, err := tx.Exec(`
INSERT INTO outbox(msg_id, conv_id, attempts, next_try_at, created_at)
VALUES(?,?,0,?,?)
ON CONFLICT(msg_id) DO UPDATE SET next_try_at = excluded.next_try_at`,
			msg.MsgID, msg.ConvID, msOf(nextTry), time.Now().UnixMilli(),
		)
		return err
	})
}

// Deliver 单事务置为已送达并删除 outbox。
func (m *Messages) Deliver(msgID string) error {
	return m.db.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE messages SET state = 'delivered' WHERE msg_id = ?`, msgID); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM outbox WHERE msg_id = ?`, msgID)
		return err
	})
}

func (m *Messages) queryOutbox(q string, args ...any) ([]ports.OutboxEntry, error) {
	rows, err := m.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ports.OutboxEntry, 0, 8)
	for rows.Next() {
		var (
			e       ports.OutboxEntry
			nextTry int64
		)
		if err := rows.Scan(&e.MsgID, &e.ConvID, &e.Attempts, &nextTry); err != nil {
			return nil, err
		}
		e.NextTryAt = timeOf(nextTry)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

// upsertConversation 保证会话行存在（messages.conv_id 的外键父行）。
//
// 顺带维护 last_msg_id / last_time，让 UI 的会话列表可以直接查询。
func upsertConversation(tx *sql.Tx, msg message.Message) error {
	kind := "group"
	if message.IsDirectConv(msg.ConvID) {
		kind = "direct"
	}
	_, err := tx.Exec(`
INSERT INTO conversations(conv_id, kind, title, last_msg_id, last_time)
VALUES(?,?,?,?,?)
ON CONFLICT(conv_id) DO UPDATE SET
  last_msg_id = excluded.last_msg_id,
  last_time   = excluded.last_time`,
		msg.ConvID, kind, msg.ConvID, msg.MsgID, msOf(msg.SentAt),
	)
	return err
}

func insertMessage(tx *sql.Tx, msg message.Message) error {
	_, err := tx.Exec(`
INSERT INTO messages(msg_id, conv_id, sender_id, direction, content, msg_type, file_id, sent_at, recv_at, state)
VALUES(?,?,?,?,?,?,?,?,?,?)`,
		msg.MsgID, msg.ConvID, msg.SenderID.String(), string(msg.Direction), msg.Content,
		msgTypeOf(msg), nullFileID(msg), msOf(msg.SentAt), nullableMs(msg.RecvAt), string(msg.State),
	)
	return err
}

func msgTypeOf(msg message.Message) string {
	if msg.MsgType == "" {
		return string(message.MsgTypeText)
	}
	return string(msg.MsgType)
}

func nullFileID(msg message.Message) any {
	if msg.FileID == "" {
		return nil
	}
	return msg.FileID
}

func nullableMs(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func scanMessage(s rowScanner) (message.Message, error) {
	var (
		out       message.Message
		senderID  string
		direction string
		msgType   string
		fileID    sql.NullString
		sentAt    int64
		recvAt    sql.NullInt64
		state     string
	)
	if err := s.Scan(&out.MsgID, &out.ConvID, &senderID, &direction, &out.Content,
		&msgType, &fileID, &sentAt, &recvAt, &state); err != nil {
		return out, err
	}

	// sender_id 在正常情况下都是合法 NodeID；解析失败时保留零值而非报错，
	// 以免一条脏数据让整个历史查询失败。
	if id, err := identity.ParseNodeID(senderID); err == nil {
		out.SenderID = id
	}
	out.Direction = message.Direction(direction)
	out.MsgType = message.MsgType(msgType)
	out.FileID = nullString(fileID)
	out.SentAt = timeOf(sentAt)
	out.RecvAt = timeOf(nullInt64(recvAt))
	out.State = message.State(state)
	return out, nil
}
