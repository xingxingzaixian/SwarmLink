package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
)

// 应用层测试的共用 fake。
//
// 仓储一律用 internal/adapters/store/mem（它与 sqlite 是对等实现，
// 共同满足 ports 的同一组接口），只有网络会话需要自己造 ——
// 真实的 TCP 会话不是这些测试的关注点，拉进来只会让测试变慢变脆。

const (
	testSelfHex = "1122334455667788"
	testPeerHex = "aabbccddeeff0011"
)

var errNoSession = errors.New("test: 没有可用会话")

// noConns 永远连不上：所有发送都会「入库成功、发送失败」，
// 这正好让我们只观察持久化与事件，而不被网络行为干扰。
type noConns struct{}

func (noConns) Dial(context.Context, identity.NodeID, string) (ports.Session, error) {
	return nil, errNoSession
}
func (noConns) Accept(context.Context) (ports.Session, error)      { return nil, errNoSession }
func (noConns) SessionOf(identity.NodeID) (ports.Session, bool)    { return nil, false }
func (noConns) Broadcast(protocol.Frame, ...identity.NodeID) error { return nil }
func (noConns) Close(identity.NodeID) error                        { return nil }

// stubSession 只实现「能收下消息并回 ACK」所需的最小方法集。
type stubSession struct{ peer identity.NodeID }

func (s stubSession) PeerID() identity.NodeID   { return s.peer }
func (s stubSession) Send(protocol.Frame) error { return nil }
func (s stubSession) Recv() (protocol.Frame, error) {
	return protocol.Frame{}, errors.New("test: 不读")
}
func (s stubSession) RTT() time.Duration { return 0 }
func (s stubSession) Close() error       { return nil }

// recordingSession 记录是否回过 ACK —— 入站坏消息的处置要断言这一点。
type recordingSession struct {
	peer  identity.NodeID
	acked bool
}

// ackEagerSession + ackEagerConns：模拟「对端在本端发送返回【之前】就确认了」。
//
// 这不是造出来的极端情况，而是同机回环下的常态：对端在几百微秒内就回 ACK，
// 而发送 RPC 还要经过 DTO 序列化 + Wails IPC 才回到前端。也就是说
// chat:delivered 事件完全可能早于发送调用的返回 —— 谁先到是竞态，不确定。
type ackEagerSession struct {
	peer identity.NodeID
	// chat 由测试在构造出 ChatApp 之后回填（ChatApp 又依赖本会话，只能两段式）。
	chat *ChatApp
}

func (s *ackEagerSession) PeerID() identity.NodeID   { return s.peer }
func (s *ackEagerSession) Recv() (protocol.Frame, error) {
	return protocol.Frame{}, errors.New("test: 不读")
}
func (s *ackEagerSession) RTT() time.Duration { return 0 }
func (s *ackEagerSession) Close() error       { return nil }

func (s *ackEagerSession) Send(f protocol.Frame) error {
	if f.Type != protocol.TypeChat || s.chat == nil {
		return nil
	}
	var c protocol.Chat
	if err := protocol.DecodeJSON(f.Payload, &c); err != nil {
		return err
	}
	payload, err := protocol.EncodeJSON(protocol.ChatAck{MsgID: c.MsgID})
	if err != nil {
		return err
	}
	// 同步走完 ACK 处理：送达事件在 Send 返回前就已发布
	return s.chat.HandleChatAck(s, protocol.New(protocol.TypeChatAck, payload))
}

type ackEagerConns struct{ sess *ackEagerSession }

func newAckEagerConns(peer identity.NodeID) *ackEagerConns {
	return &ackEagerConns{sess: &ackEagerSession{peer: peer}}
}

func (c *ackEagerConns) Dial(context.Context, identity.NodeID, string) (ports.Session, error) {
	return c.sess, nil
}
func (c *ackEagerConns) Accept(context.Context) (ports.Session, error) { return c.sess, nil }
func (c *ackEagerConns) SessionOf(identity.NodeID) (ports.Session, bool) {
	return c.sess, true
}
func (c *ackEagerConns) Broadcast(protocol.Frame, ...identity.NodeID) error { return nil }
func (c *ackEagerConns) Close(identity.NodeID) error                       { return nil }

func (s *recordingSession) PeerID() identity.NodeID { return s.peer }
func (s *recordingSession) Send(f protocol.Frame) error {
	if f.Type == protocol.TypeChatAck {
		s.acked = true
	}
	return nil
}
func (s *recordingSession) Recv() (protocol.Frame, error) {
	return protocol.Frame{}, errors.New("test: 不读")
}
func (s *recordingSession) RTT() time.Duration { return 0 }
func (s *recordingSession) Close() error       { return nil }

func mustNodeID(t *testing.T, hex string) identity.NodeID {
	t.Helper()
	id, err := identity.ParseNodeID(hex)
	if err != nil {
		t.Fatalf("解析 NodeID %q: %v", hex, err)
	}
	return id
}

func mustGroupFrame(t *testing.T, gm protocol.GroupMsg) protocol.Frame {
	t.Helper()
	payload, err := protocol.EncodeJSON(gm)
	if err != nil {
		t.Fatalf("编码 GroupMsg: %v", err)
	}
	return protocol.New(protocol.TypeGroupMsg, payload)
}
