// Package bootstrap 是【唯一的组合根】。
//
// 架构书 3.1 第 4 点：所有 new 具体适配器、注入依赖、启动 goroutine 都发生在这里；
// 业务代码里不允许出现 sqlite.New(...)（红线 R3 由 CI 检查）。
//
// CLI 与 GUI 共享本包，因此二者跑的是同一条装配路径 ——
// 在 CLI 上验证过的链路，GUI 上必然也通。
package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/config"
	"github.com/swarmlink/swarmlink/internal/adapters/filesink"
	"github.com/swarmlink/swarmlink/internal/adapters/keystore"
	"github.com/swarmlink/swarmlink/internal/adapters/net/tcp"
	"github.com/swarmlink/swarmlink/internal/adapters/net/udp"
	"github.com/swarmlink/swarmlink/internal/adapters/store/mem"
	"github.com/swarmlink/swarmlink/internal/adapters/store/sqlite"
	"github.com/swarmlink/swarmlink/internal/app"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// Options 是装配参数。零值即「用配置文件的默认值」。
type Options struct {
	Name        string
	DataDir     string
	DownloadDir string
	// TCPPort/UDPPort 为 -1 表示沿用配置值；0 表示由系统分配。
	TCPPort int
	UDPPort int
	// UseMem=true 时不落盘（试验/测试用）。
	UseMem bool
	// SeedOnly=true 关闭自动广播发现，只用种子。
	SeedOnly bool
	// ExtraSeeds 是命令行/UI 额外追加的种子。
	ExtraSeeds []peer.SeedAddr
	Level      slog.Level
}

// Node 是一个装配完成、已经跑起来的节点。
type Node struct {
	KP        *identity.KeyPair
	Cfg       *config.Config
	ConfigDir string

	Bus *eventbus.Bus
	// Clk 暴露给事件桥使用（节流需要可控时钟）。
	Clk ports.Clock

	Conn     *tcp.Manager
	BC       *udp.Broadcaster
	Registry *udp.Registry
	Sink     *filesink.Sink

	Messages  ports.ChatStore
	Transfers ports.TransferRepo
	Groups    ports.GroupRepo
	PeerDir   ports.PeerDirectory

	PeerApp     *app.PeerApp
	ChatApp     *app.ChatApp
	GroupApp    *app.GroupApp
	TransferApp *app.TransferApp
	DiscApp     *app.DiscoveryApp
	Router      *app.Router

	TCPPort    int
	UDPPort    int
	SelfSubnet string
	Ifaces     []string

	// Notices 是端口回退等需要提示用户的非致命信息。
	Notices []string

	closers []func()
}

// Start 完成全部装配并启动后台循环。
func Start(ctx context.Context, opts Options) (*Node, error) {
	// ---------------------------------------------------------------- 配置
	dir := opts.DataDir
	if dir == "" {
		d, err := config.Dir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return nil, err
	}
	if opts.Name != "" {
		cfg.General.DisplayName = opts.Name
	}
	if opts.DownloadDir != "" {
		cfg.Download.DefaultDir = opts.DownloadDir
	}
	if opts.TCPPort >= 0 {
		cfg.Network.TCPPort = opts.TCPPort
	}
	if opts.UDPPort >= 0 {
		cfg.Network.UDPPort = opts.UDPPort
	}
	if opts.SeedOnly {
		cfg.Network.InterfaceMode = "seed_only"
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// ---------------------------------------------------------------- 身份
	kp, err := keystore.LoadOrCreate(dir)
	if err != nil {
		return nil, err
	}
	lg := newLogger(opts.Level, kp.NodeID().String())

	n := &Node{KP: kp, Cfg: cfg, ConfigDir: dir}
	clk := clock.New()
	bus := eventbus.New()
	n.Bus = bus
	n.Clk = clk

	// ---------------------------------------------------------------- 存储
	if opts.UseMem {
		m := mem.NewMessages(clk)
		n.Messages, n.Transfers = m, m
		n.Groups = mem.NewGroups()
		n.PeerDir = mem.NewPeers(clk)
	} else {
		dbPath := config.DBPath(dir)
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			return nil, err
		}
		db, err := sqlite.Open(dbPath)
		if err != nil {
			return nil, err
		}
		n.closers = append(n.closers, func() { _ = db.Close() })
		n.Messages = sqlite.NewMessages(db)
		n.Transfers = sqlite.NewTransfers(db)
		n.Groups = sqlite.NewGroups(db)
		n.PeerDir = sqlite.NewPeers(db, clk)
	}

	// ---------------------------------------------------------------- 落盘
	sink, err := filesink.New(cfg.Download.DefaultDir)
	if err != nil {
		n.Close()
		return nil, err
	}
	n.Sink = sink

	// ---------------------------------------------------------------- 网络
	policy := tcp.DefaultPolicy()
	policy.MaxDialConcurrency = cfg.Connection.MaxDialConcurrency
	policy.MaxActiveConns = cfg.Connection.MaxActiveConns
	policy.IdleTimeout = cfg.Connection.IdleConnTimeout.Std()

	hcfg := tcp.DefaultHandshakeConfig()
	hcfg.DisplayName = cfg.General.DisplayName
	hcfg.MaxChunkSize = int(cfg.Transfer.ChunkSize)
	hcfg.WindowSize = cfg.Transfer.WindowSize
	hcfg.RequireAuth = cfg.Security.RequireAuth

	conn := tcp.NewManager(kp, hcfg, policy, clk, lg, bus, nil)
	n.Conn = conn

	actualTCP, notice, err := startTCP(ctx, conn, cfg.Network.TCPPort, cfg.Network.PortFallbackRange)
	if err != nil {
		n.Close()
		return nil, err
	}
	n.TCPPort = actualTCP
	if notice != "" {
		n.Notices = append(n.Notices, notice)
	}

	bcCfg := udp.DefaultConfig()
	bcCfg.DisplayName = cfg.General.DisplayName
	bcCfg.TCPPort = uint16(actualTCP)
	bcCfg.AnnounceInterval = cfg.Discovery.AnnounceInterval.Std()
	bcCfg.AllowInterfaces = cfg.Network.AllowInterfaces
	bcCfg.DenyInterfaces = cfg.Network.DenyInterfaces
	bcCfg.EnableMulticast = cfg.Network.EnableMulticast
	bcCfg.SeedOnly = cfg.Network.InterfaceMode == "seed_only"

	bc := udp.NewBroadcaster(kp, bcCfg, clk, lg)
	n.BC = bc

	peerApp := app.NewPeerApp(kp.NodeID(), n.PeerDir, bus, clk, cfg.Discovery.PeerTTL.Std(), lg)
	n.PeerApp = peerApp

	if err := startBroadcast(ctx, bc, cfg.Network.UDPPort, cfg.Network.PortFallbackRange, peerApp); err != nil {
		n.Close()
		return nil, err
	}
	n.UDPPort = bc.UDPPort()
	if cfg.Network.UDPPort != 0 && n.UDPPort != cfg.Network.UDPPort {
		n.Notices = append(n.Notices, fmt.Sprintf(
			"UDP %d 被占用，已回退到 %d（对端从 announce 学习，无需配置）",
			cfg.Network.UDPPort, n.UDPPort))
	}
	for _, i := range bc.Interfaces() {
		n.Ifaces = append(n.Ifaces, fmt.Sprintf("%s(%s)", i.Name, i.SubnetString()))
		if n.SelfSubnet == "" {
			n.SelfSubnet = i.SubnetString()
		}
	}

	// ---------------------------------------------------------------- 应用层
	n.ChatApp = app.NewChatApp(kp.NodeID(), n.Messages, conn, n.PeerDir, bus, clk, lg)
	n.GroupApp = app.NewGroupApp(kp.NodeID(), kp, n.Groups, n.Messages, conn, n.PeerDir, bus, clk, lg)
	n.TransferApp = app.NewTransferApp(kp.NodeID(), n.Transfers, sink, conn, n.PeerDir, bus, clk, lg)
	n.TransferApp.MaxConcurrent = cfg.Transfer.MaxConcurrent
	n.TransferApp.ChunkSize = cfg.Transfer.ChunkSize
	n.TransferApp.WindowSize = cfg.Transfer.WindowSize
	n.TransferApp.BitmapFlushInterval = cfg.Transfer.BitmapFlushInterval.Std()

	seedAddrs, seedErrs := cfg.SeedAddrs()
	seedAddrs = append(seedAddrs, opts.ExtraSeeds...)
	n.Notices = append(n.Notices, formatSeedErrs(seedErrs)...)

	registry := udp.NewRegistry(seedAddrs, clk)
	n.Registry = registry
	prober := &udp.Prober{Timeout: cfg.Discovery.Seeds.ProbeTimeout.Std()}

	n.DiscApp = app.NewDiscoveryApp(kp.NodeID(), peerApp, n.PeerDir, conn, registry, prober, bus, clk, lg)
	n.DiscApp.LocalEpoch = bc.Epoch
	n.DiscApp.RefreshInterval = cfg.Discovery.Seeds.RefreshInterval.Std()
	n.DiscApp.SeedsPerRefresh = cfg.Discovery.Seeds.SeedsPerRefresh
	n.DiscApp.MaxEntries = cfg.Discovery.MaxPeerListSize

	n.Router = app.NewRouter(n.ChatApp, n.GroupApp, n.TransferApp, n.DiscApp, peerApp, lg)
	conn.SetHandler(n.Router.Handle)

	peerApp.Start()
	n.ChatApp.Start(ctx)
	n.DiscApp.Start(ctx)

	// 后台维护：目录 TTL、outbox 重发、空闲连接回收。
	// 不用真实定时器驱动领域逻辑，而是让 App 暴露 Sweep() 由这里统一节拍。
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-t.C:
				peerApp.Sweep()
				n.ChatApp.SweepOutbox(ctx)
				n.GroupApp.SweepOutbox(ctx)
			}
		}
	}()
	n.closers = append(n.closers, func() { close(stop) })

	return n, nil
}

// Close 关闭全部资源（幂等由各组件自己保证）。
func (n *Node) Close() {
	if n.Conn != nil {
		n.Conn.Stop()
	}
	if n.BC != nil {
		_ = n.BC.Stop()
	}
	for i := len(n.closers) - 1; i >= 0; i-- {
		n.closers[i]()
	}
	n.closers = nil
}

// startTCP 按 ADR-008 处理端口占用：2425 → 2426 …（0 表示交给系统分配）。
func startTCP(ctx context.Context, mgr *tcp.Manager, base, fallback int) (int, string, error) {
	if base == 0 {
		addr, err := mgr.Start(ctx, "0.0.0.0:0")
		if err != nil {
			return 0, "", err
		}
		return portOf(addr), "", nil
	}
	if fallback < 1 {
		fallback = 1
	}
	var lastErr error
	for i := 0; i < fallback; i++ {
		port := base + i
		addr, err := mgr.Start(ctx, fmt.Sprintf("0.0.0.0:%d", port))
		if err == nil {
			if i == 0 {
				return portOf(addr), "", nil
			}
			return portOf(addr), fmt.Sprintf("TCP %d 被占用，已回退到 %d", base, port), nil
		}
		lastErr = err
	}
	return 0, "", fmt.Errorf("TCP 端口 %d..%d 全部被占用: %w", base, base+fallback-1, lastErr)
}

func startBroadcast(ctx context.Context, bc *udp.Broadcaster, base, fallback int, peerApp *app.PeerApp) error {
	sink := func(an peer.Announcement) { _ = peerApp.OnAnnouncement(an) }

	if base == 0 {
		return bc.Start(ctx, sink)
	}
	if fallback < 1 {
		fallback = 1
	}
	var lastErr error
	for i := 0; i < fallback; i++ {
		bc.SetUDPPort(base + i)
		if err := bc.Start(ctx, sink); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return fmt.Errorf("UDP 端口 %d..%d 全部被占用: %w", base, base+fallback-1, lastErr)
}

func portOf(addr string) int {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return 0
	}
	var p int
	_, _ = fmt.Sscanf(addr[i+1:], "%d", &p)
	return p
}

func formatSeedErrs(errs []error) []string {
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, "种子地址格式错误已忽略: "+e.Error())
	}
	return out
}

func newLogger(level slog.Level, nodeID string) *slog.Logger {
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	return slog.New(h).With("node_id", nodeID)
}
