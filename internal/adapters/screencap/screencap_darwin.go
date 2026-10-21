//go:build darwin && cgo

package screencap

/*
#cgo LDFLAGS: -framework CoreGraphics -framework CoreFoundation

#include <CoreGraphics/CoreGraphics.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>

// sl_capture_rect 抓取【指定矩形】那块屏幕的画面。
//
// excludePID 目前只接受 0：CoreGraphics 没有「排除某个进程」这种合成方式。
// 唯一的近似做法 CGWindowListCreateImageFromArray 是「只合成这些窗口」，
// 会把桌面壁纸与非窗口内容一起丢掉；而「画面里不要本应用」这个需求，
// 调用方本来就是靠【先最小化主窗口】解决的（见 screenshot_overlay.go 的说明），
// 那条路在各平台都成立，不必在这里再造一份不可靠的实现。
// Go 侧遇到 excludeSelf=true 会直接返回 ErrUnsupported，不会带着默认值进来。
// 返回 0 表示成功；成功时缓冲区由本函数 calloc，调用方负责 free。
static int sl_capture_rect(CGRect screenRect, int32_t excludePID, int32_t *outW, int32_t *outH, uint8_t **outData) {
	if (!outW || !outH || !outData) return 1;
	*outW = 0; *outH = 0; *outData = NULL;
	(void)excludePID; // 见函数说明：本平台只支持「整屏合成」

	// 整屏合成：kCGNullWindowID 表示「矩形内所有在屏窗口」。
	//
	// 注意 32 位类型名是 kCFNumberSInt32Type（CFNumber.h）；写成
	// kCFNumberInt32Type 在 Windows 的思维里很自然，但它在 SDK 里不存在。
	CGImageRef img = CGWindowListCreateImage(screenRect, kCGWindowListOptionOnScreenOnly,
		kCGNullWindowID, kCGWindowImageDefault);
	if (!img) return 4;

	size_t w = CGImageGetWidth(img);
	size_t h = CGImageGetHeight(img);
	if (w == 0 || h == 0) { CGImageRelease(img); return 5; }

	uint8_t *buf = (uint8_t *)calloc(w * h * 4, 1);
	if (!buf) { CGImageRelease(img); return 6; }

	CGColorSpaceRef cs = CGColorSpaceCreateDeviceRGB();
	CGContextRef ctx = CGBitmapContextCreate(buf, w, h, 8, w * 4, cs,
		kCGImageAlphaPremultipliedLast | kCGBitmapByteOrder32Big);
	CGColorSpaceRelease(cs);
	if (!ctx) { free(buf); CGImageRelease(img); return 7; }
	CGContextDrawImage(ctx, CGRectMake(0, 0, (CGFloat)w, (CGFloat)h), img);
	CGContextRelease(ctx);
	CGImageRelease(img);

	*outW = (int32_t)w;
	*outH = (int32_t)h;
	*outData = buf;
	return 0;
}

// sl_capture 抓取【光标所在那块显示器】的画面。
static int sl_capture(int32_t excludePID, int32_t *outW, int32_t *outH, uint8_t **outData) {
	CGEventRef ev = CGEventCreate(NULL);
	if (!ev) return 2;
	CGPoint cursor = CGEventGetLocation(ev);
	CFRelease(ev);

	CGDirectDisplayID display = 0;
	uint32_t dispCount = 0;
	if (CGGetDisplaysWithPoint(cursor, 1, &display, &dispCount) != kCGErrorSuccess || dispCount == 0) {
		return 3;
	}
	return sl_capture_rect(CGDisplayBounds(display), excludePID, outW, outH, outData);
}

// sl_capture_display 抓取【序号为 index 的那块显示器】的画面。
static int sl_capture_display(int32_t index, int32_t excludePID, int32_t *outW, int32_t *outH, uint8_t **outData) {
	CGDirectDisplayID displays[32];
	uint32_t count = 0;
	if (CGGetActiveDisplayList(32, displays, &count) != kCGErrorSuccess || count == 0) {
		return 8;
	}
	if (index < 0 || index >= (int32_t)count) {
		return 9;
	}
	return sl_capture_rect(CGDisplayBounds(displays[index]), excludePID, outW, outH, outData);
}

// sl_displays 枚举显示器。
//
// 每块 6 个 int32：x, y, w, h（逻辑点，CoreGraphics 的全局坐标，原点在主屏左上）
// 与 physW, physH（物理像素）。两者之比即缩放比。
static int sl_displays(int32_t *out, int32_t outMax, int32_t *outCount) {
	if (!out || !outCount) return 1;
	CGDirectDisplayID displays[32];
	uint32_t count = 0;
	if (CGGetActiveDisplayList(32, displays, &count) != kCGErrorSuccess || count == 0) {
		return 2;
	}
	if (count > (uint32_t)outMax) {
		count = (uint32_t)outMax;
	}
	for (uint32_t i = 0; i < count; i++) {
		CGRect b = CGDisplayBounds(displays[i]);
		out[i * 6 + 0] = (int32_t)b.origin.x;
		out[i * 6 + 1] = (int32_t)b.origin.y;
		out[i * 6 + 2] = (int32_t)b.size.width;
		out[i * 6 + 3] = (int32_t)b.size.height;
		out[i * 6 + 4] = (int32_t)CGDisplayPixelsWide(displays[i]);
		out[i * 6 + 5] = (int32_t)CGDisplayPixelsHigh(displays[i]);
	}
	*outCount = (int32_t)count;
	return 0;
}
*/
import "C"

import (
	"context"
	"fmt"
	"unsafe"

	"github.com/swarmlink/swarmlink/internal/domain/ports"
)

// errNoExcludeSelf 说明 macOS 不支持「抓屏时排除自身窗口」。
//
// 明确报错而不是悄悄忽略参数：调用方要的是「画面里没有自己」，
// 悄悄返回一张带自己的图，比失败更难查。正确的替代做法是先最小化窗口。
var errNoExcludeSelf = fmt.Errorf("%w：macOS 抓屏不支持排除自身窗口，请先最小化窗口", ErrUnsupported)

func capture(ctx context.Context, excludeSelf bool) (ports.ScreenImage, error) {
	if err := ctx.Err(); err != nil {
		return ports.ScreenImage{}, err
	}
	if excludeSelf {
		return ports.ScreenImage{}, errNoExcludeSelf
	}
	var w, h C.int32_t
	var data *C.uint8_t
	rc := C.sl_capture(0, &w, &h, &data)
	if rc != 0 {
		return ports.ScreenImage{}, fmt.Errorf("screencap: macOS 抓屏失败（错误码 %d）", int(rc))
	}
	defer C.free(unsafe.Pointer(data))
	size := int(w) * int(h) * 4
	raw := C.GoBytes(unsafe.Pointer(data), C.int(size))
	return ports.ScreenImage{Width: int(w), Height: int(h), Data: raw}, nil
}

// captureDisplay 抓取序号为 index 的那块显示器。
func captureDisplay(ctx context.Context, index int, excludeSelf bool) (ports.ScreenImage, error) {
	if err := ctx.Err(); err != nil {
		return ports.ScreenImage{}, err
	}
	if excludeSelf {
		return ports.ScreenImage{}, errNoExcludeSelf
	}
	var w, h C.int32_t
	var data *C.uint8_t
	rc := C.sl_capture_display(C.int32_t(index), 0, &w, &h, &data)
	if rc != 0 {
		return ports.ScreenImage{}, fmt.Errorf("screencap: macOS 抓取第 %d 块显示器失败（错误码 %d）", index, int(rc))
	}
	defer C.free(unsafe.Pointer(data))
	size := int(w) * int(h) * 4
	raw := C.GoBytes(unsafe.Pointer(data), C.int(size))
	return ports.ScreenImage{Width: int(w), Height: int(h), Data: raw}, nil
}

// displays 枚举显示器。
//
// CGDisplayBounds 给的是【逻辑点】（已按缩放折算），CGDisplayPixelsWide
// 给的是【物理像素】—— 两者之比就是缩放比，和 Windows 的 DPI 语义一致。
func displays() ([]ports.Display, error) {
	buf := make([]C.int32_t, 32*6)
	var count C.int32_t
	if rc := C.sl_displays(&buf[0], C.int32_t(32), &count); rc != 0 {
		return nil, fmt.Errorf("screencap: macOS 枚举显示器失败（错误码 %d）", int(rc))
	}
	out := make([]ports.Display, 0, int(count))
	for i := 0; i < int(count); i++ {
		b := buf[i*6 : i*6+6]
		physW := int(b[4])
		physH := int(b[5])
		logicW := int(b[2])
		logicH := int(b[3])
		scale := 1.0
		if logicW > 0 {
			scale = float64(physW) / float64(logicW)
		}
		out = append(out, ports.Display{
			Index:          i,
			X:              int(b[0]),
			Y:              int(b[1]),
			Width:          logicW,
			Height:         logicH,
			ScaleFactor:    scale,
			PhysicalWidth:  physW,
			PhysicalHeight: physH,
			PhysicalX:      int(float64(b[0]) * scale),
			PhysicalY:      int(float64(b[1]) * scale),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("screencap: macOS 没有枚举到任何显示器")
	}
	return out, nil
}
