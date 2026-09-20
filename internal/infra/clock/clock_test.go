package clock

import (
	"testing"
	"time"
)

func TestFakeClockAdvances(t *testing.T) {
	fc := NewFake(time.Unix(0, 0))
	base := fc.Now()
	fc.Advance(5 * time.Second)
	if d := fc.Now().Sub(base); d != 5*time.Second {
		t.Fatalf("want 5s got %v", d)
	}
}

func TestFakeAfterFiresOnAdvance(t *testing.T) {
	fc := NewFake(time.Unix(0, 0))
	ch := fc.After(30 * time.Second)

	select {
	case <-ch:
		t.Fatal("fired too early")
	default:
	}

	fc.Advance(30 * time.Second)
	select {
	case <-ch:
	default:
		t.Fatal("expected to fire after advance")
	}
}

func TestFakeAfterDoesNotFireBeforeDeadline(t *testing.T) {
	fc := NewFake(time.Unix(0, 0))
	ch := fc.After(10 * time.Second)
	fc.Advance(9 * time.Second)
	select {
	case <-ch:
		t.Fatal("fired before deadline")
	default:
	}
	fc.Advance(time.Second)
	select {
	case <-ch:
	default:
		t.Fatal("should have fired at deadline")
	}
}

func TestRealClockNowMonotonicEnough(t *testing.T) {
	c := New()
	if c.Now().IsZero() {
		t.Fatal("zero time")
	}
}
