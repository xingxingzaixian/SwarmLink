package app

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
)

// 环境与设置都是快照函数：测试里直接改闭包捕获的变量，模拟「窗口状态变了」。
func newNotifyFixture(t *testing.T, env *NotifyEnv, set *NotifySettings) (*NotifyApp, *clock.Fake) {
	t.Helper()
	return newNotifyFixtureNamed(t, env, set, nil)
}

// newNotifyFixtureNamed 额外注入「发送者名字」查询（nil = 一律查不到）。
func newNotifyFixtureNamed(
	t *testing.T,
	env *NotifyEnv,
	set *NotifySettings,
	nameOf func(identity.NodeID) string,
) (*NotifyApp, *clock.Fake) {
	t.Helper()
	fc := clock.NewFake(time.Unix(1_700_000_000, 0))
	return NewNotifyApp(NotifyDeps{
		Clock:    fc,
		Env:      func() NotifyEnv { return *env },
		Settings: func() NotifySettings { return *set },
		NameOf:   nameOf,
	}), fc
}

// names 造一个「节点 ID → 名字」的查表函数，用于悬停文案测试。
func names(m map[string]string) func(identity.NodeID) string {
	return func(id identity.NodeID) string { return m[id.String()] }
}

// inboundFrom 造一条带发送者、类型与内容的消息。
func inboundFrom(t *testing.T, senderHex, content string, typ message.MsgType) message.Message {
	t.Helper()
	return message.Message{
		MsgID:     "m-" + content,
		ConvID:    "c1",
		SenderID:  mustNodeID(t, senderHex),
		Direction: message.DirectionIn,
		Content:   content,
		MsgType:   typ,
	}
}

func kinds(acts []Act) []ActKind {
	out := make([]ActKind, 0, len(acts))
	for _, a := range acts {
		out = append(out, a.Kind)
	}
	return out
}

func has(acts []Act, k ActKind) bool {
	for _, a := range acts {
		if a.Kind == k {
			return true
		}
	}
	return false
}

// popupOf 取出「自动弹出提示」动作的文案。
func popupOf(acts []Act) (string, bool) {
	for _, a := range acts {
		if a.Kind == ActShowPopup {
			return a.Text, true
		}
	}
	return "", false
}

// 收到新消息要【主动弹出】「谁发来的」，而不是等用户把鼠标移到托盘上（QQ 的做法）。
func TestIncomingMessageEmitsPopup(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{}
	n, _ := newNotifyFixtureNamed(t, &env, &set, names(map[string]string{testPeerHex: "张三"}))

	acts := n.OnChatMessage(inboundFrom(t, testPeerHex, "你好", message.MsgTypeText))

	text, ok := popupOf(acts)
	if !ok {
		t.Fatalf("新消息应产生自动弹出动作（动作=%v）", kinds(acts))
	}
	if text != "张三：你好" {
		t.Errorf("弹出文案 = %q，期望 %q", text, "张三：你好")
	}
}

// 文件/图片消息的 content 是 JSON 信封：提示框里必须给「[文件] 名字」这种人话，
// 不能把 {"job_id":"01a0ed92-…"} 原样弹出来（左栏预览是同一份规则）。
func TestMediaMessagePreviewIsNotRawJSON(t *testing.T) {
	cases := []struct {
		name    string
		typ     message.MsgType
		content string
		want    string
	}{
		{
			"文件（snake_case，当前后端写法）",
			message.MsgTypeFile,
			`{"job_id":"01a0ed92-0204-7b3c","file_name":"SwarmLink.exe"}`,
			"张三：[文件] SwarmLink.exe",
		},
		{
			"文件（Go 字段名的历史数据）",
			message.MsgTypeFile,
			`{"JobID":"j2","FileName":"老安装包.zip"}`,
			"张三：[文件] 老安装包.zip",
		},
		{
			"文件发送失败要点出失败",
			message.MsgTypeFileFailed,
			`{"job_id":"j3","file_name":"丢了.pdf","reason":"文件已被删除或移动"}`,
			"张三：[文件] 丢了.pdf（发送失败）",
		},
		{
			"坏 JSON 宁可少个名字，也不能漏出原文",
			message.MsgTypeFile,
			`{"job_id":`,
			"张三：[文件]",
		},
		{
			"图片",
			message.MsgTypeImage,
			`{"v":1,"mime":"image/png","b64":"iVBORw0KGgo="}`,
			"张三：[图片]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, set := NotifyEnv{WindowVisible: false}, NotifySettings{}
			n, _ := newNotifyFixtureNamed(t, &env, &set, names(map[string]string{testPeerHex: "张三"}))

			text, ok := popupOf(n.OnChatMessage(inboundFrom(t, testPeerHex, tc.content, tc.typ)))
			if !ok {
				t.Fatal("新消息应产生自动弹出动作")
			}
			if text != tc.want {
				t.Errorf("弹出文案 = %q，期望 %q", text, tc.want)
			}
			if strings.ContainsAny(text, "{}") {
				t.Errorf("提示框漏出了 JSON 信封：%q", text)
			}
		})
	}
}

// 不该打扰的场景一个弹出动作都不给：自己发的、正看着该会话、以及传输事件。
//
// 传输事件刻意不弹：它的「谁发来的」没有意义（HoverText 会退化成纯计数），
// 为此在托盘上闪一个空气泡只会让人困惑。
func TestNoPopupWhenSilent(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: true, WindowFocused: true, ActiveConvID: "c1"}, NotifySettings{}
	n, _ := newNotifyFixtureNamed(t, &env, &set, nil)

	// 正看着这个会话
	if _, ok := popupOf(n.OnChatMessage(inboundFrom(t, testPeerHex, "你好", message.MsgTypeText))); ok {
		t.Error("正看着该会话时不该弹提示")
	}

	// 自己发的
	own := inboundFrom(t, testPeerHex, "你好", message.MsgTypeText)
	own.Direction = message.DirectionOut
	if _, ok := popupOf(n.OnChatMessage(own)); ok {
		t.Error("自己发的消息不该弹提示")
	}

	// 传输事件（窗口隐藏，属于「该提示」的场景，但只闪烁/计数，不弹气泡）
	hidden := NotifyEnv{WindowVisible: false}
	n2, _ := newNotifyFixtureNamed(t, &hidden, &set, nil)
	if _, ok := popupOf(n2.OnTransferError()); ok {
		t.Error("传输事件不该弹「谁发来的」气泡")
	}
	if _, ok := popupOf(n2.OnTransferDone()); ok {
		t.Error("传输事件不该弹「谁发来的」气泡")
	}
}

// 连发多条时每次都要给出最新的弹出文案（气泡内容要跟着更新）。
func TestPopupTextFollowsLatestMessage(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{}
	n, _ := newNotifyFixtureNamed(t, &env, &set, names(map[string]string{testPeerHex: "张三"}))

	n.OnChatMessage(inboundFrom(t, testPeerHex, "第一条", message.MsgTypeText))
	acts := n.OnChatMessage(inboundFrom(t, testPeerHex, "第二条", message.MsgTypeText))

	text, ok := popupOf(acts)
	if !ok {
		t.Fatal("第二条消息也应产生弹出动作")
	}
	if !strings.HasPrefix(text, "张三：第二条") {
		t.Errorf("弹出文案 = %q，期望以最新那条开头", text)
	}
}

// titleOf 取出窗口标题动作的文案。
//
// 未读数走窗口标题而不是托盘 tooltip：Windows 下 Wails 启用了
// NOTIFYICON_VERSION_4，该系统模式会抑制标准 tooltip（详见 notify_app.go
// 里 ActWindowTitle 的注释），实测悬停无任何反应。
func titleOf(acts []Act) string {
	for _, a := range acts {
		if a.Kind == ActWindowTitle {
			return a.Text
		}
	}
	return ""
}

func inbound(convID string) message.Message {
	return message.Message{MsgID: "m1", ConvID: convID, Direction: message.DirectionIn}
}

// 规则表：新消息 × 四种环境
func TestChatMessageRules(t *testing.T) {
	cases := []struct {
		name       string
		env        NotifyEnv
		wantSound  bool
		wantFlash  bool
		wantUnread int
	}{
		{"隐藏到托盘 → 响+闪", NotifyEnv{WindowVisible: false}, true, true, 1},
		{"可见未聚焦 → 响+闪", NotifyEnv{WindowVisible: true, WindowFocused: false}, true, true, 1},
		{"聚焦且正是该会话 → 静默", NotifyEnv{WindowVisible: true, WindowFocused: true, ActiveConvID: "c1"}, false, false, 0},
		{"聚焦但别的会话 → 响+闪", NotifyEnv{WindowVisible: true, WindowFocused: true, ActiveConvID: "c2"}, true, true, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, set := tc.env, NotifySettings{Sound: true, MinimizeToTray: true}
			n, _ := newNotifyFixture(t, &env, &set)
			acts := n.OnChatMessage(inbound("c1"))

			if got := has(acts, ActPlaySound); got != tc.wantSound {
				t.Errorf("发声 = %v，期望 %v（动作=%v）", got, tc.wantSound, kinds(acts))
			}
			if got := has(acts, ActFlashOn) || has(acts, ActTrayAlert); got != tc.wantFlash {
				t.Errorf("闪烁 = %v，期望 %v", got, tc.wantFlash)
			}
			if n.Unread() != tc.wantUnread {
				t.Errorf("未读 = %d，期望 %d", n.Unread(), tc.wantUnread)
			}
		})
	}
}

// 自己发的消息永不提示（回显不该响）
func TestOutgoingMessageNeverNotifies(t *testing.T) {
	env, set := NotifyEnv{}, NotifySettings{Sound: true}
	n, _ := newNotifyFixture(t, &env, &set)
	m := inbound("c1")
	m.Direction = message.DirectionOut
	if acts := n.OnChatMessage(m); len(acts) != 0 {
		t.Fatalf("自己发的消息产生了动作：%v", kinds(acts))
	}
	if n.Unread() != 0 {
		t.Fatalf("未读 = %d，期望 0", n.Unread())
	}
}

// 传输事件只在窗口不可见时提示：用户可能正盯着传输列表看进度
func TestTransferEventsOnlyWhenHidden(t *testing.T) {
	events := map[string]func(*NotifyApp) []Act{
		"传输完成": func(n *NotifyApp) []Act { return n.OnTransferDone() },
		"传输失败": func(n *NotifyApp) []Act { return n.OnTransferError() },
	}

	for _, tc := range []struct {
		name    string
		visible bool
		want    bool
	}{
		{"隐藏 → 提示", false, true},
		{"可见（未聚焦也算）→ 静默", true, false},
	} {
		for evName, fire := range events {
			// 每个事件用独立的决策器：同一个实例上连调两次会被 3 秒节流挡掉，
			// 那是刻意的行为（连发十条只响一声），不是这里要断言的东西。
			t.Run(tc.name+"/"+evName, func(t *testing.T) {
				env, set := NotifyEnv{WindowVisible: tc.visible}, NotifySettings{Sound: true}
				n, _ := newNotifyFixture(t, &env, &set)

				acts := fire(n)
				if has(acts, ActPlaySound) != tc.want {
					t.Errorf("发声 = %v，期望 %v（动作=%v）", has(acts, ActPlaySound), tc.want, kinds(acts))
				}
				if got := n.Unread() == 1; got != tc.want {
					t.Errorf("计入未读 = %v，期望 %v", got, tc.want)
				}
			})
		}
	}
}

// 节流：连发多条只响一声，但未读仍按条累加
func TestSoundThrottledButUnreadAccumulates(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{Sound: true}
	n, fc := newNotifyFixture(t, &env, &set)

	first := n.OnChatMessage(inbound("c1"))
	if !has(first, ActPlaySound) {
		t.Fatal("第一条应当发声")
	}

	fc.Advance(time.Second)
	second := n.OnChatMessage(inbound("c1"))
	if has(second, ActPlaySound) {
		t.Error("1 秒内的第二条不应再发声")
	}
	if n.Unread() != 2 {
		t.Errorf("未读 = %d，期望 2（节流只影响声音，不影响未读）", n.Unread())
	}

	fc.Advance(4 * time.Second)
	third := n.OnChatMessage(inbound("c1"))
	if !has(third, ActPlaySound) {
		t.Error("超过节流窗口后应恢复发声")
	}
}

// 静音开关：不发声，其余（未读、闪烁）照旧
func TestSoundDisabledStillCountsUnread(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{Sound: false}
	n, _ := newNotifyFixture(t, &env, &set)
	acts := n.OnChatMessage(inbound("c1"))
	if has(acts, ActPlaySound) {
		t.Error("静音时不应发声")
	}
	if !has(acts, ActTrayAlert) {
		t.Error("静音只关声音，闪烁与未读仍要工作")
	}
	if n.Unread() != 1 {
		t.Errorf("未读 = %d，期望 1", n.Unread())
	}
}

// 焦点回到窗口：停闪、未读归零、标题复位
func TestWindowFocusClearsEverything(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{Sound: true}
	n, _ := newNotifyFixture(t, &env, &set)
	n.OnChatMessage(inbound("c1"))

	acts := n.OnWindowFocus()
	if !has(acts, ActFlashOff) || !has(acts, ActTrayNormal) {
		t.Errorf("焦点恢复应停闪并切回正常图标：%v", kinds(acts))
	}
	if n.Unread() != 0 {
		t.Errorf("未读 = %d，期望 0", n.Unread())
	}
	if title := titleOf(acts); title != "SwarmLink" {
		t.Errorf("窗口标题 = %q，期望复位为 SwarmLink", title)
	}
}

// 隐藏时靠交替托盘图标来「闪」：Tick 必须真的在两种图标间切换
func TestTickAlternatesTrayIconWhenHidden(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{Sound: true}
	n, _ := newNotifyFixture(t, &env, &set)
	n.OnChatMessage(inbound("c1"))

	seen := map[ActKind]int{}
	for i := 0; i < 4; i++ {
		for _, a := range n.Tick() {
			seen[a.Kind]++
		}
	}
	if seen[ActTrayAlert] == 0 || seen[ActTrayNormal] == 0 {
		t.Fatalf("托盘图标没有交替：%v（窗口隐藏时这是唯一的闪烁手段）", seen)
	}

	// 窗口恢复可见后不再换图标（有任务栏按钮，用 FlashWindow）
	env.WindowVisible = true
	if acts := n.Tick(); len(acts) != 0 {
		t.Errorf("可见时不该再换托盘图标：%v", kinds(acts))
	}
}

// 未提示过就不该闪：Tick 是空操作
func TestTickIsNoopWithoutFlash(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{Sound: true}
	n, _ := newNotifyFixture(t, &env, &set)
	if acts := n.Tick(); len(acts) != 0 {
		t.Fatalf("没有任何提示时 Tick 不应产生动作：%v", kinds(acts))
	}
}

// 关闭请求：开关关闭 → 放行退出
func TestCloseAllowsQuitWhenDisabled(t *testing.T) {
	env, set := NotifyEnv{TrayOK: true}, NotifySettings{MinimizeToTray: false}
	n, _ := newNotifyFixture(t, &env, &set)
	if !has(n.OnWindowClose(), ActAllowQuit) {
		t.Fatal("开关关闭时必须放行退出")
	}
}

// 关闭请求：开关开启、托盘可用 → 隐藏到托盘
func TestCloseHidesToTrayWhenEnabled(t *testing.T) {
	env, set := NotifyEnv{TrayOK: true}, NotifySettings{MinimizeToTray: true}
	n, _ := newNotifyFixture(t, &env, &set)
	if !has(n.OnWindowClose(), ActHideToTray) {
		t.Fatal("开关开启且托盘可用时应隐藏到托盘")
	}
}

// 关键失败模式：托盘不可用时必须退化为真正退出，
// 否则用户会遇到「窗口消失、右下角没图标、彻底关不掉」。
func TestCloseFallsBackToQuitWhenTrayUnavailable(t *testing.T) {
	env, set := NotifyEnv{TrayOK: false}, NotifySettings{MinimizeToTray: true}
	n, _ := newNotifyFixture(t, &env, &set)
	if !has(n.OnWindowClose(), ActAllowQuit) {
		t.Fatal("托盘不可用时必须放行退出，不能让用户关不掉应用")
	}
}

// 决策器会被多方【并发】调用：事件总线是同步派发的（网络读循环、传输 worker
// 各自在自己的 goroutine 里 Publish），托盘闪烁计时器又在另一个 goroutine 里
// 调 Tick。因此它必须是并发安全的 —— 这条用例要用 -race 跑才有意义，
// 串行执行时它只会「碰巧通过」。
func TestNotifyAppIsConcurrencySafe(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{Sound: true}
	n, _ := newNotifyFixture(t, &env, &set)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				n.OnChatMessage(inbound("c1"))
				n.Tick()
				n.OnWindowFocus()
				n.OnTransferError()
				_ = n.Unread()
			}
		}()
	}
	wg.Wait()

	// 计数允许因交错而不同，但必须落在合法区间内
	if got := n.Unread(); got < 0 || got > 8*50*2 {
		t.Fatalf("未读计数越界：%d（说明计数被并发写坏了）", got)
	}
}

// 悬停文案是「QQ 式」提示的核心：用户要知道【是谁】发来的，以及大概说了什么。
//
// 这条链路走的是自建原生 tooltip（Wails 的托盘 tooltip 在 Windows 上无效），
// 但「说什么」全部由这里决定，因此规则可以脱离平台单测。
func TestHoverTextShowsSenderAndPreview(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{}
	n, _ := newNotifyFixtureNamed(t, &env, &set, names(map[string]string{testPeerHex: "张三"}))

	n.OnChatMessage(inboundFrom(t, testPeerHex, "你好", message.MsgTypeText))

	if got := n.HoverText(); got != "张三：你好" {
		t.Errorf("悬停文案 = %q，期望 %q", got, "张三：你好")
	}
}

// 同一个人连发多条：只占一行（显示最新那条），条数用尾行表达。
// 否则「对方刷屏 20 条」会把提示框撑成一整屏。
func TestHoverTextDedupesSenderAndKeepsLatest(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{}
	n, _ := newNotifyFixtureNamed(t, &env, &set, names(map[string]string{testPeerHex: "张三"}))

	for _, text := range []string{"第一条", "第二条", "第三条"} {
		n.OnChatMessage(inboundFrom(t, testPeerHex, text, message.MsgTypeText))
	}

	want := "张三：第三条\n共 3 条未读"
	if got := n.HoverText(); got != want {
		t.Errorf("悬停文案 = %q，期望 %q", got, want)
	}
}

// 多个人发来：每人一行，最新的在最上面。
func TestHoverTextListsMultipleSenders(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{}
	n, _ := newNotifyFixtureNamed(t, &env, &set, names(map[string]string{
		testPeerHex: "张三",
		testSelfHex: "李四",
	}))

	n.OnChatMessage(inboundFrom(t, testPeerHex, "你好", message.MsgTypeText))
	n.OnChatMessage(inboundFrom(t, testSelfHex, "在吗", message.MsgTypeText))

	want := "李四：在吗\n张三：你好\n共 2 条未读"
	if got := n.HoverText(); got != want {
		t.Errorf("悬停文案 = %q，期望 %q", got, want)
	}
}

// 图片消息的 content 是内联图的 JSON 信封（几百 KB 的 base64）。
// 悬停文案必须是「[图片]」—— 把 JSON 或 base64 漏到提示框里既没用又难看，
// 而且会瞬间撑出一个巨大的提示框。
func TestHoverTextNeverLeaksImageJSON(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{}
	n, _ := newNotifyFixtureNamed(t, &env, &set, names(map[string]string{testPeerHex: "张三"}))

	raw := `{"v":1,"mime":"image/jpeg","w":1280,"h":720,"b64":"aGVsbG8td29ybGQ="}`
	n.OnChatMessage(inboundFrom(t, testPeerHex, raw, message.MsgTypeImage))

	got := n.HoverText()
	if got != "张三：[图片]" {
		t.Errorf("悬停文案 = %q，期望 %q", got, "张三：[图片]")
	}
	if strings.Contains(got, "b64") || strings.Contains(got, "aGVsbG8") {
		t.Fatalf("悬停文案泄漏了图片数据：%q", got)
	}
}

// 名字与内容都必须截断：提示框不是消息列表，超长内容会把框撑到屏幕外。
func TestHoverTextTruncatesNameAndPreview(t *testing.T) {
	longName := "这是一个特别特别长的显示名超过十二个字符"
	longText := strings.Repeat("很长的内容", 20)

	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{}
	n, _ := newNotifyFixtureNamed(t, &env, &set, names(map[string]string{testPeerHex: longName}))
	n.OnChatMessage(inboundFrom(t, testPeerHex, longText, message.MsgTypeText))

	got := n.HoverText()
	line, _, _ := strings.Cut(got, "\n")
	name, preview, ok := strings.Cut(line, "：")
	if !ok {
		t.Fatalf("悬停文案缺少「名字：内容」结构：%q", got)
	}
	if r := []rune(name); len(r) > 13 || !strings.HasSuffix(name, "…") {
		t.Errorf("名字未按预期截断：%q（%d 字符）", name, len(r))
	}
	if r := []rune(preview); len(r) > 31 || !strings.HasSuffix(preview, "…") {
		t.Errorf("内容未按预期截断：%q（%d 字符）", preview, len(r))
	}
}

// 名字查不到（首见节点、目录里还没登记）也必须能显示，
// 不能出现「空名字：你好」这种破文案。
func TestHoverTextFallsBackWhenNameUnknown(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{}
	n, _ := newNotifyFixtureNamed(t, &env, &set, nil) // 查不到任何名字

	n.OnChatMessage(inboundFrom(t, testPeerHex, "你好", message.MsgTypeText))

	if got := n.HoverText(); got != "未知用户：你好" {
		t.Errorf("悬停文案 = %q，期望回落到「未知用户」", got)
	}
}

// 没有未读时悬停显示应用名（与窗口标题一致）。
func TestHoverTextIsAppNameWithoutUnread(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{}
	n, _ := newNotifyFixtureNamed(t, &env, &set, nil)

	if got := n.HoverText(); got != "SwarmLink" {
		t.Errorf("无未读时悬停文案 = %q，期望 SwarmLink", got)
	}

	n.OnChatMessage(inboundFrom(t, testPeerHex, "你好", message.MsgTypeText))
	n.OnWindowFocus() // 聚焦即已读

	if got := n.HoverText(); got != "SwarmLink" {
		t.Errorf("聚焦后悬停文案 = %q，期望复位为 SwarmLink", got)
	}
}

// 发来消息的人很多时只列最近 3 个，其余用尾行概括 —— 提示框有物理尺寸上限。
func TestHoverTextCapsListedSenders(t *testing.T) {
	peerNameByHex := map[string]string{
		"aabbccddeeff0011": "甲",
		"aabbccddeeff0022": "乙",
		"aabbccddeeff0033": "丙",
		"aabbccddeeff0044": "丁",
		"aabbccddeeff0055": "戊",
		"aabbccddeeff0066": "己",
	}
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{}
	n, _ := newNotifyFixtureNamed(t, &env, &set, names(peerNameByHex))

	for _, hex := range []string{
		"aabbccddeeff0011", "aabbccddeeff0022", "aabbccddeeff0033",
		"aabbccddeeff0044", "aabbccddeeff0055", "aabbccddeeff0066",
	} {
		n.OnChatMessage(inboundFrom(t, hex, "在吗", message.MsgTypeText))
	}

	got := n.HoverText()
	if lines := strings.Split(got, "\n"); len(lines) != 4 {
		t.Fatalf("悬停文案应为「3 行发送者 + 1 行合计」，实得 %d 行：%q", len(lines), got)
	}
	if !strings.Contains(got, "共 6 条未读") {
		t.Errorf("尾行未概括总未读数：%q", got)
	}
	// 最先来的「甲」被挤出列表是刻意的：提示框优先显示最近发生的事
	if !strings.HasPrefix(got, "己：") {
		t.Errorf("最新的发送者应排在最前：%q", got)
	}
}

// 未读数要出现在窗口标题上：窗口在任务栏时这是用户唯一能看到精确条数的地方
// （托盘只有「有/无未读」两态）。
func TestWindowTitleCarriesUnreadCount(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{Sound: false}
	n, _ := newNotifyFixture(t, &env, &set)

	n.OnChatMessage(inbound("c1"))
	n.OnChatMessage(inbound("c1"))
	if got := titleOf(n.OnTransferError()); got != "SwarmLink · 3 条未读" {
		t.Errorf("窗口标题 = %q，期望 SwarmLink · 3 条未读", got)
	}
}

// 标题必须【每次】都给出，哪怕这一条不发声也不闪。
//
// 反例：早期实现把标题动作挂在「发声」分支里，结果静音或节流时标题不会更新 ——
// 未读数就静默地不再变化了。
func TestWindowTitleEmittedEvenWhenSilentAndThrottled(t *testing.T) {
	env, set := NotifyEnv{WindowVisible: false}, NotifySettings{Sound: false}
	n, _ := newNotifyFixture(t, &env, &set)

	for i := 1; i <= 3; i++ {
		acts := n.OnChatMessage(inbound("c1"))
		if has(acts, ActPlaySound) {
			t.Fatalf("第 %d 条：静音开关下不应发声", i)
		}
		want := fmt.Sprintf("SwarmLink · %d 条未读", i)
		if got := titleOf(acts); got != want {
			t.Errorf("第 %d 条窗口标题 = %q，期望 %q", i, got, want)
		}
	}
}
