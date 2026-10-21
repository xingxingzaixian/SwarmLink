//go:build linux

package screencap

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"

	"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/composite"
	"github.com/BurntSushi/xgb/xinerama"
	"github.com/BurntSushi/xgb/xproto"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
)

// x11 实现走【Composite 逐窗合成】这条路。
//
// 为什么不能直接抓根窗口：开启 Composite 扩展后，子窗口被重定向到离屏
// pixmap，根窗口自身只剩下背景 —— GetImage(root) 拿到的是壁纸而不是
// 屏幕内容。而 Composite 恰恰提供了逐窗取画面的能力
// （XCompositeNameWindowPixmap），于是「正确抓屏」与「排除指定窗口」
// 变成同一件事：逐窗取，跳过不要的。
//
// 依赖选 BurntSushi/xgb（纯 Go 实现的 X11 协议库）而不是 go-xlib（cgo）：
// 本仓是纯 Go 构建，引入 cgo 会让 Linux 构建需要系统装 libX11-dev，
// 并失去交叉编译能力。
//
// 本文件未在真机上验证过 —— 仓库的开发机是 Windows。
// 语法与类型由交叉编译（GOOS=linux go build）保证，行为需要 Linux 真机走查。

// capture 在 X11 上抓取「光标所在那块屏幕」的画面。
func capture(ctx context.Context, excludeSelf bool) (ports.ScreenImage, error) {
	if err := ctx.Err(); err != nil {
		return ports.ScreenImage{}, err
	}
	// Wayland 做不到：合成器不允许应用抓取其它应用的画面。
	// 这里必须明确报错而不是给一张黑图 —— 用户会以为是别的问题。
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		return ports.ScreenImage{}, fmt.Errorf(
			"%w：当前是 Wayland 会话，合成器不允许抓屏（切到 X11 桌面后可用）", ErrUnsupported)
	}

	conn, err := xgb.NewConn()
	if err != nil {
		return ports.ScreenImage{}, fmt.Errorf("screencap: 连接 X11 显示失败: %w", err)
	}
	defer conn.Close()

	var si xproto.SetupInfo
	xproto.SetupInfoRead(conn.SetupBytes, &si)
	if len(si.Roots) == 0 {
		return ports.ScreenImage{}, fmt.Errorf("screencap: X11 没有可用的屏幕")
	}
	root := si.Roots[0].Root

	if err := composite.Init(conn); err != nil {
		return ports.ScreenImage{}, fmt.Errorf(
			"screencap: X11 Composite 扩展不可用（没有它就抓不到窗口内容）: %w", err)
	}

	rect, err := cursorScreenRect(conn, root)
	if err != nil {
		return ports.ScreenImage{}, err
	}
	return grabX11(conn, root, rect, excludeSelf)
}

// cursorScreenRect 返回光标所在屏幕的矩形（根窗口坐标系）。
//
// 优先用 Xinerama 划分的多屏几何：多屏用户移动鼠标到另一块屏时，
// 截到的应当是那块屏，而不是整个虚拟桌面。
func cursorScreenRect(conn *xgb.Conn, root xproto.Window) (xproto.Rectangle, error) {
	var rect xproto.Rectangle

	ptr, err := xproto.QueryPointer(conn, root).Reply()
	if err != nil {
		return rect, fmt.Errorf("screencap: 读取鼠标位置失败: %w", err)
	}
	cx, cy := int(ptr.RootX), int(ptr.RootY)

	if screens, err := xineramaScreens(conn); err == nil {
		for _, s := range screens {
			if cx >= int(s.X) && cx < int(s.X)+int(s.Width) &&
				cy >= int(s.Y) && cy < int(s.Y)+int(s.Height) {
				return s, nil
			}
		}
	}

	// 回退：整个根窗口。
	geom, err := xproto.GetGeometry(conn, xproto.Drawable(root)).Reply()
	if err != nil {
		return rect, fmt.Errorf("screencap: 读取屏幕尺寸失败: %w", err)
	}
	return xproto.Rectangle{X: 0, Y: 0, Width: geom.Width, Height: geom.Height}, nil
}

// xineramaScreens 返回 Xinerama 划分出的屏幕几何。
//
// 扩展不可用时返回错误，调用方据此回退到「整个根窗口」。
func xineramaScreens(conn *xgb.Conn) ([]xproto.Rectangle, error) {
	if err := xinerama.Init(conn); err != nil {
		return nil, fmt.Errorf("screencap: Xinerama 扩展不可用: %w", err)
	}
	reply, err := xinerama.QueryScreens(conn).Reply()
	if err != nil {
		return nil, fmt.Errorf("screencap: 读取多屏几何失败: %w", err)
	}
	out := make([]xproto.Rectangle, 0, len(reply.ScreenInfo))
	for _, s := range reply.ScreenInfo {
		out = append(out, xproto.Rectangle{
			X: s.XOrg, Y: s.YOrg, Width: s.Width, Height: s.Height,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("screencap: Xinerama 没有返回任何屏幕")
	}
	return out, nil
}

// displays 枚举显示器。
//
// X11 没有 per-monitor DPI 的统一约定，Xorg 也不做缩放（缩放由客户端自己
// 处理），因此这里的缩放比恒为 1：逻辑像素 = 物理像素。
//
// 几何来源是 Xinerama（多屏划分）；扩展不可用时回退到「整个根窗口」
// 当成一块屏 —— 覆盖层会铺满整个虚拟桌面。
func displays() ([]ports.Display, error) {
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("screencap: 连接 X11 显示失败: %w", err)
	}
	defer conn.Close()

	var si xproto.SetupInfo
	xproto.SetupInfoRead(conn.SetupBytes, &si)
	if len(si.Roots) == 0 {
		return nil, fmt.Errorf("screencap: X11 没有可用的屏幕")
	}
	root := si.Roots[0].Root

	rects, err := xineramaScreens(conn)
	if err != nil {
		geom, gerr := xproto.GetGeometry(conn, xproto.Drawable(root)).Reply()
		if gerr != nil {
			return nil, fmt.Errorf("screencap: 读取屏幕尺寸失败: %w", gerr)
		}
		rects = []xproto.Rectangle{{X: 0, Y: 0, Width: geom.Width, Height: geom.Height}}
	}

	out := make([]ports.Display, 0, len(rects))
	for i, r := range rects {
		out = append(out, ports.Display{
			Index:          i,
			X:              int(r.X),
			Y:              int(r.Y),
			Width:          int(r.Width),
			Height:         int(r.Height),
			ScaleFactor:    1,
			PhysicalWidth:  int(r.Width),
			PhysicalHeight: int(r.Height),
			PhysicalX:      int(r.X),
			PhysicalY:      int(r.Y),
		})
	}
	return out, nil
}

// captureDisplay 抓取序号为 index 的那块显示器。
func captureDisplay(ctx context.Context, index int, excludeSelf bool) (ports.ScreenImage, error) {
	if err := ctx.Err(); err != nil {
		return ports.ScreenImage{}, err
	}
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		return ports.ScreenImage{}, fmt.Errorf(
			"%w：当前是 Wayland 会话，合成器不允许抓屏（切到 X11 桌面后可用）", ErrUnsupported)
	}

	conn, err := xgb.NewConn()
	if err != nil {
		return ports.ScreenImage{}, fmt.Errorf("screencap: 连接 X11 显示失败: %w", err)
	}
	defer conn.Close()

	var si xproto.SetupInfo
	xproto.SetupInfoRead(conn.SetupBytes, &si)
	if len(si.Roots) == 0 {
		return ports.ScreenImage{}, fmt.Errorf("screencap: X11 没有可用的屏幕")
	}
	root := si.Roots[0].Root

	if err := composite.Init(conn); err != nil {
		return ports.ScreenImage{}, fmt.Errorf(
			"screencap: X11 Composite 扩展不可用（没有它就抓不到窗口内容）: %w", err)
	}

	ds, err := displays()
	if err != nil {
		return ports.ScreenImage{}, err
	}
	if index < 0 || index >= len(ds) {
		return ports.ScreenImage{}, fmt.Errorf("screencap: 显示器序号越界（要第 %d 块，实际只有 %d 块）", index, len(ds))
	}
	d := ds[index]
	rect := xproto.Rectangle{
		X: int16(d.PhysicalX), Y: int16(d.PhysicalY),
		Width: uint16(d.PhysicalWidth), Height: uint16(d.PhysicalHeight),
	}
	return grabX11(conn, root, rect, excludeSelf)
}

// grabX11 合成目标屏幕的画面。
func grabX11(
	conn *xgb.Conn,
	root xproto.Window,
	rect xproto.Rectangle,
	excludeSelf bool,
) (ports.ScreenImage, error) {
	cv := newCanvas(int(rect.Width), int(rect.Height))

	// 底图：根窗口自身的背景（壁纸）。子窗口被重定向后 root 只剩背景，
	// 所以这一层取到的永远是壁纸而不是窗口内容 —— 它是合成器的底。
	if bg, w, h, err := fetchDrawable(conn, xproto.Drawable(root),
		rect.X, rect.Y, rect.Width, rect.Height); err == nil {
		cv.blit(0, 0, bg, w, h)
	}

	// 把子窗口重定向到离屏，才能逐窗取画面。
	composite.RedirectSubwindows(conn, root, composite.RedirectManual)
	// 复原必须挂 defer：这是对 X server 状态的改动。
	defer composite.UnredirectSubwindows(conn, root, composite.RedirectManual)

	pidAtom, err := internAtom(conn, "_NET_WM_PID")
	if err != nil {
		return ports.ScreenImage{}, err
	}
	drawWindowTree(conn, root, root, pidAtom, cv, rect, excludeSelf)

	return ports.ScreenImage{Width: cv.w, Height: cv.h, Data: cv.pix}, nil
}

// drawWindowTree 按 Z 序把窗口画到画布上。
//
// 顺序：先画自己（自己的内容在子窗口之下），再画子窗口；
// 兄弟之间按 X11 的堆叠顺序，【从底往上】。
func drawWindowTree(
	conn *xgb.Conn,
	win xproto.Window,
	root xproto.Window,
	pidAtom xproto.Atom,
	cv *canvas,
	rect xproto.Rectangle,
	excludeSelf bool,
) {
	if excludeSelf && isOwnWindow(conn, win, pidAtom) {
		return
	}
	drawWindow(conn, win, root, cv, rect)

	children, err := xproto.QueryTree(conn, win).Reply()
	if err != nil {
		return
	}
	// QueryTree 返回的 children 是【从上到下】的，合成要从底往上画，
	// 所以倒着遍历。
	for i := len(children.Children) - 1; i >= 0; i-- {
		drawWindowTree(conn, children.Children[i], root, pidAtom, cv, rect, excludeSelf)
	}
}

// drawWindow 把一个窗口的画面贴到画布上。
func drawWindow(
	conn *xgb.Conn,
	win xproto.Window,
	root xproto.Window,
	cv *canvas,
	rect xproto.Rectangle,
) {
	geom, err := xproto.GetGeometry(conn, xproto.Drawable(win)).Reply()
	if err != nil || geom.Width == 0 || geom.Height == 0 {
		return
	}
	// 窗口坐标是相对父窗口的，贴图需要根窗口坐标系下的位置。
	tr, err := xproto.TranslateCoordinates(conn, win, root, 0, 0).Reply()
	if err != nil {
		return
	}
	// 目标画布只覆盖一块屏幕，窗口可能在画布之外 —— 按相对屏幕原点的偏移贴。
	dx := int(tr.DstX) - int(rect.X)
	dy := int(tr.DstY) - int(rect.Y)

	// CreatePixmap 的第二个参数是「资源 ID 所属的域」，这里传 0（无域）。
	// depth 固定 24：主流 Xorg 的 root depth 就是 24，ZPixmap 取回的
	// 是 32bpp（BGRX）。
	//
	// xgb 的 Cookie.Reply() 给的是原始 reply 缓冲（4 字节的 pixmap ID），
	// 不是解析好的结构体 —— 这里自己取出来。
	buf, err := xproto.CreatePixmap(conn, 24, xproto.Pixmap(0), xproto.Drawable(root),
		geom.Width, geom.Height).Reply()
	if err != nil {
		return
	}
	if len(buf) < 4 {
		return
	}
	px := xproto.Pixmap(xgb.Get32(buf))
	defer xproto.FreePixmap(conn, px)

	composite.NameWindowPixmap(conn, win, px)
	data, w, h, err := fetchDrawable(conn, xproto.Drawable(px), 0, 0, geom.Width, geom.Height)
	if err != nil {
		return
	}
	cv.blit(dx, dy, data, w, h)
}

// fetchDrawable 取一个 drawable 的像素，返回 BGRA 数据与实际尺寸。
func fetchDrawable(
	conn *xgb.Conn,
	d xproto.Drawable,
	x, y int16,
	w, h uint16,
) ([]byte, int, int, error) {
	reply, err := xproto.GetImage(conn, byte(xproto.ImageFormatZPixmap), d,
		x, y, w, h, 0xffffffff).Reply()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("screencap: 读取窗口画面失败: %w", err)
	}
	if reply == nil {
		return nil, 0, 0, fmt.Errorf("screencap: 读取窗口画面失败：X11 没有返回数据")
	}
	// ZPixmap 的 32 位像素在内存里是 B,G,R,X。
	if len(reply.Data) < int(w)*int(h)*4 {
		return nil, 0, 0, fmt.Errorf(
			"screencap: 窗口画面数据长度异常（期望 %d 字节，实得 %d 字节）",
			int(w)*int(h)*4, len(reply.Data))
	}
	return reply.Data, int(w), int(h), nil
}

// isOwnWindow 判断窗口是否属于本进程。
//
// 靠 _NET_WM_PID 属性：这是 X11 的标准约定（绝大多数现代工具都写它）。
// 读不到就当作「不是自己的」—— 宁可把窗口拍进去，也不要漏掉该排除的。
func isOwnWindow(conn *xgb.Conn, win xproto.Window, pidAtom xproto.Atom) bool {
	reply, err := xproto.GetProperty(conn, false, win, pidAtom,
		xproto.GetPropertyTypeAny, 0, 1).Reply()
	if err != nil || reply == nil || len(reply.Value) < 4 {
		return false
	}
	return int(binary.LittleEndian.Uint32(reply.Value)) == os.Getpid()
}

// internAtom 取一个原子（属性名）的 ID。
func internAtom(conn *xgb.Conn, name string) (xproto.Atom, error) {
	reply, err := xproto.InternAtom(conn, false, uint16(len(name)), name).Reply()
	if err != nil {
		return 0, fmt.Errorf("screencap: 查询 X11 属性 %s 失败: %w", name, err)
	}
	return reply.Atom, nil
}

// canvas 是合成目标：一块 RGBA 画布。
type canvas struct {
	w, h int
	pix  []byte
}

func newCanvas(w, h int) *canvas {
	return &canvas{w: w, h: h, pix: make([]byte, w*h*4)}
}

// blit 把 BGRA 的 src 贴到 (dx, dy)。越界部分被裁掉。
func (c *canvas) blit(dx, dy int, src []byte, srcW, srcH int) {
	for y := 0; y < srcH; y++ {
		ty := dy + y
		if ty < 0 || ty >= c.h {
			continue
		}
		for x := 0; x < srcW; x++ {
			tx := dx + x
			if tx < 0 || tx >= c.w {
				continue
			}
			si := (y*srcW + x) * 4
			if si+3 >= len(src) {
				continue
			}
			di := (ty*c.w + tx) * 4
			c.pix[di+0] = src[si+2]
			c.pix[di+1] = src[si+1]
			c.pix[di+2] = src[si+0]
			c.pix[di+3] = 0xFF
		}
	}
}
