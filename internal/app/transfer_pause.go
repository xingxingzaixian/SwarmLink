package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
)

// errJobPaused 表示「停下来，但全部进度保留」。
//
// 与 errJobCancelled 的分工：取消是终态（cancelled，再也不自动开始），
// 暂停是中间态（paused，等用户点恢复）。sendFile 的统一出口靠这个哨兵值
// 决定落哪种状态、发不发事件。
var errJobPaused = errors.New("transfer: 任务已暂停")

// signalTimeout 是控制信令拨号的最长等待。
//
// 为什么要有上限：暂停/恢复是用户点击驱动的同步 RPC，而拨号可能要等
// TCP 超时。正常路径上会话一定还在（两边几毫秒前刚交换过帧），拨号只会
// 发生在「会话刚好断了」的边角上 —— 为它让按钮假死几秒不值得。
const signalTimeout = 3 * time.Second

// ---------------------------------------------------------------------------
// 暂停意图的两本账
// ---------------------------------------------------------------------------

func (a *TransferApp) markPauseUser(jobID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pausedByUser == nil {
		a.pausedByUser = make(map[string]bool)
	}
	a.pausedByUser[jobID] = true
}

func (a *TransferApp) markPausePeer(jobID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pausedByPeer == nil {
		a.pausedByPeer = make(map[string]bool)
	}
	a.pausedByPeer[jobID] = true
}

func (a *TransferApp) isPausedByUser(jobID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pausedByUser[jobID]
}

func (a *TransferApp) isPausedByPeer(jobID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pausedByPeer[jobID]
}

// clearPauseUser 只清本机用户的意图（对端恢复信令用它）。
func (a *TransferApp) clearPauseUser(jobID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.pausedByUser, jobID)
}

// clearPausePeer 只清对端的意图（新到的 META 或本机恢复信令用它）。
func (a *TransferApp) clearPausePeer(jobID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.pausedByPeer, jobID)
}

// clearPause 两本账一起清：任务走到终态（done/failed/cancelled）时调用。
//
// 不清的后果不是内存泄漏，而是「同一个 job_id 的下一次尝试一进门就被判为
// 暂停」—— 与 clearCancelling 同一个道理。
func (a *TransferApp) clearPause(jobID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.pausedByUser, jobID)
	delete(a.pausedByPeer, jobID)
}

// ---------------------------------------------------------------------------
// 用户操作：暂停 / 恢复
// ---------------------------------------------------------------------------

// isJobPaused 报告某条任务此刻是否被按下（本机用户或对端任一方）。
//
// 这是数据路径唯一该看的暂停判据，读的是 app 层那两本账（自带互斥），
// 而不是 sendJob / recvJob 里的字段。原因是一个踩过的坑：
// recvJob（含它的 job 快照与位图）由【会话读协程】独占，而暂停是用户操作
// 驱动的另一个 goroutine —— 跨过去读就是数据竞争（-race 直接报），
// 写更是会把读协程刚推进的位图覆盖成零值（表现为「一按暂停进度条归零」）。
//
// 于是分工固定下来：两本账由暂停路径改写，recvJob 由读协程改写，
// 两边只在「要不要继续发/收块」这一个问题上相遇。
func (a *TransferApp) isJobPaused(jobID string) bool {
	return a.isPausedByUser(jobID) || a.isPausedByPeer(jobID)
}

// setRecvStatus 把接收任务的状态改为 to，并广播状态事件；返回是否真的改了。
//
// 走 SetJobStatus 而不是 UpsertJob：后者整行覆盖，会把另一条写入路径
// （接收侧 SaveBitmap）刚推进的位图与已完成数打回零值。
//
// 只在中间态之间切换：对端可能在信令到达【之前】就校验完并发出了 DONE，
// 把这个刚写完的 done 拉回「传输中」是伪造状态 —— 用户会看到一条
// 永远传不完的任务。
func (a *TransferApp) setRecvStatus(jobID string, to transfer.State) bool {
	j, err := a.store.GetJob(jobID)
	if err != nil {
		return false
	}
	switch j.Status {
	case transfer.StateMetaExchange, transfer.StateTransferring, transfer.StateVerifying, transfer.StatePaused:
	default:
		return false // 终态一概不动
	}
	if j.Status == to {
		return false // 已经是目标状态：不写库、不发事件（幂等）
	}
	if err := a.store.SetJobStatus(jobID, to); err != nil {
		return false
	}
	a.publishState(transfer.Job{JobID: jobID}, string(j.Status), string(to), "")
	return true
}

// PauseJob 暂停一条正在传输的任务。
//
// 两侧的行为：
//   - 发送中：给 sender 发暂停信号，由它自己落 status=paused（位图在它手里，
//     与 CancelJob 同一条理由）；同时给对端发 FILE_PAUSE，让接收方也停下来。
//   - 接收中：只改状态列（读协程只认那两本账，见 isJobPaused）。
//
// 等待态（等对方上线 / 批次排队）【不接受】暂停：那两条路已有取消与立即
// 重试，暂停一条还没开始的任务只会造出第三种说不清的中间态，也压不住
// 自动拉起（见待开发清单 D-07 的边界说明）。
func (a *TransferApp) PauseJob(jobID string) error {
	j, err := a.store.GetJob(jobID)
	if err != nil {
		return fmt.Errorf("transfer: 任务不存在: %w", err)
	}
	if j.Status != transfer.StateMetaExchange && j.Status != transfer.StateTransferring {
		return fmt.Errorf("transfer: 只能暂停正在传输的任务（当前 %s）", j.Status)
	}
	a.markPauseUser(jobID)

	from := string(j.Status)
	switch {
	case j.Direction == transfer.DirectionSend && a.getSender(jobID) != nil:
		// 在跑的 goroutine 自己写终态与状态事件；这里只负责发信号。
		// 抢在它前面写库会把它的位图覆盖掉（大文件的位图是分钟级进度）。
		a.getSender(jobID).requestPause()
	case j.Direction == transfer.DirectionRecv:
		a.setRecvStatus(jobID, transfer.StatePaused)
	default:
		// 发送侧但此刻没有在跑的 goroutine（算哈希那段窗口）：自己落库。
		// 发送行没有第二条并发写入路径，整行写是安全的；随后 goroutine
		// 走到「哈希结束、META 之前」会看到暂停意图并停下（见 sendFile）。
		j.Status = transfer.StatePaused
		j.WaitReason = transfer.WaitNone
		j.Error = ""
		j.UpdatedAt = a.clk.Now()
		_ = a.store.UpsertJob(j)
		a.publishState(j, from, string(transfer.StatePaused), "")
	}

	a.notifyPeer(context.Background(), j.PeerID, protocol.TypeFilePause, jobID)
	return nil
}

// ResumeJob 恢复一条已暂停的任务。
//
// 对端还按着（pausedByPeer）时拒绝：那正是「对方暂停了这场传输」，
// 本机单方面恢复只会再被按一次，还给用户一个「点了没用」的按钮。
// 对端一点恢复，FILE_RESUME 会经 HandleFileResume 自动继续。
func (a *TransferApp) ResumeJob(ctx context.Context, jobID string) error {
	j, err := a.store.GetJob(jobID)
	if err != nil {
		return fmt.Errorf("transfer: 任务不存在: %w", err)
	}
	if j.Status != transfer.StatePaused {
		return fmt.Errorf("transfer: 任务未暂停（当前 %s）", j.Status)
	}
	if a.isPausedByPeer(jobID) {
		return fmt.Errorf("transfer: 对方已暂停，对方恢复后会自动继续")
	}
	a.clearPauseUser(jobID)

	if j.Direction == transfer.DirectionSend {
		// 状态事件先发：前端靠它把「已暂停」翻回「协商中」，否则随后到达的
		// 进度事件会被「暂停中不接受进度」的护栏丢掉，界面卡死在已暂停。
		// 库里的状态仍留 paused，由 sendFile 进门后改写（它的入口校验认 paused）。
		a.publishState(j, string(transfer.StatePaused), string(transfer.StateMetaExchange), "")
		// 先解信令再重传：对端可能因我们之前的 FILE_PAUSE 而停着，
		// 两帧走同一条 TCP 流、保序 —— 它先恢复，再收 META。
		a.notifyPeer(ctx, j.PeerID, protocol.TypeFileResume, jobID)
		go func() {
			// force：用户点了恢复就是要真的试一次，哪怕目录说对方离线
			// （与「立即重试」同一口径，见 sendFile 的 force 参数）。
			_, _ = a.sendFile(ctx, j.PeerID, j.LocalPath, jobID, true)
		}()
		return nil
	}

	// 接收侧：位图在自己手里，直接回传输中；不需要重走 META
	// （domain/transfer 的状态机为这条路单独放行，见 state.go）。
	//
	// 内存里没有接收任务（重启后的陈行）也走同一条路：只把库里的状态改过来，
	// 真正的任务由发送方随后的 META 重建（HandleFileMeta 认它）。
	a.setRecvStatus(jobID, transfer.StateTransferring)
	a.notifyPeer(ctx, j.PeerID, protocol.TypeFileResume, jobID)
	return nil
}

// ---------------------------------------------------------------------------
// 对端信令
// ---------------------------------------------------------------------------

// HandleFilePause 处理对端的 FILE_PAUSE。
//
// 方向从【库里的任务行】读，而不是从内存表猜：job_id 在两个表里互斥，
// 但内存表可能因为重启、或读协程还没登记而缺席，而任务行一定在。
func (a *TransferApp) HandleFilePause(_ ports.Session, f protocol.Frame) error {
	var p protocol.FilePause
	if err := protocol.DecodeJSON(f.Payload, &p); err != nil {
		return err
	}
	if p.JobID == "" {
		return nil
	}
	j, err := a.store.GetJob(p.JobID)
	if err != nil {
		return nil // 不认识的任务：静默丢弃（对端可能已把它清掉了）
	}

	a.markPausePeer(p.JobID)
	if j.Direction == transfer.DirectionRecv {
		// 对端是这条入站传输的发送方：我们停嘴（块与 DONE 的丢弃由读协程
		// 按两本账判断，这里只改状态）。
		if a.isPausedByUser(p.JobID) {
			return nil // 用户自己按的暂停优先，状态与文案都不动
		}
		a.setRecvStatus(p.JobID, transfer.StatePaused)
		return nil
	}

	// 发送侧：有在跑的 goroutine 就发信号，由它的统一出口落 paused；
	// 没有（正在算哈希，或这是重启后的陈帧）就只留意图 —— 哈希结束、
	// META 之前的那道检查会兑现它。
	if sj := a.getSender(p.JobID); sj != nil {
		sj.requestPause()
	}
	return nil
}

// HandleFileResume 处理对端的 FILE_RESUME。
func (a *TransferApp) HandleFileResume(_ ports.Session, f protocol.Frame) error {
	var r protocol.FileResume
	if err := protocol.DecodeJSON(f.Payload, &r); err != nil {
		return err
	}
	if r.JobID == "" {
		return nil
	}

	j, err := a.store.GetJob(r.JobID)
	if err != nil {
		return nil // 不认识的任务：静默丢弃
	}

	a.clearPausePeer(r.JobID)
	if j.Direction == transfer.DirectionRecv {
		if a.isPausedByUser(r.JobID) {
			return nil // 本机用户还按着：只有他自己能解
		}
		// 只在确实停着时恢复（setRecvStatus 自己带这个判断）。
		a.setRecvStatus(r.JobID, transfer.StateTransferring)
		return nil
	}

	if a.getSender(r.JobID) != nil {
		// 在跑的 goroutine 没停（停的时候会退出并摘掉 sender），只清标志。
		return nil
	}
	// 没有在跑的发送任务：大概率是「被对端暂停、尝试已退出」的那条。
	if j.Status != transfer.StatePaused {
		return nil
	}
	if a.isPausedByUser(r.JobID) {
		return nil // 本机用户还按着：等他自己点恢复
	}
	if !a.claimStarting(r.JobID) {
		return nil // 已经有一条在跑了
	}
	a.publishState(j, string(transfer.StatePaused), string(transfer.StateMetaExchange), "")
	go func() {
		defer a.releaseStarting(r.JobID)
		// 重走 META（架构书 3.6）：发送侧内存里的位图只是「对端 ACK 过的
		// 部分」，暂停期间对端可能已落盘更多块，必须重新协商续传起点。
		_, _ = a.sendFile(context.Background(), j.PeerID, j.LocalPath, r.JobID, true)
	}()
	return nil
}

// ---------------------------------------------------------------------------
// 信令发送
// ---------------------------------------------------------------------------

// notifyPeer 给对端发一条控制信令（best-effort）。
//
// 为什么尽力而为就好：暂停/恢复首先必须让【本机】停下（那才是用户点按钮
// 直接看到的效果），信令只是让对端的界面与带宽同步。发不出去的场景
// （会话刚好断了）由后续的 META 重传自然收敛，不该让按钮假死。
func (a *TransferApp) notifyPeer(ctx context.Context, peerID identity.NodeID, typ byte, jobID string) {
	if a.conns == nil {
		return
	}
	if _, ok := a.conns.SessionOf(peerID); !ok && typ != protocol.TypeFileResume {
		// 暂停时【不】新建连接：为了告诉对方「我要停了」而先拨号建链，
		// 代价与收益完全不成比例。恢复则允许兜底拨号 —— 它要把对端叫醒。
		return
	}
	dialCtx, cancel := context.WithTimeout(ctx, signalTimeout)
	defer cancel()
	sess, err := a.session(dialCtx, peerID)
	if err != nil {
		a.logSignalFailure(typ, jobID, err)
		return
	}
	var payload []byte
	if typ == protocol.TypeFilePause {
		payload, err = protocol.EncodeJSON(protocol.FilePause{JobID: jobID})
	} else {
		payload, err = protocol.EncodeJSON(protocol.FileResume{JobID: jobID})
	}
	if err != nil {
		return
	}
	if err := sess.Send(protocol.New(typ, payload)); err != nil {
		a.logSignalFailure(typ, jobID, err)
	}
}

// sendPauseSignal 在【已持有会话】的回信路径上发 FILE_PAUSE。
//
// 专供 HandleFileMeta 的「保留用户暂停」分支：那里刚回过 ACK，会话一定在
// 手上，不需要（也不该）再走一次查表 + 拨号。
func (a *TransferApp) sendPauseSignal(sess ports.Session, jobID string) error {
	payload, err := protocol.EncodeJSON(protocol.FilePause{JobID: jobID})
	if err != nil {
		return err
	}
	return sess.Send(protocol.New(protocol.TypeFilePause, payload))
}

// logSignalFailure 记录一次信令发送失败。
//
// 只到 Debug 级：这是尽力而为的旁路，正常场景（对端在传）不会失败，
// 真失败时用户感知到的现象是「对端没跟着暂停」，排障时看日志即可。
func (a *TransferApp) logSignalFailure(typ byte, jobID string, err error) {
	if a.lg == nil {
		return
	}
	a.lg.Debug("transfer: 控制信令发送失败",
		"type", protocol.TypeName(typ), "job_id", jobID, "err", err)
}
