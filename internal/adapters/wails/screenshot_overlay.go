package wails

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/infra/imagecodec"
)

// ---------------------------------------------------------------------------
// 覆盖层窗口：壳层能力的抽象
// ---------------------------------------------------------------------------

// OverlayOptions 描述一个覆盖层窗口要开在哪、加载什么。
type OverlayOptions struct {
	// Name 是窗口的唯一名字（用来关它）。
	Name string
	// URL 是窗口要加载的页面。
	URL string
	// X / Y / Width / Height 是窗口在虚拟桌面上的位置与尺寸，
	// 单位是 DIP 逻辑像素 —— 与 Wails 的窗口定位同一套坐标。
	X, Y, Width, Height int
	// PhysicalX / PhysicalY / PhysicalWidth / PhysicalHeight 是同一个
	// 矩形的【物理像素】版本。
	//
	// 为什么两份：DIP 坐标在「缩放比例不同的多屏」上换算回物理像素后
	// 与显示器真实边界对不上（Windows 的虚拟桌面 DIP 布局是按 Chromium
	// 的规则算的）。平台实现能用物理坐标精确摆窗时就用物理坐标。
	PhysicalX, PhysicalY, PhysicalWidth, PhysicalHeight int
}

// PinOptions 描述一个贴图窗口要开在哪、多大、加载什么。
//
// 与 OverlayOptions 的区别：贴图窗口是「用户要看/要留着的图」，
// 尺寸就是图片尺寸（不铺满屏幕），位置就是刚才框选的位置。
type PinOptions struct {
	// Name 是窗口的唯一名字（关它的时候用）。
	Name string
	// URL 是窗口要加载的页面。
	URL string
	// X / Y / Width / Height 是窗口在虚拟桌面上的 DIP 位置与尺寸。
	X, Y, Width, Height int
	// onClose 在窗口被关掉时回调（无论关它的是谁：用户 Alt+F4、系统
	// 关机、还是 ClosePin）。
	//
	// 存在的理由：图片字节存在 Go 侧，窗口没了就必须一起释放；
	// 只在 ClosePin 里删会漏掉「用户直接关窗口」这条路。
	//
	// 刻意不导出：它不是给前端看的字段（函数也没法 JSON 编码），
	// 只是服务层与壳层之间的回调。
	onClose func()
}

// OverlayHost 是「开一个覆盖层窗口」这项壳层能力的最小抽象。
//
// 抽象存在的理由：截屏的覆盖层要开窗、关窗、发事件，这三件事都是
// 桌面壳层的职责；把它们抽成接口，服务层就能在没有 Wails 运行时的
// 环境里被测试（换个假实现即可），而不必把 application.App 引进服务层。
//
// 组合根（main_wails.go）提供真实实现。
type OverlayHost interface {
	// OpenOverlay 开一个覆盖层窗口。
	OpenOverlay(opts OverlayOptions) error
	// ShowOverlay 显示一个覆盖层窗口（前端画好第一帧之后调用）。
	ShowOverlay(name string) error
	// FocusOverlay 把键盘焦点移到一个覆盖层窗口。
	//
	// 为什么需要：多屏时每个屏各有一个覆盖层窗口，键盘焦点只可能在
	// 其中一个上。用户用鼠标在 A 屏选区、焦点却在 B 屏时，Esc / Enter
	// 会被送到 B 屏去 —— 表现为「按了没反应」。所以用户一动手（第一次
	// 鼠标按下）就把焦点抢回自己这块屏。
	FocusOverlay(name string) error
	// CloseOverlays 关掉全部覆盖层窗口。
	CloseOverlays() error
	// MinimizeMain 最小化主窗口，并在系统确认收起之后返回。
	//
	// 「隐藏本窗口」的截屏靠它：抓屏必须发生在窗口真正收起之后，
	// 否则会抓到动画的中间帧（半透明、残影），甚至抓到一块黑。
	MinimizeMain() error
	// RestoreMain 把主窗口从最小化恢复（截图会话结束时调用）。
	RestoreMain() error
	// OpenPin 开一个贴图窗口（桌面置顶的图片窗口）。
	OpenPin(opts PinOptions) error
	// CloseWindow 关掉指定名字的窗口。
	CloseWindow(name string) error
	// Emit 向所有窗口广播一个事件（主窗口靠它收到截图结果）。
	Emit(name string, data any)
}

// ---------------------------------------------------------------------------
// 会话状态
// ---------------------------------------------------------------------------

// overlaySession 是一次交互式截图的会话状态。
//
// 底图（每块屏各一张 JPEG）存在这里而不是直接发给前端，是因为
// 「抓屏」必须发生在「覆盖层窗口出现」【之前】—— 窗口一旦出现，
// 它自己就会入镜。所以顺序是：先抓 → 存 → 再开窗 → 窗口从会话里取图。
type overlaySession struct {
	// images[i] 是第 i 块屏的底图（JPEG 字节）。
	images [][]byte
	// displays[i] 是第 i 块屏的物理信息。
	displays []ports.Display
	// mainMinimized 记下「这次是否为了抓屏把主窗口最小化了」。
	//
	// 只有为 true 时收尾才需要恢复窗口 —— keep_window 模式下去动
	// 用户的窗口是最不该做的事。
	mainMinimized bool
}

// ---------------------------------------------------------------------------
// 会话流程
// ---------------------------------------------------------------------------

// BeginCapture 开始一次交互式截图：抓下所有屏幕，开出覆盖层窗口。
//
// 覆盖层窗口铺在【每一块】显示器上，屏幕上因此整体变暗，用户可以
// 在任意一块屏上拖拽选区 —— 与微信/QQ 的多屏行为一致。
//
// mode 只决定「画面里要不要有蜂群自己」：
//   - keep_window  保留本窗口 —— 主窗口留在屏幕上，抓屏时把它拍进去；
//   - hide_window  排除本窗口 —— 先把主窗口最小化，确认收起后再抓屏，
//     截图会话结束时再恢复。
//
// 为什么「排除本窗口」不用 DDA（CaptureDisplay 的 excludeSelf）：那条路
// 在部分显卡/驱动组合下会把窗口区域拍成一块纯黑 —— 也就是用户看到的
// 「隐藏窗口变成黑框」。最小化是从「窗口根本不在画面上」这个根上解决的，
// 不依赖抓屏实现的行为。
//
// 抓屏必须先于开窗完成：窗口一旦出现，它自己就会入镜。
func (s *ScreenshotService) BeginCapture(mode string) (OverlaySessionDTO, error) {
	if s.d.Capturer == nil {
		return OverlaySessionDTO{}, fmt.Errorf("截屏功能不可用：本机没有可用的屏幕抓取实现")
	}
	if s.d.Overlay == nil {
		return OverlaySessionDTO{}, fmt.Errorf("截屏功能不可用：本机没有可用的覆盖层窗口实现")
	}

	// 上一次会话可能还开着（用户按了 Esc 但窗口没关干净）。
	_ = s.closeOverlaySession()

	displays, err := s.d.Capturer.Displays()
	if err != nil {
		return OverlaySessionDTO{}, err
	}
	if len(displays) == 0 {
		return OverlaySessionDTO{}, fmt.Errorf("没有找到可截屏的显示器")
	}

	// 只有 hide_window 会把窗口收起来；其余（含空串、未知值）一律按
	// 「保留本窗口」处理 —— 默认值不该动用户的窗口。
	hide := mode == ports.ScreenshotModeHideWindow
	if hide {
		if err := s.d.Overlay.MinimizeMain(); err != nil {
			return OverlaySessionDTO{}, fmt.Errorf("隐藏本窗口失败: %w", err)
		}
	}

	sess := &overlaySession{
		displays:      displays,
		images:        make([][]byte, len(displays)),
		mainMinimized: hide,
	}
	// 会话尽早挂上：下面任何一步失败，都由 closeOverlaySession 收尾 ——
	// 它同时负责关掉已开的窗口、把最小化的主窗口恢复回来。
	// 若把这一步留到最后，中途失败会让用户的主窗口「不见了」。
	s.mu.Lock()
	s.session = sess
	s.mu.Unlock()

	// 逐块抓屏。一律不排除自身：hide 模式下窗口已经最小化（画面里本来
	// 就没有它），keep 模式下用户要的就是画面带上蜂群界面。
	for i := range displays {
		img, err := s.d.Capturer.CaptureDisplay(context.Background(), i, false)
		if err != nil {
			_ = s.closeOverlaySession()
			return OverlaySessionDTO{}, fmt.Errorf("抓取第 %d 块显示器失败: %w", i, err)
		}
		raw, err := encodeScreenshot(img)
		if err != nil {
			_ = s.closeOverlaySession()
			return OverlaySessionDTO{}, err
		}
		inline, err := imagecodec.Prepare(raw, screenshotMaxEdge, screenshotMaxBytes)
		if err != nil {
			_ = s.closeOverlaySession()
			return OverlaySessionDTO{}, fmt.Errorf("处理第 %d 块显示器的画面失败: %w", i, err)
		}
		sess.images[i] = inline.Data
	}

	// 开窗。每个显示器一个窗口。
	for i, d := range displays {
		opts := OverlayOptions{
			Name:           fmt.Sprintf("screenshot-overlay-%d", i),
			URL:            fmt.Sprintf("/?mode=screenshot&display=%d", i),
			X:              d.X,
			Y:              d.Y,
			Width:          d.Width,
			Height:         d.Height,
			PhysicalX:      d.PhysicalX,
			PhysicalY:      d.PhysicalY,
			PhysicalWidth:  d.PhysicalWidth,
			PhysicalHeight: d.PhysicalHeight,
		}
		if err := s.d.Overlay.OpenOverlay(opts); err != nil {
			_ = s.closeOverlaySession()
			return OverlaySessionDTO{}, fmt.Errorf("打开覆盖层窗口失败: %w", err)
		}
	}

	dto := OverlaySessionDTO{Displays: make([]OverlayDisplayDTO, 0, len(displays))}
	for i, d := range displays {
		dto.Displays = append(dto.Displays, OverlayDisplayDTO{
			Index:          i,
			X:              d.X,
			Y:              d.Y,
			Width:          d.Width,
			Height:         d.Height,
			PhysicalWidth:  d.PhysicalWidth,
			PhysicalHeight: d.PhysicalHeight,
			ScaleFactor:    d.ScaleFactor,
		})
	}
	return dto, nil
}

// OverlayReady 覆盖层说「我已经画好第一帧了」。
//
// 这时才把窗口显示出来。窗口若是一创建就显示，用户会看到一块白屏
// 在屏幕上一闪而过（WebView 加载 + 首帧渲染要一两百毫秒）。
func (s *ScreenshotService) OverlayReady(index int) error {
	if s.d.Overlay == nil {
		return fmt.Errorf("截屏功能不可用")
	}
	return s.d.Overlay.ShowOverlay(fmt.Sprintf("%s%d", overlayWindowPrefix, index))
}

// FocusOverlay 把键盘焦点抢到本块屏的覆盖层窗口上。
//
// 由前端在用户第一次按下鼠标时调用：多屏时焦点可能在别的屏上，
// 不抢回来的话 Esc / Enter 会打空。
func (s *ScreenshotService) FocusOverlay(index int) error {
	if s.d.Overlay == nil {
		return fmt.Errorf("截屏功能不可用")
	}
	return s.d.Overlay.FocusOverlay(fmt.Sprintf("%s%d", overlayWindowPrefix, index))
}

// GetOverlayImage 取本块显示器的底图。
//
// 由覆盖层窗口自己在加载完成后调用 —— 底图不随 BeginCapture 的返回值
// 下发，因为主窗口拿到它也没用（主窗口不显示选区）。
func (s *ScreenshotService) GetOverlayImage(index int) (OverlayImageDTO, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil {
		return OverlayImageDTO{}, fmt.Errorf("截图会话已结束")
	}
	if index < 0 || index >= len(s.session.images) {
		return OverlayImageDTO{}, fmt.Errorf("显示器序号越界（要第 %d 块）", index)
	}
	d := s.session.displays[index]
	return OverlayImageDTO{
		MIME:        "image/jpeg",
		Width:       d.PhysicalWidth,
		Height:      d.PhysicalHeight,
		ScaleFactor: d.ScaleFactor,
		DataBase64:  base64.StdEncoding.EncodeToString(s.session.images[index]),
	}, nil
}

// CommitOverlay 提交用户在覆盖层里选好并标注完的结果。
//
// 结果以 base64 JPEG 交上来：裁剪与标注都在前端 Canvas 上完成
// （同一套代码在三个平台通用），后端只做校验与投递。
func (s *ScreenshotService) CommitOverlay(dataB64 string, width, height int) error {
	data, err := decodeImageBytes(dataB64)
	if err != nil {
		return err
	}
	if len(data) > message.MaxInlineImageBytes {
		return fmt.Errorf("截图结果过大（%d 字节），超出单条图片消息上限", len(data))
	}
	if width <= 0 || height <= 0 {
		return fmt.Errorf("截图结果尺寸无效（%d×%d）", width, height)
	}
	if _, err := imagecodec.Sniff(data); err != nil {
		return fmt.Errorf("截图结果不是可识别的图片")
	}

	if err := s.closeOverlaySession(); err != nil {
		return err
	}
	// 通知主窗口：草稿就绪。
	s.d.Overlay.Emit(EventScreenshotReady, ScreenshotDTO{
		MIME:       "image/jpeg",
		Width:      width,
		Height:     height,
		DataBase64: base64.StdEncoding.EncodeToString(data),
	})
	return nil
}

// CancelOverlay 放弃这次截图。
func (s *ScreenshotService) CancelOverlay() error {
	return s.closeOverlaySession()
}

// closeOverlaySession 关掉全部覆盖层窗口并清掉会话。
//
// 幂等：没有会话时也返回 nil（关窗口那一步可能会失败，但那时
// 已经没什么可收拾的了 —— 不该因此让用户看到一条错误）。
//
// 顺序是「先关覆盖层、再恢复主窗口」：反过来的话，主窗口会在覆盖层
// 还铺在屏幕上时冒出来，用户会看到它被压在遮罩底下闪一下。
func (s *ScreenshotService) closeOverlaySession() error {
	s.mu.Lock()
	sess := s.session
	s.session = nil
	s.mu.Unlock()

	if s.d.Overlay == nil {
		return nil
	}
	closeErr := s.d.Overlay.CloseOverlays()

	// 只有「为了抓屏而最小化过」才恢复：keep_window 模式下动用户的
	// 窗口是最不该做的事，哪怕只是多一次 Show/Focus。
	if sess != nil && sess.mainMinimized {
		if err := s.d.Overlay.RestoreMain(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}
