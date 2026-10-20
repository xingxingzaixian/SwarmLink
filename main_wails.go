//go:build wails

// SwarmLink 桌面外壳 —— 组合根。
//
// 这是【唯一】接触 Wails API 的文件：五个服务与事件桥都在
// internal/adapters/wails 里以纯 Go 实现，因此 Wails beta 期的 API 波动
// 只需要在这里适配一次（架构书 7.5：Wails 版本锁定，升级作为独立任务 + 回归测试。
// 当前版本以 go.mod 为准，且 CLI 必须装同一个版本 —— 版本号不在注释里重复一遍，
// 免得它成为下一个「文档说了但实际不成立」的地方）。
//
// 例外是系统通知（internal/adapters/systemnotify）：它只能经由 Wails 的
// notifications 服务获得，没有「纯 Go 实现」这一说。因此那里的对外接口
// 刻意只收基本类型，Wails 的选项结构体不外泄，组合根依旧不必认识它。
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
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/swarmlink/swarmlink/internal/adapters/config"
	"github.com/swarmlink/swarmlink/internal/adapters/notify"
	"github.com/swarmlink/swarmlink/internal/adapters/systemnotify"
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

// autostartAdapter 把 Wails 的 *application.App 适配成自动启动接口
type autostartAdapter struct {
	app *application.App
}

func (a *autostartAdapter) AutostartEnable() error {
	return a.app.Autostart.Enable()
}

func (a *autostartAdapter) AutostartDisable() error {
	return a.app.Autostart.Disable()
}

// 单实例加密密钥（固定值，用于识别同一应用的不同实例）
var singleInstanceKey = [32]byte{
	0x53, 0x77, 0x61, 0x72, 0x6d, 0x4c, 0x69, 0x6e,
	0x6b, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
}

// setupFileLog 让日志同时落盘，返回日志文件（失败时返回 nil 与原因）。
//
// 为什么这不是锦上添花：产物以 -ldflags "-H windowsgui" 链接，**没有控制台**，
// 任何发生在窗口出现之前的失败都只会写进一个没人接的句柄。用户看到的现象就是
// 「双击了没反应，进程也没有」—— 本次事故正是如此：bootstrap 报
// `sqlite: attempt to write a readonly database`，Wails 报 WebView2 控制器
// 创建失败，两条信息全部丢失，只能靠事后用重定向复现才看到。
//
// 用标准库 log.SetOutput 而不是只重定向 os.Stderr：Wails 与 WebView2 的报错
// 和完整堆栈走的是标准库 log，改 os.Stderr 抓不到它们。同时保留原 stderr，
// 所以开发构建（带控制台）依旧能在终端里看到同样的内容。
// setupFileLog 让日志同时落盘，返回文件句柄、真正落到的路径。
//
// 落点优先级：数据目录 → 临时目录。
//
// 为什么必须有第二级：数据目录不可写，恰恰是最需要日志的那一类故障（本项目已经
// 遇到过），而那时若日志也写不进去，用户手上就一点可转交的证据都没有 —— 本次
// 事故的对话框里那句「日志文件本身也写不了」就是这么来的。临时目录几乎总是可写，
// 退到那里至少保住「有东西可查」。
//
// 返回值的语义（调用方据此决定对话框怎么写）：
//   - 文件非 nil 且 primaryErr 为 nil：正常，logs 就在数据目录下；
//   - 文件非 nil 且 primaryErr 非 nil：数据目录写不了，已退到临时目录，path 是那边；
//   - 文件为 nil：两处都写不了，path 是尝试过的首选路径。
func setupFileLog() (*os.File, string, error) {
	paths, err := logTargets()
	if err != nil {
		return nil, "", err
	}
	f, path, degraded := openFirstWritable(paths)
	if f != nil {
		log.SetOutput(io.MultiWriter(os.Stderr, f))
	}
	return f, path, degraded
}

// logTargets 按优先级给出候选日志路径：数据目录优先（最好找），
// 不可写时退到临时目录（几乎总是可写）。
func logTargets() ([]string, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	return []string{
		filepath.Join(dir, "logs", "swarmlink.log"),
		filepath.Join(os.TempDir(), "SwarmLink", "swarmlink.log"),
	}, nil
}

// openFirstWritable 依次尝试候选路径，返回第一个打开成功的句柄与落点。
//
// 返回的 error 语义：落在首选位置时为 nil；退到备用位置时是「首选为什么写不了」
// （调用方要据此解释「日志为什么不在数据目录里」）；全部失败时是每条候选的原因。
func openFirstWritable(paths []string) (*os.File, string, error) {
	var reasons []string
	for i, p := range paths {
		f, err := openLogFile(p)
		if err == nil {
			if i == 0 {
				return f, p, nil
			}
			return f, p, fmt.Errorf("%s 不可写：%s", paths[0], reasons[0])
		}
		reasons = append(reasons, fmt.Sprintf("%s：%v", p, err))
	}
	return nil, paths[0], fmt.Errorf("所有候选位置都不可写（%s）", strings.Join(reasons, "；"))
}

// openLogFile 建父目录并以追加方式打开日志文件。
func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// reportFatalStartup 在窗口出现之前就把失败讲清楚：写日志 + 弹原生对话框。
//
// 这里兑现的是 main 里那句注释的承诺（「GUI 必须明确报错而不是静默半死」）。
// 原实现只有 slog.Error + os.Exit(1)：在 -H windowsgui 的产物上，slog 的 handler
// 绑在 os.Stderr 上，等于把错误丢进黑洞 —— 注释承诺的事一件也没做。
func reportFatalStartup(logFile *os.File, logPath string, logErr error, cause error) {
	dir, _ := config.Dir()

	var b strings.Builder
	fmt.Fprintf(&b, "蜂群 启动失败。\n\n原因：\n%v\n\n", cause)
	fmt.Fprintf(&b, "日志文件：%s\n", logPath)
	fmt.Fprintf(&b, "数据目录：%s\n", dir)
	switch {
	case logFile == nil:
		fmt.Fprintf(&b, "\n（日志也写不进去：%v）\n", logErr)
	case logErr != nil:
		// 退到了临时目录：说清楚为什么，否则用户会去数据目录里找一个不存在的日志。
		fmt.Fprintf(&b, "\n（注意：日志已退到临时目录 —— 数据目录写不了：%v）\n", logErr)
	}
	fmt.Fprintf(&b, "\n排查提示（本对话框可用 Ctrl+C 复制）：\n"+
		"  · 若日志显示数据库无法写入：检查安全软件（火绒 / 亚信等）是否拦住了\n"+
		"    本程序对数据目录的写入；也可用环境变量 SWARMLINK_DATA_DIR 换一个可写目录。\n"+
		"  · 若显示 WebView2 控制器创建失败：删掉 <数据目录>\\webview 后重开即可。")

	slog.Error("后端启动失败", "err", cause)
	if logFile != nil {
		fmt.Fprintf(logFile, "%s ERROR 后端启动失败 err=%v\n",
			time.Now().Format(time.RFC3339), cause)
	}
	// 自动化/脚本场景下模态对话框会把进程挂住，给一个显式开关。
	if os.Getenv("SWARMLINK_NO_STARTUP_DIALOG") == "1" {
		return
	}
	showFatalDialog("蜂群 启动失败", b.String())
}

func main() {
	logFile, logPath, logErr := setupFileLog()
	if logFile != nil {
		defer func() { _ = logFile.Close() }()
	}

	// 1. 先起后端：网络/存储起不来时，GUI 必须明确报错而不是静默半死。
	node, err := bootstrap.Start(context.Background(), bootstrap.Options{Level: slog.LevelInfo})
	if err != nil {
		reportFatalStartup(logFile, logPath, logErr, err)
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

	// 定义窗口显示函数（需要在单实例回调中使用）
	var win *application.WebviewWindow
	showWindow := func() {
		if win != nil {
			win.Show()
			win.Restore()
			win.Focus()
		}
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
		Share:        node.ShareApp,
		PeerDir:      node.PeerDir,
		TransferRepo: node.Transfers,
		GroupRepo:    node.Groups,
		Deliveries:   node.Deliveries,
		Conns:        node.Conn,
		Seeds:        node.Registry,
		Interfaces:   func() []string { return node.Ifaces },
		SeedSnapshot: func() []wadapter.SeedDTO {
			snap := node.Registry.Snapshot()
			out := make([]wadapter.SeedDTO, 0, len(snap))
			for _, s := range snap {
				// 可用性只在这里定：判定规则在 udp.SeedState.Available，
				// 服务层只数个数（app.NewDiscoveryHealth 会再收口一次）。
				var lastSeen int64
				if !s.LastSeen.IsZero() {
					lastSeen = s.LastSeen.UnixMilli()
				}
				out = append(out, wadapter.SeedDTO{
					Addr:      s.Addr.String(),
					FailCount: s.FailCnt,
					NodeID:    s.NodeID,
					TCPPort:   int(s.TCPPort),
					Subnet:    s.Subnet,
					LastSeen:  lastSeen,
					Available: s.Available(),
				})
			}
			return out
		},
		SetActiveConv: setActiveConv,
		Update:        node.UpdateApp,
		Cfg:           node.Cfg,
		ConfigDir:     node.ConfigDir,
		LogPath:       logPath,
	})

	// 系统通知：一个 Wails 服务，但在本文件里只用它自己的两个方法（Send / OnActivated）。
	//
	// Available() 会随「启动失败」「发送失败」翻成 false，决策层据此回落到
	// 自建托盘气泡 —— 因此这里不需要知道它为什么不可用，也不需要重试。
	sysNotify := systemnotify.New()

	// 3. 创建应用：服务在 Options 里注册，Wails 会静态分析并生成 TypeScript 绑定。
	//
	// 系统通知也在这里注册：它必须由 Wails 启动（ServiceStartup 会准备
	// 平台侧的通知通道），而适配器保证「启动失败只降级、不让应用起不来」。
	var app *application.App
	app = application.New(application.Options{
		Name:        "蜂群",
		Description: "无中心内网 P2P 聊天与文件分享工具",
		Services: []application.Service{
			application.NewService(services.Settings),
			application.NewService(services.Chat),
			application.NewService(services.Transfer),
			application.NewService(services.Peer),
			application.NewService(services.Group),
			application.NewService(services.Share),
			application.NewService(services.Notify),
			application.NewService(services.Search),
			application.NewService(services.Update),
			application.NewService(sysNotify),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		// WebView2 的缓存目录默认是 %APPDATA%\<可执行文件名>。两个问题：
		//  ① 浏览器缓存放 Roaming 本就不对（会被漫游配置文件同步，体积也不小）；
		//  ② 用户目录不可写时它会以 ERROR_BUSY / 0x8000FFFF 失败，而那个错误
		//     由 Wails 内部 os.Exit 收尾 —— 我们放在 main 里的启动失败对话框
		//     根本来不及介入，用户看到的仍然是「双击没反应」。
		// 让它跟着数据目录走，「换一个可写目录就能跑」这条出路才真正成立。
		Windows: application.WindowsOptions{
			WebviewUserDataPath: filepath.Join(node.ConfigDir, "webview"),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
		// 使用 Wails 内置的单实例功能
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID:      "com.swarmlink.app",
			EncryptionKey: singleInstanceKey,
			OnSecondInstanceLaunch: func(data application.SecondInstanceData) {
				slog.Info("检测到第二个实例启动", "args", data.Args, "workingDir", data.WorkingDir)
				// 激活已有实例的窗口
				showWindow()
				// 通知前端有新实例启动（可选）
				app.Event.Emit(wadapter.EventSecondInstanceLaunched, map[string]any{
					"args":       data.Args,
					"workingDir": data.WorkingDir,
				})
			},
			AdditionalData: map[string]string{
				"launchtime": time.Now().Local().String(),
			},
		},
	})

	// 用户点了系统通知：把窗口叫到前台，并让前端切到对应会话。
	//
	// 两件事都必须做：只切会话而窗口还在后台等于没反应；只叫窗口则用户
	// 还得自己去找是哪个人发来的 —— 通知里本来就知道。
	// convID 经 NotificationOptions.Data 随通知发出去、点击时原样回来。
	sysNotify.OnActivated(func(convID string) {
		showWindow()
		app.Event.Emit(wadapter.EventSystemNotifyActivated, map[string]any{"convId": convID})
	})

	// 局域网更新：把「状态变化 → 前端事件」与「安装完退出进程」接上。
	//
	// 这两件事必须由外壳注入：app 层不许 import Wails，也不该知道
	// 「退出进程」长什么样（CLI 模式下根本没有 app.Quit）。
	services.Update.SetEmitter(func(name string, data any) { app.Event.Emit(name, data) })
	// 窗口收在托盘里时用一条系统通知提醒，否则「更新已就绪」这件事
	// 只存在于一个用户看不见的窗口里，等到 4 小时兜底才被动安装。
	// 点击通知会把窗口叫到前台 —— 确认框本来就已经渲染好了（由状态驱动），
	// 因此这里不需要额外的跳转参数。
	var lastUpdateNotify atomic.Value
	node.UpdateApp.OnChange = func(st capp.UpdateState) {
		services.Update.Publish(st)
		if !st.PromptPending || st.TargetVersion == "" || win.IsVisible() {
			return
		}
		if v, _ := lastUpdateNotify.Load().(string); v == st.TargetVersion {
			return
		}
		lastUpdateNotify.Store(st.TargetVersion)
		_ = sysNotify.Send(systemnotify.Message{
			ID:    "update:" + st.TargetVersion,
			Title: "蜂群有可用更新",
			Body:  fmt.Sprintf("v%s 已下载完成，点击查看并选择安装时间", st.TargetVersion),
		})
	}
	node.UpdateApp.Quit = func() {
		// 给安装器的分离进程一点启动时间，再退出自己。
		// 不 sleep 的话极少数情况下会在 cmd 被调度之前退出，
		// 表现就是「点了立即更新，程序关了但没重装」。
		go func() {
			time.Sleep(300 * time.Millisecond)
			app.Quit()
		}()
	}

	// 注入自动启动适配器到服务层（用于自动启动功能）
	autostart := &autostartAdapter{app: app}
	wadapter.SetAutostartFuncs(autostart.AutostartEnable, autostart.AutostartDisable)

	// 启动时检查自动启动配置并同步到系统
	if node.Cfg.Startup.AutoStart {
		if err := app.Autostart.Enable(); err != nil {
			slog.Warn("启用开机自启动失败", "err", err)
		}
	} else {
		if err := app.Autostart.Disable(); err != nil {
			slog.Warn("禁用开机自启动失败", "err", err)
		}
	}

	// 4. 领域事件 → 前端事件（含按 job 的 4 Hz 进度节流与调试面板馈送）
	bridge := wadapter.NewBridge(node.Bus, node.Clk, wailsEmitter{app: app})
	bridge.Start()
	defer bridge.Stop()

	// 5. 主窗口
	win = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "蜂群",
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
			// 现取现用：可用性会在发送失败时被适配器翻成 false，
			// 下一条消息就该走气泡了。
			SystemNotifyOK: sysNotify.Available(),
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
	tray.SetTooltip("蜂群")

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
			case capp.ActSystemNotify:
				// 失败即降级：可用性标志已被适配器翻成 false（后续消息改走气泡），
				// 本次仍要用气泡把这条消息补上 —— 否则「第一次失败」的那条消息
				// 就什么提示都没有了。
				if err := sysNotify.Send(systemnotify.Message{
					ID:     a.ConvID,
					Title:  a.Title,
					Body:   a.Text,
					ConvID: a.ConvID,
					Silent: a.Silent,
				}); err != nil {
					slog.Warn("发送系统通知失败，回落到托盘气泡", "err", err)
					showPopup(a.Text)
				}
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
		case notifyOptAutoStart:
			node.Cfg.Startup.AutoStart = on
			// 同步到系统
			if on {
				if err := app.Autostart.Enable(); err != nil {
					slog.Warn("启用开机自启动失败", "err", err)
				}
			} else {
				if err := app.Autostart.Disable(); err != nil {
					slog.Warn("禁用开机自启动失败", "err", err)
				}
			}
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
	autoStartItem := menu.AddCheckbox("开机自启动", true).SetChecked(node.Cfg.Startup.AutoStart)
	autoStartItem.OnClick(func(*application.Context) {
		setNotifyOption(notifyOptAutoStart, autoStartItem.Checked())
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

// notifyOption 标识托盘菜单里的提示开关。
type notifyOption int

const (
	notifyOptSound notifyOption = iota
	notifyOptTray
	notifyOptAutoStart
)

const (
	// hoverLinger 是鼠标离开图标后提示框延迟收起的时间。
	// 不能立刻收：提示框刚弹出时系统可能先给一个 NIN_POPUPCLOSE，
	// 立刻收就等于「永远看不到」（详见 OnMouseLeave 处的说明）。
	hoverLinger = 250 * time.Millisecond
	// popupLinger 是新消息气泡停留的时长：够看清「谁发来的」，又不至于烦人。
	popupLinger = 4 * time.Second
)
