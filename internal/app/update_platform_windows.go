//go:build windows

package app

// updatePlatformSupported 报告当前平台是否支持局域网自动更新。
//
// 只有 Windows 为真：静默安装依赖 NSIS 的 /S、安装范围依赖
// INSTALL_SCOPE=user（装到 %LOCALAPPDATA%，无 UAC），而 macOS 的 .app 替换
// 需要代码签名、Linux 要区分 AppImage/deb 两套 —— 那两条链路的成本与风险
// 完全不同，不应该用同一个开关假装都支持。
func updatePlatformSupported() bool { return true }
