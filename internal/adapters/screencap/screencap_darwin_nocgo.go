//go:build darwin && !cgo

package screencap

import (
	"context"
	"fmt"

	"github.com/swarmlink/swarmlink/internal/domain/ports"
)

// capture 在没有 cgo 的 macOS 构建上返回明确错误。
//
// macOS 的屏幕捕获必须调 CoreGraphics，而 CoreGraphics 没有纯 Go 绑定 ——
// 所以 darwin 目标必须带 cgo 编译。这条分支存在的意义是：
// 有人用 CGO_ENABLED=0 构建时，得到的不是「编译不过」这种莫名其妙的中断，
// 而是一句能指向问题的明确报错。
func capture(_ context.Context, _ bool) (ports.ScreenImage, error) {
	return ports.ScreenImage{}, fmt.Errorf("%w：macOS 构建未启用 cgo（CoreGraphics 无纯 Go 绑定）", ErrUnsupported)
}

// displays 在没有 cgo 的 macOS 构建上返回明确错误（与 capture 同理）。
func displays() ([]ports.Display, error) {
	return nil, fmt.Errorf("%w：macOS 构建未启用 cgo（CoreGraphics 无纯 Go 绑定）", ErrUnsupported)
}

// captureDisplay 在没有 cgo 的 macOS 构建上返回明确错误（与 capture 同理）。
func captureDisplay(_ context.Context, _ int, _ bool) (ports.ScreenImage, error) {
	return ports.ScreenImage{}, fmt.Errorf("%w：macOS 构建未启用 cgo（CoreGraphics 无纯 Go 绑定）", ErrUnsupported)
}
