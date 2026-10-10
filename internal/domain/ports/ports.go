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
	"errors"
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
	State  peer.State
	Source string
	// OnlineOnly 只返回「在」的节点：online 与 discovered 都算 ——
	// 前者由连接层维护、后者由 announce 维护，把 discovered 排除掉会让
	// 「正在广播但还没握手」的节点在 UI 上凭空消失（ADR-009）。
	OnlineOnly bool
	// IncludeBlocked 让「被本地移除」的条目也出现在结果里（诊断用，默认不返回）。
	// 注意：State 显式指定为 blocked 时本字段无意义 —— 显式查询优先。
	IncludeBlocked bool
	Limit          int
}

// PeerDirectory 只回答「我听说过谁」，不回答「我现在连着谁」（那是 ConnManager）。
type PeerDirectory interface {
	Upsert(p peer.Peer) error
	Get(id identity.NodeID) (peer.Peer, bool)
	List(filter PeerFilter) []peer.Peer
	MarkOffline(id identity.NodeID, at time.Time) error
	// SetSuppressed 设置/清除「本地已移除」标记。
	//
	// 实现必须逐条满足下面的契约：两个仓储（sqlite / mem）互为替身，
	// 任何一条上分叉都会让「测试跑 mem、生产跑 sqlite」出现行为差异。
	//
	//   - suppressed=true：把该行标记为「已移除」，并把 at 写进 last_seen
	//     （「什么时候删的」要能查）。对任意当前状态都生效，包括 online。
	//   - suppressed=false：**只对当前确实被标记的行**生效，改回 discovered 并写 last_seen；
	//     对其它状态必须是**彻底空操作**（连 last_seen 也不动）。因此它不是 true 的真逆操作 ——
	//     旧状态（online / offline）不可恢复，需要精确状态的调用方随后自行 Upsert。
	//   - node_id 不存在：静默空操作（返回 nil），**绝不插入新行**。
	//   - at 必须是调用时刻：零值会静默把 last_seen 写成 0，破坏 SeenRecently 与 List 的排序。
	//
	// 标记效果：不再参与目录交换传播（见 DiscoveryApp.entries）；默认也不再出现在 List
	// （List 默认排除 blocked；显式 IncludeBlocked 或显式 State=blocked 查询除外 ——
	// 诊断类调用方要显式打开，否则会少统计被移除的条目）。
	//
	// 「恢复」不由本接口负责：app 层在该身份再次被验证到时（有效签名通告或握手成功）
	// 调用 SetSuppressed(id, false)，见 app.PeerApp。
	//
	// 完整设计见 docs/plans/2026-10-10-identity-vault-and-peer-removal-design.md §5。
	SetSuppressed(id identity.NodeID, suppressed bool, at time.Time) error
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

// ErrEmptyBatchID 表示调用方传了空的 batch_id。
//
// 单独一个哨兵错误而不是各实现自己写一句文案：调用方需要把「传了空 id」
// 这种不该发生的编程错误与「存储故障」分开处理，而两个存储实现也必须
// 给出同一种可判别的错误（否则上层只能靠比较错误字符串）。
var ErrEmptyBatchID = errors.New("transfer: empty batch id")

// TransferRepo 是传输任务持久化端口。
type TransferRepo interface {
	UpsertJob(j transfer.Job) error
	GetJob(jobID string) (transfer.Job, error)
	// FindResumable 找可续传的任务。relPath 非空时（文件夹批次）必须一起匹配。
	//
	// 为什么必须带上 rel_path：同一张文件夹里两份内容相同的文件 file_hash
	// 也相同，只按 (peer, file_hash) 匹配会让第二条命中第一条并继承它的
	// 临时文件与最终路径 —— 结果是目录里少一个文件、另一个内容错位。
	// relPath 为空（单文件传输）时保持旧的 (peer, file_hash) 语义。
	FindResumable(peerID identity.NodeID, fileHash, relPath string) (transfer.Job, bool)
	// SaveBitmap 批量落盘，避免每块一次事务（P1-9）。
	SaveBitmap(jobID string, bm transfer.ChunkBitmap, completed int64) error
	ListActive() ([]transfer.Job, error)
	// ListWaiting 返回「可以被自动拉起」的发送任务（重启恢复与周期性 sweep 用）。
	//
	// 取值集合来自 WaitReason.AutoStartable()：等待对方上线的，以及文件夹批次里
	// 排队的。用户取消过的任务（WaitReason 被清成 WaitNone）必须【不在】其中 ——
	// 被自动拉起就等于「取消了又自己跑起来」。
	//
	// 【不要】用 ListActive 代替：那个语义是「未结束」，把 failed/paused/
	// verifying 都算进来，拿它驱动自动重试会把不该重试的任务一起拉起来。
	ListWaiting() ([]transfer.Job, error)
	// ListByBatch 返回同一文件夹批次的全部任务，顺序为 rel_path 升序
	// （即目录树顺序），同路径再按 job_id。
	//
	// 为什么用 rel_path 而不是「创建时间」排序：批次是同一个循环里建的，
	// created_at 以毫秒落库、整批落在同一毫秒是常态，而 job_id 是 UUIDv7
	// （同毫秒内是随机尾）。拿它们排序得到的顺序虽然确定却【没有语义】——
	// 用户看到的文件次序会与他看到的目录结构无关。rel_path 是两端都能
	// 复现的目录树顺序，也是这个界面唯一有意义的顺序。
	//
	// 空 batchID 返回 ErrEmptyBatchID。
	//
	// 单独一个方法而不是复用 ListActive：批次聚合进度与「取消全部」都要看到
	// 已结束的任务 —— 用户在一个 32 个文件的文件夹卡片上看到的必须是完整的
	// 32 条，不能因为其中 30 条已经传完就只剩 2 条。
	//
	// batchID 由发送端生成且【必须全局唯一】（随机串，不是文件夹名、不是序号）：
	// 两个对端各发来一个同名批次时，卡片会把两个文件夹合并、取消也会连带停掉
	// 另一批。空 batchID 一律报错而不是「返回所有单文件任务」——后者会让
	// CancelBatch("") 变成「停掉本机全部单文件传输」。
	ListByBatch(batchID string) ([]transfer.Job, error)
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

	// 会话「清空点」：早于它的消息不再出现在聊天区（消息本身【不删除】）。
	//
	// 三个方法都按 conv_id 操作：单聊与群聊共用一份实现（单聊 conv_id 含冒号、
	// 群 ID 是纯 hex，形态天然不相交）。
	//
	// SetConvCleared 写入/覆盖清空点：用户清空后又聊几句再清空一次时，
	// 边界必须【往前推】，所以覆盖是必须的语义而不是意外。
	SetConvCleared(convID string, at time.Time) error
	// ConvClearedAt 读取清空点；从未清空时返回 ok=false（这不是错误）。
	ConvClearedAt(convID string) (time.Time, bool, error)
	// RemoveConvClear 撤销清空（幂等：没有记录也返回 nil）。
	RemoveConvClear(convID string) error
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

// SinkRequest 是一次「落在目录树里」的写入请求（文件夹批次专用）。
//
// 单独一个结构体而不是给 Create 加参数：单文件路径的行为一个字都不能变
// —— 那是已经上线并测过的路径，参数一改，所有实现与替身都要跟着动。
type SinkRequest struct {
	JobID string
	// BatchID 是批次标识，同时决定落盘的根目录：同一个批次只分配一次根
	// （「照片」已存在则整批退到「照片 (1)」），因此【必须全局唯一】，
	// 不能是文件夹名。
	BatchID string
	// RelPath 是相对文件夹根的路径（含根名，如 `照片/2024/a.jpg`）。
	RelPath string
	// IsDir 表示这是一个空目录条目：只建目录、不建临时文件。
	IsDir bool
}

// FileSink 把 G3 的路径清洗收敛到一个实现里。
type FileSink interface {
	// Create 返回临时文件句柄 + 最终路径；实现内做全部清洗。
	Create(jobID, rawName string) (SinkHandle, error)
	// CreateIn 按相对路径把文件写进批次目录树里。
	//
	// 与 Create 的差别不只是「多了几层」：它还要做逐段清洗，并且为整个批次
	// 只分配一次根目录 —— 否则同名文件夹被抢到时，一批文件会散落在
	// 「照片」与「照片 (1)」两个目录里。
	CreateIn(req SinkRequest) (SinkHandle, error)
	// AdoptRoot 认领一个已经分配过的批次根目录（重启恢复用）。
	//
	// 不认领的后果：进程重启后续传会重新分配出「照片 (1)」，把一个文件夹
	// 拆成两份，用户看到的是「文件少了一半」。
	AdoptRoot(batchID, absPath string)
	// Commit 原子替换为最终文件。
	Commit(h SinkHandle) (finalPath string, err error)
	// Abort 清理临时文件。
	Abort(h SinkHandle) error
}
