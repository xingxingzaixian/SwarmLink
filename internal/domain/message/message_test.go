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
	// base 的两条按 msg_id 升序："a" 在 "c" 之前
	want := []string{"a", "c", "b"}
	for i, w := range want {
		if ms[i].MsgID != w {
			t.Fatalf("index %d want %s got %s", i, w, ms[i].MsgID)
		}
	}
}

func TestConvIDHelpers(t *testing.T) {
	var p identity.NodeID
	p[0] = 1
	if DirectConvID(p) != p.String() {
		t.Fatal("direct conv id mismatch")
	}
	if GroupConvID("gid") != "gid" {
		t.Fatal("group conv id mismatch")
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
