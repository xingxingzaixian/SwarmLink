// Package udp 实现 UDP 发现适配器：网段内广播 + 签名 ANNOUNCE + 种子单播。
package udp

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
)

// Config 是发现配置（对应 7.4 节 TOML [discovery]）。
type Config struct {
	DisplayName      string
	TCPPort          uint16
	UDPPort          int
	AnnounceInterval time.Duration
	JitterRatio      float64
	EnableMulticast  bool
	MulticastAddr    string
	AllowInterfaces  []string
	DenyInterfaces   []string
	// SeedOnly=true 时关闭自动发现，只用种子（VPN / 安全敏感环境）。
	SeedOnly bool
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		AnnounceInterval: 5 * time.Second,
		JitterRatio:      0.2,
		DenyInterfaces:   []string{"utun*", "vmnet*", "vboxnet*", "docker*"},
	}
}

// Broadcaster 实现 ports.DiscoveryStrategy（广播发现）。
type Broadcaster struct {
	kp  *identity.KeyPair
	cfg Config
	clk ports.Clock
	lg  *slog.Logger

	conn   *net.UDPConn
	port   int
	ifaces []Interface
	// forced 非空时跳过接口枚举（测试注入用）。
	forced []Interface

	epoch atomic.Uint64

	sinkMu sync.RWMutex
	sink   func(peer.Announcement)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var _ ports.DiscoveryStrategy = (*Broadcaster)(nil)

// NewBroadcaster 构造广播发现器。
func NewBroadcaster(kp *identity.KeyPair, cfg Config, clk ports.Clock, lg *slog.Logger) *Broadcaster {
	return &Broadcaster{kp: kp, cfg: cfg, clk: clk, lg: lg}
}

// Name 返回策略名。
func (b *Broadcaster) Name() string { return "broadcast" }

// UDPPort 返回实际绑定的 UDP 端口（0 表示自动分配，需在 Start 后读取）。
func (b *Broadcaster) UDPPort() int { return b.port }

// Epoch 返回当前目录 epoch。
func (b *Broadcaster) Epoch() uint64 { return b.epoch.Load() }

// BumpEpoch 递增目录 epoch 并返回新值（本地目录变化时调用，使对端重新拉取）。
func (b *Broadcaster) BumpEpoch() uint64 { return b.epoch.Add(1) }

// Start 枚举接口、绑定接收 socket 并启动收发循环。
func (b *Broadcaster) Start(ctx context.Context, sink func(peer.Announcement)) error {
	b.ctx, b.cancel = context.WithCancel(ctx)
	b.sink = sink
	b.epoch.Store(1)

	ifaces, err := EnumerateInterfaces(b.cfg.AllowInterfaces, b.cfg.DenyInterfaces)
	if err != nil {
		return err
	}
	if b.forced != nil {
		ifaces = b.forced
	}
	b.ifaces = ifaces

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: b.cfg.UDPPort})
	if err != nil {
		return fmt.Errorf("udp: listen %d: %w", b.cfg.UDPPort, err)
	}
	b.conn = conn
	b.port = conn.LocalAddr().(*net.UDPAddr).Port

	b.wg.Add(1)
	go b.recvLoop()

	if !b.cfg.SeedOnly && len(b.ifaces) > 0 {
		b.wg.Add(1)
		go b.announceLoop()
	}

	if b.lg != nil {
		names := make([]string, 0, len(b.ifaces))
		for _, i := range b.ifaces {
			names = append(names, i.Name+"("+i.SubnetString()+")")
		}
		b.lg.Info("discovery started", "udp_port", b.port, "interfaces", names, "seed_only", b.cfg.SeedOnly)
	}
	return nil
}

// Stop 停止发现并关闭 socket。
func (b *Broadcaster) Stop() error {
	if b.cancel != nil {
		b.cancel()
	}
	if b.conn != nil {
		_ = b.conn.Close()
	}
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	return nil
}

// Interfaces 返回参与发现的接口（诊断面板用）。
func (b *Broadcaster) Interfaces() []Interface { return b.ifaces }

// SetInterfaces 覆盖接口列表。仅用于测试注入（生产由 EnumerateInterfaces 决定）。
func (b *Broadcaster) SetInterfaces(ifaces []Interface) { b.forced = ifaces }

// SetUDPPort 设置将要绑定的 UDP 端口（0 = 自动分配）。必须在 Start 之前调用。
//
// 用途：ADR-008 的端口回退 —— 2425 被占则试 2426…2434。
// 实际端口会写进 ANNOUNCE 供对端学习，因此对端无需配置。
func (b *Broadcaster) SetUDPPort(port int) { b.cfg.UDPPort = port }

func (b *Broadcaster) announceLoop() {
	defer b.wg.Done()
	for {
		wait := jitter(b.cfg.AnnounceInterval, b.cfg.JitterRatio)
		select {
		case <-b.ctx.Done():
			return
		case <-b.clk.After(wait):
			b.sendAnnounces()
		}
	}
}

func (b *Broadcaster) recvLoop() {
	defer b.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, src, err := b.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-b.ctx.Done():
				return
			default:
			}
			continue
		}

		typ, body, err := protocol.DecodeUDP(buf[:n])
		if err != nil {
			continue // 非本协议报文：静默丢弃
		}

		switch typ {
		case protocol.UDPTypeAnnounce:
			var w protocol.AnnounceWire
			if protocol.DecodeJSON(body, &w) != nil {
				continue
			}
			a, err := w.Announcement()
			if err != nil {
				continue
			}
			// 自发现过滤：UDP 广播默认回环到自己
			if a.NodeID == b.kp.NodeID() {
				continue
			}
			if !identity.Verify(a.PublicKey, a.SigningBytes(), a.Sig) {
				continue
			}
			a.Source = peer.SourceBroadcast
			if src != nil {
				a.ObservedIP = src.IP.String()
			}
			if h := b.getSink(); h != nil {
				h(a)
			}
		case protocol.UDPTypeSeedProbe:
			// 任意节点都回应探测（零特权、无放大：回包与请求同量级）。
			b.replyAnnounce(src)
		}
	}
}

func (b *Broadcaster) getSink() func(peer.Announcement) {
	b.sinkMu.RLock()
	defer b.sinkMu.RUnlock()
	return b.sink
}

// sendAnnounces 每个接口发一份 ANNOUNCE（各自携带该接口的 subnet，便于 P-2 检测）。
func (b *Broadcaster) sendAnnounces() {
	for _, iface := range b.ifaces {
		payload, err := protocol.EncodeUDP(protocol.UDPTypeAnnounce, protocol.NewAnnounceWire(b.buildAnnounce(iface.SubnetString())))
		if err != nil {
			continue
		}
		// 以接口 IP 作为源地址，强制报文从该网卡发出（可移植的等价于绑定接口）。
		conn, err := net.DialUDP("udp4",
			&net.UDPAddr{IP: iface.IP, Port: 0},
			&net.UDPAddr{IP: iface.Broadcast, Port: b.port},
		)
		if err != nil {
			continue
		}
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, _ = conn.Write(payload)
		_ = conn.Close()
	}
}

func (b *Broadcaster) replyAnnounce(src *net.UDPAddr) {
	// subnet 必须尽量填上：P-2（地址段重叠）检测完全依赖它。
	// 若探测源不在任何本机子网内（如从回环探测），退回首个网卡的子网，
	// 也好过留空 —— 空值会让对端失去这一唯一的冲突检测信号。
	subnet := ""
	for _, iface := range b.ifaces {
		if iface.Contains(src.IP) {
			subnet = iface.SubnetString()
			break
		}
	}
	if subnet == "" && len(b.ifaces) > 0 {
		subnet = b.ifaces[0].SubnetString()
	}
	payload, err := protocol.EncodeUDP(protocol.UDPTypeAnnounce, protocol.NewAnnounceWire(b.buildAnnounce(subnet)))
	if err != nil {
		return
	}
	_, _ = b.conn.WriteToUDP(payload, src)
}

func (b *Broadcaster) buildAnnounce(subnet string) peer.Announcement {
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])

	a := peer.Announcement{
		NodeID:      b.kp.NodeID(),
		DisplayName: b.cfg.DisplayName,
		PublicKey:   b.kp.PublicKey(),
		TCPPort:     b.cfg.TCPPort,
		UDPPort:     uint16(b.port),
		Subnet:      subnet,
		Nonce:       nonce,
		Timestamp:   b.clk.Now().Unix(),
		Epoch:       b.epoch.Load(),
		Source:      peer.SourceBroadcast,
	}
	a.Sig = b.kp.Sign(a.SigningBytes())
	return a
}

// jitter 给周期加上 ±ratio 的随机抖动，避免多节点开机同步风暴。
func jitter(d time.Duration, ratio float64) time.Duration {
	if ratio <= 0 {
		return d
	}
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return d
	}
	f := float64(n.Int64())/1_000_000*2 - 1 // [-1, 1)
	return time.Duration(float64(d) * (1 + f*ratio))
}
