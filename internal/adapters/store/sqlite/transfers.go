package sqlite

import (
	"database/sql"
	"fmt"
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

const transferColumns = `job_id, peer_id, file_name, file_size, file_hash, chunk_size, total_chunks,
                         direction, local_path, temp_path, completed, chunk_bitmap, status,
                         window_size, retry_count, error, created_at, updated_at`

// UpsertJob 写入/更新传输任务。
func (t *Transfers) UpsertJob(j transfer.Job) error {
	return t.db.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
INSERT INTO transfer_jobs(job_id, peer_id, file_name, file_size, file_hash, chunk_size, total_chunks,
                         direction, local_path, temp_path, completed, chunk_bitmap, status,
                         window_size, retry_count, error, created_at, updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
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
  error        = excluded.error,
  updated_at   = excluded.updated_at`,
			j.JobID, j.PeerID.String(), j.FileName, j.FileSize, j.FileHash, j.ChunkSize, j.TotalChunks,
			string(j.Direction), j.LocalPath, j.TempPath, j.Completed, j.Bitmap.Bytes(), j.Status.DBStatus(),
			j.WindowSize, j.RetryCount, j.Error, msOf(j.CreatedAt), msOf(j.UpdatedAt),
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
func (t *Transfers) ListActive() ([]transfer.Job, error) {
	rows, err := t.db.Query(`
SELECT ` + transferColumns + ` FROM transfer_jobs
WHERE status NOT IN ('done','cancelled') ORDER BY created_at`)
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

func scanTransfer(s rowScanner) (transfer.Job, error) {
	var (
		out       transfer.Job
		peerID    string
		direction string
		bitmap    []byte
		status    string
		tempPath  sql.NullString
		errMsg    sql.NullString
		createdAt int64
		updatedAt int64
	)
	if err := s.Scan(
		&out.JobID, &peerID, &out.FileName, &out.FileSize, &out.FileHash, &out.ChunkSize,
		&out.TotalChunks, &direction, &out.LocalPath, &tempPath, &out.Completed, &bitmap,
		&status, &out.WindowSize, &out.RetryCount, &errMsg, &createdAt, &updatedAt,
	); err != nil {
		return out, err
	}

	if id, err := identity.ParseNodeID(peerID); err == nil {
		out.PeerID = id
	}
	out.Direction = transfer.Direction(direction)
	out.TempPath = nullString(tempPath)
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
