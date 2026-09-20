//go:build !wails

// 未启用 wails tag 时的占位入口。
//
// 作用：保证 `go build ./...` 与 `go test ./...` 在没有 Wails 工具链的环境下
// 依然可用（否则根目录只有带 build tag 的文件，Go 会报
// "build constraints exclude all Go files"）。
//
// 后端与 CLI 完全不受影响：
//
//	go test ./...                            全部测试
//	go run ./cmd/swarmlink-cli --name alice   命令行前端
//
// 构建桌面应用：
//
//	wails3 build -tags wails
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprint(os.Stderr, `这是 SwarmLink 的占位入口（未启用 wails tag）。

构建桌面 GUI：
  wails3 build -tags wails

只想用命令行版（功能完整，含 P-1/P-2 自检）：
  go run ./cmd/swarmlink-cli --name alice

跑测试：
  make ci
`)
	os.Exit(2)
}
