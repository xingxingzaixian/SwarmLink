package mem

import (
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
)

// 本文件是 sqlite 侧 messages_search_test.go 的同构副本。
//
// 两边都写是刻意的：mem 与 sqlite 是互为替身的两个实现，单测跑 mem、线上跑
// sqlite，任何一侧单独漂移都会变成「测试是绿的、用户看到的不一样」。
// 除了内存特有的写入序号（seq），两侧的期望值必须逐字相同。

func seedMsg(t *testing.T, m *Messages, convID, msgID, content string, sentAt time.Time) {
	t.Helper()
	err := m.Append(message.Message{
		MsgID: msgID, ConvID: convID, SenderID: mkID(1),
		Direction: message.DirectionIn, Content: content,
		MsgType: message.MsgTypeText, SentAt: sentAt, State: message.StateDelivered,
	})
	if err != nil {
		t.Fatalf("append %s: %v", msgID, err)
	}
}

func idsOf(ms []message.Message) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.MsgID)
	}
	return out
}

func equalIDs(a, b []string) bool {
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

// seqOf 取测试 id 里的序号（m1..m10）。不能按字符串比 "m9" 与 "m10"。
func seqOf(id string) int {
	n := 0
	for i := 0; i < len(id); i++ {
		if id[i] >= '0' && id[i] <= '9' {
			n = n*10 + int(id[i]-'0')
		}
	}
	return n
}

func newSearchRepo() *Messages { return NewMessages(clock.New()) }

// LIKE 的元字符必须转义（搜 "%" 不得命中全库）。
func TestSearchEscapesLikeMetacharacters(t *testing.T) {
	m := newSearchRepo()
	base := time.Unix(1000, 0)

	seedMsg(t, m, "c1", "m1", "进度 100% 了", base)
	seedMsg(t, m, "c1", "m2", "普通消息", base.Add(time.Second))
	seedMsg(t, m, "c1", "m3", "a_b 下划线", base.Add(2*time.Second))
	seedMsg(t, m, "c1", "m4", `C:\path\file.txt`, base.Add(3*time.Second))
	seedMsg(t, m, "c1", "m5", "百分号 % 后面还有", base.Add(4*time.Second))

	cases := []struct {
		name string
		text string
		want []string
	}{
		{"百分号", "%", []string{"m5", "m1"}},
		{"下划线", "_", []string{"m3"}},
		{"反斜杆", `\`, []string{"m4"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := m.Search(ports.MessageSearchQuery{Text: c.text, ConvIDs: []string{"c1"}})
			if err != nil {
				t.Fatal(err)
			}
			if !equalIDs(idsOf(got), c.want) {
				t.Fatalf("搜 %q = %v，期望 %v", c.text, idsOf(got), c.want)
			}
		})
	}
}

// 子串匹配的大小写规则：中文照旧，ASCII 折叠，非 ASCII 不折叠
// （与 SQLite 的 LIKE 一致 —— 这正是不能用 strings.ToLower 的原因）。
func TestLikeContainsFoldsASCIIOnly(t *testing.T) {
	cases := []struct {
		haystack, needle string
		want             bool
	}{
		{"明天的会议要讨论接口设计", "会议", true},
		{"Quick brown fox", "quick", true},
		{"Quick brown fox", "QUICK", true},
		{"Quick brown fox", "QuIcK", true},
		{"Quick brown fox", "quickly", false},
		// 非 ASCII 不折叠：ToLower 会把它们变成相等，SQLite LIKE 不会。
		{"Ärger mit Übermut", "ärger", false},
		{"Ärger mit Übermut", "ärger", false},
		{"ärger klein", "ärger", true},
		{"任意内容", "", true},
		{"短", "比它长", false},
	}
	for _, c := range cases {
		if got := likeContains(c.haystack, c.needle); got != c.want {
			t.Errorf("likeContains(%q, %q) = %v，期望 %v", c.haystack, c.needle, got, c.want)
		}
	}
}

func TestSearchRespectsConvClear(t *testing.T) {
	m := newSearchRepo()

	seedMsg(t, m, "c1", "m1", "会议一", time.UnixMilli(1_000))
	seedMsg(t, m, "c1", "m2", "会议二", time.UnixMilli(2_000))
	seedMsg(t, m, "c1", "m3", "会议三", time.UnixMilli(3_000))
	seedMsg(t, m, "c2", "m4", "会议四", time.UnixMilli(2_500))

	if err := m.SetConvCleared("c1", time.UnixMilli(2_000)); err != nil {
		t.Fatal(err)
	}
	got, err := m.Search(ports.MessageSearchQuery{Text: "会议", ConvIDs: []string{"c1", "c2"}})
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"m4", "m3"}) {
		t.Fatalf("清空后搜 = %v，期望 [m4 m3]（m2 在清空点上，必须消失）", idsOf(got))
	}

	if err := m.RemoveConvClear("c1"); err != nil {
		t.Fatal(err)
	}
	got, err = m.Search(ports.MessageSearchQuery{Text: "会议", ConvIDs: []string{"c1", "c2"}})
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"m4", "m3", "m2", "m1"}) {
		t.Fatalf("撤销清空后搜 = %v，期望 4 条都回来", idsOf(got))
	}
}

func TestSearchWhitelistIsMandatory(t *testing.T) {
	m := newSearchRepo()
	base := time.Unix(3000, 0)
	seedMsg(t, m, "c1", "m1", "会议", base)
	seedMsg(t, m, "c2", "m2", "会议", base.Add(time.Second))

	got, err := m.Search(ports.MessageSearchQuery{Text: "会议"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("空白名单必须返回空，得到 %v", idsOf(got))
	}

	got, err = m.Search(ports.MessageSearchQuery{Text: "会议", ConvIDs: []string{"c2"}})
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"m2"}) {
		t.Fatalf("白名单只认 c2，得到 %v", idsOf(got))
	}
}

// 同一毫秒的连续消息必须分页不重不漏。
func TestSearchCursorPagingIsExact(t *testing.T) {
	m := newSearchRepo()
	sameMS := time.UnixMilli(4_000)

	want := make([]string, 0, 10)
	for i := 1; i <= 10; i++ {
		id := "m" + itoa(i)
		seedMsg(t, m, "c1", id, "会议纪要", sameMS)
		want = append(want, id)
	}
	for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
		want[i], want[j] = want[j], want[i]
	}

	var got []string
	cursor, pages := "", 0
	for {
		page, err := m.Search(ports.MessageSearchQuery{
			Text: "会议", ConvIDs: []string{"c1"}, Limit: 3, BeforeMsgID: cursor,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		pageIDs := idsOf(page)
		for i := 1; i < len(pageIDs); i++ {
			if seqOf(pageIDs[i-1]) <= seqOf(pageIDs[i]) {
				t.Fatalf("第 %d 页内部顺序错乱: %v", pages, pageIDs)
			}
		}
		got = append(got, pageIDs...)
		cursor = pageIDs[len(pageIDs)-1]
		pages++
		if pages > 10 {
			t.Fatal("分页不收敛")
		}
	}
	if !equalIDs(got, want) {
		t.Fatalf("分页结果 = %v，期望 %v", got, want)
	}
}

func TestSearchUnknownCursorReturnsEmpty(t *testing.T) {
	m := newSearchRepo()
	seedMsg(t, m, "c1", "m1", "会议", time.UnixMilli(5_000))

	got, err := m.Search(ports.MessageSearchQuery{
		Text: "会议", ConvIDs: []string{"c1"}, BeforeMsgID: "不存在",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("未知游标必须返回空，得到 %v", idsOf(got))
	}
}

// 排序键是写入序而不是 sent_at（与 sqlite 的 rowid 对齐）。
func TestSearchOrderIsInsertionOrderNotSentAt(t *testing.T) {
	m := newSearchRepo()
	seedMsg(t, m, "c1", "m1", "会议", time.UnixMilli(9_000))
	seedMsg(t, m, "c1", "m2", "会议", time.UnixMilli(1_000))

	got, err := m.Search(ports.MessageSearchQuery{Text: "会议", ConvIDs: []string{"c1"}})
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"m2", "m1"}) {
		t.Fatalf("排序 = %v，期望按写入序倒排 [m2 m1]", idsOf(got))
	}
}

func TestWindowAroundAnchor(t *testing.T) {
	m := newSearchRepo()
	base := time.UnixMilli(6_000)
	for i := 1; i <= 10; i++ {
		seedMsg(t, m, "c1", "m"+itoa(i), "正文", base.Add(time.Duration(i)*time.Second))
	}
	seedMsg(t, m, "c2", "x1", "别的会话", base)

	got, err := m.Window("c1", "m5", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"m8", "m7", "m6", "m5", "m4", "m3"}) {
		t.Fatalf("窗口 = %v，期望 [m8 m7 m6 m5 m4 m3]", idsOf(got))
	}

	got, err = m.Window("c1", "m10", 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"m10", "m9", "m8", "m7"}) {
		t.Fatalf("尾部窗口 = %v，期望 [m10 m9 m8 m7]", idsOf(got))
	}

	got, err = m.Window("c1", "m1", 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"m3", "m2", "m1"}) {
		t.Fatalf("头部窗口 = %v，期望 [m3 m2 m1]", idsOf(got))
	}

	got, err = m.Window("c1", "m5", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"m5"}) {
		t.Fatalf("零窗口 = %v，期望只有 [m5]", idsOf(got))
	}
}

// 同一毫秒的锚点与邻居：命中必须在窗口里且不重不漏（行值边界的意义所在）。
func TestWindowKeepsHitWhenNeighborsShareMillisecond(t *testing.T) {
	m := newSearchRepo()
	sameMS := time.UnixMilli(9_000)
	for i := 1; i <= 5; i++ {
		seedMsg(t, m, "c1", "m"+itoa(i), "同毫秒", sameMS)
	}

	got, err := m.Window("c1", "m3", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"m5", "m4", "m3", "m2"}) {
		t.Fatalf("窗口 = %v，期望 [m5 m4 m3 m2]", idsOf(got))
	}

	for i := 1; i <= 5; i++ {
		id := "m" + itoa(i)
		one, err := m.Window("c1", id, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !equalIDs(idsOf(one), []string{id}) {
			t.Fatalf("跳转到 %s 得到 %v，期望就是它自己", id, idsOf(one))
		}
	}
}

// 窗口按 (sent_at, 写入序) 排（= 显示序），不是纯写入序：
// 入站消息带的是发送方时间戳，乱序到达是常态。
func TestWindowOrdersBySentAtThenSeq(t *testing.T) {
	m := newSearchRepo()
	seedMsg(t, m, "c1", "late", "正文", time.UnixMilli(9_000))
	seedMsg(t, m, "c1", "early", "正文", time.UnixMilli(1_000))
	seedMsg(t, m, "c1", "mid", "正文", time.UnixMilli(5_000))

	got, err := m.Window("c1", "early", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"late", "mid", "early"}) {
		t.Fatalf("窗口 = %v，期望 [late mid early]（新→旧按 sent_at）", idsOf(got))
	}
}

func TestWindowRejectsUnusableAnchor(t *testing.T) {
	m := newSearchRepo()
	base := time.UnixMilli(7_000)
	for i := 1; i <= 4; i++ {
		seedMsg(t, m, "c1", "m"+itoa(i), "正文", base.Add(time.Duration(i)*time.Second))
	}

	cases := []struct {
		name          string
		convID, msgID string
	}{
		{"锚点不存在", "c1", "没这条"},
		{"会话不匹配", "c2", "m2"},
		{"空会话", "", "m2"},
		{"空消息", "c1", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := m.Window(c.convID, c.msgID, 2, 2)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Fatalf("应当返回空，得到 %v", idsOf(got))
			}
		})
	}

	// 锚点被清空点隐藏 → 整体空；窗口内的邻居同样遵守清空点。
	if err := m.SetConvCleared("c1", base.Add(2*time.Second+500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	got, err := m.Window("c1", "m2", 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("锚点已被清空时必须返回空，得到 %v", idsOf(got))
	}

	got, err = m.Window("c1", "m4", 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"m4", "m3"}) {
		t.Fatalf("窗口 = %v，期望 [m4 m3]（m1/m2 已被清空）", idsOf(got))
	}
}

// itoa 只用于拼测试数据的 id。
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
