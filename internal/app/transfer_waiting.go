package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
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

// errPeerReject 是「对端明确拒收这条传输」的哨兵错误（例如超过它的接收上限）。
//
// 必须与「对端够不着」分开：拒收是终态，重试一万次也是同一个结果。
// 混进等待路径的后果有两个：界面上是一条永远转圈的任务，而且每次退避到点
// 都会把同一个注定被拒的文件重发一遍。
var errPeerReject = errors.New("对端拒收")

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
	// 对端明确拒收：终态，与可达性无关，也压过下面所有文案特征。
	// 必须排在最前 —— 拒收理由里也可能凑巧含某条特征字样。
	if errors.Is(e.Err, errPeerReject) {
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
		// 尝试次数从 0 开始：递增这件事【只由 markWaiting 做】。
		// 这里若写成 1，第一次进入等待就成了「第 2 次尝试」，
		// 整条退避曲线提前一档（首档变成 2s 而不是 1s）。
		RetryCount: 0,
		NextTryAt:  now.Add(backoff(1, a.BaseBackoff, a.MaxBackoff)),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

// markWaiting 把一个任务转入「等待对方上线」并落库。
//
// 【所有进入等待的路径都必须经过这里】：尝试次数递增、创建时间承接、
// 清掉错误留痕、发状态事件，这几件事只写一遍 —— 少写一处就会出现
// 「某条路径的退避永远停在第一档」或「等待任务在列表里乱序」。
//
// 不写 job.Error：那不是失败，把原因写进去只会让传输列表整行标红。
// 原因交给日志（排障用）与 WaitReason（界面用）。
func (a *TransferApp) markWaiting(job transfer.Job, jobID string, peerID identity.NodeID, path string, cause error) transfer.Job {
	if job.JobID == "" {
		job.JobID = jobID
	}
	if job.LocalPath == "" {
		job.LocalPath = path
	}

	// 承接历史：退避档位靠 RetryCount，列表顺序靠 CreatedAt，两者都不能丢。
	// 取 max 而不是直接覆盖：调用方可能已经带上了历史（尝试中建的任务字面量），
	// 也可能是全新构造的（早退路径的 newWaitingJob，计数从 1 开始）。
	if prev, err := a.store.GetJob(job.JobID); err == nil {
		if prev.RetryCount > job.RetryCount {
			job.RetryCount = prev.RetryCount
		}
		if !prev.CreatedAt.IsZero() {
			job.CreatedAt = prev.CreatedAt
		}
	}

	from := string(job.Status)
	job.RetryCount++
	job.Status = transfer.StateIdle
	job.WaitReason = transfer.WaitPeerOffline
	job.Error = ""
	job.NextTryAt = a.clk.Now().Add(backoff(job.RetryCount, a.BaseBackoff, a.MaxBackoff))
	job.UpdatedAt = a.clk.Now()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = job.UpdatedAt
	}
	_ = a.store.UpsertJob(job)
	if a.lg != nil && cause != nil {
		a.lg.Info("transfer: 对端不可达，转入等待",
			"job_id", job.JobID, "peer_id", peerID.String(), "attempt", job.RetryCount, "err", cause)
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

// Start 订阅「对端上线」类事件，作为等待任务的主要触发源。
//
// 与 ChatApp.Start 同构：peer.online（TCP 握手成功）与 peer.discovered
// （UDP 通告）都要订阅 —— 只订阅前者会漏掉「对端一直在广播但没建连」
// 这种最常见的离线恢复场景（那时它是 discovered，不是 online）。
func (a *TransferApp) Start(ctx context.Context) {
	// 调度补位循环与事件订阅无关，所以放在 bus 判空之前：它是「额度一空就
	// 立刻拉下一条」的唯一实现，少了它并发补位就退回 1 秒节拍，一个几百个
	// 小文件的批次会慢一个数量级。ctx 结束即退出。
	go a.schedulerLoop(ctx)

	if a.bus == nil {
		return
	}
	a.bus.Subscribe(eventbus.TopicPeerOnline, func(payload any) {
		ev, ok := payload.(eventbus.PeerOnline)
		if !ok {
			return
		}
		id, err := identity.ParseNodeID(ev.NodeID)
		if err != nil {
			return
		}
		go a.WakePeer(ctx, id)
	})
	a.bus.Subscribe(eventbus.TopicPeerDiscovered, func(payload any) {
		p, ok := payload.(peer.Peer)
		if !ok {
			return
		}
		if p.State != peer.StateOnline && p.State != peer.StateDiscovered {
			return
		}
		go a.WakePeer(ctx, p.NodeID)
	})
}

// SweepWaiting 扫描等待任务，启动「到点」的那些。
//
// 由 bootstrap 的 1 秒节拍驱动（与 outbox 同一套）：peer.online 是主触发源，
// 这里兜住其余情况 —— 对端早就在线、退避到期、以及事件丢了。
func (a *TransferApp) SweepWaiting(ctx context.Context) {
	jobs, err := a.store.ListWaiting()
	if err != nil {
		return
	}
	a.startDue(ctx, jobs)
}

// WakePeer 对端刚上线：无视退避，立刻试它那些等待任务。
func (a *TransferApp) WakePeer(ctx context.Context, peerID identity.NodeID) {
	jobs, err := a.store.ListWaiting()
	if err != nil {
		return
	}
	due := make([]transfer.Job, 0, len(jobs))
	for _, j := range jobs {
		if j.PeerID == peerID {
			due = append(due, j)
		}
	}
	a.startJobs(ctx, due)
}

// startDue 启动到点的任务（退避未到的留到下一轮）。
func (a *TransferApp) startDue(ctx context.Context, jobs []transfer.Job) {
	now := a.clk.Now()
	due := make([]transfer.Job, 0, len(jobs))
	for _, j := range jobs {
		if !j.NextTryAt.IsZero() && j.NextTryAt.After(now) {
			continue
		}
		due = append(due, j)
	}
	a.startJobs(ctx, due)
}

// startJobs 尝试启动给定任务：受【全局】并发额度约束，不再有每对端限制。
//
// 为什么不再限制每对端（D-11）：那条规则本来就只写在调度路径上，
// 而用户直接发送的多个文件从来不受它约束（各自独立 goroutine 抢全局额度）——
// 保留它等于让「文件夹批次」比「手动一个个发」慢 N 倍。防哈希风暴靠的
// 也不是它，而是下面这行：额度在【哈希之前】就已经占住了。
//
// 额度占不到就【停止本轮】而不是继续起 goroutine：一个几千条的批次会因此
// 变成几千个阻塞在信号量上的 goroutine，而且它们身上的取消标记要等到
// 真拿到额度、白算完一整遍哈希之后才会被看到。剩下的留给下一轮
// （sweep 的 1 秒节拍，以及槽位释放时的 kick）—— 它们不会丢。
func (a *TransferApp) startJobs(ctx context.Context, jobs []transfer.Job) {
	for _, j := range jobs {
		// 只自动拉起「可自动拉起」的任务：等对方上线的，以及文件夹批次里
		// 排队等轮到的（WaitQueued）。
		//
		// 仓储层（ListWaiting）已经按同一份取值清单过滤过一遍，这里再守一道：
		// 取消过的任务只是状态变了（WaitReason 被清成 WaitNone），若某个实现
		// 漏了过滤，它就会被自动重试 —— 用户点了「取消」之后文件自己又传起来，
		// 是最难解释的那类 bug。
		//
		// 判据用 AutoStartable() 而不是在这里手写成员比较：手写就会出现
		// 第三份「哪些原因能被拉起」的真相（仓储 SQL、领域方法、这里），
		// 而三处迟早会分叉。
		if !j.WaitReason.AutoStartable() {
			continue
		}
		if !a.claimStarting(j.JobID) {
			continue
		}
		if !a.trySendSlot() {
			// 额度满了：连它一起放回队列（抢占也要退掉），本轮到此为止。
			a.releaseStarting(j.JobID)
			return
		}
		go func(job transfer.Job) {
			defer a.releaseStarting(job.JobID)
			// 额度由调度器预留，由这里归还（sendFile 收 slotHeld=true）。
			defer a.releaseSendSlot()
			_, _ = a.sendFile(ctx, job.PeerID, job.LocalPath, job.JobID, false, true)
		}(j)
	}
}

// errJobCancelled 表示用户主动取消了这条任务。
//
// 必须是独立的哨兵值：顶层失败出口要据此【先判取消】，否则取消会掉进
// 「对端不可达 → 转等待」那条分支，变成一个「点了取消、对方一上线又自己
// 跑起来」的任务。与 errAppInterrupted 分开也是同理：一个是用户意图，
// 一个是上次没走完，两者的落库结果不同。
var errJobCancelled = errors.New("transfer: 任务已取消")

// markCancelling 记下「该任务已被要求取消」。
//
// 存在的理由是一条时间窗口：sendFile 在登记 sender 之前要读完整文件算哈希，
// 大文件是分钟级，而用户恰恰最容易在「正在准备」时点取消 —— 那段窗口里
// 没有 sendJob 可以发信号。
func (a *TransferApp) markCancelling(jobID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancelling == nil {
		a.cancelling = make(map[string]struct{})
	}
	a.cancelling[jobID] = struct{}{}
}

// isCancelling 报告该任务是否被要求取消（发送路径的每个检查点都问它）。
func (a *TransferApp) isCancelling(jobID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.cancelling[jobID]
	return ok
}

// clearCancelling 清掉取消标记（任务落终态时调用）。
//
// 【必须清】：不清的话同一个 job_id 的下一次尝试一进门就会被判为取消，
// 表现为「这个文件从此再也发不出去」。
func (a *TransferApp) clearCancelling(jobID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.cancelling, jobID)
}

// cancelSender 给正在跑的任务发取消信号；没有在跑的任务返回 false。
func (a *TransferApp) cancelSender(jobID string) bool {
	a.mu.Lock()
	sj := a.senders[jobID]
	a.mu.Unlock()
	if sj == nil {
		return false
	}
	sj.requestCancel()
	return true
}

// errAppInterrupted 标记「上次运行没走完」。
//
// 它不是对端的问题：用同一个错误值是为了让界面与日志里的措辞一致，
// 而不是各写各的字符串。
var errAppInterrupted = errors.New("传输被中断（应用已退出）")

// Recover 收尾上一次运行留下的任务，并立刻试一轮等待中的任务。
//
// 两件事都只在启动时做一次：
//  1. 崩溃残留的 active 任务 —— 它们既不会自己继续、也不会失败，
//     界面上表现为「永远传输中」的僵尸行（ListActive 一直没人调用，
//     这个洞是本次顺带补上的）；
//  2. 等待中的任务 —— 退出期间对端可能已经上线，不该干等到下一个事件。
func (a *TransferApp) Recover(ctx context.Context) {
	if active, err := a.store.ListActive(); err == nil {
		for _, j := range active {
			switch j.Status {
			case transfer.StateMetaExchange, transfer.StateTransferring, transfer.StateVerifying:
			default:
				continue
			}
			if j.Direction == transfer.DirectionSend {
				// 发送侧：转入等待，随后由下面这一轮立刻重试。
				// 续传起点由对端回传的位图决定（接收侧的临时文件与位图还在）。
				a.markWaiting(j, j.JobID, j.PeerID, j.LocalPath, errAppInterrupted)
				continue
			}
			// 接收侧：标失败即可 —— FindResumable 只排除 done/cancelled，
			// failed 行照样会被对方的下一轮 FILE_META 继承并续传。
			j.Status = transfer.StateFailed
			j.Error = errAppInterrupted.Error()
			j.WaitReason = transfer.WaitNone
			j.NextTryAt = time.Time{}
			j.UpdatedAt = a.clk.Now()
			_ = a.store.UpsertJob(j)
			if a.bus != nil {
				a.bus.Publish(eventbus.TopicTransferError, eventbus.TransferError{
					JobID: j.JobID, PeerID: j.PeerID.String(), Err: errAppInterrupted,
				})
			}
		}
	}

	// 批次根目录必须在重启后【认领】回来：根目录当初可能是「退到唯一名」的
	// 结果（同名文件挡路），重新解析可能得到另一个目录，把一个文件夹拆成
	// 两份，用户看到的是「文件少了一半」。
	//
	// 刻意【另起一轮】ListActive 而不是折进上面那个循环：上面按状态过滤，
	// 只看「上次运行被打断」的三种状态；而批次里还没轮到的任务状态是 idle，
	// 它们的根目录同样要认领 —— 折进去会让这批任务重新分配根。
	//
	// 只用接收侧的任务：发送侧任务的 local_path 是用户自己的源文件路径，
	// 拿它当接收根就等于把接收数据写进用户源文件夹（filesink.AdoptRoot
	// 另有一道接收目录包含性校验兜底，这里是第一道）。
	if a.sink != nil {
		if active, err := a.store.ListActive(); err == nil {
			for _, j := range active {
				if j.BatchID == "" || j.Direction != transfer.DirectionRecv || j.LocalPath == "" {
					continue
				}
				if root, ok := batchRootOf(j); ok {
					a.sink.AdoptRoot(j.BatchID, root)
				}
			}
		}
	}

	// 等待中的任务：无视退避立刻试一轮（退出期间对端可能已经上线了）。
	if jobs, err := a.store.ListWaiting(); err == nil {
		a.startJobs(ctx, jobs)
	}
}

// ---------------------------------------------------------------------------
// 并发额度（全局，不再有每对端限制：D-11）
//
// 额度只有一个语义来源：sendSem 的容量。过去它只在 sendFile 里阻塞获取，
// 于是「到点就起 goroutine、再阻塞排队」—— 批次的队列长度直接变成 goroutine
// 数量。现在调度路径改成先占位再起（见 startJobs），两者数量严格相等。
// ---------------------------------------------------------------------------

// trySendSlot 尝试非阻塞地占一个全局并发额度。
func (a *TransferApp) trySendSlot() bool {
	select {
	case a.sendSem <- struct{}{}:
		return true
	default:
		return false
	}
}

// acquireSendSlot 阻塞获取一个额度（只给用户直接发起的路径用）：用户按下的
// 按钮最终一定要跑起来，哪怕要等一会儿。ctx 结束即放弃。
func (a *TransferApp) acquireSendSlot(ctx context.Context) error {
	select {
	case a.sendSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseSendSlot 归还一个额度，并顺手请求一次补位。
//
// 必须【持有额度时】调用：它就是对 sendSem 的一次接收。
func (a *TransferApp) releaseSendSlot() {
	<-a.sendSem
	a.kickSweep()
}

// kickSweep 请求「尽快补位」（非阻塞、自动合并）。
func (a *TransferApp) kickSweep() {
	if a.kick == nil {
		return
	}
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// schedulerLoop 把「额度空出来了」这件事转成一次调度。
//
// 它只决定「什么时候」再扫一遍，不改变任何语义：没有到点的任务就不会有
// 额度释放，也就不会有信号 —— 不存在自旋。bootstrap 的 1 秒 sweep 仍是兜底。
func (a *TransferApp) schedulerLoop(ctx context.Context) {
	if a.kick == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.kick:
			a.SweepWaiting(ctx)
		}
	}
}

// CancelJob 取消一条任务：排队/等待中、或已暂停的直接落取消；正在传输的通过
// 取消信号让它干净退出并落取消。返回错误表示不可取消。
//
// 过去这里明确拒绝取消在传任务，理由是「sender 没有中断点，假取消比拒绝更糟」
// —— 那个理由现在依然成立，所以这里只在【确实能把任务停下来】的前提下放行：
// 有在跑的 sender 就发信号，没有就（且只能）对排队中的任务直接落状态。
//
// 暂停态也走「直接落状态」那条：暂停的发送侧没有在跑的 goroutine（它已经
// 干净退出了），不在这里放行的话，用户暂停之后就没法把这条任务清掉。
func (a *TransferApp) CancelJob(jobID string) error {
	j, err := a.store.GetJob(jobID)
	if err != nil {
		return fmt.Errorf("transfer: 任务不存在: %w", err)
	}
	if j.Direction != transfer.DirectionSend {
		return fmt.Errorf("transfer: 只能取消本机发出的任务")
	}
	if j.Status == transfer.StateDone || j.Status == transfer.StateCancelled {
		return fmt.Errorf("transfer: 任务已结束，无需取消")
	}

	// 先留标记：它同时覆盖「已经登记 sender」（下面顺手发信号）与
	// 「还在算哈希、没有 sender 可发」（发送路径进门时自己会看）。
	a.markCancelling(jobID)
	if a.cancelSender(jobID) {
		// 有在跑的 goroutine：由它写最终状态 —— 它手里有最新的 job 变量，
		// 这里再写一次会与它竞争，还可能把它的位图覆盖掉。
		return nil
	}
	if a.isStarting(jobID) {
		// 已被交给 goroutine、但还没登记 sender（正在算哈希）：标记已经留了，
		// 它下一个检查点就会退出。
		return nil
	}
	if j.Status != transfer.StateIdle && j.Status != transfer.StatePaused {
		a.clearCancelling(jobID)
		return fmt.Errorf("transfer: 任务既不在等待中、也没有在传输（当前 %s）", j.Status)
	}

	from := string(j.Status)
	j.Status = transfer.StateCancelled
	j.WaitReason = transfer.WaitNone
	j.NextTryAt = time.Time{}
	j.UpdatedAt = a.clk.Now()
	if err := a.store.UpsertJob(j); err != nil {
		return err
	}
	a.clearCancelling(jobID)
	// 暂停的任务被取消：两本账都要清，否则同一个 job_id 的下一次尝试
	// 会带着「还暂停着」的记忆进门（见 clearPause）。
	a.clearPause(jobID)
	a.publishState(j, from, string(transfer.StateCancelled), "")
	return nil
}

// CancelBatch 取消整个批次里所有未结束的任务，返回取消条数。
//
// 与逐条取消的区别只有两点：只动这一批（靠 ListByBatch 按 batch_id 过滤），
// 以及单条失败不中断整批 —— 用户点的是「全停」，尽力而为比半途而废好。
func (a *TransferApp) CancelBatch(batchID string) (int, error) {
	if batchID == "" {
		// 空 batch_id 会让 ListByBatch 退化成「返回所有单文件任务」，
		// 也就是把本机全部单文件传输一起停掉。
		return 0, fmt.Errorf("%w: app", ports.ErrEmptyBatchID)
	}
	jobs, err := a.store.ListByBatch(batchID)
	if err != nil {
		return 0, err
	}

	n := 0
	for _, j := range jobs {
		if j.Status == transfer.StateDone || j.Status == transfer.StateCancelled {
			continue // 已结束的不动（卡片上的按钮可能被连点）
		}
		if err := a.CancelJob(j.JobID); err != nil {
			if a.lg != nil {
				a.lg.Warn("transfer: 取消批次中的任务失败", "job_id", j.JobID, "err", err)
			}
			continue
		}
		n++
	}
	return n, nil
}

// RetryNow 立刻重试一条等待中的任务：不等退避，也不要求目录里显示在线
// （目录可能滞后于现实）。
func (a *TransferApp) RetryNow(ctx context.Context, jobID string) error {
	j, err := a.store.GetJob(jobID)
	if err != nil {
		return fmt.Errorf("transfer: 任务不存在: %w", err)
	}
	// 判据与 startJobs 一致：凡是「可自动拉起」的任务都允许手动提前启动
	// （等对端的，以及批次里排队等前一个传完的）。手写成员比较会在这里生出
	// 第四份「哪些原因能被启动」的真相，而分叉的方向是「界面上的按钮点了没反应」。
	if j.Status != transfer.StateIdle || !j.WaitReason.AutoStartable() {
		return fmt.Errorf("transfer: 该任务不在等待中")
	}
	j.NextTryAt = a.clk.Now()
	j.UpdatedAt = j.NextTryAt
	if err := a.store.UpsertJob(j); err != nil {
		return err
	}
	if !a.claimStarting(jobID) {
		return nil // 已经在传了，什么都不用做
	}
	go func() {
		defer a.releaseStarting(jobID)
		// force：真拨一次；slotHeld=false：用户点的按钮自己等一个并发额度，
		// 与直接发送同一条语义（不会因为额度满就永远不动）。
		_, _ = a.sendFile(ctx, j.PeerID, j.LocalPath, j.JobID, true, false)
	}()
	return nil
}
