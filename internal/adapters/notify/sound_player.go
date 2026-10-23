package notify

import "os"

// soundPlayer 描述「用哪个外部命令播放提示音」。
//
// 为什么走外部命令而不是原生 API：macOS 要接 AudioToolbox，Linux 要接
// PulseAudio / PipeWire / ALSA，三条路分别需要 cgo 或一整套音频栈依赖；
// 而本仓是纯 Go 构建（screencap 选 BurntSushi/xgb 而不是 go-xlib 也是同一个
// 理由）。afplay / paplay / aplay 是 macOS 与各发行版自带的，代价只是一个子进程。
//
// 【本文件刻意不带构建标签】它没有任何平台相关代码，却要能被所有平台的测试
// 复用 —— 「找不到播放器就静默不响」正是「有提示音但从来听不到」这种最难
// 被发现的失败方式，必须能在开发机（Windows）上被测到。
type soundPlayer struct {
	// name 是命令名，按 PATH 查找。
	name string
	// args 生成命令行参数；path 是临时音频文件的绝对路径。
	args func(path string) []string
}

// pickPlayer 按顺序返回第一个能在 PATH 里找到的播放器。
//
// look 注入 exec.LookPath 而不是直接调用它，只为一个理由：让「一个都找不到
// 时返回 false」这条分支能被测试钉住（调用方据此静默，不报错、不重试）。
func pickPlayer(cands []soundPlayer, look func(string) (string, error)) (soundPlayer, bool) {
	for _, c := range cands {
		if _, err := look(c.name); err == nil {
			return c, true
		}
	}
	return soundPlayer{}, false
}

// writeTempSound 把内嵌音频写到临时文件并返回路径；调用方负责删除。
//
// Windows 的 PlaySound 能直接吃内存缓冲（SND_MEMORY），macOS/Linux 的播放器
// 只接受文件路径。48KB 的顺序写盘代价可以忽略（提示音本身要响 0.55s），
// 换来的是不必维护「一份常驻临时文件」的全局状态与它的清理时机。
func writeTempSound(data []byte) (string, error) {
	f, err := os.CreateTemp("", "swarmlink-notify-*.wav")
	if err != nil {
		return "", err
	}
	path := f.Name()

	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}
