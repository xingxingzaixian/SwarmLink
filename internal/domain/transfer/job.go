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

// WaitReason 说明一个【还没开始】的任务在等什么。
//
// 空串 = 不在等待。它必须落库而不只是内存标记：sweep 只允许自动拉起
// AutoStartable 的任务 —— 用户取消过的（WaitNone）绝不能被自动重试。
type WaitReason string

const (
	WaitNone        WaitReason = ""
	WaitPeerOffline WaitReason = "peer_offline"
	// WaitQueued 表示「排在文件夹批次队列里，轮到就传」。
	//
	// 与 WaitPeerOffline 一样属于「我们自己会拉起」的任务：文件夹的每个文件
	// 都是一条独立任务，靠这个原因排队，由既有调度按序开跑。
	WaitQueued WaitReason = "queued"
)

// AutoStartable 报告该等待原因是否属于「可以自动拉起」的一类。
//
// 这个判断同时被仓储（ListWaiting 的 SQL）与应用层（startJobs 的守卫）使用。
// 两处各写一遍迟早会分叉，而分叉的方向是「用户取消过的任务自己又跑起来」——
// 那是这个功能最不能出的问题，因此收敛成一个方法。
func (r WaitReason) AutoStartable() bool {
	return r == WaitPeerOffline || r == WaitQueued
}

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
	// WaitReason / NextTryAt 只在 Status 为 StateIdle（"还没开始"）时有意义：
	// 前者说明在等谁，后者说明什么时候可以再试。
	WaitReason WaitReason
	NextTryAt  time.Time
	// 文件夹批次（BatchID 非空时才有意义）。单文件传输时三者都是零值，
	// 因此老路径的行为一个字都不变。
	//
	// RelPath 是【发送方看到的】相对路径（含根名，如 `照片/2024/a.jpg`）：
	// 接收端靠它还原目录树，发送端靠它显示「这是文件夹里的哪个文件」。
	// IsDir 是空目录占位条目：零字节、无分块，只用于在接收端建出目录。
	BatchID string
	RelPath string
	IsDir   bool

	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
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
