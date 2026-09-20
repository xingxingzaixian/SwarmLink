package protocol

import "errors"

// 协议错误。
var (
	ErrBadMagic      = errors.New("protocol: bad magic")
	ErrFrameTooLarge = errors.New("protocol: frame exceeds max size")
	ErrShortFrame    = errors.New("protocol: short frame")
	ErrBadVersion    = errors.New("protocol: unsupported version")
)

// Frame 是 TCP 协议帧。
type Frame struct {
	Flags   byte
	Type    byte
	Payload []byte
}

// New 构造一个无标志的帧。
func New(t byte, payload []byte) Frame { return Frame{Type: t, Payload: payload} }

// IsKnownFlag 判断标志位是否已知。
func IsKnownFlag(f byte) bool { return f&^FlagKnownMask == 0 }
