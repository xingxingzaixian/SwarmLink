package protocol

import (
	"bytes"
	"testing"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
)

func TestUDPDatagramRoundTrip(t *testing.T) {
	raw, err := EncodeUDP(UDPTypeAnnounce, AnnounceWire{NodeID: "abcd"})
	if err != nil {
		t.Fatal(err)
	}
	// 必须带 4 字节魔数（否则会被其他 UDP 服务解析）
	if !bytes.Equal(raw[:4], UDPMagic[:]) {
		t.Fatal("missing udp magic")
	}
	typ, body, err := DecodeUDP(raw)
	if err != nil {
		t.Fatal(err)
	}
	if typ != UDPTypeAnnounce {
		t.Fatalf("type want %#x got %#x", UDPTypeAnnounce, typ)
	}
	var w AnnounceWire
	if err := DecodeJSON(body, &w); err != nil {
		t.Fatal(err)
	}
	if w.NodeID != "abcd" {
		t.Fatal("body mismatch")
	}
}

func TestDecodeUDPRejectsForeignPackets(t *testing.T) {
	for _, bad := range [][]byte{
		nil,
		[]byte("short"),
		[]byte("XXXX__not-swarmlink-payload"),
		[]byte{0x53, 0x4C, 0x41}, // 魔数前缀不完整
	} {
		if _, _, err := DecodeUDP(bad); err == nil {
			t.Fatalf("must reject foreign packet %q", bad)
		}
	}
}

func TestAnnounceWireRoundTrip(t *testing.T) {
	kp, _ := identity.GenerateKeyPair()
	var nonce [16]byte
	nonce[0] = 9

	a := peer.Announcement{
		NodeID:      kp.NodeID(),
		DisplayName: "alice",
		PublicKey:   kp.PublicKey(),
		TCPPort:     2425,
		UDPPort:     2426,
		Subnet:      "192.168.1.0/24",
		Nonce:       nonce,
		Timestamp:   1700000000,
		Epoch:       7,
	}
	a.Sig = kp.Sign(a.SigningBytes())

	raw, err := EncodeUDP(UDPTypeAnnounce, NewAnnounceWire(a))
	if err != nil {
		t.Fatal(err)
	}
	_, body, err := DecodeUDP(raw)
	if err != nil {
		t.Fatal(err)
	}
	var w AnnounceWire
	if err := DecodeJSON(body, &w); err != nil {
		t.Fatal(err)
	}
	back, err := w.Announcement()
	if err != nil {
		t.Fatal(err)
	}

	if back.NodeID != a.NodeID || back.DisplayName != a.DisplayName ||
		back.TCPPort != a.TCPPort || back.UDPPort != a.UDPPort ||
		back.Subnet != a.Subnet || back.Timestamp != a.Timestamp || back.Epoch != a.Epoch {
		t.Fatalf("field mismatch:\n got %+v\nwant %+v", back, a)
	}
	if !bytes.Equal(back.Nonce[:], a.Nonce[:]) {
		t.Fatal("nonce mismatch")
	}
	if !identity.Verify(back.PublicKey, back.SigningBytes(), back.Sig) {
		t.Fatal("signature must survive wire roundtrip")
	}
}

func TestAnnounceWireRejectsBadNonceLength(t *testing.T) {
	kp, _ := identity.GenerateKeyPair()
	w := AnnounceWire{
		NodeID:    kp.NodeID().String(),
		PublicKey: identity.MarshalPublic(kp.PublicKey()),
		Nonce:     "0011", // 2 字节，非法（必须 16）
	}
	if _, err := w.Announcement(); err == nil {
		t.Fatal("expected nonce length error")
	}
}

func TestPeerListRespWireRoundTrip(t *testing.T) {
	raw, err := EncodeUDP(UDPTypePeerListResp, PeerListRespWire{Epoch: 3, Entries: []PeerListEntry{{NodeID: "aa"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, body, _ := DecodeUDP(raw)
	var out PeerListRespWire
	if err := DecodeJSON(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Epoch != 3 || len(out.Entries) != 1 || out.Entries[0].NodeID != "aa" {
		t.Fatalf("mismatch: %+v", out)
	}
}
