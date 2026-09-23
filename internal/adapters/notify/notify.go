// Package notify 提供与提示相关的平台能力：提示音播放 + 托盘图标素材。
//
// 它刻意不认识 Wails、也不认识事件桥 —— 两者都由组合根接线，
// 这样「什么时候提示」的决策（app.NotifyApp）与「怎么提示」的执行互不耦合。
package notify

import _ "embed"

//go:embed assets/notify.wav
var soundWav []byte

//go:embed assets/tray.png
var trayIcon []byte

//go:embed assets/tray-alert.png
var trayAlertIcon []byte

// Sound 返回内嵌提示音的 WAV 字节。
//
// 内嵌而不是随安装包放一个音频文件：提示音是行为的一部分，
// 不该因为用户移动/删除安装目录里的文件而静默消失。
func Sound() []byte { return soundWav }

// TrayIcon 返回托盘正常状态图标（PNG，32×32，Wails 会缩到系统小图标尺寸）。
//
// 这是「无文字版」：应用图标里的产品名在 16px 下只剩约 1.2px 高，纯噪声。
func TrayIcon() []byte { return trayIcon }

// TrayAlertIcon 返回托盘「有未读」图标（PNG，带红点角标）。
//
// 窗口收进托盘后没有任务栏按钮，「闪烁」只能靠在这两版之间交替切换。
func TrayAlertIcon() []byte { return trayAlertIcon }
