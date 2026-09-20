package transfer

import (
	"fmt"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
)

// Direction 是传输方向。
type Direction string

const (
	DirectionSend Direction = "send"
	DirectionRecv Direction = "recv"
)

// DefaultChunkSize 是默认分块大小（架构书 4.8：LAN 1MB / 跨网段 256KB / 默认 512KB）。
const DefaultChunkSize int64 = 512 * 1024

// Job 是传输任务聚合。
type Job struct {
	JobID       string
	PeerID      identity.NodeID
	FileName    string
	FileSize    int64
	FileHash    string
	ChunkSize   int64
	TotalChunks int
	Direction   Direction
	LocalPath   string
	TempPath    string
	Completed   int64
	Bitmap      ChunkBitmap
	Status      State
	WindowSize  int
	RetryCount  int
	Error       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ChunkCount 计算总块数（向上取整）。
func ChunkCount(fileSize, chunkSize int64) int {
	if fileSize <= 0 || chunkSize <= 0 {
		return 0
	}
	return int((fileSize + chunkSize - 1) / chunkSize)
}

// OffsetOf 返回第 i 块的字节偏移。
func OffsetOf(i int, chunkSize int64) int64 { return int64(i) * chunkSize }

// SizeOf 返回第 i 块的真实长度（尾块可能不足 chunkSize）。
func SizeOf(i int, fileSize, chunkSize int64) int64 {
	off := OffsetOf(i, chunkSize)
	remain := fileSize - off
	if remain <= 0 {
		return 0
	}
	if remain < chunkSize {
		return remain
	}
	return chunkSize
}

// MissingChunks 返回位图中未完成的块号。
func (j *Job) MissingChunks() []int { return j.Bitmap.Missing(j.TotalChunks) }

// Percent 返回完成百分比 [0,100]。
func (j *Job) Percent() float64 {
	if j.TotalChunks == 0 {
		return 0
	}
	return float64(j.Bitmap.CompletedCount()) / float64(j.TotalChunks) * 100
}

// SetCompleted 更新 completed 计数（由位图推导，属于合理冗余，便于高频 UI 查询）。
func (j *Job) SetCompleted() { j.Completed = int64(j.Bitmap.CompletedCount()) }

// LocateBadChunks 通过逐块哈希比对定位坏块。
// expected[i] 为期望哈希；actual(i) 返回本地重算的第 i 块哈希。
// 校验失败时【不清空位图】，只返回需要重传的坏块号。
func (j *Job) LocateBadChunks(expected []string, actual func(i int) string) []int {
	bad := make([]int, 0)
	for i := 0; i < j.TotalChunks; i++ {
		if !j.Bitmap.IsSet(i) {
			continue // 未完成的块不算「坏」，由 MissingChunks 处理
		}
		if i >= len(expected) {
			bad = append(bad, i)
			continue
		}
		if expected[i] != actual(i) {
			bad = append(bad, i)
		}
	}
	return bad
}

// Validate 校验任务的不变量。
func (j *Job) Validate() error {
	if j.JobID == "" {
		return fmt.Errorf("transfer: empty job id")
	}
	if j.PeerID.IsZero() {
		return fmt.Errorf("transfer: empty peer id")
	}
	if j.ChunkSize <= 0 {
		return fmt.Errorf("transfer: invalid chunk size %d", j.ChunkSize)
	}
	if want := ChunkCount(j.FileSize, j.ChunkSize); want != j.TotalChunks {
		return fmt.Errorf("transfer: total_chunks %d inconsistent with size %d / chunk %d (want %d)",
			j.TotalChunks, j.FileSize, j.ChunkSize, want)
	}
	return nil
}
