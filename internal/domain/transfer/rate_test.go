package transfer

import (
	"math"
	"testing"
	"time"
)

// rateBase 是测试用的固定起点（虚拟时钟从不停顿，样本间隔全靠 Advance 控制）。
func rateBase() time.Time { return time.Unix(1_700_000_000, 0) }

// 匀速传输：窗口内的平均速率必须贴近真实速率。
func TestRateMeterConstantRate(t *testing.T) {
	var m RateMeter
	t0 := rateBase()
	m.Advance(0, t0)
	for i := 1; i <= 5; i++ {
		m.Advance(int64(i)<<20, t0.Add(time.Duration(i)*time.Second))
	}
	const want = float64(1 << 20) // 1 MB/s
	if got := m.Speed(); math.Abs(got-want) > want*0.01 {
		t.Fatalf("Speed() = %v，期望 ≈ %v", got, want)
	}
}

// 第一个样本之后没有速率可算：必须给 0，而不是除零或一个编出来的数。
func TestRateMeterNoSpeedBeforeSecondSample(t *testing.T) {
	var m RateMeter
	m.Advance(0, rateBase())
	if s := m.Speed(); s != 0 {
		t.Fatalf("只有一个样本时 Speed() = %v，期望 0", s)
	}
	if e := m.ETA(1 << 20); e != 0 {
		t.Fatalf("没有速率时 ETA() = %v，期望 0（前端据此不显示剩余时间）", e)
	}
}

// 停滞：新样本不再带来字节，旧样本滑出窗口后速率必须归零。
//
// 否则界面上会永远挂着一个「传输中的速度」，而文件早就停在那儿了 ——
// 这正是选滑动窗口而不是 EMA 的主要原因。
func TestRateMeterStallDecaysToZero(t *testing.T) {
	var m RateMeter
	t0 := rateBase()
	m.Advance(0, t0)
	m.Advance(1<<20, t0.Add(time.Second))
	if m.Speed() == 0 {
		t.Fatal("前置条件不成立：有增量时必须有速率")
	}
	// 停滞：每个节拍都来报到，但字节不再增长。
	// 一个窗口之后旧样本全部滑出，速率必须归零。
	ticks := int(RateWindow/time.Second) + 2
	for i := 2; i <= ticks; i++ {
		m.Advance(1<<20, t0.Add(time.Duration(i)*time.Second))
	}
	if s := m.Speed(); s != 0 {
		t.Fatalf("停滞超过一个窗口后 Speed() = %v，期望 0", s)
	}
}

// ETA = 剩余字节 / 速率；没有剩余时是 0。
func TestRateMeterETA(t *testing.T) {
	var m RateMeter
	t0 := rateBase()
	m.Advance(0, t0)
	m.Advance(1<<20, t0.Add(time.Second)) // 1 MB/s
	if got := m.ETA(4 << 20); got != 4*time.Second {
		t.Fatalf("ETA(4MB) = %v，期望 4s", got)
	}
	if got := m.ETA(0); got != 0 {
		t.Fatalf("ETA(0) = %v，期望 0", got)
	}
	if got := m.ETA(-1); got != 0 {
		t.Fatalf("ETA(-1) = %v，期望 0", got)
	}
}

// 同一时刻的多个样本（时钟没走）不能算出速率：除零会得到 +Inf。
func TestRateMeterZeroElapsed(t *testing.T) {
	var m RateMeter
	t0 := rateBase()
	m.Advance(0, t0)
	m.Advance(1<<20, t0)
	if s := m.Speed(); s != 0 {
		t.Fatalf("dt=0 时 Speed() = %v，期望 0", s)
	}
	if e := m.ETA(1 << 20); e != 0 {
		t.Fatalf("dt=0 时 ETA() = %v，期望 0", e)
	}
}

// 累计值倒退 = 换了计量对象（重试开了新任务）：必须重置而不是算出负速率。
func TestRateMeterResetOnDoneRegression(t *testing.T) {
	var m RateMeter
	t0 := rateBase()
	m.Advance(1<<20, t0)
	m.Advance(2<<20, t0.Add(time.Second))
	m.Advance(512<<10, t0.Add(2*time.Second)) // 倒退：新任务从一半开始
	if s := m.Speed(); s != 0 {
		t.Fatalf("倒退后 Speed() = %v，期望 0（重置后只剩一个样本）", s)
	}
	// 重置之后必须能重新计量，不能卡死在 0
	m.Advance(1<<20, t0.Add(3*time.Second))
	if s := m.Speed(); s == 0 {
		t.Fatal("重置后应能重新计量出速率")
	}
}

// 时钟回拨的样本必须被丢弃（负 dt 会算出负速率）。
func TestRateMeterIgnoresBackwardClock(t *testing.T) {
	var m RateMeter
	t0 := rateBase()
	m.Advance(0, t0)
	m.Advance(1<<20, t0.Add(time.Second))
	m.Advance(2<<20, t0) // 回拨：整个样本作废
	if s := m.Speed(); s != float64(1<<20) {
		t.Fatalf("回拨样本应被丢弃，Speed() = %v，期望 %v", s, float64(1<<20))
	}
}

// 窗口外的旧样本不参与计算：只保留最近 RateWindow 内的样本。
func TestRateMeterWindowDropsOldSamples(t *testing.T) {
	var m RateMeter
	t0 := rateBase()
	m.Advance(0, t0)
	m.Advance(1<<20, t0.Add(RateWindow+time.Second)) // 与上一个样本相距超窗口
	// 第一个样本已被淘汰 → 只剩一个样本 → 没有速率
	if s := m.Speed(); s != 0 {
		t.Fatalf("相距超过窗口时 Speed() = %v，期望 0（旧样本已淘汰）", s)
	}
	// 第三个样本距第二个 1 秒：窗口内只剩后两个
	m.Advance(2<<20, t0.Add(RateWindow+2*time.Second))
	if s := m.Speed(); s != float64(1<<20) {
		t.Fatalf("Speed() = %v，期望 %v（窗口内 (2MB-1MB)/1s）", s, float64(1<<20))
	}
}

// 环容量边界：超过 rateSamplesCap 个样本不能 panic，且速率仍然只由
// 最近的那段窗口决定。
func TestRateMeterHandlesMoreSamplesThanCapacity(t *testing.T) {
	var m RateMeter
	t0 := rateBase()
	// 以 100ms 间隔喂 100 个样本，每个带 100KB：超过 32 容量、也超过 5 秒窗口
	for i := 0; i <= 100; i++ {
		m.Advance(int64(i)*100<<10, t0.Add(time.Duration(i)*100*time.Millisecond))
	}
	// 窗口（5 秒）内的样本是最后 50 个左右，每 100ms 100KB → 约 1MB/s
	const (
		lo = float64(900 << 10)
		hi = float64(1<<20) * 1.1
	)
	if s := m.Speed(); s < lo || s > hi {
		t.Fatalf("Speed() = %v，期望 ≈ 1 MB/s", s)
	}
}
