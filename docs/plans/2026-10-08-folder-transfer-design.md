# 文件夹传输设计（batch：一批排队任务 + 相对路径）

- 日期：2026-10-08
- 状态：已评审，待实施
- 相关：ADR-005（滑动窗口与位图续传）、`docs/plans/2026-10-08-offline-file-transfer-v2-design.md`（等待队列，本设计直接复用）
- 取代既有承诺：`README.md:105`、`设计书.md:30`、`docs/superpowers/specs/2026-09-20-swarmlink-v1-design.md:25` 的「明确不做：文件夹传输」

## 1. 问题

现在只能发单个文件：`SendFileAs` 一个路径一条任务；用户选一个目录时，`sendFile` 会以 `transfer: <path> is a directory` 直接失败（`internal/app/transfer_app.go:251-253`）。而实际使用中最常见的需求恰恰是「把这个文件夹发给你」。

目标：**选中一个文件夹即可发送，接收端还原出同样的目录树**；过程中的每个文件仍是一条独立的传输任务（可续传、可等待、可单独看到失败），整批在界面上表现为一条聚合卡片，并且能整体取消。

## 2. 现状（已核实）

| 事实 | 位置 |
| --- | --- |
| 协议帧无目录语义：`FileMeta` 只有 `file_name`，没有相对路径 | `internal/domain/protocol/wire.go:137-146` |
| 接收侧**主动去目录化**：清洗第一步就是 `filepath.Base(raw)`，并拒绝含 `/` `\` 的名字 | `internal/infra/fsutil/safename.go:44-60` |
| `FileSink.Create` 只接受一个扁平文件名，同名加 `(1)`，**建不出子目录** | `internal/adapters/filesink/filesink.go:37-64`、`:103-132` |
| 落盘目录固定为配置的下载目录，接收不弹框（设计书 32/480 的约定） | `internal/adapters/config/config.go:56`、`internal/bootstrap/bootstrap.go:176` |
| `Job` 是单文件模型，无 batch / 相对路径维度 | `internal/domain/transfer/job.go:33-56` |
| DB 最新迁移 v3；迁移只追加、带 checksum，已发布版本不可改 | `internal/adapters/store/sqlite/migrations.go:21-22`、`:153-169` |
| 能力位 `CapsFolderTransfer`（bit 6）**已定义**，握手 `negotiate()` 已在真协商；但我们自己**没打开这一位** | `internal/domain/peer/peer.go:31-40`、`internal/adapters/net/tcp/handshake.go:49`、`:178`、`:235` |
| 已有「轻量任务 + 自动拉起」的完整机制：`newWaitingJob` 只 stat 不哈希 | `internal/app/transfer_waiting.go:136-164` |
| 三个拉起触发点：`SweepWaiting`（1 秒节拍）、`WakePeer`（对端上线）、`Recover`（启动） | `transfer_waiting.go:298-334`、`:378-412` |
| `startJobs` 已保证**同一对端同时只启动一个**任务（防哈希风暴） | `transfer_waiting.go:336-363` |
| 自动拉起**只认 `WaitReason == WaitPeerOffline`**；`WaitReason` 同时是「用户意图」标记，取消会把它清空 | `transfer_waiting.go:343-350`、`:447-451` |
| `ListWaiting` 的 SQL 硬编码 `wait_reason = ?`（单一值） | `internal/adapters/store/sqlite/transfers.go:118-122` |
| `sendFile` 与 `newWaitingJob` 都**从零构造 Job 再 Upsert**（会抹掉批次字段） | `transfer_app.go:304-320`、`transfer_waiting.go:143-163` |
| 无中断点：取消**只允许**取消等待中的任务，注释明说 sender 没有取消信号 | `transfer_waiting.go:425-429` |
| 状态机**已允许** `Transferring → Cancelled` / `Verifying → Cancelled` | `internal/domain/transfer/state.go:24-34` |
| 前端 `List()` 走 `ListRecent(50)`，重启后能拿到任务列表（但只有 50 条窗口） | `internal/adapters/wails/services.go:712-723` |
| 前端发文件消息的入口是 `chat.sendFile → api.sendFileMessage → ChatService.SendFileMessage` | `frontend/src/stores/chat.ts:273-282`、`frontend/src/api/bindings.ts:94` |
| 目录选择能力现成：Wails 内置 `Dialogs.OpenFile` 支持 `CanChooseDirectories`，当前被写死为 `false` | `frontend/src/api/dialog.ts:27-42` |

## 3. 设计

### 3.1 数据模型：给 Job 加三个字段，迁移 v4

```go
// internal/domain/transfer/job.go
type Job struct {
	// …既有字段不动…
	BatchID  string // 所属文件夹批次；单文件传输为空串
	RelPath  string // 相对文件夹根的路径（含根名，如 `照片/2024/a.jpg`）；单文件为空串
	IsDir    bool   // 目录条目（空目录占位），零字节、无分块
}
```

迁移 **v4**（只追加，不动 v1–v3 文本）：

| 列 | 类型 | 含义 |
| --- | --- | --- |
| `batch_id` | `TEXT NOT NULL DEFAULT ''` | 批次 ID；空串 = 单文件传输 |
| `rel_path` | `TEXT NOT NULL DEFAULT ''` | 相对路径（发送方意图，也是接收方还原目录的依据） |
| `is_dir` | `INTEGER NOT NULL DEFAULT 0` | 目录条目 |

外加 `CREATE INDEX IF NOT EXISTS ix_transfer_batch ON transfer_jobs(batch_id)`：批次聚合进度与整体取消都要按 `batch_id` 查。

`batch_id` 的取值约定（无法用 CHECK 表达，只能靠契约）：**由发送端生成且全局唯一**（随机串，不是文件夹名、不是序号），同一个 `batch_id` 只属于一个对端的一次发送。两个对端各发来一个同名批次时，卡片会把两个文件夹合并、取消也会连带停掉另一批。**空 `batch_id` 一律报错**，不许退化成「返回所有单文件任务」—— `CancelBatch("")` 若走到那里，就是把本机全部单文件传输一起停掉。

**不用新状态**：批次不是状态机的概念，`State` 与 `transitions` 一个字不改。

### 3.2 协议与能力

`protocol.FileMeta` 追加三个字段（JSON 是加法，老解码器忽略未知字段）：

```go
BatchID  string `json:"batch_id,omitempty"`
RelPath  string `json:"rel_path,omitempty"`
IsDir    bool   `json:"is_dir,omitempty"`
```

- 我们自己的能力位在 `DefaultHandshakeConfig()` 里打开 `peer.CapsFolderTransfer`（`handshake.go:49`）。
- **发送前必须查对端能力**：`dir.Get(peerID).Caps.Has(peer.CapsFolderTransfer)` 为假时**拒绝发送并明确提示**，不做「偷偷打包成 zip」这种猜测用户意图的降级。理由：老接收端会忽略 `rel_path`，把整个文件夹的几十个文件**平铺**到下载目录里，还会因同名互相加 `(1)` —— 那是数据的错乱，不是降级。
- 单文件传输路径**完全不碰**这三个字段（全为零值），因此即使对端是老版本也不受影响。

### 3.3 发送侧：枚举即排队

新入口（`SendFileAs` 的同级方法）：

```go
// SendFolderAs 枚举目录、建好一批排队任务，随后立即返回。
// 它【不】等文件传完：传输由既有的三个触发点驱动。
func (a *TransferApp) SendFolderAs(ctx context.Context, peerID identity.NodeID, dir, batchID string) (FolderSummary, error)
```

`FolderSummary` = `{BatchID, FolderName, FileCount, TotalSize, Skipped}`，其中 `Skipped` 是被跳过的符号链接数与超限条目数。

枚举用 `filepath.WalkDir`：

- 普通文件 → 一条轻量 job：`Status = StateIdle`、`WaitReason = WaitQueued`、**不哈希**（只 stat 拿大小），`LocalPath` 是绝对路径，`RelPath` 是相对路径。
- **目录条目**：空目录也要还原 → 每个空目录一条 `IsDir = true` 的零字节 job；整个文件夹为空时，为根目录本身发一条。
- 符号链接**不跟随**（`d.Type()&fs.ModeSymlink != 0` → 跳过并计数）。跟随会引入循环与越权读取，且跨平台语义不一致。
- 上限：条目数超过 `MaxFolderEntries`（默认 20000）→ 直接拒绝并提示，不建半批任务。防止一次误操作产生几十万条 DB 行。

**为什么复用「轻量任务 + 自动拉起」而不是新写调度**：`startJobs` 已经保证同一对端同时只传一个（`transfer_waiting.go:336-363`），三个触发点也已经接好，所以整批天然串行、重启后自动继续 —— 这正是文件夹需要的行为，一行新调度都不用写。

新增等待原因：

```go
// WaitQueued 表示「排在批次队列里，轮到就传」。
//
// 与 WaitPeerOffline 一样属于「我们自己会拉起」的任务；
// 用户取消会把 WaitReason 清成 WaitNone，因此取消过的任务永远不会被
// sweep 自动拉起（transfer_waiting.go:343-350 的既有约束）。
//
// 取值刻意【不】叫 "queued"：那句查询里紧挨着还有 status = 'queued'
// （StateIdle 的落库值），两个 "queued" 并排会让人以为写重了。
WaitQueued WaitReason = "batch_queued"
```

`ListWaiting` 的 SQL 从 `wait_reason = ?` 改为 `wait_reason IN (?, ?)`（`transfers.go:118-122`），并把「只自动拉起」的守卫从「等于 `peer_offline`」改成「属于可自动拉起集合」，**取消态的 `None` 依旧被排除**。

**批次字段承接（易错点）**：`sendFile`（`transfer_app.go:304-320`）与 `newWaitingJob`（`transfer_waiting.go:143-163`）都是从零构造 Job 再 Upsert，会把 `BatchID/RelPath/IsDir` 抹成零值，表现为「传输列表里文件的相对路径没了、聚合进度算不出来」。处理方式与既有的 `RetryCount` 完全一致：同一处已经在读 `prev`，一并把这三个字段承接过来（抽一个 `carryOverBatch(prev, job)` 小函数，两条路径共用）。

### 3.4 接收侧：逐层清洗 + 子目录落盘

**新增 `fsutil.SanitizeRelPath(rel string) ([]string, error)`**：把相对路径按分隔符切开，**每一段**都走现有的 `SanitizeFileName`，并额外拒绝：

- 空段 / `.` / `..`（路径穿越）；
- 绝对路径与盘符（`C:`、`/`、`\` 开头）；
- 空相对路径（`rel_path` 非空时才有目录语义）；
- 深度超过 `MaxRelPathDepth`（默认 32）；
- 总长度超过平台上限。

返回**已清洗的段列表**，由 `filesink` 逐段 `SecureJoin` 到批次根目录下 —— 与现有 `SecureJoin`（`fsutil/atomic.go:14`）是同一条思路，只是作用于每一层。

`ports.FileSink` **新增**方法（不改现有 `Create`，单文件路径零风险）：

```go
// SinkRequest 是一次「落在目录树里」的写入请求。
type SinkRequest struct {
	JobID   string
	BatchID string   // 空串 = 单文件，与 Create 行为一致
	RelPath string   // 目录树里的相对路径
	IsDir   bool     // 只建目录、不写文件
}

type FileSink interface {
	Create(jobID, rawName string) (SinkHandle, error) // 既有，不动
	CreateIn(req SinkRequest) (SinkHandle, error)     // 新增
	// AdoptRoot 认领一个已经分配过的批次根目录（重启恢复用）。
	AdoptRoot(batchID, absPath string)
	Commit/Abort … // 不动
}
```

- 临时文件仍然落在**最终目录内**（`.part` 与目标同目录）—— 跨目录 `os.Rename` 不保证原子，ADR-005 的原子替换依赖这一点。
- `IsDir` 请求：只 `MkdirAll`，不建临时文件，`SinkHandle` 的 `TempPath` 为空、`FinalPath` 是目录本身。
- **批次根目录只分配一次**：第一次见到某 `batchID` 时用现有的 `uniqueLocked` 逻辑确定 `照片 (1)/`，并记住 `batchID → 根目录`；后续同批文件都在这个根下。
- **重启不能重新分配**：进程重启后续传，内存里的映射没了，会重新分配出 `照片 (1)`，把一个文件夹拆成两份。因此 `AdoptRoot` + 在 `TransferApp.Recover()` 里从库中同批任务的 `local_path` 取回根目录（`Recover` 本来就是「收尾上一次运行」的地方）。

### 3.5 目录条目（`IsDir`）的收发

- 发送：`IsDir` 任务照常发 `FILE_META`（带 `is_dir=true`、`file_size=0`），**不发任何分块**，直接发 `FILE_DONE`。
- 接收：`HandleFileMeta` 现有的 `total == 0 → invalid meta` 校验（`transfer_app.go:517-524`）必须为 `IsDir` 放行（否则空目录会在对端报「协议非法」）；收到即 `MkdirAll`，回一张空位图的 `FILE_META_ACK`；`FILE_DONE` 时无需校验，直接判完成。
- 好处：空目录也继承等待队列、重试、串行与批次聚合，不需要为它单独造一套机制或新帧类型。

### 3.6 取消：新增中断点 + 批量取消

现状注释写得很清楚：「真正在传的任务要取消，需要给每条任务一条取消信号，而当前 sender 是『一个任务一个 goroutine + 一条共享 ctx』，没有中断点」（`transfer_waiting.go:425-429`）。本设计补上这个中断点：

- `sendJob` 增加取消信号（`cancelled atomic.Bool` + 一条 channel），`runSender` 的循环里检查 → 立即返回哨兵错误 `errJobCancelled`。
- 顶层统一失败出口（`transfer_app.go:207-239`）必须**先判取消**：取消**不能**掉进「对端不可达 → 转等待」那条分支，否则用户点了取消、对方一上线文件又自己跑起来 —— 这是既有代码明确禁止的情形。
- 取消的落库结果：`Status = StateCancelled`、`WaitReason = WaitNone`、`NextTryAt = 零值`，发 `transfer:state`；半截临时文件**保留**（下次重发可按位图续传），不删。
- 顺带把 `CancelJob` 放开到「在传的任务也能取消」：机制同一份，且「文件夹能停、单个文件不能停」在界面上说不通。`services.go:629-637` 的注释与错误文案要一起改。

新方法：

```go
// CancelBatch 取消整个批次里所有未结束的任务，返回取消条数。
func (a *TransferApp) CancelBatch(batchID string) (int, error)
```

需要新端口方法 `ListByBatch(batchID)`（sqlite 走新索引；内存实现同步加）。

### 3.7 重启恢复

`TransferApp.Recover` 追加两件事：

1. 对 `ListActive()` 里带 `batch_id` 的任务，按批次分组的 `local_path` 取根目录 → `sink.AdoptRoot(batchID, root)`；
2. `ListWaiting()` 已经会把 `WaitQueued` 的任务一起捡起来（3.3 的 SQL 改动），所以批次重启后自动继续，**不需要新增恢复逻辑**。

无中断点时崩溃的 `active` 发送任务仍按现状转入等待；接收侧仍标 `failed`（`FindResumable` 只排除 `done/cancelled`，failed 行照样能被继承续传）。

### 3.8 界面

- **新消息类型** `MsgTypeFolder = "folder"`（`internal/domain/message/message.go:35-44`），content 是 `FolderMessageContent` 的 JSON 信封：
  `{batch_id, folder_name, file_count, total_size, skipped}`。
  与 `MsgTypeFile` 完全同构：只负责让两端聊天框出现一张卡片，传输由独立通道完成。
- **聊天框一条聚合卡片**：`文件夹名/ · N 个文件 · 1.2 GB`，进度按 `batchId` 聚合传输 store 里的任务（`已完成 x/N`），`取消全部` 按钮调 `CancelBatch`；被跳过符号链接时显示 `已跳过 3 项`。
- **传输列表逐文件分行**（已定）：那是真实任务，每行显示 `RelPath`（区分 `a/dup.txt` 与 `b/dup.txt`）；`排队中` 的文案由 `waitReason === 'batch_queued'` 决定，**不是** `peer_offline` 的「等待对方上线」。
- **入口**：`pickFiles(multiple)` 旁边加一个「发送文件夹」，调 `Dialogs.OpenFile{CanChooseDirectories: true, CanChooseFiles: false}`；Go 侧零改动。
- **批次任务查询**：新增 `TransferService.ListBatch(batchId)`（走新端口方法）。不能靠 `List()` 的 `ListRecent(50)`：500 个文件的文件夹重启后聚合进度会算缺。
- `TransferDTO` 补 `batchId/relPath/isDir`；`TransferJob`（前端）同步补。

## 4. 明确不做

1. **群聊文件夹传输** —— 与现有「文件传输暂不支持群聊」一致，入口按钮在群里本就禁用。
2. **打包成 zip 的降级路径** —— 见 3.2，宁可明确拒绝也不猜用户意图。
3. **并发额度与排队序号** —— 整批仍是「同一对端一次一个」，不做多流并发（架构书 972-977 的排队位仍留白）。
4. **文件权限位 / mtime 还原** —— 内网分享场景无此需求。
5. **跟随符号链接** —— 见 3.3。
6. **发送前的整体预哈希与「准备中」进度** —— 哈希仍在每个文件真的要传的那一刻做（沿用现状），因此没有独立的准备阶段。代价：文件夹卡片一出现就是最终文件数与总大小，但单个文件的哈希完成后才开始传。

## 5. 风险

| 风险 | 应对 |
| --- | --- |
| 大批次产生几十万 DB 行 | 条目上限 20000，超限整批拒绝；`PurgeFinished` 保留最近 keep 条（既有） |
| 单文件失败拖累整批 | 每个文件独立任务、独立终态；批次只做聚合显示，一个失败不影响其余 |
| 重启后根目录被重复分配 | `AdoptRoot` + `Recover` 认领（3.4） |
| 取消后任务自己复活 | 取消先于「转等待」判定；`WaitReason` 清空即不可被自动拉起 |
| 接收端被恶意 `rel_path` 写到下载目录之外 | 逐段清洗 + 每层 `SecureJoin` + 深度上限（3.4） |
| 老版本对端收到文件夹 | 发送前能力位检查，直接拒绝，不产生平铺错乱 |
| 20000 条任务一次性 Upsert 拖慢 RPC | 枚举与写库都在同步路径上但只做 stat；若实测卡顿，退路是分批写入（本次不做） |
| **接收端续传键误命中**：`FindResumable` 的键是 `(peer, file_hash)`，同一批次里两个内容相同的文件（一张文件夹里两份一样的图/文档很常见）会命中同一条，后一个会**继承前一个的临时文件与最终路径** —— 结果是目录里少一个文件、另一个内容错位 | Task 7 落地时把批次内的续传键升为「按 `rel_path`」：`FindResumable(peerID, fileHash, relPath)`，`relPath == ""` 时保持单文件旧语义。**这条必须在批次上线前解决**，见实施计划的 Task 7 |
| `IsDir` 条目的 `FileID` 为空，会带着空 hash 走进续传查找 | Task 7 显式化：目录条目不进 `FindResumable`（现在只是被 `TempPath != ""` 这个次生条件偶然挡住） |
| 批次把任务数放大：`ListRecent(50)` 与 `PurgeFinished(keep)` 都以 job 计数，一个 20000 文件的文件夹会把单文件历史挤出窗口、吃光保留位 | 本次不动（字段零值兼容、行为未变）；Task 6 之后值得重新想保留策略（例如按 batch 折叠计数） |

## 6. 验收标准

1. A 选一个有多层子目录的文件夹发给 B：B 的下载目录里出现**同名的完整目录树**，文件名与层次与源一致。
2. 文件夹**空子目录**也出现在 B 的接收目录里。
3. 文件夹里的一个文件被删除/不可读：**只有那一行失败**，其余文件照常传完，卡片显示 `N-1/N 完成，1 个失败`。
4. 对端离线时发文件夹：整批进入「排队中」，对方上线后自动按序传完（不需要用户操作）。
5. 传一半时 A 关掉应用再打开：整批继续，且**不会**出现第二个 `照片 (1)` 目录。
6. 点卡片上的「取消全部」：排队中的与正在传的都停下，状态为 `cancelled`，用户无法通过任何后续操作让它自己跑起来。
7. 目标目录已存在同名文件夹：整批落到 `照片 (1)/`，两份内容不混合。
8. 发送给不支持 `CapsFolderTransfer` 的对端：明确报错提示，不发送任何文件。
9. 单文件传输的一切行为（含离线等待、续传、取消）与本次改动前一致。
