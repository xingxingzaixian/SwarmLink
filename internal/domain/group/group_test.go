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
	if !bytes.Equal(g1.RosterSigningBytes(), g2.RosterSigningBytes()) {
		t.Fatal("signing bytes must not depend on member order")
	}
	g2.Epoch = 2
	if bytes.Equal(g1.RosterSigningBytes(), g2.RosterSigningBytes()) {
		t.Fatal("epoch must affect signing bytes")
	}
}

// 名册签名【只能】覆盖群主有权写的东西：
// 把本人的 left 纳入签名域，会让「成员一退出」就得劳烦群主重签一次。
func TestRosterSigningBytesExcludesLeft(t *testing.T) {
	// base 必须是【函数】而不是值：Members 是切片，直接拷贝再改元素会顺着共享
	// 底层数组改到「基准」自己，用例会以假绿灯通过。
	base := func() Group {
		return Group{
			ID: "g1", Name: "群", OwnerID: mkID(1), Epoch: 3, MintSeq: 9,
			Members: []Member{{NodeID: mkID(1), Role: RoleOwner, AdmitSeq: 1}},
		}
	}

	a := base()
	b := base()
	b.Members[0].LeftSeq = 5
	b.Members[0].LeftSig = []byte("x")
	if !bytes.Equal(a.RosterSigningBytes(), b.RosterSigningBytes()) {
		t.Error("left 变化不该改变名册签名字节")
	}

	// 群主有权写的东西必须【都】改变签名：漏掉任何一个，接收方就验不出篡改。
	for name, mutate := range map[string]func(*Group){
		"name":     func(g *Group) { g.Name = "改名" },
		"epoch":    func(g *Group) { g.Epoch++ },
		"mintSeq":  func(g *Group) { g.MintSeq++ },
		"state":    func(g *Group) { g.State = StateDissolved },
		"admitSeq": func(g *Group) { g.Members[0].AdmitSeq = 42 },
		"kickSeq":  func(g *Group) { g.Members[0].KickSeq = 7 },
	} {
		g := base()
		mutate(&g)
		// 取一份未被改动的基准。必须落到变量上：RosterSigningBytes 是
		// 指针方法，直接对 base() 的返回值调用不可寻址。
		ref := base()
		if bytes.Equal(ref.RosterSigningBytes(), g.RosterSigningBytes()) {
			t.Errorf("修改 %s 后签名字节不变 —— 该字段没有进入签名域", name)
		}
	}
}

// 成员顺序不同必须产生同一签名（否则各节点验签结论不一致）。
func TestRosterSigningBytesIsOrderIndependent(t *testing.T) {
	m := func(i byte) Member { return Member{NodeID: mkID(i), Role: RoleMember, AdmitSeq: 1} }
	a := Group{ID: "g", OwnerID: mkID(1), Members: []Member{m(2), m(3)}}
	b := Group{ID: "g", OwnerID: mkID(1), Members: []Member{m(3), m(2)}}
	if !bytes.Equal(a.RosterSigningBytes(), b.RosterSigningBytes()) {
		t.Error("成员顺序不同产生了不同签名字节")
	}
}

// 自签字节的构造只能有一份真相：签发与验签各写一遍迟早会分叉。
func TestLeftSigningBytesBindsGroupNodeAndSeq(t *testing.T) {
	base := LeftSigningBytes("g1", mkID(2), 7)
	if !bytes.Equal(base, LeftSigningBytes("g1", mkID(2), 7)) {
		t.Fatal("同样入参必须得到同样字节")
	}
	if bytes.Equal(base, LeftSigningBytes("g2", mkID(2), 7)) {
		t.Error("换一个群必须得到不同字节，否则退出签名可跨群重放")
	}
	if bytes.Equal(base, LeftSigningBytes("g1", mkID(3), 7)) {
		t.Error("换一个成员必须得到不同字节，否则可以伪造他人退出")
	}
	if bytes.Equal(base, LeftSigningBytes("g1", mkID(2), 8)) {
		t.Error("换一个票号必须得到不同字节")
	}
}

// 撕票只改状态、不删记录（保留以便审计与 epoch 一致），重新发票能把他拉回。
func TestKickKeepsRecordAndAdmitReactivates(t *testing.T) {
	owner := mkID(1)
	now := time.Unix(1700000000, 0)
	g := &Group{ID: "g", OwnerID: owner, MintSeq: 1,
		Members: []Member{{NodeID: owner, Role: RoleOwner, AdmitSeq: 1, State: MemberActive}}}
	if err := g.Admit(mkID(2), now); err != nil {
		t.Fatal(err)
	}
	if len(g.ActiveMembers()) != 2 {
		t.Fatalf("want 2 active got %d", len(g.ActiveMembers()))
	}

	g.Kick(mkID(2))
	if _, ok := g.Member(mkID(2)); !ok {
		t.Fatal("member record should be retained after kicking")
	}
	if len(g.ActiveMembers()) != 1 {
		t.Fatalf("踢人后 want 1 active got %d", len(g.ActiveMembers()))
	}

	if err := g.Admit(mkID(2), now); err != nil {
		t.Fatal(err)
	}
	if len(g.ActiveMembers()) != 2 {
		t.Fatal("重新发票应当把他拉回")
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

func TestMemberActiveRequiresStrictGreater(t *testing.T) {
	// 踢人 / 退出之后，「撕票号」恰好等于「票号」——撕的就是当前那张活票。
	// 因此判据必须是严格大于；写成 >= 会让踢人当场失效。
	cases := []struct {
		name string
		m    Member
		want bool
	}{
		{"admit == kick（刚被踢）", Member{AdmitSeq: 7, KickSeq: 7}, false},
		{"admit > kick", Member{AdmitSeq: 7, KickSeq: 6}, true},
		{"admit < kick", Member{AdmitSeq: 6, KickSeq: 7}, false},
		{"admit == left（刚退出）", Member{AdmitSeq: 7, LeftSeq: 7}, false},
		{"admit > left", Member{AdmitSeq: 7, LeftSeq: 6}, true},
		{"退出后被重新拉回（更高票号）", Member{AdmitSeq: 13, LeftSeq: 7}, true},
		{"退出后又踢（更高撕票号）", Member{AdmitSeq: 13, KickSeq: 14, LeftSeq: 7}, false},
	}
	for _, c := range cases {
		if got := c.m.Active(); got != c.want {
			t.Errorf("%s: Active() = %v，期望 %v（成员 %+v）", c.name, got, c.want, c.m)
		}
	}
}

// 存量数据与「只构造了成员、没填票号」的调用点必须继续按老语义工作 ——
// 否则一次模型升级会让所有群瞬间空掉。
func TestMemberActiveFallsBackToStateWhenNoSeqs(t *testing.T) {
	if !(Member{State: MemberActive}).Active() {
		t.Error("三个票号全为 0 时应退回按 State 判定为 active")
	}
	if (Member{State: MemberKicked}).Active() {
		t.Error("三个票号全为 0 时被踢成员不该是 active")
	}
	if !(Member{}).Active() {
		t.Error("State 是零值时应归一为 active")
	}
}
