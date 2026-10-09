//go:build wails && !windows

package main

import "log"

// showFatalDialog 在非 Windows 平台上退化为写日志。
//
// 那些平台通常有终端或系统日志可查，而为一次启动失败引入 GTK / Cocoa 的对话框
// 会把组合根拖进平台特定的依赖里 —— 与本文件存在的目的（把 Wails 接触面收敛到
// main_wails.go）相悖。
func showFatalDialog(title, msg string) {
	log.Printf("%s\n%s", title, msg)
}
