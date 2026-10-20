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
	"github.com/swarmlink/swarmlink/internal/domain/share"
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

	// AnnouncePorts 是广播 ANNOUNCE 的【目标端口集合】。
	//
	// 为什么是集合而不是一个端口：ADR-008 允许 udp_port 被占时回退
	// （2425→2426…），而 ANNOUNCE 里携带的端口只对【单播】有用 ——
	// 广播的接收方必须恰好监听在发送方选择的端口上。若只发自己的端口，
	// 两台机器一旦回退到不同端口就永远互相看不见
	// （这正是「udp_port=0 时默认发现失效」的根因）。
	// 为空时退化为只发自己的端口（兼容旧调用方与测试）。
	AnnouncePorts []int

	// Shares 返回本机共享摘要（FR-14），由组合根注入。
	//
	// 为什么是回调而不是值：摘要会随发布/取消变化，而 Config 在 Start 之前
	// 就固定了 —— 传值的话「发布完别人看不到」会一直到重启。回调也让
	// udp 适配器不必认识 app 层（只依赖 domain/share 这个值类型）。
	//
	// 为 nil 时不携带任何摘要（旧行为）。
	Shares func() []share.Summary

	// AppVersion 是本机应用版本，随 ANNOUNCE 广播出去（局域网更新用）。
	//
	// 这里是【值】而不是回调：版本在一次进程生命周期内不可能变化，
	// 与 Shares 那种会随用户操作变的东西不同。
	// 空串时字段被 omitempty 省略，对端按 0.0.0 处理（永不作为更新源）。
	AppVersion string
}

// DefaultConfig 返回默认配置。
//
// 通告周期 45s ± 33% → 实际落在 30~60s：局域网上几十个节点时，
// 这个量级既能让新节点在一分钟内被发现，又把纯保活流量压到可忽略
// （对比 5s 一轮：200 节点 ≈ 40 pkt/s 的常驻噪声，换来的只是「更早几秒被看到」）。
// 代价是「对方异常掉线」的感知变慢，因此配了 BYE 主动下线（见 UDPTypeBye）——
// 正常关闭立即通知，只有崩溃/断电才回退到 TTL 兜底。
func DefaultConfig() Config {
	return Config{
		AnnounceInterval: 45 * time.Second,
		JitterRatio:      0.33,
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
	// stopOnce 保证 Stop 只完整执行一次：BYE 只能发一次（发完就退出了），
	// 重复执行会变成「已经下线了还在广播」的噪声。
	stopOnce sync.Once
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

// Stop 先宣告下线（BYE），再停止发现并关闭 socket。
//
// 顺序不能反：BYE 要趁 socket 与网卡信息都还在的时候发出去。它是
// 「优雅退出」与「崩溃」之间的唯一区别 —— 没有它，对端只能等 TTL 过期。
func (b *Broadcaster) Stop() error {
	b.stopOnce.Do(func() {
		b.sendByes()
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
	})
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

// SetSharesFunc 注入「本机共享摘要」的取值回调。可在 Start 之后调用。
//
// 与 SetAnnouncePorts 同样是【晚绑定】：共享清单由 app 层维护，而
// 广播器在 app 层之前就构造好了（组合根的顺序约束，见 bootstrap）。
func (b *Broadcaster) SetSharesFunc(fn func() []share.Summary) { b.cfg.Shares = fn }

// AnnounceNow 立刻广播一次 ANNOUNCE（不等下一个周期）。
//
// 用途：共享发布/取消后让对端在秒级看见（周期是 45 秒，用户会以为没生效）。
// 未启动（没有网卡）时是空操作 —— 这时本来也无处可发。
//
// 刻意不并发：调用点在「用户点了发布」这条路径上，多等一次 UDP 写
// （微秒级）换来「顺序可推理」是划算的。
func (b *Broadcaster) AnnounceNow() {
	if b.ctx == nil || b.cfg.SeedOnly || len(b.ifaces) == 0 {
		return
	}
	b.sendAnnounces()
}

// SetAnnouncePorts 设置广播 ANNOUNCE 的目标端口集合。必须在 Start 之前调用。
//
// 见 Config.AnnouncePorts：它让「本机端口被占而回退到非基准端口」的机器
// 依然能被按基准端口广播的其他节点发现。
func (b *Broadcaster) SetAnnouncePorts(ports []int) { b.cfg.AnnouncePorts = ports }

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
		case protocol.UDPTypeAnnounce, protocol.UDPTypeBye:
			a, ok := b.decodeAnnounce(body)
			if !ok {
				continue
			}
			// 语义差别只在这里：BYE 表示对方正在退出，接收方应立即判离线。
			a.Leaving = typ == protocol.UDPTypeBye
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

// decodeAnnounce 解析并验签一条 ANNOUNCE / BYE 载荷。
//
// 两者载荷完全同构（BYE 也带签名），因此共用这一条校验路径 ——
// 若是给 BYE 单独写一套解析，最可能漏掉的就是验签：那会让任何人
// 凭一个伪造的 BYE 把别人从节点列表里踢下线。
func (b *Broadcaster) decodeAnnounce(body []byte) (peer.Announcement, bool) {
	var w protocol.AnnounceWire
	if protocol.DecodeJSON(body, &w) != nil {
		return peer.Announcement{}, false
	}
	a, err := w.Announcement()
	if err != nil {
		return peer.Announcement{}, false
	}
	// 自发现过滤：UDP 广播默认回环到自己
	if a.NodeID == b.kp.NodeID() {
		return peer.Announcement{}, false
	}
	if !identity.Verify(a.PublicKey, a.SigningBytes(), a.Sig) {
		return peer.Announcement{}, false
	}
	// 身份自洽：公钥必须真的对应它声称的 node_id。
	//
	// 只验签是不够的 —— 签名与「报文自带的公钥」确实配套，所以任何人都能用自己
	// 的一套密钥去声称别人的 node_id，把目录塞满自相矛盾的条目（发现协议设计书
	// §15.1）。校验口径与 TCP 握手侧完全一致（handshake.go:141 与 :222）。
	//
	// ANNOUNCE 与 BYE 共用本函数，因此这一行同时挡住「伪造他人的下线通知」——
	// 那本来是本函数注释里点名的、最不能漏掉验签的一条路径。
	if identity.Fingerprint(a.PublicKey) != a.NodeID {
		return peer.Announcement{}, false
	}
	return a, true
}

func (b *Broadcaster) getSink() func(peer.Announcement) {
	b.sinkMu.RLock()
	defer b.sinkMu.RUnlock()
	return b.sink
}

// byeRounds 是 BYE 的重发次数。
//
// UDP 不保证送达，而这是节点发出的【最后一个】报文 —— 退出之后就再也没有
// 下一次了，因此值得多发两遍（间隔 20ms）。三遍足以覆盖单个报文丢失，
// 又不会让退出流程慢到用户能察觉。
const byeRounds = 3

// sendAnnounces 每个接口发一份 ANNOUNCE（各自携带该接口的 subnet，便于 P-2 检测）。
func (b *Broadcaster) sendAnnounces() { b.broadcastTo(protocol.UDPTypeAnnounce) }

// sendByes 向所有网卡宣告「我要下线了」。
//
// 只在真正参与过广播的节点上发：SeedOnly 模式本就不广播，
// 未启动过（ifaces 为空）时也没有网卡可发。
func (b *Broadcaster) sendByes() {
	if b.cfg.SeedOnly || len(b.ifaces) == 0 {
		return
	}
	// 打一行日志：退出流程出问题时，「到底有没有发出下线通知」是第一个要确认的事实。
	// 不重复打 node_id —— logger 上下文里已经有了。
	if b.lg != nil {
		b.lg.Info("broadcast bye", "interfaces", len(b.ifaces), "rounds", byeRounds)
	}
	for i := 0; i < byeRounds; i++ {
		b.broadcastTo(protocol.UDPTypeBye)
		if i < byeRounds-1 {
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// broadcastTo 给每个参与发现的网卡发一份指定类型的通告报文。
//
// 目标端口取 cfg.AnnouncePorts（通常是基准端口的整个回退区间），而不是只发
// 自己绑定的那个：否则「某台机器端口被占而回退到 2426」后，仍按 2425 广播的
// 节点永远到不了它。端口上没有监听者时会收到 ICMP 不可达 —— 每次都新建
// socket 且忽略写错误，因此不会出现连接被置为错误态的连带问题。
func (b *Broadcaster) broadcastTo(typ byte) {
	targets := b.cfg.AnnouncePorts
	if len(targets) == 0 {
		targets = []int{b.port}
	}
	for _, iface := range b.ifaces {
		payload, err := protocol.EncodeUDP(typ, protocol.NewAnnounceWire(b.buildAnnounce(iface.SubnetString())))
		if err != nil {
			continue
		}
		for _, port := range targets {
			// 以接口 IP 作为源地址，强制报文从该网卡发出（可移植的等价于绑定接口）。
			conn, err := net.DialUDP("udp4",
				&net.UDPAddr{IP: iface.IP, Port: 0},
				&net.UDPAddr{IP: iface.Broadcast, Port: port},
			)
			if err != nil {
				continue
			}
			_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_, _ = conn.Write(payload)
			_ = conn.Close()
		}
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
		// 版本号在签名【之前】填入但不参与签名域（见 peer.Announcement.AppVersion）。
		AppVersion: b.cfg.AppVersion,
	}
	// 摘要在签名【之前】填好但【不参与】签名域（见 peer.Announcement.Shares）：
	// 这样旧版本收到它照常验签通过，新版本能读到它。
	if b.cfg.Shares != nil {
		a.Shares = share.NormalizeSummaries(b.cfg.Shares())
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
