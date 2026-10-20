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
	"github.com/swarmlink/swarmlink/internal/domain/share"
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

	// Shares 是该节点声明的共享摘要（FR-14）。
	//
	// 为什么放在目录条目上而不是单开一张远端共享表：共享的可发现性与
	// 「我听说过谁」是同一件事，随 ANNOUNCE 与目录交换一起传播天然正确；
	// 单开一张表就得再造一条同步路径，而两条路径迟早会分叉。
	//
	// 语义边界（务必看清）：这里的摘要是【对方声明 + 第三方转述】，
	// 不参与 ANNOUNCE 签名（见 Announcement.Shares），因此只能用于
	// 展示与「去哪儿问」。真正的访问控制一律由共享方在收到
	// SHARE_INDEX_REQ / SHARE_PULL_REQ 时校验（ShareApp.HandleIndexReq）。
	Shares []share.Summary

	// AppVersion 是该节点自述的应用版本（"1.2.0"），空串表示「对方没报」。
	//
	// 与 ProtoVer 的区别必须分清：ProtoVer 是线协议版本（能不能对话），
	// AppVersion 是产品版本（要不要升级成一样）。前者不匹配会导致断连，
	// 后者只影响「局域网更新」这一个功能。
	//
	// 它同样【不参与 ANNOUNCE 签名】（见 Announcement.AppVersion），
	// 因此只可用于「谁可能更新」的候选排序，真正的版本以
	// UPDATE_INFO_RESP 的应答为准（那份应答由对端在会话内直接给出）。
	AppVersion string
}

// Expired 判断条目是否超过 TTL。
func (p Peer) Expired(now time.Time, ttl time.Duration) bool {
	return now.Sub(p.LastSeen) > ttl
}

// LocallyDiscovered 报告这条目录条目能否证明「本网段有邻居」。
//
// 只有广播与组播两条通道算：它们的报文从本网段链路进来，且 ObservedIP 由
// 本地网络栈给出。种子学来的条目（SourceSeed）与第三方转述（SourceGossip）
// 只能证明「别人告诉我他是谁」，证明不了他和我同网段 ——
// 若把两者混进同一个计数，「同网段已连接」就成了一句谎话，
// 而这恰恰是跨网段故障时用户第一个会看的地方。
func (p Peer) LocallyDiscovered() bool {
	return p.Source == SourceBroadcast || p.Source == SourceMulticast
}

// Reachable 报告这个节点当前「发得出消息」。
//
// 必须与前端 stores/peers.ts 的 reachable() 同口径：状态灯上的数字与左栏
// 的「在线」分组用的是同一批节点，两处判据不一致时用户会看到
// 「同网段 3」但列表里只有 1 个在线，而这种不一致最难解释。
//
// 三档可达性（ADR-009）：
//   - 已有 TCP 会话：首条消息零跳数，立刻发得出；
//   - state == online：握手过；
//   - state == discovered：只有新鲜的广播、地址已知，首条消息多一个 RTT。
func (p Peer) Reachable(connected bool) bool {
	if connected {
		return true
	}
	switch p.State {
	case StateOnline:
		return true
	case StateDiscovered:
		return p.LastAddr != ""
	}
	return false
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

	// Shares 是本次通告携带的共享摘要（可为空）。
	//
	// 【刻意不参与签名】（SigningBytes）。理由与 DisplayName 相同：
	// 摘要会随目录交换被第三方转述（那条路径本来就没有签名），
	// 而把它塞进签名域会让新旧版本的 SigningBytes 字节序列不同 ——
	// 旧端 identity.Verify 失败后会【丢弃新端的全部 ANNOUNCE】，
	// 那是「升级一个节点，安静地孤立它」这种最难排查的故障。
	//
	// 代价是摘要可被链路上的人篡改。接受它的前提是：摘要只用于
	// 「有哪些共享可看」的展示，任何一次真实访问都要向共享方
	// 单独发起请求，由共享方校验可见性（见 Peer.Shares 的说明）。
	Shares []share.Summary

	// AppVersion 是对方自述的应用版本（局域网更新用），可为空。
	//
	// 【刻意不参与签名】（SigningBytes），理由与 Shares 完全相同：
	// 把它塞进签名域会让新旧版本的 SigningBytes 字节序列不同 ——
	// 旧端 identity.Verify 失败后会丢弃新端的全部 ANNOUNCE，
	// 也就是「升级一个节点，安静地孤立它」。
	//
	// 代价是版本号可被链路上的人篡改。接受它的前提是：这个字段只用于
	// 「挑一个候选源」，真正的包版本由 UPDATE_INFO_RESP 给出，且安装后
	// 还会做一次「版本真的变了吗」的自检（见 app.UpdateApp）。
	AppVersion string

	// Leaving 表示这是一条「我要下线了」的通告（BYE），而不是「我在线」。
	//
	// 它由 UDP 报文【类型】表达（protocol.UDPTypeBye），不占线格式字段、
	// 不参与签名：签名只证明「是同一个人发的」，「我要走了」这层语义由
	// 报文类型承载就足够了 —— 而且旧版本客户端遇到未知类型会直接忽略，
	// 不会把一条告别误当成在线刷新。
	Leaving bool
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
