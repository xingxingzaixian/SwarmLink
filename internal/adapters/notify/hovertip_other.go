//go:build !windows && !darwin && !linux

package notify

// HoverTip 在没有实现的气泡通知平台上什么也不做：不弹、不崩。
//
// 目前的实现覆盖 Windows（自建 tooltips_class32 提示框）、macOS（osascript
// 的 display notification）、Linux（notify-send）。其余平台没有「系统自带、
// 无需额外依赖」的通知通道，因此宁可不显示，也不引入半成品。
//
// 与 play_other.go 一样：别把这个文件当成「可以删掉的占位」——
// 没有它，这些平台直接编译不过。
type HoverTip struct{}

// NewHoverTip 构造一个不做任何事的悬停提示。
func NewHoverTip() *HoverTip { return &HoverTip{} }

// Show 在这些平台是空操作。
func (t *HoverTip) Show(string) error { return nil }

// ShowNearNotificationArea 在这些平台是空操作。
func (t *HoverTip) ShowNearNotificationArea(string) error { return nil }

// Hide 在这些平台是空操作。
func (t *HoverTip) Hide() {}

// Close 在这些平台是空操作。
func (t *HoverTip) Close() {}

// LastSize 在这些平台恒为 0,0（没有提示框窗口）。
func (t *HoverTip) LastSize() (int, int) { return 0, 0 }
