//go:build !wails

// 本文件保证在没有 Wails 工具链时 cmd/swarmlink-gui 仍可编译
// （否则该目录只有带 build tag 的文件，go build ./... 会报
// "build constraints exclude all Go files"）。
//
// 构建 GUI：
//
//	go get github.com/wailsapp/wails/v3@v3.0.0-beta.23
//	wails3 build -tags wails
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprint(os.Stderr, `swarmlink-gui 需要 Wails 工具链才能构建：

  1) 安装 wails3 CLI（见 Wails v3 官方文档）
  2) go get github.com/wailsapp/wails/v3@v3.0.0-beta.23
  3) wails3 build -tags wails

当前未启用 wails tag，因此这是占位入口。
后端核心（含全部服务与事件桥）不受影响：
  go build ./...            构建后端
  go test ./...             跑全部测试
  go run ./cmd/swarmlink-cli --name alice   # 命令行前端

`)
	os.Exit(2)
}
