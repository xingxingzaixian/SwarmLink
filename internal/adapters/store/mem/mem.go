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
	}
}

// Append 追加消息（不判重）。
func (s *Messages) Append(m message.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages[m.MsgID] = m
	return nil
}

// AppendIfAbsent 幂等写入：msg_id 已存在则 inserted=false。
func (s *Messages) AppendIfAbsent(m message.Message) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.messages[m.MsgID]; ok {
		return false, nil
	}
	s.messages[m.MsgID] = m
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
	s.messages[m.MsgID] = m
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

// FindResumable 按 (peer, file_hash) 查找可续传任务。
func (s *Messages) FindResumable(peerID identity.NodeID, fileHash string) (transfer.Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, j := range s.jobs {
		if j.PeerID == peerID && j.FileHash == fileHash &&
			j.Status != transfer.StateDone && j.Status != transfer.StateCancelled {
			j.Bitmap = j.Bitmap.Copy()
			return j, true
		}
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
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
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
	if old, ok := s.items[p.NodeID]; ok && !old.FirstSeen.IsZero() {
		p.FirstSeen = old.FirstSeen
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
		if filter.State != "" && p.State != filter.State {
			continue
		}
		if filter.Source != "" && p.Source != filter.Source {
			continue
		}
		if filter.OnlineOnly && p.State != peer.StateOnline {
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
	p.State = peer.StateOffline
	if !at.IsZero() {
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
	g.Members = append([]group.Member(nil), g.Members...)
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
