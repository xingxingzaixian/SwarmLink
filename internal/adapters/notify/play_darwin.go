//go:build darwin

package notify

// playCandidates 是 macOS 上播放提示音的候选。
//
// afplay 是系统自带的（/usr/bin/afplay），从 Finder 启动的 app 也走得到；
// 它会用默认输出设备播放，并且接受 WAV，因此不需要转码。
//
// 为什么不走 NSSound / AudioToolbox：那需要 cgo 与 Cocoa 框架，而本包是纯 Go
// 且明确不认识 Wails（红线 R2）。一次提示音换一个子进程，代价可以忽略。
var playCandidates = []soundPlayer{
	{
		name: "afplay",
		args: func(path string) []string { return []string{path} },
	},
}
