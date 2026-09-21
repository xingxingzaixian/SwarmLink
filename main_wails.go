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

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	wadapter "github.com/swarmlink/swarmlink/internal/adapters/wails"
	"github.com/swarmlink/swarmlink/internal/bootstrap"
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
		Cfg:       node.Cfg,
		ConfigDir: node.ConfigDir,
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

	if err := app.Run(); err != nil {
		slog.Error("GUI 退出异常", "err", err)
		os.Exit(1)
	}
}
