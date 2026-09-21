// Package wails 是 L4 入站适配器：把应用层能力暴露给 Wails 绑定的前端。
//
// 关键设计：本包【不 import Wails】。
//   - 服务只是普通结构体，方法返回 JSON 友好的 DTO；
//   - 事件通过 Emitter 接口输出，Wails 的具体桥接在根目录 main_wails.go 里注入。
//
// 这样做的收益：服务层可以在没有 Wails 工具链的环境下编译与单测，
// 而 Wails beta 期 API 波动只影响一个薄薄的装配文件。
package wails

import (
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
)

// MessageDTO 是消息的对外表示。
type MessageDTO struct {
	MsgID     string `json:"msgId"`
	ConvID    string `json:"convId"`
	SenderID  string `json:"senderId"`
	Direction string `json:"direction"`
	Content   string `json:"content"`
	MsgType   string `json:"msgType"`
	FileID    string `json:"fileId,omitempty"`
	State     string `json:"state"`
	SentAt    int64  `json:"sentAt"`
	RecvAt    int64  `json:"recvAt,omitempty"`
}

// ToMessageDTO 转换领域消息。
func ToMessageDTO(m message.Message) MessageDTO {
	return MessageDTO{
		MsgID:     m.MsgID,
		ConvID:    m.ConvID,
		SenderID:  m.SenderID.String(),
		Direction: string(m.Direction),
		Content:   m.Content,
		MsgType:   string(m.MsgType),
		FileID:    m.FileID,
		State:     string(m.State),
		SentAt:    m.SentAt.UnixMilli(),
		RecvAt:    m.RecvAt.UnixMilli(),
	}
}

// ConversationDTO 是会话列表项。
type ConversationDTO struct {
	ConvID   string `json:"convId"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	PeerID   string `json:"peerId,omitempty"`
	State    string `json:"state,omitempty"`
	IsOnline bool   `json:"isOnline"`
	LastTime int64  `json:"lastTime"`
}

// PeerDTO 是节点列表项。
//
// 注意：UI 必须同时展示「节点在线」（announce 维护）与「连接可用」（TCP 维护），
// 否则用户会看到「在线但发不出消息」而困惑（ADR-009 的代价之一）。
type PeerDTO struct {
	NodeID      string `json:"nodeId"`
	ShortID     string `json:"shortId"`
	DisplayName string `json:"displayName"`
	State       string `json:"state"`
	LastAddr    string `json:"lastAddr"`
	Subnet      string `json:"subnet"`
	Source      string `json:"source"`
	LastSeen    int64  `json:"lastSeen"`
	Online      bool   `json:"online"`
	Connected   bool   `json:"connected"`
}

// ToPeerDTO 转换领域节点；connected 由连接层提供。
func ToPeerDTO(p peer.Peer, connected bool) PeerDTO {
	short := p.NodeID.String()
	if len(short) > 8 {
		short = short[:8]
	}
	name := p.DisplayName
	if name == "" {
		name = short
	}
	return PeerDTO{
		NodeID:      p.NodeID.String(),
		ShortID:     short,
		DisplayName: name,
		State:       string(p.State),
		LastAddr:    p.LastAddr,
		Subnet:      p.Subnet,
		Source:      p.Source,
		LastSeen:    p.LastSeen.UnixMilli(),
		Online:      p.State == peer.StateOnline,
		Connected:   connected,
	}
}

// TransferDTO 是传输任务项。
type TransferDTO struct {
	JobID       string  `json:"jobId"`
	PeerID      string  `json:"peerId"`
	FileName    string  `json:"fileName"`
	FileSize    int64   `json:"fileSize"`
	Direction   string  `json:"direction"`
	LocalPath   string  `json:"localPath"`
	Completed   int64   `json:"completed"`
	TotalChunks int     `json:"totalChunks"`
	Percent     float64 `json:"percent"`
	Status      string  `json:"status"`
	Error       string  `json:"error,omitempty"`
	UpdatedAt   int64   `json:"updatedAt"`
}

// ToTransferDTO 转换领域任务。
func ToTransferDTO(j transfer.Job) TransferDTO {
	return TransferDTO{
		JobID:       j.JobID,
		PeerID:      j.PeerID.String(),
		FileName:    j.FileName,
		FileSize:    j.FileSize,
		Direction:   string(j.Direction),
		LocalPath:   j.LocalPath,
		Completed:   j.Completed,
		TotalChunks: j.TotalChunks,
		Percent:     j.Percent(),
		Status:      string(j.Status),
		Error:       j.Error,
		UpdatedAt:   j.UpdatedAt.UnixMilli(),
	}
}

// GroupMemberDTO 是群成员项。
type GroupMemberDTO struct {
	NodeID      string `json:"nodeId"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
	State       string `json:"state"`
}

// GroupDTO 是群项。
type GroupDTO struct {
	GroupID   string           `json:"groupId"`
	Name      string           `json:"name"`
	OwnerID   string           `json:"ownerId"`
	Epoch     int64            `json:"epoch"`
	IsOwner   bool             `json:"isOwner"`
	Members   []GroupMemberDTO `json:"members"`
	CreatedAt int64            `json:"createdAt"`
}

// SelfDTO 是本机身份。
type SelfDTO struct {
	NodeID      string `json:"nodeId"`
	DisplayName string `json:"displayName"`
	TCPPort     int    `json:"tcpPort"`
	UDPPort     int    `json:"udpPort"`
	Subnet      string `json:"subnet"`
}

// DiagnosticsDTO 是诊断面板数据。
type DiagnosticsDTO struct {
	PeersTotal       int       `json:"peersTotal"`
	PeersOnline      int       `json:"peersOnline"`
	ActiveSessions   int       `json:"activeSessions"`
	ActiveTransfers  int       `json:"activeTransfers"`
	SeedCount        int       `json:"seedCount"`
	SeedDetails      []SeedDTO `json:"seedDetails"`
	InterfaceSummary []string  `json:"interfaceSummary"`
}

// SeedDTO 是种子状态项（含退避信息）。
type SeedDTO struct {
	Addr      string `json:"addr"`
	FailCount int    `json:"failCount"`
	NodeID    string `json:"nodeId"`
	TCPPort   int    `json:"tcpPort"`
	Subnet    string `json:"subnet"`
}

// SettingsDTO 是设置项。
type SettingsDTO struct {
	DisplayName     string   `json:"displayName"`
	DownloadDir     string   `json:"downloadDir"`
	AutoOpen        bool     `json:"autoOpen"`
	TCPPort         int      `json:"tcpPort"`
	UDPPort         int      `json:"udpPort"`
	InterfaceMode   string   `json:"interfaceMode"`
	AllowInterfaces []string `json:"allowInterfaces"`
	DenyInterfaces  []string `json:"denyInterfaces"`
	Seeds           []string `json:"seeds"`
	SeedRefreshSec  int      `json:"seedRefreshSec"`
	SeedsPerRefresh int      `json:"seedsPerRefresh"`
	MaxConcurrent   int      `json:"maxConcurrent"`
	ChunkSize       int64    `json:"chunkSize"`
	WindowSize      int      `json:"windowSize"`
	MaxActiveConns  int      `json:"maxActiveConns"`
	IdleTimeoutSec  int      `json:"idleTimeoutSec"`
	RequireAuth     bool     `json:"requireAuth"`
	Encryption      string   `json:"encryption"`
}

// SelfCheckDTO 是部署前置条件（P-1 / P-2）自检结果。
//
// Detail 字段是【给人读的结论】而不是错误码：这两条前提只有部署者能修
// （改网段规划 / 开三层路由），软件侧无能为力，因此必须说清「怎么修」。
type SelfCheckDTO struct {
	Performed bool      `json:"performed"`
	SeedCount int       `json:"seedCount"`
	Learned   int       `json:"learned"`
	P1OK      bool      `json:"p1OK"`
	P1Detail  string    `json:"p1Detail"`
	P2Overlap bool      `json:"p2Overlap"`
	P2Detail  string    `json:"p2Detail"`
	Seeds     []SeedDTO `json:"seeds"`
}
