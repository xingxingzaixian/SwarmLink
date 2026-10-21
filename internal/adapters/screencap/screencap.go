// Package screencap 实现「抓本机屏幕」这项平台能力。
//
// 存在意义：截屏要调系统 API（Win32 / CoreGraphics / X11），是典型的
// 【出站适配器】。它被 ports.ScreenCapturer 锚住，业务代码只认接口。
//
// 为什么不放 internal/app：截屏不涉及任何业务规则、没有跨用例编排、
// 没有领域状态 —— 它就是「一个系统能力的门面」。硬塞进 app 层只会造出
// 一个只有一行转发的方法。
//
// 为什么不放 internal/infra：infra 里放的是「与业务无关的纯计算 / 格式适配」
// （如 imagecodec）。抓屏要调系统 API，是平台适配器而非纯计算。
//
// 为什么不放 internal/adapters/wails：红线 R2 规定适配器之间禁止互引。
// Wails 服务层因此不能直接 import 本包，只能经由 ports.ScreenCapturer
// 接口，由组合根（internal/bootstrap）注入实现。
package screencap

import (
	"context"
	"errors"

	"github.com/swarmlink/swarmlink/internal/domain/ports"
)

// 截屏模式（keep_window / hide_window）与它们的归一规则放在
// ports 包（domain 层）—— 服务层也要用它们，而红线 R2 规定适配器之间
// 禁止互引，让服务层 import 本包只为拿两个常量是不必要的架构破坏。

// ErrUnsupported 表示当前平台没有截屏实现。
//
// 它是【可预期】的错误（不是 bug）：调用方应当把它翻译成一句人话
// （「当前平台暂不支持截屏」），而不是当成故障上报。
var ErrUnsupported = errors.New("screencap: 当前平台不支持截屏")

// Capturer 是 ports.ScreenCapturer 的实现。
//
// 它本身没有状态：所有平台相关的差异都落在各平台的 capture 函数里。
type Capturer struct{}

// New 构造 Capturer。
func New() *Capturer { return &Capturer{} }

// Capture 实现 ports.ScreenCapturer。
func (c *Capturer) Capture(ctx context.Context, excludeSelf bool) (ports.ScreenImage, error) {
	return capture(ctx, excludeSelf)
}

// Displays 实现 ports.ScreenCapturer。
func (c *Capturer) Displays() ([]ports.Display, error) {
	return displays()
}

// CaptureDisplay 实现 ports.ScreenCapturer。
func (c *Capturer) CaptureDisplay(ctx context.Context, index int, excludeSelf bool) (ports.ScreenImage, error) {
	return captureDisplay(ctx, index, excludeSelf)
}
