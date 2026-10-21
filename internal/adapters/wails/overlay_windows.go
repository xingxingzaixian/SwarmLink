//go:build windows

package wails

import (
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/w32"
)

// positionOverlayWindow 把覆盖层窗口精确摆到目标显示器的物理矩形上。
//
// 为什么需要这一步：Wails 的窗口定位走 DIP（逻辑像素）↔ 物理像素的换算，
// 而虚拟桌面的 DIP 布局是按 Chromium 的规则算出来的（以主屏为根的树）。
// 在【缩放比例不同的多屏】上，它算出的 DIP 坐标换算回物理像素后与显示器
// 的真实边界对不上 —— 表现是覆盖层没有铺满，屏幕边缘露出一条缝，
// 而选区也就跟着偏了。
//
// 直接用 SetWindowPos 给物理坐标就没有这个问题：显示器边界本来就是
// 物理像素，1:1 对得上。
func positionOverlayWindow(win *application.WebviewWindow, opts OverlayOptions) {
	if opts.PhysicalWidth <= 0 || opts.PhysicalHeight <= 0 {
		return
	}
	ptr := win.NativeWindow()
	if ptr == nil {
		return
	}
	w32.SetWindowPos(
		w32.HWND(ptr),
		w32.HWND_TOPMOST,
		opts.PhysicalX,
		opts.PhysicalY,
		opts.PhysicalWidth,
		opts.PhysicalHeight,
		w32.SWP_NOACTIVATE|w32.SWP_FRAMECHANGED,
	)
}
