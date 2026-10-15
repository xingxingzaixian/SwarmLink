package transfer

import (
	"fmt"
	"slices"
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
	//
	// 取值刻意【不】叫 "queued"：那一句查询里紧挨着还有 status = 'queued'
	// （StateIdle 的落库值，见 state.go 的 DBStatus），两个 "queued" 并排
	// 会让读代码的人以为写重了 —— 本仓已经有过一次这类同名撞车。
	WaitQueued WaitReason = "batch_queued"
)

// autoStartableReasons 是【唯一】一份「可自动拉起」的取值清单。
//
// 仓储的 SQL IN 列表（AutoStartableReasons）与调用方的守卫（AutoStartable）
// 都从它派生。各写一遍的分叉方向是「用户取消过的任务自己又跑起来」——
// 那是这个功能最不能出的问题。
var autoStartableReasons = []WaitReason{WaitPeerOffline, WaitQueued}

// AutoStartable 报告该等待原因是否属于「可以自动拉起」的一类。
//
// 未登记的原因一律返回 false，这个方向是刻意选的：将来加了「用户暂停」这类
// 新原因而忘了登记时，后果是「任务停在队列里不动」（响亮、一眼可见），
// 而不是「取消过的任务被重新跑起来」（致命且难复现）。
func (r WaitReason) AutoStartable() bool { return slices.Contains(autoStartableReasons, r) }

// AutoStartableReasons 返回可自动拉起的全部取值（副本）。
//
// 供仓储构造 SQL 的 IN 列表：让「哪些原因能被自动拉起」只有一份真相，
// 而不是领域层一个方法、仓储层一句手写的 IN (?, ?)。
func AutoStartableReasons() []WaitReason { return slices.Clone(autoStartableReasons) }

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
	// ResumeFrom 是【本次尝试的起点块数】：0 表示从头传，>0 表示这次是从第
	// ResumeFrom 块接上的，界面据此显示「续传自 XX%」。
	//
	// 与 Completed 的分工是这个字段存在的全部理由：Completed 随传输往前爬，
	// ResumeFrom 记的是「这一次从哪儿接上」，写下之后不再改动。
	//
	// 它必须落库而不是只放在内存里：重启一次、或者换一次尝试（恢复 / 重试 /
	// 退避后自动重发）之后，用户仍然需要看到「这条是从半截续的」——
	// 否则同一份文件在列表里看起来就像从头传了一遍。
	ResumeFrom int64
	Status     State
	WindowSize int
	RetryCount int
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

// ResumePercent 返回本次尝试的起点百分比 [0,100]；0 表示从头传。
//
// 与 Percent 分开而不是复用它：Percent 读的是位图（随时在涨），这里读的是
// ResumeFrom（这一次从哪儿接上，写死不动的那个数）。界面上一行同时出现
// 「续传自 12%」与「42%」正是两者的分工。
func (j *Job) ResumePercent() float64 {
	if j.TotalChunks <= 0 || j.ResumeFrom <= 0 {
		return 0
	}
	if j.ResumeFrom >= int64(j.TotalChunks) {
		// 对端声称全部已完成：这条任务会立刻收尾，但别让界面出现「续传自 120%」。
		return 100
	}
	return float64(j.ResumeFrom) / float64(j.TotalChunks) * 100
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
