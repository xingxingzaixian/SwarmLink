package tcp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

// Policy 是连接策略（ADR-009：在线状态与传输连接解耦）。
type Policy struct {
	// MaxDialConcurrency 是群聊扇出等场景的并发拨号上限，避免瞬间 SYN 密集。
	MaxDialConcurrency int
	// MaxActiveConns 是单节点连接硬上限（常态目标 ≤ 10）。
	MaxActiveConns int
	// IdleTimeout 是无业务帧后的空闲回收时间。
	IdleTimeout time.Duration
	// HeartbeatInterval 是保活间隔。
	HeartbeatInterval time.Duration
	// HeartbeatTimeout 是超过该时间未收到任何帧即判连接失效。
	HeartbeatTimeout time.Duration
}

// DefaultPolicy 返回配置默认值（与 7.4 节 TOML 对齐）。
func DefaultPolicy() Policy {
	return Policy{
		MaxDialConcurrency: 8,
		MaxActiveConns:     32,
		IdleTimeout:        5 * time.Minute,
		HeartbeatInterval:  30 * time.Second,
		HeartbeatTimeout:   90 * time.Second,
	}
}

// FrameHandler 处理业务帧。
//
// 由组合根注入，使本适配器不依赖消息/群组/传输的业务语义
// （架构书把「tcp 适配器发事件」的职责交由 app 层的路由器承担，
// 依赖方向因此保持单向：适配器不 import 业务 App）。
type FrameHandler func(sess ports.Session, f protocol.Frame)

// Manager 实现 ports.ConnManager。
type Manager struct {
	kp     *identity.KeyPair
	cfg    HandshakeConfig
	policy Policy
	clk    ports.Clock
	lg     *slog.Logger
	bus    ports.EventBus

	mu       sync.RWMutex
	sessions map[identity.NodeID]*session

	handlerMu sync.RWMutex
	handler   FrameHandler

	acceptCh chan ports.Session
	dialSem  chan struct{}

	ln net.Listener

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var _ ports.ConnManager = (*Manager)(nil)

// NewManager 构造连接管理器（不启动）。
func NewManager(
	kp *identity.KeyPair,
	cfg HandshakeConfig,
	policy Policy,
	clk ports.Clock,
	lg *slog.Logger,
	bus ports.EventBus,
	handler FrameHandler,
) *Manager {
	if policy.MaxDialConcurrency <= 0 {
		policy.MaxDialConcurrency = DefaultPolicy().MaxDialConcurrency
	}
	if policy.MaxActiveConns <= 0 {
		policy.MaxActiveConns = DefaultPolicy().MaxActiveConns
	}
	return &Manager{
		kp:       kp,
		cfg:      cfg,
		policy:   policy,
		clk:      clk,
		lg:       lg,
		bus:      bus,
		sessions: make(map[identity.NodeID]*session),
		handler:  handler,
		acceptCh: make(chan ports.Session, 64),
		dialSem:  make(chan struct{}, policy.MaxDialConcurrency),
	}
}

// SetHandler 替换业务帧处理器。
func (m *Manager) SetHandler(h FrameHandler) {
	m.handlerMu.Lock()
	m.handler = h
	m.handlerMu.Unlock()
}

func (m *Manager) getHandler() FrameHandler {
	m.handlerMu.RLock()
	defer m.handlerMu.RUnlock()
	return m.handler
}

// Start 监听 addr 并启动 accept 与 housekeeping 循环，返回实际绑定地址。
// addr 中的端口为 0 时由系统分配（便于测试）。
func (m *Manager) Start(ctx context.Context, addr string) (string, error) {
	m.ctx, m.cancel = context.WithCancel(ctx)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("tcp: listen %s: %w", addr, err)
	}
	m.ln = ln

	m.wg.Add(2)
	go m.acceptLoop()
	go m.housekeeping()

	return ln.Addr().String(), nil
}

// LocalAddr 返回监听地址。
func (m *Manager) LocalAddr() string {
	if m.ln == nil {
		return ""
	}
	return m.ln.Addr().String()
}

// Stop 停止管理器：取消上下文、关闭监听与所有会话。
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	if m.ln != nil {
		_ = m.ln.Close()
	}

	m.mu.Lock()
	all := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.sessions = make(map[identity.NodeID]*session)
	m.mu.Unlock()

	for _, s := range all {
		_ = s.Close()
	}

	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

// ---------------------------------------------------------------------------
// 内部循环
// ---------------------------------------------------------------------------

func (m *Manager) acceptLoop() {
	defer m.wg.Done()
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			select {
			case <-m.ctx.Done():
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.handleInbound(conn)
		}()
	}
}

func (m *Manager) handleInbound(conn net.Conn) {
	res, err := handshakeResponder(conn, m.kp, m.cfg)
	if err != nil {
		m.emitError("", "inbound", err)
		_ = conn.Close()
		return
	}
	s := newSession(conn, m.clk, res, false)
	m.adopt(s, true)
}

func (m *Manager) adopt(s *session, notifyAccept bool) {
	kept, loser := m.register(s)
	if loser != nil {
		// 关闭负者前不发任何业务帧（架构书 1.5 兜底规则）。
		_ = loser.Close()
	}
	if kept == nil {
		return
	}
	if kept != s {
		return // 新会话是负者，已被关闭
	}

	m.wg.Add(1)
	go m.pump(s)

	if notifyAccept {
		select {
		case m.acceptCh <- s:
		default:
		}
	}
	m.publishOnline(s)
}

// register 执行双向拨号胜负规则。
//
// 规则（双方独立算出同一结论）：若 self.NodeID < peer.NodeID，本方负责拨号，
// 因此「isDialer == (self < peer)」的那条连接为胜者。
func (m *Manager) register(s *session) (kept, loser *session) {
	selfLess := m.kp.NodeID().Less(s.peerID)

	m.mu.Lock()
	defer m.mu.Unlock()

	existing, ok := m.sessions[s.peerID]
	if !ok {
		if len(m.sessions) >= m.policy.MaxActiveConns {
			return nil, s
		}
		m.sessions[s.peerID] = s
		return s, nil
	}

	newWins := s.isDialer == selfLess
	oldWins := existing.isDialer == selfLess

	switch {
	case newWins && !oldWins:
		m.sessions[s.peerID] = s
		return s, existing
	default:
		// 老会话胜出，或两条同型（双方同时拨号）→ 保留先到者
		return existing, s
	}
}

func (m *Manager) pump(s *session) {
	defer m.wg.Done()
	defer m.remove(s, "closed")

	for {
		f, err := s.Recv()
		if err != nil {
			_ = s.Close()
			return
		}

		switch f.Type {
		case protocol.TypeHeartbeat:
			var hb protocol.Heartbeat
			if protocol.DecodeJSON(f.Payload, &hb) == nil {
				if payload, err := protocol.EncodeJSON(protocol.HeartbeatAck{Seq: hb.Seq, SentAt: hb.SentAt}); err == nil {
					_ = s.Send(protocol.New(protocol.TypeHeartbeatAck, payload))
				}
			}
			continue
		case protocol.TypeHeartbeatAck:
			var hb protocol.HeartbeatAck
			if protocol.DecodeJSON(f.Payload, &hb) == nil {
				sent := time.Unix(0, hb.SentAt)
				s.setRTT(m.clk.Now().Sub(sent))
			}
			continue
		}

		// 同一会话的消息在 pump goroutine 内串行处理，因此连接内顺序天然保持。
		if h := m.getHandler(); h != nil {
			h(s, f)
		}
	}
}

func (m *Manager) housekeeping() {
	defer m.wg.Done()
	t := m.clk.NewTicker(m.policy.HeartbeatInterval)
	defer t.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C():
			m.Sweep()
		}
	}
}

// Sweep 执行一次保活与回收检查。导出以便测试用 FakeClock 驱动。
func (m *Manager) Sweep() {
	now := m.clk.Now()

	m.mu.RLock()
	live := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		live = append(live, s)
	}
	m.mu.RUnlock()

	for _, s := range live {
		if now.Sub(s.LastRecv()) > m.policy.HeartbeatTimeout {
			m.remove(s, "heartbeat_timeout")
			_ = s.Close()
			continue
		}
		if now.Sub(s.LastBusiness()) > m.policy.IdleTimeout {
			m.remove(s, "idle_timeout")
			_ = s.Close()
			continue
		}
	}

	for _, s := range live {
		if s.isClosed() {
			continue
		}
		hb := protocol.Heartbeat{Seq: s.nextSeq(), SentAt: m.clk.Now().UnixNano()}
		if payload, err := protocol.EncodeJSON(hb); err == nil {
			_ = s.Send(protocol.New(protocol.TypeHeartbeat, payload))
		}
	}
}

func (m *Manager) remove(s *session, reason string) {
	m.mu.Lock()
	cur, ok := m.sessions[s.peerID]
	if ok && cur == s {
		delete(m.sessions, s.peerID)
	}
	m.mu.Unlock()

	if ok && cur == s {
		if m.bus != nil {
			m.bus.Publish(eventbus.TopicPeerOffline, eventbus.PeerOffline{NodeID: s.peerID.String(), Reason: reason})
		}
		if m.lg != nil {
			m.lg.Info("peer session closed", "peer_id", s.peerID.String(), "reason", reason)
		}
	}
}

func (m *Manager) publishOnline(s *session) {
	if m.bus != nil {
		m.bus.Publish(eventbus.TopicPeerOnline, eventbus.PeerOnline{
			NodeID: s.peerID.String(),
			Addr:   s.RemoteAddr(),
			RTT:    s.RTT(),
		})
	}
	if m.lg != nil {
		m.lg.Info("peer session established", "peer_id", s.peerID.String(), "dialer", s.isDialer, "addr", s.RemoteAddr())
	}
}

func (m *Manager) emitError(peerID, op string, err error) {
	if m.bus != nil {
		m.bus.Publish(eventbus.TopicNetError, eventbus.NetError{PeerID: peerID, Op: op, Err: err})
	}
	if m.lg != nil {
		m.lg.Warn("net error", "op", op, "peer_id", peerID, "err", err)
	}
}

// ---------------------------------------------------------------------------
// ports.ConnManager
// ---------------------------------------------------------------------------

// Dial 按需拨号并完成握手。若已存在该 peer 的会话则直接复用。
func (m *Manager) Dial(ctx context.Context, id identity.NodeID, addr string) (ports.Session, error) {
	if s, ok := m.SessionOf(id); ok {
		return s, nil
	}

	select {
	case m.dialSem <- struct{}{}:
		defer func() { <-m.dialSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.done():
		return nil, fmt.Errorf("tcp: manager stopped")
	}

	// 拿到并发令牌后再查一次，避免并发重复拨号。
	if s, ok := m.SessionOf(id); ok {
		return s, nil
	}

	d := net.Dialer{Timeout: m.cfg.Timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		m.emitError(id.String(), "dial", err)
		return nil, fmt.Errorf("tcp: dial %s: %w", addr, err)
	}

	res, err := handshakeInitiator(conn, m.kp, m.cfg)
	if err != nil {
		m.emitError(id.String(), "handshake", err)
		_ = conn.Close()
		return nil, err
	}
	if res.PeerID != id {
		_ = conn.Close()
		return nil, fmt.Errorf("tcp: dialed %s but peer identity is %s (want %s)", addr, res.PeerID, id)
	}

	s := newSession(conn, m.clk, res, true)
	kept, loser := m.register(s)
	if loser != nil {
		_ = loser.Close()
	}
	if kept == nil {
		return nil, fmt.Errorf("tcp: rejected %s: max active conns reached", id)
	}
	if kept != s {
		return kept, nil
	}

	m.wg.Add(1)
	go m.pump(s)
	m.publishOnline(s)
	return s, nil
}

// Accept 返回一个已完成握手的入站会话。
func (m *Manager) Accept(ctx context.Context) (ports.Session, error) {
	select {
	case s := <-m.acceptCh:
		return s, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.done():
		return nil, fmt.Errorf("tcp: manager stopped")
	}
}

// SessionOf 返回该 peer 当前的会话。
func (m *Manager) SessionOf(id identity.NodeID) (ports.Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, false
	}
	return s, true
}

// Broadcast 向指定 peers 发送帧；ids 为空表示全部会话。
func (m *Manager) Broadcast(f protocol.Frame, ids ...identity.NodeID) error {
	targets := ids
	if len(targets) == 0 {
		m.mu.RLock()
		for id := range m.sessions {
			targets = append(targets, id)
		}
		m.mu.RUnlock()
	}

	var firstErr error
	for _, id := range targets {
		s, ok := m.SessionOf(id)
		if !ok {
			continue
		}
		if err := s.Send(f); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Close 关闭指定 peer 的会话。
func (m *Manager) Close(id identity.NodeID) error {
	m.mu.RLock()
	s, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	m.remove(s, "closed")
	return s.Close()
}

// SessionCount 返回当前会话数（诊断用）。
func (m *Manager) SessionCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

func (m *Manager) done() <-chan struct{} {
	if m.ctx == nil {
		return nil
	}
	return m.ctx.Done()
}
