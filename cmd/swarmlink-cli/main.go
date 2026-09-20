// Command swarmlink-cli 是 SwarmLink 的命令行前端。
//
// 它同时承担两个角色：
//  1. M1 交付物 —— 验证「identity → discovery → transport → store → 读回」全链路
//  2. 长期调试工具 —— P2P 排障在第一现场最缺的就是一个能直连协议层的入口
//
// 注意：它不是抛弃型原型。装配方式与 GUI 完全一致（同一个组合根逻辑），
// 因此 CLI 上跑通的链路，GUI 上必然也通。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
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

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

type cli struct {
	self identity.NodeID
	kp   *identity.KeyPair

	dir ports.PeerDirectory
	// 保留具体类型（而非 ports.ConnManager）以便诊断面板读取 SessionCount。
	conns *tcp.Manager
	bus   *eventbus.Bus

	peers    *app.PeerApp
	chat     *app.ChatApp
	group    *app.GroupApp
	transfer *app.TransferApp
	disc     *app.DiscoveryApp

	registry *udp.Registry
	bc       *udp.Broadcaster

	tcpPort    int
	selfSubnet string
}

func main() {
	var (
		name        = flag.String("name", "", "显示名（默认取主机名）")
		dataDir     = flag.String("data-dir", "", "配置/数据目录（默认用户配置目录）")
		downloadDir = flag.String("download-dir", "", "接收文件目录（默认配置中的 download.default_dir）")
		tcpPort     = flag.Int("tcp-port", -1, "TCP 端口（-1 = 用配置值，0 = 自动）")
		udpPort     = flag.Int("udp-port", -1, "UDP 端口（-1 = 用配置值，0 = 自动）")
		useMem      = flag.Bool("mem", false, "使用内存存储（不落盘，便于试验）")
		seedOnly    = flag.Bool("seed-only", false, "关闭自动广播发现，只用种子（VPN/安全敏感环境）")
		selfCheck   = flag.Bool("self-check", false, "启动后执行 P-1/P-2 部署前置条件自检并退出")
		verbose     = flag.Bool("v", false, "输出调试日志")
	)
	var seeds stringList
	flag.Var(&seeds, "seed", "种子地址 ip:udp_port（可重复）")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}

	if err := run(*name, *dataDir, *downloadDir, *tcpPort, *udpPort, *useMem, *seedOnly, *selfCheck, seeds, level); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run(name, dataDir, downloadDir string, tcpPort, udpPort int, useMem, seedOnly, selfCheck bool,
	seeds stringList, level slog.Level) error {

	// ---------------------------------------------------------------- 配置
	dir := dataDir
	if dir == "" {
		d, err := config.Dir()
		if err != nil {
			return err
		}
		dir = d
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return err
	}
	if name != "" {
		cfg.General.DisplayName = name
	}
	if downloadDir != "" {
		cfg.Download.DefaultDir = downloadDir
	}
	if tcpPort >= 0 {
		cfg.Network.TCPPort = tcpPort
	}
	if udpPort >= 0 {
		cfg.Network.UDPPort = udpPort
	}
	if seedOnly {
		cfg.Network.InterfaceMode = "seed_only"
	}
	if len(seeds) > 0 {
		cfg.Discovery.Seeds.List = append(cfg.Discovery.Seeds.List, seeds...)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	// ---------------------------------------------------------------- 身份
	kp, err := keystore.LoadOrCreate(dir)
	if err != nil {
		return err
	}
	lg := newLogger(level, kp.NodeID().String())

	fmt.Printf("SwarmLink CLI\n")
	fmt.Printf("  显示名  : %s\n", cfg.General.DisplayName)
	fmt.Printf("  NodeID  : %s\n", kp.NodeID())
	fmt.Printf("  配置目录: %s\n", dir)
	fmt.Printf("  接收目录: %s\n\n", cfg.Download.DefaultDir)

	// ---------------------------------------------------------------- 存储
	clk := clock.New()
	bus := eventbus.New()

	var (
		messages  ports.ChatStore
		transfers ports.TransferRepo
		groups    ports.GroupRepo
		peerDir   ports.PeerDirectory
		storeKind string
	)
	if useMem {
		m := mem.NewMessages(clk)
		messages, transfers = m, m
		groups = mem.NewGroups()
		peerDir = mem.NewPeers(clk)
		storeKind = "memory (不落盘)"
	} else {
		dbPath := config.DBPath(dir)
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			return err
		}
		db, err := sqlite.Open(dbPath)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		messages = sqlite.NewMessages(db)
		transfers = sqlite.NewTransfers(db)
		groups = sqlite.NewGroups(db)
		peerDir = sqlite.NewPeers(db, clk)
		storeKind = dbPath
	}
	fmt.Printf("  存储    : %s\n", storeKind)

	// ---------------------------------------------------------------- 落盘
	sink, err := filesink.New(cfg.Download.DefaultDir)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	conns := tcp.NewManager(kp, hcfg, policy, clk, lg, bus, nil)

	tcpAddr, actualTCPPort, err := startTCP(ctx, conns, cfg.Network.TCPPort, cfg.Network.PortFallbackRange)
	if err != nil {
		return err
	}
	if cfg.Network.TCPPort != 0 && actualTCPPort != cfg.Network.TCPPort {
		fmt.Printf("  注意    : TCP %d 被占用，已回退到 %d\n", cfg.Network.TCPPort, actualTCPPort)
	}

	bcCfg := udp.DefaultConfig()
	bcCfg.DisplayName = cfg.General.DisplayName
	bcCfg.TCPPort = uint16(actualTCPPort)
	bcCfg.AnnounceInterval = cfg.Discovery.AnnounceInterval.Std()
	bcCfg.AllowInterfaces = cfg.Network.AllowInterfaces
	bcCfg.DenyInterfaces = cfg.Network.DenyInterfaces
	bcCfg.EnableMulticast = cfg.Network.EnableMulticast
	bcCfg.SeedOnly = cfg.Network.InterfaceMode == "seed_only"

	bc := udp.NewBroadcaster(kp, bcCfg, clk, lg)

	peerApp := app.NewPeerApp(kp.NodeID(), peerDir, bus, clk, cfg.Discovery.PeerTTL.Std(), lg)
	if err := startBroadcast(ctx, bc, cfg.Network.UDPPort, cfg.Network.PortFallbackRange, peerApp); err != nil {
		return err
	}
	if cfg.Network.UDPPort != 0 && bc.UDPPort() != cfg.Network.UDPPort {
		fmt.Printf("  注意    : UDP %d 被占用，已回退到 %d（对端从 announce 学习，无需配置）\n",
			cfg.Network.UDPPort, bc.UDPPort())
	}

	chatApp := app.NewChatApp(kp.NodeID(), messages, conns, peerDir, bus, clk, lg)
	groupApp := app.NewGroupApp(kp.NodeID(), kp, groups, messages, conns, peerDir, bus, clk, lg)
	transferApp := app.NewTransferApp(kp.NodeID(), transfers, sink, conns, peerDir, bus, clk, lg)
	transferApp.MaxConcurrent = cfg.Transfer.MaxConcurrent
	transferApp.ChunkSize = cfg.Transfer.ChunkSize
	transferApp.WindowSize = cfg.Transfer.WindowSize
	transferApp.BitmapFlushInterval = cfg.Transfer.BitmapFlushInterval.Std()

	seedAddrs, seedErrs := cfg.SeedAddrs()
	for _, e := range seedErrs {
		fmt.Fprintf(os.Stderr, "  警告    : 种子地址格式错误已忽略: %v\n", e)
	}
	registry := udp.NewRegistry(seedAddrs, clk)
	prober := &udp.Prober{Timeout: cfg.Discovery.Seeds.ProbeTimeout.Std()}

	discApp := app.NewDiscoveryApp(kp.NodeID(), peerApp, peerDir, conns, registry, prober, bus, clk, lg)
	discApp.LocalEpoch = bc.Epoch
	discApp.RefreshInterval = cfg.Discovery.Seeds.RefreshInterval.Std()
	discApp.SeedsPerRefresh = cfg.Discovery.Seeds.SeedsPerRefresh
	discApp.MaxEntries = cfg.Discovery.MaxPeerListSize

	router := app.NewRouter(chatApp, groupApp, transferApp, discApp, peerApp, lg)
	conns.SetHandler(router.Handle)

	peerApp.Start()
	chatApp.Start(ctx)
	discApp.Start(ctx)

	c := &cli{
		self: kp.NodeID(), kp: kp, dir: peerDir, conns: conns, bus: bus,
		peers: peerApp, chat: chatApp, group: groupApp, transfer: transferApp, disc: discApp,
		registry: registry, bc: bc,
		tcpPort: actualTCPPort,
	}

	fmt.Printf("  监听    : TCP %s / UDP :%d\n", tcpAddr, bc.UDPPort())
	if ifaces := bc.Interfaces(); len(ifaces) > 0 {
		parts := make([]string, 0, len(ifaces))
		for _, i := range ifaces {
			parts = append(parts, fmt.Sprintf("%s(%s)", i.Name, i.SubnetString()))
		}
		fmt.Printf("  网卡    : %s\n", strings.Join(parts, ", "))
		c.selfSubnet = ifaces[0].SubnetString()
	} else {
		fmt.Printf("  网卡    : 未找到可用物理网卡（跨网段场景可忽略）\n")
	}
	if n := registry.Len(); n > 0 {
		list := make([]string, 0, n)
		for _, s := range seedAddrs {
			list = append(list, s.String())
		}
		fmt.Printf("  种子    : %s\n", strings.Join(list, ", "))
	}
	fmt.Printf("\n输入 /help 查看命令。\n\n")

	if selfCheck {
		c.runSelfCheck(ctx)
		return nil
	}

	// ---------------------------------------------------------------- 后台维护
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.peers.Sweep()
				c.chat.SweepOutbox(ctx)
				c.group.SweepOutbox(ctx)
			}
		}
	}()

	c.subscribeEvents()

	// ---------------------------------------------------------------- REPL
	c.repl(ctx)

	fmt.Println("正在退出…")
	conns.Stop()
	_ = bc.Stop()
	return nil
}

// startTCP 按 ADR-008 处理端口占用：2425 → 2426 …（0 表示交给系统分配）。
func startTCP(ctx context.Context, mgr *tcp.Manager, base, fallback int) (string, int, error) {
	if fallback < 1 {
		fallback = 1
	}
	if base == 0 {
		addr, err := mgr.Start(ctx, "0.0.0.0:0")
		if err != nil {
			return "", 0, err
		}
		return addr, portOf(addr), nil
	}
	var lastErr error
	for i := 0; i < fallback; i++ {
		port := base + i
		addr, err := mgr.Start(ctx, fmt.Sprintf("0.0.0.0:%d", port))
		if err == nil {
			return addr, portOf(addr), nil
		}
		lastErr = err
	}
	return "", 0, fmt.Errorf("TCP 端口 %d..%d 全部被占用: %w", base, base+fallback-1, lastErr)
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

// runSelfCheck 执行 P-1 / P-2 部署前置条件自检。
//
// 这两条是【网络侧的既定事实，代码救不了】，因此必须能在部署前被验证，
// 而不是等到现场才发现跨网段完全不通（见 0.5 节）。
func (c *cli) runSelfCheck(ctx context.Context) {
	fmt.Println("=== 部署前置条件自检 ===")

	seeds := c.disc.SeedSnapshot()
	if len(seeds) == 0 {
		fmt.Println("P-1/P-2 未配置种子，跳过（单网段部署无需自检）")
		return
	}

	c.disc.RefreshSeeds(ctx)
	time.Sleep(300 * time.Millisecond)

	subnets := map[string][]string{}
	okCount := 0
	for _, s := range seeds {
		found := false
		for _, p := range c.dir.List(ports.PeerFilter{Source: peer.SourceSeed}) {
			if p.LastAddr == "" {
				continue
			}
			// 只针对本种子学到的条目做粗粒度判断（地址与种子同网段时归为同批）
			found = true
			if p.Subnet != "" {
				subnets[p.Subnet] = append(subnets[p.Subnet], p.NodeID.String())
			}
		}
		if found {
			okCount++
			fmt.Printf("  [P-1 OK]   种子 %s 已探通并拉取到目录\n", s.String())
		} else {
			fmt.Printf("  [P-1 FAIL] 种子 %s 不可达或无可拉取目录 —— 跨网段将完全失效，只能引入中继\n", s.String())
		}
	}
	if okCount == 0 {
		fmt.Println("  结论：P-1 不成立。请确认任意两网段的种子 IP 之间可 TCP 直连（三层路由可达，非 NAT 隔离）。")
	}

	warned := false
	for subnet, ids := range subnets {
		if len(ids) < 2 {
			continue
		}
		// 同一 subnet 上出现多个不同 node_id 且均来自种子 → 疑似地址段重叠
		if c.selfSubnet != "" && subnet == c.selfSubnet {
			fmt.Printf("  [P-2 警告] 通过种子学到的条目与【本机】处于同一地址段 %s（%d 个节点）。\n"+
				"             若这些节点实际位于不同物理网段，说明各网段地址段重叠，跨网段寻址会崩溃。\n", subnet, len(ids))
			warned = true
		}
	}
	if !warned {
		fmt.Println("  [P-2 未发现异常] 未观察到与地址段重叠一致的现象。")
	}

	snap := c.registry.Snapshot()
	fmt.Println("  种子状态：")
	for _, s := range snap {
		fmt.Printf("    - %-22s fail=%d node=%s tcp=%d subnet=%s\n",
			s.Addr.String(), s.FailCnt, orDash(s.NodeID), s.TCPPort, orDash(s.Subnet))
	}
}

func newLogger(level slog.Level, nodeID string) *slog.Logger {
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	return slog.New(h).With("node_id", nodeID)
}
