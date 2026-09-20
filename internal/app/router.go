package app

import (
	"log/slog"

	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
)

// Router 把入站业务帧分派到各应用，并记录处理错误。
//
// 它由组合根注入给 tcp.ConnManager 作为 FrameHandler —— 这样 tcp 适配器
// 无需 import 任何业务 App（依赖方向单向向内），而
// 「业务帧 → 领域状态/事件」的编排留在应用层。
type Router struct {
	Chat      *ChatApp
	Group     *GroupApp
	Transfer  *TransferApp
	Discovery *DiscoveryApp
	Peers     *PeerApp
	Log       *slog.Logger
}

// NewRouter 构造路由器。
func NewRouter(
	chat *ChatApp,
	group *GroupApp,
	tr *TransferApp,
	disc *DiscoveryApp,
	peers *PeerApp,
	lg *slog.Logger,
) *Router {
	return &Router{Chat: chat, Group: group, Transfer: tr, Discovery: disc, Peers: peers, Log: lg}
}

// Handle 实现 tcp.FrameHandler。
//
// 注意：HEARTBEAT / HEARTBEAT_ACK 永远不会到达这里 —— 它们由 tcp 管理器
// 在读取循环内部就地处理（保活与 RTT 是连接层职责）。
func (r *Router) Handle(sess ports.Session, f protocol.Frame) {
	switch f.Type {
	case protocol.TypeChat:
		r.dispatch(sess, f, handleOf(r.Chat, (*ChatApp).HandleChat))
	case protocol.TypeChatAck:
		r.dispatch(sess, f, handleOf(r.Chat, (*ChatApp).HandleChatAck))

	case protocol.TypeGroupMsg:
		r.dispatch(sess, f, handleOf(r.Group, (*GroupApp).HandleGroupMsg))
	case protocol.TypeGroupMeta:
		r.dispatch(sess, f, handleOf(r.Group, (*GroupApp).HandleGroupMeta))
	case protocol.TypeGroupMembersReq:
		r.dispatch(sess, f, handleOf(r.Group, (*GroupApp).HandleMembersReq))
	case protocol.TypeGroupMembersRes:
		r.dispatch(sess, f, handleOf(r.Group, (*GroupApp).HandleMembersResp))

	case protocol.TypePeerListReq:
		r.dispatch(sess, f, handleOf(r.Discovery, (*DiscoveryApp).HandlePeerListReq))
	case protocol.TypePeerListResp:
		r.dispatch(sess, f, handleOf(r.Discovery, (*DiscoveryApp).HandlePeerListResp))

	case protocol.TypeFileMeta:
		r.dispatch(sess, f, handleOf(r.Transfer, (*TransferApp).HandleFileMeta))
	case protocol.TypeFileMetaAck:
		r.dispatch(sess, f, handleOf(r.Transfer, (*TransferApp).HandleFileMetaAck))
	case protocol.TypeFileChunk:
		r.dispatch(sess, f, handleOf(r.Transfer, (*TransferApp).HandleFileChunk))
	case protocol.TypeFileChunkAck:
		r.dispatch(sess, f, handleOf(r.Transfer, (*TransferApp).HandleFileChunkAck))
	case protocol.TypeFileDone:
		r.dispatch(sess, f, handleOf(r.Transfer, (*TransferApp).HandleFileDone))
	case protocol.TypeFileDoneAck:
		r.dispatch(sess, f, handleOf(r.Transfer, (*TransferApp).HandleFileDoneAck))

	default:
		// 未知 Type：载荷已按 Length 读完，直接丢弃并计数，不得断连。
		if r.Log != nil {
			r.Log.Debug("dropping unknown frame", "type", f.Type)
		}
	}
}

func (r *Router) dispatch(sess ports.Session, f protocol.Frame, fn func(ports.Session, protocol.Frame) error) {
	if fn == nil {
		return
	}
	if err := fn(sess, f); err != nil && r.Log != nil {
		r.Log.Warn("frame handling failed", "type", protocol.TypeName(f.Type), "err", err)
	}
}

type handlerFunc = func(ports.Session, protocol.Frame) error

// handleOf 把 (*T).Method 提升为闭包；当 app 为 nil 时返回 nil（调用方跳过）。
func handleOf[T any](app *T, m func(*T, ports.Session, protocol.Frame) error) handlerFunc {
	if app == nil {
		return nil
	}
	return func(s ports.Session, f protocol.Frame) error { return m(app, s, f) }
}
