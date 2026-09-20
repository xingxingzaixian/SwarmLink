// Package clock 提供可注入的时间源，使心跳超时、TTL 过期、重传退避等
// 时间相关逻辑可在测试中确定性推进（无需 time.Sleep）。
//
// 接口定义位于 domain/ports（ports.Clock / ports.Ticker），本包只提供实现，
// 依赖方向保持 infra → ports（向内）。
package clock

import (
	"sync"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/ports"
)

// New 返回基于真实时间的 Clock。
func New() ports.Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (realClock) NewTicker(d time.Duration) ports.Ticker { return &realTicker{t: time.NewTicker(d)} }

type realTicker struct{ t *time.Ticker }

func (r *realTicker) C() <-chan time.Time { return r.t.C }
func (r *realTicker) Stop()               { r.t.Stop() }

// ---------------------------------------------------------------------------
// Fake
// ---------------------------------------------------------------------------

// Fake 是可控时钟。Advance 会把到期的等待者唤醒。
type Fake struct {
	mu    sync.Mutex
	now   time.Time
	waits []*fakeWait
}

type fakeWait struct {
	at time.Time
	ch chan time.Time
}

// NewFake 创建从 start 开始的虚拟时钟。
func NewFake(start time.Time) *Fake { return &Fake{now: start} }

// Now 返回虚拟当前时间。
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance 推进虚拟时间，并唤醒所有到期的 After/Ticker 等待者。
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	now := f.now
	var fire []*fakeWait
	rest := f.waits[:0]
	for _, w := range f.waits {
		if !w.at.After(now) {
			fire = append(fire, w)
		} else {
			rest = append(rest, w)
		}
	}
	f.waits = rest
	f.mu.Unlock()

	for _, w := range fire {
		// 非阻塞投递：channel 容量为 1，不会卡住 Advance。
		select {
		case w.ch <- now:
		default:
		}
	}
}

// After 返回在虚拟时间 +d 时收到一次的 channel。
func (f *Fake) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 1)
	f.waits = append(f.waits, &fakeWait{at: f.now.Add(d), ch: ch})
	return ch
}

// NewTicker 在 Fake 上退化为单次 After（测试中通常 Advance 后重新订阅）。
func (f *Fake) NewTicker(d time.Duration) ports.Ticker {
	return &fakeTicker{c: f.After(d)}
}

type fakeTicker struct{ c <-chan time.Time }

func (t *fakeTicker) C() <-chan time.Time { return t.c }
func (t *fakeTicker) Stop()               {}
