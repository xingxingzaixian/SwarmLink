package wails

import (
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/store/mem"
	"github.com/swarmlink/swarmlink/internal/app"
	"github.com/swarmlink/swarmlink/internal/domain/group"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// SearchService 的测试。
//
// 重点在两件【只有这一层才能负责】的事：
//  1. 会话白名单必须由可见会话派生 —— 被本地移除的对端不能借搜索重新露面；
//  2. 输入归一化（去首尾空白）与空输入短路。
// 分页与窗口的语义在存储层已经钉死（store/sqlite、store/mem、store/parity），
// 这里只用一条最小用例证明服务层把参数正确转了下去。

type searchFixture struct {
	svc    *SearchService
	store  *mem.Messages
	peers  *mem.Peers
	groups *mem.Groups
	self   identity.NodeID
}

func newSearchFixture(t *testing.T) *searchFixture {
	t.Helper()
	clk := clock.New()
	self := selfID(t)
	store := mem.NewMessages(clk)
	peers := mem.NewPeers(clk)
	groups := mem.NewGroups()
	capp := app.NewChatApp(self, store, stubConns{}, peers, eventbus.New(), clk, nil)
	return &searchFixture{
		svc: &SearchService{d: Deps{
			Self:      SelfInfo{NodeID: self},
			Chat:      capp,
			PeerDir:   peers,
			GroupRepo: groups,
		}},
		store:  store,
		peers:  peers,
		groups: groups,
		self:   self,
	}
}

func (f *searchFixture) seed(t *testing.T, conv, id, content string, at time.Time) {
	t.Helper()
	err := f.store.Append(message.Message{
		MsgID: id, ConvID: conv, SenderID: f.self,
		Direction: message.DirectionIn, Content: content,
		MsgType: message.MsgTypeText, SentAt: at, State: message.StateDelivered,
	})
	if err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

// addPeer 造一个节点并返回它的单聊 conv_id；blocked=true 表示「本地已移除」。
func (f *searchFixture) addPeer(t *testing.T, hex string, blocked bool) string {
	t.Helper()
	id, err := identity.ParseNodeID(hex)
	if err != nil {
		t.Fatalf("解析 peer %s: %v", hex, err)
	}
	if err := f.peers.Upsert(peer.Peer{NodeID: id, DisplayName: hex, State: peer.StateOnline}); err != nil {
		t.Fatalf("upsert peer: %v", err)
	}
	if blocked {
		if err := f.peers.SetSuppressed(id, true, time.Now()); err != nil {
			t.Fatalf("suppress peer: %v", err)
		}
	}
	return message.DirectConvID(f.self, id)
}

func (f *searchFixture) addGroup(t *testing.T, id, name string, dissolved bool) string {
	t.Helper()
	state := group.StateActive
	if dissolved {
		state = group.StateDissolved
	}
	g := group.Group{ID: id, Name: name, OwnerID: f.self, State: state, CreatedAt: time.Now()}
	if err := f.groups.Upsert(g); err != nil {
		t.Fatalf("upsert group: %v", err)
	}
	return message.GroupConvID(id)
}

func msgIDs(dtos []MessageDTO) []string {
	out := make([]string, 0, len(dtos))
	for _, d := range dtos {
		out = append(out, d.MsgID)
	}
	return out
}

// 被本地移除的对端不得出现在结果里：这是「用户说要删掉它」的隐私义务，
// 不能因为搜索走了另一条查询路径而失守。
func TestSearchServiceExcludesRemovedPeers(t *testing.T) {
	f := newSearchFixture(t)
	base := time.UnixMilli(1_700_000_000_000)

	keptConv := f.addPeer(t, testPeerHex, false)
	removedConv := f.addPeer(t, "aabbccddeeff0022", true)
	activeGroup := f.addGroup(t, "g1", "项目组", false)
	dissolvedGroup := f.addGroup(t, "g2", "已解散的组", true)

	f.seed(t, keptConv, "k1", "会议纪要 A", base)
	f.seed(t, removedConv, "r1", "会议纪要 B", base.Add(time.Second))
	f.seed(t, activeGroup, "g1-1", "会议纪要 C", base.Add(2*time.Second))
	f.seed(t, dissolvedGroup, "g2-1", "会议纪要 D", base.Add(3*time.Second))

	got, err := f.svc.Search("会议", nil, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"g2-1", "g1-1", "k1"}
	if ids := msgIDs(got); !sameStrings(ids, want) {
		t.Fatalf("结果 = %v，期望 %v（r1 属于已被移除的对端，必须搜不到）", ids, want)
	}
	for _, d := range got {
		if d.ConvID == removedConv {
			t.Fatal("被本地移除的对端的消息不得出现在结果里")
		}
	}

	// 可见会话集合本身也不该包含它（左栏筛选与搜索同源）
	for _, id := range visibleConvIDs(f.svc.d) {
		if id == removedConv {
			t.Fatal("visibleConvIDs 里不得包含已移除对端的会话")
		}
	}
}

// 已解散的群仍列入结果：会话列表里它还在、历史也还打得开，
// 把它的历史从搜索里抹掉只会变成「我记得那条消息但找不回来」。
func TestSearchServiceKeepsDissolvedGroupHistory(t *testing.T) {
	f := newSearchFixture(t)
	dissolved := f.addGroup(t, "g9", "已解散的组", true)
	f.seed(t, dissolved, "d1", "会议纪要", time.UnixMilli(1_700_000_000_000))

	got, err := f.svc.Search("会议", nil, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if ids := msgIDs(got); !sameStrings(ids, []string{"d1"}) {
		t.Fatalf("结果 = %v，期望 [d1]", ids)
	}
}

func TestSearchServiceEmptyInputShortCircuits(t *testing.T) {
	f := newSearchFixture(t)
	f.seed(t, f.addPeer(t, testPeerHex, false), "m1", "会议", time.UnixMilli(1_700_000_000_000))

	for _, text := range []string{"", "   ", "\t\r\n"} {
		got, err := f.svc.Search(text, nil, 0, "")
		if err != nil {
			t.Fatalf("输入 %q 不该报错: %v", text, err)
		}
		if len(got) != 0 {
			t.Fatalf("输入 %q 应返回空数组，得到 %v", text, msgIDs(got))
		}
		if got == nil {
			t.Fatal("应返回空数组而不是 null（前端要直接当数组用）")
		}
	}
}

// 服务层必须把游标与 limit 原样转下去：分页是唯一一个「参数错了会默默丢数据」
// 的路径，需要一条端到端的用例。
func TestSearchServicePagesWithCursor(t *testing.T) {
	f := newSearchFixture(t)
	conv := f.addPeer(t, testPeerHex, false)
	base := time.UnixMilli(1_700_000_000_000)
	for i := 1; i <= 5; i++ {
		f.seed(t, conv, "m"+itoa(i), "会议", base.Add(time.Duration(i)*time.Second))
	}

	page1, err := f.svc.Search("会议", []string{conv}, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if ids := msgIDs(page1); !sameStrings(ids, []string{"m5", "m4"}) {
		t.Fatalf("第 1 页 = %v，期望 [m5 m4]", ids)
	}

	page2, err := f.svc.Search("会议", []string{conv}, 2, "m4")
	if err != nil {
		t.Fatal(err)
	}
	if ids := msgIDs(page2); !sameStrings(ids, []string{"m3", "m2"}) {
		t.Fatalf("第 2 页 = %v，期望 [m3 m2]", ids)
	}
}

// 跳转定位：命中必须在窗口里，两侧条数正确；锚点不可用时返回空数组
// （前端据此回退到最新一页并提示，不当作错误）。
func TestSearchServiceJumpContext(t *testing.T) {
	f := newSearchFixture(t)
	conv := f.addPeer(t, testPeerHex, false)
	base := time.UnixMilli(1_700_000_000_000)
	for i := 1; i <= 6; i++ {
		f.seed(t, conv, "m"+itoa(i), "正文", base.Add(time.Duration(i)*time.Second))
	}

	got, err := f.svc.JumpContext(conv, "m3", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if ids := msgIDs(got); !sameStrings(ids, []string{"m5", "m4", "m3", "m2", "m1"}) {
		t.Fatalf("窗口 = %v，期望 [m5 m4 m3 m2 m1]", ids)
	}

	empty, err := f.svc.JumpContext(conv, "没这条", 2, 2)
	if err != nil {
		t.Fatalf("锚点不可用不该报错: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("锚点不可用应返回空数组，得到 %v", msgIDs(empty))
	}
	if empty == nil {
		t.Fatal("应返回空数组而不是 null")
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
