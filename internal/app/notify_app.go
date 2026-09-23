package app

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
)

// ActKind 是提示动作的类型。
//
// 用「值」而不是回调接口：测试只需断言「产生了哪些动作」，
// 不必 mock 一个 Wails 适配器；Wails 相关的执行全部留在组合根。
type ActKind int

const (
	ActPlaySound ActKind = iota
	ActFlashOn
	ActFlashOff
	ActTrayAlert  // 托盘图标切成带红点版
	ActTrayNormal // 切回正常版

	// ActWindowTitle 让组合根把窗口标题设成 Text。
	//
	// 【为什么是窗口标题而不是托盘 tooltip】托盘 tooltip 在 Windows 上拿不到。
	// Wails 建图标时会发 NIM_SETVERSION + NOTIFYICON_VERSION_4
	// （systemtray_windows.go:619），而 MSDN 明确规定该模式下
	// "the standard tooltip is suppressed"；Wails 虽然补了 NIF_SHOWTIP，
	// 但它把 NIM_MODIFY 的失败静默吞掉（systemtray_windows.go:524），
	// 实测在 Windows 10 19045 上悬停毫无反应。窗口标题则是 100% 可见的位置
	// （窗口在任务栏时），且不依赖任何平台细节。
	ActWindowTitle

	// ActShowPopup 让组合根把 Text 作为「新消息气泡」主动弹在托盘附近。
	//
	// 与 ActWindowTitle 同时产生：标题给计数（常驻、可见于任务栏），
	// 气泡给「谁发来的」（消息到达即弹、几秒后自动消失，QQ 的做法）。
	// 位置由组合根决定：悬停时可以用光标位置，主动弹出时光标在别处，
	// 必须自己去找通知区域。
	ActShowPopup

	ActHideToTray // 关闭请求 → 隐藏窗口、保持进程
	ActAllowQuit  // 关闭请求 → 放行退出
)

// Act 是一个待执行的动作。
type Act struct {
	Kind ActKind
	Text string
}

// NotifyEnv 是决策所需的环境快照（窗口状态是 Wails 的事，这里只读值）。
type NotifyEnv struct {
	WindowVisible bool
	WindowFocused bool
	ActiveConvID  string
	// TrayOK 表示托盘图标是否真的建起来了：它是「关闭 = 收进托盘」的前提。
	TrayOK bool
}

// NotifySettings 是决策所需的设置快照。
type NotifySettings struct {
	Sound          bool
	MinimizeToTray bool
}

const (
	defaultSoundThrottle = 3 * time.Second
	// appTitleIdle 是无未读时的窗口标题，必须与窗口创建时的标题一致。
	appTitleIdle = "SwarmLink"

	// 悬停提示（QQ 式「谁发来的」）的排版上限。
	//
	// 提示框不是消息列表：屏幕上的可用空间有限，撑太大反而看不清。
	hoverSendersShown = 3  // 最多列几个发送者
	hoverSendersKeep  = 5  // 最多记几个（多留两个，列表滚动时不至于立刻丢掉）
	hoverNameMax      = 12 // 名字截断长度（字符数）
	hoverPreviewMax   = 30 // 内容截断长度（字符数）

	hoverUnknownName  = "未知用户"
	hoverImagePreview = "[图片]"
	hoverEmptyPreview = "发来一条消息"
)

// NotifyDeps 是决策器的依赖集合。
//
// 用结构体而不是四个位置参数：它们全是函数，位置参数会让调用点变成
// 一串难以阅读的闭包，而且很容易把 env 与 set 传反。
type NotifyDeps struct {
	Clock    ports.Clock
	Env      func() NotifyEnv
	Settings func() NotifySettings
	// NameOf 把节点 ID 变成给人看的名字；查不到返回空串即可（会被回落到「未知用户」）。
	// 由组合根注入，本层因此不需要认识节点目录；nil 安全。
	NameOf func(identity.NodeID) string
}

// hoverSender 记着「最近是谁发来的、大概说了什么」。
type hoverSender struct {
	name    string
	preview string
}

// NotifyApp 决定「什么时候提示、提示成什么样」。
//
// 零 Wails 依赖、零 goroutine：环境与设置都由组合根以快照函数注入，
// 时间来自 ports.Clock，因此整张规则表都能用 FakeClock 表驱动测试。
// 动作只以值的形式返回，由组合根翻译成 win.Flash / tray.SetIcon / 播放声音。
//
// 【并发】本类型会被多方同时调用：事件总线是同步派发的（网络读循环与传输
// worker 各自在自己的 goroutine 里 Publish），托盘闪烁计时器又在另一个
// goroutine 里调 Tick。因此内部状态一律走 mu 保护。
//
// 锁的纪律：env() / set() 这两个快照函数【绝不】在持锁时调用。组合根注入的
// 实现会读 Wails 窗口状态（内部经 InvokeSync 阻塞等主线程执行），而主线程自己
// 也会回调本类型（焦点钩子）—— 持锁调用就是一条通往死锁的路：
// 后台 goroutine 持锁等主线程，主线程等锁。
type NotifyApp struct {
	clk    ports.Clock
	env    func() NotifyEnv
	set    func() NotifySettings
	nameOf func(identity.NodeID) string

	soundThrottle time.Duration

	mu          sync.Mutex
	lastSoundAt time.Time
	soundPlayed bool

	unread   int
	flashing bool
	alertOn  bool
	// senders 是最近发来消息的人（最新的在最前，同一人只留最新一条）。
	// 悬停提示靠它回答「是谁发来的」。
	senders []hoverSender
}

// NewNotifyApp 构造决策器。
func NewNotifyApp(d NotifyDeps) *NotifyApp {
	return &NotifyApp{
		clk:           d.Clock,
		env:           d.Env,
		set:           d.Settings,
		nameOf:        d.NameOf,
		soundThrottle: defaultSoundThrottle,
	}
}

// Unread 返回 Go 侧自算的未读数（供组合根写窗口标题，也方便测试断言）。
func (n *NotifyApp) Unread() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.unread
}

// OnChatMessage 处理一条入站消息。
func (n *NotifyApp) OnChatMessage(m message.Message) []Act {
	if m.Direction != message.DirectionIn {
		return nil // 自己发的消息永不提示
	}
	e, s := n.env(), n.set() // 锁外：见类型注释里的锁纪律

	// 正看着这个会话：不打扰，也不计入未读（前端会把它标成已读）
	if e.WindowVisible && e.WindowFocused && e.ActiveConvID == m.ConvID {
		return nil
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	n.unread++
	n.rememberSenderLocked(m)
	acts := n.startFlashLocked(e)
	acts = append(acts, n.soundActsLocked(s)...)
	acts = append(acts, Act{Kind: ActWindowTitle, Text: n.titleLocked()})
	// 消息到达即主动弹「谁发来的」——不必等用户去悬停托盘
	return append(acts, Act{Kind: ActShowPopup, Text: n.hoverTextLocked()})
}

// HoverText 是鼠标悬停在托盘图标上时要显示的内容（QQ 式：谁发来的、说了什么）。
//
// 与窗口标题不同，这里是【拉取】语义：组合根在收到鼠标进入事件时现取现用，
// 不必为每条消息都推一次文本。
func (n *NotifyApp) HoverText() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.hoverTextLocked()
}

// OnTransferDone / OnTransferError：传输类提示只在窗口不可见时给。
//
// 理由：用户很可能正盯着右栏传输列表看进度，那时再响一声是纯噪声；
// 但窗口收在托盘时，传输失败现在完全无从得知 —— 那才是最该提醒的场景。
func (n *NotifyApp) OnTransferDone() []Act  { return n.onTransferEvent() }
func (n *NotifyApp) OnTransferError() []Act { return n.onTransferEvent() }

func (n *NotifyApp) onTransferEvent() []Act {
	e, s := n.env(), n.set() // 锁外
	if e.WindowVisible {
		return nil
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	n.unread++
	acts := n.startFlashLocked(e)
	acts = append(acts, n.soundActsLocked(s)...)
	return append(acts, Act{Kind: ActWindowTitle, Text: n.titleLocked()})
}

// OnWindowFocus 窗口重新获得焦点：停闪、未读归零、标题复位。
func (n *NotifyApp) OnWindowFocus() []Act {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.flashing = false
	n.alertOn = false
	n.unread = 0
	n.senders = nil
	return []Act{
		{Kind: ActFlashOff},
		{Kind: ActTrayNormal},
		{Kind: ActWindowTitle, Text: appTitleIdle},
	}
}

// OnWindowClose 回应窗口关闭请求。
//
// 只读环境与设置、不改内部状态，因此不需要加锁。
func (n *NotifyApp) OnWindowClose() []Act {
	e, s := n.env(), n.set()
	if !s.MinimizeToTray || !e.TrayOK {
		// 开关关掉，或托盘根本没建起来。后者必须退化为真正退出：
		// 否则用户会遇到「窗口消失、右下角没图标、彻底关不掉」。
		return []Act{{Kind: ActAllowQuit}}
	}
	return []Act{{Kind: ActHideToTray}}
}

// Tick 由组合根的计时器驱动（约 600ms）：窗口隐藏期间交替托盘图标来「闪」。
//
// Wails 的托盘没有闪烁 API，换图标是唯一办法；有任务栏按钮时不用它（那边用 Flash）。
func (n *NotifyApp) Tick() []Act {
	e := n.env() // 锁外：它会阻塞等主线程，持锁调用有死锁风险

	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.flashing || e.WindowVisible {
		return nil
	}
	n.alertOn = !n.alertOn
	if n.alertOn {
		return []Act{{Kind: ActTrayAlert}}
	}
	return []Act{{Kind: ActTrayNormal}}
}

// startFlashLocked 需要持有 n.mu。
func (n *NotifyApp) startFlashLocked(e NotifyEnv) []Act {
	n.flashing = true
	if e.WindowVisible {
		// 有任务栏按钮（含最小化）→ 闪按钮
		return []Act{{Kind: ActFlashOn}}
	}
	n.alertOn = true
	return []Act{{Kind: ActTrayAlert}}
}

// soundActsLocked 施加节流：连续多条消息只响一次，避免「对方连发十条 = 十声」。
// 需要持有 n.mu。
func (n *NotifyApp) soundActsLocked(s NotifySettings) []Act {
	if !s.Sound {
		return nil
	}
	now := n.clk.Now()
	if n.soundPlayed && now.Sub(n.lastSoundAt) < n.soundThrottle {
		return nil
	}
	n.lastSoundAt, n.soundPlayed = now, true
	return []Act{{Kind: ActPlaySound}}
}

// titleLocked 构造窗口标题。需要持有 n.mu。
func (n *NotifyApp) titleLocked() string {
	if n.unread <= 0 {
		return appTitleIdle
	}
	return fmt.Sprintf("%s · %d 条未读", appTitleIdle, n.unread)
}

// rememberSenderLocked 记下这条消息的发送者与内容预览。需要持有 n.mu。
//
// 同一发送者只保留最新一条并提到最前：对方连发 20 条时，
// 提示框里出现 20 行同一个人是没有意义的。
func (n *NotifyApp) rememberSenderLocked(m message.Message) {
	name := ""
	if n.nameOf != nil {
		name = strings.TrimSpace(n.nameOf(m.SenderID))
	}
	if name == "" {
		name = hoverUnknownName
	}

	next := make([]hoverSender, 0, hoverSendersKeep)
	next = append(next, hoverSender{name: name, preview: previewOf(m)})
	for _, s := range n.senders {
		if s.name == name {
			continue
		}
		if len(next) >= hoverSendersKeep {
			break
		}
		next = append(next, s)
	}
	n.senders = next
}

// hoverTextLocked 组装悬停提示的文本。需要持有 n.mu。
//
// 形如：
//
//	张三：晚上一起吃饭吗
//	李四：[图片]
//	共 3 条未读
//
// 尾行只在确实还有更多内容时才出现（只有一条时它就是噪音）。
func (n *NotifyApp) hoverTextLocked() string {
	if n.unread <= 0 {
		return appTitleIdle
	}
	// 只有传输类未读时没有发送者可列，退回标题那种「计数」文案
	if len(n.senders) == 0 {
		return n.titleLocked()
	}

	lines := make([]string, 0, hoverSendersShown+1)
	for i, s := range n.senders {
		if i >= hoverSendersShown {
			break
		}
		lines = append(lines, truncateRunes(s.name, hoverNameMax)+"："+truncateRunes(s.preview, hoverPreviewMax))
	}
	if n.unread > 1 {
		lines = append(lines, fmt.Sprintf("共 %d 条未读", n.unread))
	}
	return strings.Join(lines, "\n")
}

// previewOf 把消息压成一行可读的预览。
//
// 图片消息的 content 是内联图的 JSON 信封（含几百 KB base64）：
// 直接透出去既没用，又会撑出一个巨大的提示框，因此单独处理。
func previewOf(m message.Message) string {
	if m.MsgType == message.MsgTypeImage {
		return hoverImagePreview
	}
	first, _, _ := strings.Cut(m.Content, "\n") // 只取首行
	first = strings.TrimSpace(first)
	if first == "" {
		return hoverEmptyPreview
	}
	return truncateRunes(first, hoverPreviewMax)
}

// truncateRunes 按【字符】而非字节截断：按字节切会把中文切成半个字。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
