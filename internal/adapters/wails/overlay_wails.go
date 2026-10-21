package wails

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// overlayWindowPrefix 是覆盖层窗口名的前缀。
//
// 关掉全部覆盖层时靠它识别哪些窗口是覆盖层 —— 主窗口不能被误关。
const overlayWindowPrefix = "screenshot-overlay-"

// 等主窗口最小化的三个参数。
//
// 超时取 600ms 是保守值：最小化动画的时长受系统设置影响（辅助功能里
// 可以把动画放慢），取太短就会抓到动画中间帧 —— 半透明残影或黑块。
// 超时后不报错（指令已经下达），只是不再等。
const (
	minimizeSettleTimeout = 600 * time.Millisecond
	minimizeSettleExtra   = 80 * time.Millisecond
	minimizePollInterval  = 15 * time.Millisecond
)

// WailsOverlayHost 是 OverlayHost 的 Wails 实现。
//
// app 在服务装配【之后】才创建（服务必须注册进 Wails 的 Options），
// 而 OverlayHost 又必须在装配时就注入 —— 所以这里用一个延迟解析的
// 引用，而不是直接持有 *application.App。
type WailsOverlayHost struct {
	mu  sync.Mutex
	app *application.App
	// main 是主窗口。它在 SetApp 之后才创建，因此同样是延迟绑定。
	main *application.WebviewWindow
}

// NewWailsOverlayHost 构造一个尚未绑定 app 的宿主。
func NewWailsOverlayHost() *WailsOverlayHost { return &WailsOverlayHost{} }

// SetApp 绑定 app（组合根在 app 创建完成后调用一次）。
func (h *WailsOverlayHost) SetApp(app *application.App) {
	h.mu.Lock()
	h.app = app
	h.mu.Unlock()
}

// SetMainWindow 绑定主窗口（组合根在创建主窗口之后调用一次）。
func (h *WailsOverlayHost) SetMainWindow(win *application.WebviewWindow) {
	h.mu.Lock()
	h.main = win
	h.mu.Unlock()
}

func (h *WailsOverlayHost) getApp() *application.App {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.app
}

func (h *WailsOverlayHost) getMain() *application.WebviewWindow {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.main
}

// MinimizeMain 最小化主窗口，并等到系统确认它已经收起。
//
// 为什么要等：最小化是有动画的（Windows 上约 150-250ms）。发出指令就
// 立刻抓屏，会抓到动画中间那一帧 —— 半透明的窗口残影，或者更糟，
// 一块还没重绘完成的黑。
func (h *WailsOverlayHost) MinimizeMain() error {
	win := h.getMain()
	if win == nil {
		return fmt.Errorf("主窗口尚未就绪")
	}
	win.Minimise()

	deadline := time.Now().Add(minimizeSettleTimeout)
	for time.Now().Before(deadline) {
		// 「已最小化」或「已不可见」都算收起：开了「最小化到托盘」的用户，
		// 最小化会直接把窗口藏起来，IsMinimised 可能一直为 false。
		if win.IsMinimised() || !win.IsVisible() {
			// 已经报告收起后再让出一帧，避开 DWM 的收尾动画。
			time.Sleep(minimizeSettleExtra)
			return nil
		}
		time.Sleep(minimizePollInterval)
	}
	// 超时不报错：最小化指令已经下达，绝大多数情况下画面已经对了。
	// 报错会让用户看到「隐藏本窗口失败」，反而更糟。
	return nil
}

// RestoreMain 把主窗口从最小化恢复，并把焦点还回去 ——
// 用户截完图是要在输入框里打字的。
func (h *WailsOverlayHost) RestoreMain() error {
	win := h.getMain()
	if win == nil {
		return nil
	}
	win.Show()
	win.Restore()
	win.Focus()
	return nil
}

// OpenOverlay 开一个覆盖层窗口。
//
// 窗口先以 Hidden 创建：内容加载完（前端画好第一帧）之后才 Show ——
// 否则用户会看到一块白屏在屏幕上一闪而过。
func (h *WailsOverlayHost) OpenOverlay(opts OverlayOptions) error {
	app := h.getApp()
	if app == nil {
		return fmt.Errorf("应用尚未就绪")
	}
	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:  opts.Name,
		Title: "截屏",
		URL:   opts.URL,
		// 初始尺寸先给个大致值，随后用 SetSize 精确设定。
		Width:  opts.Width,
		Height: opts.Height,
		// 置顶：用户随时可能在别的应用上，覆盖层必须压得住。
		AlwaysOnTop: true,
		// 无边框：覆盖层铺满整块屏幕，任何边框都是瑕疵。
		Frameless: true,
		// 不可缩放：它是一层遮罩，不是普通窗口。
		DisableResize: true,
		// 透明：覆盖层要透出底下的桌面画面。
		BackgroundColour: application.RGBA{Alpha: 0},
		// 先藏着，等前端说「画好了」。
		Hidden: true,
	})
	win.SetPosition(opts.X, opts.Y)
	win.SetSize(opts.Width, opts.Height)
	// 平台相关的精确定位（Windows 上按物理像素纠正）。
	positionOverlayWindow(win, opts)
	return nil
}

// ShowOverlay 显示一个覆盖层窗口并把焦点给它。
//
// 焦点是必须的：Esc / Enter 这类键盘操作要落在覆盖层上，
// 否则按键会被主窗口吃掉。
func (h *WailsOverlayHost) ShowOverlay(name string) error {
	app := h.getApp()
	if app == nil {
		return fmt.Errorf("应用尚未就绪")
	}
	win, ok := app.Window.GetByName(name)
	if !ok {
		return fmt.Errorf("覆盖层窗口 %s 不存在", name)
	}
	win.Show()
	win.Focus()
	return nil
}

// FocusOverlay 把键盘焦点移到一个覆盖层窗口。
func (h *WailsOverlayHost) FocusOverlay(name string) error {
	app := h.getApp()
	if app == nil {
		return fmt.Errorf("应用尚未就绪")
	}
	win, ok := app.Window.GetByName(name)
	if !ok {
		return fmt.Errorf("覆盖层窗口 %s 不存在", name)
	}
	win.Focus()
	return nil
}

// CloseOverlays 关掉全部覆盖层窗口。
func (h *WailsOverlayHost) CloseOverlays() error {
	app := h.getApp()
	if app == nil {
		return nil
	}
	for _, win := range app.Window.GetAll() {
		if strings.HasPrefix(win.Name(), overlayWindowPrefix) {
			win.Close()
		}
	}
	return nil
}

// OpenPin 开一个贴图窗口。
//
// 与覆盖层的关键区别：
//   - 尺寸就是图片尺寸（不铺满屏幕），位置就是刚才框选的位置；
//   - 立即可见（图已经在内存里，前端画出来只是一帧的事）；
//   - 不抢焦点：贴图是用来「摆在旁边对照」的，抢焦点会打断用户
//     正在打字的窗口。
func (h *WailsOverlayHost) OpenPin(opts PinOptions) error {
	app := h.getApp()
	if app == nil {
		return fmt.Errorf("应用尚未就绪")
	}
	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:  opts.Name,
		Title: "贴图",
		URL:   opts.URL,
		// 初始尺寸先给最终值：贴图窗口没有「先藏后显」的必要。
		Width:  opts.Width,
		Height: opts.Height,
		// 置顶：贴图的意义就是「压在其他窗口上面」。
		AlwaysOnTop: true,
		// 无边框：窗口边缘只该有图片本身。
		Frameless: true,
		// 不可缩放：缩放由前端按固定的比例步进改窗口尺寸（SetSize），
		// 拖边框缩放会让图片被拉伸变形，而那不是用户想要的。
		DisableResize: true,
		// 透明：图片之外的角落不该出现一块底色。
		BackgroundColour: application.RGBA{Alpha: 0},
	})
	win.SetPosition(opts.X, opts.Y)
	win.SetSize(opts.Width, opts.Height)
	// 窗口被关掉时（用户 Alt+F4 也算）通知服务层释放图片字节。
	if opts.onClose != nil {
		win.RegisterHook(events.Common.WindowClosing, func(*application.WindowEvent) {
			opts.onClose()
		})
	}
	return nil
}

// CloseWindow 关掉指定名字的窗口。
//
// 窗口不存在时返回 nil：关闭动作是幂等的 —— 用户已经关掉的贴图
// 不该因为「再关一次」而收到一条错误提示。
func (h *WailsOverlayHost) CloseWindow(name string) error {
	app := h.getApp()
	if app == nil {
		return nil
	}
	win, ok := app.Window.GetByName(name)
	if !ok {
		return nil
	}
	win.Close()
	return nil
}

// Emit 向所有窗口广播一个事件。
func (h *WailsOverlayHost) Emit(name string, data any) {
	app := h.getApp()
	if app == nil {
		return
	}
	app.Event.Emit(name, data)
}
