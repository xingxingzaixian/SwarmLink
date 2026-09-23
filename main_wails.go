//go:build wails

// SwarmLink 桌面外壳 —— 组合根。
//
// 这是【唯一】接触 Wails API 的文件：五个服务与事件桥都在
// internal/adapters/wails 里以纯 Go 实现，因此 Wails beta 期的 API 波动
// 只需要在这里适配一次（架构书 7.5：锁定 v3.0.0-beta.24，升级作为独立任务）。
//
// 构建步骤（在仓库根目录执行）：
//
//	wails3 generate bindings -f "-tags wails" -clean .
//	wails3 build -tags wails          # 产物：bin/SwarmLink
//
// 为什么入口在根目录：wails3 的构建任务在仓库根执行 `go build`（不带包路径），
// 且架构书本来就把 `main.go` 定义为唯一组合根。
package main

import (
	"context"
	"embed"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/swarmlink/swarmlink/internal/adapters/config"
	"github.com/swarmlink/swarmlink/internal/adapters/notify"
	wadapter "github.com/swarmlink/swarmlink/internal/adapters/wails"
	// capp 是 internal/app 的别名：本文件里 `app` 这个名字已经被 Wails 的
	// application.App 占用了，直接 import 成 app 会被局部变量遮蔽。
	capp "github.com/swarmlink/swarmlink/internal/app"
	"github.com/swarmlink/swarmlink/internal/bootstrap"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// 前端产物必须【嵌入二进制】：wails3 的资产服务器从 embed.FS 读取。
// 因此构建前 frontend/dist 必须已存在（Taskfile 的 common:build:frontend 会先跑 npm run build）。
//
//go:embed all:frontend/dist
var assets embed.FS

// wailsEmitter 把 Wails 的 Event.Emit 适配成 internal/adapters/wails.Emitter。
//
// 若你的 beta 版本签名不同（例如 Emit 是可变参），只需要改这一个方法 ——
// 这正是把 API 接触面收敛到本文件的目的。
type wailsEmitter struct {
	app *application.App
}

func (e wailsEmitter) Emit(name string, data any) {
	e.app.Event.Emit(name, data)
}

func main() {
	// 1. 先起后端：网络/存储起不来时，GUI 必须明确报错而不是静默半死。
	node, err := bootstrap.Start(context.Background(), bootstrap.Options{Level: slog.LevelInfo})
	if err != nil {
		slog.Error("后端启动失败", "err", err)
		os.Exit(1)
	}
	defer node.Close()

	// 「用户正在看哪个会话」由前端经 NotifyService 上报，供提示决策判断是否打扰。
	// 只有这一项需要前端告诉 Go —— 窗口的可见性与焦点由 Go 侧直接读 Wails。
	var (
		activeConvMu sync.RWMutex
		activeConvID string
	)
	setActiveConv := func(id string) {
		activeConvMu.Lock()
		activeConvID = id
		activeConvMu.Unlock()
	}
	readActiveConv := func() string {
		activeConvMu.RLock()
		defer activeConvMu.RUnlock()
		return activeConvID
	}

	// 2. 构造服务（纯 Go，不依赖 Wails）
	services := wadapter.NewServices(wadapter.Deps{
		Self: wadapter.SelfInfo{
			NodeID:      node.KP.NodeID(),
			DisplayName: node.Cfg.General.DisplayName,
			TCPPort:     node.TCPPort,
			UDPPort:     node.UDPPort,
			Subnet:      node.SelfSubnet,
		},
		Chat:         node.ChatApp,
		Transfer:     node.TransferApp,
		Group:        node.GroupApp,
		Peers:        node.PeerApp,
		Discovery:    node.DiscApp,
		PeerDir:      node.PeerDir,
		TransferRepo: node.Transfers,
		GroupRepo:    node.Groups,
		Conns:        node.Conn,
		Seeds:        node.Registry,
		Interfaces:   func() []string { return node.Ifaces },
		SeedSnapshot: func() []wadapter.SeedDTO {
			snap := node.Registry.Snapshot()
			out := make([]wadapter.SeedDTO, 0, len(snap))
			for _, s := range snap {
				out = append(out, wadapter.SeedDTO{
					Addr:      s.Addr.String(),
					FailCount: s.FailCnt,
					NodeID:    s.NodeID,
					TCPPort:   int(s.TCPPort),
					Subnet:    s.Subnet,
				})
			}
			return out
		},
		SetActiveConv: setActiveConv,
		Cfg:           node.Cfg,
		ConfigDir:     node.ConfigDir,
	})

	// 3. 创建应用：服务在 Options 里注册，Wails 会静态分析并生成 TypeScript 绑定。
	app := application.New(application.Options{
		Name:        "SwarmLink",
		Description: "无中心内网 P2P 聊天与文件分享工具",
		Services: []application.Service{
			application.NewService(services.Settings),
			application.NewService(services.Chat),
			application.NewService(services.Transfer),
			application.NewService(services.Peer),
			application.NewService(services.Group),
			application.NewService(services.Notify),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	// 4. 领域事件 → 前端事件（含按 job 的 4 Hz 进度节流与调试面板馈送）
	bridge := wadapter.NewBridge(node.Bus, node.Clk, wailsEmitter{app: app})
	bridge.Start()
	defer bridge.Stop()

	// 5. 主窗口
	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "SwarmLink",
		Width:  1180,
		Height: 760,
		// 允许把文件拖进窗口来发送。
		//
		// 为什么必须开这个开关并把路径转发出去：WebView 里 drop 出来的 File
		// 对象【没有本地路径】（浏览器安全模型），只有窗口层能拿到真实路径。
		// 因此拖放发文件这条链路注定要过一次 Go，不可能纯前端实现。
		EnableFileDrop: true,
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 40,
			Backdrop:                application.MacBackdropTranslucent,
			TitleBar:                application.MacTitleBarHiddenInset,
		},
		// 窗口底色要和前端的氛围层兜底色一致（#0F0723）：
		// 不一致的话，调整窗口尺寸时会闪出一道白边
		BackgroundColour: application.NewRGB(15, 7, 35),
		URL:              "/",
	})

	// 拖入文件的路径 → 前端。落在哪个区域由前端的 data-file-drop-target 决定，
	// 这里只做搬运，不做业务判断（发给谁、能不能发，属于 UI 的决策）。
	win.OnWindowEvent(events.Common.WindowFilesDropped, func(e *application.WindowEvent) {
		paths := e.Context().DroppedFiles()
		if len(paths) == 0 {
			return
		}
		target := ""
		if d := e.Context().DropTargetDetails(); d != nil {
			target = d.Attributes["data-file-drop-target"]
		}
		app.Event.Emit(wadapter.EventFilesDropped, map[string]any{
			"paths":  paths,
			"target": target,
		})
	})

	// ------------------------------------------------------------ 托盘与提示
	//
	// 「什么时候提示、提示成什么样」全部由 capp.NotifyApp 决策（零 Wails 依赖、
	// 有表驱动单测）；这里只做三件事：提供环境快照、把动作翻译成 Wails 调用、
	// 接上事件源。适配器之间不互相引用，接线只在组合根发生。
	var trayOK atomic.Bool
	// 悬停提示失败只报一次日志（见 showTip）
	var tipWarn sync.Once
	// 收起提示框的延迟计时器（见 OnMouseLeave 的说明）
	var hideTimer *time.Timer

	envSnapshot := func() capp.NotifyEnv {
		return capp.NotifyEnv{
			WindowVisible: win.IsVisible(),
			WindowFocused: win.IsFocused(),
			ActiveConvID:  readActiveConv(),
			TrayOK:        trayOK.Load(),
		}
	}
	settingsSnapshot := func() capp.NotifySettings {
		// 实时读配置而不是拷贝一份：设置页与托盘菜单改的都是同一个
		// *config.Config，因此改完立刻生效、两个入口也不会状态不一致。
		return capp.NotifySettings{
			Sound:          node.Cfg.Notify.Sound,
			MinimizeToTray: node.Cfg.Notify.MinimizeToTray,
		}
	}
	// 悬停提示要靠「谁发来的」，而只有节点目录知道这件事。
	// 名字查不到时返回空串，决策器会回落到「未知用户」。
	nameOf := func(id identity.NodeID) string {
		p, ok := node.PeerDir.Get(id)
		if !ok {
			return ""
		}
		return p.DisplayName
	}
	notifier := capp.NewNotifyApp(capp.NotifyDeps{
		Clock:    node.Clk,
		Env:      envSnapshot,
		Settings: settingsSnapshot,
		NameOf:   nameOf,
	})

	// 托盘：SetIcon/SetMenu 在 app.Run() 之前是「缓存」语义
	// （Wails 的 impl 尚未创建时只存值），Run 时会一并应用，因此顺序没问题。
	tray := app.SystemTray.New()
	tray.SetIcon(notify.TrayIcon())

	// 【必须】给托盘图标设一个非空 tooltip，哪怕它本身并不显示。
	//
	// 实测：Windows 下 Wails 走 NOTIFYICON_VERSION_4，标准 tooltip 被抑制
	// （MSDN: "the standard tooltip is suppressed"），所以这行字你在界面上看不到。
	// 但它决定了系统【发不发】悬停事件：用自建探针（NIM_ADD 时带 NIF_TIP）
	// 悬停时收到了 NIN_POPUPOPEN；而本应用的图标在不设 tooltip 时收不到 ——
	// 没有 szTip 就没有「本该显示的提示」可被替代，系统也就没必要通知应用。
	// 去掉这一行，托盘悬停提示会静默失效（表现为「只闪烁，不弹提示」）。
	tray.SetTooltip("SwarmLink")

	showWindow := func() {
		win.Show()
		win.Restore()
		win.Focus()
	}

	// 提示气泡：QQ 式「谁发来的、说了什么」。
	//
	// 为什么不用 tray.SetTooltip：Windows 下 Wails 走 NOTIFYICON_VERSION_4，
	// 该模式会抑制标准 tooltip（实测悬停毫无反应）。这里用自建的原生提示框，
	// 两种触发方式共用它：
	//   - 鼠标悬停在图标上（NIN_POPUPOPEN / NIN_POPUPCLOSE，Wails 转发给
	//     OnMouseEnter / OnMouseLeave）→ 按光标位置弹；
	//   - 收到新消息主动弹 → 按通知区域位置弹（此时鼠标可能在屏幕任何地方）。
	tip := notify.NewHoverTip()
	defer tip.Close()

	// resetHideTimer 让「收起提示」这件事只有一个来源，避免两处计时器互相打断。
	resetHideTimer := func(d time.Duration) {
		if hideTimer != nil {
			hideTimer.Stop()
		}
		hideTimer = time.AfterFunc(d, tip.Hide)
	}
	stopHideTimer := func() {
		if hideTimer != nil {
			hideTimer.Stop()
			hideTimer = nil
		}
	}
	reportTipErr := func(err error) {
		// 只报一次：提示框建不起来（无桌面）时会每次触发都失败，
		// 逐次打日志只会把日志刷满。
		tipWarn.Do(func() { slog.Warn("显示提示气泡失败", "err", err) })
	}

	showTip := func() { // 悬停：跟着光标
		stopHideTimer()
		if err := tip.Show(notifier.HoverText()); err != nil {
			reportTipErr(err)
		}
	}
	showPopup := func(text string) { // 新消息：弹在托盘上方，几秒后自动消失
		stopHideTimer()
		if err := tip.ShowNearNotificationArea(text); err != nil {
			reportTipErr(err)
			return
		}
		resetHideTimer(popupLinger)
	}

	// 鼠标离开时不立刻收起，而是延迟一点 —— 这不是为了动画好看。
	//
	// 托盘悬停有个恼人的时序：提示框弹出的瞬间，系统偶尔会先送来一个
	// NIN_POPUPCLOSE（光标从图标「移到」了新出现的提示框上）。若立刻收起，
	// 用户就永远看不到提示框。延迟一点，期间若又收到 ENTER 就取消收起。
	tray.OnMouseEnter(showTip)
	tray.OnMouseLeave(func() { resetHideTimer(hoverLinger) })

	runActs := func(acts []capp.Act) {
		for _, a := range acts {
			switch a.Kind {
			case capp.ActPlaySound:
				notify.Play()
			case capp.ActFlashOn:
				win.Flash(true)
			case capp.ActFlashOff:
				win.Flash(false)
			case capp.ActTrayAlert:
				tray.SetIcon(notify.TrayAlertIcon())
			case capp.ActTrayNormal:
				tray.SetIcon(notify.TrayIcon())
			case capp.ActWindowTitle:
				win.SetTitle(a.Text)
			case capp.ActShowPopup:
				showPopup(a.Text)
			}
			// ActHideToTray / ActAllowQuit 由关闭钩子消费，这里不处理。
		}
	}

	// 托盘菜单与设置页共用同一份配置：改完立即落盘并通知前端，
	// 因此从哪个入口改都不会出现「设置页显示已关、实际还在响」。
	setNotifyOption := func(opt notifyOption, on bool) {
		switch opt {
		case notifyOptSound:
			node.Cfg.Notify.Sound = on
		case notifyOptTray:
			node.Cfg.Notify.MinimizeToTray = on
		}
		if err := config.Save(node.ConfigDir, node.Cfg); err != nil {
			slog.Warn("保存提示设置失败", "err", err)
		}
		// 直接发前端事件，不走事件总线：TopicConfigChanged 在总线里只有常量、
		// 事件桥并没有订阅它，发到总线上不会有任何人转发给前端。
		app.Event.Emit(wadapter.EventConfigChanged, nil)
	}

	menu := app.NewMenu()
	menu.Add("显示主窗口").OnClick(func(*application.Context) { showWindow() })
	// checkbox 的 checked 状态由 Wails 在回调【之前】翻转，
	// 因此这里读到的是用户操作后的新值。
	soundItem := menu.AddCheckbox("新消息提示音", true).SetChecked(node.Cfg.Notify.Sound)
	soundItem.OnClick(func(*application.Context) {
		setNotifyOption(notifyOptSound, soundItem.Checked())
	})
	trayItem := menu.AddCheckbox("关闭时最小化到托盘", true).SetChecked(node.Cfg.Notify.MinimizeToTray)
	trayItem.OnClick(func(*application.Context) {
		setNotifyOption(notifyOptTray, trayItem.Checked())
	})
	menu.AddSeparator()
	menu.Add("退出").OnClick(func(*application.Context) { app.Quit() })
	tray.SetMenu(menu)
	tray.OnClick(showWindow)
	trayOK.Store(true)

	// 关闭按钮：默认收进托盘而不是退出。
	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		for _, a := range notifier.OnWindowClose() {
			if a.Kind == capp.ActHideToTray {
				e.Cancel()
				win.Hide()
				// 「我还在这儿」：刚隐藏时闪几下托盘图标。
				// 否则用户看到窗口消失会以为程序退出了 —— 托盘里多出一个小图标
				// 这件事，很多人根本不会注意到。
				go blinkTray(tray)
				return
			}
		}
		// 走到这里即 ActAllowQuit：不取消，正常退出
	})

	// 焦点回到窗口：停闪、未读归零、托盘图标复位。
	win.RegisterHook(events.Common.WindowFocus, func(*application.WindowEvent) {
		runActs(notifier.OnWindowFocus())
	})

	// 事件源：新消息要提示；传输类只在窗口不可见时提示（用户可能正盯着
	// 传输列表看进度，那时再响一声是纯噪声）。
	node.Bus.Subscribe(eventbus.TopicChatReceived, func(p any) {
		if m, ok := p.(message.Message); ok {
			runActs(notifier.OnChatMessage(m))
		}
	})
	node.Bus.Subscribe(eventbus.TopicTransferDone, func(any) { runActs(notifier.OnTransferDone()) })
	node.Bus.Subscribe(eventbus.TopicTransferError, func(any) { runActs(notifier.OnTransferError()) })

	// 窗口隐藏时没有任务栏按钮可闪，Wails 的托盘也没有闪烁 API，
	// 因此「闪」只能靠交替切换托盘图标（正常版 ↔ 红点版）。
	// 600ms 一次：比任务栏闪烁略慢，足够显眼又不会晃得人烦。
	tickCtx, stopTicker := context.WithCancel(context.Background())
	defer stopTicker()
	go func() {
		t := time.NewTicker(600 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-tickCtx.Done():
				return
			case <-t.C:
				runActs(notifier.Tick())
			}
		}
	}()

	if err := app.Run(); err != nil {
		slog.Error("GUI 退出异常", "err", err)
		os.Exit(1)
	}
}

// blinkTray 刚收进托盘时把图标闪几下作为「程序还活着」的反馈。
//
// 与 Tick 的闪烁不同：这是固定次数的短促闪烁，用于确认「我收起来了」，
// 而不是「有未读消息」（后者由 Tick 持续交替）。
func blinkTray(tray *application.SystemTray) {
	for i := 0; i < 3; i++ {
		tray.SetIcon(notify.TrayAlertIcon())
		time.Sleep(180 * time.Millisecond)
		tray.SetIcon(notify.TrayIcon())
		time.Sleep(180 * time.Millisecond)
	}
}

// notifyOption 标识托盘菜单里的两个提示开关。
type notifyOption int

const (
	notifyOptSound notifyOption = iota
	notifyOptTray
)

const (
	// hoverLinger 是鼠标离开图标后提示框延迟收起的时间。
	// 不能立刻收：提示框刚弹出时系统可能先给一个 NIN_POPUPCLOSE，
	// 立刻收就等于「永远看不到」（详见 OnMouseLeave 处的说明）。
	hoverLinger = 250 * time.Millisecond
	// popupLinger 是新消息气泡停留的时长：够看清「谁发来的」，又不至于烦人。
	popupLinger = 4 * time.Second
)
