// Package peer 定义节点实体、生命周期状态、能力位与种子地址值对象。
package peer

import (
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
)

// State 是 Peer 生命周期状态（架构书 3.6）。
// 注意：discovered → offline 与 online → offline 是两条不同路径，
// 前者表示「从邻居列表消失」（TTL 过期），后者表示「连接真的断了」。
type State string

const (
	StateUnknown    State = "unknown"
	StateDiscovered State = "discovered"
	StateOnline     State = "online"
	StateOffline    State = "offline"
	StateBlocked    State = "blocked"
)

// Caps 是 8 位能力掩码的强类型（架构书 4.3）。
type Caps uint32

const (
	CapsChatReliable Caps = 1 << iota
	CapsGroupChat
	CapsResumeBitmap
	CapsSlidingWindow
	CapsCompression
	CapsEncrypted
	CapsFolderTransfer
	CapsRelay
)

// Has 判断是否包含全部给定能力。
func (c Caps) Has(other Caps) bool { return c&other == other }

// Common 计算协商后的公共能力（A.caps & B.caps）。
func (c Caps) Common(other Caps) Caps { return c & other }

// 条目来源。
const (
	SourceBroadcast = "broadcast"
	SourceMulticast = "multicast"
	SourceSeed      = "seed"
	SourceManual    = "manual"
	SourceGossip    = "gossip"
)

// Peer 是节点目录中的一条记录。
type Peer struct {
	NodeID      identity.NodeID
	DisplayName string
	PublicKey   ed25519.PublicKey
	LastAddr    string
	Caps        Caps
	ProtoVer    int
	FirstSeen   time.Time
	// LastSeen 必须是【本地到达时间】（架构书 ADR-011）。
	// TTL 只与它比较，绝不使用对端 timestamp —— 跨网段时钟偏差因此不影响正确性。
	LastSeen time.Time
	Subnet   string
	State    State
	Source   string
}

// Expired 判断条目是否超过 TTL。
func (p Peer) Expired(now time.Time, ttl time.Duration) bool {
	return now.Sub(p.LastSeen) > ttl
}

// Announcement 是 UDP ANNOUNCE / 种子探测回包携带的节点通告。
type Announcement struct {
	NodeID      identity.NodeID
	DisplayName string
	PublicKey   ed25519.PublicKey
	TCPPort     uint16
	UDPPort     uint16
	Subnet      string
	Nonce       [16]byte
	Timestamp   int64
	Epoch       uint64
	Source      string
	Sig         []byte

	// ObservedIP 是【接收方】观测到的来源 IP。
	// 它由本地网络栈给出，不是对端声明，因此不参与签名，
	// 也不会被对端伪造 —— 目录里的可拨号地址必须以它为准。
	ObservedIP string
}

// announceDomain 是 ANNOUNCE 签名的域分隔串。
const announceDomain = "swarmlink-announce-v1"

// SigningBytes 返回待签名的规范字节序列。
//
// 在架构书「magic||ver||nodeID||tcpPort||nonce||timestamp||epoch」基础上
// 追加了 subnet：subnet 参与 P-2 地址重叠检测，若不签名则可被伪造，
// 使冲突检测失去意义。
func (a Announcement) SigningBytes() []byte {
	var tmp [8]byte
	buf := make([]byte, 0, 128)
	buf = append(buf, announceDomain...)
	buf = append(buf, a.NodeID[:]...)

	binary.BigEndian.PutUint16(tmp[:2], a.TCPPort)
	buf = append(buf, tmp[:2]...)
	binary.BigEndian.PutUint16(tmp[:2], a.UDPPort)
	buf = append(buf, tmp[:2]...)

	buf = append(buf, a.Nonce[:]...)

	binary.BigEndian.PutUint64(tmp[:], uint64(a.Timestamp))
	buf = append(buf, tmp[:]...)
	binary.BigEndian.PutUint64(tmp[:], a.Epoch)
	buf = append(buf, tmp[:]...)

	buf = append(buf, a.Subnet...)
	return buf
}

// SeedAddr 是「值得先问一声的地址」（架构书 ADR-011）。
// 它不带任何角色语义——种子与普通节点跑完全相同的目录交换协议。
type SeedAddr struct {
	IP      string
	UDPPort uint16
}

// String 返回 "ip:udp_port"。
// 注意：不使用 net.JoinHostPort，因为本包禁止 import net（红线 R1）。
func (s SeedAddr) String() string { return fmt.Sprintf("%s:%d", s.IP, s.UDPPort) }

// IsZero 判断是否为空值。
func (s SeedAddr) IsZero() bool { return s.IP == "" && s.UDPPort == 0 }

// ParseSeedAddr 解析 "ip:udp_port"。
func ParseSeedAddr(v string) (SeedAddr, error) {
	v = strings.TrimSpace(v)
	i := strings.LastIndexByte(v, ':')
	if i <= 0 || i == len(v)-1 {
		return SeedAddr{}, fmt.Errorf("peer: invalid seed address %q", v)
	}
	port, err := strconv.ParseUint(v[i+1:], 10, 16)
	if err != nil {
		return SeedAddr{}, fmt.Errorf("peer: invalid seed port in %q: %w", v, err)
	}
	return SeedAddr{IP: v[:i], UDPPort: uint16(port)}, nil
}
