// Package mem 提供内存仓储实现。
//
// 用途：领域/应用层单测、test/e2e 多节点集成、test/scale 200 节点规模验证。
// 它与 sqlite 实现是【对等实现】，共同满足 domain/ports 的同一组接口
// —— 这正是「适配器可被内存实现替换」这条红线的价值所在。
//
// 为什么拆成三个类型：ports.PeerDirectory 与 ports.GroupRepo 都有
// Upsert/Get/List 但参数类型不同，Go 不支持方法重载，单个类型无法同时实现二者。
// 因此按仓储边界拆为 Messages / Peers / Groups，各自持有独立锁。
package mem

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/group"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/share"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
)

// ---------------------------------------------------------------------------
// Messages：实现 ports.ChatStore 与 ports.TransferRepo
// ---------------------------------------------------------------------------

// Messages 是消息 / outbox / 传输任务的内存仓储。
type Messages struct {
	mu       sync.RWMutex
	clk      ports.Clock
	messages map[string]message.Message
	outbox   map[string]ports.OutboxEntry
	jobs     map[string]transfer.Job
	// cleared 是会话「清空点」：早于它的消息不显示（消息不删除）。
	cleared map[string]time.Time
	// seq 是「写入序号」：Search / Window 需要一个唯一且单调的排序键
	// （见 ports.MessageSearchQuery.BeforeMsgID）。
	//
	// sqlite 侧这个角色由 rowid 扮演，内存里没有等价物 —— 不显式维护一份，
	// 内存实现就只能退化成按 sent_at 分页，而那正是「与锚点同毫秒的其它消息
	// 会被整条跳过」的写法。两个实现对等是本仓的测试策略，所以这里补上它。
	seq     map[string]int64
	nextSeq int64
}

var (
	_ ports.ChatStore    = (*Messages)(nil)
	_ ports.TransferRepo = (*Messages)(nil)
)

// NewMessages 创建消息仓储。
func NewMessages(clk ports.Clock) *Messages {
	return &Messages{
		clk:      clk,
		messages: make(map[string]message.Message),
		outbox:   make(map[string]ports.OutboxEntry),
		jobs:     make(map[string]transfer.Job),
		cleared:  make(map[string]time.Time),
		seq:      make(map[string]int64),
	}
}

// put 写入消息并分配写入序号（调用方必须已持有写锁）。
func (s *Messages) put(m message.Message) {
	s.nextSeq++
	s.messages[m.MsgID] = m
	s.seq[m.MsgID] = s.nextSeq
}

// visible 报告消息是否未被会话清空点隐藏（调用方必须已持有读锁）。
//
// 判据必须与 sqlite 实现（messages_search.go 的 visibleClause）以及前端
// stores/chat.ts 的 activeMessages 逐字一致：sent_at 严格大于 cleared_at
// 才可见。差一个等号就是「界面上没有、搜索能搜到」。
func (s *Messages) visible(m message.Message) bool {
	at, ok := s.cleared[m.ConvID]
	return !ok || m.SentAt.After(at)
}

// Append 追加消息（不判重）。
func (s *Messages) Append(m message.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.put(m)
	return nil
}

// AppendIfAbsent 幂等写入：msg_id 已存在则 inserted=false。
func (s *Messages) AppendIfAbsent(m message.Message) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.messages[m.MsgID]; ok {
		return false, nil
	}
	s.put(m)
	return true, nil
}

// Latest 按会话返回最近 limit 条（新→旧）；before 为毫秒时间戳游标。
func (s *Messages) Latest(convID string, limit int, before int64) ([]message.Message, error) {
	s.mu.RLock()
	out := make([]message.Message, 0)
	for _, m := range s.messages {
		if m.ConvID != convID {
			continue
		}
		if before > 0 && m.SentAt.UnixMilli() >= before {
			continue
		}
		out = append(out, m)
	}
	s.mu.RUnlock()

	message.SortInPlace(out)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Search 实现 ports.MessageRepo.Search（语义见端口注释）。
func (s *Messages) Search(q ports.MessageSearchQuery) ([]message.Message, error) {
	if q.Text == "" || len(q.ConvIDs) == 0 {
		return []message.Message{}, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = ports.DefaultSearchLimit
	}
	allow := make(map[string]struct{}, len(q.ConvIDs))
	for _, id := range q.ConvIDs {
		allow[id] = struct{}{}
	}

	s.mu.RLock()
	cursor := int64(0)
	if q.BeforeMsgID != "" {
		c, ok := s.seq[q.BeforeMsgID]
		if !ok {
			// 游标指向不存在的消息：返回空，绝不退化成首页（否则前端会以为
			// 「又翻出了一批结果」而把第一页重复贴一遍）。
			s.mu.RUnlock()
			return []message.Message{}, nil
		}
		cursor = c
	}
	hits := make([]seqMessage, 0, limit)
	for id, m := range s.messages {
		seq := s.seq[id]
		if cursor > 0 && seq >= cursor {
			continue
		}
		if _, ok := allow[m.ConvID]; !ok {
			continue
		}
		if !s.visible(m) {
			continue
		}
		if !likeContains(m.Content, q.Text) {
			continue
		}
		hits = append(hits, seqMessage{seq: seq, msg: m})
	}
	s.mu.RUnlock()

	// 写入序倒序 = sqlite 侧的 ORDER BY id DESC（见端口注释里的排序契约）。
	sort.Slice(hits, func(i, j int) bool { return hits[i].seq > hits[j].seq })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]message.Message, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.msg)
	}
	return out, nil
}

// Window 实现 ports.MessageRepo.Window（语义见端口注释）。
func (s *Messages) Window(convID, msgID string, before, after int) ([]message.Message, error) {
	if convID == "" || msgID == "" {
		return []message.Message{}, nil
	}
	if before < 0 {
		before = 0
	}
	if after < 0 {
		after = 0
	}

	s.mu.RLock()
	anchor, ok := s.messages[msgID]
	if !ok || anchor.ConvID != convID || !s.visible(anchor) {
		// 不存在 / 不属于这个会话 / 已被清空：都是「回退到最新」的信号，
		// 与 sqlite 实现返回同一种结果（空切片 + 无错误）。
		s.mu.RUnlock()
		return []message.Message{}, nil
	}
	anchorEntry := seqMessage{seq: s.seq[msgID], msg: anchor}

	// 比较键是 (sent_at, 写入序号)，与 sqlite 侧的 (sent_at, rowid) 逐字对应
	//（那边用的索引就是 (conv_id, sent_at DESC, id DESC)）。用行值比较是为了
	// 让锚点唯一：同一毫秒里的其它消息不能与锚点互踩，否则命中会被挤出窗口。
	older := make([]seqMessage, 0, before+1)
	newer := make([]seqMessage, 0, after)
	for id, m := range s.messages {
		if m.ConvID != convID || !s.visible(m) {
			continue
		}
		e := seqMessage{seq: s.seq[id], msg: m}
		if lessByDisplay(anchorEntry, e) {
			newer = append(newer, e) // 显示序上晚于锚点
			continue
		}
		older = append(older, e) // 含锚点本身
	}
	s.mu.RUnlock()

	// 旧侧取紧邻锚点之前的 before 条（含锚点本身，故 +1）；新侧取紧邻锚点
	// 之后的 after 条 —— 「紧邻」二字是窗口语义的全部，取成整段尾部就错了。
	sort.Slice(older, func(i, j int) bool { return lessByDisplay(older[j], older[i]) })
	if len(older) > before+1 {
		older = older[:before+1]
	}
	sort.Slice(newer, func(i, j int) bool { return lessByDisplay(newer[i], newer[j]) })
	if len(newer) > after {
		newer = newer[:after]
	}

	out := make([]message.Message, 0, len(older)+len(newer))
	for i := len(newer) - 1; i >= 0; i-- {
		out = append(out, newer[i].msg)
	}
	for _, e := range older {
		out = append(out, e.msg)
	}
	return out, nil
}

// seqMessage 把消息与它的写入序号绑在一起排序用。
type seqMessage struct {
	seq int64
	msg message.Message
}

// lessByDisplay 报告 a 是否排在 b 之前（显示序：(sent_at, 写入序号) 升序）。
//
// 这与 sqlite 侧 `ORDER BY sent_at, id` 是同一条规则；写入序号就是内存实现里的
// rowid 替身。两个实现必须逐字一致，否则对拍测试会（也应该）失败。
func lessByDisplay(a, b seqMessage) bool {
	if !a.msg.SentAt.Equal(b.msg.SentAt) {
		return a.msg.SentAt.Before(b.msg.SentAt)
	}
	return a.seq < b.seq
}

// likeContains 报告 haystack 是否包含 needle（子串），大小写规则与 SQLite 的
// LIKE 一致：只对 ASCII 字母忽略大小写，其余字节逐字精确。
//
// 为什么不用 strings.Contains(strings.ToLower(...))：ToLower 会折叠 Unicode
// （西里尔字母、'İ'、希腊字母…），而生产实现（SQLite LIKE）不会 —— 用 mem 跑
// 的单测与跑 sqlite 的线上就会在这些边界上给出不同的答案。
//
// 逐字节比较在这里是安全的：needle 是合法 UTF-8，其首字节不可能是续字节
// （0x80~0xBF），因此匹配只可能落在字符边界上。
func likeContains(haystack, needle string) bool {
	n, h := len(needle), len(haystack)
	if n == 0 {
		return true
	}
	if n > h {
		return false
	}
	for i := 0; i+n <= h; i++ {
		matched := true
		for j := 0; j < n; j++ {
			if !equalFoldASCII(haystack[i+j], needle[j]) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// equalFoldASCII 逐字节比较，仅 ASCII 字母忽略大小写。
func equalFoldASCII(a, b byte) bool {
	if a == b {
		return true
	}
	return foldASCII(a) == foldASCII(b)
}

func foldASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// MarkDelivered 置为已送达。
func (s *Messages) MarkDelivered(msgID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.messages[msgID]
	if !ok {
		return fmt.Errorf("mem: message %s not found", msgID)
	}
	m.State = message.StateDelivered
	s.messages[msgID] = m
	return nil
}

// Enqueue 入队一条待确认消息。
func (s *Messages) Enqueue(msgID, convID string, nextTry time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.outbox[msgID]
	if !ok {
		e = ports.OutboxEntry{MsgID: msgID, ConvID: convID}
	}
	e.NextTryAt = nextTry
	s.outbox[msgID] = e
	return nil
}

// Due 返回到期的 outbox 记录。
func (s *Messages) Due(now time.Time, limit int) ([]ports.OutboxEntry, error) {
	s.mu.RLock()
	out := make([]ports.OutboxEntry, 0)
	for _, e := range s.outbox {
		if !e.NextTryAt.After(now) {
			out = append(out, e)
		}
	}
	s.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].NextTryAt.Before(out[j].NextTryAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Delete 删除 outbox 记录。
func (s *Messages) Delete(msgID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.outbox, msgID)
	return nil
}

// BumpAttempt 增加重试次数并设置下次重试时间。
func (s *Messages) BumpAttempt(msgID string, nextTry time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.outbox[msgID]
	if !ok {
		return nil
	}
	e.Attempts++
	e.NextTryAt = nextTry
	s.outbox[msgID] = e
	return nil
}

// ListByConv 返回某会话的全部待确认消息。
func (s *Messages) ListByConv(convID string) ([]ports.OutboxEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ports.OutboxEntry, 0)
	for _, e := range s.outbox {
		if e.ConvID == convID {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NextTryAt.Before(out[j].NextTryAt) })
	return out, nil
}

// AppendOutgoing 原子写入出站消息（state=pending）与 outbox。
func (s *Messages) AppendOutgoing(m message.Message, nextTry time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.messages[m.MsgID]; ok {
		return nil
	}
	if m.State == "" {
		m.State = message.StatePending
	}
	s.put(m)
	s.outbox[m.MsgID] = ports.OutboxEntry{MsgID: m.MsgID, ConvID: m.ConvID, NextTryAt: nextTry}
	return nil
}

// Deliver 原子置为已送达并删除 outbox。
func (s *Messages) Deliver(msgID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.messages[msgID]; ok {
		m.State = message.StateDelivered
		s.messages[msgID] = m
	}
	delete(s.outbox, msgID)
	return nil
}

// SetConvCleared 写入/覆盖会话清空点（覆盖是必须的：清空后又聊几句再清空，
// 边界要往前推）。
func (s *Messages) SetConvCleared(convID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleared[convID] = at
	return nil
}

// ConvClearedAt 读取清空点；从未清空时返回 ok=false（这不是错误）。
func (s *Messages) ConvClearedAt(convID string) (time.Time, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	at, ok := s.cleared[convID]
	return at, ok, nil
}

// RemoveConvClear 撤销清空（幂等）。
func (s *Messages) RemoveConvClear(convID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cleared, convID)
	return nil
}

// OutboxLen 返回待确认消息数（测试/诊断用）。
func (s *Messages) OutboxLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.outbox)
}

// Count 返回消息总数（测试/诊断用）。
func (s *Messages) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.messages)
}

// Get 按 msg_id 读取消息。
func (s *Messages) Get(msgID string) (message.Message, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.messages[msgID]
	return m, ok
}

// UpsertJob 写入/更新传输任务（位图深拷贝）。
func (s *Messages) UpsertJob(j transfer.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j.Bitmap = j.Bitmap.Copy()
	s.jobs[j.JobID] = j
	return nil
}

// GetJob 读取传输任务。
func (s *Messages) GetJob(jobID string) (transfer.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return transfer.Job{}, fmt.Errorf("mem: job %s not found", jobID)
	}
	j.Bitmap = j.Bitmap.Copy()
	return j, nil
}

// FindResumable 按 (peer, file_hash[, rel_path]) 查找可续传任务（语义见端口）。
func (s *Messages) FindResumable(peerID identity.NodeID, fileHash, relPath string) (transfer.Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, j := range s.jobs {
		if j.PeerID != peerID || j.FileHash != fileHash ||
			j.Status == transfer.StateDone || j.Status == transfer.StateCancelled {
			continue
		}
		// 批次内按相对路径区分（与 sqlite 实现同一条规则）：
		// 同 hash 的两个文件混在一起会互相继承临时文件与最终路径。
		if relPath != "" && j.RelPath != relPath {
			continue
		}
		j.Bitmap = j.Bitmap.Copy()
		return j, true
	}
	return transfer.Job{}, false
}

// SaveBitmap 保存位图与完成计数。
func (s *Messages) SaveBitmap(jobID string, bm transfer.ChunkBitmap, completed int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return fmt.Errorf("mem: job %s not found", jobID)
	}
	j.Bitmap = bm.Copy()
	j.Completed = completed
	j.UpdatedAt = s.clk.Now()
	s.jobs[jobID] = j
	return nil
}

// SetJobStatus 只改状态，不动位图与进度（与 sqlite 实现语义一致）。
func (s *Messages) SetJobStatus(jobID string, st transfer.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return fmt.Errorf("mem: job %s not found", jobID)
	}
	j.Status = st
	j.UpdatedAt = s.clk.Now()
	s.jobs[jobID] = j
	return nil
}

// ListActive 返回所有未结束的任务。
func (s *Messages) ListActive() ([]transfer.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]transfer.Job, 0)
	for _, j := range s.jobs {
		if j.Status == transfer.StateDone || j.Status == transfer.StateCancelled {
			continue
		}
		j.Bitmap = j.Bitmap.Copy()
		out = append(out, j)
	}
	sortJobsByCreated(out)
	return out, nil
}

// ListWaiting 返回可以被自动拉起的发送任务（语义见 ports.TransferRepo.ListWaiting）。
//
// 取值集合与 SQL 实现【必须一致】：单进程测试跑的是这里，真实运行跑的是
// sqlite —— 两处不一致会让「取消之后任务又自己跑起来」只在其中一边出现。
func (s *Messages) ListWaiting() ([]transfer.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]transfer.Job, 0)
	for _, j := range s.jobs {
		if j.Direction != transfer.DirectionSend ||
			j.Status != transfer.StateIdle ||
			!j.WaitReason.AutoStartable() {
			continue
		}
		j.Bitmap = j.Bitmap.Copy()
		out = append(out, j)
	}
	sortJobsByCreated(out)
	return out, nil
}

// ListByBatch 返回同一批次的全部任务（语义见 ports.TransferRepo.ListByBatch）。
//
// 与 ListActive 的区别是决定性的：这里【包含】已结束的任务 ——
// 文件夹卡片上要显示的是完整的 N 条，少一条数字就对不上。
//
// 顺序为 rel_path 升序（目录树顺序），与 sqlite 侧的 ORDER BY rel_path, job_id 一致。
func (s *Messages) ListByBatch(batchID string) ([]transfer.Job, error) {
	if batchID == "" {
		// 与 sqlite 实现同一条守卫：空 batch_id 会退化成「返回所有单文件任务」，
		// 对 CancelBatch 而言就是把本机全部单文件传输一起停掉。
		return nil, fmt.Errorf("%w: mem", ports.ErrEmptyBatchID)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]transfer.Job, 0)
	for _, j := range s.jobs {
		if j.BatchID != batchID {
			continue
		}
		j.Bitmap = j.Bitmap.Copy()
		out = append(out, j)
	}
	sort.Slice(out, func(i, k int) bool { return lessBatchOrder(out[i], out[k]) })
	return out, nil
}

// ListRecent 返回最近更新的若干任务，包含已结束的（界面的传输记录用）。
//
// 语义见 ports.TransferRepo.ListRecent：与 ListActive 相反，这里【不】排除
// 已完成的任务 —— 否则「刚传完的文件从列表里消失」。
func (s *Messages) ListRecent(limit int) ([]transfer.Job, error) {
	if limit <= 0 {
		limit = 50
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]transfer.Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		j.Bitmap = j.Bitmap.Copy()
		out = append(out, j)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PurgeFinished 清理已结束的任务，返回删除条数（语义见 ports.TransferRepo）。
func (s *Messages) PurgeFinished(keep int, olderThan time.Time) (int, error) {
	if keep < 0 {
		keep = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	terminal := make([]transfer.Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		if isTerminal(j.Status) {
			terminal = append(terminal, j)
		}
	}
	sort.Slice(terminal, func(i, j int) bool { return terminal[i].UpdatedAt.After(terminal[j].UpdatedAt) })

	retain := make(map[string]bool, keep)
	for i := 0; i < keep && i < len(terminal); i++ {
		retain[terminal[i].JobID] = true
	}

	n := 0
	for id, j := range s.jobs {
		if !isTerminal(j.Status) || retain[id] || !j.UpdatedAt.Before(olderThan) {
			continue
		}
		delete(s.jobs, id)
		n++
	}
	return n, nil
}

// isTerminal 判断是否为不会再变动的终态。
func isTerminal(st transfer.State) bool {
	return st == transfer.StateDone || st == transfer.StateFailed || st == transfer.StateCancelled
}

// lessBatchOrder 是批次的顺序：rel_path 升序（目录树顺序），同路径再按 job_id。
//
// 与 sqlite 侧 ORDER BY rel_path, job_id 必须一致。刻意不按创建时间：
// 批次是同一个循环里建的，整批落在同一毫秒是常态，而 job_id 是 UUIDv7
// （同毫秒内随机），按它们排出来的顺序确定但没有语义。
func lessBatchOrder(a, b transfer.Job) bool {
	if a.RelPath != b.RelPath {
		return a.RelPath < b.RelPath
	}
	return a.JobID < b.JobID
}

// sortJobsByCreated 是队列顺序：先按创建时间，同刻再走批次顺序（目录树）。
//
// 必须有第二、第三排序键：只比 CreatedAt 时顺序由 map 迭代顺序决定 ——
// 用户看到的次序会一次一个样，而且与 sqlite 实现给出不同结果
// （sqlite 侧是 ORDER BY created_at, rel_path, job_id）。
func sortJobsByCreated(out []transfer.Job) {
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return lessBatchOrder(a, b)
	})
}

// ---------------------------------------------------------------------------
// Peers：实现 ports.PeerDirectory
// ---------------------------------------------------------------------------

// Peers 是节点目录的内存实现。
type Peers struct {
	mu    sync.RWMutex
	clk   ports.Clock
	items map[identity.NodeID]peer.Peer
}

var _ ports.PeerDirectory = (*Peers)(nil)

// NewPeers 创建节点目录。
func NewPeers(clk ports.Clock) *Peers {
	return &Peers{clk: clk, items: make(map[identity.NodeID]peer.Peer)}
}

// Upsert 写入/更新节点条目（LastSeen 为空时记为当前时间）。
func (s *Peers) Upsert(p peer.Peer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.items[p.NodeID]; ok {
		// blocked 是粘性状态：任何写入都不得覆盖它，否则每新增一条写入路径都是一次
		// 「被移除的节点复活」的风险。解除只能走 SetSuppressed（见 ports.PeerDirectory）。
		if old.State == peer.StateBlocked {
			p.State = peer.StateBlocked
		}
		if !old.FirstSeen.IsZero() {
			p.FirstSeen = old.FirstSeen
		}
	}
	if p.FirstSeen.IsZero() {
		p.FirstSeen = s.clk.Now()
	}
	if p.LastSeen.IsZero() {
		p.LastSeen = s.clk.Now()
	}
	s.items[p.NodeID] = p
	return nil
}

// Get 读取节点条目。
func (s *Peers) Get(id identity.NodeID) (peer.Peer, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.items[id]
	return p, ok
}

// List 按 LastSeen 倒序返回条目。
func (s *Peers) List(filter ports.PeerFilter) []peer.Peer {
	s.mu.RLock()
	out := make([]peer.Peer, 0, len(s.items))
	for _, p := range s.items {
		if filter.State != "" {
			if p.State != filter.State {
				continue
			}
		} else if !filter.IncludeBlocked && p.State == peer.StateBlocked {
			// 默认不返回「被本地移除」的条目（见 ports.PeerFilter.IncludeBlocked）。
			// 显式按 State 查询优先：诊断面板要能看到那几条。
			continue
		}
		if filter.Source != "" && p.Source != filter.Source {
			continue
		}
		// OnlineOnly：online 与 discovered 都算「在」，与 sqlite 实现保持一致
		// （这里曾只认 online，两个互为替身的实现因此在同一条语句上分叉）
		if filter.OnlineOnly && p.State != peer.StateOnline && p.State != peer.StateDiscovered {
			continue
		}
		out = append(out, p)
	}
	s.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out
}

// MarkOffline 标记节点离线（保留条目以便 UI 区分「从未见过」与「离线」）。
func (s *Peers) MarkOffline(id identity.NodeID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.items[id]
	if !ok {
		return nil
	}
	if p.State == peer.StateBlocked {
		// 已移除的条目不能被离线事件变回可见：List 只排除 blocked，而 offline 是可见的。
		return nil
	}
	p.State = peer.StateOffline
	if !at.IsZero() {
		p.LastSeen = at
	}
	s.items[id] = p
	return nil
}

// ResetLiveness 清零上次运行残留的在线状态（语义见 ports.PeerDirectory）。
func (s *Peers) ResetLiveness() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, p := range s.items {
		if p.State != peer.StateOnline {
			continue
		}
		// last_seen 保持原值：TTL 兜底据此判断该条目是否已过期。
		// 这里绝不能写 now，否则「失联很久」会被伪装成「刚刚见过」。
		p.State = peer.StateDiscovered
		s.items[id] = p
		n++
	}
	return n, nil
}

// SetSuppressed 设置/清除「本地已移除」标记（语义见 ports.PeerDirectory）。
func (s *Peers) SetSuppressed(id identity.NodeID, suppressed bool, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.items[id]
	if !ok {
		return nil
	}
	if suppressed {
		p.State = peer.StateBlocked
		p.LastSeen = at
	} else if p.State == peer.StateBlocked {
		// 解除路径：只有真正发生迁移时才写 last_seen。非 blocked 行必须是彻底空操作
		// （连 last_seen 也不动），与 sqlite 实现一致 —— 那里的解除语句带
		// `AND state = 'blocked'`，匹配 0 行时 last_seen 保持原值。
		// 两边在这点上分叉，List 的 ORDER BY last_seen DESC 与 SeenRecently 会跟着分叉。
		p.State = peer.StateDiscovered
		p.LastSeen = at
	}
	s.items[id] = p
	return nil
}

// SeenRecently 判断该节点是否在 window 内被再次见到。
func (s *Peers) SeenRecently(id identity.NodeID, window time.Duration) bool {
	s.mu.RLock()
	p, ok := s.items[id]
	now := s.clk.Now()
	s.mu.RUnlock()
	if !ok {
		return false
	}
	return now.Sub(p.LastSeen) < window
}

// Len 返回条目数。
func (s *Peers) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.items)
}

// ---------------------------------------------------------------------------
// GroupDeliveries：实现 ports.GroupDeliveryRepo
// ---------------------------------------------------------------------------

// GroupDeliveries 是群消息「按成员」投递跟踪的内存实现。
//
// 与 sqlite 实现是【对等实现】：两者必须给出同样的语义，否则
// 「单进程测试跑 mem、生产跑 sqlite」会在离线补投上分叉。
type GroupDeliveries struct {
	mu sync.RWMutex
	// acked[msgID][memberID] = 送达时间；成员已在 map 中但时间为零值 = 未送达。
	acked map[string]map[identity.NodeID]time.Time
	group map[string]string // msgID -> groupID
}

var _ ports.GroupDeliveryRepo = (*GroupDeliveries)(nil)

// NewGroupDeliveries 创建投递跟踪仓储。
func NewGroupDeliveries() *GroupDeliveries {
	return &GroupDeliveries{
		acked: make(map[string]map[identity.NodeID]time.Time),
		group: make(map[string]string),
	}
}

// EnsureDeliveries 登记全体目标成员（幂等；已存在的成员保持原状）。
func (s *GroupDeliveries) EnsureDeliveries(msgID, groupID string, members []identity.NodeID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.acked[msgID]
	if !ok {
		set = make(map[identity.NodeID]time.Time)
		s.acked[msgID] = set
		s.group[msgID] = groupID
	}
	for _, m := range members {
		if m.IsZero() {
			continue
		}
		if _, exists := set[m]; !exists {
			set[m] = time.Time{} // 未送达
		}
	}
	return nil
}

// AckDelivery 标记某成员已送达（幂等）。
func (s *GroupDeliveries) AckDelivery(msgID string, memberID identity.NodeID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.acked[msgID]
	if !ok {
		return nil
	}
	if at, exists := set[memberID]; exists && at.IsZero() {
		set[memberID] = time.Now()
	}
	return nil
}

// PendingMembers 返回尚未送达的成员。
func (s *GroupDeliveries) PendingMembers(msgID string) ([]identity.NodeID, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	set := s.acked[msgID]
	out := make([]identity.NodeID, 0, len(set))
	for id, at := range set {
		if at.IsZero() {
			out = append(out, id)
		}
	}
	return out, nil
}

// StatsByConv 返回会话内每条群消息的送达统计。
func (s *GroupDeliveries) StatsByConv(convID string) (map[string]ports.DeliveryStat, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]ports.DeliveryStat)
	for msgID, set := range s.acked {
		if s.group[msgID] != convID {
			continue
		}
		stat := ports.DeliveryStat{Total: len(set)}
		for _, at := range set {
			if !at.IsZero() {
				stat.Acked++
			}
		}
		out[msgID] = stat
	}
	return out, nil
}

// DeleteByMsg 清掉一条消息的全部投递记录。
func (s *GroupDeliveries) DeleteByMsg(msgID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.acked, msgID)
	delete(s.group, msgID)
	return nil
}

// DeleteByGroup 清掉某群的全部投递记录。
func (s *GroupDeliveries) DeleteByGroup(groupID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for msgID, gid := range s.group {
		if gid == groupID {
			delete(s.acked, msgID)
			delete(s.group, msgID)
		}
	}
	return nil
}

// DeleteGroupMember 清掉某群某成员的全部投递记录。
func (s *GroupDeliveries) DeleteGroupMember(groupID string, memberID identity.NodeID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for msgID, gid := range s.group {
		if gid != groupID {
			continue
		}
		if set, ok := s.acked[msgID]; ok {
			delete(set, memberID)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Groups：实现 ports.GroupRepo
// ---------------------------------------------------------------------------

// Groups 是群组的内存实现。
type Groups struct {
	mu    sync.RWMutex
	items map[string]group.Group
}

var _ ports.GroupRepo = (*Groups)(nil)

// NewGroups 创建群仓储。
func NewGroups() *Groups { return &Groups{items: make(map[string]group.Group)} }

// Upsert 写入/更新群。
func (s *Groups) Upsert(g group.Group) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 与 sqlite 实现对齐（那边由列的 DEFAULT 'active' 承担零值归一）：
	//   - 群级状态零值归一为 active；
	//   - 成员的 State 缓存按票号刷新。
	// 少做后者会让「mem 跑测试、sqlite 跑生产」在成员状态上分叉。
	g.State = g.State.StateOrActive()
	g.Members = append([]group.Member(nil), g.Members...)
	g.SyncDerivedState()
	s.items[g.ID] = g
	return nil
}

// Get 读取群。
func (s *Groups) Get(groupID string) (group.Group, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.items[groupID]
	if ok {
		g.Members = append([]group.Member(nil), g.Members...)
	}
	return g, ok
}

// List 返回全部群。
func (s *Groups) List() []group.Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]group.Group, 0, len(s.items))
	for _, g := range s.items {
		g.Members = append([]group.Member(nil), g.Members...)
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// ---------------------------------------------------------------------------
// Shares：实现 ports.ShareRepo
// ---------------------------------------------------------------------------

// Shares 是文件共享的内存实现（与 sqlite 实现互为替身）。
type Shares struct {
	mu    sync.RWMutex
	items map[string]share.Share
}

var _ ports.ShareRepo = (*Shares)(nil)

// NewShares 创建共享仓储。
func NewShares() *Shares { return &Shares{items: make(map[string]share.Share)} }

// Upsert 整份写入一条共享（清单整份替换，语义同 sqlite 实现）。
func (s *Shares) Upsert(in share.Share) error {
	// 深拷贝清单：调用方持有原切片，共享后再改它会让仓储里的内容跟着变
	// （sqlite 侧是序列化落盘，天然没有这个问题 —— 两侧必须一致）。
	cp := in
	cp.Visibility = in.Visibility.Normalize()
	cp.Items = append([]share.Item(nil), in.Items...)

	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.items[in.ID]; ok && !old.CreatedAt.IsZero() {
		cp.CreatedAt = old.CreatedAt
	}
	s.items[in.ID] = cp
	return nil
}

// Get 读取一条共享。
func (s *Shares) Get(shareID string) (share.Share, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.items[shareID]
	if !ok {
		return share.Share{}, false
	}
	v.Items = append([]share.Item(nil), v.Items...)
	return v, true
}

// List 按创建时间升序返回全部共享。
func (s *Shares) List() []share.Share {
	s.mu.RLock()
	out := make([]share.Share, 0, len(s.items))
	for _, v := range s.items {
		v.Items = append([]share.Item(nil), v.Items...)
		out = append(out, v)
	}
	s.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		// 第二排序键必须有：CreatedAt 相同（同一毫秒建的）时顺序会由 map
		// 迭代顺序决定，界面每次刷新都换一个次序，且与 sqlite 实现不同。
		return out[i].ID < out[j].ID
	})
	return out
}

// Delete 删除一条共享（幂等）。
func (s *Shares) Delete(shareID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, shareID)
	return nil
}
