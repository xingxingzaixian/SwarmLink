//go:build !windows

package notify

// HoverTip 在非 Windows 平台是空实现：不弹提示、不崩。
//
// 要做到真弹需要各自平台的方案（macOS 的托盘菜单栏提示、Linux 的
// AppIndicator 提示），本次不做 —— 宁可不显示，也不引入半成品。
// 与 play_other.go 一样：别把这个文件当成「可以删掉的占位」。
type HoverTip struct{}

// NewHoverTip 构造一个不做任何事的悬停提示。
func NewHoverTip() *HoverTip { return &HoverTip{} }

// Show 在非 Windows 平台是空操作。
func (t *HoverTip) Show(string) error { return nil }

// ShowNearNotificationArea 在非 Windows 平台是空操作。
func (t *HoverTip) ShowNearNotificationArea(string) error { return nil }

// Hide 在非 Windows 平台是空操作。
func (t *HoverTip) Hide() {}

// Close 在非 Windows 平台是空操作。
func (t *HoverTip) Close() {}

// LastSize 在非 Windows 平台恒为 0,0（没有提示框窗口）。
func (t *HoverTip) LastSize() (int, int) { return 0, 0 }
