package transfer

import "testing"

func TestStateMachineHappyPath(t *testing.T) {
	m := NewStateMachine()
	if m.State() != StateIdle {
		t.Fatal("must start idle")
	}
	for _, s := range []State{StateMetaExchange, StateTransferring, StateVerifying, StateDone} {
		if err := m.Transition(s); err != nil {
			t.Fatalf("transition to %s: %v", s, err)
		}
	}
}

// 暂停后的恢复路径对两侧是不同的（D-07）：
//   - 发送侧必须重走 META 重新协商位图（app 层保证：恢复 = 重跑 sendFile）；
//   - 接收侧位图在自己手里，可以直接回 TRANSFERRING。
//
// 这条钉住两条迁移都合法、且都能继续走到 DONE。
func TestPausedResumePaths(t *testing.T) {
	sender := NewStateMachine()
	_ = sender.Transition(StateMetaExchange)
	_ = sender.Transition(StateTransferring)
	if err := sender.Transition(StatePaused); err != nil {
		t.Fatal(err)
	}
	if err := sender.Transition(StateMetaExchange); err != nil {
		t.Fatalf("发送侧恢复必须能重走 META: %v", err)
	}
	if err := sender.Transition(StateTransferring); err != nil {
		t.Fatal(err)
	}
	if err := sender.Transition(StateVerifying); err != nil {
		t.Fatal(err)
	}
	if err := sender.Transition(StateDone); err != nil {
		t.Fatal(err)
	}

	receiver := NewStateMachine()
	_ = receiver.Transition(StateMetaExchange)
	_ = receiver.Transition(StateTransferring)
	if err := receiver.Transition(StatePaused); err != nil {
		t.Fatal(err)
	}
	if err := receiver.Transition(StateTransferring); err != nil {
		t.Fatalf("接收侧恢复可以直接续传: %v", err)
	}
	if err := receiver.Transition(StateVerifying); err != nil {
		t.Fatal(err)
	}
	if err := receiver.Transition(StateDone); err != nil {
		t.Fatal(err)
	}
}

// 暂停不能绕过重协商「偷偷」变成别的中间态：只有取消是另一条出路。
func TestPausedOnlyExitsViaMetaExchangeTransferringOrCancel(t *testing.T) {
	for _, to := range []State{StateIdle, StateVerifying, StateDone, StateFailed} {
		m := NewStateMachine()
		_ = m.Transition(StateMetaExchange)
		_ = m.Transition(StateTransferring)
		_ = m.Transition(StatePaused)
		if err := m.Transition(to); err == nil {
			t.Fatalf("PAUSED -> %s must be illegal", to)
		}
		if m.State() != StatePaused {
			t.Fatalf("state must be unchanged after illegal transition, got %s", m.State())
		}
	}
}

func TestVerifyFailureReturnsToTransferring(t *testing.T) {
	m := NewStateMachine()
	_ = m.Transition(StateMetaExchange)
	_ = m.Transition(StateTransferring)
	_ = m.Transition(StateVerifying)
	// 校验失败 → 回 TRANSFERRING 重传坏块（不清空位图）
	if err := m.Transition(StateTransferring); err != nil {
		t.Fatalf("VERIFYING -> TRANSFERRING must be allowed: %v", err)
	}
}

func TestIllegalTransitions(t *testing.T) {
	m := NewStateMachine()
	if err := m.Transition(StateDone); err == nil {
		t.Fatal("IDLE -> DONE must be illegal")
	}
	if m.State() != StateIdle {
		t.Fatal("state must be unchanged after illegal transition")
	}
	_ = m.Transition(StateMetaExchange)
	_ = m.Transition(StateTransferring)
	_ = m.Transition(StateVerifying)
	_ = m.Transition(StateDone)
	if err := m.Transition(StateTransferring); err == nil {
		t.Fatal("DONE is terminal")
	}
}

func TestDBStatusMapping(t *testing.T) {
	cases := map[State]string{
		StateIdle:         "queued",
		StateMetaExchange: "active",
		StateTransferring: "active",
		StatePaused:       "paused",
		StateVerifying:    "verifying",
		StateDone:         "done",
		StateFailed:       "failed",
		StateCancelled:    "cancelled",
	}
	for st, want := range cases {
		if got := st.DBStatus(); got != want {
			t.Fatalf("%s: want %s got %s", st, want, got)
		}
	}
}
