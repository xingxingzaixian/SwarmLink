//go:build wails && windows

package main

import (
	"syscall"
	"unsafe"
)

// showFatalDialog 用原生消息框报告启动期致命错误。
//
// 为什么必须是原生对话框：产物以 -H windowsgui 链接（无控制台），窗口出现之前
// 的任何失败都没有地方可说，用户看到的就是「双击了没反应」。
//
// 为什么直接用 user32 的 MessageBoxW，而不是引第三方库或复用 Wails 的包：
// 这是【启动失败】路径，必须假设 Wails 自己已经不可用；只用标准库也避免为
// 一个对话框增加依赖。
func showFatalDialog(title, msg string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	messageBox := user32.NewProc("MessageBoxW")

	text, err := syscall.UTF16PtrFromString(msg)
	if err != nil {
		return
	}
	caption, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return
	}

	const (
		mbOK        = 0x00000000
		mbIconError = 0x00000010
		// 让对话框抢到前台：启动失败时进程还没有任何窗口，不加这一位
		// 对话框可能被压在别的窗口后面，于是「还是没反应」。
		mbSetForeground = 0x00010000
	)
	_, _, _ = messageBox.Call(
		0,
		uintptr(unsafe.Pointer(text)),
		uintptr(unsafe.Pointer(caption)),
		uintptr(mbOK|mbIconError|mbSetForeground),
	)
}
