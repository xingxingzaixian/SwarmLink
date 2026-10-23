//go:build !windows && !darwin && !linux

package notify

// Play 在没有实现的平台上是空实现：不响、不崩。
//
// 目前的实现覆盖 Windows（winmm PlaySound）、macOS（afplay）、
// Linux（paplay / pw-play / aplay）。其余平台（FreeBSD 等）没有
// 「系统自带、无需额外依赖」的播放器，因此宁可不响，也不引入半成品的音频栈。
//
// 注意别把这里的「空」当成占位符删掉：没有它，这些平台直接编译不过。
func Play() {}
