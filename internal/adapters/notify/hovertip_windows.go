//go:build windows

package notify

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// 为什么需要这个文件：Wails 托盘的 tooltip 在 Windows 上拿不到。
//
//	Wails 建图标时发 NIM_SETVERSION + NOTIFYICON_VERSION_4
//	（systemtray_windows.go:619），MSDN 规定该模式下标准 tooltip 被抑制；
//	Wails 虽然补了 NIF_SHOWTIP，却把 NIM_MODIFY 的失败静默吞掉（:524），
//	实测（Windows 10 19045）悬停毫无反应。
//
// 因此我们自己建一个 tooltip：用系统的 tooltips_class32 控件而不是自绘窗口，
// 圆角、阴影、DPI 缩放、字体都由系统负责，我们只决定「放什么字、放在哪」。
//
// # 线程模型（这个文件里最容易踩死的坑）
//
// Win32 的窗口属于【创建它的那个线程】：向另一个线程的窗口 SendMessage 时，
// 对方必须在泵消息，否则调用会永久阻塞。而 Go 的 goroutine 会在 OS 线程之间
// 迁移（实测：同一个 goroutine 里先 CreateWindowEx、再 SendMessage，就卡死了）。
//
// 所以本类型自己拥有一条专用线程：runtime.LockOSThread 锁定 + 自己的消息泵，
// 所有窗口操作都在那条线程上执行。外部调用者（托盘回调、菜单、任意 goroutine）
// 只需投递命令并等待事件唤醒，不必关心线程问题。
type HoverTip struct {
	cmds chan hoverCmd
	done chan struct{}
	once sync.Once
	// wake 是一个自动重置事件：投递命令后唤醒泵线程。
	wake uintptr

	mu           sync.Mutex
	lastX, lastY int
	lastW, lastH int
}

// LastSize 返回最近一次成功显示时提示框的像素尺寸（尚未显示或建不出来时为 0,0）。
//
// 它存在的唯一理由是【测试】：这个提示框曾经因为多设了一个 TTM_SETMAXTIPWIDTH
// 而退化成 6×4 的一条缝 —— 所有 Win32 调用都返回成功、窗口 `visible=1`，
// 界面上却什么都看不到。只断言「没报错」的测试抓不住它，必须断言尺寸。
func (t *HoverTip) LastSize() (int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastW, t.lastH
}

// LastRect 返回最近一次成功显示时提示框的屏幕矩形（left, top, right, bottom）。
//
// 与 LastSize 一样只为测试存在：主动弹出的气泡位置是我们自己算的
// （通知区域上方 + 夹取进屏幕），算错就会出现「消息来了，气泡在屏幕外」，
// 同样是「什么都没报错但用户什么都看不到」。
func (t *HoverTip) LastRect() (int, int, int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastX, t.lastY, t.lastX + t.lastW, t.lastY + t.lastH
}

// screenSize 返回主显示器尺寸（测试里用来断言气泡没有越界）。
func screenSize() (int, int) {
	sw, _, _ := procGetSystemMetric.Call(smCxScreen)
	sh, _, _ := procGetSystemMetric.Call(smCyScreen)
	return int(sw), int(sh)
}

type hoverCmd struct {
	text string
	// nearTray 为真时按「通知区域上方」摆放（主动弹出的新消息气泡）；
	// 为假时按光标位置摆放（鼠标悬停）。
	nearTray bool
	hide     bool
	close    bool
	reply    chan error
}

var (
	hoverUser32   = syscall.NewLazyDLL("user32.dll")
	hoverKernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreateWindowExW    = hoverUser32.NewProc("CreateWindowExW")
	procDestroyWindow      = hoverUser32.NewProc("DestroyWindow")
	procSendMessageW       = hoverUser32.NewProc("SendMessageW")
	procGetCursorPos       = hoverUser32.NewProc("GetCursorPos")
	procGetModuleHandleW   = hoverKernel32.NewProc("GetModuleHandleW")
	procSetWindowPos       = hoverUser32.NewProc("SetWindowPos")
	procGetDesktopWindow   = hoverUser32.NewProc("GetDesktopWindow")
	procPeekMessageW       = hoverUser32.NewProc("PeekMessageW")
	procTranslateMessage   = hoverUser32.NewProc("TranslateMessage")
	procDispatchMessageW   = hoverUser32.NewProc("DispatchMessageW")
	procMsgWaitForMultiple = hoverUser32.NewProc("MsgWaitForMultipleObjects")
	procCreateEventW       = hoverKernel32.NewProc("CreateEventW")
	procSetEvent           = hoverKernel32.NewProc("SetEvent")
	procCloseHandle        = hoverKernel32.NewProc("CloseHandle")

	// 用于记录显示后的真实尺寸（见 HoverTip.LastSize 的说明）
	procGetWindowRect = hoverUser32.NewProc("GetWindowRect")
	// 用于给「主动弹出」找通知区域（托盘图标区）的位置
	procFindWindowW     = hoverUser32.NewProc("FindWindowW")
	procFindWindowExW   = hoverUser32.NewProc("FindWindowExW")
	procGetSystemMetric = hoverUser32.NewProc("GetSystemMetrics")
)

const (
	hoverWsPopup       = 0x80000000
	hoverWsExTopmost   = 0x00000008
	hoverTtsAlwaysTip  = 0x0001
	hoverTtsNoPrefix   = 0x0002
	hoverTtfTrack      = 0x0020
	hoverTtfAbsolute   = 0x0080
	hoverSwpNoSize     = 0x0001
	hoverSwpNoMove     = 0x0002
	hoverSwpNoActivate = 0x0010
	hoverHwndTopmost   = ^uintptr(0) // (HWND)-1

	// hoverTextCap 是内部 UTF-16 缓冲的容量。
	// 缓冲【复用且指针不变】：tooltip 控件保存的是 lpszText 指针而不是字符串副本，
	// 每次换文字都重新分配的话，系统手上会留下悬空指针。
	hoverTextCap = 512

	// 提示框相对光标的偏移：向左上角让开。
	//
	// 【这不是审美问题，是能不能显示的问题】托盘悬停状态下，如果提示框盖住了
	// 光标（也就盖住了托盘图标），系统会立刻判定「鼠标离开了图标」并发来
	// NIN_POPUPCLOSE —— 实测应用日志里 ENTER 之后 29ms 就是 LEAVE，
	// 表现就是「压根看不到提示框」。
	// 向上让出 110px：图标在任务栏底部，提示框落在图标正上方（与系统自带
	// 托盘提示的位置一致），既不遮光标也不遮图标。
	hoverOffsetX = 20
	hoverOffsetY = 110

	hoverPmRemove   = 0x0001
	hoverQsAllInput = 0x04FF
	hoverInfinite   = 0xFFFFFFFF

	// 主动弹出（新消息气泡）时的摆放参数
	smCxScreen = 0
	smCyScreen = 1
	// popupGapY 是气泡与通知区域之间的留白：太低会盖住任务栏，太高显得没关联
	popupGapY = 120
	// popupMaxW 是估算的提示框宽度，仅用于夹取摆放位置（显示后会按实测尺寸再夹一次）
	popupMaxW  = 360
	screenEdge = 8

	// 与 tooltip 控件通信的消息号（WM_USER = 0x0400）。
	// 注意 ANSI 版与 Unicode 版是两个不同的号（ADDTOOLA=+4 / ADDTOOLW=+50），
	// 抄错一个数字的后果是「控件沉默地拒绝你」，没有任何错误码可看。
	//
	// 这里【刻意不包含】TTM_SETMAXTIPWIDTH(+24)：它会让提示框塌成 6×4，
	// 原因见 hoverWindow.ensure 里的注释。
	ttmAddToolW       = 0x0400 + 50
	ttmTrackActivate  = 0x0400 + 17
	ttmTrackPosition  = 0x0400 + 18
	ttmPop            = 0x0400 + 28
	ttmUpdateTipTextW = 0x0400 + 57
)

// errNoDesktop 表示当前会话建不出窗口（无交互式桌面）。
//
// 单独定义是为了让测试能区分「环境不允许」与「控件拒绝了我们的参数」：
// 后者是真 bug，不该被一句 t.Skip 掩盖。
var errNoDesktop = errors.New("notify: 建不出 tooltip 窗口（当前会话可能没有交互式桌面）")

type hoverPoint struct {
	X, Y int32
}

type hoverRect struct {
	Left, Top, Right, Bottom int32
}

// toolInfoW 对应 Win32 的 TOOLINFOW。
//
// 字段顺序与宽度必须与 C 结构完全一致（Go 的对齐规则在 32/64 位下都与 C 相符），
// cbSize 由 unsafe.Sizeof 算出，因此不能随意增删字段。
//
// 【注意】不要凭印象给它加回 lpReserved：x64 下加上它就是 72，而控件只接受 64 ——
// 实测 cbSize=72 时 TTM_ADDTOOLW 返回 0（拒绝），64 时返回 1。
// 控件对 cbSize 的校验是「认识的尺寸才收」，且失败时不给任何错误码，
// 只表现为「悬停毫无反应」。
type toolInfoW struct {
	CbSize    uint32
	UFlags    uint32
	Hwnd      uintptr
	UId       uintptr
	Rect      hoverRect
	HInstance uintptr
	LpszText  *uint16
	LParam    uintptr
}

// msgW 对应 Win32 的 MSG。我们只用它转交给 DispatchMessage，
// 字段布局与 C 保持一致（x64 下 48 字节）。
type msgW struct {
	Hwnd    uintptr
	Message uint32
	_       uint32 // x64 对齐填充
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      hoverPoint
}

// NewHoverTip 构造悬停提示器，并启动它的专用线程。
func NewHoverTip() *HoverTip {
	t := &HoverTip{
		cmds: make(chan hoverCmd, 8),
		done: make(chan struct{}),
	}
	// 自动重置事件：投递命令后用它把泵线程从 MsgWaitForMultipleObjects 唤醒
	ev, _, _ := procCreateEventW.Call(0, 0, 0, 0)
	t.wake = ev
	go t.loop()
	return t
}

// Show 在鼠标当前位置弹出提示框（鼠标悬停用），并等着它真的弹出来。
func (t *HoverTip) Show(text string) error { return t.show(hoverCmd{text: text}) }

// ShowNearNotificationArea 把提示框弹在通知区域（托盘图标区）正上方，
// 用于【收到消息时主动弹出】—— 此时鼠标可能在屏幕任何地方，
// 因此不能像悬停那样依赖光标位置，必须自己找托盘。
func (t *HoverTip) ShowNearNotificationArea(text string) error {
	return t.show(hoverCmd{text: text, nearTray: true})
}

func (t *HoverTip) show(cmd hoverCmd) error {
	reply := make(chan error, 1)
	cmd.reply = reply
	select {
	case t.cmds <- cmd:
		t.signal()
	case <-t.done:
		return nil // 已关闭：退出路径上静默忽略
	}
	select {
	case err := <-reply:
		return err
	case <-t.done:
		return nil
	}
}

// Hide 收起提示框。故意不等回复：鼠标离开的事件回调不该被阻塞。
func (t *HoverTip) Hide() {
	select {
	case t.cmds <- hoverCmd{hide: true}:
		t.signal()
	case <-t.done:
	default:
		// 队列满（连续快速进出）：丢掉这次 hide 无伤大雅，
		// 下一次 hide / 后续 show 会纠正状态
	}
}

// Close 关闭专用线程并销毁窗口。可重复调用。
func (t *HoverTip) Close() {
	t.once.Do(func() {
		close(t.done)
		reply := make(chan error, 1)
		select {
		case t.cmds <- hoverCmd{close: true, reply: reply}:
			t.signal()
			<-reply
		default:
		}
		if t.wake != 0 {
			procCloseHandle.Call(t.wake)
			t.wake = 0
		}
	})
}

func (t *HoverTip) signal() {
	if t.wake != 0 {
		procSetEvent.Call(t.wake)
	}
}

// loop 是专用线程：锁定 OS 线程、泵消息、执行窗口操作。
func (t *HoverTip) loop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	w := hoverWindow{text: make([]uint16, hoverTextCap)}
	defer w.destroy()

	var m msgW
	for {
		// 等「事件被唤醒」或「本线程的窗口消息到达」。
		// 这里必须真的泵消息：tooltip 控件的绘制与内部计时都靠它。
		if t.wake != 0 {
			procMsgWaitForMultiple.Call(
				1, uintptr(unsafe.Pointer(&t.wake)), 0, hoverInfinite, hoverQsAllInput)
		} else {
			time.Sleep(5 * time.Millisecond)
		}

		for {
			ok, _, _ := procPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, hoverPmRemove)
			if ok == 0 {
				break
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}

		drained := false
		for !drained {
			select {
			case cmd := <-t.cmds:
				if cmd.close {
					// destroy 由 defer 负责，这里只需应答
					if cmd.reply != nil {
						cmd.reply <- nil
					}
					return
				}
				var err error
				if cmd.hide {
					w.hide()
				} else {
					err = w.show(cmd.text, cmd.nearTray)
					t.mu.Lock()
					t.lastX, t.lastY = w.sizeX, w.sizeY
					t.lastW, t.lastH = w.sizeW, w.sizeH
					t.mu.Unlock()
				}
				if cmd.reply != nil {
					cmd.reply <- err
				}
			default:
				drained = true
			}
		}
	}
}

// hoverWindow 只能由 loop 所在线程触碰（零值不可用）。
type hoverWindow struct {
	hwnd   uintptr
	text   []uint16
	info   toolInfoW
	added  bool
	failed error // 首次失败后不再重试，避免每次悬停都白折腾

	// 最近一次显示的位置与尺寸（供 HoverTip.LastSize / LastRect 暴露给测试）
	sizeX, sizeY int
	sizeW, sizeH int
}

func (w *hoverWindow) show(text string, nearTray bool) error {
	if w.failed != nil {
		return w.failed
	}
	if err := w.ensure(); err != nil {
		w.failed = err
		return err
	}

	w.writeText(text)
	if !w.added {
		if ret, _, _ := procSendMessageW.Call(w.hwnd, ttmAddToolW, 0, uintptr(unsafe.Pointer(&w.info))); ret == 0 {
			w.failed = errors.New("notify: TTM_ADDTOOLW 被工具提示控件拒绝（cbSize 或消息号不对）")
			return w.failed
		}
		w.added = true
	} else {
		procSendMessageW.Call(w.hwnd, ttmUpdateTipTextW, 0, uintptr(unsafe.Pointer(&w.info)))
	}

	anchor := w.anchor(nearTray)
	// MAKELPARAM：低 16 位 X、高 16 位 Y（TTF_ABSOLUTE 下按屏幕坐标解释）
	procSendMessageW.Call(w.hwnd, ttmTrackPosition, 0, packPoint(anchor))

	if ret, _, _ := procSendMessageW.Call(w.hwnd, ttmTrackActivate, 1, uintptr(unsafe.Pointer(&w.info))); ret == 0 {
		w.failed = errors.New("notify: TTM_TRACKACTIVATE 失败")
		return w.failed
	}

	// 记下真实尺寸：调用全部成功但窗口塌成几个像素，是这套 API 最阴的失败方式
	w.measure()
	w.clampOnScreen()
	return nil
}

// anchor 计算提示框左上角的落点。
func (w *hoverWindow) anchor(nearTray bool) hoverPoint {
	if nearTray {
		return trayAnchor()
	}
	var pt hoverPoint
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// 向左上偏移，让提示框别盖住光标/托盘图标（见 hoverOffsetX/Y 的说明）
	x, y := pt.X-hoverOffsetX, pt.Y-hoverOffsetY
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	return hoverPoint{X: x, Y: y}
}

// measure 读取窗口的真实位置与尺寸。
func (w *hoverWindow) measure() {
	var r hoverRect
	procGetWindowRect.Call(w.hwnd, uintptr(unsafe.Pointer(&r)))
	w.sizeX, w.sizeY = int(r.Left), int(r.Top)
	w.sizeW, w.sizeH = int(r.Right-r.Left), int(r.Bottom-r.Top)
}

// clampOnScreen 把提示框整体拉回屏幕内。
//
// 工具提示控件自己也会做调整，但它的策略（往哪边翻）不可控；主动弹出的气泡
// 出现在屏幕右下角，一旦越界就可能被翻到任务栏下面去。这里按实测尺寸显式夹一次。
func (w *hoverWindow) clampOnScreen() {
	if w.sizeW <= 0 || w.sizeH <= 0 {
		return
	}
	var r hoverRect
	procGetWindowRect.Call(w.hwnd, uintptr(unsafe.Pointer(&r)))
	l, t := int(r.Left), int(r.Top)
	nl, nt := l, t

	sw, _, _ := procGetSystemMetric.Call(smCxScreen)
	sh, _, _ := procGetSystemMetric.Call(smCyScreen)
	if nl+w.sizeW > int(sw)-screenEdge {
		nl = int(sw) - screenEdge - w.sizeW
	}
	if nl < screenEdge {
		nl = screenEdge
	}
	if nt+w.sizeH > int(sh)-screenEdge {
		nt = int(sh) - screenEdge - w.sizeH
	}
	if nt < screenEdge {
		nt = screenEdge
	}
	if nl == l && nt == t {
		return
	}
	procSendMessageW.Call(w.hwnd, ttmTrackPosition, 0,
		packPoint(hoverPoint{X: int32(nl), Y: int32(nt)}))
	w.measure()
}

func packPoint(p hoverPoint) uintptr {
	return uintptr(uint16(p.X)) | uintptr(uint16(p.Y))<<16
}

// trayAnchor 计算「主动弹出」的落点：通知区域（托盘图标区）正上方。
//
// 悬停时可以直接用光标位置，但主动弹气泡时鼠标可能在屏幕任何地方，
// 因此必须自己找托盘：Shell_TrayWnd → TrayNotifyWnd。找不到（非标准 shell）
// 就退到主屏右下角，宁可位置差一点也别不弹。
func trayAnchor() hoverPoint {
	sw, _, _ := procGetSystemMetric.Call(smCxScreen)
	sh, _, _ := procGetSystemMetric.Call(smCyScreen)

	trayCls, _ := syscall.UTF16PtrFromString("Shell_TrayWnd")
	notifyCls, _ := syscall.UTF16PtrFromString("TrayNotifyWnd")
	tray, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(trayCls)))
	var r hoverRect
	found := false
	if tray != 0 {
		if notify, _, _ := procFindWindowExW.Call(tray, 0, uintptr(unsafe.Pointer(notifyCls)), 0); notify != 0 {
			if ok, _, _ := procGetWindowRect.Call(notify, uintptr(unsafe.Pointer(&r))); ok != 0 {
				found = true
			}
		}
	}
	if !found {
		return hoverPoint{X: int32(sw) - popupMaxW - screenEdge, Y: int32(sh) - popupGapY - 60}
	}

	x := int(r.Left)
	y := int(r.Top) - popupGapY
	// 任务栏在屏幕上方时，通知区域上边没有空间 → 改放到它下方
	if y < screenEdge {
		y = int(r.Bottom) + screenEdge
	}
	if x+popupMaxW > int(sw)-screenEdge {
		x = int(sw) - screenEdge - popupMaxW
	}
	if x < screenEdge {
		x = screenEdge
	}
	return hoverPoint{X: int32(x), Y: int32(y)}
}

func (w *hoverWindow) hide() {
	if w.hwnd == 0 || !w.added {
		return
	}
	procSendMessageW.Call(w.hwnd, ttmTrackActivate, 0, uintptr(unsafe.Pointer(&w.info)))
	procSendMessageW.Call(w.hwnd, ttmPop, 0, 0)
}

func (w *hoverWindow) destroy() {
	if w.hwnd == 0 {
		return
	}
	procDestroyWindow.Call(w.hwnd)
	w.hwnd = 0
	w.added = false
}

// ensure 延迟创建窗口并填好 TOOLINFO。
func (w *hoverWindow) ensure() error {
	if w.hwnd != 0 {
		return nil
	}

	cls, err := syscall.UTF16PtrFromString("tooltips_class32")
	if err != nil {
		return err
	}
	hInst, _, _ := procGetModuleHandleW.Call(0)
	// TTF_TRACK 的 tooltip 不绑定到某个控件，由我们主动 track 到鼠标位置；
	// hwnd/uId 仍需给出一个合法宿主（用桌面窗口），否则 TTM_ADDTOOLW 会失败。
	desktop, _, _ := procGetDesktopWindow.Call()

	hwnd, _, callErr := procCreateWindowExW.Call(
		hoverWsExTopmost,
		uintptr(unsafe.Pointer(cls)),
		0,
		hoverWsPopup|hoverTtsAlwaysTip|hoverTtsNoPrefix,
		0, 0, 0, 0,
		desktop,
		0,
		hInst,
		0,
	)
	if hwnd == 0 {
		return fmt.Errorf("%w: %v", errNoDesktop, callErr)
	}

	w.hwnd = hwnd
	// 置顶且不抢焦点：提示框绝不能把用户的输入焦点弄丢
	procSetWindowPos.Call(hwnd, hoverHwndTopmost, 0, 0, 0, 0,
		hoverSwpNoMove|hoverSwpNoSize|hoverSwpNoActivate)

	// 【绝不能加 TTM_SETMAXTIPWIDTH】—— 这一行会让提示框退化成 6×4 的一条缝。
	//
	// 实测（同一段代码，只改这一项）：
	//   设了 maxwidth=320 → 激活后窗口 6×4，文本溢出边框，肉眼看不到提示框；
	//   不设            → 激活后窗口 189×21，正常按文本撑开。
	// 原因是 maxwidth 把控件切进「自动换行」模式，而换行宽度取自 tool 的矩形，
	// 跟踪型 tool（TTF_TRACK）的矩形是退化的，于是整个框塌成最小值。
	// 我们不依赖它换行：文案在决策层已经按字符截断（名字 12 / 内容 30）。

	w.info = toolInfoW{
		CbSize:    uint32(unsafe.Sizeof(toolInfoW{})),
		UFlags:    hoverTtfTrack | hoverTtfAbsolute,
		Hwnd:      desktop,
		UId:       0,
		HInstance: hInst,
		LpszText:  &w.text[0],
	}
	return nil
}

// writeText 把文本写进固定缓冲。
//
// 换行统一成 CRLF：工具提示控件按 CRLF 断行，只给 \n 的话多行文案会挤成一行。
func (w *hoverWindow) writeText(text string) {
	n := 0
	put := func(u uint16) {
		if n >= len(w.text)-1 {
			return
		}
		w.text[n] = u
		n++
	}

	for _, r := range text {
		switch {
		case r == '\n':
			put('\r')
			put('\n')
		case r < 0x10000:
			put(uint16(r))
		default: // 代理对（emoji 等）
			r -= 0x10000
			put(uint16(0xD800 + (r >> 10)))
			put(uint16(0xDC00 + (r & 0x3FF)))
		}
	}
	w.text[n] = 0
}
