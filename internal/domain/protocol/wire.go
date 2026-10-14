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

// PeerListReq 请求节点目录。
//
// ListEpoch 是历史遗留的省流字段：服务端当前【忽略】它，无条件返回全量目录。
// 原因见 DiscoveryApp.HandlePeerListReq —— 本地 epoch 无人递增（BumpEpoch 没有
// 调用点），一旦按它比对就会让同一对 (本机, 种子) 从第二轮起永远拿到空目录。
// 字段保留在线上格式里只是为了不改协议版本；新代码不要依赖它，也不要在没有
// 定下「谁负责 bump」之前恢复这个判定。
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
//
// 后三个字段是文件夹批次专用的（单文件传输时为零值，且因 omitempty
// 【不会出现在报文里】—— 不能让单文件传输的线上体积因为功能扩充而变大）。
// JSON 是加法：老对端读到这三个 key 会忽略，我们读老对端的报文则得到零值。
// 但"能忽略"不等于"能正确处理"——老接收端会把整批文件平铺到下载目录，
// 因此发送前必须查能力位（peer.CapsFolderTransfer），不支持就拒绝发送。
type FileMeta struct {
	JobID       string   `json:"job_id"`
	FileID      string   `json:"file_id"`
	FileName    string   `json:"file_name"`
	FileSize    int64    `json:"file_size"`
	ChunkSize   int64    `json:"chunk_size"`
	TotalChunks int      `json:"total_chunks"`
	ChunkHashes []string `json:"chunk_hashes"`

	// BatchID 是批次标识（全局唯一，见 ports.TransferRepo.ListByBatch 的契约）。
	BatchID string `json:"batch_id,omitempty"`
	// RelPath 是相对文件夹根的路径，含根名（如 `照片/2024/a.jpg`）。
	// 接收端靠它逐段清洗后还原目录树。
	RelPath string `json:"rel_path,omitempty"`
	// IsDir 标记空目录条目：零字节、无分块，只为在接收端建出目录。
	IsDir bool `json:"is_dir,omitempty"`
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
//
// AdmitSeq/KickSeq 属于【群主签名块】，LeftSeq/LeftSig 属于【本人签名域】——
// 两块在同一条消息里携带，但各自独立认证、独立合并（见 ADR-012 Decision §5）。
// State 只是派生缓存，接收端应当用票号自行判定，不要信它。
type GroupMemberWire struct {
	NodeID      string `json:"node_id"`
	DisplayName string `json:"display_name,omitempty"` // 非权威提示
	Role        string `json:"role"`
	State       string `json:"state,omitempty"`
	JoinedAt    int64  `json:"joined_at"`

	AdmitSeq int64 `json:"admit_seq"`
	KickSeq  int64 `json:"kick_seq"`

	LeftSeq int64  `json:"left_seq,omitempty"`
	LeftSig string `json:"left_sig,omitempty"` // base64
}

// GroupMeta 传播群名册快照（owner 签名的名册块 + 各成员自签的 left 覆盖）。
//
// State 是群级状态（active/dissolved），它已进入群主签名域 —— 单独一个
// 字段而不是「把成员删光」：解散要能被成员明确识别并提示，而不是看起来像自己被踢。
type GroupMeta struct {
	GroupID string `json:"group_id"`
	Name    string `json:"name"`
	OwnerID string `json:"owner_id"`
	Epoch   int64  `json:"epoch"`
	State   string `json:"state,omitempty"`
	// RosterSig 是群主对「群级字段 + 每成员 admit/kick」的签名（base64）。
	RosterSig string            `json:"roster_sig"`
	MintSeq   int64             `json:"mint_seq"`
	Members   []GroupMemberWire `json:"members"`
	CreatedAt int64             `json:"created_at"`
	// RosterDigest 是名册完整状态摘要（base64）：回答「我是不是拿全了」。
	RosterDigest string `json:"roster_digest,omitempty"`
}

// GroupMembersReq 拉取成员快照。
type GroupMembersReq struct {
	GroupID    string `json:"group_id"`
	KnownEpoch int64  `json:"known_epoch"`
	// KnownDigest 是请求方当前的名册摘要（base64）。一致则响应方回空，
	// 省掉一次「其实什么都没变」的全量往返。
	KnownDigest string `json:"known_digest,omitempty"`
}

// GroupLeave 是【本人签名】的退出增量。
type GroupLeave struct {
	GroupID string `json:"group_id"`
	NodeID  string `json:"node_id"`
	LeftSeq int64  `json:"left_seq"`
	// Sig 是本人对 LeftSigningBytes(group_id, node_id, left_seq) 的签名（base64）。
	Sig string `json:"sig"`
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
// 文件共享报文（FR-14）
// ---------------------------------------------------------------------------
//
// 共享的发现不在这里：摘要随 ANNOUNCE / PEER_LIST 走（peer.Announcement.Shares）。
// 这一组只承载「要清单」与「要文件」两件事，都是请求-响应式的单次交互。
//
// 为什么不在通告里直接带完整清单：一个大目录的 item 列表可达数 MB，而通告是
// 45 秒一轮的广播、目录拉取还要逐条目转发 —— 把清单塞进去等于让每个节点的
// 每次心跳都背着全网的目录走（PRD RK-09）。

// ShareIndexReq 请求某个共享的完整文件清单。
type ShareIndexReq struct {
	ShareID string `json:"share_id"`
}

// ShareIndexItem 是清单里的一条。
type ShareIndexItem struct {
	// Path 是相对共享根的路径（斜杠分隔）。
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	// Hash 当前恒为空（发布时不预先算哈希，见 share.Item）。
	Hash  string `json:"hash,omitempty"`
	MTime int64  `json:"mtime,omitempty"`
}

// ShareIndexResp 返回清单；OK=false 时 Error 是给用户看的拒绝原因。
//
// 拒绝原因必须在线上回传而不是只在日志里：越权访问与「共享方已取消共享」
// 对请求方是两种完全不同的处置（前者要改可见性、后者要让对方重新发布），
// 而请求方看不到对方的日志。
type ShareIndexResp struct {
	OK      bool             `json:"ok"`
	Error   string           `json:"error,omitempty"`
	ShareID string           `json:"share_id"`
	Name    string           `json:"name"`
	Owner   string           `json:"owner,omitempty"`
	Items   []ShareIndexItem `json:"items,omitempty"`
}

// SharePullReq 请共享方把指定文件【推】给自己。
//
// 方向说明：拉取方主动，但数据方向仍然是「共享方 → 拉取方」，
// 因为文件在共享方手里 —— 复用现有的发送引擎（FILE_META 起头）
// 比新写一条反向的拉取通道省下整套分块/续传/校验逻辑。
type SharePullReq struct {
	ShareID string `json:"share_id"`
	// Paths 是相对共享根的文件路径（斜杠分隔，来自 ShareIndexResp.Items）。
	Paths []string `json:"paths"`
	// BatchID 由【拉取方】生成并在这里传给共享方，共享方原样回填到每个
	// 传输任务的 batch_id 上：接收端的目录树还原与同一个批次根目录的分配
	// 都靠它，两边对不上就会出现「文件落到了别的目录」。
	BatchID string `json:"batch_id"`
	// RootName 是批次的根目录名（共享名）。接收端 RelPath 的首段就是它。
	RootName string `json:"root_name"`
}

// SharePullResp 是拉取请求的受理结果。
//
// 只回复「受理与否」：被受理的文件通过随后到来的 FILE_META 列表体现，
// 逐文件的成败由传输层自己的事件承担 —— 在这里再维护一份进度真相，
// 迟早会与传输任务列表不一致。
type SharePullResp struct {
	// BatchID 原样回填请求里的值：protocol.Frame 没有 msg_id，
	// 而这份响应天然要对应「哪一次请求」，batch_id 是唯一稳定的关联键。
	BatchID string `json:"batch_id"`
	OK      bool   `json:"ok"`
	// Accepted 是受理的文件数（0 与 OK=true 同时出现表示「清单已过期」）。
	Accepted int `json:"accepted,omitempty"`
	// Rejected 是被跳过的条目（文件已删除/越界/超限），带原因是让用户知道
	// 「少传了几个、为什么」。
	Rejected []string `json:"rejected,omitempty"`
	Error    string   `json:"error,omitempty"`
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
