package protocol

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// HelloDomain 是握手签名的域分隔串（架构书 4.2）。
// 作用：防止签名被挪用到其他协议上下文（例如被当作文件块签名重放）。
const HelloDomain = "swarmlink-hello"

// ---------------------------------------------------------------------------
// 握手转录
// ---------------------------------------------------------------------------

// HelloTranscript 返回待签名的转录哈希：
//
//	H("swarmlink-hello" || nonce_A || nonce_B || pubKey_A || pubKey_B)
//
// nonce_A 由 A 生成、nonce_B 由 B 生成，任一方都无法单方面构造可重放的转录
// —— 这正是防重放的关键（只签自己发的 nonce 是不够的，攻击者可原样重放整个握手）。
func HelloTranscript(nonceA, nonceB []byte, pubA, pubB ed25519.PublicKey) []byte {
	h := sha256.New()
	h.Write([]byte(HelloDomain))
	h.Write(nonceA)
	h.Write(nonceB)
	h.Write(pubA)
	h.Write(pubB)
	return h.Sum(nil)
}

// ---------------------------------------------------------------------------
// 控制报文（JSON）
// ---------------------------------------------------------------------------

// Hello 是握手第一段。
type Hello struct {
	ProtoMin     int    `json:"proto_min"`
	ProtoMax     int    `json:"proto_max"`
	Caps         uint32 `json:"caps"`
	MaxFrameSize int    `json:"max_frame_size"`
	MaxChunkSize int    `json:"max_chunk_size"`
	WindowSize   int    `json:"window_size"`
	NodeID       string `json:"node_id"`
	PublicKey    string `json:"public_key"` // base64
	Nonce        string `json:"nonce"`      // hex(32B)
	DisplayName  string `json:"display_name"`
	TCPPort      uint16 `json:"tcp_port"`
	UDPPort      uint16 `json:"udp_port"`
}

// HelloAck 是握手第二段（携带 sig_B，让 A 立即知道对面确实持有私钥）。
type HelloAck struct {
	ProtoChosen int    `json:"proto_chosen"`
	CapsCommon  uint32 `json:"caps_common"`
	NodeID      string `json:"node_id"`
	PublicKey   string `json:"public_key"`
	Nonce       string `json:"nonce"`
	Sig         string `json:"sig"` // base64
	DisplayName string `json:"display_name"`
	TCPPort     uint16 `json:"tcp_port"`
	UDPPort     uint16 `json:"udp_port"`
}

// Auth 是握手第三段（sig_A）。
type Auth struct {
	Sig string `json:"sig"`
}

// AuthOK 是握手完成。
type AuthOK struct{}

// ErrorPayload 是协议错误。
type ErrorPayload struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Chat 是单聊/群聊消息载荷。
type Chat struct {
	MsgID    string `json:"msg_id"`
	ConvID   string `json:"conv_id"`
	SenderID string `json:"sender_id"`
	Content  string `json:"content"`
	MsgType  string `json:"msg_type"`
	FileID   string `json:"file_id,omitempty"`
	SentAt   int64  `json:"sent_at"`
}

// ChatAck 是消息已落库确认。
type ChatAck struct {
	MsgID string `json:"msg_id"`
}

// Heartbeat 与 HeartbeatAck 用于 RTT 测量与保活。
type Heartbeat struct {
	Seq    uint64 `json:"seq"`
	SentAt int64  `json:"sent_at"`
}

// HeartbeatAck 回应心跳。
type HeartbeatAck struct {
	Seq    uint64 `json:"seq"`
	SentAt int64  `json:"sent_at"`
}

// PeerListReq 请求节点目录；携带本地 list_epoch 以省流。
type PeerListReq struct {
	ListEpoch uint64 `json:"list_epoch"`
}

// PeerEntry 是目录中的一条节点记录。
type PeerEntry struct {
	NodeID      string `json:"node_id"`
	DisplayName string `json:"display_name"`
	PublicKey   string `json:"public_key"`
	Addr        string `json:"addr"`
	Subnet      string `json:"subnet"`
	Caps        uint32 `json:"caps"`
	ProtoVer    int    `json:"proto_ver"`
}

// PeerListResp 返回全量目录（epoch 相同则为空响应）。
type PeerListResp struct {
	Epoch   uint64      `json:"epoch"`
	Entries []PeerEntry `json:"entries"`
}

// ---------------------------------------------------------------------------
// 文件传输报文
// ---------------------------------------------------------------------------

// FileMeta 是文件元数据。chunk_hashes 始终发送全量（简化 > 省流，ADR-005）。
type FileMeta struct {
	JobID       string   `json:"job_id"`
	FileID      string   `json:"file_id"`
	FileName    string   `json:"file_name"`
	FileSize    int64    `json:"file_size"`
	ChunkSize   int64    `json:"chunk_size"`
	TotalChunks int      `json:"total_chunks"`
	ChunkHashes []string `json:"chunk_hashes"`
}

// FileMetaAck 返回接收方已完成的块位图（base64）。
type FileMetaAck struct {
	JobID           string `json:"job_id"`
	CompletedBitmap string `json:"completed_bitmap"`
	CompletedChunks int    `json:"completed_chunks"`
}

// FileChunkAck 周期性回传全量位图（非累积 ACK）。
type FileChunkAck struct {
	JobID           string `json:"job_id"`
	Bitmap          string `json:"bitmap"`
	CompletedChunks int    `json:"completed_chunks"`
}

// FileDone 表示所有块已发送。
type FileDone struct {
	JobID string `json:"job_id"`
}

// FileDoneAck 是整体校验结果。
type FileDoneAck struct {
	JobID string `json:"job_id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// 群组报文
// ---------------------------------------------------------------------------

// GroupMemberWire 是群成员的线上表示。
type GroupMemberWire struct {
	NodeID      string `json:"node_id"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	State       string `json:"state"`
	JoinedAt    int64  `json:"joined_at"`
}

// GroupMeta 传播群状态（owner 签名）。
type GroupMeta struct {
	GroupID   string            `json:"group_id"`
	Name      string            `json:"name"`
	OwnerID   string            `json:"owner_id"`
	Epoch     int64             `json:"epoch"`
	StateSig  string            `json:"state_sig"` // base64
	Members   []GroupMemberWire `json:"members"`
	CreatedAt int64             `json:"created_at"`
}

// GroupMembersReq 拉取成员快照。
type GroupMembersReq struct {
	GroupID    string `json:"group_id"`
	KnownEpoch int64  `json:"known_epoch"`
}

// GroupMembersResp 返回成员快照。
type GroupMembersResp struct {
	Meta GroupMeta `json:"meta"`
}

// GroupMsg 是群消息（conv_id = group_id）。
//
// MsgType / FileID 与单聊的 Chat 对齐：群里的图片消息同样需要声明类型，
// 否则接收端只能当纯文本渲染。新增字段对旧版本是「忽略未知字段」，向后兼容。
type GroupMsg struct {
	MsgID    string `json:"msg_id"`
	GroupID  string `json:"group_id"`
	SenderID string `json:"sender_id"`
	Content  string `json:"content"`
	MsgType  string `json:"msg_type,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	SentAt   int64  `json:"sent_at"`
}

// ---------------------------------------------------------------------------
// 编解码助手
// ---------------------------------------------------------------------------

// EncodeJSON 序列化控制报文体。
func EncodeJSON(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("protocol: encode json: %w", err)
	}
	return b, nil
}

// DecodeJSON 反序列化控制报文体。
func DecodeJSON(data []byte, v any) error {
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("protocol: decode json: %w", err)
	}
	return nil
}

// NonceHex 编码 nonce 为 hex。
func NonceHex(b []byte) string { return hex.EncodeToString(b) }

// ParseNonceHex 解析 hex nonce。
func ParseNonceHex(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("protocol: parse nonce: %w", err)
	}
	return b, nil
}
