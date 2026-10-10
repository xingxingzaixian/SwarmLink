package eventbus

import (
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/peer"
)

// 事件载荷类型集中定义，避免各处重复声明匿名 struct。
// 各 App 订阅后据此转换为领域类型或直接转发给 UI 层。

// PeerOnline 是 peer.online 的载荷。
//
// Caps / ProtoVer 是 HELLO 协商出的共同能力（caps_common），【必须】随事件带出来：
// 目录里的 caps 原本只有两条写入路径（目录交换的第三方转述、announce 的「保留旧值」），
// 而 announce 不携带能力位 —— 于是能力位永远停在 0，任何文件夹传输都会被
// 判成「对方版本不支持」。适配器不许写目录（依赖方向 R2），
// 这条事件就是协商结果抵达目录的唯一通道。
type PeerOnline struct {
	NodeID string
	Addr   string
	RTT    time.Duration
	// Caps 是协商后的共同能力位；零值表示「本次没有协商出能力」，不成信息。
	Caps peer.Caps
	// ProtoVer 是协商后选定的协议版本。
	ProtoVer int
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
//
// Reason 目前只用于等待态（"peer_offline"）；其余状态为空串。
// 界面靠它区分「等待对方上线」与「还没轮到」—— 两者在状态上都是 idle/queued。
type TransferState struct {
	JobID  string
	From   string
	To     string
	Reason string
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
