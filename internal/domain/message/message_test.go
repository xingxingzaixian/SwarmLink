package message

import (
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
)

func TestNewIDIsUniqueAndParseable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewID()
		if seen[id] {
			t.Fatalf("duplicate msg id %s", id)
		}
		seen[id] = true
		if len(id) != 36 {
			t.Fatalf("uuid string form expected 36 chars got %d", len(id))
		}
	}
}

func TestSortInPlaceBySentAtThenMsgID(t *testing.T) {
	base := time.Unix(1000, 0)
	ms := []Message{
		{MsgID: "b", SentAt: base.Add(2 * time.Second)},
		{MsgID: "c", SentAt: base},
		{MsgID: "a", SentAt: base},
	}
	SortInPlace(ms)
	want := []string{"a", "c", "b"}
	for i, w := range want {
		if ms[i].MsgID != w {
			t.Fatalf("index %d want %s got %s", i, w, ms[i].MsgID)
		}
	}
}

func TestSortInPlaceStableForEqualKeys(t *testing.T) {
	ts := time.Unix(5, 0)
	ms := []Message{
		{MsgID: "x", SentAt: ts},
		{MsgID: "x", SentAt: ts},
	}
	SortInPlace(ms)
	if len(ms) != 2 {
		t.Fatal("lost messages")
	}
}

func TestDirectConvIDIsSymmetric(t *testing.T) {
	var p, q identity.NodeID
	p[0] = 0x01
	q[0] = 0x02

	// 这是本模块最容易写错的一处：若 conv_id 取「对端 ID」，
	// A 与 B 会各自落在不同的会话里，历史与去重全部分裂。
	if DirectConvID(p, q) != DirectConvID(q, p) {
		t.Fatal("direct conv id must be symmetric")
	}
	if !IsDirectConv(DirectConvID(p, q)) {
		t.Fatal("should be recognized as a direct conversation")
	}
}

func TestDirectPeerResolvesTheOtherSide(t *testing.T) {
	var p, q identity.NodeID
	p[0] = 0x01
	q[0] = 0x02

	conv := DirectConvID(p, q)
	got, ok := DirectPeer(conv, p)
	if !ok || got != q {
		t.Fatalf("DirectPeer(self=p) want %s got %s (ok=%v)", q, got, ok)
	}
	got, ok = DirectPeer(conv, q)
	if !ok || got != p {
		t.Fatalf("DirectPeer(self=q) want %s got %s (ok=%v)", p, got, ok)
	}
}

func TestGroupConvIsNotMistakenForDirect(t *testing.T) {
	gid := GroupConvID("0123456789abcdef0123456789abcdef")
	if gid != "0123456789abcdef0123456789abcdef" {
		t.Fatal("group conv id mismatch")
	}
	if IsDirectConv(gid) {
		t.Fatal("group conv id must not be treated as direct")
	}
	if _, ok := DirectPeer(gid, identity.NodeID{}); ok {
		t.Fatal("DirectPeer must reject group conv id")
	}
}
