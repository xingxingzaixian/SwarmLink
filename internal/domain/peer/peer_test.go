package peer

import (
	"bytes"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
)

func TestSigningBytesDeterministicAndFieldSensitive(t *testing.T) {
	var nid identity.NodeID
	nid[0] = 0xAB
	a := Announcement{NodeID: nid, TCPPort: 2425, UDPPort: 2425, Subnet: "192.168.1.0/24", Timestamp: 1, Epoch: 7}
	b := a
	if !bytes.Equal(a.SigningBytes(), b.SigningBytes()) {
		t.Fatal("not deterministic")
	}

	// 每个关键字段变化都必须改变签名内容
	variants := []Announcement{a, a, a, a, a}
	variants[0].TCPPort = 2426
	variants[1].UDPPort = 2426
	variants[2].Timestamp = 2
	variants[3].Epoch = 8
	variants[4].Subnet = "10.0.0.0/24"
	for i, v := range variants {
		if bytes.Equal(v.SigningBytes(), a.SigningBytes()) {
			t.Fatalf("variant %d did not change signing bytes", i)
		}
	}
}

func TestAnnouncementNonceAffectsSignature(t *testing.T) {
	a := Announcement{}
	b := Announcement{}
	b.Nonce[0] = 1
	if bytes.Equal(a.SigningBytes(), b.SigningBytes()) {
		t.Fatal("nonce must affect signing bytes")
	}
}

func TestPeerExpiredUsesProvidedNow(t *testing.T) {
	base := time.Unix(1000, 0)
	p := Peer{LastSeen: base}
	if p.Expired(base.Add(300*time.Second), 300*time.Second) {
		t.Fatal("exactly at TTL boundary must not be expired")
	}
	if !p.Expired(base.Add(301*time.Second), 300*time.Second) {
		t.Fatal("beyond TTL must be expired")
	}
}

func TestCapsCommonAndHas(t *testing.T) {
	a := CapsChatReliable | CapsGroupChat | CapsSlidingWindow
	b := CapsGroupChat | CapsEncrypted
	if got := a.Common(b); got != CapsGroupChat {
		t.Fatalf("common want GroupChat got %b", got)
	}
	if !a.Has(CapsChatReliable) {
		t.Fatal("Has failed")
	}
	if a.Has(CapsEncrypted) {
		t.Fatal("Has must require all bits")
	}
}

func TestParseSeedAddr(t *testing.T) {
	s, err := ParseSeedAddr("192.168.1.10:2425")
	if err != nil {
		t.Fatal(err)
	}
	if s.IP != "192.168.1.10" || s.UDPPort != 2425 {
		t.Fatalf("bad parse: %+v", s)
	}
	if s.String() != "192.168.1.10:2425" {
		t.Fatalf("bad String: %s", s.String())
	}
	for _, bad := range []string{"", "no-port", "1.2.3.4:", ":2425", "1.2.3.4:abc", "1.2.3.4:99999"} {
		if _, err := ParseSeedAddr(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}
