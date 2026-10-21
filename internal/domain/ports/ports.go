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
	"github.com/swarmlink/swarmlink/internal/domain/share"
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
	// 注意：State 非空时本字段无意义 —— 显式查询优先。
	IncludeBlocked bool
	Limit          int
}

// PeerDirectory 只回答「我听说过谁」，不回答「我现在连着谁」（那是 ConnManager）。
type PeerDirectory interface {
	Upsert(p peer.Peer) error
	Get(id identity.NodeID) (peer.Peer, bool)
	List(filter PeerFilter) []peer.Peer
	MarkOffline(id identity.NodeID, at time.Time) error
	// ResetLiveness 清零【上次运行残留】的在线状态：把所有 state='online' 的条目降级
	// 为 'discovered'，返回被降级的条目数。这是一次性启动清理，由组合根在装配后、
	// 服务对外提供能力之前调用一次。
	//
	// 为什么必须由存储层批量完成、而不是让上层逐条 Upsert：后者会顺带刷新 last_seen，
	// 把「已失联」误判成「刚刚见过」。契约（两个仓储实现必须逐条一致）：
	//   - 只动 online：discovered / offline / blocked 一律不动（blocked 是粘性状态）；
	//   - **保留 last_seen**：TTL 兜底据此判断该条目是否已过期 —— 这是它与
	//     MarkOffline / SetSuppressed 刻意不同的地方；
	//   - 幂等：没有 online 条目时返回 (0, nil)，node_id 不存在不影响结果。
	//
	// 为什么需要它：进程刚启动时不可能存在任何已握手的 TCP 会话，因此目录里的 online
	// 一律不可信。典型来源是「本机上次退出时对端在线，之后自己离线、对端也离线，那条
	// offline 事件永远到不了本机」—— 不清零就会复现「重启后已经离线的对端仍显示在线」
	// （ADR-009 的代价）。降级后对端若真在线，其 announce 会刷新 last_seen；若已离线，
	// last_seen 早已过期，下一次 Sweep 即标记 offline。
	ResetLiveness() (int, error)
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
	// 「恢复」不由本接口负责：app.PeerApp 在该身份再次被验证到时调用 SetSuppressed(id, false) ——
	// 有效签名通告（ANNOUNCE / BYE）在 OnAnnouncement 里，握手成功在 TopicPeerOnline 的处理里；
	// 目录交换的第三方转述【不】恢复（它没有签名）。
	//
	// 完整设计见 docs/plans/2026-10-10-identity-vault-and-peer-removal-design.md §5。
	SetSuppressed(id identity.NodeID, suppressed bool, at time.Time) error
	// SeenRecently 供发现层做重复抑制。
	SeenRecently(id identity.NodeID, window time.Duration) bool
}

// ---------------------------------------------------------------------------
// 消息仓储
// ---------------------------------------------------------------------------

// DefaultSearchLimit 是搜索单页的默认条数（调用方给 0 时实现取此值）。
const DefaultSearchLimit = 50

// MessageSearchQuery 是「本机消息历史」的全局搜索参数（ADR-013）。
//
// 性能纪律（实测数据见 ADR-013，勿改）：
//
//	搜索 = LIKE 子串 + `ORDER BY id DESC LIMIT n` 的【尾优先倒扫】。
//	20 万条消息 / 20 个会话下首屏 1.1ms（查询计划里没有 TEMP B-TREE：
//	SQLite 从 rowid 尾部倒着扫，凑够 n 条即停）。
//	因此实现【禁止】出现 count(*) 与 GROUP BY conv_id —— 它们在同样的
//	数据量下分别是 213ms 与 1.9s，本方案的成立与否就压在这一点上。
//
// 另一条纪律：不要引入 FTS5 替代 LIKE。trigram 分词器要求查询词 >= 3 个字符，
// 中文两字词（会议/照片/周报）会【静默返回 0 命中】；unicode61 对 CJK 无效
// （整段当一个 token）。LIKE 子串是唯一同时支持「任意长度 / 两字中文 /
// 英文大小写不敏感」的选择。
type MessageSearchQuery struct {
	// Text 是用户输入的待查子串（调用方负责去掉首尾空白；空串 = 无查询）。
	//
	// 实现【必须】转义 LIKE 的元字符（\ % _）并配 ESCAPE 使用：漏掉转义时
	// 搜一个 "%" 会命中全库，而这是最容易漏、也最难在界面上看出错的一条。
	Text string
	// ConvIDs 是会话白名单（由调用方派生，见 wails.SearchService）。
	//
	// 空切片 = 【没有任何可见会话】→ 实现返回空结果，而不是退化成全库搜索：
	// 已被本地移除的对端、已解散的群，其消息不应该因为调用方漏传白名单而
	// 突然出现在搜索结果里。
	ConvIDs []string
	// Limit 是单页上限；<= 0 时实现取 DefaultSearchLimit。
	Limit int
	// BeforeMsgID 是分页游标：只返回比这条【更旧】的命中。空串 = 首页。
	//
	// 为什么游标是 msg_id 而不是 sent_at：同一毫秒内写入多条是常态（连发、
	// 群发、入站消息共用发送方时间戳），按时间戳翻页会把边界上同毫秒的其它
	// 命中整页丢掉（既不重复也不遗漏这条要求，只有按唯一且单调的写入序才能
	// 满足）。实现内部把 msg_id 换算成自己的写入序（sqlite 的 rowid / mem 的
	// 写入序号），因此游标不泄漏任何存储细节。
	//
	// 指向不存在的消息时返回空结果（绝不退化为首页）。
	BeforeMsgID string
}

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
	// Search 全局搜索本机消息历史（跨会话，新→旧）。两个实现必须逐条一致：
	//
	//   - 只搜 content，且是【子串】匹配；ASCII 忽略大小写、非 ASCII 逐字节
	//     精确 —— 这正是 SQLite LIKE 的默认语义，mem 实现不得使用
	//     strings.ToLower（它会把 Unicode 也折叠，与生产实现分叉）；
	//   - **遵守会话清空点**：sent_at <= cleared_at 的消息不出现在结果里。
	//     判据与前端 activeMessages 逐字对齐（严格大于才可见）。
	//     过滤必须发生在 SQL 里，不能取回后再过滤 —— 后者会让每页条数不定、
	//     出现「翻了半天全是空白页」；
	//   - ConvIDs 为空 / Text 为空：返回空切片（不是全库、不是 nil 报错）；
	//   - BeforeMsgID 指向不存在的消息：返回空切片；
	//   - 排序：写入序倒序（与「时间倒序」在正常写入路径下等价）；
	//   - 不返回命中总数：调用方无法也不需要知道（见 MessageSearchQuery）。
	Search(q MessageSearchQuery) ([]message.Message, error)
	// Window 取「以某条消息为中心」的一页（含该消息本身），新→旧。
	//
	// 跳转定位专用（搜索命中 → 打开会话并停在那一条）。before / after 是命中
	// 消息【前后各取多少条】；两数都 <= 0 时只有命中消息本身。
	//
	// 契约：
	//   - 锚点不存在、或锚点已被会话清空点隐藏、或 convID 与锚点实际所属会话
	//     不符：一律返回【空切片】，不返回错误 —— 「点了历史里一条已被清空的
	//     消息」是正常的用户路径，调用方据此回退到 Latest 并给出提示；
	//   - 窗口内的消息同样遵守清空点（与 Search 一致，界面不会看到被隐藏的
	//     消息混在窗口里）；
	//   - 返回新→旧，与 Latest 一致（前端 loadHistory 就是按这个方向 reverse）。
	//
	// 为什么不拆成 Latest(旧侧) + After(新侧) 由调用方拼：Latest 的游标是
	// sent_at，与锚点同毫秒的其它消息会被整条跳过，两侧由此错位；而 Window
	// 的锚点是内部写入序，两侧严丝合缝。
	Window(convID, msgID string, before, after int) ([]message.Message, error)
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
	// SetJobStatus 【只】改任务状态，不动位图与进度（D-07 的暂停/恢复用）。
	//
	// 为什么不能拿 UpsertJob 代替：UpsertJob 整行覆盖，而调用方手里那份 Job
	// 是【内存快照】——接收侧的进度由另一条写入路径（SaveBitmap）推进，
	// 快照里往往还是零值。用它覆盖整行，就等于「暂停一下，进度条先归零」。
	//
	// 只更新 status 还有一个并发上的好处：暂停/恢复由用户操作驱动，与
	// 传输数据路径是两个 goroutine，两条路径写同一行时交集越小越好。
	SetJobStatus(jobID string, st transfer.State) error
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
// 文件共享仓储（FR-14）
// ---------------------------------------------------------------------------

// ErrShareNotFound 表示共享不存在。
//
// 做成哨兵错误而不是让两个实现各写一句话：调用方要区分「这条共享已被取消」
// （用户可行动：重新发布）与「存储故障」（重试），而两个实现互为替身，
// 判据分叉会让「测试跑 mem、生产跑 sqlite」出现不同的用户提示。
var ErrShareNotFound = errors.New("share: not found")

// ShareRepo 是【本机发布的共享】的持久化端口。
//
// 只管本地发布的共享；他人共享随节点目录传播（peer.Peer.Shares），
// 不落这张表 —— 共享方离线时他人的共享本就该消失（PRD RK-08），
// 持久化它反而会造出「列表里有、点开报错」的僵尸条目。
//
// 两个实现（sqlite / mem）必须给出同样的语义：
//   - Upsert 覆盖同 ID 的整条记录【含文件清单】（清单是整份替换，
//     不是增量合并 —— 一次「刷新索引」必须能删掉已经不存在的文件）；
//   - Get 未命中一律返回 (零值, false)，不返回 ErrShareNotFound；
//   - List 按创建时间升序（给界面一个稳定的次序）；
//   - Delete 幂等：不存在时返回 nil。
type ShareRepo interface {
	Upsert(s share.Share) error
	Get(shareID string) (share.Share, bool)
	List() []share.Share
	Delete(shareID string) error
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

// DeliveryStat 是一条群消息的送达统计。
type DeliveryStat struct {
	Acked int
	Total int
}

// Pending 返回尚未送达的人数。
func (d DeliveryStat) Pending() int {
	if d.Total <= d.Acked {
		return 0
	}
	return d.Total - d.Acked
}

// GroupDeliveryRepo 跟踪群消息的「按成员」送达情况。
//
// 存在理由：群消息过去用「单条 outbox + 任一成员 ACK 即出队」——
// 群里第一个回 ACK 的人就让它停止重发，离线成员永远收不到。改成按成员
// 登记后，离线成员上线时才知道「还差谁」，也才能显示「N 人未送达」。
//
// 两个实现（sqlite / mem）必须给出同样的语义，否则「测试跑 mem、生产跑 sqlite」
// 会在离线补投上分叉。
type GroupDeliveryRepo interface {
	// EnsureDeliveries 为一条群消息登记全体目标成员的投递记录（幂等；已存在的行不动，
	// 以免把已送达的标记覆盖回未送达）。
	EnsureDeliveries(msgID, groupID string, members []identity.NodeID) error
	// AckDelivery 标记某成员已送达（幂等；成员不在记录里则忽略）。
	AckDelivery(msgID string, memberID identity.NodeID) error
	// PendingMembers 返回尚未送达的成员。
	PendingMembers(msgID string) ([]identity.NodeID, error)
	// StatsByConv 返回会话内每条群消息的送达统计（key = msg_id）。
	StatsByConv(convID string) (map[string]DeliveryStat, error)
	// DeleteByMsg 清掉一条消息的全部投递记录（消息完成/清理时用）。
	DeleteByMsg(msgID string) error
	// DeleteByGroup 清掉某群的全部投递记录（解散时停止一切补投）。
	DeleteByGroup(groupID string) error
	// DeleteGroupMember 清掉某群某成员的全部投递记录（成员被移出群时用）。
	//
	// 必须做：留着的话，一个已被移出的成员会永远挂在「未送达」里 ——
	// 消息既不会重发给他（fanout 只发给 active 成员），也就永远凑不齐「全员送达」，
	// 于是这条消息会一直留在 outbox 里空转、界面上一直显示「还有人没收到」。
	DeleteGroupMember(groupID string, memberID identity.NodeID) error
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
	//
	// 注意它【会跳过正在退避的种子】：这份结果用于「这一轮问谁」，
	// 不能拿来当「配置了哪些种子」用。
	Pick(n int) []peer.SeedAddr
	// List 返回全部配置的种子（含正在退避的）。
	//
	// 展示与自检必须用这份：用 Pick 代替会让失败中的种子从界面上消失，
	// 界面上的「种子 0/0」于是被读成「一颗都没配」，而真实情况是
	// 「配了两颗，都连不上」—— 这两种故障的修法完全不同。
	List() []peer.SeedAddr
	// Report 回填探测/拉取结果；learned == nil 表示失败，据此累计 fail_cnt。
	Report(addr peer.SeedAddr, learned *peer.Announcement, err error)
	// Replace 用新清单替换注册表内容（设置页保存后热生效，见 D-15）。
	//
	// 语义要求：**保留同一地址的历史状态**（失败计数、退避期限、学到的身份）。
	// 只改一条清单就把全部退避清零，会让「刚恢复的种子」和「刚挂掉的种子」
	// 同时被立刻重探一遍 —— 用户看到的是「保存设置之后网络抖了一下」。
	Replace(addrs []peer.SeedAddr)
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
	// BatchID 是批次标识，同时决定落盘的根目录：同一个批次只分配一次根，
	// 避免一批文件散落在两个目录里，因此【必须全局唯一】，不能是文件夹名。
	//
	// 【同名目录就是同一个目录】：根目录已存在时直接复用它（重复下载同一个
	// 共享、对方把同一个文件夹再发一次、失败后重试都落到同一处），
	// 而不是另建「照片 (1)」。
	BatchID string
	// RelPath 是相对文件夹根的路径（含根名，如 `照片/2024/a.jpg`）。
	//
	// 同名同路径的再次到达是【同一份文件的再次到达】：实现应当就地覆盖，
	// 而不是加「 (1)」后缀 —— 后缀会让同一份内容越堆越多，且无法再合并。
	RelPath string
	// IsDir 表示这是一个空目录条目：只建目录、不建临时文件。
	IsDir bool
}

// FileSink 把 G3 的路径清洗收敛到一个实现里。
type FileSink interface {
	// Create 返回临时文件句柄 + 最终路径；实现内做全部清洗。
	//
	// 单文件（不带目录树）【保留同名去重】：「 (1)」是它防止覆盖用户已有
	// 文件的手段，那里没有「同一个目录」这个判据可用。
	Create(jobID, rawName string) (SinkHandle, error)
	// CreateIn 按相对路径把文件写进批次目录树里。
	//
	// 与 Create 的差别不只是「多了几层」：它还要做逐段清洗，并且为整个批次
	// 只分配一次根目录 —— 否则同名文件夹被抢到时，一批文件会散落在
	// 「照片」与「照片 (1)」两个目录里。
	CreateIn(req SinkRequest) (SinkHandle, error)
	// Root 返回接收落点根目录。
	//
	// 空间预检要用它：Create/CreateIn 之前拿不到具体落点，而批次目录与根
	// 是同一个卷，按根判剩余空间就够（见 TransferApp.spaceRejectReason）。
	Root() string
	// AdoptRoot 认领一个已经分配过的批次根目录（重启恢复用）。
	//
	// 不认领的后果：根目录当初可能是「退到唯一名」的结果（同名文件挡路），
	// 进程重启后重新解析可能得到另一个目录，把一个文件夹拆成两份，
	// 用户看到的是「文件少了一半」。
	AdoptRoot(batchID, absPath string)
	// Commit 原子替换为最终文件。
	Commit(h SinkHandle) (finalPath string, err error)
	// Abort 清理临时文件。
	Abort(h SinkHandle) error
}

// ---------------------------------------------------------------------------
// 屏幕抓取（本地单帧截屏）
// ---------------------------------------------------------------------------

// ScreenImage 是一张抓到的屏幕图像（尚未编码的像素）。
type ScreenImage struct {
	// Width / Height 是像素尺寸，且是【物理像素】不是逻辑像素。
	//
	// 必须是物理像素：用户在 150% 缩放的屏幕上看到的是物理像素，
	// 抓成逻辑像素会得到一张被系统插值放大过的糊图。
	Width, Height int
	// Data 是紧密排列的 RGBA8888 像素，长度恒为 Width*Height*4。
	//
	// 「紧密排列」是刻意的：各平台原始格式不同（Windows 的 DIB 是 BGRA、
	// macOS 的 CGImage 可能带 stride），把它们统一到同一种布局，
	// 免得每个消费方都要重新处理一遍 stride 与通道序。
	Data []byte
}

// Display 是一块显示器在虚拟桌面上的位置与尺寸。
//
// 坐标系统一说明（这是整个截屏覆盖层能铺准屏幕的前提）：
//
//   - X / Y / Width / Height 是【DIP 逻辑像素】，也就是 Windows 的
//     「按 96 DPI 折算过的像素」。它们与 Wails 窗口定位用的是同一套坐标
//     （Wails v3 的 SetPosition/SetSize 走 DIP ↔ 物理的换算），
//     所以覆盖层窗口可以直接用这组数字定位，不需要再做一次换算。
//   - PhysicalWidth / PhysicalHeight 是【物理像素】，也就是真正被拍下来的
//     那些像素。抓屏拿到的是物理像素，两者之比即 ScaleFactor。
//
// 虚拟桌面允许负坐标：副屏摆在主屏左边时 X 就是负的。
type Display struct {
	// Index 是本机内的显示器序号（从 0 开始），用于按序号抓取。
	//
	// 序号由实现自行枚举并保持稳定于「一次调用之内」—— 跨调用不保证，
	// 因此它只适合做「枚举出来的列表里的第几块」的临时引用。
	Index int
	// X / Y 是该显示器左上角在虚拟桌面中的 DIP 坐标。
	X, Y int
	// Width / Height 是该显示器的 DIP 尺寸。
	Width, Height int
	// ScaleFactor 是 DIP → 物理像素的倍率（DPI/96）：150% 缩放 = 1.5。
	ScaleFactor float64
	// PhysicalWidth / PhysicalHeight 是该显示器的物理像素尺寸。
	PhysicalWidth, PhysicalHeight int
	// PhysicalX / PhysicalY 是该显示器左上角在虚拟桌面中的【物理像素】坐标。
	//
	// 单独给出，而不是让调用方拿 DIP 乘 ScaleFactor 反算：DIP ↔ 物理的
	// 换算带舍入，反算一次可能与真实边界差一两个像素 —— 对「铺满屏幕的
	// 覆盖层窗口」来说，这一两个像素就是屏幕上的一条缝。
	PhysicalX, PhysicalY int
}

// ScreenCapturer 是「抓本机屏幕」这项平台能力。
type ScreenCapturer interface {
	// Capture 抓取【鼠标当前所在的那块显示器】的画面。
	//
	// excludeSelf 为真时，本应用自己的窗口不出现在画面里。
	//
	// 两条契约：
	//
	//  1. 平台中立 —— 这里不出现窗口句柄之类平台概念。句柄是平台专有的
	//     （Windows HWND / macOS CGWindowID / X11 Window），把它放进接口
	//     会让 domain 层认识 Win32。各平台自己枚举自己的窗口即可。
	//
	//  2. 不留残余状态 —— 实现对系统做的改动必须在返回前复原。
	//     Windows 的显示亲和性（DDA）是【粘性】的：设上去就一直生效。
	//     忘了复位的后果是用户此后用任何工具截图都拍不到本应用的窗口，
	//     而且没有任何报错会提示他去找原因。
	Capture(ctx context.Context, excludeSelf bool) (ScreenImage, error)

	// Displays 枚举本机的所有显示器。
	//
	// 供「交互式截图」的覆盖层窗口定位使用：每块屏开一个全屏窗口，
	// 窗口的位置与尺寸直接取这组 DIP 坐标。
	Displays() ([]Display, error)

	// CaptureDisplay 抓取【序号为 index 的那块显示器】的画面。
	//
	// 与 Capture 的区别只在「抓哪一块」：前者按鼠标位置选，后者按序号选。
	// 之所以要按序号而不是按物理矩形，是因为 macOS 的 CoreGraphics
	// 只能按 display ID 抓整块屏（CGDisplayCreateImage 不支持任意矩形），
	// 按矩形抓会让 macOS 变成特例。
	CaptureDisplay(ctx context.Context, index int, excludeSelf bool) (ScreenImage, error)
}

// 截屏模式。
//
// 只有交互式截图一条路（覆盖层里选区 + 标注），模式只决定一件事：
// 画面里要不要有蜂群自己。放在 ports（domain 层）而不是 screencap
// 适配器包里，是因为服务层（adapters/wails）要用到它们，而红线 R2
// 规定适配器之间禁止互引 —— 让服务层去 import 适配器只为拿两个常量，
// 是一次不必要的架构破坏。这些是纯字符串常量，不违反红线 R1。
const (
	// ScreenshotModeKeepWindow 保留本应用窗口 —— 画面里能看到蜂群自己的界面。
	ScreenshotModeKeepWindow = "keep_window"
	// ScreenshotModeHideWindow 排除本应用窗口 —— 先最小化主窗口再抓屏，
	// 画面里没有蜂群自己的界面。
	ScreenshotModeHideWindow = "hide_window"
)
