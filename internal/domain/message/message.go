// Package message 定义消息实体、ID 生成与排序/去重规则。
package message

import (
	"sort"
	"strings"
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

// MsgType 是消息类型。
type MsgType string

const (
	MsgTypeText MsgType = "text"
	// MsgTypeImage 的 content 是 InlineImage 的 JSON 信封（见 inline_image.go）。
	MsgTypeImage MsgType = "image"
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

// DirectConvID 返回单聊会话 ID。
//
// 必须【对称】：A 与 B 对同一段会话必须算出同一个 conv_id，否则两端会各建
// 一条「半条会话」，历史记录、未读与去重都会分裂。
// 因此取两个 NodeID 排序后拼接，而不是取「对端 ID」。
func DirectConvID(a, b identity.NodeID) string {
	if b.Less(a) {
		a, b = b, a
	}
	return a.String() + ":" + b.String()
}

// GroupConvID 返回群聊会话 ID（= group_id）。
func GroupConvID(groupID string) string { return groupID }

// IsDirectConv 判断 conv_id 是否为单聊（形如 "nodeid:nodeid"）。
// 群聊 conv_id 是 group_id（32 位 hex，不含冒号），因此不会误判。
func IsDirectConv(convID string) bool {
	parts := strings.Split(convID, ":")
	if len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		if _, err := identity.ParseNodeID(p); err != nil {
			return false
		}
	}
	return true
}

// DirectPeer 从对称 conv_id 中解析出【对端】NodeID。
//
// outbox 重发需要它：待确认记录只存了 conv_id，而重发必须知道拨给谁。
func DirectPeer(convID string, self identity.NodeID) (identity.NodeID, bool) {
	if !IsDirectConv(convID) {
		return identity.NodeID{}, false
	}
	for _, p := range strings.Split(convID, ":") {
		id, err := identity.ParseNodeID(p)
		if err != nil {
			continue
		}
		if id != self {
			return id, true
		}
	}
	return identity.NodeID{}, false
}

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
