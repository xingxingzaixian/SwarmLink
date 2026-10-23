//go:build linux

package notify

// playCandidates 是 Linux 上播放提示音的候选，按「桌面环境下最可能出声」排序：
//
//  1. paplay   —— PulseAudio；PipeWire 通过 pipewire-pulse 兼容层同样回应它，
//     因此这一条同时覆盖了多数 PipeWire 桌面；
//  2. pw-play  —— 纯 PipeWire 会话（没装 pulse 兼容层时）；
//  3. aplay    —— 只有 ALSA 的最小系统（无声音服务、直接独占声卡）。
//
// 三者都直接接受 WAV，不需要转码。找不到的候选会被过滤掉；全都失败就静默
// （见 play_external.go 的 playFile）。
var playCandidates = []soundPlayer{
	{
		name: "paplay",
		args: func(path string) []string { return []string{path} },
	},
	{
		name: "pw-play",
		args: func(path string) []string { return []string{path} },
	},
	{
		// -q：不往 stderr 打播放参数，避免无终端会话里刷日志
		name: "aplay",
		args: func(path string) []string { return []string{"-q", path} },
	},
}
