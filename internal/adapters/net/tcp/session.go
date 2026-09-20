// Package tcp 实现 TCP 传输适配器：三段握手、按需拨号、会话级保活与空闲回收。
package tcp

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
)

const writeTimeout = 30 * time.Second

// session 实现 ports.Session。
//
// 三个时间戳的语义必须区分清楚：
//   - lastActive  : 任一方向的任一帧
//   - lastBusiness: 任一方向的【业务帧】（排除心跳）→ 空闲回收依据
//   - lastRecv    : 任一方向的接收帧 → 心跳超时依据
type session struct {
	conn net.Conn
	clk  ports.Clock

	peerID        identity.NodeID
	publicKey     []byte
	displayName   string
	caps          peer.Caps
	protoVersion  int
	remoteTCPPort uint16
	remoteUDPPort uint16
	isDialer      bool

	writeMu   sync.Mutex
	closeOnce sync.Once
	closed    chan struct{}

	rttMu sync.RWMutex
	rtt   time.Duration

	lastActive   atomic.Int64
	lastBusiness atomic.Int64
	lastRecv     atomic.Int64

	seq atomic.Uint64
}

var _ ports.Session = (*session)(nil)

func newSession(conn net.Conn, clk ports.Clock, res *HandshakeResult, isDialer bool) *session {
	s := &session{
		conn:          conn,
		clk:           clk,
		peerID:        res.PeerID,
		publicKey:     res.PublicKey,
		displayName:   res.DisplayName,
		caps:          res.Caps,
		protoVersion:  res.ProtoVersion,
		remoteTCPPort: res.TCPPort,
		remoteUDPPort: res.UDPPort,
		isDialer:      isDialer,
		closed:        make(chan struct{}),
	}
	now := clk.Now().UnixNano()
	s.lastActive.Store(now)
	s.lastBusiness.Store(now)
	s.lastRecv.Store(now)
	return s
}

// PeerID 返回对端 NodeID。
func (s *session) PeerID() identity.NodeID { return s.peerID }

// DisplayName 返回对端显示名。
func (s *session) DisplayName() string { return s.displayName }

// Caps 返回协商后的公共能力。
func (s *session) Caps() peer.Caps { return s.caps }

// ProtoVersion 返回协商后的协议版本。
func (s *session) ProtoVersion() int { return s.protoVersion }

// RemoteAddr 返回对端观测地址。
func (s *session) RemoteAddr() string {
	if s.conn == nil || s.conn.RemoteAddr() == nil {
		return ""
	}
	return s.conn.RemoteAddr().String()
}

// RemoteTCPPort 返回对端监听的 TCP 端口（从 HELLO 学到）。
func (s *session) RemoteTCPPort() uint16 { return s.remoteTCPPort }

// RemoteUDPPort 返回对端监听的 UDP 端口。
func (s *session) RemoteUDPPort() uint16 { return s.remoteUDPPort }

// IsDialer 返回本方是否为拨号方（胜负规则的输入）。
func (s *session) IsDialer() bool { return s.isDialer }

// LastActive 返回最近一次任一帧的时刻。
func (s *session) LastActive() time.Time { return time.Unix(0, s.lastActive.Load()) }

// LastBusiness 返回最近一次业务帧时刻。
func (s *session) LastBusiness() time.Time { return time.Unix(0, s.lastBusiness.Load()) }

// LastRecv 返回最近一次接收帧时刻。
func (s *session) LastRecv() time.Time { return time.Unix(0, s.lastRecv.Load()) }

func isHeartbeat(t byte) bool { return t == protocol.TypeHeartbeat || t == protocol.TypeHeartbeatAck }

// Send 编码并写入一帧。
func (s *session) Send(f protocol.Frame) error {
	if s.isClosed() {
		return fmt.Errorf("tcp: session closed")
	}

	buf, err := protocol.Encode(f)
	if err != nil {
		return err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := s.conn.Write(buf); err != nil {
		return fmt.Errorf("tcp: write: %w", err)
	}

	now := s.clk.Now().UnixNano()
	s.lastActive.Store(now)
	if !isHeartbeat(f.Type) {
		s.lastBusiness.Store(now)
	}
	return nil
}

// Recv 阻塞读取一帧。连接关闭（含对端断开、本方 Close）即返回错误。
func (s *session) Recv() (protocol.Frame, error) {
	f, err := protocol.Read(s.conn)
	if err != nil {
		return protocol.Frame{}, err
	}
	now := s.clk.Now().UnixNano()
	s.lastActive.Store(now)
	s.lastRecv.Store(now)
	if !isHeartbeat(f.Type) {
		s.lastBusiness.Store(now)
	}
	return f, nil
}

// RTT 返回心跳测得的往返时延。
func (s *session) RTT() time.Duration {
	s.rttMu.RLock()
	defer s.rttMu.RUnlock()
	return s.rtt
}

func (s *session) setRTT(d time.Duration) {
	s.rttMu.Lock()
	s.rtt = d
	s.rttMu.Unlock()
}

// nextSeq 返回下一个心跳序号。
func (s *session) nextSeq() uint64 { return s.seq.Add(1) }

// Close 关闭连接（幂等）。
func (s *session) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.closed)
		err = s.conn.Close()
	})
	return err
}

func (s *session) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}
