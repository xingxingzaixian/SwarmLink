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

func TestPausedMustGoThroughMetaExchange(t *testing.T) {
	m := NewStateMachine()
	_ = m.Transition(StateMetaExchange)
	_ = m.Transition(StateTransferring)
	if err := m.Transition(StatePaused); err != nil {
		t.Fatal(err)
	}
	// 暂停后直接续传是非法的：必须先重新交换位图
	if err := m.Transition(StateTransferring); err == nil {
		t.Fatal("PAUSED -> TRANSFERRING must be illegal (must re-run META_EXCHANGE)")
	}
	if err := m.Transition(StateMetaExchange); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(StateTransferring); err != nil {
		t.Fatal(err)
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
