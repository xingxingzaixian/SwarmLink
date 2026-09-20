package group

import (
	"bytes"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
)

func mkID(b byte) identity.NodeID {
	var n identity.NodeID
	n[7] = b
	return n
}

func TestNewIDDeterministic(t *testing.T) {
	owner := mkID(1)
	at := time.Unix(1700000000, 0).UTC()
	if NewID(owner, at, "team") != NewID(owner, at, "team") {
		t.Fatal("group id not deterministic")
	}
	if NewID(owner, at, "team") == NewID(owner, at, "other") {
		t.Fatal("name must affect group id")
	}
	if len(NewID(owner, at, "team")) != 32 {
		t.Fatal("group id must be 16 bytes hex = 32 chars")
	}
}

func TestSigningBytesIndependentOfMemberOrder(t *testing.T) {
	g1 := &Group{ID: "g", Epoch: 1, Members: []Member{
		{NodeID: mkID(3), Role: RoleMember, State: MemberActive},
		{NodeID: mkID(1), Role: RoleOwner, State: MemberActive},
		{NodeID: mkID(2), Role: RoleMember, State: MemberActive},
	}}
	g2 := &Group{ID: "g", Epoch: 1, Members: []Member{
		{NodeID: mkID(2), Role: RoleMember, State: MemberActive},
		{NodeID: mkID(3), Role: RoleMember, State: MemberActive},
		{NodeID: mkID(1), Role: RoleOwner, State: MemberActive},
	}}
	if !bytes.Equal(g1.SigningBytes(), g2.SigningBytes()) {
		t.Fatal("signing bytes must not depend on member order")
	}
	g2.Epoch = 2
	if bytes.Equal(g1.SigningBytes(), g2.SigningBytes()) {
		t.Fatal("epoch must affect signing bytes")
	}
}

func TestAddMemberEnforcesLimit(t *testing.T) {
	owner := mkID(1)
	g := &Group{ID: "g", OwnerID: owner}
	if err := g.AddMember(Member{NodeID: owner, Role: RoleOwner, State: MemberActive}); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= MaxMembers; i++ {
		if err := g.AddMember(Member{NodeID: mkID(byte(i)), Role: RoleMember, State: MemberActive}); err != nil {
			t.Fatalf("adding member %d: %v", i, err)
		}
	}
	if err := g.AddMember(Member{NodeID: mkID(200), Role: RoleMember, State: MemberActive}); err == nil {
		t.Fatal("expected member limit error")
	}
}

func TestAddExistingMemberReactivates(t *testing.T) {
	owner := mkID(1)
	g := &Group{ID: "g", OwnerID: owner}
	_ = g.AddMember(Member{NodeID: owner, Role: RoleOwner, State: MemberActive})
	_ = g.AddMember(Member{NodeID: mkID(2), Role: RoleMember, State: MemberActive})
	g.RemoveMember(mkID(2), MemberLeft)
	if _, ok := g.Member(mkID(2)); !ok {
		t.Fatal("member record should be retained after leaving")
	}
	if len(g.ActiveMembers()) != 1 {
		t.Fatalf("want 1 active got %d", len(g.ActiveMembers()))
	}
	_ = g.AddMember(Member{NodeID: mkID(2), Role: RoleMember})
	if len(g.ActiveMembers()) != 2 {
		t.Fatal("re-add should reactivate")
	}
}

func TestFanoutPlanExcludesSelfAndOffline(t *testing.T) {
	owner := mkID(1)
	g := &Group{ID: "g", OwnerID: owner, Members: []Member{
		{NodeID: owner, Role: RoleOwner, State: MemberActive},
		{NodeID: mkID(2), Role: RoleMember, State: MemberActive},
		{NodeID: mkID(3), Role: RoleMember, State: MemberActive},
		{NodeID: mkID(4), Role: RoleMember, State: MemberLeft},
	}}
	online := map[identity.NodeID]bool{mkID(2): true, mkID(3): false}
	plan := g.FanoutPlan(owner, online)
	if len(plan) != 1 || plan[0] != mkID(2) {
		t.Fatalf("unexpected plan: %v", plan)
	}
}

func TestValidate(t *testing.T) {
	owner := mkID(1)
	g := &Group{ID: "g", OwnerID: owner, Members: []Member{{NodeID: owner, Role: RoleOwner, State: MemberActive}}}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := &Group{ID: "", OwnerID: owner}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected error for empty id")
	}
	noOwner := &Group{ID: "g", OwnerID: mkID(9)}
	if err := noOwner.Validate(); err == nil {
		t.Fatal("expected error when owner missing")
	}
}
