//go:build windows

package screencap

import (
	"context"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/swarmlink/swarmlink/internal/domain/ports"
)

// ---------------------------------------------------------------------------
// Win32 入口
//
// 手写 syscall 而不是引第三方库：本仓的 Windows 能力一贯是手写的
// （见 internal/adapters/notify/hovertip_windows.go），依赖面小、可逐个核对。
// ---------------------------------------------------------------------------

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	// shcore 提供 GetDpiForMonitor（Win 8.1+）。Win 7 上没有这个 DLL，
	// 因此不能直接 NewProc 后调用 —— 那会 panic 而不是返回错误。
	shcore = syscall.NewLazyDLL("shcore.dll")

	procGetCursorPos             = user32.NewProc("GetCursorPos")
	procMonitorFromPoint         = user32.NewProc("MonitorFromPoint")
	procGetMonitorInfo           = user32.NewProc("GetMonitorInfoW")
	procEnumDisplayMonitors      = user32.NewProc("EnumDisplayMonitors")
	procGetDC                    = user32.NewProc("GetDC")
	procReleaseDC                = user32.NewProc("ReleaseDC")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procSetWindowDisplayAffinity = user32.NewProc("SetWindowDisplayAffinity")
	procSetThreadDPIContext      = user32.NewProc("SetThreadDpiAwarenessContext")

	procGetDpiForMonitor = shcore.NewProc("GetDpiForMonitor")

	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procBitBlt             = gdi32.NewProc("BitBlt")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procDeleteObject       = gdi32.NewProc("DeleteObject")

	procGetCurrentProcessID = kernel32.NewProc("GetCurrentProcessId")
)

const (
	// monitorDefaultToNearest：找不到精确包含该点的显示器时取最近的一个。
	monitorDefaultToNearest = 0x00000002

	// srcCopy：SRCCOPY，纯拷贝不混色。
	srcCopy = 0x00CC0020

	// dibRGB：BITMAPINFO 的 biCompression 取 BI_RGB（未压缩）。
	dibRGB = 0x00

	// biRGB：BITMAPINFOHEADER 的压缩方式。
	biRGB = 0

	// 显示亲和性（DDA, SetWindowDisplayAffinity）。
	//
	// 这三个值是【粘性】的：设上去就一直生效，直到被显式改掉。
	wdaNone = 0x00000000
	// wdaMonitor：窗口在屏幕捕获中显示为黑块。需要 Win7+。
	wdaMonitor = 0x00000001
	// wdaExcludeFromCapture：窗口【不出现在】屏幕捕获中。需要 Win10 2004+。
	wdaExcludeFromCapture = 0x00000020
)

// winPoint 对应 Win32 的 POINT（LONG = int32）。
type winPoint struct{ X, Y int32 }

// winRect 对应 Win32 的 RECT（LONG = int32）。
type winRect struct{ Left, Top, Right, Bottom int32 }

// winMonitorInfo 对应 Win32 的 MONITORINFO。
type winMonitorInfo struct {
	Size    uint32
	Monitor winRect
	Work    winRect
	Flags   uint32
}

// winBitmapInfoHeader 对应 Win32 的 BITMAPINFOHEADER。
type winBitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

// winBitmapInfo 对应 Win32 的 BITMAPINFO（头 + 调色板）。
type winBitmapInfo struct {
	Header winBitmapInfoHeader
	Colors [1]uint32
}

// ---------------------------------------------------------------------------
// 主流程
// ---------------------------------------------------------------------------

// capture 抓取「鼠标所在那块显示器」的画面。
//
// 顺序很重要：
//
//  1. 先切 DPI 感知 —— 坐标必须在同一套尺度下取，否则截出来的图会错位；
//  2. 再设置窗口排除 —— 必须在抓屏【之前】设、抓屏【之后】复原；
//  3. 最后才是抓 + 取像素。
func capture(ctx context.Context, excludeSelf bool) (ports.ScreenImage, error) {
	if err := ctx.Err(); err != nil {
		return ports.ScreenImage{}, err
	}

	// 排除本应用的窗口。DDA 是粘性的，因此复原函数必须挂在 defer 上：
	// 任何一条提前返回的路径都不能把它漏掉（漏掉的后果是用户此后
	// 用任何工具截图都拍不到蜂群，而且没有任何报错会提示他去找原因）。
	if excludeSelf {
		restore := excludeOwnWindows()
		defer restore()
	}

	restoreDPI := enterPerMonitorDPI()
	defer restoreDPI()

	rect, err := cursorMonitorRect()
	if err != nil {
		return ports.ScreenImage{}, err
	}
	return grabRect(rect)
}

// captureDisplay 抓取序号为 index 的那块显示器。
func captureDisplay(ctx context.Context, index int, excludeSelf bool) (ports.ScreenImage, error) {
	if err := ctx.Err(); err != nil {
		return ports.ScreenImage{}, err
	}
	if excludeSelf {
		restore := excludeOwnWindows()
		defer restore()
	}
	restoreDPI := enterPerMonitorDPI()
	defer restoreDPI()

	ds, err := displays()
	if err != nil {
		return ports.ScreenImage{}, err
	}
	if index < 0 || index >= len(ds) {
		return ports.ScreenImage{}, fmt.Errorf("screencap: 显示器序号越界（要第 %d 块，实际只有 %d 块）", index, len(ds))
	}
	d := ds[index]
	// 用物理坐标抓，而不是 DIP 换算回来的值 —— 换算带舍入，
	// 而抓屏要的是【像素级】对齐。
	rect := winRect{
		Left:   int32(d.PhysicalX),
		Top:    int32(d.PhysicalY),
		Right:  int32(d.PhysicalX + d.PhysicalWidth),
		Bottom: int32(d.PhysicalY + d.PhysicalHeight),
	}
	return grabRect(rect)
}

// displays 枚举所有显示器。
//
// 返回 DIP 坐标 + 缩放比 + 物理矩形，三者缺一不可：
// 覆盖层窗口要 DIP 坐标来定位（Wails 的窗口定位就是 DIP），
// 抓屏要物理矩形（抓的是物理像素），前端要缩放比把物理像素画到 DIP 尺寸上。
func displays() ([]ports.Display, error) {
	restoreDPI := enterPerMonitorDPI()
	defer restoreDPI()

	var out []ports.Display
	cb := syscall.NewCallback(func(hMon uintptr, _ uintptr, _ uintptr, _ uintptr) uintptr {
		mi := winMonitorInfo{Size: uint32(unsafe.Sizeof(winMonitorInfo{}))}
		if r, _, _ := procGetMonitorInfo.Call(hMon, uintptr(unsafe.Pointer(&mi))); r == 0 {
			return 1 // 拿不到就跳过这一块，但继续枚举
		}
		scale := monitorScaleFactor(hMon)
		physW := int(mi.Monitor.Right - mi.Monitor.Left)
		physH := int(mi.Monitor.Bottom - mi.Monitor.Top)
		out = append(out, ports.Display{
			Index:          len(out),
			X:              int(float64(mi.Monitor.Left) / scale),
			Y:              int(float64(mi.Monitor.Top) / scale),
			Width:          int(float64(physW) / scale),
			Height:         int(float64(physH) / scale),
			ScaleFactor:    scale,
			PhysicalWidth:  physW,
			PhysicalHeight: physH,
			PhysicalX:      int(mi.Monitor.Left),
			PhysicalY:      int(mi.Monitor.Top),
		})
		return 1 // TRUE：继续枚举
	})
	procEnumDisplayMonitors.Call(0, 0, cb, 0)
	// 回调对象必须在 EnumDisplayMonitors 返回之前保持存活。
	runtime.KeepAlive(cb)

	if len(out) == 0 {
		return nil, fmt.Errorf("screencap: 没有枚举到任何显示器（Win32 错误 %d）", lastError())
	}
	return out, nil
}

// monitorScaleFactor 返回该显示器的缩放比（DPI/96）。
//
// 拿不到时回退到 1.0：宁可让覆盖层窗口在缩放屏上对不齐，
// 也不要让整个截屏功能因为一个 DPI 查询失败而不可用。
func monitorScaleFactor(hMon uintptr) float64 {
	// shcore.dll 只在 Win 8.1 以后才有。syscall.LazyDLL.Load 返回错误而不是
	// 让随后的 Call panic —— 那条路径的报错信息会指向「找不到 GetDpiForMonitor
	// 这个过程」而不是「这台系统太老」，把排查方向带偏。
	if err := shcore.Load(); err != nil {
		return 1.0
	}
	var dpiX, dpiY uint32
	// MDT_EFFECTIVE_DPI = 0：取「实际生效」的 DPI，而不是标称值。
	if r, _, _ := procGetDpiForMonitor.Call(hMon, 0,
		uintptr(unsafe.Pointer(&dpiX)), uintptr(unsafe.Pointer(&dpiY))); r == 0 || dpiX == 0 {
		return 1.0
	}
	return float64(dpiX) / 96.0
}

// cursorMonitorRect 返回鼠标当前所在显示器的屏幕矩形（物理像素）。
func cursorMonitorRect() (winRect, error) {
	var pt winPoint
	if r, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt))); r == 0 {
		return winRect{}, fmt.Errorf("screencap: 读取鼠标位置失败（Win32 错误 %d）", lastError())
	}
	hMon, _, _ := procMonitorFromPoint.Call(uintptr(unsafe.Pointer(&pt)), monitorDefaultToNearest)
	if hMon == 0 {
		return winRect{}, fmt.Errorf("screencap: 找不到鼠标所在的显示器（Win32 错误 %d）", lastError())
	}
	mi := winMonitorInfo{Size: uint32(unsafe.Sizeof(winMonitorInfo{}))}
	if r, _, _ := procGetMonitorInfo.Call(hMon, uintptr(unsafe.Pointer(&mi))); r == 0 {
		return winRect{}, fmt.Errorf("screencap: 读取显示器信息失败（Win32 错误 %d）", lastError())
	}
	return mi.Monitor, nil
}

// grabRect 把屏幕矩形拷进内存位图并取出像素。
func grabRect(rect winRect) (ports.ScreenImage, error) {
	w := int(rect.Right - rect.Left)
	h := int(rect.Bottom - rect.Top)
	if w <= 0 || h <= 0 {
		return ports.ScreenImage{}, fmt.Errorf("screencap: 显示器区域无效（%d×%d）", w, h)
	}

	screenDC, _, _ := procGetDC.Call(0)
	if screenDC == 0 {
		return ports.ScreenImage{}, fmt.Errorf("screencap: 取屏幕 DC 失败（Win32 错误 %d）", lastError())
	}
	defer procReleaseDC.Call(0, screenDC)

	memDC, _, _ := procCreateCompatibleDC.Call(screenDC)
	if memDC == 0 {
		return ports.ScreenImage{}, fmt.Errorf("screencap: 创建内存 DC 失败（Win32 错误 %d）", lastError())
	}
	defer procDeleteDC.Call(memDC)

	// 直接建一块 DIB 段（而不是「建位图 + BitBlt + GetDIBits 取回」）：
	// DIB 段的像素内存是系统直接给的，BitBlt 完就能读，零中转。
	//
	// 换成 CreateDIBSection 的原因：GetDIBits 从 DDB 取像素时对
	// 「hdc 与位图的对应关系、位深、上下方向」的组合很挑，失败时
	// GetLastError 常常返回 0 —— 看起来像参数错，又不像，很难查。
	bi := winBitmapInfo{}
	bi.Header.Size = uint32(unsafe.Sizeof(winBitmapInfoHeader{}))
	bi.Header.Width = int32(w)
	// 高度取负 = 自上而下（否则图像上下颠倒）。
	bi.Header.Height = -int32(h)
	bi.Header.Planes = 1
	bi.Header.BitCount = 32
	bi.Header.Compression = biRGB

	var bits unsafe.Pointer
	dib, _, _ := procCreateDIBSection.Call(
		screenDC, uintptr(unsafe.Pointer(&bi)), dibRGB,
		uintptr(unsafe.Pointer(&bits)), 0, 0,
	)
	if dib == 0 || bits == nil {
		return ports.ScreenImage{}, fmt.Errorf("screencap: 创建 DIB 段失败（Win32 错误 %d）", lastError())
	}
	defer procDeleteObject.Call(dib)

	old, _, _ := procSelectObject.Call(memDC, dib)
	if old == 0 {
		return ports.ScreenImage{}, fmt.Errorf("screencap: 选入位图失败（Win32 错误 %d）", lastError())
	}
	defer procSelectObject.Call(memDC, old)

	// 源坐标用 int64 中转：多显示器的虚拟桌面允许负坐标，
	// 直接 uintptr(int32) 会在 32 位下截断。
	if r, _, _ := procBitBlt.Call(
		memDC, 0, 0, uintptr(w), uintptr(h),
		screenDC, uintptr(int64(rect.Left)), uintptr(int64(rect.Top)),
		srcCopy,
	); r == 0 {
		return ports.ScreenImage{}, fmt.Errorf("screencap: 复制屏幕画面失败（Win32 错误 %d）", lastError())
	}

	raw := unsafe.Slice((*byte)(bits), w*h*4)
	return ports.ScreenImage{Width: w, Height: h, Data: bgraToRGBA(raw)}, nil
}

// bgraToRGBA 把 32 位 BI_RGB 的 BGRA 像素转成 RGBA。
//
// 32 位 DIB 在内存里的通道序是 B、G、R、A（BI_RGB 下 alpha 恒为 0），
// 而我们的对外契约是 RGBA8888（见 ports.ScreenImage）。
func bgraToRGBA(src []byte) []byte {
	out := make([]byte, len(src))
	for i := 0; i+3 < len(src); i += 4 {
		out[i+0] = src[i+2]
		out[i+1] = src[i+1]
		out[i+2] = src[i+0]
		out[i+3] = 0xFF
	}
	return out
}

// ---------------------------------------------------------------------------
// 排除本应用的窗口
// ---------------------------------------------------------------------------

// excludeOwnWindows 把本进程的所有顶层窗口设为「不出现在屏幕捕获中」，
// 并返回一个复原函数。
//
// 为什么按「本进程的所有窗口」而不是「蜂群主窗口」：
// 托盘气泡（internal/adapters/notify 的自建 hover tip）也是本进程创建的
// 顶层窗口，它出现在截图里同样会让人分不清那是通知还是被截的内容。
// 按进程枚举一次就把这类窗口一并解决了。
//
// 为什么用 DDA 而不是「真隐藏 + 等待动画 + 恢复」：
// 真隐藏会让用户看到窗口瞬隐瞬现，而且在他手上有别的操作时可能抢不到
// 想要的画面；DDA 对用户完全无感，且没有等待窗口期。
func excludeOwnWindows() func() {
	pid := currentProcessID()
	var handles []syscall.Handle

	cb := syscall.NewCallback(func(hwnd syscall.Handle, _ uintptr) uintptr {
		var wpid uint32
		procGetWindowThreadProcessID.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&wpid)))
		if wpid == pid {
			handles = append(handles, hwnd)
		}
		return 1 // TRUE：继续枚举
	})
	procEnumWindows.Call(cb)
	// 回调对象必须在 EnumWindows 返回之前保持存活，否则它可能被回收，
	// 而 Win32 仍持有它的指针 —— 那是必崩的时序。
	runtime.KeepAlive(cb)

	for _, h := range handles {
		if ok, _, _ := procSetWindowDisplayAffinity.Call(uintptr(h), wdaExcludeFromCapture); ok == 0 {
			// 降级：Win10 2004 以前没有 WDA_EXCLUDEFROMCAPTURE，
			// 退到 WDA_MONITOR —— 窗口在捕获里显示为黑块。
			// 视觉上「看不到本窗口内容」这个诉求仍然成立。
			procSetWindowDisplayAffinity.Call(uintptr(h), wdaMonitor)
		}
	}

	return func() {
		for _, h := range handles {
			procSetWindowDisplayAffinity.Call(uintptr(h), wdaNone)
		}
	}
}

// ---------------------------------------------------------------------------
// DPI
// ---------------------------------------------------------------------------

// enterPerMonitorDPI 把当前线程切到 per-monitor DPI 感知，返回复原函数。
//
// 为什么必须做：进程若不是 per-monitor aware，GetDC(NULL) 拿到的坐标会被
// 系统按主屏缩放虚拟化，而 GetMonitorInfo 返回的是【物理】像素 —— 两者
// 不在同一套尺度上，截出来的图会错位。混合缩放（150% + 100%）的多屏
// 机器上这个错位最明显，而且错位量随分辨率变化，很难一眼看出原因。
func enterPerMonitorDPI() func() {
	noop := func() {}
	// Windows 10 1607 以前没有这个 API。
	if procSetThreadDPIContext.Find() != nil {
		return noop
	}
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4。
	// 必须走变量：uintptr(常量 -4) 是编译期溢出错误。
	want := int32(-4)
	old, _, _ := procSetThreadDPIContext.Call(uintptr(want))
	if old == 0 {
		return noop
	}
	return func() {
		procSetThreadDPIContext.Call(old)
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func currentProcessID() uint32 {
	pid, _, _ := procGetCurrentProcessID.Call()
	return uint32(pid)
}

func lastError() uintptr {
	// syscall.GetLastError 返回的是 error 接口，实际类型是 syscall.Errno。
	if err, ok := syscall.GetLastError().(syscall.Errno); ok {
		return uintptr(err)
	}
	return 0
}
