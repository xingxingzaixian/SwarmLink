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
	// MsgTypeFile 的 content 是 FileMessageContent 的 JSON 信封（job_id + file_name），
	// 不是裸 job_id —— 预览/渲染都必须先解析它，否则界面上就是一段 JSON。
	MsgTypeFile MsgType = "file"
	// MsgTypeFileFailed 表示文件传输失败
	MsgTypeFileFailed MsgType = "file_failed"
	// MsgTypeFolder 的 content 是 FolderMessageContent 的 JSON 信封。
	//
	// 与 MsgTypeFile 同构：这条消息只让两端聊天框出现【一张】文件夹卡片，
	// 传输本身由 TransferApp 的独立通道完成 —— 一个几百个文件的文件夹
	// 不能往聊天框里塞几百张卡片。
	MsgTypeFolder MsgType = "folder"
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

// FileFailedContent 文件传输失败的内容。
//
// 字段名必须是 snake_case：这份 JSON 会原样存进消息、并投递给对端，
// 由前端解析成「文件发送失败」卡片。曾经没有 tag，于是序列化出
// JobID/FileName/Reason —— 前端一个字段都读不到，只能显示成一段裸 JSON。
type FileFailedContent struct {
	JobID    string `json:"job_id"`    // 失败的任务ID
	FileName string `json:"file_name"` // 文件名
	Reason   string `json:"reason"`    // 失败原因
}

// FileMessageContent 文件消息内容。
//
// 【不】含发送方的本地路径：这条消息会投递给对端，而本地路径在对方那里
// 毫无用处（那是发送方磁盘上的位置），带上它只是把隐私送出网。
// 真正的传输由 TransferApp 的独立通道完成，与本消息无关 —— 本消息只负责
// 让两端的聊天框都出现一张文件卡片。
type FileMessageContent struct {
	JobID    string `json:"job_id"`    // 传输任务ID
	FileName string `json:"file_name"` // 文件名
}

// FolderMessageContent 是文件夹消息的内容。
//
// 与 FileMessageContent 一样【不】含发送方本地路径：这条消息会投递给对端。
//
// file_count / total_size 由发送端枚举后给出，接收端直接用：让接收端自己数，
// 两端就会出现两个不同的「共 N 个文件」（接收端可能还没收全）。
// skipped 是发送端跳过的符号链接数 —— 用户需要知道「少了几项」。
type FolderMessageContent struct {
	BatchID    string `json:"batch_id"`    // 批次ID（点击/取消都用它）
	FolderName string `json:"folder_name"` // 文件夹名
	FileCount  int    `json:"file_count"`  // 条目数（含空目录）
	TotalSize  int64  `json:"total_size"`  // 总字节数
	Skipped    int    `json:"skipped"`     // 被跳过的项（符号链接）
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
