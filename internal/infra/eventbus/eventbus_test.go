package eventbus

import (
	"sync"
	"testing"
)

func TestPublishSubscribeSync(t *testing.T) {
	b := New()
	var got any
	unsub := b.Subscribe("t", func(p any) { got = p })

	b.Publish("t", 42)
	if got != 42 {
		t.Fatalf("want 42 got %v", got)
	}

	unsub()
	b.Publish("t", 43)
	if got != 42 {
		t.Fatalf("unsubscribe failed, got %v", got)
	}
}

func TestUnsubscribeIsIdempotent(t *testing.T) {
	b := New()
	unsub := b.Subscribe("t", func(any) {})
	unsub()
	unsub() // 不应 panic
}

func TestMultipleSubscribersAllInvoked(t *testing.T) {
	b := New()
	var mu sync.Mutex
	count := 0
	for i := 0; i < 3; i++ {
		b.Subscribe("t", func(any) { mu.Lock(); count++; mu.Unlock() })
	}
	b.Publish("t", nil)
	if count != 3 {
		t.Fatalf("want 3 got %d", count)
	}
}

func TestUnknownTopicNoop(t *testing.T) {
	b := New()
	b.Publish("nobody-listens", nil) // 不应 panic
}

func TestTopicIsolation(t *testing.T) {
	b := New()
	var a, c int
	b.Subscribe("a", func(any) { a++ })
	b.Subscribe("c", func(any) { c++ })
	b.Publish("a", nil)
	if a != 1 || c != 0 {
		t.Fatalf("topic isolation broken: a=%d c=%d", a, c)
	}
}
