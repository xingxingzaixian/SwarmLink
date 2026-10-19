package sqlite

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
)

// 本文件钉住搜索 / 跳转定位的语义与【性能地基】。
//
// 语义那部分在 mem 侧有同构副本（mem_search_test.go），两边都写是刻意的：
// 它们是互为替身的两个实现，任何一侧单独漂移都会让「单测跑 mem、线上跑
// sqlite」出现行为差异。
//
// 性能那部分（TestSearchUsesTailFirstPlan）只有 sqlite 侧有 —— 它是 SQL
// 查询计划的问题，内存实现里不存在这个维度。

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

// idsOf 把结果拍成 msg_id 列表，方便整体比较顺序。
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

// LIKE 的元字符必须被转义：
//
// 漏掉转义时搜一个 "%" 会命中全库（用户看到「搜什么都有一万条」），
// 搜 "_" 会命中任意单字。这是本需求最容易漏的一条，单独用例钉死。
func TestSearchEscapesLikeMetacharacters(t *testing.T) {
	db, _ := openTest(t)
	m := NewMessages(db)
	base := time.Unix(1000, 0)

	seedMsg(t, m, "c1", "m1", "进度 100% 了", base)
	seedMsg(t, m, "c1", "m2", "普通消息", base.Add(time.Second))
	seedMsg(t, m, "c1", "m3", "a_b 下划线", base.Add(2*time.Second))
	seedMsg(t, m, "c1", "m4", `C:\path\file.txt`, base.Add(3*time.Second))
	seedMsg(t, m, "c1", "m5", "百分号 % 后面还有", base.Add(4*time.Second))

	all := []string{"c1"}
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"百分号", "%", []string{"m5", "m1"}},
		{"下划线", "_", []string{"m3"}},
		{"反斜杆", `\`, []string{"m4"}},
		{"下划线加任意字符", "_b", []string{"m3"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := m.Search(ports.MessageSearchQuery{Text: c.text, ConvIDs: all})
			if err != nil {
				t.Fatal(err)
			}
			if !equalIDs(idsOf(got), c.want) {
				t.Fatalf("搜 %q = %v，期望 %v（元字符没转义就会命中全库）", c.text, idsOf(got), c.want)
			}
		})
	}
}

// 中文两字词与英文大小写：这两条决定了「不用 FTS5、用 LIKE」这个选择是否成立。
func TestSearchMatchesCJKAndFoldsASCIIOnly(t *testing.T) {
	db, _ := openTest(t)
	m := NewMessages(db)
	base := time.Unix(2000, 0)

	seedMsg(t, m, "c1", "m1", "明天的会议要讨论接口设计", base)
	seedMsg(t, m, "c1", "m2", "Quick brown fox", base.Add(time.Second))
	seedMsg(t, m, "c1", "m3", "Ärger mit Übermut", base.Add(2*time.Second))
	seedMsg(t, m, "c2", "m4", "会议纪要 #2", base.Add(3*time.Second))

	all := []string{"c1", "c2"}
	t.Run("中文两字词", func(t *testing.T) {
		// trigram 分词器在这里会【静默返回 0 命中】，这正是本用例的存在理由。
		got, err := m.Search(ports.MessageSearchQuery{Text: "会议", ConvIDs: all})
		if err != nil {
			t.Fatal(err)
		}
		if !equalIDs(idsOf(got), []string{"m4", "m1"}) {
			t.Fatalf("搜「会议」= %v，期望 [m4 m1]", idsOf(got))
		}
	})
	t.Run("英文大小写不敏感", func(t *testing.T) {
		for _, text := range []string{"quick", "QUICK", "QuIcK"} {
			got, err := m.Search(ports.MessageSearchQuery{Text: text, ConvIDs: all})
			if err != nil {
				t.Fatal(err)
			}
			if !equalIDs(idsOf(got), []string{"m2"}) {
				t.Fatalf("搜 %q = %v，期望 [m2]", text, idsOf(got))
			}
		}
	})
	t.Run("非 ASCII 不折叠", func(t *testing.T) {
		// SQLite 的 LIKE 只对 ASCII 忽略大小写；这条同时是 mem 实现里
		// likeContains 不用 strings.ToLower 的理由。
		got, err := m.Search(ports.MessageSearchQuery{Text: "ärger", ConvIDs: all})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("搜「ärger」不应命中「Ärger」，得到 %v", idsOf(got))
		}
	})
}

// 会话清空点必须被遵守：搜得到已清空的消息就是隐私回退（界面上明明没有）。
func TestSearchRespectsConvClear(t *testing.T) {
	db, _ := openTest(t)
	m := NewMessages(db)

	seedMsg(t, m, "c1", "m1", "会议一", time.UnixMilli(1_000))
	seedMsg(t, m, "c1", "m2", "会议二", time.UnixMilli(2_000))
	seedMsg(t, m, "c1", "m3", "会议三", time.UnixMilli(3_000))
	seedMsg(t, m, "c2", "m4", "会议四", time.UnixMilli(2_500))

	q := ports.MessageSearchQuery{Text: "会议", ConvIDs: []string{"c1", "c2"}}

	if err := m.SetConvCleared("c1", time.UnixMilli(2_000)); err != nil {
		t.Fatal(err)
	}
	got, err := m.Search(q)
	if err != nil {
		t.Fatal(err)
	}
	// 边界是【严格大于】：清空点那一毫秒的消息也要消失，与前端 activeMessages 一致。
	if !equalIDs(idsOf(got), []string{"m4", "m3"}) {
		t.Fatalf("清空后搜 = %v，期望 [m4 m3]（m2 在清空点上，必须消失）", idsOf(got))
	}

	if err := m.RemoveConvClear("c1"); err != nil {
		t.Fatal(err)
	}
	got, err = m.Search(q)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"m4", "m3", "m2", "m1"}) {
		t.Fatalf("撤销清空后搜 = %v，期望全部 4 条回来", idsOf(got))
	}
}

// 空白名单必须返回空，不能退化成全库搜索：
// 调用方传空集合的语义是「一个可见会话都没有」，而不是「不限制」。
func TestSearchWhitelistIsMandatory(t *testing.T) {
	db, _ := openTest(t)
	m := NewMessages(db)
	base := time.Unix(3000, 0)
	seedMsg(t, m, "c1", "m1", "会议", base)
	seedMsg(t, m, "c2", "m2", "会议", base.Add(time.Second))

	got, err := m.Search(ports.MessageSearchQuery{Text: "会议"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("空白名单必须返回空（否则已移除的对端会被搜出来），得到 %v", idsOf(got))
	}

	got, err = m.Search(ports.MessageSearchQuery{Text: "会议", ConvIDs: []string{"c2"}})
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"m2"}) {
		t.Fatalf("白名单只认 c2，得到 %v", idsOf(got))
	}
}

// 游标分页必须不重不漏 —— 尤其是【同一毫秒】写入的连续消息：
// 这正是「游标不能是时间戳」的现场（按 sent_at 分页会把整页同毫秒的命中丢掉）。
func TestSearchCursorPagingIsExact(t *testing.T) {
	db, _ := openTest(t)
	m := NewMessages(db)

	sameMS := time.UnixMilli(4_000)
	want := make([]string, 0, 10)
	for i := 1; i <= 10; i++ {
		id := "m" + itoa(i)
		seedMsg(t, m, "c1", id, "会议纪要", sameMS) // 全部同一毫秒
		want = append(want, id)
	}
	// want 现在是插入顺序（旧→新），搜索结果应当是新→旧。
	for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
		want[i], want[j] = want[j], want[i]
	}

	var got []string
	cursor := ""
	pages := 0
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
		// 页内必须严格递减（隐含「无重复」）
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

	if len(got) != len(want) {
		t.Fatalf("分页共 %d 条，期望 %d 条（重复或缺漏）: got=%v", len(got), len(want), got)
	}
	if !equalIDs(got, want) {
		t.Fatalf("分页结果 = %v，期望 %v", got, want)
	}
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

// 游标指向不存在的消息时必须返回空：
// 退化成「首页」会让前端把第一页重复贴一遍（看起来像是「又翻出了一批」）。
func TestSearchUnknownCursorReturnsEmpty(t *testing.T) {
	db, _ := openTest(t)
	m := NewMessages(db)
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

// 排序键是【写入序】而不是 sent_at：
// 入站消息带的是发送方时间戳，乱序到达是常态；如果按 sent_at 排序，翻页会
// 在「同一毫秒 / 回退的时间戳」上错位。两个实现必须都按写入序。
func TestSearchOrderIsInsertionOrderNotSentAt(t *testing.T) {
	db, _ := openTest(t)
	m := NewMessages(db)

	// 先写入的反而「更新」——模拟对方时间戳不准 / 乱序到达。
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

// 跳转定位：窗口必须是「紧邻锚点的 before/after 条」，不是「整段的首尾」。
func TestWindowAroundAnchor(t *testing.T) {
	db, _ := openTest(t)
	m := NewMessages(db)

	base := time.UnixMilli(6_000)
	for i := 1; i <= 10; i++ {
		seedMsg(t, m, "c1", "m"+itoa(i), "正文", base.Add(time.Duration(i)*time.Second))
	}
	seedMsg(t, m, "c2", "x1", "别的会话", base)

	// 新→旧：命中在中间
	got, err := m.Window("c1", "m5", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"m8", "m7", "m6", "m5", "m4", "m3"}) {
		t.Fatalf("窗口 = %v，期望 [m8 m7 m6 m5 m4 m3]", idsOf(got))
	}

	// 尾部：没有更新的消息
	got, err = m.Window("c1", "m10", 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"m10", "m9", "m8", "m7"}) {
		t.Fatalf("尾部窗口 = %v，期望 [m10 m9 m8 m7]", idsOf(got))
	}

	// 头部：没有更旧的消息
	got, err = m.Window("c1", "m1", 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"m3", "m2", "m1"}) {
		t.Fatalf("头部窗口 = %v，期望 [m3 m2 m1]", idsOf(got))
	}

	// 只要命中本身
	got, err = m.Window("c1", "m5", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"m5"}) {
		t.Fatalf("零窗口 = %v，期望只有 [m5]", idsOf(got))
	}
}

// 锚点与邻居落在同一毫秒时，命中仍必须在窗口里、且不重不漏。
//
// 这是窗口边界用 (sent_at, id) 行值而不是单 sent_at 的全部理由：
// 群发 / 连发 / 入站消息共用发送方时间戳时，「同一毫秒」是常态，
// 只按 sent_at 分界会让锚点与邻居互踩，命中被挤出窗口 —— 那不是显示瑕疵，
// 是「点了搜索结果却跳不到那条消息」。
func TestWindowKeepsHitWhenNeighborsShareMillisecond(t *testing.T) {
	db, _ := openTest(t)
	m := NewMessages(db)

	sameMS := time.UnixMilli(9_000)
	for i := 1; i <= 5; i++ {
		seedMsg(t, m, "c1", "m"+itoa(i), "同毫秒", sameMS)
	}

	got, err := m.Window("c1", "m3", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	// 新→旧：新侧是紧邻锚点之后的 m4、m5（按写入序），旧侧是 m3 与它前面的 m2。
	if !equalIDs(idsOf(got), []string{"m5", "m4", "m3", "m2"}) {
		t.Fatalf("窗口 = %v，期望 [m5 m4 m3 m2]（锚点必须在窗口里且不重不漏）", idsOf(got))
	}

	// 零窗口也必须有锚点本身
	got, err = m.Window("c1", "m3", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"m3"}) {
		t.Fatalf("零窗口 = %v，期望只有 [m3]", idsOf(got))
	}

	// 同一毫秒里每一条都必须能定位到（逐条跳转）
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

// 锚点不可用（不存在 / 会话不匹配 / 已被清空）必须返回空 —— 调用方据此回退，
// 而不是拿到一个「跳到了别处」的窗口。
func TestWindowRejectsUnusableAnchor(t *testing.T) {
	db, _ := openTest(t)
	m := NewMessages(db)

	base := time.UnixMilli(7_000)
	for i := 1; i <= 4; i++ {
		seedMsg(t, m, "c1", "m"+itoa(i), "正文", base.Add(time.Duration(i)*time.Second))
	}

	cases := []struct {
		name   string
		convID string
		msgID  string
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

	// 锚点被清空点隐藏：必须整体返回空，而不是「只给出更旧的那些」。
	if err := m.SetConvCleared("c1", base.Add(2*time.Second+500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	got, err := m.Window("c1", "m2", 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("锚点已被清空时必须返回空（调用方据此回退到最新），得到 %v", idsOf(got))
	}

	// 窗口内的邻居同样遵守清空点：只剩 m3、m4
	got, err = m.Window("c1", "m4", 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(idsOf(got), []string{"m4", "m3"}) {
		t.Fatalf("窗口 = %v，期望 [m4 m3]（m1/m2 已被清空）", idsOf(got))
	}
}

// planCase 是一条「查询计划必须是这个样子」的断言。
type planCase struct {
	name  string
	sql   string
	args  []any
	want  []string // 计划里必须出现的片段
	avoid []string // 计划里不得出现的片段
}

// 性能地基：四条查询的计划必须钉死，而且必须在**无统计信息**的状态下也是对的。
//
// 为什么值得用查询计划做断言：同一句 SQL 的计划差 200~500 倍（0.5ms vs 543ms），
// 而计划会被看似无害的改动推翻 —— 加一个 count(*)、把 ORDER BY 换掉、
// 或者仅仅让规划器改变主意。单测全绿，只是 20 万条消息时首屏从 1 毫秒变成半秒。
//
// 为什么必须先在无统计信息的状态下断言：本应用从不 ANALYZE（全仓只有测试里
// 出现过这个词），那才是生产上的规划器输入。第一版守卫先 ANALYZE 再断言，
// 于是完整漏掉了「无统计信息时搜索退化成 ix_msg_conv_time + TEMP B-TREE、
// 20 万条 543ms」这个真实缺陷；SQL 里的 NOT INDEXED / INDEXED BY 就是那次
// 事故的修复（见 ADR-013）。
func TestQueryPlansArePinned(t *testing.T) {
	db, _ := openTest(t)
	m := NewMessages(db)

	base := time.UnixMilli(8_000)
	convs := make([]string, 0, 5)
	for c := 0; c < 5; c++ {
		conv := "c" + itoa(c)
		convs = append(convs, conv)
		for i := 0; i < 20; i++ {
			seedMsg(t, m, conv, conv+"-"+itoa(i), "会议正文", base.Add(time.Second))
		}
	}
	searchFirstSQL, searchFirstArgs, ok := searchSQL(ports.MessageSearchQuery{Text: "会议", ConvIDs: convs})
	if !ok {
		t.Fatal("searchSQL 拒绝了这次查询")
	}
	searchPageSQL, searchPageArgs, ok := searchSQL(ports.MessageSearchQuery{
		Text: "会议", ConvIDs: convs, BeforeMsgID: "c0-10",
	})
	if !ok {
		t.Fatal("searchSQL 拒绝了分页查询")
	}

	noTemp := "计划里出现了 TEMP B-TREE（排序/临时表），性能地基被推翻"

	cases := []planCase{
		{
			name: "搜索首页",
			sql:  searchFirstSQL, args: searchFirstArgs,
			// 首页必须从 rowid 尾部倒扫、凑够 LIMIT 就停。
			avoid: []string{"TEMP B-TREE"},
		},
		{
			name: "搜索游标页",
			sql:  searchPageSQL, args: searchPageArgs,
			want:  []string{"INTEGER PRIMARY KEY"},
			avoid: []string{"TEMP B-TREE"},
		},
		{
			name: "窗口新侧",
			sql:  windowNewerSQL, args: []any{convs[0], int64(1), int64(2), 20},
			want:  []string{"ix_msg_conv_time"},
			avoid: []string{"TEMP B-TREE"},
		},
		{
			name: "窗口旧侧",
			sql:  windowOlderSQL, args: []any{convs[0], int64(1), int64(2), 21},
			want:  []string{"ix_msg_conv_time"},
			avoid: []string{"TEMP B-TREE"},
		},
	}

	// 【必须】先在「无统计信息」的状态下断言：本应用从不 ANALYZE（全仓只有测试里
	// 出现过这个词），那才是生产上的规划器输入。第一版计划守卫先 ANALYZE 再断言，
	// 于是漏掉了「无统计信息时搜索会退化成 ix_msg_conv_time + TEMP B-TREE、
	// 20 万条下 543ms」这个真实缺陷（见 ADR-013）。
	t.Run("无统计信息（生产常态）", func(t *testing.T) {
		assertPlans(t, db, cases, noTemp)
	})

	// 再断言一次「有统计信息」的状态：SQL 里的 NOT INDEXED / INDEXED BY 让两种
	// 状态给出同一个计划，这条子测试就是那个不变量的守卫。
	t.Run("ANALYZE 后", func(t *testing.T) {
		if _, err := db.db.Exec(`ANALYZE`); err != nil {
			t.Fatal(err)
		}
		assertPlans(t, db, cases, noTemp)
	})
}

func assertPlans(t *testing.T, db *DB, cases []planCase, noTemp string) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := explainPlan(t, db, c.sql, c.args...)
			// 把计划打进 -v 输出：断言失败时需要的正是「当时实际是什么计划」，
			// 而事后重跑往往已经不是同一个 SQLite 版本了。
			t.Logf("查询计划：\n%s", plan)
			for _, bad := range c.avoid {
				if strings.Contains(plan, bad) {
					t.Fatalf("%s：%s\n%s", c.name, noTemp, plan)
				}
			}
			for _, want := range c.want {
				if !strings.Contains(plan, want) {
					t.Fatalf("%s：计划里没有 %q\n%s", c.name, want, plan)
				}
			}
		})
	}
}

// seedMany 一次性灌入 convs×perConv 条消息（其中 1/17 含「会议」），返回会话 ID 列表。
//
// 直接走底层连接：用 Append 会把每次写入串行化，20 万条要几分钟，
// 没人会愿意跑这个基准 / 诊断。
func seedMany(tb testing.TB, db *DB, convs, perConv int) []string {
	tb.Helper()
	convIDs := make([]string, 0, convs)
	base := time.Now().Add(-30 * 24 * time.Hour)

	tx, err := db.db.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	for c := 0; c < convs; c++ {
		conv := "bench-conv-" + itoa(c)
		convIDs = append(convIDs, conv)
		if _, err := tx.Exec(`INSERT INTO conversations(conv_id, kind, title) VALUES(?,?,?)`,
			conv, "group", conv); err != nil {
			tb.Fatal(err)
		}
	}
	stmt, err := tx.Prepare(`INSERT INTO messages(msg_id, conv_id, sender_id, direction, content, msg_type, sent_at, state)
		VALUES(?,?,?,'in',?,'text',?,'delivered')`)
	if err != nil {
		tb.Fatal(err)
	}
	for c := 0; c < convs; c++ {
		for i := 0; i < perConv; i++ {
			n := c*perConv + i
			content := "第 " + itoa(n) + " 条普通消息，讨论项目进度与排期安排。"
			switch n % 17 {
			case 0:
				content = "第 " + itoa(n) + " 条：明天的会议要讨论接口设计。"
			case 1:
				content = "第 " + itoa(n) + " 条：quick brown fox jumps over the lazy dog."
			}
			if _, err := stmt.Exec("m-"+itoa(n), convIDs[c], "aabb",
				content, base.Add(time.Duration(n)*time.Second).UnixMilli()); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	return convIDs
}

// BenchmarkSearch200k 用【真实代码路径】复现 ADR-013 的实测表。
//
// 它不是回归守卫（基准只在显式 -bench 时运行，日常 go test 不跑），
// 而是留给后人的一把尺子：改了排序、加了 count、换了分页方式之后，
// 用同一条命令就能看到代价。
//
//	go test ./internal/adapters/store/sqlite/ -run XXX -bench BenchmarkSearch200k -benchtime 20x
//
// 规模与 ADR-013 一致：20 个会话 × 1 万条 = 20 万条。
func BenchmarkSearch200k(b *testing.B) {
	db, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	m := NewMessages(db)

	convIDs := seedMany(b, db, 20, 10000)

	cases := []struct {
		name string
		q    ports.MessageSearchQuery
	}{
		{"首页-常见词", ports.MessageSearchQuery{Text: "会议", ConvIDs: convIDs}},
		{"游标页-常见词", ports.MessageSearchQuery{Text: "会议", ConvIDs: convIDs, BeforeMsgID: "m-100000"}},
		{"零命中-最坏全扫", ports.MessageSearchQuery{Text: "不存在的词xyz", ConvIDs: convIDs}},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				got, err := m.Search(c.q)
				if err != nil {
					b.Fatal(err)
				}
				if len(got) > ports.DefaultSearchLimit {
					b.Fatalf("返回条数 %d 超过上限", len(got))
				}
			}
		})
	}
}

func explainPlan(t *testing.T, db *DB, q string, args ...any) string {
	t.Helper()
	rows, err := db.db.Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id, parent int
		var notUsed, detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, "  "+detail)
	}
	return strings.Join(lines, "\n")
}

// itoa 只用于拼测试数据的 id（m1..m10），不引入 strconv 免得和主代码混淆。
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
