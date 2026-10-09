package sqlite

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
)

// Transfers 实现 ports.TransferRepo。
type Transfers struct{ db *DB }

var _ ports.TransferRepo = (*Transfers)(nil)

// NewTransfers 构造传输任务仓储。
func NewTransfers(db *DB) *Transfers { return &Transfers{db: db} }

// transferColumns 是全部列。
//
// 【四处必须严格同序】：这张列清单、INSERT 的列清单、Exec 的实参、scanTransfer
// 的 Scan 目标。错位不会报任何错 —— 只会把值整体读串（例如把 file_size 读成
// file_hash），因此新增列一律加在尾部，加完立刻跑 store 包的用例。
const transferColumns = `job_id, peer_id, file_name, file_size, file_hash, chunk_size, total_chunks,
                         direction, local_path, temp_path, completed, chunk_bitmap, status,
                         window_size, retry_count, wait_reason, next_try_at, error, created_at, updated_at,
                         batch_id, rel_path, is_dir`

// UpsertJob 写入/更新传输任务。
func (t *Transfers) UpsertJob(j transfer.Job) error {
	return t.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
INSERT INTO transfer_jobs(job_id, peer_id, file_name, file_size, file_hash, chunk_size, total_chunks,
                         direction, local_path, temp_path, completed, chunk_bitmap, status,
                         window_size, retry_count, wait_reason, next_try_at, error, created_at, updated_at,
                         batch_id, rel_path, is_dir)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(job_id) DO UPDATE SET
  file_name    = excluded.file_name,
  file_size    = excluded.file_size,
  file_hash    = excluded.file_hash,
  chunk_size   = excluded.chunk_size,
  total_chunks = excluded.total_chunks,
  local_path   = excluded.local_path,
  temp_path    = excluded.temp_path,
  completed    = excluded.completed,
  chunk_bitmap = excluded.chunk_bitmap,
  status       = excluded.status,
  window_size  = excluded.window_size,
  retry_count  = excluded.retry_count,
  wait_reason  = excluded.wait_reason,
  next_try_at  = excluded.next_try_at,
  batch_id     = excluded.batch_id,
  rel_path     = excluded.rel_path,
  is_dir       = excluded.is_dir,
  error        = excluded.error,
  updated_at   = excluded.updated_at`,
			j.JobID, j.PeerID.String(), j.FileName, j.FileSize, j.FileHash, j.ChunkSize, j.TotalChunks,
			string(j.Direction), j.LocalPath, j.TempPath, j.Completed, j.Bitmap.Bytes(), j.Status.DBStatus(),
			j.WindowSize, j.RetryCount, string(j.WaitReason), msOf(j.NextTryAt), j.Error,
			msOf(j.CreatedAt), msOf(j.UpdatedAt),
			j.BatchID, j.RelPath, j.IsDir,
		)
		return err
	})
}

// GetJob 读取传输任务。
func (t *Transfers) GetJob(jobID string) (transfer.Job, error) {
	row := t.db.QueryRow(`SELECT `+transferColumns+` FROM transfer_jobs WHERE job_id = ?`, jobID)
	out, err := scanTransfer(row)
	if err != nil {
		return transfer.Job{}, fmt.Errorf("sqlite: get job %s: %w", jobID, err)
	}
	return out, nil
}

// FindResumable 按 (peer, file_hash) 查找可续传任务。
func (t *Transfers) FindResumable(peerID identity.NodeID, fileHash string) (transfer.Job, bool) {
	row := t.db.QueryRow(`
SELECT `+transferColumns+` FROM transfer_jobs
WHERE peer_id = ? AND file_hash = ? AND status NOT IN ('done','cancelled')
ORDER BY updated_at DESC LIMIT 1`, peerID.String(), fileHash)
	out, err := scanTransfer(row)
	if err != nil {
		return transfer.Job{}, false
	}
	return out, true
}

// SaveBitmap 批量落盘位图与完成计数（避免每块一次事务）。
func (t *Transfers) SaveBitmap(jobID string, bm transfer.ChunkBitmap, completed int64) error {
	return t.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE transfer_jobs SET chunk_bitmap = ?, completed = ?, updated_at = ? WHERE job_id = ?`,
			bm.Bytes(), completed, time.Now().UnixMilli(), jobID,
		)
		return err
	})
}

// ListActive 返回所有未结束的任务（供启动时全局恢复）。
//
// 顺序没有承诺（唯一的消费者 Recover 只看状态），但两种存储必须给出
// 同一种确定性顺序：一边有序、另一边随机，下一个人就会以为它有顺序语义。
func (t *Transfers) ListActive() ([]transfer.Job, error) {
	rows, err := t.db.Query(`
SELECT ` + transferColumns + ` FROM transfer_jobs
WHERE status NOT IN ('done','cancelled') ORDER BY created_at, rel_path, job_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []transfer.Job
	for rows.Next() {
		item, err := scanTransfer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ListWaiting 返回可以被自动拉起的发送任务（语义见 ports.TransferRepo.ListWaiting）。
//
// 取值集合【派生自】transfer.AutoStartableReasons()：手写一句 IN (?, ?) 就等于
// 在仓储层再维护一份「哪些原因能被自动拉起」的真相，而分叉的方向是
// 「用户取消过的任务被重新拉起」。
//
// 「是否到点」刻意不在 SQL 里判：到点判断要用 ports.Clock，留在应用层
// 才能用 clock.Fake 做确定性测试。
func (t *Transfers) ListWaiting() ([]transfer.Job, error) {
	reasons := transfer.AutoStartableReasons()
	args := make([]any, len(reasons))
	for i, r := range reasons {
		args[i] = string(r)
	}
	rows, err := t.db.Query(`
SELECT `+transferColumns+` FROM transfer_jobs
WHERE direction = 'send' AND status = 'queued'
  AND wait_reason IN (`+placeholders(len(reasons))+`)
ORDER BY created_at, rel_path, job_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []transfer.Job
	for rows.Next() {
		item, err := scanTransfer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ListByBatch 返回同一批次的全部任务（语义见 ports.TransferRepo.ListByBatch）。
//
// 按 rel_path 排（目录树顺序），不是按创建时间：批次是同一个循环里建的，
// created_at 以毫秒落库、整批同毫秒是常态，而 job_id 是 UUIDv7（同毫秒内
// 随机）—— 按它们排出来的顺序确定但没有语义，用户看到的文件次序会与
// 他看到的目录结构无关。rel_path 升序是两种存储都能复现的目录树顺序。
func (t *Transfers) ListByBatch(batchID string) ([]transfer.Job, error) {
	if batchID == "" {
		// 空 batch_id 会退化成「返回所有单文件任务」：对 CancelBatch 来说
		// 就是「把本机全部单文件传输一起停掉」。宁可在最底层响亮地拒绝。
		return nil, fmt.Errorf("%w: sqlite", ports.ErrEmptyBatchID)
	}
	rows, err := t.db.Query(`
SELECT `+transferColumns+` FROM transfer_jobs
WHERE batch_id = ?
ORDER BY rel_path, job_id`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []transfer.Job
	for rows.Next() {
		item, err := scanTransfer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ListRecent 返回最近更新的若干任务，包含已结束的（界面的传输记录用）。
//
// 见 ports.TransferRepo.ListRecent：它和 ListActive 的语义完全不同，
// 这里【不】排除 done/cancelled —— 否则用户刚传完的文件会从列表里消失。
func (t *Transfers) ListRecent(limit int) ([]transfer.Job, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := t.db.Query(`
SELECT `+transferColumns+` FROM transfer_jobs
ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []transfer.Job
	for rows.Next() {
		item, err := scanTransfer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// placeholders 生成 n 个逗号分隔的占位符（供 IN 列表拼接）。
//
// n <= 0 时返回 NULL。这里【绝不能】返回空串占位：空串正是 WaitNone 的取值
// （用户取消过），IN (”) 会把「最不该被自动拉起」的那一类整批捞出来。
// IN (NULL) 的比较结果是 unknown，一行都匹配不上 —— 这才是空列表唯一
// 可接受的语义。
func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// PurgeFinished 清理已结束的任务；正在进行的永远不动。
//
// 为什么必须有它：每条记录都带 chunk_bitmap，而界面只看最近若干条 ——
// 没有清理策略的话，这张表会成为应用里唯一持续膨胀的东西。
// 清理放在【启动】时做而不是退出时：异常退出根本走不到退出逻辑。
func (t *Transfers) PurgeFinished(keep int, olderThan time.Time) (int, error) {
	if keep < 0 {
		keep = 0
	}
	var n int
	err := t.db.Write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`
DELETE FROM transfer_jobs
WHERE status IN ('done','failed','cancelled')
  AND updated_at < ?
  AND job_id NOT IN (
    SELECT job_id FROM transfer_jobs
    WHERE status IN ('done','failed','cancelled')
    ORDER BY updated_at DESC LIMIT ?
  )`, msOf(olderThan), keep)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		n = int(affected)
		return nil
	})
	return n, err
}

func scanTransfer(s rowScanner) (transfer.Job, error) {
	var (
		out        transfer.Job
		peerID     string
		direction  string
		bitmap     []byte
		status     string
		tempPath   sql.NullString
		errMsg     sql.NullString
		waitReason string
		nextTryAt  int64
		createdAt  int64
		updatedAt  int64
		batchID    string
		relPath    string
		isDir      int64
	)
	if err := s.Scan(
		&out.JobID, &peerID, &out.FileName, &out.FileSize, &out.FileHash, &out.ChunkSize,
		&out.TotalChunks, &direction, &out.LocalPath, &tempPath, &out.Completed, &bitmap,
		&status, &out.WindowSize, &out.RetryCount, &waitReason, &nextTryAt, &errMsg,
		&createdAt, &updatedAt, &batchID, &relPath, &isDir,
	); err != nil {
		return out, err
	}

	if id, err := identity.ParseNodeID(peerID); err == nil {
		out.PeerID = id
	}
	out.Direction = transfer.Direction(direction)
	out.TempPath = nullString(tempPath)
	// wait_reason 是 NOT NULL 的 TEXT，可以直接扫成 string，不必走 NullString。
	out.WaitReason = transfer.WaitReason(waitReason)
	// next_try_at 用 timeOf 还原（msOf 的逆运算：0 ↔ 零值时间）。
	out.NextTryAt = timeOf(nextTryAt)
	out.BatchID = batchID
	out.RelPath = relPath
	// is_dir 是 INTEGER 0/1（列定义见 migrations.go 的 v4）。
	out.IsDir = isDir != 0
	out.Error = nullString(errMsg)
	out.Bitmap = transfer.NewChunkBitmapFromBytes(bitmap)
	out.Bitmap.EnsureLen(out.TotalChunks)
	out.Status = dbStatusToState(status)
	out.CreatedAt = timeOf(createdAt)
	out.UpdatedAt = timeOf(updatedAt)
	return out, nil
}

// dbStatusToState 把 transfer_jobs.status 的值域映射回领域状态。
//
// 注意：'active' 与 'queued' 都映射为领域状态，因为 DB 的取值集合
// （queued/active/paused/verifying/done/failed/cancelled）比领域状态机更粗。
func dbStatusToState(s string) transfer.State {
	switch s {
	case "queued":
		return transfer.StateIdle
	case "active":
		return transfer.StateTransferring
	case "paused":
		return transfer.StatePaused
	case "verifying":
		return transfer.StateVerifying
	case "done":
		return transfer.StateDone
	case "cancelled":
		return transfer.StateCancelled
	default:
		return transfer.StateFailed
	}
}
