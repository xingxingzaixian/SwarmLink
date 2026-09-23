//go:build windows

package notify

import (
	"sync"
	"syscall"
	"unsafe"
)

var (
	winmm         = syscall.NewLazyDLL("winmm.dll")
	procPlaySound = winmm.NewProc("PlaySoundW")
)

const (
	sndAsync     = 0x0001
	sndNoDefault = 0x0002
	sndMemory    = 0x0004
)

// PlaySound 是进程级的：上一次播放会被下一次打断，
// 加锁串行化避免「连发两条时两声互相截断」听起来像爆音。
var playMu sync.Mutex

// Play 异步播放内嵌提示音。
//
// 失败一律静默：提示音是锦上添花，绝不能因为它弹错误或阻塞调用方。
// SND_MEMORY 要求缓冲区在播放期间保持有效 —— soundWav 是包级变量
// （go:embed 的只读数据），天然满足这一点，不需要额外的生命周期管理。
func Play() {
	if len(soundWav) == 0 {
		return
	}
	playMu.Lock()
	defer playMu.Unlock()
	procPlaySound.Call(
		uintptr(unsafe.Pointer(&soundWav[0])),
		0,
		uintptr(sndAsync|sndMemory|sndNoDefault),
	)
}
