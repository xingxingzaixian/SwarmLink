package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
)

// defaultMaxFolderEntries 是一次文件夹传输允许的最大条目数（文件 + 目录）。
const defaultMaxFolderEntries = 20000

// errTooManyEntries 标记「条目数超限」。单独一个哨兵值是为了把它与
// 「枚举失败」（权限、IO 错误）分开：前者是用户可行动的（文件夹太大），
// 后者要原样报出来。
var errTooManyEntries = errors.New("文件夹条目过多")

// carryOverBatch 把上一次落库行里的批次字段带进新构造的任务。
//
// 为什么必须有：sendFile 与 newWaitingJob 都是从零构造 Job 再 Upsert，
// 漏掉这里，批次字段会在文件【真正开始传】的那一刻被抹成零值 ——
// 表现为「传输列表里的相对路径没了、文件夹卡片的进度永远算不出来」。
// 与 RetryCount 的承接是同一个道理，因此写在同一个位置。
func carryOverBatch(prev, job transfer.Job) transfer.Job {
	job.BatchID = prev.BatchID
	job.RelPath = prev.RelPath
	job.IsDir = prev.IsDir
	return job
}

// FolderSummary 是 SendFolderAs 的返回值：界面用它渲染一条文件夹卡片。
type FolderSummary struct {
	BatchID    string
	FolderName string
	FileCount  int   // 含目录条目
	TotalSize  int64 // 目录条目算 0
	Skipped    int   // 被跳过的符号链接数
}

// batchRootNameOf 返回文件夹名（卡片标题与 rel_path 的首段都用它）。
func batchRootNameOf(dir string) string { return filepath.Base(dir) }

// SendFolderAs 枚举目录、建好一批排队任务后立即返回。
//
// 为什么立即返回而不是「等整批传完」：传输由既有的三个触发点
// （SweepWaiting / WakePeer / Recover）驱动，且 startJobs 已经保证同一对端
// 同时只传一个 —— 整批天然串行、天然继承离线等待与断点续传，一行新调度
// 都不用写。
//
// 为什么枚举不哈希：一个 10GB 的文件夹为了「开始发送」先读一整遍磁盘，
// 用户会以为程序卡死。哈希仍在单个文件真的要传的那一刻做（同 sendFile），
// 因此这里只 stat。
func (a *TransferApp) SendFolderAs(
	ctx context.Context,
	peerID identity.NodeID,
	dir, batchID string,
) (FolderSummary, error) {
	var sum FolderSummary
	if batchID == "" {
		// 空 batch_id 会让 ListByBatch 退化成「返回所有单文件任务」，
		// 对整体取消而言就是把本机全部单文件传输一起停掉。
		return sum, fmt.Errorf("transfer: batch_id 不能为空")
	}
	if err := a.requireFolderCap(peerID); err != nil {
		return sum, err
	}

	st, err := os.Stat(dir)
	if err != nil {
		return sum, fmt.Errorf("transfer: %w", err)
	}
	if !st.IsDir() {
		return sum, fmt.Errorf("transfer: %s 不是文件夹", dir)
	}

	rootName := batchRootNameOf(dir)
	entries, skipped, err := a.walkFolder(dir, rootName)
	if err != nil {
		return sum, err
	}

	now := a.clk.Now()
	jobs := make([]transfer.Job, 0, len(entries))
	for _, e := range entries {
		// job_id 在本层生成（与 SendFile 不同：那里由外壳先给 id 以便界面
		// 立刻对上事件）。文件夹一次要建几十上百条，让外壳逐个给 id 没有意义
		// —— 界面靠 batch_id 聚合，不靠单个 job_id 先建条目。
		job := a.newWaitingJob(message.NewID(), peerID, e.abs, e.size)
		job.BatchID = batchID
		job.RelPath = e.rel
		job.IsDir = e.isdir
		job.FileName = filepath.Base(e.abs)
		if e.isdir {
			// 目录条目：零字节、无分块。它只需要把目录建出来。
			job.FileSize = 0
			job.TotalChunks = 0
			job.Bitmap = transfer.NewChunkBitmap(0)
		}
		// 排队等的是「前一个传完」，不是时间：不退避（newWaitingJob 给的
		// 1s 退避会让整批每个文件都白等一秒）。
		job.WaitReason = transfer.WaitQueued
		job.NextTryAt = now

		if err := a.store.UpsertJob(job); err != nil {
			return sum, err
		}
		jobs = append(jobs, job)
		sum.FileCount++
		sum.TotalSize += e.size
	}

	sum.BatchID, sum.FolderName, sum.Skipped = batchID, rootName, skipped

	// 立刻试第一轮：否则用户要等下一个 1 秒节拍才看到任何动静。
	// 其余任务由 sweep 依次拉起（同一对端同时只传一个）。
	a.startJobs(ctx, jobs)
	return sum, nil
}

// folderEntry 是枚举出来的一个条目。
type folderEntry struct {
	abs   string // 绝对路径（发送侧直接用）
	rel   string // 相对文件夹根的路径，含根名，已 ToSlash
	size  int64
	isdir bool
}

// walkFolder 枚举目录。
//
// 不做的事（都是刻意的）：不跟随符号链接（会引入循环与越权读取，跨平台
// 语义也不一致）、不读文件内容（不哈希）、不跟随隐藏文件规则（把目录里
// 有什么就发什么）。
func (a *TransferApp) walkFolder(root, rootName string) ([]folderEntry, int, error) {
	limit := a.MaxFolderEntries
	if limit <= 0 {
		limit = defaultMaxFolderEntries
	}

	entries := make([]folderEntry, 0, 32)
	skipped := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil // 根目录本身不单独成条（整目录为空时才补一条）
		}
		if d.Type()&fs.ModeSymlink != 0 {
			// 链接不跟随，但要如实计数：用户需要知道「少了几项」。
			skipped++
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = rootName + "/" + filepath.ToSlash(rel)

		if d.IsDir() {
			// 只把【空目录】单独成条：非空目录会因为它的文件而被建出来，
			// 多一条就是多一次 META/ACK 往返。
			empty, err := isEmptyDir(path)
			if err != nil {
				return err
			}
			if empty {
				entries = append(entries, folderEntry{abs: path, rel: rel, isdir: true})
			}
			if len(entries) > limit {
				return errTooManyEntries
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		entries = append(entries, folderEntry{abs: path, rel: rel, size: info.Size()})
		if len(entries) > limit {
			return errTooManyEntries
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errTooManyEntries) {
			return nil, 0, fmt.Errorf("transfer: 文件夹条目超过 %d 个，已取消发送（%w）", limit, errTooManyEntries)
		}
		return nil, 0, fmt.Errorf("transfer: 枚举 %s: %w", root, err)
	}

	// 整个文件夹为空：为根目录本身补一条目录条目，否则接收端建不出它。
	if len(entries) == 0 {
		entries = append(entries, folderEntry{abs: root, rel: rootName, isdir: true})
	}
	return entries, skipped, nil
}

// requireFolderCap 确认对端支持目录传输。
//
// 不做「偷偷打包成 zip」这类猜测用户意图的降级：老接收端会忽略 rel_path，
// 把整批文件平铺到下载目录里、还会因为同名互相加 (1) —— 那是数据错乱，
// 不是降级。
func (a *TransferApp) requireFolderCap(peerID identity.NodeID) error {
	if a.dir == nil {
		return fmt.Errorf("transfer: 无法确认对方是否支持文件夹传输（节点目录不可用）")
	}
	p, ok := a.dir.Get(peerID)
	if !ok {
		return fmt.Errorf("transfer: 没见过这个对端，无法确认它是否支持文件夹传输")
	}
	if !p.Caps.Has(peer.CapsFolderTransfer) {
		return fmt.Errorf("transfer: 对方版本不支持文件夹传输，请对方升级后再试")
	}
	return nil
}

// isEmptyDir 判断目录是否为空（只读一个名字，不列全表）。
func isEmptyDir(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()

	names, err := f.Readdirnames(1)
	if err != nil && err != io.EOF {
		return false, err
	}
	return len(names) == 0, nil
}
