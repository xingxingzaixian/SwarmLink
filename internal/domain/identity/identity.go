// Package identity 定义节点身份：Ed25519 密钥对、NodeID 派生、签名与验证。
//
// 本包是纯领域代码（红线 R1）：禁止 import net / database/sql / os / wails。
// 密钥文件的读写由 adapters/keystore 承担。
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
)

const (
	// nodeIDDomain 是 NodeID 派生的域分隔串（架构书 4.2），
	// 避免该哈希被复用到其他协议上下文时产生交叉攻击面。
	nodeIDDomain = "swarmlink-node-id-v1"
	nodeIDLen    = 8
)

// NodeID 是公钥指纹，8 字节（16 个 hex 字符）。
// 200 节点规模下碰撞概率约 1e-15，无需加长（加长会让 UI 指纹更难读）。
type NodeID [nodeIDLen]byte

// String 返回 16 字符的 hex 表示。
func (n NodeID) String() string { return hex.EncodeToString(n[:]) }

// IsZero 判断是否为零值。
func (n NodeID) IsZero() bool { return n == NodeID{} }

// Less 提供确定性的全序，用于「双向拨号胜负规则」等双方独立计算同一结论的场景。
func (n NodeID) Less(o NodeID) bool {
	for i := 0; i < nodeIDLen; i++ {
		if n[i] != o[i] {
			return n[i] < o[i]
		}
	}
	return false
}

// ParseNodeID 解析 16 字符 hex 的 NodeID。
func ParseNodeID(s string) (NodeID, error) {
	var n NodeID
	b, err := hex.DecodeString(s)
	if err != nil {
		return n, fmt.Errorf("identity: parse node id: %w", err)
	}
	if len(b) != nodeIDLen {
		return n, fmt.Errorf("identity: node id must be %d bytes, got %d", nodeIDLen, len(b))
	}
	copy(n[:], b)
	return n, nil
}

// Fingerprint 由公钥派生 NodeID：SHA-256("swarmlink-node-id-v1" || pubkey)[:8]。
func Fingerprint(pub ed25519.PublicKey) NodeID {
	h := sha256.New()
	h.Write([]byte(nodeIDDomain))
	h.Write(pub)
	sum := h.Sum(nil)
	var n NodeID
	copy(n[:], sum[:nodeIDLen])
	return n
}

// KeyPair 是不可变的节点密钥对。
type KeyPair struct {
	nodeID NodeID
	priv   ed25519.PrivateKey
	pub    ed25519.PublicKey
}

// NewKeyPair 由私钥构造密钥对（NodeID 由公钥派生，不信任外部传入）。
func NewKeyPair(priv ed25519.PrivateKey) (*KeyPair, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("identity: invalid private key size %d", len(priv))
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("identity: private key does not yield ed25519 public key")
	}
	return &KeyPair{nodeID: Fingerprint(pub), priv: priv, pub: pub}, nil
}

// GenerateKeyPair 生成新的 Ed25519 密钥对。
func GenerateKeyPair() (*KeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate key: %w", err)
	}
	return &KeyPair{nodeID: Fingerprint(pub), priv: priv, pub: pub}, nil
}

// NodeID 返回派生出的节点标识。
func (k *KeyPair) NodeID() NodeID { return k.nodeID }

// PublicKey 返回公钥。
func (k *KeyPair) PublicKey() ed25519.PublicKey { return k.pub }

// PrivateKey 返回私钥。调用方必须避免将其写入日志。
func (k *KeyPair) PrivateKey() ed25519.PrivateKey { return k.priv }

// Sign 用私钥签名。
func (k *KeyPair) Sign(msg []byte) []byte { return ed25519.Sign(k.priv, msg) }

// Verify 用公钥验签。公钥长度不合法时直接返回 false（不 panic）。
func Verify(pub ed25519.PublicKey, msg, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize {
		return false
	}
	if len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}

// MarshalPrivateKeyPEM 序列化为 PKCS#8 PEM（标准块类型 "PRIVATE KEY"）。
func MarshalPrivateKeyPEM(priv ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("identity: marshal pkcs8: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParsePrivateKeyPEM 解析 PKCS#8 PEM。
func ParsePrivateKeyPEM(data []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("identity: no PEM block found")
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("identity: unexpected PEM block type %q", block.Type)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("identity: parse pkcs8: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("identity: key is not ed25519")
	}
	return priv, nil
}

// MarshalPublic 将公钥编码为 base64（用于 peers.pub_key 与握手载荷）。
func MarshalPublic(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

// ParsePublic 解析 base64 公钥并校验长度。
func ParsePublic(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("identity: decode public key: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("identity: public key must be %d bytes, got %d", ed25519.PublicKeySize, len(b))
	}
	return ed25519.PublicKey(b), nil
}
