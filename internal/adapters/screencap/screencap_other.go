//go:build !windows && !darwin && !linux

package screencap

import (
	"context"
	"fmt"

	"github.com/swarmlink/swarmlink/internal/domain/ports"
)

// capture 在没有实现的平台上返回明确错误。
//
// 刻意不返回一张空图或黑图：空图会被前端当成「截到了但是全黑」，
// 用户会去查亮度、查显示器、查权限 —— 而真实原因是「这台系统上根本没做」。
// 一句「当前平台不支持截屏」能让人立刻知道该干什么。
func capture(_ context.Context, _ bool) (ports.ScreenImage, error) {
	return ports.ScreenImage{}, fmt.Errorf("%w：本功能目前覆盖 Windows、macOS 与 Linux(X11)", ErrUnsupported)
}

// displays 在没有实现的平台上返回明确错误（与 capture 同理）。
func displays() ([]ports.Display, error) {
	return nil, fmt.Errorf("%w：本功能目前覆盖 Windows、macOS 与 Linux(X11)", ErrUnsupported)
}

// captureDisplay 在没有实现的平台上返回明确错误（与 capture 同理）。
func captureDisplay(_ context.Context, _ int, _ bool) (ports.ScreenImage, error) {
	return ports.ScreenImage{}, fmt.Errorf("%w：本功能目前覆盖 Windows、macOS 与 Linux(X11)", ErrUnsupported)
}
