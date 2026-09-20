// Package harness 在单进程内装配一个完整的 SwarmLink 节点。
//
// 它是 test/e2e 与 test/scale 共用的基础：用【内存适配器】+ 真实 TCP/UDP
// （均绑定 127.0.0.1）跑通发现、握手、聊天、群聊与文件传输。
// 这正是「ConnManager 可被替换」与「领域层零 I/O」两条设计带来的测试红利。
package harness

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/filesink"
	"github.com/swarmlink/swarmlink/internal/adapters/keystore"
	"github.com/swarmlink/swarmlink/internal/adapters/net/tcp"
	"github.com/swarmlink/swarmlink/internal/adapters/net/udp"
	"github.com/swarmlink/swarmlink/internal/adapters/store/mem"
	"github.com/swarmlink/swarmlink/internal/app"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// Options 控制节点的装配参数。
type Options struct {
	// Seeds 是该节点配置的种子清单（"值得先问一声的地址"）。
	Seeds []peer.SeedAddr
	// TTL 是节点目录条目存活时间。
	TTL time.Duration
	// ChunkSize 是文件传输分块大小（测试用小分块以产生更多块）。
	ChunkSize int64
	// DisableBroadcast=true 时不启动广播（等价 seed_only / 隔离域）。
	DisableBroadcast bool
}

// Node 是一个装配完成的完整节点。
type Node struct {
	Name string
	KP   *identity.KeyPair

	Bus      *eventbus.Bus
	Messages *mem.Messages
	Peers    *mem.Peers
	Groups   *mem.Groups

	Conn     *tcp.Manager
	BC       *udp.Broadcaster
	Registry *udp.Registry
	Prober   *udp.Prober

	PeerApp     *app.PeerApp
	ChatApp     *app.ChatApp
	GroupApp    *app.GroupApp
	TransferApp *app.TransferApp
	DiscApp     *app.DiscoveryApp
	Router      *app.Router

	Sink        *filesink.Sink
	DownloadDir string

	clk    ports.Clock
	cancel context.CancelFunc
	ctx    context.Context
}

// New 装配并启动一个节点。
func New(name string, opts Options) (*Node, error) {
	kp, err := identity.GenerateKeyPair()
	if err != nil {
		return nil, err
	}
	clk := clock.New()
	bus := eventbus.New()

	messages := mem.NewMessages(clk)
	peers := mem.NewPeers(clk)
	groups := mem.NewGroups()

	downloadDir, err := os.MkdirTemp("", "swarmlink-dl-"+name+"-")
	if err != nil {
		return nil, err
	}
	sink, err := filesink.New(downloadDir)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	policy := tcp.DefaultPolicy()
	policy.HeartbeatInterval = time.Hour // 测试中不靠心跳推动
	policy.HeartbeatTimeout = time.Hour
	policy.IdleTimeout = time.Hour

	conn := tcp.NewManager(kp, tcp.DefaultHandshakeConfig(), policy, clk, nil, bus, nil)

	tcpAddr, err := conn.Start(ctx, "127.0.0.1:0")
	if err != nil {
		cancel()
		return nil, err
	}
	_, portStr, _ := net.SplitHostPort(tcpAddr)
	var tcpPort uint16
	if _, err := fmt.Sscanf(portStr, "%d", &tcpPort); err != nil {
		cancel()
		return nil, err
	}

	ttl := opts.TTL
	if ttl <= 0 {
		ttl = 300 * time.Second
	}

	peerApp := app.NewPeerApp(kp.NodeID(), peers, bus, clk, ttl, nil)
	chatApp := app.NewChatApp(kp.NodeID(), messages, conn, peers, bus, clk, nil)
	groupApp := app.NewGroupApp(kp.NodeID(), kp, groups, messages, conn, peers, bus, clk, nil)
	transferApp := app.NewTransferApp(kp.NodeID(), messages, sink, conn, peers, bus, clk, nil)
	if opts.ChunkSize > 0 {
		transferApp.ChunkSize = opts.ChunkSize
	}

	bcCfg := udp.DefaultConfig()
	bcCfg.DisplayName = name
	bcCfg.TCPPort = tcpPort
	bcCfg.SeedOnly = true // 测试中不依赖广播循环，仅用其 recv 能力响应探测

	bc := udp.NewBroadcaster(kp, bcCfg, clk, nil)
	lo := net.ParseIP("127.0.0.1").To4()
	bc.SetInterfaces([]udp.Interface{{
		Name: "lo0", IP: lo, Mask: net.CIDRMask(8, 32), Broadcast: lo,
	}})
	if err := bc.Start(ctx, func(an peer.Announcement) {
		_ = peerApp.OnAnnouncement(an)
	}); err != nil {
		cancel()
		conn.Stop()
		return nil, err
	}

	registry := udp.NewRegistry(opts.Seeds, clk)
	prober := &udp.Prober{Timeout: 2 * time.Second}

	discApp := app.NewDiscoveryApp(kp.NodeID(), peerApp, peers, conn, registry, prober, bus, clk, nil)
	discApp.LocalEpoch = bc.Epoch
	discApp.RefreshInterval = 300 * time.Second // 由测试显式调用 RefreshSeeds
	discApp.SeedsPerRefresh = 2

	router := app.NewRouter(chatApp, groupApp, transferApp, discApp, peerApp, nil)
	conn.SetHandler(router.Handle)

	peerApp.Start()
	chatApp.Start(ctx)

	n := &Node{
		Name: name, KP: kp,
		Bus: bus, Messages: messages, Peers: peers, Groups: groups,
		Conn: conn, BC: bc, Registry: registry, Prober: prober,
		PeerApp: peerApp, ChatApp: chatApp, GroupApp: groupApp,
		TransferApp: transferApp, DiscApp: discApp, Router: router,
		Sink: sink, DownloadDir: downloadDir,
		clk: clk, cancel: cancel, ctx: ctx,
	}
	return n, nil
}

// Context 返回节点生命周期上下文。
func (n *Node) Context() context.Context { return n.ctx }

// ID 返回 NodeID。
func (n *Node) ID() identity.NodeID { return n.KP.NodeID() }

// TCPPort 返回监听的 TCP 端口。
func (n *Node) TCPPort() uint16 {
	_, portStr, _ := net.SplitHostPort(n.Conn.LocalAddr())
	var p uint16
	_, _ = fmt.Sscanf(portStr, "%d", &p)
	return p
}

// UDPPort 返回绑定的 UDP 端口。
func (n *Node) UDPPort() int { return n.BC.UDPPort() }

// SeedAddr 返回可用于种子清单的地址（第一跳走 UDP）。
func (n *Node) SeedAddr() peer.SeedAddr {
	return peer.SeedAddr{IP: "127.0.0.1", UDPPort: uint16(n.BC.UDPPort())}
}

// Announcement 构造本节点的通告（供测试直接注入，模拟同广播域）。
//
// Link 用它把两个节点放进同一个「广播域」—— 相当于它们互相听到了广播。
func (n *Node) Announcement() peer.Announcement {
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])

	an := peer.Announcement{
		NodeID:      n.KP.NodeID(),
		DisplayName: n.Name,
		PublicKey:   n.KP.PublicKey(),
		TCPPort:     n.TCPPort(),
		UDPPort:     uint16(n.UDPPort()),
		Subnet:      "127.0.0.1/8",
		Nonce:       nonce,
		Timestamp:   time.Now().Unix(),
		Epoch:       1,
		Source:      peer.SourceBroadcast,
		ObservedIP:  "127.0.0.1",
	}
	an.Sig = n.KP.Sign(an.SigningBytes())
	return an
}

// Link 让 a 与 b 互相「听见」对方（模拟同网段广播发现）。
//
// 跨网段用例【不要】用 Link —— 那正是要通过种子才能互相看见的场景。
func Link(a, b *Node) error {
	if err := a.PeerApp.OnAnnouncement(b.Announcement()); err != nil {
		return err
	}
	return b.PeerApp.OnAnnouncement(a.Announcement())
}

// Tick 驱动一次后台维护：目录 TTL、outbox 重发、连接回收。
//
// 不使用真实定时器，测试因此是确定性的。
func (n *Node) Tick() {
	n.PeerApp.Sweep()
	n.ChatApp.SweepOutbox(n.ctx)
	n.GroupApp.SweepOutbox(n.ctx)
	n.Conn.Sweep()
}

// SetSeeds 重新配置种子清单。
//
// 测试里种子地址（UDP 端口）要等节点启动后才知道，因此提供这一注入点；
// 生产环境种子清单来自配置文件，在启动前即固定。
func (n *Node) SetSeeds(addrs []peer.SeedAddr) {
	n.Registry = udp.NewRegistry(addrs, n.clk)
	n.DiscApp.SetRegistry(n.Registry)
}

// Close 关闭节点并清理临时目录。
func (n *Node) Close() {
	n.cancel()
	n.Conn.Stop()
	_ = n.BC.Stop()
	_ = os.RemoveAll(n.DownloadDir)
}

// LoadOrCreateIdentity 暴露 keystore，便于需要持久身份的测试。
func LoadOrCreateIdentity(dir string) (*identity.KeyPair, error) { return keystore.LoadOrCreate(dir) }
