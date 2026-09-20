package tcp

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
)

// 握手错误。
var (
	ErrAuthFailed      = errors.New("tcp: handshake authentication failed")
	ErrUnexpectedFrame = errors.New("tcp: unexpected frame during handshake")
	ErrNoCommonVersion = errors.New("tcp: no common protocol version")
)

const (
	nonceLen = 32
	// ProtoVersionCurrent 是本实现支持的协议版本。
	ProtoVersionCurrent = 2
)

// HandshakeConfig 是握手参数（来自配置与能力表）。
type HandshakeConfig struct {
	DisplayName  string
	TCPPort      uint16
	UDPPort      uint16
	Caps         peer.Caps
	ProtoMin     int
	ProtoMax     int
	MaxFrameSize int
	MaxChunkSize int
	WindowSize   int
	// RequireAuth=false 时跳过签名校验（仅用于本地调试，安全默认 true）。
	RequireAuth bool
	Timeout     time.Duration
}

// DefaultHandshakeConfig 返回安全默认值。
func DefaultHandshakeConfig() HandshakeConfig {
	return HandshakeConfig{
		Caps:         peer.CapsChatReliable | peer.CapsGroupChat | peer.CapsResumeBitmap | peer.CapsSlidingWindow,
		ProtoMin:     ProtoVersionCurrent,
		ProtoMax:     ProtoVersionCurrent,
		MaxFrameSize: protocol.MaxFrameSize,
		MaxChunkSize: 512 * 1024,
		WindowSize:   8,
		RequireAuth:  true,
		Timeout:      10 * time.Second,
	}
}

// HandshakeResult 是握手成功后的对端信息。
type HandshakeResult struct {
	PeerID       identity.NodeID
	PublicKey    ed25519.PublicKey
	DisplayName  string
	Caps         peer.Caps
	ProtoVersion int
	TCPPort      uint16
	UDPPort      uint16
}

func newNonce() ([]byte, error) {
	b := make([]byte, nonceLen)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("tcp: nonce: %w", err)
	}
	return b, nil
}

func negotiate(protoMaxA, protoMaxB int, capsA, capsB peer.Caps) (int, peer.Caps, error) {
	chosen := protoMaxA
	if protoMaxB < chosen {
		chosen = protoMaxB
	}
	if chosen < 1 {
		return 0, 0, ErrNoCommonVersion
	}
	return chosen, capsA.Common(capsB), nil
}

// handshakeInitiator 执行拨号方（A）侧的三段握手。
func handshakeInitiator(conn net.Conn, kp *identity.KeyPair, cfg HandshakeConfig) (*HandshakeResult, error) {
	deadline := time.Now().Add(cfg.Timeout)
	_ = conn.SetDeadline(deadline)

	nonceA, err := newNonce()
	if err != nil {
		return nil, err
	}

	hello := protocol.Hello{
		ProtoMin:     cfg.ProtoMin,
		ProtoMax:     cfg.ProtoMax,
		Caps:         uint32(cfg.Caps),
		MaxFrameSize: cfg.MaxFrameSize,
		MaxChunkSize: cfg.MaxChunkSize,
		WindowSize:   cfg.WindowSize,
		NodeID:       kp.NodeID().String(),
		PublicKey:    identity.MarshalPublic(kp.PublicKey()),
		Nonce:        protocol.NonceHex(nonceA),
		DisplayName:  cfg.DisplayName,
		TCPPort:      cfg.TCPPort,
		UDPPort:      cfg.UDPPort,
	}
	if err := writeJSON(conn, protocol.TypeHello, hello); err != nil {
		return nil, err
	}

	// --- 第二段：读取 HELLO_ACK ---
	ackFrame, err := protocol.Read(conn)
	if err != nil {
		return nil, err
	}
	if ackFrame.Type != protocol.TypeHelloAck {
		return nil, fmt.Errorf("%w: want HELLO_ACK got %s", ErrUnexpectedFrame, protocol.TypeName(ackFrame.Type))
	}
	var ack protocol.HelloAck
	if err := protocol.DecodeJSON(ackFrame.Payload, &ack); err != nil {
		return nil, err
	}

	peerID, err := identity.ParseNodeID(ack.NodeID)
	if err != nil {
		return nil, err
	}
	pubB, err := identity.ParsePublic(ack.PublicKey)
	if err != nil {
		return nil, err
	}
	// ① 校验 nodeID_B == Fingerprint(pubKey_B)：公钥与声称的身份必须自洽
	if identity.Fingerprint(pubB) != peerID {
		return nil, fmt.Errorf("%w: node id does not match public key", ErrAuthFailed)
	}
	nonceB, err := protocol.ParseNonceHex(ack.Nonce)
	if err != nil {
		return nil, err
	}

	if cfg.RequireAuth {
		if len(nonceB) != nonceLen {
			return nil, fmt.Errorf("%w: bad nonce length %d", ErrAuthFailed, len(nonceB))
		}
		transcript := protocol.HelloTranscript(nonceA, nonceB, kp.PublicKey(), pubB)
		sigB, err := base64.StdEncoding.DecodeString(ack.Sig)
		if err != nil {
			return nil, fmt.Errorf("%w: bad sig encoding", ErrAuthFailed)
		}
		// ② 用 pubKey_B 验 sig_B：证明对端确实持有对应私钥
		if !identity.Verify(pubB, transcript, sigB) {
			return nil, fmt.Errorf("%w: sig_B invalid", ErrAuthFailed)
		}
	}

	// --- 第三段：发送 AUTH ---
	transcript := protocol.HelloTranscript(nonceA, nonceB, kp.PublicKey(), pubB)
	auth := protocol.Auth{Sig: base64.StdEncoding.EncodeToString(kp.Sign(transcript))}
	if err := writeJSON(conn, protocol.TypeAuth, auth); err != nil {
		return nil, err
	}

	okFrame, err := protocol.Read(conn)
	if err != nil {
		return nil, err
	}
	if okFrame.Type != protocol.TypeAuthOK {
		return nil, fmt.Errorf("%w: want AUTH_OK got %s", ErrUnexpectedFrame, protocol.TypeName(okFrame.Type))
	}

	chosen, common, err := negotiate(cfg.ProtoMax, ack.ProtoChosen, cfg.Caps, peer.Caps(ack.CapsCommon))
	if err != nil {
		return nil, err
	}

	_ = conn.SetDeadline(time.Time{})
	return &HandshakeResult{
		PeerID:       peerID,
		PublicKey:    pubB,
		DisplayName:  ack.DisplayName,
		Caps:         common,
		ProtoVersion: chosen,
		TCPPort:      ack.TCPPort,
		UDPPort:      ack.UDPPort,
	}, nil
}

// handshakeResponder 执行接受方（B）侧的三段握手。
func handshakeResponder(conn net.Conn, kp *identity.KeyPair, cfg HandshakeConfig) (*HandshakeResult, error) {
	deadline := time.Now().Add(cfg.Timeout)
	_ = conn.SetDeadline(deadline)

	// --- 第一段：读取 HELLO ---
	helloFrame, err := protocol.Read(conn)
	if err != nil {
		return nil, err
	}
	if helloFrame.Type != protocol.TypeHello {
		return nil, fmt.Errorf("%w: want HELLO got %s", ErrUnexpectedFrame, protocol.TypeName(helloFrame.Type))
	}
	var hello protocol.Hello
	if err := protocol.DecodeJSON(helloFrame.Payload, &hello); err != nil {
		return nil, err
	}

	peerID, err := identity.ParseNodeID(hello.NodeID)
	if err != nil {
		return nil, err
	}
	pubA, err := identity.ParsePublic(hello.PublicKey)
	if err != nil {
		return nil, err
	}
	if identity.Fingerprint(pubA) != peerID {
		return nil, fmt.Errorf("%w: node id does not match public key", ErrAuthFailed)
	}
	nonceA, err := protocol.ParseNonceHex(hello.Nonce)
	if err != nil {
		return nil, err
	}

	nonceB, err := newNonce()
	if err != nil {
		return nil, err
	}
	transcript := protocol.HelloTranscript(nonceA, nonceB, pubA, kp.PublicKey())

	chosen, common, err := negotiate(cfg.ProtoMax, hello.ProtoMax, cfg.Caps, peer.Caps(hello.Caps))
	if err != nil {
		return nil, err
	}

	// --- 第二段：发送 HELLO_ACK（含 sig_B）---
	ack := protocol.HelloAck{
		ProtoChosen: chosen,
		CapsCommon:  uint32(common),
		NodeID:      kp.NodeID().String(),
		PublicKey:   identity.MarshalPublic(kp.PublicKey()),
		Nonce:       protocol.NonceHex(nonceB),
		Sig:         base64.StdEncoding.EncodeToString(kp.Sign(transcript)),
		DisplayName: cfg.DisplayName,
		TCPPort:     cfg.TCPPort,
		UDPPort:     cfg.UDPPort,
	}
	if err := writeJSON(conn, protocol.TypeHelloAck, ack); err != nil {
		return nil, err
	}

	// --- 第三段：读取 AUTH 并验签 ---
	authFrame, err := protocol.Read(conn)
	if err != nil {
		return nil, err
	}
	if authFrame.Type != protocol.TypeAuth {
		return nil, fmt.Errorf("%w: want AUTH got %s", ErrUnexpectedFrame, protocol.TypeName(authFrame.Type))
	}
	var auth protocol.Auth
	if err := protocol.DecodeJSON(authFrame.Payload, &auth); err != nil {
		return nil, err
	}
	if cfg.RequireAuth {
		sigA, err := base64.StdEncoding.DecodeString(auth.Sig)
		if err != nil {
			return nil, fmt.Errorf("%w: bad sig encoding", ErrAuthFailed)
		}
		if !identity.Verify(pubA, transcript, sigA) {
			return nil, fmt.Errorf("%w: sig_A invalid", ErrAuthFailed)
		}
	}

	if err := writeJSON(conn, protocol.TypeAuthOK, protocol.AuthOK{}); err != nil {
		return nil, err
	}

	_ = conn.SetDeadline(time.Time{})
	return &HandshakeResult{
		PeerID:       peerID,
		PublicKey:    pubA,
		DisplayName:  hello.DisplayName,
		Caps:         common,
		ProtoVersion: chosen,
		TCPPort:      hello.TCPPort,
		UDPPort:      hello.UDPPort,
	}, nil
}

func writeJSON(conn net.Conn, typ byte, v any) error {
	payload, err := protocol.EncodeJSON(v)
	if err != nil {
		return err
	}
	if _, err := protocol.Write(conn, protocol.New(typ, payload)); err != nil {
		return fmt.Errorf("tcp: write %s: %w", protocol.TypeName(typ), err)
	}
	return nil
}
