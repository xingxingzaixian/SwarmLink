package transfer

import "fmt"

// State 是传输任务状态。
//
// 修正点（架构书 3.6）：
//   - PAUSED → TRANSFERRING 必须重新走 META_EXCHANGE（暂停期间对端可能已从别处拿到部分块）
//   - VERIFYING 失败【不清空位图】，标记坏块后回到 TRANSFERRING 重传坏块
type State string

const (
	StateIdle         State = "idle"
	StateMetaExchange State = "meta_exchange"
	StateTransferring State = "transferring"
	StatePaused       State = "paused"
	StateVerifying    State = "verifying"
	StateDone         State = "done"
	StateFailed       State = "failed"
	StateCancelled    State = "cancelled"
)

// transitions 是状态机的合法迁移表。
var transitions = map[State][]State{
	StateIdle:         {StateMetaExchange, StateCancelled},
	StateMetaExchange: {StateTransferring, StatePaused, StateFailed, StateCancelled},
	StateTransferring: {StatePaused, StateVerifying, StateFailed, StateCancelled},
	// 暂停后必须重新交换位图（而非直接续传）
	StatePaused:    {StateMetaExchange, StateCancelled},
	StateVerifying: {StateDone, StateTransferring, StateFailed, StateCancelled},
	StateDone:      {},
	StateFailed:    {StateMetaExchange, StateCancelled},
	StateCancelled: {},
}

// StateMachine 是单任务状态机。
type StateMachine struct {
	state State
}

// NewStateMachine 创建处于 IDLE 的状态机。
func NewStateMachine() *StateMachine { return &StateMachine{state: StateIdle} }

// State 返回当前状态。
func (m *StateMachine) State() State { return m.state }

// Can 判断是否允许迁移。
func (m *StateMachine) Can(to State) bool {
	for _, s := range transitions[m.state] {
		if s == to {
			return true
		}
	}
	return false
}

// Transition 执行迁移，非法迁移返回错误且不改变状态。
func (m *StateMachine) Transition(to State) error {
	if !m.Can(to) {
		return fmt.Errorf("transfer: illegal transition %s -> %s", m.state, to)
	}
	m.state = to
	return nil
}

// DBStatus 把领域状态映射为 transfer_jobs.status 的取值集合
// （queued/active/paused/verifying/done/failed/cancelled）。
func (s State) DBStatus() string {
	switch s {
	case StateIdle:
		return "queued"
	case StateMetaExchange, StateTransferring:
		return "active"
	case StatePaused:
		return "paused"
	case StateVerifying:
		return "verifying"
	case StateDone:
		return "done"
	case StateFailed:
		return "failed"
	case StateCancelled:
		return "cancelled"
	default:
		return "failed"
	}
}
