//go:build wails

// 本文件是 Wails 桌面外壳的装配。
//
// 它是【唯一】接触 Wails API 的地方：服务层与事件桥都在
// internal/adapters/wails 里以纯 Go 实现，因此 Wails beta 期 API 波动
// 只需要在这里适配一次（架构书 7.5：锁定 v3.0.0-beta.23，升级作为独立任务）。
//
// 构建：
//
//	go get github.com/wailsapp/wails/v3@v3.0.0-beta.23
//	wails3 build -tags wails
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/swarmlink/swarmlink/internal/adapters/wails"
	"github.com/swarmlink/swarmlink/internal/bootstrap"
)

// wailsEmitter 把 wails 的 Event.Emit 适配成 internal/adapters/wails.Emitter。
//
// 若你的 Wails beta 版本签名不同（例如 Emit(name, data...) 可变参），
// 只需改这一个方法 —— 这正是把 API 接触面收敛到这里的目的。
type wailsEmitter struct {
	app *application.App
}

func (e wailsEmitter) Emit(name string, data any) {
	e.app.Event.Emit(name, data)
}

func main() {
	app := application.New(application.Options{
		Name:        "SwarmLink",
		Description: "无中心内网 P2P 聊天与文件分享工具",
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	ctx := context.Background()

	node, err := bootstrap.Start(ctx, bootstrap.Options{Level: slog.LevelInfo})
	if err != nil {
		// 网络/存储起不来时，GUI 应当明确报错而不是静默半死
		slog.Error("启动失败", "err", err)
		os.Exit(1)
	}
	defer node.Close()

	services := wails.NewServices(wails.Deps{
		Self: wails.SelfInfo{
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
		PeerDir:      node.PeerDir,
		TransferRepo: node.Transfers,
		GroupRepo:    node.Groups,
		Conns:        node.Conn,
		Seeds:        node.Registry,
		Interfaces:   func() []string { return node.Ifaces },
		SeedSnapshot: func() []wails.SeedDTO {
			snap := node.Registry.Snapshot()
			out := make([]wails.SeedDTO, 0, len(snap))
			for _, s := range snap {
				out = append(out, wails.SeedDTO{
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

	// 服务只需注册；Wails 会静态分析并生成 TypeScript 绑定。
	app.RegisterService(application.NewService(services.Settings))
	app.RegisterService(application.NewService(services.Chat))
	app.RegisterService(application.NewService(services.Transfer))
	app.RegisterService(application.NewService(services.Peer))
	app.RegisterService(application.NewService(services.Group))

	// 领域事件 → 前端事件（含 4 Hz 进度节流与调试面板馈送）
	bridge := wails.NewBridge(node.Bus, node.Clk, wailsEmitter{app: app})
	bridge.Start()
	defer bridge.Stop()

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "SwarmLink",
		Width:  1180,
		Height: 760,
	})

	if err := app.Run(); err != nil {
		slog.Error("GUI 退出异常", "err", err)
		os.Exit(1)
	}
}
