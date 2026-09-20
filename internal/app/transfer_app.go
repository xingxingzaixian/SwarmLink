package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	// BitmapFlushInterval / FlushEveryChunks 控制位图批量落盘（P1-9）。
	BitmapFlushInterval time.Duration
	FlushEveryChunks    int
	// AckInterval / AckEveryChunks 控制接收侧回传全量位图的频率。
	AckInterval    time.Duration
	AckEveryChunks int
	// ProgressInterval 是进度事件节流间隔（4~10 Hz，P1-10）。
	ProgressInterval time.Duration

	mu        sync.Mutex
	senders   map[string]*sendJob
	receivers map[string]*recvJob

	sendSem chan struct{}
}

type chunkAck struct {
	bitmap    transfer.ChunkBitmap
	completed int
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
}

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
		BitmapFlushInterval: 2 * time.Second,
		FlushEveryChunks:    256,
		AckInterval:         500 * time.Millisecond,
		AckEveryChunks:      32,
		ProgressInterval:    250 * time.Millisecond,
		senders:             make(map[string]*sendJob),
		receivers:           make(map[string]*recvJob),
		sendSem:             make(chan struct{}, maxConc),
	}
}

// ---------------------------------------------------------------------------
// 发送侧
// ---------------------------------------------------------------------------

// SendFile 发送单个文件，阻塞直到完成/失败。
//
// 流程：FILE_META →（对端回已完成位图）→ 窗口内发送缺失块 →
// （对端周期性回全量位图，据此补洞）→ FILE_DONE → 校验结果。
func (a *TransferApp) SendFile(ctx context.Context, peerID identity.NodeID, path string) (transfer.Job, error) {
	select {
	case a.sendSem <- struct{}{}:
		defer func() { <-a.sendSem }()
	case <-ctx.Done():
		return transfer.Job{}, ctx.Err()
	}

	chunkSize := a.ChunkSize
	if chunkSize <= 0 {
		chunkSize = transfer.DefaultChunkSize
	}

	fileHash, chunkHashes, size, err := hashFileChunks(path, chunkSize)
	if err != nil {
		return transfer.Job{}, fmt.Errorf("transfer: hash %s: %w", path, err)
	}
	total := transfer.ChunkCount(size, chunkSize)
	if total == 0 {
		return transfer.Job{}, fmt.Errorf("transfer: empty file")
	}

	window := a.WindowSize
	if window <= 0 {
		window = transfer.DefaultWindow
	}

	job := transfer.Job{
		JobID:       message.NewID(),
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
		CreatedAt:   a.clk.Now(),
		UpdatedAt:   a.clk.Now(),
	}
	if err := a.store.UpsertJob(job); err != nil {
		return job, err
	}

	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return job, err
	}
	defer f.Close()

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
		sj.acked.OrBits(ck.bitmap)
	case <-a.clk.After(10 * time.Second):
		return job, fmt.Errorf("transfer: timeout waiting for FILE_META_ACK")
	case <-ctx.Done():
		return job, ctx.Err()
	}

	if err := a.runSender(ctx, sess, sj); err != nil {
		job.Status = transfer.StateFailed
		job.Error = err.Error()
		_ = a.store.UpsertJob(job)
		if a.bus != nil {
			a.bus.Publish(eventbus.TopicTransferError, eventbus.TransferError{
				JobID: job.JobID, PeerID: peerID.String(), Err: err,
			})
		}
		return job, err
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
			job.Status = transfer.StateFailed
			job.Error = da.err
			_ = a.store.UpsertJob(job)
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
	if total == 0 {
		return fmt.Errorf("transfer: invalid meta (total_chunks=0)")
	}

	var (
		job    transfer.Job
		handle ports.SinkHandle
	)

	// 断点续传：同一 peer + 同一 file_hash 已有未完成任务则复用其【临时文件与位图】。
	//
	// 注意：任务标识必须采用本次的 meta.JobID（发送方是按新 job_id 发块的），
	// 只继承 TempPath / LocalPath / Bitmap —— 若沿用旧 job_id，接收端会认不出新块。
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
		CreatedAt:   a.clk.Now(),
		UpdatedAt:   a.clk.Now(),
	}

	if existing, ok := a.store.FindResumable(peerID, meta.FileID); ok && existing.TempPath != "" {
		inherited := existing.Bitmap.Copy()
		inherited.EnsureLen(total)
		job.Bitmap = inherited
		job.TempPath = existing.TempPath
		job.LocalPath = existing.LocalPath
		job.Completed = int64(inherited.CompletedCount())
		handle = ports.SinkHandle{
			JobID: meta.JobID, TempPath: existing.TempPath, FinalPath: existing.LocalPath,
		}
	} else {
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

// HandleFileMetaAck 处理对端回的续传位图。
func (a *TransferApp) HandleFileMetaAck(_ ports.Session, f protocol.Frame) error {
	var ack protocol.FileMetaAck
	if err := protocol.DecodeJSON(f.Payload, &ack); err != nil {
		return err
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
