//go:build !windows

package selfupdate

import "fmt"

// Launch 在非 Windows 平台不可用。
//
// 不是「还没写」而是「不该做」：macOS 的 .app 替换必须重新签名（否则下次
// 启动会被 Gatekeeper 拦住），Linux 的 AppImage / deb 是两条完全不同的
// 安装链路。把它们塞进一个统一入口，只会得到一个看起来很通用、
// 实际上每个分支都不可靠的壳。
func Launch(installer string) error {
	_ = installer
	return fmt.Errorf("selfupdate: 当前平台暂不支持自动安装")
}
