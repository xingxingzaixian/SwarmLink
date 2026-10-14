package group

import (
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
)

// 名册广播的受众必须【包含】已被移出 / 已退出的人。
//
// 用 FanoutPlan 当受众会把刚被踢的人排除掉（他已不在 active 集合里），
// 于是他永远收不到「你被踢了」—— 而离线被踢的人反而能靠上线推送得知，
// 同一个动作的两种结果取决于当时在不在线，属于最难解释的那类 bug。
func TestMetaAudienceIncludesRemovedMembers(t *testing.T) {
	oid, kept, gone := mkID(1), mkID(2), mkID(3)
	g := Group{ID: "g", OwnerID: oid, Members: []Member{
		{NodeID: oid, Role: RoleOwner, AdmitSeq: 1},
		{NodeID: kept, Role: RoleMember, AdmitSeq: 2},
		// gone 已被移出：kickSeq 等于票号。
		{NodeID: gone, Role: RoleMember, AdmitSeq: 3, KickSeq: 3},
	}}
	online := map[identity.NodeID]bool{kept: true, gone: true}

	// 群消息：只发给在群的人。
	plan := g.FanoutPlan(oid, online)
	if len(plan) != 1 || plan[0] != kept {
		t.Fatalf("FanoutPlan 不该包含已移出的人，得到 %v", plan)
	}

	// 名册广播：名册里出现过的所有人都要发（除自己）。
	aud := g.MetaAudience(oid, online)
	var hasGone, hasSelf bool
	for _, id := range aud {
		if id == gone {
			hasGone = true
		}
		if id == oid {
			hasSelf = true
		}
	}
	if !hasGone {
		t.Errorf("已移出的成员必须在名册广播受众里，得到 %v", aud)
	}
	if hasSelf {
		t.Error("名册广播不该发给自己")
	}
	if len(aud) != 2 {
		t.Errorf("受众应为 2 人（含已移出者），得到 %v", aud)
	}
}

// 合并的三条性质。判据写错不会报错，只会让两台机器对同一名册得出不同结论 ——
// 那是这类 CRDT 最难排查的故障，所以用表驱动把性质钉死。
func TestMergeLeftTakesMaxAndIsIdempotent(t *testing.T) {
	mid, oid := mkID(2), mkID(1)
	base := func() Group {
		return Group{ID: "g", OwnerID: oid, Members: []Member{
			{NodeID: oid, Role: RoleOwner, AdmitSeq: 1},
			{NodeID: mid, Role: RoleMember, AdmitSeq: 5},
		}}
	}

	g := base()
	if !g.MergeLeft(mid, 5, []byte("sig5")) {
		t.Fatal("首次合并应当报告变化")
	}
	if g.MergeLeft(mid, 5, []byte("sig5")) {
		t.Error("重复合并同一份 left 必须是无副作用的（幂等）")
	}
	if !g.MergeLeft(mid, 9, []byte("sig9")) {
		t.Fatal("更高票号应当被采纳")
	}
	if g.MergeLeft(mid, 7, []byte("sig7")) {
		t.Error("更低票号必须被忽略（单调）")
	}
	m, ok := g.Member(mid)
	if !ok {
		t.Fatal("成员应当仍在名册里（退出不删记录）")
	}
	if m.LeftSeq != 9 || string(m.LeftSig) != "sig9" {
		t.Errorf("最终 left = %d/%q，期望 9/sig9", m.LeftSeq, string(m.LeftSig))
	}
}

// 交换律：两份 left 以任意顺序到达，结果必须一致。
func TestMergeLeftIsCommutative(t *testing.T) {
	mid, oid := mkID(2), mkID(1)
	base := func() Group {
		return Group{ID: "g", OwnerID: oid, Members: []Member{{NodeID: mid, Role: RoleMember, AdmitSeq: 5}}}
	}
	a, b := base(), base()
	a.MergeLeft(mid, 7, []byte("s7"))
	a.MergeLeft(mid, 9, []byte("s9"))
	b.MergeLeft(mid, 9, []byte("s9"))
	b.MergeLeft(mid, 7, []byte("s7"))
	ma, _ := a.Member(mid)
	mb, _ := b.Member(mid)
	if ma.LeftSeq != mb.LeftSeq || string(ma.LeftSig) != string(mb.LeftSig) {
		t.Errorf("两种到达顺序结果不同：%d/%q vs %d/%q", ma.LeftSeq, ma.LeftSig, mb.LeftSeq, mb.LeftSig)
	}
}

// 名册里没有这个人时不做任何事：那说明我比对方旧，该由 owner 快照去补他，
// 而不是凭空插一条没有 admit 记录的成员。
func TestMergeLeftIgnoresUnknownMember(t *testing.T) {
	g := Group{ID: "g", OwnerID: mkID(1), Members: []Member{{NodeID: mkID(1), Role: RoleOwner, AdmitSeq: 1}}}
	if g.MergeLeft(mkID(9), 3, []byte("s3")) {
		t.Error("未知成员的退出必须被忽略")
	}
	if len(g.Members) != 1 {
		t.Errorf("不该为未知成员插入记录，现在有 %d 条", len(g.Members))
	}
}

// owner 快照按 Epoch 原子替换：更高 epoch 才采纳，且【整块】替换 ——
// 绝不逐字段取 max（那会拼出一份没人签过的名册，见 ADR-012 Decision §5.2）。
func TestMergeOwnerSnapshotReplacesAtomically(t *testing.T) {
	oid, a, b := mkID(1), mkID(2), mkID(3)
	cur := Group{ID: "g", OwnerID: oid, Epoch: 5, MintSeq: 5, Name: "新名",
		Members: []Member{{NodeID: oid, Role: RoleOwner, AdmitSeq: 5}, {NodeID: a, Role: RoleMember, AdmitSeq: 4}}}
	in := Group{ID: "g", OwnerID: oid, Epoch: 4, MintSeq: 4, Name: "旧名",
		Members: []Member{{NodeID: oid, Role: RoleOwner, AdmitSeq: 5}, {NodeID: b, Role: RoleMember, AdmitSeq: 3}}}

	if cur.MergeOwnerSnapshot(in) {
		t.Error("更低 epoch 的快照必须被忽略")
	}
	if cur.Name != "新名" || len(cur.Members) != 2 {
		t.Error("更低 epoch 的快照改动了本地状态")
	}
	if _, ok := cur.Member(a); !ok {
		t.Error("更低 epoch 不该把本地成员换成对方的")
	}

	newer := in
	newer.Epoch, newer.MintSeq, newer.Name = 6, 6, "更新"
	if !cur.MergeOwnerSnapshot(newer) {
		t.Fatal("更高 epoch 应当被采纳")
	}
	if cur.Name != "更新" || cur.Epoch != 6 || cur.MintSeq != 6 {
		t.Errorf("群级字段未整块替换：%+v", cur)
	}
	if _, ok := cur.Member(b); !ok {
		t.Error("更高 epoch 应当整块替换成员表")
	}
	if _, ok := cur.Member(a); ok {
		t.Error("整块替换时不该保留对方名册里没有的成员")
	}
}

// 替换 owner 块【不能】丢掉本地已合并的 left 覆盖：
// 丢掉会让「成员退出」在一次正常的名册同步之后被静默抹掉 —— 退出就白退了。
func TestMergeOwnerSnapshotKeepsLocalLeftOverrides(t *testing.T) {
	oid, mid := mkID(1), mkID(2)
	cur := Group{ID: "g", OwnerID: oid, Epoch: 5, MintSeq: 5,
		Members: []Member{{NodeID: oid, Role: RoleOwner, AdmitSeq: 5}, {NodeID: mid, Role: RoleMember, AdmitSeq: 4}}}
	cur.MergeLeft(mid, 4, []byte("mysig")) // 本地已知：mid 自己撕掉了 4 号票

	// 群主发来一份更新的名册，里面 mid 仍带着 4 号票（群主还不知道他退出了）。
	newer := Group{ID: "g", OwnerID: oid, Epoch: 6, MintSeq: 6,
		Members: []Member{{NodeID: oid, Role: RoleOwner, AdmitSeq: 5}, {NodeID: mid, Role: RoleMember, AdmitSeq: 4}}}
	if !cur.MergeOwnerSnapshot(newer) {
		t.Fatal("更高 epoch 应当被采纳")
	}
	m, _ := cur.Member(mid)
	if m.LeftSeq != 4 || string(m.LeftSig) != "mysig" {
		t.Fatalf("替换 owner 块后 left 覆盖被抹掉了：%d/%q", m.LeftSeq, string(m.LeftSig))
	}
	if m.Active() {
		t.Error("mid 已退出，替换名册后不该变回在群")
	}
}

// 退出后拉回：这是把「每用户各自计数器」方案否决掉的那个反例，必须钉死。
// 群主无需知道对方撕到几号，只要发一张【更高票号】的票就必然生效。
func TestReAddAfterLeaveAlwaysWins(t *testing.T) {
	m := Member{AdmitSeq: 4, LeftSeq: 4} // 4 号票被本人撕掉
	if m.Active() {
		t.Fatal("刚退出时不该在群")
	}
	m.AdmitSeq = 13 // 群主重新发票（群级计数器，无需知道对方撕到 4）
	if !m.Active() {
		t.Fatal("重新发票后必须生效 —— 这正是否决「每用户各自计数器」的理由")
	}
}

// 群主侧的发票必须真的推进群级票号，且【恒大于】此前发过的所有票号 ——
// 「新票必胜」这条性质一旦被破坏，拉回就会随机失效。
func TestAdmitAlwaysMintsStrictlyHigherSeq(t *testing.T) {
	owner, m2 := mkID(1), mkID(2)
	now := time.Unix(1700000000, 0)
	g := Group{ID: "g", OwnerID: owner, MintSeq: 1,
		Members: []Member{{NodeID: owner, Role: RoleOwner, AdmitSeq: 1, State: MemberActive}}}

	var prev int64 = 1
	for i := 0; i < 3; i++ {
		if err := g.Admit(m2, now); err != nil {
			t.Fatalf("第 %d 次发票失败：%v", i+1, err)
		}
		m, _ := g.Member(m2)
		if m.AdmitSeq <= prev {
			t.Fatalf("第 %d 次发票的票号 %d 没有大于上一次的 %d", i+1, m.AdmitSeq, prev)
		}
		prev = m.AdmitSeq
	}

	// 退出之后群主再发票，必须重新生效。
	g.Kick(m2)
	if m, _ := g.Member(m2); m.Active() {
		t.Fatal("踢人后不该在群")
	}
	if err := g.Admit(m2, now); err != nil {
		t.Fatal(err)
	}
	if m, _ := g.Member(m2); !m.Active() {
		t.Fatal("重新发票后必须生效")
	}
}

// 上限按「接纳后会变成几个 active」算：被踢的人重新拉回同样占一个名额。
func TestAdmitEnforcesLimit(t *testing.T) {
	owner := mkID(1)
	now := time.Unix(1700000000, 0)
	g := Group{ID: "g", OwnerID: owner, MintSeq: 1,
		Members: []Member{{NodeID: owner, Role: RoleOwner, AdmitSeq: 1, State: MemberActive}}}
	for i := 2; i <= MaxMembers; i++ {
		if err := g.Admit(mkID(byte(i)), now); err != nil {
			t.Fatalf("加入第 %d 个成员：%v", i, err)
		}
	}
	if err := g.Admit(mkID(200), now); err == nil {
		t.Fatal("超过上限必须报错")
	}
	// 已在群里的人再确认一次不占新名额。
	if err := g.Admit(mkID(2), now); err != nil {
		t.Fatalf("重复确认已在群成员不该失败：%v", err)
	}
}

// 名册摘要必须把两块内容都纳入：只覆盖一块会让「我漏了对方的退出」永远发现不了。
func TestRosterDigestCoversBothBlocks(t *testing.T) {
	base := func() Group {
		return Group{ID: "g", OwnerID: mkID(1), Epoch: 3, MintSeq: 3, Members: []Member{
			{NodeID: mkID(1), Role: RoleOwner, AdmitSeq: 1},
			{NodeID: mkID(2), Role: RoleMember, AdmitSeq: 3},
		}}
	}
	ref := base()

	// DisplayName 是非权威提示：改了它不该触发同步。
	withName := base()
	withName.Members[1].DisplayName = "换了昵称"
	if string(ref.RosterDigest()) != string(withName.RosterDigest()) {
		t.Error("DisplayName 变化不该改变名册摘要")
	}

	// 退出必须改变摘要（否则对端摘要一致就不会来同步，退出永远传不出去）。
	withLeft := base()
	withLeft.MergeLeft(mkID(2), 3, []byte("s"))
	if string(ref.RosterDigest()) == string(withLeft.RosterDigest()) {
		t.Error("left 变化必须改变名册摘要")
	}
}
