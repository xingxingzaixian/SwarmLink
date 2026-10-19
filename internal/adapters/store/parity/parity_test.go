// Package parity 把 mem 与 sqlite 两个「互为替身」的仓储放在同一套场景上对拍。
//
// 为什么单独开一个包，而不是塞进任一实现包：R2 红线禁止适配器之间互相 import
// （测试文件豁免，见 internal/archtest 的 TestR2），把对拍放在这里，「对等」
// 这件事就有了一个中立的家 —— 谁都不必认识对方。
//
// 为什么需要它：两侧各有一份同构的单测（messages_search_test.go 与
// mem_search_test.go），但它们可能【同时写错同一个期望值】—— 那种情况下两边
// 都绿，而单测跑 mem、线上跑 sqlite 的差异会直抵用户。对拍把「同一个输入必须
// 得到同一个输出」变成一条机械可验证的断言。
package parity

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/store/mem"
	"github.com/swarmlink/swarmlink/internal/adapters/store/sqlite"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
)

// newRepos 给出一对干净实现，键是名字（用于 t.Run 与失败信息）。
//
// 返回 ports.ChatStore 而不是具体类型：对拍只能依赖公共接口，否则这个测试本身
// 就成了「适配器互相认识」的通道。
func newRepos(t *testing.T) map[string]ports.ChatStore {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "parity.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return map[string]ports.ChatStore{
		"mem":    mem.NewMessages(clock.New()),
		"sqlite": sqlite.NewMessages(db),
	}
}

func seed(t *testing.T, r ports.ChatStore, conv, id, content string, at time.Time) {
	t.Helper()
	err := r.Append(message.Message{
		MsgID: id, ConvID: conv,
		Direction: message.DirectionIn, Content: content,
		MsgType: message.MsgTypeText, SentAt: at, State: message.StateDelivered,
	})
	if err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

func ids(ms []message.Message) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.MsgID)
	}
	return out
}

func searchIDs(t *testing.T, r ports.ChatStore, q ports.MessageSearchQuery) []string {
	t.Helper()
	ms, err := r.Search(q)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return ids(ms)
}

func windowIDs(t *testing.T, r ports.ChatStore, conv, msgID string, before, after int) []string {
	t.Helper()
	ms, err := r.Window(conv, msgID, before, after)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	return ids(ms)
}

func same(a, b []string) bool {
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

// 一套场景。setup / run 对所有实现【完全相同】，因为两者满足同一个接口。
type scenario struct {
	name  string
	setup func(t *testing.T, r ports.ChatStore)
	run   func(t *testing.T, r ports.ChatStore) []string
	want  []string // 非 nil 时同时钉住期望值（两侧都必须得到它）
}

func TestSearchAndWindowParity(t *testing.T) {
	base := time.UnixMilli(1_700_000_000_000)
	at := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }

	// 三个会话的公共语料。含：中文两字词、大小写混排、非 ASCII、LIKE 元字符。
	common := func(t *testing.T, r ports.ChatStore) {
		seed(t, r, "c1", "m1", "明天的会议要讨论接口设计", at(1))
		seed(t, r, "c1", "m2", "Quick brown fox", at(2))
		seed(t, r, "c1", "m3", `C:\path\file.txt`, at(3))
		seed(t, r, "c1", "m4", "进度 100% 了", at(4))
		seed(t, r, "c1", "m5", "a_b 下划线", at(5))
		seed(t, r, "c1", "m6", "Ärger mit Übermut", at(6))
		seed(t, r, "c2", "n1", "会议纪要 #2", at(7))
		seed(t, r, "c3", "o1", "会议纪要 #3", at(8))
	}

	allConvs := []string{"c1", "c2", "c3"}

	scenarios := []scenario{
		{
			name:  "LIKE 百分号必须转义",
			setup: common,
			run: func(t *testing.T, r ports.ChatStore) []string {
				return searchIDs(t, r, ports.MessageSearchQuery{Text: "%", ConvIDs: allConvs})
			},
			want: []string{"m4"},
		},
		{
			name:  "LIKE 下划线必须转义",
			setup: common,
			run: func(t *testing.T, r ports.ChatStore) []string {
				return searchIDs(t, r, ports.MessageSearchQuery{Text: "_", ConvIDs: allConvs})
			},
			want: []string{"m5"},
		},
		{
			name:  "反斜杆必须转义",
			setup: common,
			run: func(t *testing.T, r ports.ChatStore) []string {
				return searchIDs(t, r, ports.MessageSearchQuery{Text: `\`, ConvIDs: allConvs})
			},
			want: []string{"m3"},
		},
		{
			name:  "中文两字词",
			setup: common,
			run: func(t *testing.T, r ports.ChatStore) []string {
				return searchIDs(t, r, ports.MessageSearchQuery{Text: "会议", ConvIDs: allConvs})
			},
			want: []string{"o1", "n1", "m1"},
		},
		{
			name:  "英文大小写不敏感",
			setup: common,
			run: func(t *testing.T, r ports.ChatStore) []string {
				return searchIDs(t, r, ports.MessageSearchQuery{Text: "qUiCk", ConvIDs: allConvs})
			},
			want: []string{"m2"},
		},
		{
			name:  "非 ASCII 不折叠",
			setup: common,
			run: func(t *testing.T, r ports.ChatStore) []string {
				return searchIDs(t, r, ports.MessageSearchQuery{Text: "ärger", ConvIDs: allConvs})
			},
			want: []string{},
		},
		{
			name:  "会话白名单",
			setup: common,
			run: func(t *testing.T, r ports.ChatStore) []string {
				return searchIDs(t, r, ports.MessageSearchQuery{Text: "会议", ConvIDs: []string{"c3", "c2"}})
			},
			want: []string{"o1", "n1"},
		},
		{
			name:  "空白名单返回空而非全库",
			setup: common,
			run: func(t *testing.T, r ports.ChatStore) []string {
				return searchIDs(t, r, ports.MessageSearchQuery{Text: "会议"})
			},
			want: []string{},
		},
		{
			name: "清空点（含边界那一条）",
			setup: func(t *testing.T, r ports.ChatStore) {
				common(t, r)
				// 清空点到 m2 那一毫秒：m2 自己也要消失（严格大于才可见）。
				if err := r.SetConvCleared("c1", at(2)); err != nil {
					t.Fatal(err)
				}
			},
			run: func(t *testing.T, r ports.ChatStore) []string {
				return searchIDs(t, r, ports.MessageSearchQuery{Text: "会议", ConvIDs: allConvs})
			},
			want: []string{"o1", "n1"},
		},
		{
			name: "撤销清空后消息回来",
			setup: func(t *testing.T, r ports.ChatStore) {
				common(t, r)
				if err := r.SetConvCleared("c1", at(2)); err != nil {
					t.Fatal(err)
				}
				if err := r.RemoveConvClear("c1"); err != nil {
					t.Fatal(err)
				}
			},
			run: func(t *testing.T, r ports.ChatStore) []string {
				return searchIDs(t, r, ports.MessageSearchQuery{Text: "会议", ConvIDs: allConvs})
			},
			want: []string{"o1", "n1", "m1"},
		},
		{
			name: "同一毫秒的游标分页不重不漏",
			setup: func(t *testing.T, r ports.ChatStore) {
				// 全部同一毫秒：按 sent_at 分页会在边界上整页丢掉，按写入序不会。
				for i := 1; i <= 7; i++ {
					seed(t, r, "c1", "p"+itoa(i), "会议", base)
				}
			},
			run: func(t *testing.T, r ports.ChatStore) []string {
				var out []string
				cursor := ""
				for page := 0; page < 10; page++ {
					got := searchIDs(t, r, ports.MessageSearchQuery{
						Text: "会议", ConvIDs: []string{"c1"}, Limit: 3, BeforeMsgID: cursor,
					})
					if len(got) == 0 {
						break
					}
					out = append(out, got...)
					cursor = got[len(got)-1]
				}
				return out
			},
			want: []string{"p7", "p6", "p5", "p4", "p3", "p2", "p1"},
		},
		{
			name:  "未知游标返回空",
			setup: common,
			run: func(t *testing.T, r ports.ChatStore) []string {
				return searchIDs(t, r, ports.MessageSearchQuery{
					Text: "会议", ConvIDs: allConvs, BeforeMsgID: "没这条",
				})
			},
			want: []string{},
		},
		{
			name: "排序按写入序而非 sent_at",
			setup: func(t *testing.T, r ports.ChatStore) {
				seed(t, r, "c1", "late", "会议", at(90))  // 先写入但时间戳更大
				seed(t, r, "c1", "early", "会议", at(10)) // 后写入但时间戳更小
			},
			run: func(t *testing.T, r ports.ChatStore) []string {
				return searchIDs(t, r, ports.MessageSearchQuery{Text: "会议", ConvIDs: []string{"c1"}})
			},
			want: []string{"early", "late"},
		},
		{
			name: "跳转窗口（中间命中）",
			setup: func(t *testing.T, r ports.ChatStore) {
				for i := 1; i <= 6; i++ {
					seed(t, r, "c1", "w"+itoa(i), "正文", at(i))
				}
			},
			run: func(t *testing.T, r ports.ChatStore) []string {
				return windowIDs(t, r, "c1", "w3", 2, 2)
			},
			want: []string{"w5", "w4", "w3", "w2", "w1"},
		},
		{
			name: "跳转窗口（同一毫秒）",
			setup: func(t *testing.T, r ports.ChatStore) {
				for i := 1; i <= 5; i++ {
					seed(t, r, "c1", "s"+itoa(i), "同毫秒", base)
				}
			},
			run: func(t *testing.T, r ports.ChatStore) []string {
				return windowIDs(t, r, "c1", "s3", 1, 2)
			},
			// 行值边界 (sent_at, 写入序) 的意义就在这条：锚点必须留在窗口里，
			// 同一毫秒的邻居不重不漏。
			want: []string{"s5", "s4", "s3", "s2"},
		},
		{
			name: "跳转窗口（写入序与时间戳不一致）",
			setup: func(t *testing.T, r ports.ChatStore) {
				// 窗口按 (sent_at, 写入序) 排（= 显示序），不是纯写入序：
				// 入站消息带的是发送方时间戳，乱序到达是常态。
				seed(t, r, "c1", "late", "正文", at(90))
				seed(t, r, "c1", "early", "正文", at(10))
				seed(t, r, "c1", "mid", "正文", at(50))
			},
			run: func(t *testing.T, r ports.ChatStore) []string {
				return windowIDs(t, r, "c1", "early", 0, 2)
			},
			want: []string{"late", "mid", "early"},
		},
		{
			name: "跳转窗口（锚点不可用）",
			setup: func(t *testing.T, r ports.ChatStore) {
				for i := 1; i <= 6; i++ {
					seed(t, r, "c1", "w"+itoa(i), "正文", at(i))
				}
			},
			run: func(t *testing.T, r ports.ChatStore) []string {
				// 会话不匹配与锚点不存在都必须是空（调用方据此回退到最新）。
				a := windowIDs(t, r, "c2", "w3", 2, 2)
				b := windowIDs(t, r, "c1", "没这条", 2, 2)
				return append(a, b...)
			},
			want: []string{},
		},
		{
			name: "跳转窗口遵守清空点",
			setup: func(t *testing.T, r ports.ChatStore) {
				for i := 1; i <= 6; i++ {
					seed(t, r, "c1", "w"+itoa(i), "正文", at(i))
				}
				if err := r.SetConvCleared("c1", at(3)); err != nil {
					t.Fatal(err)
				}
			},
			run: func(t *testing.T, r ports.ChatStore) []string {
				return windowIDs(t, r, "c1", "w5", 3, 1)
			},
			want: []string{"w6", "w5", "w4"},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			repos := newRepos(t)
			results := make(map[string][]string, len(repos))
			names := make([]string, 0, len(repos))
			for _, name := range []string{"mem", "sqlite"} {
				r := repos[name]
				sc.setup(t, r)
				results[name] = sc.run(t, r)
				names = append(names, name)
			}

			if !same(results["mem"], results["sqlite"]) {
				t.Fatalf("两个实现分叉了：mem=%v sqlite=%v", results["mem"], results["sqlite"])
			}
			if sc.want != nil && !same(results["mem"], sc.want) {
				t.Fatalf("结果 = %v，期望 %v（%s）", results["mem"], sc.want, names)
			}
		})
	}
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
