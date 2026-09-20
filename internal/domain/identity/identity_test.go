package identity

import (
	"bytes"
	"crypto/ed25519"
	"testing"
)

func TestGenerateAndSignVerify(t *testing.T) {
	kp, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("swarmlink-hello")
	sig := kp.Sign(msg)
	if !Verify(kp.PublicKey(), msg, sig) {
		t.Fatal("valid signature rejected")
	}
	if Verify(kp.PublicKey(), append([]byte(nil), append(msg, 1)...), sig) {
		t.Fatal("tampered message accepted")
	}
}

func TestNodeIDIs16HexChars(t *testing.T) {
	kp, _ := GenerateKeyPair()
	id := kp.NodeID().String()
	if len(id) != 16 {
		t.Fatalf("want 16 hex chars got %d (%q)", len(id), id)
	}
}

func TestFingerprintIsDeterministicAndDomainSeparated(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	a := Fingerprint(pub)
	b := Fingerprint(pub)
	if a != b {
		t.Fatal("fingerprint not deterministic")
	}
	// 域分隔：裸 SHA-256(pub)[:8] 必须与 Fingerprint 不同（否则域分隔失效）
	if bytes.Equal(a[:], pub[:8]) {
		t.Fatal("fingerprint looks like raw pubkey prefix")
	}
}

func TestParseNodeIDRoundTripAndErrors(t *testing.T) {
	kp, _ := GenerateKeyPair()
	s := kp.NodeID().String()
	got, err := ParseNodeID(s)
	if err != nil {
		t.Fatal(err)
	}
	if got != kp.NodeID() {
		t.Fatal("roundtrip mismatch")
	}
	for _, bad := range []string{"", "zz", "0011", "00112233445566778899"} {
		if _, err := ParseNodeID(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestNodeIDLessIsTotalOrder(t *testing.T) {
	var a, b NodeID
	a[7] = 1
	b[7] = 2
	if !a.Less(b) || b.Less(a) {
		t.Fatal("Less is not a correct order")
	}
	if a.Less(a) {
		t.Fatal("Less must be irreflexive")
	}
}

func TestPrivateKeyPEMRoundTrip(t *testing.T) {
	kp, _ := GenerateKeyPair()
	pemBytes, err := MarshalPrivateKeyPEM(kp.PrivateKey())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(pemBytes, []byte("PRIVATE KEY")) {
		t.Fatal("expected PKCS#8 PRIVATE KEY block")
	}
	priv, err := ParsePrivateKeyPEM(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	back, err := NewKeyPair(priv)
	if err != nil {
		t.Fatal(err)
	}
	if back.NodeID() != kp.NodeID() {
		t.Fatal("node id changed after PEM roundtrip")
	}
}

func TestParsePrivateKeyPEMRejectsGarbage(t *testing.T) {
	if _, err := ParsePrivateKeyPEM([]byte("not a pem")); err == nil {
		t.Fatal("expected error")
	}
}

func TestPublicKeyBase64RoundTrip(t *testing.T) {
	kp, _ := GenerateKeyPair()
	s := MarshalPublic(kp.PublicKey())
	pub, err := ParsePublic(s)
	if err != nil {
		t.Fatal(err)
	}
	if !pub.Equal(kp.PublicKey()) {
		t.Fatal("public key mismatch")
	}
	if _, err := ParsePublic("!!!not-base64!!!"); err == nil {
		t.Fatal("expected error")
	}
}

func TestVerifyRejectsBadSizes(t *testing.T) {
	if Verify(nil, []byte("x"), []byte("y")) {
		t.Fatal("nil pubkey must fail")
	}
	kp, _ := GenerateKeyPair()
	if Verify(kp.PublicKey(), []byte("x"), []byte("short")) {
		t.Fatal("bad signature size must fail")
	}
}
