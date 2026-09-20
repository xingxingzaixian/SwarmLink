package transfer

import "time"

// 滑动窗口边界（架构书 4.8）。
const (
	MinWindow     = 1
	MaxWindow     = 64
	DefaultWindow = 8
)

// Window 是传输窗口，按 BDP 自适应。
//
// 注意：我们跑在 TCP 之上，这里的窗口只是为了【填满管道】，
// 不需要（也不应该）实现自有拥塞控制 —— TCP 已经在处理拥塞。
type Window struct {
	size      int
	chunkSize int64
}

// NewWindow 创建默认窗口。
func NewWindow(chunkSize int64) *Window {
	if chunkSize <= 0 {
		chunkSize = 512 * 1024
	}
	return &Window{size: DefaultWindow, chunkSize: chunkSize}
}

// Size 返回当前窗口块数。
func (w *Window) Size() int { return w.size }

// SetSize 直接设置窗口（会被 clamp 到 [MinWindow, MaxWindow]）。
func (w *Window) SetSize(n int) { w.size = ClampWindow(n) }

// ClampWindow 把窗口大小限制在合法区间。
func ClampWindow(n int) int {
	if n < MinWindow {
		return MinWindow
	}
	if n > MaxWindow {
		return MaxWindow
	}
	return n
}

// Recommended 计算 W = clamp(BDP / chunk_size, 1, 64)，其中 BDP = RTT × 吞吐。
func (w *Window) Recommended(rtt time.Duration, throughputBps int64) int {
	if rtt <= 0 || throughputBps <= 0 {
		return w.size
	}
	bdp := float64(rtt) / float64(time.Second) * float64(throughputBps)
	want := int(bdp / float64(w.chunkSize))
	return ClampWindow(want)
}

// Update 按测量值调整窗口（每 2 个 RTT 调用一次即可）。
func (w *Window) Update(rtt time.Duration, throughputBps int64) {
	w.size = w.Recommended(rtt, throughputBps)
}
