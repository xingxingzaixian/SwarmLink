package transfer

import "time"

// 速率估算的滑动窗口参数。
//
// 窗口取 5 秒的理由：短于一个「块往返 + 重传」周期，会把一次重传抖动
// 算成速率暴跌，界面数字看起来像坏了；长于 ~10 秒又明显滞后 ——
// 用户已经切换了动作，速度却还在报几秒前的旧账。
// 采样由进度节流驱动（默认 250ms，即 4 Hz），5 秒约 20 个样本，
// 足够平滑又不迟钝。
const (
	// RateWindow 是参与速率计算的样本时间跨度。
	RateWindow = 5 * time.Second
	// rateSamplesCap 是样本环的容量：容量 × 最快采样率必须 ≥ 窗口，
	// 否则窗口会被动缩短。32 × 4Hz = 8 秒 > 5 秒，正常走不到淘汰。
	rateSamplesCap = 32
)

// rateSample 是一个采样点：采样时刻与该时刻的【累计完成字节】。
type rateSample struct {
	at   time.Time
	done int64
}

// RateMeter 用滑动窗口估算「累计完成字节」的增长速率。
//
// 为什么按累计值而不是每次调用的增量：调用方（发送/接收两侧）天然知道
// 「一共完成了多少字节」（已确认位图 × 块大小），而增量口径要求每一次
// chunk 都来报到，还要自己防「重传导致重复计数」。累计口径下重传天然
// 无害 —— 位图里已置位的块不会让累计值倒退。
//
// 为什么是滑动窗口而不是 EMA：两者都能平滑，但窗口有明确的物理含义
// （「最近 5 秒的平均速率」），且停滞时的行为天然正确 —— 没有新字节进来，
// 旧样本滑出窗口，速率自动衰减到 0；EMA 不额外加「停滞检测」就会让一个
// 旧速度永远挂在界面上，而文件早就停在那儿了。
//
// 零值可用：采样环是定长数组，不需要构造函数，也不产生分配。
type RateMeter struct {
	samples [rateSamplesCap]rateSample
	head    int // 环内最旧样本的下标
	count   int // 有效样本数
}

// Advance 记录一个采样点（累计完成字节 + 采样时刻）。
//
// at 必须单调不减：时钟回拨的样本会让 (last - first) 变成负数，
// 直接丢弃比算出一个负速率再 clamp 诚实。
func (m *RateMeter) Advance(done int64, at time.Time) {
	if m.count > 0 {
		last := m.samples[(m.head+m.count-1)%rateSamplesCap]
		if at.Before(last.at) {
			return // 时钟回拨：丢弃这个样本
		}
		if done < last.done {
			// 累计值倒退只可能是「换了计量对象」（同一 job 重试开了新任务）。
			// 不清历史就会算出负速率，而负速率比没有速率更糟。
			m.reset()
		}
	}
	m.push(rateSample{at: at, done: done})
	m.evictOld(at)
}

// Speed 返回窗口内的平均速率（字节/秒）。
//
// 样本不足两个、窗口内时间跨度为 0、或窗口内字节没有增长时返回 0 ——
// 「不知道」必须显示成没有速度，而不是一个编出来的数。
func (m *RateMeter) Speed() float64 {
	if m.count < 2 {
		return 0
	}
	first := m.samples[m.head]
	last := m.samples[(m.head+m.count-1)%rateSamplesCap]
	dt := last.at.Sub(first.at)
	if dt <= 0 {
		return 0
	}
	return float64(last.done-first.done) / dt.Seconds()
}

// ETA 按当前速率估算还要多久传完 remaining 字节。
//
// 速率为 0（尚不知道）或没有剩余时返回 0：前端对 0 的处理是「不显示」，
// 而「剩余 3 小时」这种精度造假不如没有。
func (m *RateMeter) ETA(remaining int64) time.Duration {
	if remaining <= 0 {
		return 0
	}
	speed := m.Speed()
	if speed <= 0 {
		return 0
	}
	d := time.Duration(float64(remaining) / speed * float64(time.Second))
	if d < 0 {
		return 0 // 理论上到不了（speed>0、remaining>0），防御浮点怪值
	}
	return d
}

// push 压入样本；环满时覆盖最旧样本（等价于先丢最旧再压入）。
func (m *RateMeter) push(s rateSample) {
	if m.count == rateSamplesCap {
		m.samples[m.head] = s
		m.head = (m.head + 1) % rateSamplesCap
		return
	}
	m.samples[(m.head+m.count)%rateSamplesCap] = s
	m.count++
}

// evictOld 淘汰滑出窗口的样本。刻意保留最后一个样本：
// 它是下一次 Advance 计算增量的起点，也是「停滞」与「刚结束」的分界。
func (m *RateMeter) evictOld(now time.Time) {
	for m.count > 1 && now.Sub(m.samples[m.head].at) > RateWindow {
		m.head = (m.head + 1) % rateSamplesCap
		m.count--
	}
}

func (m *RateMeter) reset() { m.head, m.count = 0, 0 }
