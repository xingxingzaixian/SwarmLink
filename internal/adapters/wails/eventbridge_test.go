package wails

import (
	"sync"
	"testing"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/infra/clock"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

type emitted struct {
	name string
	data any
}

type fakeEmitter struct {
	mu     sync.Mutex
	events []emitted
}

func (f *fakeEmitter) Emit(name string, data any) {
	f.mu.Lock()
	f.events = append(f.events, emitted{name: name, data: data})
	f.mu.Unlock()
}

func (f *fakeEmitter) all(name string) []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []any
	for _, e := range f.events {
		if e.name == name {
			out = append(out, e.data)
		}
	}
	return out
}

func TestChatReceivedIsMappedToDTO(t *testing.T) {
	bus := eventbus.New()
	em := &fakeEmitter{}
	b := NewBridge(bus, clock.New(), em)
	b.Start()
	defer b.Stop()

	var sender identity.NodeID
	sender[0] = 0xAB
	bus.Publish(eventbus.TopicChatReceived, message.Message{
		MsgID: "m1", ConvID: "c1", SenderID: sender,
		Direction: message.DirectionIn, Content: "hi",
		MsgType: message.MsgTypeText, State: message.StateDelivered,
		SentAt: time.Unix(1000, 0),
	})

	got := em.all(EventChatNewMessage)
	if len(got) != 1 {
		t.Fatalf("want 1 chat event got %d", len(got))
	}
	dto, ok := got[0].(MessageDTO)
	if !ok {
		t.Fatalf("payload type %T", got[0])
	}
	if dto.Content != "hi" || dto.SenderID != sender.String() || dto.Direction != "in" {
		t.Fatalf("unexpected dto: %+v", dto)
	}

	// 调试面板必须也收到该事件
	if len(em.all(EventDebug)) != 1 {
		t.Fatalf("debug panel feed missing, got %d", len(em.all(EventDebug)))
	}
}

func TestTransferProgressIsThrottledPerJob(t *testing.T) {
	fc := clock.NewFake(time.Unix(0, 0))
	bus := eventbus.New()
	em := &fakeEmitter{}
	b := NewBridge(bus, fc, em)
	b.Start()
	defer b.Stop()

	// 模拟「1 GB 文件 = 2048 个块」:2000 个进度事件
	for i := 0; i < 2000; i++ {
		bus.Publish(eventbus.TopicTransferProgress, eventbus.TransferProgress{
			JobID: "j1", Percent: float64(i) / 20, Status: "transferring",
		})
	}
	if got := len(em.all(EventTransferProgress)); got != 1 {
		t.Fatalf("throttle broken: 2000 publishes must yield 1 emit, got %d", got)
	}

	// 推进一个节流窗口后应放行
	fc.Advance(ProgressThrottle + time.Millisecond)
	bus.Publish(eventbus.TopicTransferProgress, eventbus.TransferProgress{JobID: "j1", Percent: 50})
	if got := len(em.all(EventTransferProgress)); got != 2 {
		t.Fatalf("want 2 emits after window, got %d", got)
	}

	// 另一个 job 有独立的节流计时
	bus.Publish(eventbus.TopicTransferProgress, eventbus.TransferProgress{JobID: "j2", Percent: 1})
	if got := len(em.all(EventTransferProgress)); got != 3 {
		t.Fatalf("per-job throttle broken, got %d", got)
	}
}

func TestPeerOnlineAndOfflineMapping(t *testing.T) {
	bus := eventbus.New()
	em := &fakeEmitter{}
	b := NewBridge(bus, clock.New(), em)
	b.Start()
	defer b.Stop()

	id := "aabbccddeeff0011"
	bus.Publish(eventbus.TopicPeerOnline, eventbus.PeerOnline{NodeID: id, Addr: "10.0.0.5:2425"})
	bus.Publish(eventbus.TopicPeerOffline, eventbus.PeerOffline{NodeID: id, Reason: "closed"})

	events := em.all(EventPeerUpdated)
	if len(events) != 2 {
		t.Fatalf("want 2 peer events got %d", len(events))
	}
	first, _ := events[0].(map[string]any)
	if first["shortId"] != "aabbccdd" || first["state"] != "online" {
		t.Fatalf("unexpected online payload: %+v", first)
	}
	second, _ := events[1].(map[string]any)
	if second["state"] != "offline" {
		t.Fatalf("unexpected offline payload: %+v", second)
	}
}

func TestIdleTimeoutIsNotReportedAsOffline(t *testing.T) {
	bus := eventbus.New()
	em := &fakeEmitter{}
	b := NewBridge(bus, clock.New(), em)
	b.Start()
	defer b.Stop()

	// 空闲回收只代表连接不再可用，节点仍可能在广播 announce
	bus.Publish(eventbus.TopicPeerOffline, eventbus.PeerOffline{NodeID: "aabbccddeeff0011", Reason: "idle_timeout"})

	events := em.all(EventPeerUpdated)
	if len(events) != 1 {
		t.Fatalf("want 1 event got %d", len(events))
	}
	payload, _ := events[0].(map[string]any)
	if payload["state"] == "offline" {
		t.Fatal("idle reclaim must NOT be surfaced as offline (UI would mislead the user)")
	}
	if payload["state"] != "discovered" {
		t.Fatalf("want discovered got %v", payload["state"])
	}
}

func TestStopUnsubscribes(t *testing.T) {
	bus := eventbus.New()
	em := &fakeEmitter{}
	b := NewBridge(bus, clock.New(), em)
	b.Start()
	b.Stop()

	bus.Publish(eventbus.TopicChatReceived, message.Message{MsgID: "m1"})
	if len(em.all(EventChatNewMessage)) != 0 {
		t.Fatal("must not emit after Stop")
	}
}
