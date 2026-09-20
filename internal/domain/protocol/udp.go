package protocol

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
)

// UDP 报文的固定前缀长度：Magic(4) + Version(1) + Type(1)。
const UDPPrefixLen = 6

// ErrBadUDPPacket 表示报文不是本协议的（魔数或长度不符）。
var ErrBadUDPPacket = fmt.Errorf("protocol: not a swarmlink udp packet")

// EncodeUDP 编码一个 UDP 发现报文。
//
// 前面必须有强势 4 字节魔数：UDP 报文可能落到任意一个 UDP 服务上，
// 它必须能被「不认识它的服务」以最快速度丢弃。
func EncodeUDP(typ byte, v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("protocol: encode udp body: %w", err)
	}
	buf := make([]byte, 0, UDPPrefixLen+len(body))
	buf = append(buf, UDPMagic[:]...)
	buf = append(buf, UDPVersion, typ)
	buf = append(buf, body...)
	return buf, nil
}

// DecodeUDP 校验魔数并拆出 (type, body)。
//
// 布局：Magic[0:4] | Version[4] | Type[5] | body[6:]
func DecodeUDP(b []byte) (byte, []byte, error) {
	if len(b) < UDPPrefixLen {
		return 0, nil, ErrBadUDPPacket
	}
	if b[0] != UDPMagic[0] || b[1] != UDPMagic[1] || b[2] != UDPMagic[2] || b[3] != UDPMagic[3] {
		return 0, nil, ErrBadUDPPacket
	}
	return b[5], b[UDPPrefixLen:], nil
}

// ---------------------------------------------------------------------------
// ANNOUNCE
// ---------------------------------------------------------------------------

// AnnounceWire 是 ANNOUNCE 的线上表示（公钥/签名为 base64，nonce 为 hex）。
type AnnounceWire struct {
	NodeID      string `json:"node_id"`
	DisplayName string `json:"display_name"`
	PublicKey   string `json:"public_key"`
	TCPPort     uint16 `json:"tcp_port"`
	UDPPort     uint16 `json:"udp_port"`
	Subnet      string `json:"subnet"`
	Nonce       string `json:"nonce"`
	Timestamp   int64  `json:"timestamp"`
	Epoch       uint64 `json:"epoch"`
	Sig         string `json:"sig"`
}

// NewAnnounceWire 把领域通告转为线上结构。
func NewAnnounceWire(a peer.Announcement) AnnounceWire {
	return AnnounceWire{
		NodeID:      a.NodeID.String(),
		DisplayName: a.DisplayName,
		PublicKey:   identity.MarshalPublic(a.PublicKey),
		TCPPort:     a.TCPPort,
		UDPPort:     a.UDPPort,
		Subnet:      a.Subnet,
		Nonce:       NonceHex(a.Nonce[:]),
		Timestamp:   a.Timestamp,
		Epoch:       a.Epoch,
		Sig:         base64.StdEncoding.EncodeToString(a.Sig),
	}
}

// Announcement 把线上结构还原为领域通告（不做验签，验签由调用方执行）。
func (w AnnounceWire) Announcement() (peer.Announcement, error) {
	var a peer.Announcement

	nodeID, err := identity.ParseNodeID(w.NodeID)
	if err != nil {
		return a, err
	}
	pub, err := identity.ParsePublic(w.PublicKey)
	if err != nil {
		return a, err
	}
	nonce, err := ParseNonceHex(w.Nonce)
	if err != nil {
		return a, err
	}
	if len(nonce) != len(a.Nonce) {
		return a, fmt.Errorf("protocol: announce nonce must be %d bytes, got %d", len(a.Nonce), len(nonce))
	}
	sig, err := base64.StdEncoding.DecodeString(w.Sig)
	if err != nil {
		return a, fmt.Errorf("protocol: decode announce sig: %w", err)
	}

	a.NodeID = nodeID
	a.DisplayName = w.DisplayName
	a.PublicKey = pub
	a.TCPPort = w.TCPPort
	a.UDPPort = w.UDPPort
	a.Subnet = w.Subnet
	copy(a.Nonce[:], nonce)
	a.Timestamp = w.Timestamp
	a.Epoch = w.Epoch
	a.Sig = sig
	return a, nil
}

// ---------------------------------------------------------------------------
// PEER_LIST
// ---------------------------------------------------------------------------

// PeerListEntry 是目录条目的线上表示。
type PeerListEntry struct {
	NodeID      string `json:"node_id"`
	DisplayName string `json:"display_name"`
	PublicKey   string `json:"public_key"`
	Addr        string `json:"addr"`
	Subnet      string `json:"subnet"`
	Caps        uint32 `json:"caps"`
	ProtoVer    int    `json:"proto_ver"`
}

// PeerListReqWire 是目录拉取请求。
type PeerListReqWire struct {
	ListEpoch uint64 `json:"list_epoch"`
}

// PeerListRespWire 是目录拉取响应（epoch 未变时 Entries 为空）。
type PeerListRespWire struct {
	Epoch   uint64          `json:"epoch"`
	Entries []PeerListEntry `json:"entries"`
}

// SeedProbeWire 是种子探测包（第一跳）。
type SeedProbeWire struct {
	NodeID  string `json:"node_id"`
	TCPPort uint16 `json:"tcp_port"`
	UDPPort uint16 `json:"udp_port"`
}
