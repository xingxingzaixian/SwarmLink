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

	Messages   ports.ChatStore
	Transfers  ports.TransferRepo
	Groups     ports.GroupRepo
	Deliveries ports.GroupDeliveryRepo
	PeerDir    ports.PeerDirectory
	Shares     ports.ShareRepo

	PeerApp     *app.PeerApp
	ChatApp     *app.ChatApp
	GroupApp    *app.GroupApp
	TransferApp *app.TransferApp
	ShareApp    *app.ShareApp
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
	// 身份托管目录与数据目录【无关】；返回空串表示禁用（便携模式 / 两者重合 / 平台无机器级位置）。
	// 为什么要在这里解析而不是让 keystore 自己去问 config：adapters 之间禁止互引（R2）。
	vaultDir := config.VaultDir(dir)
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
	res, err := keystore.LoadOrCreate(dir, vaultDir)
	if err != nil {
		return nil, err
	}
	kp := res.KeyPair
	lg := newLogger(opts.Level, kp.NodeID().String())
	// vaultDir == "" 有四种原因（便携 / 与数据目录重合 / 平台无机器级位置 / env 被设成数据目录），
	// 其中最后一种最容易被踩，而排查「换了数据目录身份为什么没恢复」时它几乎不留线索。
	if vaultDir == "" && strings.TrimSpace(os.Getenv(config.EnvVaultDir)) != "" {
		lg.Warn("身份托管已禁用：SWARMLINK_VAULT_DIR 与数据目录重合", "data_dir", dir)
	}
	if res.Notice != "" {
		// 当前只有日志这一个呈现面（设计文档「非目标」里排除了启动通知 UI）。
		// 这条日志是「用户突然换了身份」时唯一能查到原因的地方，措辞要能自解释。
		lg.Warn("identity notice", "notice", res.Notice, "restored", res.Restored)
	}

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
		n.Deliveries = mem.NewGroupDeliveries()
		n.PeerDir = mem.NewPeers(clk)
		n.Shares = mem.NewShares()
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
		n.Deliveries = sqlite.NewGroupDeliveries(db)

		// 传输记录清理放在【启动】时，而不是退出时 ——
		// 异常退出根本走不到退出逻辑，而这张表是应用里唯一持续膨胀的东西。
		// 配置写 0 时回落到默认值：否则一次手误就会把历史全清掉。
		keep := cfg.Transfer.HistoryKeep
		if keep <= 0 {
			keep = 50
		}
		ttl := cfg.Transfer.HistoryTTL.Std()
		if ttl <= 0 {
			ttl = 7 * 24 * time.Hour
		}
		if purged, err := n.Transfers.PurgeFinished(keep, time.Now().Add(-ttl)); err != nil {
			n.Notices = append(n.Notices, fmt.Sprintf("传输记录清理失败：%v", err))
		} else if purged > 0 {
			n.Notices = append(n.Notices, fmt.Sprintf("已清理 %d 条过期传输记录", purged))
		}
		n.PeerDir = sqlite.NewPeers(db, clk)
		n.Shares = sqlite.NewShares(db)
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
	// Sweep 需要知道「是否仍有活动会话」，才能把 TTL 兜底从 discovered 扩展到 online。
	peerApp.SetConnLookup(n.Conn)

	// 启动边界：清空上次运行残留的「在线」。
	//
	// 进程刚起来不可能有已握手的会话，目录里的 online 一律不可信（ADR-009）。不清零会
	// 复现「A 重启后，已离线的 B 仍显示在线」—— B 的离线事件在 A 离线期间永远到不了本机，
	// 只能靠这里 + 下一次 TTL 兜底纠正。放在 startBroadcast 之前，避免与入站 announce 竞争。
	if cleared, err := n.PeerDir.ResetLiveness(); err != nil {
		lg.Warn("清除残留在线状态失败", "err", err)
	} else if cleared > 0 {
		lg.Info("已清除上次运行残留的在线状态", "count", cleared)
	}

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
	n.GroupApp = app.NewGroupApp(kp.NodeID(), kp, n.Groups, n.Messages, n.Deliveries, conn, n.PeerDir, bus, clk, lg)
	// 群消息与单聊共用 TypeChatAck：把群 ACK 从 ChatApp 委派到 GroupApp（按成员记账）。
	n.ChatApp.GroupAck = n.GroupApp.HandleGroupAck
	n.TransferApp = app.NewTransferApp(kp.NodeID(), n.Transfers, sink, conn, n.PeerDir, bus, clk, lg)
	n.TransferApp.MaxConcurrent = cfg.Transfer.MaxConcurrent
	n.TransferApp.ChunkSize = cfg.Transfer.ChunkSize
	n.TransferApp.WindowSize = cfg.Transfer.WindowSize
	n.TransferApp.BitmapFlushInterval = cfg.Transfer.BitmapFlushInterval.Std()

	// 文件共享（FR-14）：仓储在上面按存储分支装配，这里组装应用。
	n.ShareApp = app.NewShareApp(
		kp.NodeID(), n.Shares, n.PeerDir, n.Groups, n.TransferApp, bus, clk, lg)
	// 昵称走回调而不是快照：用户改了昵称之后重发的摘要不该还挂着旧名。
	n.ShareApp.SelfName = func() string { return cfg.General.DisplayName }
	// 发布/取消后立刻广播一次：广播周期是 45 秒，等下一轮用户会以为没生效。
	n.ShareApp.Announce = bc.AnnounceNow
	// 广播携带本机共享摘要（旧版本客户端会忽略它，见 peer.Announcement.Shares）。
	bc.SetSharesFunc(n.ShareApp.Summaries)
	n.ShareApp.Start(ctx)

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

	n.Router = app.NewRouter(
		n.ChatApp, n.GroupApp, n.TransferApp, n.DiscApp, peerApp, n.ShareApp, lg)
	conn.SetHandler(n.Router.Handle)

	peerApp.Start()
	n.ChatApp.Start(ctx)
	n.DiscApp.Start(ctx)

	// 群名册同步：peer.online / peer.discovered 时推送与拉取，另有 60s 抽查兜底。
	// 少了它，「离线成员错过一次变更」就永久错过了 —— 被移出者仍以为在群里，
	// 被加入者连群都不存在（ADR-012 Decision §6）。
	n.GroupApp.Start(ctx)
	// 群主给自己名下的每个群重签一次名册：覆盖 RosterSig 布局变更的存量代价。
	// 放在这里而不是让用户手工触发，是因为「升级后旧签名验不过」必须自动消失。
	for _, g := range n.Groups.List() {
		if g.OwnerID != kp.NodeID() {
			continue
		}
		if _, err := n.GroupApp.Resign(ctx, g.ID); err != nil {
			lg.Warn("群名册重签失败", "group_id", g.ID, "err", err)
		}
	}

	// 文件传输的等待重试：对端上线（peer.online / peer.discovered）是主触发源，
	// Recover 负责收尾上次运行留下的任务并立刻试一轮等待中的那些。
	n.TransferApp.Start(ctx)
	n.TransferApp.Recover(ctx)

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
				n.TransferApp.SweepWaiting(ctx)
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

// defaultUDPPort 是 ADR-008 的基准发现端口。
//
// udp_port=0 必须落到它，而不是交给系统随机分配：广播发现要求收发双方
// 落在【同一个】端口上，而 ANNOUNCE 携带的端口只对单播有用 ——
// 随机端口意味着两台机器几乎必然错开，广播就永远收不到对方的 ANNOUNCE。
// （TCP 的 0=系统分配是安全的：对端从 announce 学到实际端口再拨号。）
const defaultUDPPort = 2425

func startBroadcast(ctx context.Context, bc *udp.Broadcaster, base, fallback int, peerApp *app.PeerApp) error {
	sink := func(an peer.Announcement) { _ = peerApp.OnAnnouncement(an) }

	if base == 0 {
		base = defaultUDPPort
	}
	if fallback < 1 {
		fallback = 1
	}

	// 广播目标覆盖整个回退区间：即便某台机器因 2425 被占而落在 2426，
	// 其他节点按 2425..2434 广播它也收得到 —— 这才兑现了
	// 「对端从 announce 学习、无需配置端口」对发现场景的承诺。
	ports := make([]int, 0, fallback)
	for i := 0; i < fallback; i++ {
		ports = append(ports, base+i)
	}
	bc.SetAnnouncePorts(ports)

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
