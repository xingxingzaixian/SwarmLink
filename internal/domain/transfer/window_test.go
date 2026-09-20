package transfer

import (
	"testing"
	"time"
)

func TestWindowDefaultsAndClamp(t *testing.T) {
	w := NewWindow(512 * 1024)
	if w.Size() != DefaultWindow {
		t.Fatalf("want default %d got %d", DefaultWindow, w.Size())
	}
	w.SetSize(1000)
	if w.Size() != MaxWindow {
		t.Fatalf("want clamp to %d got %d", MaxWindow, w.Size())
	}
	w.SetSize(0)
	if w.Size() != MinWindow {
		t.Fatalf("want clamp to %d got %d", MinWindow, w.Size())
	}
}

func TestClampWindow(t *testing.T) {
	if ClampWindow(-5) != MinWindow || ClampWindow(5) != 5 || ClampWindow(999) != MaxWindow {
		t.Fatal("clamp broken")
	}
}

func TestWindowRecommendedFromBDP(t *testing.T) {
	w := NewWindow(512 * 1024)
	// RTT 100ms, 吞吐 100MB/s → BDP = 10,000,000 B → /524288 ≈ 19
	got := w.Recommended(100*time.Millisecond, 100_000_000)
	if got != 19 {
		t.Fatalf("want 19 got %d", got)
	}
	// LAN: RTT 0.2ms, 1GB/s → BDP=200,000 B → /524288 = 0 → clamp 1
	if got := w.Recommended(200*time.Microsecond, 1_000_000_000); got != MinWindow {
		t.Fatalf("want %d got %d", MinWindow, got)
	}
	// 超大 BDP 必须被 clamp
	if got := w.Recommended(5*time.Second, 1_000_000_000); got != MaxWindow {
		t.Fatalf("want %d got %d", MaxWindow, got)
	}
}

func TestWindowUpdateKeepsOldValueOnInvalidInput(t *testing.T) {
	w := NewWindow(512 * 1024)
	w.SetSize(16)
	w.Update(0, 0)
	if w.Size() != 16 {
		t.Fatal("invalid measurement must not change window")
	}
}
