package app

import (
	"os"
	"strings"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// failureEvidence 是判定「等一会儿还是就此失败」所需的证据快照。
//
// 做成一个结构体而不是三个参数：判定必须是【纯函数】才能表驱动测试，
// 而证据的采集（读目录、读连接管理器）留在调用点。
type failureEvidence struct {
	// PeerReachable：对端此刻看起来可达（本机持有它的会话，或目录里在线/已发现且有地址）。
	PeerReachable bool
	// SessionAlive：本机此刻仍持有它的会话。对方掉线后这里会变成 false。
	SessionAlive bool
	Err          error
}

// permanentPatterns 是「看起来像连接问题、其实是永久失败」的错误特征。
//
// 必须先判它们：`unexpected EOF` 是本地读文件读到一半文件被截断，
// 而裸 `EOF` 是连接断开 —— 两者都含 "EOF"，判错方向就再也发不出去
// （要么永远等一个不可能成功的任务，要么把能重试的当永久失败）。
var permanentPatterns = []string{
	"unexpected EOF",
	"peer verify failed",
	"invalid meta",
	"empty file",
	// 本地 IO 的两套文案。网络阶段也可能读到本地文件（发块时 f.ReadAt），
	// 那时文件刚好被删/被截断，同样是真失败。
	//   Windows: "The system cannot find the file specified" / "Access is denied"
	//   POSIX  : "no such file or directory" / "permission denied"
	"no such file",
	"cannot find the file specified",
	"cannot find the path specified",
	"is a directory",
	"permission denied",
	"access is denied",
	"no space left",
}

// unreachablePatterns 是「对端此刻够不着」的错误特征（兜底用）。
//
// 只在主判据（可达性）看不出问题时才看它：文案会随版本变化，
// 把它当主判据早晚会失效。
var unreachablePatterns = []string{
	"no known address for peer",
	"timeout waiting for FILE_META_ACK",
	"timeout waiting for FILE_DONE_ACK",
	// 拨号失败的三种写法：本仓库的包装、Windows 的 connectex、Linux 的 connect。
	"tcp: dial",
	"connectex:",
	"no route to host",
	"network is unreachable",
	"connection reset",
	"broken pipe",
	"connection refused",
	"use of closed network connection",
	"i/o timeout",
}

// shouldWaitForPeer 判断一次失败是「对端暂时够不着」还是「真的失败了」。
//
// 顺序刻意如此：
//  1. 永久失败特征优先 —— 把「文件被删」误判成等待，用户会永远等一个
//     不可能成功的任务；
//  2. 对端不可达 / 会话已消失 → 等（这是本功能的主路径，不依赖任何文案）；
//  3. 兜底看文案（覆盖"目录说在线、对端其实已死"的情况）。
func shouldWaitForPeer(e failureEvidence) bool {
	if e.Err == nil {
		return false
	}
	msg := e.Err.Error()
	if containsAny(msg, permanentPatterns) {
		return false
	}
	if !e.PeerReachable || !e.SessionAlive {
		return true
	}
	return containsAny(msg, unreachablePatterns)
}

// containsAny 报告 s 是否包含任一特征子串。
func containsAny(s string, patterns []string) bool {
	for _, p := range patterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// peerReachable 判断对端是否「值得一试」。
//
// 刻意宽松：目录里 online/discovered 且有地址就算可达 —— 拨号是按需的
// （ADR-009），先试一次再失败，比"没连上就不发"更符合直觉，
// 也不会让"能发但现在没连"这种寻常状态退化成等待。
func (a *TransferApp) peerReachable(peerID identity.NodeID) bool {
	if a.sessionAlive(peerID) {
		return true
	}
	if a.dir == nil {
		return false
	}
	p, ok := a.dir.Get(peerID)
	if !ok || p.LastAddr == "" {
		return false
	}
	return p.State == peer.StateOnline || p.State == peer.StateDiscovered
}

// sessionAlive 报告本机此刻是否仍持有该对端的会话。
func (a *TransferApp) sessionAlive(peerID identity.NodeID) bool {
	if a.conns == nil {
		return false
	}
	_, ok := a.conns.SessionOf(peerID)
	return ok
}

// newWaitingJob 组装一条「等对方上线」的轻量任务。
//
// 刻意只做 stat：等待期不读文件内容（不哈希）。哈希只在真要传的那一刻
// 发生，file_hash 也留到那时再填 —— 它唯一的用处是让对端按
// (peer, file_hash) 找到可续传的临时文件。
func (a *TransferApp) newWaitingJob(jobID string, peerID identity.NodeID, path string, size int64) transfer.Job {
	chunkSize := a.ChunkSize
	if chunkSize <= 0 {
		chunkSize = transfer.DefaultChunkSize
	}
	now := a.clk.Now()
	total := transfer.ChunkCount(size, chunkSize)
	return transfer.Job{
		JobID:       jobID,
		PeerID:      peerID,
		FileName:    fileBase(path),
		FileSize:    size,
		ChunkSize:   chunkSize,
		TotalChunks: total,
		Direction:   transfer.DirectionSend,
		LocalPath:   path,
		Bitmap:      transfer.NewChunkBitmap(total),
		Status:      transfer.StateIdle,
		WindowSize:  a.windowSize(),
		WaitReason:  transfer.WaitPeerOffline,
		NextTryAt:   now.Add(backoff(1, a.BaseBackoff, a.MaxBackoff)),
		RetryCount:  1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// markWaiting 把一个已有任务转入「等待对方上线」并落库。
//
// 不写 job.Error：那不是失败，把原因写进去只会让传输列表整行标红。
// 原因交给日志（排障用）与 WaitReason（界面用）。
func (a *TransferApp) markWaiting(job transfer.Job, jobID string, peerID identity.NodeID, path string, cause error) transfer.Job {
	if job.JobID == "" {
		job = a.newWaitingJob(jobID, peerID, path, sizeOf(path))
	}
	from := string(job.Status)
	next := job.RetryCount + 1
	job.RetryCount = next
	job.Status = transfer.StateIdle
	job.WaitReason = transfer.WaitPeerOffline
	job.Error = ""
	job.NextTryAt = a.clk.Now().Add(backoff(next, a.BaseBackoff, a.MaxBackoff))
	job.UpdatedAt = a.clk.Now()
	_ = a.store.UpsertJob(job)
	if a.lg != nil && cause != nil {
		a.lg.Info("transfer: 对端不可达，转入等待",
			"job_id", job.JobID, "peer_id", peerID.String(), "attempt", next, "err", cause)
	}
	a.publishState(job, from, string(transfer.StateIdle), string(transfer.WaitPeerOffline))
	return job
}

// sizeOf 取文件大小；取不到返回 0（随后会因「空文件」而真失败）。
func sizeOf(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// publishState 广播一次任务状态变化。UI 靠它把「等待对方上线」显示出来。
func (a *TransferApp) publishState(job transfer.Job, from, to, reason string) {
	if a.bus == nil {
		return
	}
	a.bus.Publish(eventbus.TopicTransferState, eventbus.TransferState{
		JobID: job.JobID, From: from, To: to, Reason: reason,
	})
}

// claimStarting 抢占一个 job；已被抢占返回 false。
//
// 抢占必须发生在「交给 goroutine 之前」，否则两个触发点会在真正登记
// （senders 表）之前同时通过检查，各自跑一遍全文件哈希。
func (a *TransferApp) claimStarting(jobID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.starting == nil {
		a.starting = make(map[string]struct{})
	}
	if _, ok := a.starting[jobID]; ok {
		return false
	}
	a.starting[jobID] = struct{}{}
	return true
}

// releaseStarting 释放抢占。
func (a *TransferApp) releaseStarting(jobID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.starting, jobID)
}

// isStarting 报告任务是否已被交给传输 goroutine（取消时要拦住它）。
func (a *TransferApp) isStarting(jobID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.starting[jobID]
	return ok
}
