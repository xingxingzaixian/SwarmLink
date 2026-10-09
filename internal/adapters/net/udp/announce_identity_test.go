package udp

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
)

// announcementFrom 造一条身份自洽、签名有效的通告。
func announcementFrom(kp *identity.KeyPair) peer.Announcement {
	a := peer.Announcement{
		NodeID:      kp.NodeID(),
		DisplayName: "正常种子",
		PublicKey:   kp.PublicKey(),
		TCPPort:     2425,
		UDPPort:     2425,
		Subnet:      "10.0.0.0/24",
		Timestamp:   time.Now().Unix(),
		Source:      peer.SourceSeed,
	}
	a.Sig = kp.Sign(a.SigningBytes())
	return a
}

// announcementFor 造一条【签名有效、但身份自相矛盾】的通告：
// 用 attacker 自己的密钥签名，却声称 claimed 的身份。
//
// 这种报文能通过「用报文自带的公钥验签」那一层 —— 因为签名与公钥确实配套。
// 要拦下它，必须额外校验 node_id == Fingerprint(public_key)。
func announcementFor(claimed identity.NodeID, attacker *identity.KeyPair) peer.Announcement {
	a := peer.Announcement{
		NodeID:      claimed,              // ← 声称的身份
		DisplayName: "冒充者",
		PublicKey:   attacker.PublicKey(), // ← 真正持有私钥的人
		TCPPort:     2425,
		UDPPort:     2425,
		Subnet:      "10.0.0.0/24",
		Timestamp:   time.Now().Unix(),
		Source:      peer.SourceSeed,
	}
	// 签名本身是真的，只是签的不是它声称的那个身份。
	a.Sig = attacker.Sign(a.SigningBytes())
	return a
}

// newDecodeFixture 造一个只用于解码判定的 Broadcaster（不 Start，不开 socket）。
func newDecodeFixture(t *testing.T) *Broadcaster {
	t.Helper()
	self, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	cfg := DefaultConfig()
	cfg.SeedOnly = true
	return NewBroadcaster(self, cfg, clock.New(), nil)
}

// fakeSeed 起一个收到探测就回一条指定 ANNOUNCE 的假种子，返回它的 UDP 端口。
//
// 为什么抽成 helper：「拒绝伪造通告」那条用例断言的是 Probe 返回错误 —— 而假种子
// 若根本没把报文发出去，Probe 同样会超时返回错误，用例就会【空跑通过】。
// 因此必须有一条配对用例（TestProbeAcceptsConsistentAnnounce）用同一套装置证明
// 它确实在回包。通告在进入 goroutine 之前就构造好：goroutine 里不能调 t.Fatalf。
func fakeSeed(t *testing.T, a peer.Announcement) uint16 {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	go func() {
		buf := make([]byte, 2048)
		if _, src, rerr := pc.ReadFromUDP(buf); rerr != nil {
			return
		} else {
			payload, perr := protocol.EncodeUDP(protocol.UDPTypeAnnounce, protocol.NewAnnounceWire(a))
			if perr != nil {
				return
			}
			_, _ = pc.WriteToUDP(payload, src)
		}
	}()

	return uint16(pc.LocalAddr().(*net.UDPAddr).Port)
}

// TestDecodeAnnounceRejectsSelfInconsistentIdentity 钉住「node_id 必须与公钥自洽」
// 在【广播解码路径】上的行为。
//
// TCP 握手侧一直有这条校验（handshake.go:141 与 :222），UDP 侧却只做了「用报文自带
// 的公钥验签」。于是任何人都能用自己的密钥把目录塞满身份自相矛盾的条目，而目录当时
// 连上限都没有 —— 这是一条低成本的污染面（《发现协议设计书》§15.1）。
//
// ANNOUNCE 与 BYE 共用这条解码路径（BYE 也带签名），因此修在这里同时也挡住了
// 「伪造别人的下线通知」。
func TestDecodeAnnounceRejectsSelfInconsistentIdentity(t *testing.T) {
	b := newDecodeFixture(t)

	victim, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	attacker, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}

	body, err := json.Marshal(protocol.NewAnnounceWire(announcementFor(victim.NodeID(), attacker)))
	if err != nil {
		t.Fatalf("marshal announce: %v", err)
	}

	// 前置条件：这条报文的签名本身必须有效。否则本用例证明不了任何事 ——
	// 它会被「签名不过」拒掉，而不是被「身份不自洽」拒掉，两种拒绝原因完全不同。
	var w protocol.AnnounceWire
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	decoded, err := w.Announcement()
	if err != nil {
		t.Fatalf("Announcement(): %v", err)
	}
	if !identity.Verify(decoded.PublicKey, decoded.SigningBytes(), decoded.Sig) {
		t.Fatal("前置条件不成立：伪造报文的签名本身应当是有效的")
	}

	if _, ok := b.decodeAnnounce(body); ok {
		t.Fatal("声称他人 node_id 的通告必须被丢弃：UDP 层缺少 node_id == Fingerprint(public_key) 校验")
	}
}

// TestDecodeAnnounceAcceptsConsistentIdentity：正常的通告不能被误伤。
func TestDecodeAnnounceAcceptsConsistentIdentity(t *testing.T) {
	b := newDecodeFixture(t)

	kp, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	body, err := json.Marshal(protocol.NewAnnounceWire(announcementFrom(kp)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, ok := b.decodeAnnounce(body)
	if !ok {
		t.Fatal("身份自洽且签名有效的通告被误拒")
	}
	if got.NodeID != kp.NodeID() {
		t.Fatalf("NodeID = %s，期望 %s", got.NodeID, kp.NodeID())
	}
}

// TestProbeAcceptsConsistentAnnounce 是 fakeSeed 装置的【活性证明】：
// 同一套装置回一条自洽通告时，探测必须成功。没有它，下面那条「拒绝」用例
// 即使在假种子压根没回包的情况下也会通过。
func TestProbeAcceptsConsistentAnnounce(t *testing.T) {
	kp, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	port := fakeSeed(t, announcementFrom(kp))

	p := &Prober{Timeout: 500 * time.Millisecond}
	got, err := p.Probe(context.Background(), peer.SeedAddr{IP: "127.0.0.1", UDPPort: port})
	if err != nil {
		t.Fatalf("自洽通告的种子探测失败（装置本身没跑起来？）: %v", err)
	}
	if got.NodeID != kp.NodeID() {
		t.Fatalf("NodeID = %s，期望 %s", got.NodeID, kp.NodeID())
	}
}

// TestProbeRejectsSelfInconsistentAnnounce 覆盖【第二个站点】：种子探测路径
// （seed.go 的 readAnnounce 循环）。
//
// 为什么要单独测：种子走的是自己的一套解码循环，不经过 Broadcaster.decodeAnnounce。
// 两处漏一处，攻击者就仍能把自相矛盾的条目从种子灌进别人的目录 —— 而种子是
// 跨网段唯一的入口，污染面比同网段广播更大。
func TestProbeRejectsSelfInconsistentAnnounce(t *testing.T) {
	victim, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	attacker, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	port := fakeSeed(t, announcementFor(victim.NodeID(), attacker))

	p := &Prober{Timeout: 500 * time.Millisecond}
	if got, err := p.Probe(context.Background(), peer.SeedAddr{IP: "127.0.0.1", UDPPort: port}); err == nil {
		t.Fatalf("种子回了一条身份自相矛盾的通告（声称 %s），探测必须丢弃它，却拿到了 %s",
			victim.NodeID(), got.NodeID)
	}
}
