package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// TransferApp 编排文件传输（ADR-005：滑动窗口 W 块在途 + 周期性全量位图回传）。
type TransferApp struct {
	self  identity.NodeID
	store ports.TransferRepo
	sink  ports.FileSink
	conns ports.ConnManager
	dir   ports.PeerDirectory
	bus   ports.EventBus
	clk   ports.Clock
	lg    *slog.Logger

	MaxConcurrent int
	ChunkSize     int64
	WindowSize    int

	// MaxFolderEntries 是一次文件夹传输允许的最大条目数（文件 + 目录），
	// 0 表示用默认值。上限不是洁癖：一次误操作（选了 C:\Users）会产生
	// 几十万条任务，而枚举与落库都在同步路径上，界面会先卡死。
	MaxFolderEntries int

	// MaxFileSize 是【接收侧】愿意收下的单文件上限（字节），0 表示不限制。
	//
	// 只在接收侧强制：发送方能拦住自己界面上的选择，拦不住别人机器上的配置，
	// 而"要不要收下这个文件"本来就是接收方的决定。
	MaxFileSize int64

	// MaxRetries 是自动重试次数上限，0 表示用默认值。见 transfer_waiting.go
	// 里"对端在、但每次尝试都失败"与"对端不在、继续等"的区分。
	MaxRetries int

	// DiskFree 探测某个路径所在卷的可用字节数，nil 表示不做空间预检。
	//
	// 做成字段而不是直接 import 平台包：应用层不依赖适配器（依赖方向单向
	// 向内），而且「磁盘满」在真实环境里没法可靠复现，测试必须能注入。
	DiskFree func(path string) (uint64, error)

	// BitmapFlushInterval / FlushEveryChunks 控制位图批量落盘（P1-9）。
	BitmapFlushInterval time.Duration
	FlushEveryChunks    int
	// AckInterval / AckEveryChunks 控制接收侧回传全量位图的频率。
	AckInterval    time.Duration
	AckEveryChunks int
	// ProgressInterval 是进度事件节流间隔（4~10 Hz，P1-10）。
	ProgressInterval time.Duration

	// BaseBackoff / MaxBackoff 是「等待对方上线」的重试退避参数
	// （与消息 outbox 同一套：1s → 2s → … → 5min）。
	BaseBackoff time.Duration
	MaxBackoff  time.Duration

	mu        sync.Mutex
	senders   map[string]*sendJob
	receivers map[string]*recvJob

	// starting 记录「已经交给传输 goroutine、但还可能在建任务路上」的 job_id。
	//
	// 为什么不能只看 senders：senders 是在 sendFile 内部（抢到信号量之后）
	// 才登记的，而 sweep 与 peer.online 两个触发点完全可能同时命中同一条
	// 等待任务 —— 那样会同时跑两个 goroutine、两次全文件哈希。
	starting map[string]struct{}

	// cancelling 记录「用户已要求取消、但那个 goroutine 还没看到信号」的任务。
	//
	// 为什么不能只靠 sendJob.cancel：sendFile 在【登记 sender 之前】要读完整
	// 文件算哈希（大文件是分钟级），那段窗口里还没有 sendJob 可以发信号。
	// 用户恰恰最容易在「正在准备」的时候点取消。
	cancelling map[string]struct{}

	sendSem chan struct{}
}

type chunkAck struct {
	bitmap    transfer.ChunkBitmap
	completed int
	// reject 非空表示对端【拒收】了整个任务（例如超过它的接收上限）。
	// 它不是「再等等」：收到就立刻失败，不允许掉进等待重试。
	reject string
}

type doneAck struct {
	ok  bool
	err string
}

type sendJob struct {
	job      transfer.Job
	f        *os.File
	acked    transfer.ChunkBitmap
	hashes   []string
	ackCh    chan chunkAck
	doneCh   chan doneAck
	lastSent map[int]time.Time

	// cancel 被关闭即表示用户要求本次传输尽快干净退出。
	//
	// 为什么需要中断点：sender 是「一个任务一个 goroutine + 一条共享 ctx」，
	// 没有它就只能拒绝取消，而「点了没反应的假取消」比拒绝更糟。
	// 取消【不删】半截临时文件：下次重发还能按位图续传。
	cancel    chan struct{}
	cancelOne sync.Once
}

// isCancelled 报告是否已收到取消信号（非阻塞）。
func (sj *sendJob) isCancelled() bool {
	select {
	case <-sj.cancel:
		return true
	default:
		return false
	}
}

// requestCancel 发一次取消信号；重复调用安全（连点取消不该 panic）。
func (sj *sendJob) requestCancel() { sj.cancelOne.Do(func() { close(sj.cancel) }) }

type recvJob struct {
	job          transfer.Job
	handle       ports.SinkHandle
	f            *os.File
	bitmap       transfer.ChunkBitmap
	expectHashes []string

	sinceFlush int
	lastFlush  time.Time
	sinceAck   int
	lastAck    time.Time
	dirty      bool
}

// NewTransferApp 构造传输应用。
func NewTransferApp(
	self identity.NodeID,
	store ports.TransferRepo,
	sink ports.FileSink,
	conns ports.ConnManager,
	dir ports.PeerDirectory,
	bus ports.EventBus,
	clk ports.Clock,
	lg *slog.Logger,
) *TransferApp {
	maxConc := 3
	return &TransferApp{
		self: self, store: store, sink: sink, conns: conns, dir: dir,
		bus: bus, clk: clk, lg: lg,
		MaxConcurrent:       maxConc,
		ChunkSize:           transfer.DefaultChunkSize,
		WindowSize:          transfer.DefaultWindow,
		MaxFolderEntries:    defaultMaxFolderEntries,
		BitmapFlushInterval: 2 * time.Second,
		FlushEveryChunks:    256,
		AckInterval:         500 * time.Millisecond,
		AckEveryChunks:      32,
		ProgressInterval:    250 * time.Millisecond,
		BaseBackoff:         time.Second,
		MaxBackoff:          5 * time.Minute,
		senders:             make(map[string]*sendJob),
		receivers:           make(map[string]*recvJob),
		starting:            make(map[string]struct{}),
		cancelling:          make(map[string]struct{}),
		sendSem:             make(chan struct{}, maxConc),
	}
}

// ---------------------------------------------------------------------------
// 发送侧
// ---------------------------------------------------------------------------

// SendFile 发送单个文件，阻塞直到完成/失败。job_id 由本层生成。
//
// 流程：FILE_META →（对端回已完成位图）→ 窗口内发送缺失块 →
// （对端周期性回全量位图，据此补洞）→ FILE_DONE → 校验结果。
func (a *TransferApp) SendFile(ctx context.Context, peerID identity.NodeID, path string) (transfer.Job, error) {
	return a.SendFileAs(ctx, peerID, path, message.NewID())
}

// SendFileAs 与 SendFile 完全一致，只是 job_id 由调用方指定。
//
// 存在的唯一理由：异步调用方（桌面外壳）必须在任务【真正开始之前】就拿到
// job_id，才能在同一个 RPC 的返回值里把它交给界面，让界面据此把任务条目
// 和随后到达的 transfer:progress / done / error 事件对上。
// 若 id 在本层生成，异步调用方就只能另造一个 id 返回 —— 那个 id
// 与任何事件都对不上，表现为「发了文件但进度永远不出现」。
func (a *TransferApp) SendFileAs(
	ctx context.Context,
	peerID identity.NodeID,
	path string,
	jobID string,
) (transfer.Job, error) {
	// 抢占：同一条任务同一时刻只有一个 goroutine 在跑（sweep 与 peer.online
	// 都可能来拉它）。初始发送也走这里，因此那次抢占与后续重试共用同一把锁。
	if !a.claimStarting(jobID) {
		return transfer.Job{}, fmt.Errorf("transfer: job %s 已在传输中", jobID)
	}
	defer a.releaseStarting(jobID)
	return a.sendFile(ctx, peerID, path, jobID, false)
}

// sendFile 是「一次传输尝试」的实现。force 为真时【跳过】"对端看起来不可达"
// 的快速判定 —— 只给「立即重试」用：用户点了按钮就是想要真的试一次，
// 哪怕目录说对方离线（目录可能滞后于现实）。
func (a *TransferApp) sendFile(
	ctx context.Context,
	peerID identity.NodeID,
	path string,
	jobID string,
	force bool,
) (job transfer.Job, err error) {
	if jobID == "" {
		return transfer.Job{}, fmt.Errorf("transfer: job_id 不能为空")
	}

	// 取消与启动之间的竞态：抢占成功【不代表】任务还在等待中 ——
	// 用户可能在我们抢占到之前就点了取消（那时 isStarting 还是 false，
	// CancelJob 会放行）。这里复读一次库：不是「等待中」就干净地放弃
	// （不写库、不发事件），否则我们随后会把它写成「传输中」，把取消覆盖掉，
	// 而"取消之后对方上线仍然自动传输"正是这个功能明令禁止的事。
	//
	// 反向窗口由 CancelJob 的 isStarting 检查兜住（我们已抢占 → 它拒绝）。
	// prev 一直在手：状态校验、退避档位、批次字段三处都要用它，
	// 读一次比读三次更不容易漏（漏掉的代价见 carryOverBatch 的注释）。
	prev, prevErr := a.store.GetJob(jobID)
	if prevErr == nil && prev.Status != transfer.StateIdle {
		return transfer.Job{}, fmt.Errorf("transfer: 任务已不在等待中（当前 %s）", prev.Status)
	}

	// 目录条目（空目录占位）的线上形状与文件不同：零字节、无分块、无哈希。
	// 它必须在这里分流 —— 落进下面「文件」那条路径会在 os.Stat 上就被判成
	// "is a directory" 直接失败，于是整批里唯一能把空目录建出来的消息永远发不出去。
	isDir := prevErr == nil && prev.IsDir

	// networkAttempted 标记「失败是否已经发生在网络阶段」。
	//
	// 这是分类的硬前提：本地检查（stat / 打开 / 哈希）出错一律是【真失败】——
	// 文件不在、读不了，等多久都不会好。只有进入网络阶段之后的失败才可能是
	// 「对端暂时够不着」。
	//
	// 为什么必须有这个标记，而不是靠错误文案判断本地失败：Windows 说
	// "The system cannot find the file specified"，Linux 说 "no such file"，
	// 靠文案就是在赌平台措辞 —— 赌错的方向是"把删掉的文件当成等待"，
	// 用户会永远等一个不可能成功的任务。
	networkAttempted := false

	// 统一出口：任何失败路径都必须让前端收到反馈，且只发一次。
	//
	// 与过去唯一的区别：以前【所有】失败都置 failed；现在先问一句
	// 「是不是对方暂时够不着」—— 是就转入等待（那不是错误），
	// 由 sweep 在对方上线或退避到点时重试同一条任务。
	defer func() {
		if err == nil {
			return
		}
		if job.JobID == "" {
			job.JobID = jobID
		}
		// 取消【必须最先判】：掉进下面「对端不可达 → 转等待」那条分支，
		// 就变成了「用户点了取消、对方一上线文件又自己跑起来」。
		if errors.Is(err, errJobCancelled) {
			from := string(job.Status)
			job.Status = transfer.StateCancelled
			job.WaitReason = transfer.WaitNone
			job.NextTryAt = time.Time{}
			job.UpdatedAt = a.clk.Now()
			_ = a.store.UpsertJob(job)
			a.clearCancelling(job.JobID)
			a.publishState(job, from, string(transfer.StateCancelled), "")
			err = nil // 取消不是错误
			return
		}
		if networkAttempted && shouldWaitForPeer(failureEvidence{
			PeerReachable: a.peerReachable(peerID),
			SessionAlive:  a.sessionAlive(peerID),
			Err:           err,
		}) {
			if job.Direction == "" {
				// 失败发生在任务建成之前（例如拿不到会话）：从本地文件补一份
				// 完整任务，等待期照样能显示文件名与大小。
				job = carryOverBatch(prev, a.newWaitingJob(jobID, peerID, path, sizeOf(path)))
			}
			job = a.markWaiting(job, jobID, peerID, path, err)
			err = nil // 「等一会儿」不是错误
			return
		}
		job.Status = transfer.StateFailed
		job.Error = err.Error()
		job.WaitReason = transfer.WaitNone
		job.NextTryAt = time.Time{}
		job.UpdatedAt = a.clk.Now()
		_ = a.store.UpsertJob(job)
		if a.bus != nil {
			a.bus.Publish(eventbus.TopicTransferError, eventbus.TransferError{
				JobID: job.JobID, PeerID: peerID.String(), Err: err,
			})
		}
	}()

	// 1) 本地文件先判。顺序是刻意的：文件不在 / 是空文件时不必等对方上线
	//    （等多久都发不出去），也不该占用并发额度。
	chunkSize := a.ChunkSize
	if chunkSize <= 0 {
		chunkSize = transfer.DefaultChunkSize
	}
	st, serr := os.Stat(path)
	if serr != nil {
		return transfer.Job{}, fmt.Errorf("transfer: %w", serr)
	}
	if st.IsDir() != isDir {
		// 两侧都要拦：目录条目把文件当路径（改名/重建后会出现），
		// 文件任务把目录当路径（历史行为，文案保持不变）。
		if st.IsDir() {
			return transfer.Job{}, fmt.Errorf("transfer: %s is a directory", path)
		}
		return transfer.Job{}, fmt.Errorf("transfer: %s 不是目录（任务标记为目录条目）", path)
	}
	statSize := st.Size()
	if isDir {
		statSize = 0 // 目录条目零字节：st.Size() 在目录上是平台相关的元数据大小
	} else if transfer.ChunkCount(statSize, chunkSize) == 0 {
		return transfer.Job{}, fmt.Errorf("transfer: empty file")
	}

	// 2) 对端明显不在 → 直接落等待，连哈希都不做。
	//
	//    这是本次改动的核心：过去会先算完整文件 SHA-256 再拨号失败，
	//    一个 10GB 文件为了"发不出去"要先读一整遍磁盘。
	if !force && !a.peerReachable(peerID) {
		fresh := carryOverBatch(prev, a.newWaitingJob(jobID, peerID, path, statSize))
		if a.lg != nil {
			a.lg.Info("transfer: 对端离线，任务进入等待",
				"job_id", jobID, "peer_id", peerID.String(), "file", fresh.FileName)
		}
		return a.markWaiting(fresh, jobID, peerID, path, nil), nil
	}

	select {
	case a.sendSem <- struct{}{}:
		defer func() { <-a.sendSem }()
	case <-ctx.Done():
		return transfer.Job{}, ctx.Err()
	}

	// 目录条目没有内容可哈希、也没有分块：它的全部就是 META → ACK → DONE。
	fileHash, size := "", statSize
	var chunkHashes []string
	total := 0
	if !isDir {
		h, hs, sz, herr := hashFileChunks(path, chunkSize)
		if herr != nil {
			return transfer.Job{}, fmt.Errorf("transfer: hash %s: %w", path, herr)
		}
		fileHash, chunkHashes, size = h, hs, sz
		if total = transfer.ChunkCount(size, chunkSize); total == 0 {
			return transfer.Job{}, fmt.Errorf("transfer: empty file")
		}
	}

	window := a.WindowSize
	if window <= 0 {
		window = transfer.DefaultWindow
	}

	// 承接历史尝试次数：等待重试的退避靠它递增（1s→2s→…→5min）。
	//
	// 这里必须回读一次库：任务字面量是从零构造的，不带上旧值就等于每次
	// 重试都把计数归零 —— 退避会永远停在第一档，变成"每秒重试一次"。
	// 首次发送查不到行，拿到 0，行为与过去一致。
	retryCount := 0
	if prevErr == nil {
		retryCount = prev.RetryCount
	}

	// job 是命名返回值，此处用 = 而非 :=
	job = transfer.Job{
		JobID:       jobID,
		PeerID:      peerID,
		FileName:    fileBase(path),
		FileSize:    size,
		FileHash:    fileHash,
		ChunkSize:   chunkSize,
		TotalChunks: total,
		Direction:   transfer.DirectionSend,
		LocalPath:   path,
		Bitmap:      transfer.NewChunkBitmap(total),
		Status:      transfer.StateMetaExchange,
		WindowSize:  window,
		RetryCount:  retryCount,
		CreatedAt:   a.clk.Now(),
		UpdatedAt:   a.clk.Now(),
	}
	// 批次字段从旧行承接：这个字面量是从零构造的，漏掉就会在文件真正
	// 开始传的那一刻把批次信息抹成零值（同 RetryCount 的道理）。
	job = carryOverBatch(prev, job)
	if err := a.store.UpsertJob(job); err != nil {
		return job, err
	}

	// 目录条目不打开文件句柄：Windows 上 OpenFile 一个目录会直接失败。
	// sj.f 为 nil 是安全的 —— 下面 runSender 对目录条目根本不跑。
	var f *os.File
	if !isDir {
		f, err = os.OpenFile(path, os.O_RDONLY, 0)
		if err != nil {
			return job, err
		}
		defer f.Close()
	}

	// 从这里开始进入网络阶段：之后的失败才可能是"对端够不着"。
	networkAttempted = true
	sess, err := a.session(ctx, peerID)
	if err != nil {
		return job, err
	}

	sj := &sendJob{
		job:      job,
		f:        f,
		acked:    transfer.NewChunkBitmap(total),
		hashes:   chunkHashes,
		ackCh:    make(chan chunkAck, 32),
		doneCh:   make(chan doneAck, 1),
		lastSent: make(map[int]time.Time),
		cancel:   make(chan struct{}),
	}
	a.putSender(sj)
	defer a.dropSender(job.JobID)

	// 第一段：元数据（始终携带全量 chunk_hashes，简化 > 省流）
	metaPayload, err := protocol.EncodeJSON(protocol.FileMeta{
		JobID:       job.JobID,
		FileID:      fileHash,
		FileName:    job.FileName,
		FileSize:    size,
		ChunkSize:   chunkSize,
		TotalChunks: total,
		ChunkHashes: chunkHashes,
		// 批次字段必须上线：接收端据此还原目录树。漏掉它们，整批文件会被
		// 平铺到下载目录、同名再互相加 (1)。单文件传输时三者是零值，
		// 因 omitempty 不会多出线上体积。
		BatchID: job.BatchID,
		RelPath: job.RelPath,
		IsDir:   isDir,
	})
	if err != nil {
		return job, err
	}
	if err := sess.Send(protocol.New(protocol.TypeFileMeta, metaPayload)); err != nil {
		return job, err
	}

	// 等待对端回位图（可续传起点）
	select {
	case ck := <-sj.ackCh:
		if ck.reject != "" {
			return job, fmt.Errorf("%w: %s", errPeerReject, ck.reject)
		}
		sj.acked.OrBits(ck.bitmap)
	case <-sj.cancel:
		return job, errJobCancelled
	case <-a.clk.After(10 * time.Second):
		return job, fmt.Errorf("transfer: timeout waiting for FILE_META_ACK")
	case <-ctx.Done():
		return job, ctx.Err()
	}

	// 失败时由函数顶部统一的出口发事件，避免这里和那里各发一次。
	// 目录条目零字节零分块：META → ACK → DONE 就是它的全部，没有块要发。
	if !isDir {
		if err := a.runSender(ctx, sess, sj); err != nil {
			return job, err
		}
	}

	// 最后一段：完成
	donePayload, err := protocol.EncodeJSON(protocol.FileDone{JobID: job.JobID})
	if err != nil {
		return job, err
	}
	if err := sess.Send(protocol.New(protocol.TypeFileDone, donePayload)); err != nil {
		return job, err
	}

	select {
	case da := <-sj.doneCh:
		if !da.ok {
			return job, fmt.Errorf("transfer: peer verify failed: %s", da.err)
		}
	case <-a.clk.After(30 * time.Second):
		return job, fmt.Errorf("transfer: timeout waiting for FILE_DONE_ACK")
	case <-ctx.Done():
		return job, ctx.Err()
	}

	job.Bitmap = sj.acked.Copy()
	job.SetCompleted()
	job.Status = transfer.StateDone
	job.UpdatedAt = a.clk.Now()
	_ = a.store.UpsertJob(job)
	// 防御性清理：正常不会带着取消标记跑完，但留着它会让同一个 job_id
	// 的下一次尝试一进门就被判为取消。
	a.clearCancelling(job.JobID)
	if a.bus != nil {
		a.bus.Publish(eventbus.TopicTransferDone, eventbus.TransferDone{
			JobID: job.JobID, PeerID: peerID.String(), Path: path,
		})
	}
	return job, nil
}

func (a *TransferApp) runSender(ctx context.Context, sess ports.Session, sj *sendJob) error {
	total := sj.job.TotalChunks
	window := sj.job.WindowSize
	if window <= 0 {
		window = transfer.DefaultWindow
	}

	const resendAfter = 400 * time.Millisecond
	lastProgress := time.Time{}

	for {
		// 每个检查点都问一次取消：这里同时看两条路径 —— sendJob 上的信号
		// （已经登记 sender 之后）与 app 上的标记（登记之前的窗口）。
		if sj.isCancelled() || a.isCancelling(sj.job.JobID) {
			return errJobCancelled
		}
		// 先排空已到达的 ACK（非阻塞）
		drained := false
		for !drained {
			select {
			case ck := <-sj.ackCh:
				a.applyChunkAck(sj, ck)
			default:
				drained = true
			}
		}
		if sj.acked.AllSet(total) {
			return nil
		}

		// 发送窗口内尚未确认、且超过重发时间的块
		now := a.clk.Now()
		sent := 0
		for i := 0; i < total && sent < window; i++ {
			if sj.acked.IsSet(i) {
				continue
			}
			if t, ok := sj.lastSent[i]; ok && now.Sub(t) < resendAfter {
				continue
			}
			if err := a.sendChunk(sess, sj, i); err != nil {
				return err
			}
			sj.lastSent[i] = now
			sent++
		}

		a.publishSendProgress(sj, &lastProgress)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sj.cancel:
			// 不在这里返回会让取消等到下一个 50ms 节拍才生效 —— 能更快就不拖。
			return errJobCancelled
		case ck := <-sj.ackCh:
			a.applyChunkAck(sj, ck)
		case <-a.clk.After(50 * time.Millisecond):
		}
	}
}

func (a *TransferApp) applyChunkAck(sj *sendJob, ck chunkAck) {
	if ck.bitmap.Len() == 0 {
		return
	}
	sj.acked.OrBits(ck.bitmap)
}

func (a *TransferApp) sendChunk(sess ports.Session, sj *sendJob, index int) error {
	offset := transfer.OffsetOf(index, sj.job.ChunkSize)
	size := transfer.SizeOf(index, sj.job.FileSize, sj.job.ChunkSize)
	if size <= 0 {
		return fmt.Errorf("transfer: chunk %d has zero size", index)
	}
	buf := make([]byte, size)
	if _, err := sj.f.ReadAt(buf, offset); err != nil && err != io.EOF {
		return err
	}
	payload, err := protocol.EncodeFileChunk(protocol.FileChunk{
		JobID: sj.job.JobID, Index: index, Offset: offset, Data: buf,
	})
	if err != nil {
		return err
	}
	return sess.Send(protocol.New(protocol.TypeFileChunk, payload))
}

func (a *TransferApp) publishSendProgress(sj *sendJob, last *time.Time) {
	if a.bus == nil {
		return
	}
	now := a.clk.Now()
	if !last.IsZero() && now.Sub(*last) < a.progressInterval() {
		return
	}
	*last = now
	a.bus.Publish(eventbus.TopicTransferProgress, eventbus.TransferProgress{
		JobID:   sj.job.JobID,
		PeerID:  sj.job.PeerID.String(),
		Percent: percentOf(sj.acked.CompletedCount(), sj.job.TotalChunks),
		Status:  string(transfer.StateTransferring),
	})
}

// ---------------------------------------------------------------------------
// 接收侧
// ---------------------------------------------------------------------------

// HandleFileMeta 创建/续传接收任务，并回传已完成块位图。
func (a *TransferApp) HandleFileMeta(sess ports.Session, f protocol.Frame) error {
	var meta protocol.FileMeta
	if err := protocol.DecodeJSON(f.Payload, &meta); err != nil {
		return err
	}
	peerID := sess.PeerID()
	total := transfer.ChunkCount(meta.FileSize, meta.ChunkSize)
	// 目录条目（空目录）天生没有分块，必须放行；其余情况下 total_chunks == 0
	// 仍按协议非法处理（对方算错了）。
	if total == 0 && !meta.IsDir {
		return fmt.Errorf("transfer: invalid meta (total_chunks=0)")
	}

	// 上限与磁盘空间：都必须在【创建任何东西之前】判。一旦落了库、建了 .part
	// 再拒，列表里就多出一条永远传不完的任务，接收目录里多半个没人认领的文件。
	if reason := a.metaRejectReason(meta); reason != "" {
		return a.sendMetaReject(sess, meta.JobID, reason)
	}
	if reason := a.spaceRejectReason(meta, peerID); reason != "" {
		return a.sendMetaReject(sess, meta.JobID, reason)
	}

	var (
		job    transfer.Job
		handle ports.SinkHandle
	)

	// 注意：任务标识必须采用本次的 meta.JobID（发送方是按新 job_id 发块的），
	// 续传只继承 TempPath / LocalPath / Bitmap —— 若沿用旧 job_id，接收端会认不出新块。
	job = transfer.Job{
		JobID:       meta.JobID,
		PeerID:      peerID,
		FileName:    meta.FileName,
		FileSize:    meta.FileSize,
		FileHash:    meta.FileID,
		ChunkSize:   meta.ChunkSize,
		TotalChunks: total,
		Direction:   transfer.DirectionRecv,
		Bitmap:      transfer.NewChunkBitmap(total),
		Status:      transfer.StateTransferring,
		WindowSize:  a.windowSize(),
		BatchID:     meta.BatchID,
		RelPath:     meta.RelPath,
		IsDir:       meta.IsDir,
		CreatedAt:   a.clk.Now(),
		UpdatedAt:   a.clk.Now(),
	}

	// 目录条目：只建目录。既不参与续传（它没有内容可续，FileID 也是空的），
	// 也不建临时文件 —— 否则下面那个 OpenFile("") 会失败，HandleFileDone 还会
	// 去 Close 一个 nil 句柄。
	if meta.IsDir {
		h, err := a.sink.CreateIn(ports.SinkRequest{
			JobID: meta.JobID, BatchID: meta.BatchID, RelPath: meta.RelPath, IsDir: true,
		})
		if err != nil {
			return err
		}
		job.LocalPath = h.FinalPath
		job.UpdatedAt = a.clk.Now()
		if err := a.store.UpsertJob(job); err != nil {
			return err
		}
		rj := &recvJob{
			job:          job,
			handle:       h,
			bitmap:       transfer.NewChunkBitmap(0),
			expectHashes: meta.ChunkHashes,
			lastFlush:    a.clk.Now(),
			lastAck:      a.clk.Now(),
		}
		a.putReceiver(rj)
		return a.sendMetaAck(sess, rj)
	}

	// 断点续传：同一 peer + 同一 file_hash + 同一相对路径 已有未完成任务则
	// 复用其【临时文件与位图】。批次内必须带上 rel_path（见端口契约）：
	// 同 hash 的两个文件只按 hash 匹配会互相继承落点。
	if existing, ok := a.store.FindResumable(peerID, meta.FileID, meta.RelPath); ok && existing.TempPath != "" {
		inherited := existing.Bitmap.Copy()
		inherited.EnsureLen(total)
		job.Bitmap = inherited
		job.TempPath = existing.TempPath
		job.LocalPath = existing.LocalPath
		job.Completed = int64(inherited.CompletedCount())
		handle = ports.SinkHandle{
			JobID: meta.JobID, TempPath: existing.TempPath, FinalPath: existing.LocalPath,
		}
	} else if meta.BatchID != "" || meta.RelPath != "" {
		// 批次：按相对路径还原目录树（根目录只分配一次）。
		h, err := a.sink.CreateIn(ports.SinkRequest{
			JobID: meta.JobID, BatchID: meta.BatchID, RelPath: meta.RelPath,
		})
		if err != nil {
			return err
		}
		handle = h
		job.LocalPath = h.FinalPath
		job.TempPath = h.TempPath
	} else {
		// 单文件：维持原有的扁平落盘 + 同名去重。
		h, err := a.sink.Create(meta.JobID, meta.FileName)
		if err != nil {
			return err
		}
		handle = h
		job.LocalPath = h.FinalPath
		job.TempPath = h.TempPath
	}

	job.Status = transfer.StateTransferring
	job.UpdatedAt = a.clk.Now()
	if err := a.store.UpsertJob(job); err != nil {
		return err
	}

	fh, err := os.OpenFile(handle.TempPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}

	rj := &recvJob{
		job:          job,
		handle:       handle,
		f:            fh,
		bitmap:       job.Bitmap.Copy(),
		expectHashes: meta.ChunkHashes,
		lastFlush:    a.clk.Now(),
		lastAck:      a.clk.Now(),
	}
	a.putReceiver(rj)

	return a.sendMetaAck(sess, rj)
}

// HandleFileChunk 写入块（WriteAt，天然支持乱序），并按频率回传全量位图。
func (a *TransferApp) HandleFileChunk(sess ports.Session, f protocol.Frame) error {
	c, err := protocol.DecodeFileChunk(f.Payload)
	if err != nil {
		return err
	}
	rj := a.getReceiver(c.JobID)
	if rj == nil {
		return nil // 元数据尚未到达或任务已结束：丢弃
	}
	if rj.job.IsDir {
		// 目录条目没有分块，也没有文件句柄；正常不会收到，收到就忽略。
		return nil
	}
	if _, err := rj.f.WriteAt(c.Data, c.Offset); err != nil {
		return err
	}
	rj.bitmap.Set(c.Index)
	rj.sinceAck++
	rj.sinceFlush++
	rj.dirty = true

	now := a.clk.Now()

	// 位图批量落盘：每 2 s 或每 256 块一次事务（相比每块一次，DB 写次数下降 256 倍）
	if rj.sinceFlush >= a.flushEvery() || now.Sub(rj.lastFlush) >= a.flushInterval() {
		a.persistBitmap(rj, now)
	}

	// ACK 策略：每 32 块或每 500 ms 回一次【全量位图】（非累积 ACK）。
	// 位图已完整时立即回，避免「最后一块到达后发送方只能干等」。
	complete := rj.bitmap.AllSet(rj.job.TotalChunks)
	if complete || rj.sinceAck >= a.ackEvery() || now.Sub(rj.lastAck) >= a.ackInterval() {
		if err := a.sendChunkAck(sess, rj); err == nil {
			rj.sinceAck = 0
			rj.lastAck = now
			// 顺带落盘：ACK 是低频事件，在此刷位图，
			// 使「崩溃后重启」总能从最近一次 ACK 处续传。
			a.persistBitmap(rj, now)
		}
	}

	a.publishRecvProgress(rj)
	return nil
}

func (a *TransferApp) persistBitmap(rj *recvJob, now time.Time) {
	if err := a.store.SaveBitmap(rj.job.JobID, rj.bitmap, int64(rj.bitmap.CompletedCount())); err == nil {
		rj.sinceFlush = 0
		rj.lastFlush = now
		rj.dirty = false
	}
}

// HandleFileDone 校验整体哈希并原子替换。
func (a *TransferApp) HandleFileDone(sess ports.Session, f protocol.Frame) error {
	var done protocol.FileDone
	if err := protocol.DecodeJSON(f.Payload, &done); err != nil {
		return err
	}
	rj := a.getReceiver(done.JobID)
	if rj == nil {
		return nil
	}

	// 目录条目：目录在 META 阶段就已经建好，没有内容可校验、也没有句柄可关。
	if rj.job.IsDir {
		rj.job.Status = transfer.StateDone
		rj.job.UpdatedAt = a.clk.Now()
		_ = a.store.UpsertJob(rj.job)
		a.dropReceiver(rj.job.JobID)
		if err := a.sendDoneAck(sess, done.JobID, true, ""); err != nil {
			return err
		}
		if a.bus != nil {
			a.bus.Publish(eventbus.TopicTransferDone, eventbus.TransferDone{
				JobID: rj.job.JobID, PeerID: rj.job.PeerID.String(), Path: rj.job.LocalPath,
			})
		}
		return nil
	}

	// 落盘 + 关闭句柄，保证校验读到完整数据
	if rj.dirty {
		_ = a.store.SaveBitmap(rj.job.JobID, rj.bitmap, int64(rj.bitmap.CompletedCount()))
	}
	_ = rj.f.Close()

	// 位图不完整 → 要求补洞（不清空位图）
	if missing := rj.bitmap.Missing(rj.job.TotalChunks); len(missing) > 0 {
		_ = a.sendChunkAck(sess, rj)
		rj.f, _ = os.OpenFile(rj.handle.TempPath, os.O_RDWR|os.O_CREATE, 0o644)
		if err := a.sendDoneAck(sess, done.JobID, false, "incomplete bitmap"); err != nil {
			return err
		}
		return nil
	}

	rj.job.Status = transfer.StateVerifying
	_ = a.store.UpsertJob(rj.job)

	ok, bad, err := a.verify(rj)
	if err == nil && !ok {
		// 校验失败【不清空位图】：只清坏块 → 回 TRANSFERRING 重传坏块
		for _, i := range bad {
			rj.bitmap.Clear(i)
		}
		rj.job.Bitmap = rj.bitmap.Copy()
		rj.job.Status = transfer.StateTransferring
		rj.job.UpdatedAt = a.clk.Now()
		_ = a.store.UpsertJob(rj.job)
		rj.f, _ = os.OpenFile(rj.handle.TempPath, os.O_RDWR|os.O_CREATE, 0o644)

		if a.bus != nil {
			a.bus.Publish(eventbus.TopicTransferError, eventbus.TransferError{
				JobID: rj.job.JobID, PeerID: rj.job.PeerID.String(),
				Err: fmt.Errorf("transfer: %d bad chunks, requesting retransmit", len(bad)),
			})
		}
		return a.sendDoneAck(sess, done.JobID, false, "hash mismatch")
	}
	if err != nil {
		if a.lg != nil {
			a.lg.Warn("transfer: verify error", "job_id", rj.job.JobID, "err", err)
		}
		return a.sendDoneAck(sess, done.JobID, false, err.Error())
	}

	final, err := a.sink.Commit(rj.handle)
	if err != nil {
		return a.sendDoneAck(sess, done.JobID, false, err.Error())
	}

	rj.job.Bitmap = rj.bitmap.Copy()
	rj.job.Completed = int64(rj.job.TotalChunks)
	rj.job.Status = transfer.StateDone
	rj.job.LocalPath = final
	rj.job.Error = ""
	rj.job.UpdatedAt = a.clk.Now()
	_ = a.store.UpsertJob(rj.job)
	a.dropReceiver(done.JobID)

	if err := a.sendDoneAck(sess, done.JobID, true, ""); err != nil {
		return err
	}
	if a.bus != nil {
		a.bus.Publish(eventbus.TopicTransferDone, eventbus.TransferDone{
			JobID: rj.job.JobID, PeerID: rj.job.PeerID.String(), Path: final,
		})
	}
	return nil
}

// HandleFileMetaAck 处理对端回的续传位图，或它的拒收。
func (a *TransferApp) HandleFileMetaAck(_ ports.Session, f protocol.Frame) error {
	var ack protocol.FileMetaAck
	if err := protocol.DecodeJSON(f.Payload, &ack); err != nil {
		return err
	}
	// 拒收分支必须在位图解码【之前】：被拒时对端不带位图，而
	// DecodeBase64("") 直接报错，排在后面就永远走不到。
	if ack.Reject != "" {
		if sj := a.getSender(ack.JobID); sj != nil {
			sj.ackCh <- chunkAck{reject: ack.Reject}
		}
		return nil
	}
	bm, err := transfer.DecodeBase64(ack.CompletedBitmap)
	if err != nil {
		return nil
	}
	if sj := a.getSender(ack.JobID); sj != nil {
		sj.ackCh <- chunkAck{bitmap: bm, completed: ack.CompletedChunks}
	}
	return nil
}

// HandleFileChunkAck 吸收周期性全量位图。
func (a *TransferApp) HandleFileChunkAck(_ ports.Session, f protocol.Frame) error {
	var ack protocol.FileChunkAck
	if err := protocol.DecodeJSON(f.Payload, &ack); err != nil {
		return err
	}
	bm, err := transfer.DecodeBase64(ack.Bitmap)
	if err != nil {
		return nil
	}
	if sj := a.getSender(ack.JobID); sj != nil {
		sj.ackCh <- chunkAck{bitmap: bm, completed: ack.CompletedChunks}
	}
	return nil
}

// HandleFileDoneAck 处理完成确认。
func (a *TransferApp) HandleFileDoneAck(_ ports.Session, f protocol.Frame) error {
	var ack protocol.FileDoneAck
	if err := protocol.DecodeJSON(f.Payload, &ack); err != nil {
		return err
	}
	if sj := a.getSender(ack.JobID); sj != nil {
		sj.doneCh <- doneAck{ok: ack.OK, err: ack.Error}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// metaRejectReason 判断这条 FILE_META 该不该拒收，返回给用户看的原因（空串=放行）。
//
// 只针对「超过接收上限」：其余校验（空文件、坏 rel_path）都是协议非法，
// 走的是报错路径而不是拒收路径 —— 那类问题的原因是"对方发错了"，
// 不是"本机不收"，混在一起会让用户去改一个改不动的设置。
func (a *TransferApp) metaRejectReason(meta protocol.FileMeta) string {
	if meta.IsDir || a.MaxFileSize <= 0 {
		return ""
	}
	if meta.FileSize > a.MaxFileSize {
		return fmt.Sprintf("文件超过接收上限（%s）", formatBytes(a.MaxFileSize))
	}
	return ""
}

// sendMetaReject 回一条带原因的 FILE_META_ACK。
//
// 与「直接 return err」的区别：报错只写进本机日志，发送端那边表现为
// 干等 10 秒后的「传输超时」—— 用户看到的原因完全是错的。
func (a *TransferApp) sendMetaReject(sess ports.Session, jobID, reason string) error {
	payload, err := protocol.EncodeJSON(protocol.FileMetaAck{JobID: jobID, Reject: reason})
	if err != nil {
		return err
	}
	return sess.Send(protocol.New(protocol.TypeFileMetaAck, payload))
}

// formatBytes 把字节数写成给用户看的样子（拒绝理由里要出现）。
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	for i, u := range units {
		v /= unit
		if v < unit || i == len(units)-1 {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%d B", n)
}

func (a *TransferApp) sendMetaAck(sess ports.Session, rj *recvJob) error {
	payload, err := protocol.EncodeJSON(protocol.FileMetaAck{
		JobID:           rj.job.JobID,
		CompletedBitmap: rj.bitmap.EncodeBase64(),
		CompletedChunks: rj.bitmap.CompletedCount(),
	})
	if err != nil {
		return err
	}
	return sess.Send(protocol.New(protocol.TypeFileMetaAck, payload))
}

func (a *TransferApp) sendChunkAck(sess ports.Session, rj *recvJob) error {
	payload, err := protocol.EncodeJSON(protocol.FileChunkAck{
		JobID:           rj.job.JobID,
		Bitmap:          rj.bitmap.EncodeBase64(),
		CompletedChunks: rj.bitmap.CompletedCount(),
	})
	if err != nil {
		return err
	}
	return sess.Send(protocol.New(protocol.TypeFileChunkAck, payload))
}

func (a *TransferApp) sendDoneAck(sess ports.Session, jobID string, ok bool, msg string) error {
	payload, err := protocol.EncodeJSON(protocol.FileDoneAck{JobID: jobID, OK: ok, Error: msg})
	if err != nil {
		return err
	}
	return sess.Send(protocol.New(protocol.TypeFileDoneAck, payload))
}

// verify 逐块重算哈希定位坏块，并校验整体哈希。
func (a *TransferApp) verify(rj *recvJob) (bool, []int, error) {
	f, err := os.Open(rj.handle.TempPath)
	if err != nil {
		return false, nil, err
	}
	defer f.Close()

	buf := make([]byte, rj.job.ChunkSize)
	whole := sha256.New()
	bad := make([]int, 0)

	for i := 0; i < rj.job.TotalChunks; i++ {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			whole.Write(buf[:n])
			sum := sha256.Sum256(buf[:n])
			got := hex.EncodeToString(sum[:])
			if i < len(rj.expectHashes) && rj.expectHashes[i] != got {
				bad = append(bad, i)
			}
		}
		if rerr != nil {
			if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
				break
			}
			return false, nil, rerr
		}
	}

	got := hex.EncodeToString(whole.Sum(nil))
	if got != rj.job.FileHash || len(bad) > 0 {
		return false, bad, nil
	}
	return true, nil, nil
}

func (a *TransferApp) publishRecvProgress(rj *recvJob) {
	if a.bus == nil {
		return
	}
	now := a.clk.Now()
	if now.Sub(rj.lastAck) < a.progressInterval() {
		return
	}
	a.bus.Publish(eventbus.TopicTransferProgress, eventbus.TransferProgress{
		JobID:   rj.job.JobID,
		PeerID:  rj.job.PeerID.String(),
		Percent: percentOf(rj.bitmap.CompletedCount(), rj.job.TotalChunks),
		Status:  string(transfer.StateTransferring),
	})
}

func (a *TransferApp) session(ctx context.Context, peerID identity.NodeID) (ports.Session, error) {
	if s, ok := a.conns.SessionOf(peerID); ok {
		return s, nil
	}
	if a.dir == nil {
		// 「立即重试」可以在没有任何目录的情况下被调用（测试与非标准装配）。
		// 这里必须返回错误而不是让 nil 解引用把进程带走 —— 重试路径崩掉
		// 比"这次没连上"严重得多。
		return nil, fmt.Errorf("transfer: no known address for peer %s", peerID)
	}
	p, ok := a.dir.Get(peerID)
	if !ok || p.LastAddr == "" {
		return nil, fmt.Errorf("transfer: no known address for peer %s", peerID)
	}
	return a.conns.Dial(ctx, peerID, p.LastAddr)
}

func (a *TransferApp) putSender(sj *sendJob) {
	a.mu.Lock()
	a.senders[sj.job.JobID] = sj
	a.mu.Unlock()
}
func (a *TransferApp) dropSender(id string) { a.mu.Lock(); delete(a.senders, id); a.mu.Unlock() }
func (a *TransferApp) putReceiver(rj *recvJob) {
	a.mu.Lock()
	a.receivers[rj.job.JobID] = rj
	a.mu.Unlock()
}
func (a *TransferApp) dropReceiver(id string) { a.mu.Lock(); delete(a.receivers, id); a.mu.Unlock() }

func (a *TransferApp) getSender(id string) *sendJob {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.senders[id]
}

func (a *TransferApp) getReceiver(id string) *recvJob {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.receivers[id]
}

// ActiveJobs 返回当前活跃任务数（诊断用）。
func (a *TransferApp) ActiveJobs() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.senders) + len(a.receivers)
}

func (a *TransferApp) windowSize() int {
	if a.WindowSize > 0 {
		return a.WindowSize
	}
	return transfer.DefaultWindow
}

func (a *TransferApp) flushInterval() time.Duration {
	if a.BitmapFlushInterval > 0 {
		return a.BitmapFlushInterval
	}
	return 2 * time.Second
}

func (a *TransferApp) flushEvery() int {
	if a.FlushEveryChunks > 0 {
		return a.FlushEveryChunks
	}
	return 256
}

func (a *TransferApp) ackInterval() time.Duration {
	if a.AckInterval > 0 {
		return a.AckInterval
	}
	return 500 * time.Millisecond
}

func (a *TransferApp) ackEvery() int {
	if a.AckEveryChunks > 0 {
		return a.AckEveryChunks
	}
	return 32
}

func (a *TransferApp) progressInterval() time.Duration {
	if a.ProgressInterval > 0 {
		return a.ProgressInterval
	}
	return 250 * time.Millisecond
}

func percentOf(done, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(done) / float64(total) * 100
}

// hashFileChunks 一次顺序读完成「整体 SHA-256 + 每块 SHA-256」。
func hashFileChunks(path string, chunkSize int64) (string, []string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, 0, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return "", nil, 0, err
	}

	whole := sha256.New()
	hashes := make([]string, 0, transfer.ChunkCount(st.Size(), chunkSize))
	buf := make([]byte, chunkSize)

	for {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			whole.Write(buf[:n])
			sum := sha256.Sum256(buf[:n])
			hashes = append(hashes, hex.EncodeToString(sum[:]))
		}
		if rerr != nil {
			if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
				break
			}
			return "", nil, 0, rerr
		}
	}
	return hex.EncodeToString(whole.Sum(nil)), hashes, st.Size(), nil
}

func fileBase(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[i+1:]
		}
	}
	return path
}
