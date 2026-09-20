package eventbus

import "time"

// 事件载荷类型集中定义，避免各处重复声明匿名 struct。
// 各 App 订阅后据此转换为领域类型或直接转发给 UI 层。

// PeerOnline 是 peer.online 的载荷。
type PeerOnline struct {
	NodeID string
	Addr   string
	RTT    time.Duration
}

// PeerOffline 是 peer.offline 的载荷。
//
// Reason 取值：heartbeat_timeout | idle_timeout | closed | auth_failed | superseded
type PeerOffline struct {
	NodeID string
	Reason string
}

// NetError 是 net.error 的载荷。
type NetError struct {
	ConnID string
	PeerID string
	Op     string
	Err    error
}

// ConfigChanged 是 config.changed 的载荷。
// 载荷为 config.Config 值（由 adapters/config 提供），此处不引入具体类型以保持解耦。

// TransferProgress 是 transfer.progress 的载荷（已按 job 节流至 4~10 Hz）。
type TransferProgress struct {
	JobID   string
	PeerID  string
	Percent float64
	Speed   float64 // 字节/秒（滑动窗口计算，非瞬时值）
	ETA     time.Duration
	Status  string
}

// TransferState 是 transfer.state 的载荷。
type TransferState struct {
	JobID string
	From  string
	To    string
}

// TransferDone 是 transfer.done 的载荷。
type TransferDone struct {
	JobID  string
	PeerID string
	Path   string
}

// TransferError 是 transfer.error 的载荷。
type TransferError struct {
	JobID  string
	PeerID string
	Err    error
}
