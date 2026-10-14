# 群成员「发票 / 撕票」模型 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 让群成员状态由「票号」决定（群主发票/踢人、本人撕票退出），从而支持**离线主动退出**，并让离线成员上线后**双向对齐**。

**Architecture:** 成员资格 = `AdmitSeq > KickSeq && AdmitSeq > LeftSeq`（严格大于）。名册分**两块独立认证**的内容：群主签名的「群级字段 + 每成员 admit/kick」（按 `Epoch` **原子替换**），与各成员自签的「left 覆盖」（按成员 **取 max**）。合并是集合/取 max 运算，不依赖时钟、不依赖全局全序。新增 `GroupApp.Start` 在 peer 上线/发现时推送与拉取，另有 60s 抽查兜底。

**Tech Stack:** Go 1.26（`crypto/ed25519`、`modernc.org/sqlite`）、Wails v3 + Vue3 + Pinia + TS。

**权威文档**：`docs/adr/ADR-012-group-membership-token-crdt.md`、`docs/plans/2026-10-11-group-membership-token-design.md`

**通用命令**

| 目的 | 命令 |
|---|---|
| 单包测试 | `go test ./internal/domain/group/ -run TestXxx -v` |
| 全量测试（含 Wails） | `go test -tags wails ./...` |
| 竞态 | `go test -race ./internal/app/ ./internal/adapters/store/...` |
| 架构红线 | `go test ./internal/archtest/` |
| 前端 | `cd frontend && npm run typecheck && npm run test` |
| 绑定重生成 | `wails3 generate bindings -ts -i -f "-tags wails" -clean .` |

---

## Task 1：领域 —— 票号字段与 `Active()`

**Files:**
- Modify: `internal/domain/group/group.go`
- Test: `internal/domain/group/group_test.go`

**Step 1：写失败测试**

追加到 `internal/domain/group/group_test.go`（若文件尚未导入 `bytes`，先补上 `import "bytes"`）：

```go
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
```

**Step 2：跑测试确认失败**

Run: `go test ./internal/domain/group/ -run "TestMemberActive" -v`
Expected: 编译失败 —— `m.Active undefined`。

**Step 3：实现**

在 `internal/domain/group/group.go` 的 `MemberState` 常量块之后加：

```go
// StateOrActive 把零值归一为 active（与 GroupState.StateOrActive 同一理由：
// 历史数据与「只构造了成员没填状态」的构造点都该被当成在群）。
func (s MemberState) StateOrActive() MemberState {
	if s == "" {
		return MemberActive
	}
	return s
}
```

把 `Member` 改成：

```go
// Member 是群成员。
//
// 成员资格由「票号」决定（见 Active）：群主发票（AdmitSeq）、群主撕票（KickSeq）、
// 本人撕票（LeftSeq）。State 退化为【派生缓存】—— 落库时按 Active() 写回，
// 读取时不再作为判据（见 ADR-012）。
type Member struct {
	NodeID      identity.NodeID
	DisplayName string // 非权威提示：不进签名域，冲突时以本地节点目录为准
	Role        MemberRole
	JoinedAt    time.Time
	State       MemberState // 派生缓存；判据见 Active()

	// AdmitSeq 是最近一次「发票」的票号（0 = 从未被准入）。
	AdmitSeq int64
	// KickSeq 是群主最近一次「撕票」的票号（0 = 未被踢过）。
	KickSeq int64
	// LeftSeq 是本人最近一次「撕票」的票号（0 = 未退出过）。
	LeftSeq int64
	// LeftSig 是本人对 H(group_id|node_id|left_seq) 的签名；LeftSeq==0 时为空。
	LeftSig []byte
}

// Active 报告该成员当前是否在群里。
//
// 【必须是严格大于】踢人/退出之后 KickSeq（或 LeftSeq）恰好等于 AdmitSeq
// —— 撕的就是当前那张活票；写成 >= 会让踢人当场失效。
//
// 【兼容回退】三个票号全为 0 时退回看 State：升级前的存量数据、以及测试里
// 直接构造的成员都走这条路径，避免一次模型升级让所有群空掉。
func (m Member) Active() bool {
	if m.AdmitSeq == 0 && m.KickSeq == 0 && m.LeftSeq == 0 {
		return m.State.StateOrActive() == MemberActive
	}
	return m.AdmitSeq > m.KickSeq && m.AdmitSeq > m.LeftSeq
}
```

**Step 4：把既有判定点切到 `Active()`**

在同一个文件里替换（语义等价，但为后续票号路径铺路）：

```go
// ActiveMembers 返回当前在群的成员。
func (g *Group) ActiveMembers() []Member {
	out := make([]Member, 0, len(g.Members))
	for _, m := range g.Members {
		if m.Active() {
			out = append(out, m)
		}
	}
	return out
}

// FanoutPlan 返回扇出目标：当前在群且在线的成员（不含自己），上限 MaxMembers。
func (g *Group) FanoutPlan(self identity.NodeID, online map[identity.NodeID]bool) []identity.NodeID {
	out := make([]identity.NodeID, 0, len(g.Members))
	for _, m := range g.Members {
		if !m.Active() || m.NodeID == self {
			continue
		}
		if online != nil && !online[m.NodeID] {
			continue
		}
		out = append(out, m.NodeID)
		if len(out) >= MaxMembers {
			break
		}
	}
	return out
}
```

**Step 5：跑测试**

Run: `go test ./internal/domain/group/ -v`
Expected: PASS（含既有用例 —— 它们构造的成员没有票号，走兼容回退）。

**Step 6：提交**

```bash
git add internal/domain/group/group.go internal/domain/group/group_test.go
git commit -m "feat(group): add token-seq fields and Active() to Member"
```

---

## Task 2：领域 —— 群级字段、名册签名域

**Files:**
- Modify: `internal/domain/group/group.go`
- Test: `internal/domain/group/group_test.go`

**Step 1：写失败测试**

```go
// 名册签名【只能】覆盖群主有权写的东西：
// 把本人的 left 纳入签名域，会让「成员一退出」就得劳烦群主重签一次。
func TestRosterSigningBytesExcludesLeft(t *testing.T) {
	// base 必须是【函数】而不是值：Members 是切片，直接拷贝再改元素会
	// 顺着共享底层数组改到「基准」自己，用例会以假绿灯通过。
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
		if bytes.Equal(base().RosterSigningBytes(), g.RosterSigningBytes()) {
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
```

**Step 2：跑测试确认失败**

Run: `go test ./internal/domain/group/ -run TestRosterSigningBytes -v`
Expected: 编译失败 —— `RosterSigningBytes undefined`。

**Step 3：实现**

`Group` 加 `MintSeq`，`StateSig` 改名 `RosterSig`：

```go
// Group 是群聚合。
//
// 名册分两块独立认证的内容（见 ADR-012）：
//   - 群主签名块：ID/Name/State/Epoch/MintSeq + 每成员的 AdmitSeq/KickSeq，按 Epoch 原子替换；
//   - left 覆盖：每成员自签的 LeftSeq/LeftSig，按成员取 max。
type Group struct {
	ID      string
	Name    string
	OwnerID identity.NodeID
	// Epoch 是群主签名块的序号：只在群主侧递增，用于「整块替换」的先后判定。
	Epoch int64
	// MintSeq 是群主的【群级】发票计数器，单调递增（ADR-012 Decision §2）。
	MintSeq int64
	// State 是群整体状态（零值视为 active）。
	State GroupState
	// RosterSig 是群主对 RosterSigningBytes() 的签名。
	RosterSig []byte
	Members   []Member
	CreatedAt time.Time
}

// RosterSigningBytes 返回群主待签名的规范字节。
//
// 覆盖：group_id | name | state | epoch | mint_seq | Σ(nodeID:admitSeq:kickSeq)
// 【不含】LeftSeq/LeftSig（本人签名域）、DisplayName（非权威提示）。
// 成员按 NodeID 升序，保证同一名册产生同一签名（确定性）。
func (g *Group) RosterSigningBytes() []byte {
	members := append([]Member(nil), g.Members...)
	sort.SliceStable(members, func(i, j int) bool { return members[i].NodeID.Less(members[j].NodeID) })

	var tmp [8]byte
	buf := make([]byte, 0, 96+len(members)*48)
	buf = append(buf, g.ID...)
	buf = append(buf, '|')
	buf = append(buf, g.Name...)
	buf = append(buf, '|')
	buf = append(buf, string(g.State.StateOrActive())...)
	buf = append(buf, '|')
	binary.BigEndian.PutUint64(tmp[:], uint64(g.Epoch))
	buf = append(buf, tmp[:]...)
	binary.BigEndian.PutUint64(tmp[:], uint64(g.MintSeq))
	buf = append(buf, tmp[:]...)
	for i, m := range members {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, m.NodeID[:]...)
		binary.BigEndian.PutUint64(tmp[:], uint64(m.AdmitSeq))
		buf = append(buf, tmp[:]...)
		binary.BigEndian.PutUint64(tmp[:], uint64(m.KickSeq))
		buf = append(buf, tmp[:]...)
	}
	sum := sha256.Sum256(buf)
	return sum[:]
}

// LeftSigningBytes 返回成员本人待签名的规范字节：H(group_id|node_id|left_seq)。
// 放在领域层是为了「签名什么」只有一份真相 —— 签发与验签各写一遍迟早会分叉。
func LeftSigningBytes(groupID string, nodeID identity.NodeID, leftSeq int64) []byte {
	var tmp [8]byte
	buf := make([]byte, 0, len(groupID)+len(nodeID)+16)
	buf = append(buf, groupID...)
	buf = append(buf, '|')
	buf = append(buf, nodeID[:]...)
	binary.BigEndian.PutUint64(tmp[:], uint64(leftSeq))
	buf = append(buf, tmp[:]...)
	sum := sha256.Sum256(buf)
	return sum[:]
}
```

删掉旧的 `SigningBytes()`（已被 `RosterSigningBytes` 取代），并修正 `Validate()` 里的注释。`Validate()` 另加一条：

```go
	// 群主必须是自己名册里的一位 owner —— 且必须是在群的
	// （被踢/退出的 owner 会让群进入无法变更的死状态）。
	if !owner.Active() {
		return fmt.Errorf("group: owner %s is not active", g.OwnerID)
	}
```

**Step 4：修所有引用点**

Run: `go build ./...`
Expected: 报错集中在 `g.SigningBytes()` / `g.StateSig` 的引用处。逐个改为 `g.RosterSigningBytes()` / `g.RosterSig`（`internal/app/group_app.go`、`internal/adapters/store/sqlite/groups.go`、`internal/adapters/store/mem/mem.go` 及各自测试）。

**Step 5：跑测试**

Run: `go test ./internal/domain/... ./internal/app/ ./internal/adapters/store/...`
Expected: PASS。

**Step 6：提交**

```bash
git add -A internal/
git commit -m "refactor(group): split roster signing bytes from member-signed left"
```

---

## Task 3：领域 —— 操作与合并（收敛性）

**Files:**
- Modify: `internal/domain/group/group.go`
- Test: `internal/domain/group/merge_test.go`（新建）

**Step 1：写失败测试**

```go
// 合并的三条性质。判据写错不会报错，只会让两台机器对同一名册得出不同结论 ——
// 那是这类 CRDT 最难排查的故障，所以用表驱动把性质钉死。
func TestMergeLeftTakesMaxAndIsIdempotent(t *testing.T) {
	mid, oid := mkID(2), mkID(1)
	base := Group{ID: "g", OwnerID: oid, Members: []Member{
		{NodeID: oid, Role: RoleOwner, AdmitSeq: 1},
		{NodeID: mid, Role: RoleMember, AdmitSeq: 5},
	}}

	g := base
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
	if m, _ := g.Member(mid); m.LeftSeq != 9 || string(m.LeftSig) != "sig9" {
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

	newer := in
	newer.Epoch, newer.MintSeq, newer.Name = 6, 6, "更新"
	if !cur.MergeOwnerSnapshot(newer) {
		t.Fatal("更高 epoch 应当被采纳")
	}
	if cur.Name != "更新" || len(cur.Members) != 2 {
		t.Error("更高 epoch 应当整块替换")
	}
}

// 退出后拉回：这是把「每用户各自计数器」方案否决掉的那个反例，必须钉死。
// 群主无需知道对方撕到几号，只要发一张【更高票号】的票就必然生效。
func TestReAddAfterLeaveAlwaysWins(t *testing.T) {
	mid := mkID(2)
	m := Member{AdmitSeq: 4, LeftSeq: 4} // 4 号票被本人撕掉
	if m.Active() {
		t.Fatal("刚退出时不该在群")
	}
	m.AdmitSeq = 13 // 群主重新发票（群级计数器，无需知道对方撕到 4）
	if !m.Active() {
		t.Fatal("重新发票后必须生效 —— 这正是否决「每用户各自计数器」的理由")
	}
}
```

**Step 2：跑测试确认失败**

Run: `go test ./internal/domain/group/ -run "TestMerge|TestReAdd" -v`
Expected: 编译失败 —— `MergeLeft undefined`。

**Step 3：实现**

```go
// MergeLeft 合并一条「本人撕票」（退出）记录：逐成员取 max。
//
// 返回是否带来了实质变化 —— 调用方据此决定要不要落库与广播事件：
// 摘要一致的重复投递不该触发一次 UI 刷新风暴。
func (g *Group) MergeLeft(nodeID identity.NodeID, leftSeq int64, sig []byte) bool {
	for i := range g.Members {
		if g.Members[i].NodeID != nodeID {
			continue
		}
		if leftSeq <= g.Members[i].LeftSeq {
			return false // 幂等 + 单调：更低或相等的票号一律忽略
		}
		g.Members[i].LeftSeq = leftSeq
		g.Members[i].LeftSig = sig
		return true
	}
	return false // 名册里没有这个人（我比对方旧）：留给 owner 快照去补
}

// MergeOwnerSnapshot 用一份【群主签名】的快照整块替换本地群级字段与成员票号。
//
// 只在 in.Epoch 更高时采纳，且是【整块】替换：
// 若逐字段取 max，拼出来的名册不对应任何一份合法签名，而本机还会把它当中继转发出去
// —— 那等于以群主之名散布一份没人签过的名册（ADR-012 Decision §5.2）。
//
// 调用方必须先验签（本层不持有公钥，保持 domain 零 I/O 与纯函数）。
func (g *Group) MergeOwnerSnapshot(in Group) bool {
	if in.Epoch <= g.Epoch {
		return false
	}
	// left 覆盖由本人签名、独立演进，替换 owner 块时【不能】丢：
	// 丢掉会让「成员退出」在上线后被一次正常的名册同步抹掉。
	overrides := make(map[identity.NodeID]leftOverride, len(g.Members))
	for _, m := range g.Members {
		if m.LeftSeq > 0 {
			overrides[m.NodeID] = leftOverride{seq: m.LeftSeq, sig: m.LeftSig}
		}
	}

	g.Name, g.State, g.Epoch, g.MintSeq, g.RosterSig = in.Name, in.State, in.Epoch, in.MintSeq, in.RosterSig
	g.Members = append([]Member(nil), in.Members...)
	for i := range g.Members {
		if o, ok := overrides[g.Members[i].NodeID]; ok && o.seq > g.Members[i].LeftSeq {
			g.Members[i].LeftSeq, g.Members[i].LeftSig = o.seq, o.sig
		}
	}
	return true
}

type leftOverride struct {
	seq int64
	sig []byte
}

// RosterDigest 是名册的完整状态摘要：两块内容都参与。
//
// 用途只有一个 —— 回答「我是不是拿全了」。摘要一致 ⇒ 不落库、不触发刷新；
// 不一致 ⇒ 触发一次 GROUP_MEMBERS_REQ。
// 不含 DisplayName（非权威提示，改了不该触发同步）。
func (g *Group) RosterDigest() []byte {
	members := append([]Member(nil), g.Members...)
	sort.SliceStable(members, func(i, j int) bool { return members[i].NodeID.Less(members[j].NodeID) })

	var tmp [8]byte
	buf := make([]byte, 0, 64+len(members)*40)
	buf = append(buf, g.ID...)
	buf = append(buf, '|')
	buf = append(buf, string(g.State.StateOrActive())...)
	binary.BigEndian.PutUint64(tmp[:], uint64(g.Epoch))
	buf = append(buf, tmp[:]...)
	for _, m := range members {
		buf = append(buf, m.NodeID[:]...)
		binary.BigEndian.PutUint64(tmp[:], uint64(m.AdmitSeq))
		buf = append(buf, tmp[:]...)
		binary.BigEndian.PutUint64(tmp[:], uint64(m.KickSeq))
		buf = append(buf, tmp[:]...)
		binary.BigEndian.PutUint64(tmp[:], uint64(m.LeftSeq))
		buf = append(buf, tmp[:]...)
	}
	sum := sha256.Sum256(buf)
	return sum[:]
}
```

**Step 4：群主侧操作改用票号**

```go
// Admit 由群主发票：把该成员的票号推进到一个【全新的】群级票号。
//
// 关键：票号取自群级计数器 MintSeq，因此「新票恒大于此前发过的所有票号」，
// 群主无需知道对方撕到几号 —— 这是拉回必然生效的全部理由（ADR-012 Decision §2）。
func (g *Group) Admit(id identity.NodeID, now time.Time) error {
	for i := range g.Members {
		if g.Members[i].NodeID != id {
			continue
		}
		// 已存在（含曾被踢 / 已退出）：把票号推到当前群级票号。
		// 它恒大于此前发过的所有票号，因此无需知道对方撕到几号。
		g.Members[i].AdmitSeq = g.MintSeq
		g.Members[i].State = MemberActive
		return nil
	}
	if len(g.ActiveMembers()) >= MaxMembers {
		return fmt.Errorf("group: member limit %d exceeded", MaxMembers)
	}
	g.Members = append(g.Members, Member{
		NodeID: id, Role: RoleMember, JoinedAt: now, State: MemberActive, AdmitSeq: g.MintSeq,
	})
	return nil
}

// Kick 由群主撕票：把该成员的票作废。
func (g *Group) Kick(id identity.NodeID) {
	for i := range g.Members {
		if g.Members[i].NodeID == id {
			g.Members[i].KickSeq = g.Members[i].AdmitSeq // 严格相等 ⇒ Active() 为 false
			g.Members[i].State = MemberKicked
			return
		}
	}
}
```

`RemoveMember` 与旧的 `AddMember` 删除；`SyncDerivedState()` 负责把 `State` 缓存刷新（供落库与旧读路径使用）：

```go
// SyncDerivedState 刷新 State 派生缓存（落库前调用）。
// 判据始终是 Active()；缓存只是为了让 state 列与旧查询不至于说谎。
func (g *Group) SyncDerivedState() {
	for i := range g.Members {
		switch {
		case g.Members[i].Active():
			g.Members[i].State = MemberActive
		case g.Members[i].KickSeq >= g.Members[i].AdmitSeq:
			g.Members[i].State = MemberKicked
		default:
			g.Members[i].State = MemberLeft
		}
	}
}
```

**Step 5：跑测试**

Run: `go test ./internal/domain/group/ -v`
Expected: PASS（全部 6 条新用例 + 既有用例）。

**Step 6：提交**

```bash
git add internal/domain/group/
git commit -m "feat(group): token/tombstone merge with convergence tests"
```

---

## Task 4：迁移 v6 + 仓储

**Files:**
- Modify: `internal/adapters/store/sqlite/migrations.go`（**追加**迁移 v6，永不修改历史迁移）
- Modify: `internal/adapters/store/sqlite/groups.go`
- Modify: `internal/adapters/store/mem/mem.go`
- Test: `internal/adapters/store/sqlite/sqlite_test.go`

**Step 1：追加迁移**

在 `migrations` 切片末尾追加（`version: 6`）：

```go
	{
		version: 6,
		name:    "group_member_tokens",
		// 群成员「发票 / 撕票」模型（ADR-012）：
		//   mint_seq   —— 群主的群级发票计数器
		//   admit_seq  —— 「票号」（群主签）
		//   kick_seq   —— 群主撕票号（群主签）
		//   left_seq   —— 本人撕票号 + 自签（未签名的存量数据为 NULL/0，仍按 State 兼容判定）
		//   left_sig   —— 本人对 H(group_id|node_id|left_seq) 的签名
		//
		// 默认值刻意取 admit_seq = mint_seq = 1：存量成员等价于「拿到过 1 号票」，
		// 语义上就是升级前的那次历史准入，不需要人工修补数据。
		sql: `
ALTER TABLE groups        ADD COLUMN mint_seq INTEGER NOT NULL DEFAULT 1;
ALTER TABLE group_members ADD COLUMN admit_seq INTEGER NOT NULL DEFAULT 1;
ALTER TABLE group_members ADD COLUMN kick_seq  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE group_members ADD COLUMN left_seq  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE group_members ADD COLUMN left_sig  TEXT;
`,
	},
```

**Step 2：写失败测试**

```go
// 迁移可重入 + 往返：票号与自签必须原样存取。
func TestGroupMemberTokensRoundTrip(t *testing.T) {
	db, _ := openTest(t)
	gr := NewGroups(db)
	owner, m2 := mkID(1), mkID(2)
	in := group.Group{
		ID: "g1", Name: "群", OwnerID: owner, Epoch: 7, MintSeq: 13, State: group.StateActive,
		RosterSig: []byte{1, 2, 3},
		Members: []group.Member{
			{NodeID: owner, Role: group.RoleOwner, AdmitSeq: 1, State: group.MemberActive},
			{NodeID: m2, Role: group.RoleMember, AdmitSeq: 13, KickSeq: 0, LeftSeq: 9,
				LeftSig: []byte{9, 9}, State: group.MemberLeft},
		},
	}
	if err := gr.Upsert(in); err != nil {
		t.Fatal(err)
	}
	got, ok := gr.Get("g1")
	if !ok {
		t.Fatal("读不到刚写入的群")
	}
	if got.MintSeq != 13 || got.Epoch != 7 || !bytes.Equal(got.RosterSig, []byte{1, 2, 3}) {
		t.Errorf("群级字段未往返：mint=%d epoch=%d sig=%v", got.MintSeq, got.Epoch, got.RosterSig)
	}
	m, _ := got.Member(m2)
	if m.AdmitSeq != 13 || m.LeftSeq != 9 || !bytes.Equal(m.LeftSig, []byte{9, 9}) {
		t.Errorf("成员票号未往返：%+v", m)
	}
	if !m.Active() {
		t.Error("admit=13 > left=9 ⇒ 该成员应是在群（「退出后被重新拉回」）")
	}

	// 另一半：票号被撕到同一号 ⇒ 不在群。这是判据「严格大于」的落库往返验证。
	in.Members[1].AdmitSeq = 9
	if err := gr.Upsert(in); err != nil {
		t.Fatal(err)
	}
	got, _ = gr.Get("g1")
	m, _ = got.Member(m2)
	if m.Active() {
		t.Error("admit == left ⇒ 必须是不在群（否则踢人/退出会当场失效）")
	}
}
```

**Step 3：实现仓储**

`Upsert` 的 `INSERT INTO groups(...)` 增加 `mint_seq`（列与占位符都要加，`ON CONFLICT DO UPDATE` 里同步更新）；`group_members` 增加 `admit_seq`/`kick_seq`/`left_seq`/`left_sig`，写库前调用 `in.SyncDerivedState()` 刷新 `state` 缓存。`scanGroup` / `membersOf` 读回新列（`left_sig` 用 `sql.NullString` + 既有的 `b64`/`unb64` 助手）。`mem` 实现同步跟进（它是 `Groups` 的内存替身，必须与 sqlite 对等，否则测试会给假绿灯）。

**Step 4：跑测试**

Run: `go test ./internal/adapters/store/sqlite/ ./internal/adapters/store/mem/ -v`
Expected: PASS；迁移可重入（连跑两次不报 checksum mismatch）。

**Step 5：提交**

```bash
git add internal/adapters/store/
git commit -m "feat(store): migration v6 for group member token seqs"
```

---

## Task 5：协议 —— 两块载荷 + `GROUP_LEAVE`

**Files:**
- Modify: `internal/domain/protocol/const.go`（新增 `TypeGroupLeave byte = 0x34` + `TypeName` 分支）
- Modify: `internal/domain/protocol/wire.go`
- Modify: `internal/app/router.go`
- Test: `internal/domain/protocol/const_test.go`（既有表驱动，补一行）

**Step 1：写失败测试**

在 `const_test.go` 的类型表里加：

```go
		TypeGroupLeave: "GROUP_LEAVE",
```

**Step 2：跑测试确认失败**

Run: `go test ./internal/domain/protocol/ -run TestTypeName -v`
Expected: FAIL —— `TypeGroupLeave` 映射为 `UNKNOWN`。

**Step 3：实现**

`const.go`：

```go
	// 0x30–0x3F 群组
	TypeGroupMeta       byte = 0x30
	TypeGroupMembersReq byte = 0x31
	TypeGroupMembersRes byte = 0x32
	TypeGroupMsg        byte = 0x33
	// TypeGroupLeave 是【本人签名】的退出增量：群主签名的 GROUP_META 装不下
	// 「退出」这个属于本人的权威（ADR-012 Decision §6），因此单独立帧。
	TypeGroupLeave byte = 0x34
```

`wire.go`：

```go
// GroupMemberWire 是群成员的线上表示。
//
// LeftSeq/LeftSig 属于「本人签名域」，其余属于「群主签名块」—— 两块在同一条
// 消息里携带，但各自独立认证、独立合并（见 ADR-012 Decision §5）。
type GroupMemberWire struct {
	NodeID      string `json:"node_id"`
	DisplayName string `json:"display_name,omitempty"` // 非权威提示
	Role        string `json:"role"`
	JoinedAt    int64  `json:"joined_at"`

	AdmitSeq int64 `json:"admit_seq"`
	KickSeq  int64 `json:"kick_seq"`

	LeftSeq int64  `json:"left_seq,omitempty"`
	LeftSig string `json:"left_sig,omitempty"` // base64
}

// GroupMeta 传播名册快照。
type GroupMeta struct {
	GroupID   string            `json:"group_id"`
	Name      string            `json:"name"`
	OwnerID   string            `json:"owner_id"`
	State     string            `json:"state,omitempty"`
	Epoch     int64             `json:"epoch"`
	MintSeq   int64             `json:"mint_seq"`
	StateSig  string            `json:"state_sig"` // base64，群主签名（名册块）
	Members   []GroupMemberWire `json:"members"`
	CreatedAt int64             `json:"created_at"`
	// RosterDigest 是名册完整状态摘要（base64），用于「我拿全了吗」的判据。
	RosterDigest string `json:"roster_digest,omitempty"`
}

// GroupLeave 是本人签名的退出增量。
type GroupLeave struct {
	GroupID string `json:"group_id"`
	NodeID  string `json:"node_id"`
	LeftSeq int64  `json:"left_seq"`
	Sig     string `json:"sig"` // base64，本人对 H(group_id|node_id|left_seq) 的签名
}
```

`GroupMembersReq` 增加 `KnownDigest string \`json:"known_digest,omitempty"\``。

`router.go` 增加：

```go
	case protocol.TypeGroupLeave:
		r.dispatch(sess, f, handleOf(r.Group, (*GroupApp).HandleGroupLeave))
```

**Step 4：跑测试**

Run: `go test ./internal/domain/protocol/ ./internal/archtest/ -v`
Expected: PASS（架构红线也必须过：协议包仍零 I/O）。

**Step 5：提交**

```bash
git add internal/domain/protocol/ internal/app/router.go
git commit -m "feat(protocol): carry token seqs and add GROUP_LEAVE frame"
```

---

## Task 6：`GroupApp` —— 退出、推送、拉取、校验

**Files:**
- Modify: `internal/app/group_app.go`
- Test: `internal/app/group_leave_test.go`（新建）、`internal/app/group_sync_test.go`（新建）

**Step 1：写失败测试（退出 + 无成员资格的消息）**

```go
// 本人退出必须【本地立即生效】—— 不依赖群主在线。
func TestLeaveTakesEffectLocally(t *testing.T) {
	n, kp := newGroupFixture(t)
	mid := mkID(2)
	n.groups.Upsert(group.Group{ID: "g1", OwnerID: mkID(1), Epoch: 1, MintSeq: 1,
		Members: []group.Member{
			{NodeID: mkID(1), Role: group.RoleOwner, AdmitSeq: 1},
			{NodeID: mid, Role: group.RoleMember, AdmitSeq: 1},
		}})

	g, err := n.gapp.Leave(context.Background(), "g1")
	if err != nil {
		t.Fatalf("Leave: %v", err)
	}
	m, _ := g.Member(mid)
	if m.Active() {
		t.Error("退出后本机必须立刻视为不在群")
	}
	if len(m.LeftSig) == 0 {
		t.Error("退出必须带上自签，否则别人无法在乱序下判定这条是谁发的")
	}
	_ = kp
}

// 非成员发来的群消息必须丢弃，但【仍要回 ACK】——不回会让对方按 outbox 无限重发。
func TestGroupMsgFromNonMemberDroppedButAcked(t *testing.T) {
	n, _ := newGroupFixture(t)
	stranger := mkID(9)
	n.groups.Upsert(group.Group{ID: "g1", OwnerID: mkID(1), Epoch: 1, MintSeq: 1,
		Members: []group.Member{{NodeID: mkID(1), Role: group.RoleOwner, AdmitSeq: 1}}})

	sess := &fakeSession{peer: stranger}
	payload, _ := protocol.EncodeJSON(protocol.GroupMsg{
		MsgID: "m1", GroupID: "g1", SenderID: stranger.String(), Content: "我不在这个群",
	})
	if err := n.gapp.HandleGroupMsg(sess, protocol.New(protocol.TypeGroupMsg, payload)); err != nil {
		t.Fatal(err)
	}
	if _, ok := n.store.Get("m1"); ok {
		t.Error("非成员的消息不该落库")
	}
	if !sess.acked("m1") {
		t.Error("非成员的消息也必须回 ACK，否则发送方无限重发")
	}
}
```

**Step 2：跑测试确认失败**

Run: `go test ./internal/app/ -run "TestLeave|TestGroupMsgFromNonMember" -v`
Expected: 编译失败 —— `Leave undefined`、`acked undefined`。

**Step 3：实现**

`GroupApp` 新增字段 `syncInterval time.Duration`（默认 60s）与依赖 `dir`（已有）。

```go
// Leave 由本人退出：本地立即撕票生效，然后尽力通知在线成员。
//
// 为什么本地立即生效：退出权属于本人（ADR-012），把它绑在「群主在线」上是
// 原设计的硬伤。通知失败只意味着别人晚一点知道，本机状态已经是最终态。
func (a *GroupApp) Leave(ctx context.Context, groupID string) (group.Group, error) {
	g, ok := a.groups.Get(groupID)
	if !ok {
		return group.Group{}, fmt.Errorf("group: unknown group %s", groupID)
	}
	me, ok := g.Member(a.self)
	if !ok || !me.Active() {
		return g, fmt.Errorf("group: 你不在这个群里")
	}

	leftSeq := me.AdmitSeq // 撕掉当前那张活票
	sig := a.kp.Sign(group.LeftSigningBytes(groupID, a.self, leftSeq))
	if !g.MergeLeft(a.self, leftSeq, sig) {
		return g, nil // 已经是退出态：幂等
	}
	g.SyncDerivedState()
	if err := a.groups.Upsert(g); err != nil {
		return g, err
	}
	a.publishGroup(g)
	go a.broadcastLeave(ctx, g, leftSeq, sig)
	return g, nil
}

// broadcastLeave 把本人退出通知给在线的其他成员。
func (a *GroupApp) broadcastLeave(ctx context.Context, g group.Group, leftSeq int64, sig []byte) {
	payload, err := protocol.EncodeJSON(protocol.GroupLeave{
		GroupID: g.ID, NodeID: a.self.String(), LeftSeq: leftSeq, Sig: encodeBase64(sig),
	})
	if err != nil {
		return
	}
	for _, id := range a.onlineMembers(g, a.self) {
		sess, err := a.session(ctx, id)
		if err != nil {
			continue
		}
		_ = sess.Send(protocol.New(protocol.TypeGroupLeave, payload))
	}
}

// HandleGroupLeave 应用别人的退出记录。
// 接受条件三条全满足：认识该群、自签有效、票号更高。
func (a *GroupApp) HandleGroupLeave(_ ports.Session, f protocol.Frame) error {
	var lv protocol.GroupLeave
	if err := protocol.DecodeJSON(f.Payload, &lv); err != nil {
		return err
	}
	if lv.GroupID == "" || lv.NodeID == "" || lv.LeftSeq <= 0 {
		return nil
	}
	id, err := identity.ParseNodeID(lv.NodeID)
	if err != nil {
		return nil
	}
	g, ok := a.groups.Get(lv.GroupID)
	if !ok {
		return nil // 我还不认识这个群：留给 owner 快照补
	}
	// 自签必须由【该成员本人】签：否则任何人都能替你退出。
	pub, ok := a.peerPublicKey(id)
	if !ok {
		return nil
	}
	if !identity.Verify(pub, group.LeftSigningBytes(lv.GroupID, id, lv.LeftSeq), decodeBase64(lv.Sig)) {
		return fmt.Errorf("group: invalid leave signature from %s", id)
	}
	if !g.MergeLeft(id, lv.LeftSeq, decodeBase64(lv.Sig)) {
		return nil
	}
	g.SyncDerivedState()
	if err := a.groups.Upsert(g); err != nil {
		return err
	}
	a.publishGroup(g)
	return nil
}
```

`Start`（当前**没有**这个方法 —— 这正是缺口 G-2 的所在）：

```go
// Start 订阅节点上下线，做「推送 + 拉取」并启动低频抽查。
//
// 这是本设计修掉 G-2 的地方：原实现里成员变更只做一次尽力而为的单播，
// 离线成员错过即永久错过 —— 被移出者仍以为自己在群里，被加入者连群都不存在。
func (a *GroupApp) Start(ctx context.Context) {
	if a.bus == nil {
		return
	}
	// 有人上线：主动把「他可能不知道」的群推给他。
	// 任一成员都能推 —— 这是「新成员本地压根没有这个群、无从发起拉取」的唯一解法。
	a.bus.Subscribe(eventbus.TopicPeerOnline, func(payload any) {
		ev, ok := payload.(eventbus.PeerOnline)
		if !ok {
			return
		}
		id, err := identity.ParseNodeID(ev.NodeID)
		if err != nil {
			return
		}
		a.pushGroupsTo(ctx, id)
		a.requestSyncFrom(ctx, id)
	})
	a.bus.Subscribe(eventbus.TopicPeerDiscovered, func(payload any) {
		p, ok := payload.(peer.Peer)
		if !ok {
			return
		}
		a.requestSyncFrom(ctx, p.NodeID)
	})

	go a.sweepSync(ctx)
}

// pushGroupsTo 把该节点【可能不知道或已落后】的群推给他。
func (a *GroupApp) pushGroupsTo(ctx context.Context, id identity.NodeID) {
	sess, err := a.sessionNoDial(id)
	if err != nil {
		return
	}
	for _, g := range a.groups.List() {
		if !a.shouldTell(g, id) {
			continue
		}
		payload, err := protocol.EncodeJSON(encodeGroupMeta(g))
		if err != nil {
			continue
		}
		_ = sess.Send(protocol.New(protocol.TypeGroupMeta, payload))
	}
}

// shouldTell 报告「是否值得把这个群告诉他」：
// 他是我名册里的成员（含已被移出/已退出的），或我掌握的名册里根本没有他。
// 后者覆盖「刚被拉进群、本机还不知道这个群存在」的情形 —— 他自然无从发起拉取。
func (a *GroupApp) shouldTell(g group.Group, id identity.NodeID) bool {
	if _, ok := g.Member(id); ok {
		return true
	}
	return false
}
```

`HandleGroupMsg` 增加成员校验（插在 `sanitizeImageContent` 之前）：

```go
	// 成员资格校验：非当前在群成员发来的群消息丢弃，但【仍回 ACK】。
	// 用「当前名册」而不是「发送时的名册」：成员资格是现在的事实。
	if g, ok := a.groups.Get(gm.GroupID); ok {
		if m, ok := g.Member(senderID); !ok || !m.Active() {
			return a.sendAck(sess, gm.MsgID)
		}
	}
```

`commitGroup` 改为票号语义：`MintSeq++` → 调用方已完成的 `Admit`/`Kick` → `Epoch++` → `SyncDerivedState()` → 重签 → 落库 → 广播。

**新增 `Resign`（群主对自己名下的群重签一次，不改成员）** —— 它是 Task 7 里「存量群重签」的执行体：

```go
// Resign 由群主重签一次名册（不改成员）。
//
// 唯一用途：覆盖 ADR-012 的 R-3 —— RosterSig 覆盖字节在本次改造中变了，
// 升级前已存在的群其旧签名在新代码下验不过。启动时由群主自动重签一次即可，
// 不需要用户操作，也不需要兼容代码。
func (a *GroupApp) Resign(ctx context.Context, groupID string) (group.Group, error) {
	g, ok := a.groups.Get(groupID)
	if !ok {
		return group.Group{}, fmt.Errorf("group: unknown group %s", groupID)
	}
	if g.OwnerID != a.self {
		return g, fmt.Errorf("group: only the owner can sign the roster")
	}
	return a.commitGroup(ctx, g)
}
```

`AddMember`/`RemoveMember`/`Rename`/`Dissolve` 内部改用 `Admit`/`Kick`/`SyncDerivedState`。

**`ApplyMeta` 改为「验签 → 合并 → 变化才 publish」**，并显式处理「本机还没有这个群」：

```go
func (a *GroupApp) ApplyMeta(g group.Group) error {
	if err := g.Validate(); err != nil {
		return err
	}
	pub, ok := a.ownerPublicKey(g.OwnerID)
	if !ok {
		return fmt.Errorf("group: unknown owner public key for %s", g.OwnerID)
	}
	if !identity.Verify(pub, g.RosterSigningBytes(), g.RosterSig) {
		return fmt.Errorf("group: owner signature invalid for %s", g.ID)
	}

	cur, exists := a.groups.Get(g.ID)
	changed := false
	if !exists {
		// 本机还不认识这个群（典型：刚被拉进来）—— 直接采纳。
		cur, changed = g, true
	} else {
		// 整块替换只在 Epoch 更高时发生；left 覆盖的合并发生在 MergeOwnerSnapshot 内部。
		changed = cur.MergeOwnerSnapshot(g)
		// 即使 owner 块没变，对方可能带来了更新的 left 覆盖（GROUP_META 也携带 left）。
		for _, m := range g.Members {
			if m.LeftSeq > 0 && cur.MergeLeft(m.NodeID, m.LeftSeq, m.LeftSig) {
				changed = true
			}
		}
	}
	if !changed {
		return nil // 摘要一致的重复投递：不落库、不触发 UI 刷新
	}
	cur.SyncDerivedState()
	if err := a.groups.Upsert(cur); err != nil {
		return err
	}
	a.publishGroup(cur)
	return nil
}
```

**`SweepOutbox` 的未送达判据**把 `State == MemberActive` 换成 `Active()`（`pendingActive` 里那一处）——
于是成员一旦退出/被踢，就**自动不再阻塞**那条消息的「全员送达」。

**本 Task 需要抽取的复用助手**（都从既有代码里提炼，不要新引入语义）：

| 助手 | 来源 |
|---|---|
| `a.peerPublicKey(id)` | 把现有 `ownerPublicKey` 的主体提出来（同名改调它），供「验本人退出签名」复用 |
| `a.onlineMembers(g, except)` | 从现有 `broadcastMeta` 里的 `dir.List(OnlineOnly)` + `g.FanoutPlan` 提炼 |
| `a.publishGroup(g)` | 把 `commitGroup`/`ApplyMeta` 里重复的 `bus.Publish(TopicGroupUpdated, g)` 提炼 |
| 已有会话直取 | `pushGroupsTo` 用 `a.conns.SessionOf(id)`（**不要** `Dial`：对方刚上线时不该为一次推送发起拨号） |

**测试助手 `newGroupFixture`**（放在 `internal/app/group_leave_test.go` 顶部）。
它**复用** `group_delivery_test.go` 里已有的 `mkID` / `mustNodeID` / `recordingConns` / `noConns`，
不要另造一套 —— 替身与真实实现分叉时，测试会给假绿灯。

```go
type groupFixture struct {
	gapp   *GroupApp
	groups ports.GroupRepo
	store  ports.ChatStore
	conns  *recordingConns
}

// newGroupFixture 造一个「本机是普通成员」的群环境：
// kp 是本机密钥，因此后续断言 LeftSig 是否由本人签出才有意义。
func newGroupFixture(t *testing.T) (*groupFixture, *identity.KeyPair) {
	t.Helper()
	kp, err := identity.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	store := mem.NewMessages(fc)
	groups := mem.NewGroups()
	conns := &recordingConns{}
	self := kp.NodeID()
	gapp := NewGroupApp(self, kp, groups, store, mem.NewGroupDeliveries(),
		conns, mem.NewPeers(fc), eventbus.New(), fc, nil)
	return &groupFixture{gapp: gapp, groups: groups, store: store, conns: conns}, kp
}

// fakeSession 记录本机发出的帧 —— 用来断言「有没有回 ACK」这类出站行为。
// ports.Session 只有 5 个方法（PeerID/Send/Recv/RTT/Close），补齐即可。
type fakeSession struct {
	peer identity.NodeID
	mu   sync.Mutex
	sent []protocol.Frame
}

func (s *fakeSession) PeerID() identity.NodeID { return s.peer }
func (s *fakeSession) Send(f protocol.Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, f)
	return nil
}
// Recv 返回错误而不是阻塞：本用例不读，阻塞实现一旦被误调就是测试永久挂住。
func (s *fakeSession) Recv() (protocol.Frame, error) { return protocol.Frame{}, errNotUsed }
func (s *fakeSession) RTT() time.Duration            { return 0 }
func (s *fakeSession) Close() error                  { return nil }

var errNotUsed = errors.New("fakeSession: Recv 未被使用")

// acked 报告是否对 msgID 回过聊天 ACK。
func (s *fakeSession) acked(msgID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.sent {
		if f.Type != protocol.TypeChatAck {
			continue
		}
		var ack protocol.ChatAck
		if protocol.DecodeJSON(f.Payload, &ack) == nil && ack.MsgID == msgID {
			return true
		}
	}
	return false
}
```

**Step 4：跑测试**

Run: `go test -race ./internal/app/ ./internal/archtest/ -v`
Expected: PASS。

**Step 5：提交**

```bash
git add internal/app/ internal/bootstrap/
git commit -m "feat(group): self-leave, push/pull sync on peer online, membership check"
```

---

## Task 7：服务层 + DTO + 装配

**Files:**
- Modify: `internal/adapters/wails/services.go`（`GroupService.Leave`、`History` 的未送达统计改用 `Active()`）
- Modify: `internal/adapters/wails/dto.go`（`GroupMemberDTO` 增 `admitSeq`/`leftSeq`/`active`）
- Modify: `internal/adapters/wails/eventbridge.go`（`group:updated` 带 `digest`）
- Modify: `internal/bootstrap/bootstrap.go`（调用 `GroupApp.Start(ctx)`；群主自动重签一次）
- Test: `internal/adapters/wails/services_test.go`

**Step 1：写失败测试**

```go
func TestGroupServiceLeave(t *testing.T) {
	// 非群主也能退出（这是本次改造的核心能力），且退出后 IsOwner 仍为 false、
	// 该成员在 DTO 里 active=false。
	// 用既有 newTestDeps(t) 装配，创建群后把 self 加进去再 Leave。
}
```

**Step 2：实现**

```go
// Leave 退出群（本人）。本地立即生效，不依赖群主在线。
func (s *GroupService) Leave(groupID string) (GroupDTO, error) {
	g, err := s.d.Group.Leave(context.Background(), groupID)
	if err != nil {
		return GroupDTO{}, err
	}
	return toGroupDTO(g, s.d.Self.NodeID), nil
}
```

`toGroupDTO` 每成员补 `AdmitSeq`/`LeftSeq`/`Active: m.Active()`。
`bootstrap` 里在 `peerApp.Start()` 附近加 `n.GroupApp.Start(ctx)`；启动时对「本机为群主且 `Epoch` 未变」的群做一次重签（覆盖 ADR-012 里 R-3 的存量重签）：

```go
	// 群主对自己名下的每个群重签一次名册：覆盖「RosterSig 布局变更」的存量代价。
	for _, g := range n.Groups.List() {
		if g.OwnerID == kp.NodeID() {
			_ = n.GroupApp.Resign(ctx, g.ID)
		}
	}
```

**Step 3：重生成前端绑定**

Run: `wails3 generate bindings -ts -i -f "-tags wails" -clean .`
Expected: `frontend/bindings/.../groupservice.ts` 出现 `Leave`；`models.ts` 的 `GroupMemberDTO` 出现新字段。**随后检查 `git diff --stat -- frontend/bindings` 只包含预期文件**（上次误用无 `-ts -i` 会把 `.ts` 全换成 `.js`）。

**Step 4：跑测试**

Run: `go test -tags wails ./...`
Expected: PASS。

**Step 5：提交**

```bash
git add internal/adapters/wails/ internal/bootstrap/ frontend/bindings/
git commit -m "feat(group): expose Leave, member seqs in DTO, wire Start and resign"
```

---

## Task 8：前端

**Files:**
- Modify: `frontend/src/api/types.ts`、`frontend/src/api/bindings.ts`、`frontend/src/api/mock.ts`
- Modify: `frontend/src/stores/group.ts`（`leave`）
- Modify: `frontend/src/components/GroupInfoPanel.vue`（「退出群」按钮 + 成员状态）
- Modify: `frontend/src/events/index.ts`（`group:updated` 带 digest 时静默刷新）
- Test: `frontend/tests/`（若有纯函数可测则补）

**Step 1：接口与 store**

`types.ts`：`GroupMember` 增 `admitSeq: number`、`leftSeq: number`、`active: boolean`；`SwarmApi` 增 `groupLeave(groupId): Promise<Group>`。
`bindings.ts`：`groupLeave: async (groupId) => toGroup(await GroupService.Leave(groupId))`。
`mock.ts`：`groupLeave` 把当前用户在 `members` 里的 `leftSeq = admitSeq`、`active = false`，并刷新。
`stores/group.ts`：`leave(groupId)` 调接口后 `refresh()`。

**Step 2：面板**

`GroupInfoPanel.vue`：
- 在 `#footer` 增加「退出群」（**所有成员**可见，非群主也可见；群主不可退出，改为只显示「解散群」）；
- 成员行按 `active` 显示「在群 / 已退出 / 已移出」，已退出/已移出的行降透明度；
- 退出需二次确认（文案说明：退出后不再接收该群消息，群主可再次拉你回群）。

**Step 3：静默刷新**

`events/index.ts` 的 `groupUpdated` 处理器：载荷里带 `digest` 时，与 `useGroupStore().byId(...)` 的本地摘要比对，**一致则跳过 `refresh()`**（避免摘要一致的重复投递引发刷新风暴）。

**Step 4：验证**

Run: `cd frontend && npm run typecheck && npm run test && npm run build`
Expected: 全部通过。

**Step 5：提交**

```bash
git add frontend/src frontend/bindings
git commit -m "feat(ui): group leave and member token state"
```

---

## Task 9：e2e 与回归

**Files:**
- Modify: `test/e2e/e2e_test.go`
- Test: 同上

**Step 1：写失败测试**

见 `test/e2e/group_test.go`（已实现，四条用例）：

```go
// G-2 的验收：被加入群的成员【离线】，上线后必须知道自己进了群。
func TestOfflineMemberLearnsGroupOnRejoin(t *testing.T) { ... }

// 被移出的成员【离线】，上线后必须知道自己不在群。
func TestKickedWhileOfflineNeverSeenAsMember(t *testing.T) { ... }

// 主动退出要传得出去，且不被后续全量名册复活。
func TestSelfLeavePropagatesAndIsNotResurrected(t *testing.T) { ... }

// 已被移出的成员再发群消息必须被拒（含「在群时能发」的正向对照）。
func TestRemovedMemberCannotSendGroupMessage(t *testing.T) { ... }
```

> **执行时发现的真实缺陷（已修，见设计文档 §8.0）**：这四条里前三条一次通过，
> 第四条卡在「在线被踢的人迟迟不认为自己被移出」。根因是 `broadcastMeta` 的受众
> 用了 `FanoutPlan`（只在群 ∩ 在线）—— 刚被踢的人已不在 active 集合里，**收不到自己被踢**；
> 而离线被踢的人反而能靠上线推送得知。修法是新增 `Group.MetaAudience`
> （名册里出现过的**所有人** ∩ 在线）并配 `group.TestMetaAudienceIncludesRemovedMembers`。

**Step 2：跑测试确认失败**

Run: `go test ./test/e2e/ -run "TestOfflineMemberLearns|TestKickedMemberLearns|TestSelfLeaveSurvives|TestNonMember" -v`
Expected: FAIL。

**Step 3：跑通**

Run: `go test -race -tags wails ./... && cd frontend && npm run typecheck && npm run test`
Expected: 全绿。若 `test/scale` 或既有群用例因票号语义回归，先修回归再继续。

**Step 4：提交**

```bash
git add test/
git commit -m "test(e2e): group membership token sync across offline peers"
```

---

## 收尾检查单

- [x] `go test -tags wails ./...` 全绿
- [x] `go test -race -tags wails ./...` 无竞态
- [x] `go test ./internal/archtest/` 三条红线通过
- [x] `cd frontend && npm run typecheck && npm run test && npm run build` 通过（54 tests）
- [x] `git diff --stat -- frontend/bindings` 只含 3 个预期文件（groupservice / models / transferservice）
- [x] `docs/adr/README.md` 索引已含 ADR-012
- [x] 设计文档 §14 的实施拆分与本计划一致
- [x] `gofmt -l internal/ test/` 为空

## 执行期间发现并修掉的两个真实缺陷

1. **`broadcastMeta` 受众用错**（T9 暴露）：用 `FanoutPlan`（只在群 ∩ 在线）导致
   **在线被踢的人收不到自己被踢**，而离线被踢的人反而能靠上线推送得知。
   → 新增 `Group.MetaAudience`（名册里出现过的所有人 ∩ 在线）。
2. **推送只查现有会话**（T9 前置修正）：刚被拉进群的人本机没有连接，
   只查 `SessionOf` 等于永远推不到他 → 改为**按需拨号**（先判有无共有群，避免给无关节点拨号），
   并把推送/拉取放进 goroutine（事件同步派发，订阅者里做网络 I/O 会拖住发布方）。
