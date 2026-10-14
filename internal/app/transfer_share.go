package app

import (
	"context"
	"fmt"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
)

// SharedSendRequest 是「共享方把共享内的一个文件推给请求方」的入参。
//
// 做成结构体而不是五个位置参数：这些字段几乎全是 string，位置写错
// （把 batchID 传成 relPath）编译期不会报错，运行期表现为「文件落到了
// 别的目录」—— 那是最难从现象反推原因的一类 bug。
type SharedSendRequest struct {
	// PeerID 是请求方（也是接收方）。
	PeerID identity.NodeID
	// LocalPath 是共享方磁盘上该文件的绝对路径。
	LocalPath string
	// BatchID 由【请求方】生成，整批共享下载共用一个：接收端靠它
	// 只分配一次批次根目录，并把这一批聚合到同一张卡片上。
	BatchID string
	// RelPath 是接收端目录树里的位置，首段必须是共享名（见 share.JoinRelPath）。
	RelPath string
	// IsDir 表示这是空目录占位项（零字节、无分块）。
	IsDir bool
}

// Session 返回与某对端的可用会话，必要时按需拨号。
//
// 导出是给 ShareApp 用的：共享的清单请求与拉取请求都走 TCP，而
// 「复用已有会话 → 从目录取地址 → 按需拨号」这条规则必须只有一份实现
// （ADR-009）。让 ShareApp 自己再写一遍，迟早会在其中一处漏掉复用。
func (a *TransferApp) Session(ctx context.Context, peerID identity.NodeID) (ports.Session, error) {
	return a.session(ctx, peerID)
}

// SendSharedFile 把共享里的一个文件排进发送队列。
//
// 与 SendFileAs 的差别只有三点，都是为了「一批文件」服务：
//   - batch_id / rel_path 由调用方给定（接收端据此还原目录树）；
//   - 排队而不是立刻抢占：同一对端同时只传一个，其余排在同一批次里等轮次；
//   - 不返回错误就等于「已入队」——真正的失败由 transfer.error 事件告知。
//
// 复用整条既有发送链路（FILE_META → 位图 → 分块 → 校验）意味着
// 共享下载天然获得断点续传：接收端按 (peer, file_hash, rel_path) 找到
// 上次的半截临时文件（TransferRepo.FindResumable），位图回传后只补缺块。
func (a *TransferApp) SendSharedFile(ctx context.Context, req SharedSendRequest) (transfer.Job, error) {
	if req.BatchID == "" {
		return transfer.Job{}, fmt.Errorf("transfer: 共享下载批次 id 不能为空")
	}
	if req.RelPath == "" {
		return transfer.Job{}, fmt.Errorf("transfer: 共享下载缺少相对路径")
	}
	if req.PeerID.IsZero() {
		return transfer.Job{}, fmt.Errorf("transfer: 共享下载缺少目标对端")
	}

	job := a.newWaitingJob(message.NewID(), req.PeerID, req.LocalPath, sizeOf(req.LocalPath))
	job.BatchID = req.BatchID
	job.RelPath = req.RelPath
	job.IsDir = req.IsDir
	if req.IsDir {
		// 目录条目：零字节、无分块。sendFile 会在 os.Stat 之前按 IsDir 分流，
		// 因此这里的 size 取值不影响它 —— 但把它显式置零能让落库行自洽
		// （sqlite 的 total_chunks 与 file_size 会一起是 0）。
		job.FileSize = 0
		job.TotalChunks = 0
		job.Bitmap = transfer.NewChunkBitmap(0)
	}
	// 排队等的是「前一个传完」，不是时间：与 SendFolderAs 同一口径，
	// 给 1s 退避会让整批里每个文件都白等一秒。
	job.WaitReason = transfer.WaitQueued
	job.NextTryAt = a.clk.Now()

	if err := a.store.UpsertJob(job); err != nil {
		return transfer.Job{}, err
	}
	// 立刻试第一轮：对端空闲的话这一批的第一个文件马上开始，
	// 其余由 SweepWaiting 依次拉起（同一对端同时只传一个）。
	a.startJobs(ctx, []transfer.Job{job})
	return job, nil
}
