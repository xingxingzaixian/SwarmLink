//go:build !windows

package wails

import (
	"github.com/wailsapp/wails/v3/pkg/application"
)

// positionOverlayWindow 在非 Windows 平台上不做额外修正。
//
// Windows 上多了一块 SetWindowPos 的物理定位（见 overlay_windows.go），
// 因为那里能用物理坐标 1:1 对上显示器边界。其它平台的 Wails 窗口定位
// 直接用 DIP 坐标即可 —— 覆盖层是按每块屏分别开的，每块屏的 DIP 坐标
// 由各自的缩放比换算，不会跨屏错位。
func positionOverlayWindow(_ *application.WebviewWindow, _ OverlayOptions) {}
