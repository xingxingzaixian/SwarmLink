package tcp

import (
	"encoding/base64"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
)

func testCfg(name string, caps peer.Caps) HandshakeConfig {
	c := DefaultHandshakeConfig()
	c.DisplayName = name
	c.Caps = caps
	c.TCPPort = 2425
	c.UDPPort = 2425
	c.Timeout = 5 * time.Second
	return c
}

func TestHandshakeHappyPath(t *testing.T) {
	a, _ := identity.GenerateKeyPair()
	b, _ := identity.GenerateKeyPair()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	var ra *HandshakeResult
	var ea error
	done := make(chan struct{})
	go func() {
		defer close(done)
		ra, ea = handshakeInitiator(c1, a, testCfg("alice", peer.CapsChatReliable|peer.CapsGroupChat))
	}()

	rb, eb := handshakeResponder(c2, b, testCfg("bob", peer.CapsGroupChat|peer.CapsEncrypted))
	<-done

	if ea != nil {
		t.Fatalf("initiator: %v", ea)
	}
	if eb != nil {
		t.Fatalf("responder: %v", eb)
	}
	if ra.PeerID != b.NodeID() {
		t.Fatal("initiator learned wrong peer id")
	}
	if rb.PeerID != a.NodeID() {
		t.Fatal("responder learned wrong peer id")
	}
	// caps_common = A.caps & B.caps = GROUP_CHAT
	if ra.Caps != peer.CapsGroupChat || rb.Caps != peer.CapsGroupChat {
		t.Fatalf("caps negotiation wrong: %v / %v", ra.Caps, rb.Caps)
	}
	if ra.DisplayName != "bob" || rb.DisplayName != "alice" {
		t.Fatalf("display name exchange wrong: %s / %s", ra.DisplayName, rb.DisplayName)
	}
	if ra.ProtoVersion != ProtoVersionCurrent {
		t.Fatalf("proto version %d", ra.ProtoVersion)
	}
	if ra.TCPPort != 2425 {
		t.Fatal("tcp port not exchanged")
	}
}

func TestInitiatorRejectsImpersonation(t *testing.T) {
	a, _ := identity.GenerateKeyPair()
	claimed, _ := identity.GenerateKeyPair()  // 被冒充的身份
	attacker, _ := identity.GenerateKeyPair() // 攻击者实际持有的密钥

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	go func() {
		// 收 HELLO
		if _, err := protocol.Read(c2); err != nil {
			return
		}
		// 声称是 claimed，却拿不出对应私钥（nodeID 与 pubkey 不自洽）
		ack := protocol.HelloAck{
			ProtoChosen: ProtoVersionCurrent,
			CapsCommon:  0,
			NodeID:      claimed.NodeID().String(),
			PublicKey:   identity.MarshalPublic(attacker.PublicKey()),
			Nonce:       protocol.NonceHex(make([]byte, nonceLen)),
			Sig:         base64.StdEncoding.EncodeToString(make([]byte, 64)),
		}
		payload, _ := protocol.EncodeJSON(ack)
		_, _ = protocol.Write(c2, protocol.New(protocol.TypeHelloAck, payload))
		// 保持连接直到测试结束
		_, _ = io.Copy(io.Discard, c2)
	}()

	_, err := handshakeInitiator(c1, a, testCfg("alice", 0))
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("want ErrAuthFailed got %v", err)
	}
}

func TestResponderRejectsBadSignature(t *testing.T) {
	a, _ := identity.GenerateKeyPair()
	b, _ := identity.GenerateKeyPair()

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	go func() {
		hello := protocol.Hello{
			ProtoMin: ProtoVersionCurrent, ProtoMax: ProtoVersionCurrent,
			NodeID:    a.NodeID().String(),
			PublicKey: identity.MarshalPublic(a.PublicKey()),
			Nonce:     protocol.NonceHex(make([]byte, nonceLen)),
		}
		p, _ := protocol.EncodeJSON(hello)
		_, _ = protocol.Write(c1, protocol.New(protocol.TypeHello, p))
		if _, err := protocol.Read(c1); err != nil { // HELLO_ACK
			return
		}
		// 用错误的签名回应 AUTH
		auth := protocol.Auth{Sig: base64.StdEncoding.EncodeToString([]byte("not-a-real-signature"))}
		pa, _ := protocol.EncodeJSON(auth)
		_, _ = protocol.Write(c1, protocol.New(protocol.TypeAuth, pa))
		_, _ = io.Copy(io.Discard, c1)
	}()

	_, err := handshakeResponder(c2, b, testCfg("bob", 0))
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("want ErrAuthFailed got %v", err)
	}
}

func TestHandshakeRejectsWrongFirstFrame(t *testing.T) {
	a, _ := identity.GenerateKeyPair()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	go func() {
		if _, err := protocol.Read(c2); err != nil {
			return
		}
		p, _ := protocol.EncodeJSON(protocol.ErrorPayload{Code: 1, Message: "nope"})
		_, _ = protocol.Write(c2, protocol.New(protocol.TypeError, p))
		_, _ = io.Copy(io.Discard, c2)
	}()

	_, err := handshakeInitiator(c1, a, testCfg("alice", 0))
	if !errors.Is(err, ErrUnexpectedFrame) {
		t.Fatalf("want ErrUnexpectedFrame got %v", err)
	}
}

func TestNegotiateNoCommonVersion(t *testing.T) {
	if _, _, err := negotiate(0, 0, 0, 0); !errors.Is(err, ErrNoCommonVersion) {
		t.Fatalf("want ErrNoCommonVersion got %v", err)
	}
	chosen, common, err := negotiate(3, 2, peer.CapsGroupChat|peer.CapsChatReliable, peer.CapsGroupChat)
	if err != nil {
		t.Fatal(err)
	}
	if chosen != 2 {
		t.Fatalf("chosen want 2 got %d", chosen)
	}
	if common != peer.CapsGroupChat {
		t.Fatalf("common want GroupChat got %v", common)
	}
}
