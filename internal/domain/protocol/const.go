// Package protocol 定义帧格式、协议常量与纯函数编解码。
//
// 位置说明：架构书原目录为 adapters/net/protocol，但 domain/ports.ConnManager
// 需要引用 protocol.Frame。若把它放在 adapters 下会让 domain 反向依赖 adapter，
// 违反「依赖单向向内」。由于帧编解码是纯函数、零 I/O，本包放在 domain 下，
// 使 ports 与 adapters 都能向内依赖它。
package protocol

// ---------------------------------------------------------------------------
// TCP 帧常量
// ---------------------------------------------------------------------------

const (
	// Magic 是 TCP 流对齐魔数 0x53 0x4C（"SL"）。
	// 【协议 ABI】改此值即破坏兼容。
	Magic0 byte = 0x53
	Magic1 byte = 0x4C

	// Version 是当前协议版本。
	Version byte = 0x02

	// HeaderSize = Magic(2) + Ver(1) + Flags(1) + Type(1) + Length(4) = 9 字节。
	HeaderSize = 9

	// MaxFrameSize 是单帧载荷上限（16 MB），超过即协议违规 → 关连接（防内存炸弹）。
	MaxFrameSize = 16 << 20
)

// Flags 位掩码（架构书 4.1）。
const (
	FlagEncrypted  byte = 1 << 0 // 载荷已加密（v1.1 Noise）
	FlagCompressed byte = 1 << 1 // 载荷已压缩（zstd）
	FlagFragment   byte = 1 << 2 // 分片帧
	FlagFragEnd    byte = 1 << 3 // 分片结束帧

	// FlagKnownMask 是已知标志位；未知位必须被忽略而非报错。
	FlagKnownMask = FlagEncrypted | FlagCompressed | FlagFragment | FlagFragEnd
)

// ---------------------------------------------------------------------------
// TCP 消息类型（独立编号空间）
// ---------------------------------------------------------------------------

const (
	// 0x00–0x0F 会话控制
	TypeHello        byte = 0x01
	TypeChat         byte = 0x02
	TypePeerListReq  byte = 0x03
	TypePeerListResp byte = 0x04
	TypeAuth         byte = 0x05
	TypeAuthOK       byte = 0x06
	TypeError        byte = 0x07
	TypePingAddr     byte = 0x08

	// 0x10–0x1F 文件传输
	TypeFileMeta     byte = 0x10
	TypeFileMetaAck  byte = 0x11
	TypeFileChunk    byte = 0x12
	TypeFileChunkAck byte = 0x13
	TypeFileDone     byte = 0x14
	TypeFileDoneAck  byte = 0x15

	// 0x20–0x2F 消息可靠性
	TypeHeartbeat    byte = 0x20
	TypeHeartbeatAck byte = 0x21
	TypeChatAck      byte = 0x22

	// 0x30–0x3F 群组
	TypeGroupMeta       byte = 0x30
	TypeGroupMembersReq byte = 0x31
	TypeGroupMembersRes byte = 0x32
	TypeGroupMsg        byte = 0x33

	// 0x40–0x4F 安全（v1.1 加密通道预留）
	TypeKeyExchange    byte = 0x40
	TypeKeyExchangeAck byte = 0x41
)

// ---------------------------------------------------------------------------
// UDP 发现报文（独立的魔数与编号空间）
// ---------------------------------------------------------------------------
//
// UDP 报文必须能被「不认识它的其他 UDP 服务」以最快速度丢弃，
// 所以前面要有强势魔数；TCP 帧不需要这层保护（连接已建立，对端必是本协议）。
// 【重要】两套编号空间不要混用，否则调整 UDP 魔数会牵动 TCP 版本兼容。

// UDPMagic 是 4 字节魔数 "SLAN"（0x53 0x4C 0x41 0x4E）。
var UDPMagic = [4]byte{0x53, 0x4C, 0x41, 0x4E}

// UDPVersion 是 UDP 发现报文版本。
const UDPVersion byte = 0x01

// UDP 消息类型。
const (
	UDPTypeAnnounce     byte = 0x01
	UDPTypePeerListReq  byte = 0x02
	UDPTypePeerListResp byte = 0x03
	UDPTypeSeedProbe    byte = 0x04
)

// TypeName 返回类型的可读名（日志用）。
func TypeName(t byte) string {
	switch t {
	case TypeHello:
		return "HELLO"
	case TypeChat:
		return "CHAT"
	case TypePeerListReq:
		return "PEER_LIST_REQ"
	case TypePeerListResp:
		return "PEER_LIST_RESP"
	case TypeAuth:
		return "AUTH"
	case TypeAuthOK:
		return "AUTH_OK"
	case TypeError:
		return "ERROR"
	case TypePingAddr:
		return "PING_ADDR"
	case TypeFileMeta:
		return "FILE_META"
	case TypeFileMetaAck:
		return "FILE_META_ACK"
	case TypeFileChunk:
		return "FILE_CHUNK"
	case TypeFileChunkAck:
		return "FILE_CHUNK_ACK"
	case TypeFileDone:
		return "FILE_DONE"
	case TypeFileDoneAck:
		return "FILE_DONE_ACK"
	case TypeHeartbeat:
		return "HEARTBEAT"
	case TypeHeartbeatAck:
		return "HEARTBEAT_ACK"
	case TypeChatAck:
		return "CHAT_ACK"
	case TypeGroupMeta:
		return "GROUP_META"
	case TypeGroupMembersReq:
		return "GROUP_MEMBERS_REQ"
	case TypeGroupMembersRes:
		return "GROUP_MEMBERS_RESP"
	case TypeGroupMsg:
		return "GROUP_MSG"
	case TypeKeyExchange:
		return "KEY_EXCHANGE"
	case TypeKeyExchangeAck:
		return "KEY_EXCHANGE_ACK"
	default:
		return "UNKNOWN"
	}
}
