package app

import (
	"strings"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
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
	"no such file",
	"is a directory",
	"permission denied",
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
