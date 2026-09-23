//go:build !windows

package notify

// Play 在非 Windows 平台是空实现：不响、不崩。
//
// 要做到真响需要各自平台的原生播放（macOS afplay / Linux paplay 或 GStreamer），
// 本次不做 —— 宁可不响，也不要引入一个半成品的音频栈。
// 注意别把这里的「空」当成占位符删掉：没有它，non-Windows 平台直接编译不过。
func Play() {}
