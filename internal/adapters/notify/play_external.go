//go:build darwin || linux

package notify

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"time"
)

// playTimeout 是单个播放器的存活上限。
//
// 正常情况下播放器自己会退出（片段只有 0.55s）；设上限是为了防「某个播放器
// 卡在等声卡 / 等 D-Bus」把后台 goroutine 永久挂住 —— 提示音绝不能变成
// 一个只增不减的 goroutine 泄漏。
const playTimeout = 10 * time.Second

var (
	playOnce sync.Once
	// playCand 是「探测过 PATH 之后真正可用的候选」，按优先级排列。
	playCand []soundPlayer
)

// Play 异步播放内嵌提示音。
//
// 与 Windows 实现（PlaySound 的 SND_ASYNC）语义一致：调用方立刻返回，
// 播放发生在后台；失败一律静默，绝不让提示音弹错误或阻塞调用方。
//
// 平台差异只剩「用哪些候选播放器」这一项（见 play_darwin.go / play_linux.go），
// 调度逻辑三平台之外共用这一份。
func Play() {
	if len(soundWav) == 0 {
		return
	}

	playOnce.Do(func() {
		// PATH 在进程生命周期内基本不变，只探测一次：消息连发时
		// 每次都扫一遍 PATH 是纯浪费。
		for _, c := range playCandidates {
			if _, err := exec.LookPath(c.name); err == nil {
				playCand = append(playCand, c)
			}
		}
	})
	if len(playCand) == 0 {
		return // 一个播放器都没有：静默，等同「这台机器上没配音频」
	}

	path, err := writeTempSound(soundWav)
	if err != nil {
		return
	}
	go playFile(playCand, path)
}

// playFile 依次尝试候选播放器，直到有一个【真的放出声】。
//
// 为什么要等退出码而不是只看 Start 是否成功：paplay 装上了但 PulseAudio
// 没起来、aplay 装上了但没有声卡，这两种情况 Start 都会成功、播放在第一
// 毫秒就失败 —— 只看 Start 就是「有提示音却永远听不到」，而日志里一片安静。
// 依次退让的代价只是多起一个子进程，换来的是在最小系统上也能出声。
func playFile(cands []soundPlayer, path string) {
	defer os.Remove(path)

	for _, c := range cands {
		ctx, cancel := context.WithTimeout(context.Background(), playTimeout)
		cmd := exec.CommandContext(ctx, c.name, c.args(path)...)
		err := cmd.Run()
		cancel()
		if err == nil {
			return
		}
	}
}
