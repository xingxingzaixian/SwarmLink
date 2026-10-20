//go:build !windows

package app

// updatePlatformSupported 在非 Windows 平台返回 false：
// 设置页会显示「当前平台暂不支持」，后端也不会去检查更新或下载安装包。
//
// 仍然保留接收侧代码（HandleInfoReq 等）：一台 Linux 机器照样可以把
// 手上的安装包分发给 Windows 邻居 —— 但它自己不会发起更新。
func updatePlatformSupported() bool { return false }
