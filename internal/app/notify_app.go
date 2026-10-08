package app

import (
	"encoding/json"
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

	// ActSystemNotify 让组合根发一条【系统通知】（进 Windows 通知中心 /
	// macOS 通知中心 / Linux D-Bus）。
	//
	// 与 ActShowPopup 二选一，不是叠加：系统通知本身就能被看到、能进通知中心、
	// 能点击回跳，再叠一个自建气泡就是同一条消息提示两次。
	// 什么时候用哪一个由 NotifyEnv.SystemNotifyOK 决定，本层不关心平台细节。
	//
	// 载荷：Title = 发送者名字，Text = 内容预览，ConvID = 点击时要打开的会话。
	ActSystemNotify

	ActHideToTray // 关闭请求 → 隐藏窗口、保持进程
	ActAllowQuit  // 关闭请求 → 放行退出
)

// Act 是一个待执行的动作。
type Act struct {
	Kind ActKind
	Text string

	// 以下三个字段只对 ActSystemNotify 有意义（其余动作只读 Kind/Text）。
	//
	// 为什么走「值」而不是让组合根自己去查：提示内容（谁发来的、说了什么）
	// 本来就是决策层的知识，这里已经算好了；让组合根再算一遍必然算得不一致。
	Title  string // 通知标题（发送者名字）
	ConvID string // 点击通知要打开的会话
	Silent bool   // 用户关了提示音：通知也不要发声
}

// NotifyEnv 是决策所需的环境快照（窗口状态是 Wails 的事，这里只读值）。
type NotifyEnv struct {
	WindowVisible bool
	WindowFocused bool
	ActiveConvID  string
	// TrayOK 表示托盘图标是否真的建起来了：它是「关闭 = 收进托盘」的前提。
	TrayOK bool
	// SystemNotifyOK 表示系统通知（通知中心）当前可用。
	//
	// 为真时新消息走系统通知，为假时回落到自建托盘气泡。
	// 本层只读这一个 bool：为什么不可用（平台不支持、服务起不来、
	// 第一次发送就失败）是适配器的事。
	SystemNotifyOK bool
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

	// 文件消息的 content 是 {job_id, file_name} 的 JSON 信封（不是给人读的文字）。
	// 直接透出去，托盘气泡里就是一段「{"job_id":"01a0ed92-…」。
	hoverFilePreview    = "[文件]"
	hoverFileFailSuffix = "（发送失败）"
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
	// 消息到达即主动告知「谁发来的」——不必等用户去悬停托盘。
	// 用系统通知还是自建气泡，由 alertActsLocked 按可用性二选一。
	acts = append(acts, n.alertActsLocked(e, s, m)...)
	return append(acts, Act{Kind: ActWindowTitle, Text: n.titleLocked()})
}

// alertActsLocked 产出「怎么让人注意到这条消息」这部分动作。
// 需要持有 n.mu。
//
// 系统通知优先：它自带声音、进通知中心、可点击回跳；
// 不可用（平台不支持 / 服务起不来 / 发送失败）时才回落到原始的
// 「自建提示音 + 托盘气泡」组合。
//
// 两者【互斥】是刻意的：并列会出现「同一条消息，系统通知和气泡各弹一次」，
// 而系统通知本来就比气泡更强（能回看、能点击），没有保底的必要。
func (n *NotifyApp) alertActsLocked(e NotifyEnv, s NotifySettings, m message.Message) []Act {
	if e.SystemNotifyOK {
		return []Act{n.systemNotifyLocked(m, s)}
	}
	acts := n.soundActsLocked(s)
	return append(acts, Act{Kind: ActShowPopup, Text: n.hoverTextLocked()})
}

// systemNotifyLocked 组装系统通知的内容。需要持有 n.mu。
//
// 发送者与预览直接复用悬停提示那一份（rememberSenderLocked 刚写过，
// 首条就是本条消息）：两处各算一遍，迟早会出现「气泡写着张三、通知写着李四」。
func (n *NotifyApp) systemNotifyLocked(m message.Message, s NotifySettings) Act {
	name, preview := hoverUnknownName, hoverEmptyPreview
	if len(n.senders) > 0 {
		name, preview = n.senders[0].name, n.senders[0].preview
	}
	if n.unread > 1 {
		// 通知是一句话，不是消息列表；但「还有几条」必须给出来。
		// 未读总数（而不是本会话条数）与窗口标题保持一致。
		preview = fmt.Sprintf("%s（共 %d 条未读）", preview, n.unread)
	}
	return Act{
		Kind:   ActSystemNotify,
		Title:  name,
		Text:   preview,
		ConvID: m.ConvID,
		Silent: !s.Sound,
	}
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
// 图片与文件消息的 content 都是 JSON 信封（图片那个含几百 KB base64，
// 文件那个带着 job_id）：直接透出去既没用，又会把提示框变成一段裸 JSON，
// 因此按类型给一句人话。规则与前端 utils/message.ts 的 previewText 互为镜像 ——
// 只改一边，就会出现「左栏有文件名、托盘只有一段 JSON」这种不一致。
func previewOf(m message.Message) string {
	switch m.MsgType {
	case message.MsgTypeImage:
		return hoverImagePreview
	case message.MsgTypeFile, message.MsgTypeFileFailed:
		return filePreviewOf(m)
	}
	first, _, _ := strings.Cut(m.Content, "\n") // 只取首行
	first = strings.TrimSpace(first)
	if first == "" {
		return hoverEmptyPreview
	}
	return truncateRunes(first, hoverPreviewMax)
}

// filePreviewOf 把文件消息的 JSON 信封压成「[文件] 名字」。
//
// 取不到名字时只给「[文件]」：宁可少一个名字，也不能把那串 job_id 漏到提示框里。
func filePreviewOf(m message.Message) string {
	suffix := ""
	if m.MsgType == message.MsgTypeFileFailed {
		suffix = hoverFileFailSuffix
	}
	name := fileNameOf(m.Content)
	if name == "" {
		return hoverFilePreview + suffix
	}
	return hoverFilePreview + " " + name + suffix
}

// fileNameOf 取文件消息 content 里的文件名。
//
// 三种历史写法都要认（file_name / fileName / FileName）：与前端
// utils/message.ts 的容错保持一致，否则老消息会出现「气泡有名字、托盘没名字」。
func fileNameOf(content string) string {
	var v struct {
		Snake string `json:"file_name"`
		Camel string `json:"fileName"`
		Go    string `json:"FileName"`
	}
	if err := json.Unmarshal([]byte(content), &v); err != nil {
		return ""
	}
	for _, s := range []string{v.Snake, v.Camel, v.Go} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

// truncateRunes 按【字符】而非字节截断：按字节切会把中文切成半个字。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
