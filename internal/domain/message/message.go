// Package message 定义消息实体、ID 生成与排序/去重规则。
package message

import (
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
)

// Direction 表示消息方向。
type Direction string

const (
	DirectionIn  Direction = "in"
	DirectionOut Direction = "out"
)

// State 是消息送达状态（对应 messages.state）。
type State string

const (
	StatePending   State = "pending"
	StateSent      State = "sent"
	StateDelivered State = "delivered"
	StateFailed    State = "failed"
)

// MsgType 是消息类型（text 之外为后续扩展预留）。
type MsgType string

const (
	MsgTypeText MsgType = "text"
)

// Message 是单聊/群聊共用的消息实体。
type Message struct {
	MsgID     string
	ConvID    string
	SenderID  identity.NodeID
	Direction Direction
	Content   string
	MsgType   MsgType
	FileID    string
	SentAt    time.Time
	RecvAt    time.Time
	State     State
}

// NewID 生成 UUIDv7 —— 时间有序，便于 UI 排序与游标分页（ADR-004）。
func NewID() string {
	id, err := uuid.NewV7()
	if err != nil {
		// NewV7 仅在底层随机源失败时报错；退化为 V4 以保证消息仍可发送。
		return uuid.NewString()
	}
	return id.String()
}

// DirectConvID 返回单聊会话 ID（= 对端 NodeID 的 hex）。
func DirectConvID(p identity.NodeID) string { return p.String() }

// GroupConvID 返回群聊会话 ID（= group_id）。
func GroupConvID(groupID string) string { return groupID }

// SortInPlace 按展示顺序排序：sent_at 升序，同 sent_at 按 msg_id 二次排序。
// UUIDv7 本身时间有序，因此二次排序结果稳定（ADR-004）。
func SortInPlace(ms []Message) {
	sort.SliceStable(ms, func(i, j int) bool {
		if !ms[i].SentAt.Equal(ms[j].SentAt) {
			return ms[i].SentAt.Before(ms[j].SentAt)
		}
		return ms[i].MsgID < ms[j].MsgID
	})
}
