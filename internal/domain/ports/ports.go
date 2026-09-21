// Package ports 集中定义全部跨模块接口（依赖倒置锚点，ADR-007）。
//
// 存在意义：app 依赖 ports，adapters 实现 ports，双方都不知道对方存在。
// Go 的隐式接口让这一步几乎零成本。
//
// 设计要点（架构书 3.4）：
//   - Session.Recv() 用阻塞式而非 channel（TCP 天然阻塞，便于统一 ctx 取消，不泄漏 channel）
//   - FileSink 把 G3 路径清洗收敛到一个接口实现里，清洗规则只有一个测试点
//   - Clock 让心跳超时、TTL 过期、退避全部可测
//   - ConnManager 是最值钱的接口：测试里换成内存实现即可单进程跑多节点
package ports

import (
	"context"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/group"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
)

// ---------------------------------------------------------------------------
// 时钟
// ---------------------------------------------------------------------------

// Ticker 抽象周期性定时器。
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Clock 是系统唯一时间来源。
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	NewTicker(d time.Duration) Ticker
}

// ---------------------------------------------------------------------------
// 事件总线
// ---------------------------------------------------------------------------

// EventBus 是同步发布/订阅（仅用于通知，不承载需要返回值或事务的路径）。
type EventBus interface {
	Publish(topic string, payload any)
	Subscribe(topic string, fn func(any)) (unsubscribe func())
}

// ---------------------------------------------------------------------------
// 节点目录
// ---------------------------------------------------------------------------

// PeerFilter 是目录查询过滤条件。
type PeerFilter struct {
	State      peer.State
	Source     string
	OnlineOnly bool
	Limit      int
}

// PeerDirectory 只回答「我听说过谁」，不回答「我现在连着谁」（那是 ConnManager）。
type PeerDirectory interface {
	Upsert(p peer.Peer) error
	Get(id identity.NodeID) (peer.Peer, bool)
	List(filter PeerFilter) []peer.Peer
	MarkOffline(id identity.NodeID, at time.Time) error
	// SeenRecently 供发现层做重复抑制。
	SeenRecently(id identity.NodeID, window time.Duration) bool
}

// ---------------------------------------------------------------------------
// 消息仓储
// ---------------------------------------------------------------------------

// MessageRepo 是消息持久化端口。
type MessageRepo interface {
	Append(m message.Message) error
	// AppendIfAbsent 幂等写入：msg_id 已存在则返回 inserted=false。
	// 调用方据此决定 UI 是否更新，但【无论是否重复都必须回 ACK】。
	AppendIfAbsent(m message.Message) (inserted bool, err error)
	// Get 按 msg_id 读取消息（outbox 重发时需要取回原文）。
	Get(msgID string) (message.Message, bool)
	// Latest 按 conv 取最近 limit 条；before 为游标（毫秒时间戳，0 表示最新）。
	Latest(convID string, limit int, before int64) ([]message.Message, error)
	MarkDelivered(msgID string) error
}

// ---------------------------------------------------------------------------
// 传输任务仓储
// ---------------------------------------------------------------------------

// TransferRepo 是传输任务持久化端口。
type TransferRepo interface {
	UpsertJob(j transfer.Job) error
	GetJob(jobID string) (transfer.Job, error)
	FindResumable(peerID identity.NodeID, fileHash string) (transfer.Job, bool)
	// SaveBitmap 批量落盘，避免每块一次事务（P1-9）。
	SaveBitmap(jobID string, bm transfer.ChunkBitmap, completed int64) error
	ListActive() ([]transfer.Job, error)
	// ListRecent 返回最近更新的若干任务，【包含】已结束的。
	//
	// 与 ListActive 必须分开：ListActive 只服务「续传 / 还有哪些在跑」，
	// 因此排除 done/cancelled；而界面的传输记录必须能看到已完成的条目，
	// 拿 ListActive 当数据源会让「刚传完的文件凭空消失」。
	ListRecent(limit int) ([]transfer.Job, error)
	// PurgeFinished 清理已结束的任务，返回删除条数。
	//
	// 保留规则：永远保留最近的 keep 条（keep<=0 表示一条都不留），
	// 且只清理 updated_at 早于 olderThan 的。【正在进行】的任务一律不动。
	PurgeFinished(keep int, olderThan time.Time) (int, error)
}

// ---------------------------------------------------------------------------
// outbox（G4：崩溃后可恢复重发）
// ---------------------------------------------------------------------------

// OutboxEntry 是一条待确认消息。
type OutboxEntry struct {
	MsgID     string
	ConvID    string
	Attempts  int
	NextTryAt time.Time
}

// OutboxRepo 是未确认消息队列端口。
// 注意：Enqueue 必须与消息写入在同一事务内，由 app 层通过 MessageRepo 的事务接口保证。
type OutboxRepo interface {
	Enqueue(msgID, convID string, nextTry time.Time) error
	Due(now time.Time, limit int) ([]OutboxEntry, error)
	Delete(msgID string) error
	BumpAttempt(msgID string, nextTry time.Time) error
	ListByConv(convID string) ([]OutboxEntry, error)
}

// ---------------------------------------------------------------------------
// 消息 + outbox 的原子写入（ADR-004）
// ---------------------------------------------------------------------------

// ChatStore 组合消息与 outbox，提供跨表原子操作。
//
// 存在理由：ADR-004 要求「messages(pending) + outbox」必须【单事务】写入，
// 否则进程崩溃会出现「消息没存但 outbox 有」或反之的不一致。
// 事件总线不能用于需要事务的路径，因此由 ChatApp 显式编排本接口。
type ChatStore interface {
	MessageRepo
	OutboxRepo
	// AppendOutgoing 单事务写入出站消息（state=pending）与 outbox 记录。
	AppendOutgoing(m message.Message, nextTry time.Time) error
	// Deliver 单事务把消息置为 delivered 并删除对应 outbox 记录。
	Deliver(msgID string) error
}

// ---------------------------------------------------------------------------
// 群仓储
// ---------------------------------------------------------------------------

// GroupRepo 是群组持久化端口。
type GroupRepo interface {
	Upsert(g group.Group) error
	Get(groupID string) (group.Group, bool)
	List() []group.Group
}

// ---------------------------------------------------------------------------
// 连接管理
// ---------------------------------------------------------------------------

// ConnManager 管理 TCP 会话（按需拨号 + 会话级保活 + 空闲回收）。
// 测试里替换为内存实现即可在单进程内跑多个虚拟节点。
type ConnManager interface {
	Dial(ctx context.Context, id identity.NodeID, addr string) (Session, error)
	Accept(ctx context.Context) (Session, error)
	SessionOf(id identity.NodeID) (Session, bool)
	Broadcast(f protocol.Frame, ids ...identity.NodeID) error
	Close(id identity.NodeID) error
}

// Session 是一条已建立（握手通过）的连接。
type Session interface {
	PeerID() identity.NodeID
	Send(f protocol.Frame) error
	// Recv 阻塞读取一帧；ctx 取消即返回。
	Recv() (protocol.Frame, error)
	// RTT 由心跳测得，供滑动窗口自适应。
	RTT() time.Duration
	Close() error
}

// ---------------------------------------------------------------------------
// 发现策略（可插拔）
// ---------------------------------------------------------------------------

// DiscoveryStrategy 是可插拔的发现方式（广播 / 组播 / 种子 / 未来 mDNS）。
type DiscoveryStrategy interface {
	Name() string
	Start(ctx context.Context, sink func(peer.Announcement)) error
	Stop() error
}

// ---------------------------------------------------------------------------
// 种子注册表（ADR-011 / 4.5.1）
// ---------------------------------------------------------------------------

// SeedRegistry 管理种子清单。
//
// 关键：种子不拥有任何特权接口。它产出的就是普通的 peer.Announcement，
// 与广播产出的结构完全同构 → 走同一条 Upsert 路径进入 PeerDirectory。
// 若这里出现 "Relay" / "Authoritative" 之类的方法，说明设计已经走形。
type SeedRegistry interface {
	// Pick 返回本轮该问的 N 颗种子（随机，避免 200 节点集中打同一颗）。
	Pick(n int) []peer.SeedAddr
	// Report 回填探测/拉取结果；learned == nil 表示失败，据此累计 fail_cnt。
	Report(addr peer.SeedAddr, learned *peer.Announcement, err error)
}

// SeedProber 执行种子第一跳（UDP 探测 → 种子单播回 ANNOUNCE）。
//
// 定义在 ports 而非直接使用 udp.Prober，是为了让 app 层不依赖具体适配器
// （依赖方向保持 app → ports，实现由 adapters/net/udp 提供）。
type SeedProber interface {
	Probe(ctx context.Context, addr peer.SeedAddr) (*peer.Announcement, error)
}

// ---------------------------------------------------------------------------
// 文件落盘（接收侧，含 G3 路径清洗）
// ---------------------------------------------------------------------------

// SinkHandle 是一次文件写入的句柄。
type SinkHandle struct {
	JobID     string
	TempPath  string
	FinalPath string
}

// FileSink 把 G3 的路径清洗收敛到一个实现里。
type FileSink interface {
	// Create 返回临时文件句柄 + 最终路径；实现内做全部清洗。
	Create(jobID, rawName string) (SinkHandle, error)
	// Commit 原子替换为最终文件。
	Commit(h SinkHandle) (finalPath string, err error)
	// Abort 清理临时文件。
	Abort(h SinkHandle) error
}
