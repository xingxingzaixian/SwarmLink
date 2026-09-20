package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Encode 把帧序列化为字节切片。
func Encode(f Frame) ([]byte, error) {
	if len(f.Payload) > MaxFrameSize {
		return nil, fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, len(f.Payload), MaxFrameSize)
	}
	buf := make([]byte, HeaderSize+len(f.Payload))
	buf[0] = Magic0
	buf[1] = Magic1
	buf[2] = Version
	buf[3] = f.Flags
	buf[4] = f.Type
	binary.BigEndian.PutUint32(buf[5:9], uint32(len(f.Payload)))
	copy(buf[HeaderSize:], f.Payload)
	return buf, nil
}

// Write 把帧写入 w。返回写入的字节数。
func Write(w io.Writer, f Frame) (int, error) {
	buf, err := Encode(f)
	if err != nil {
		return 0, err
	}
	return w.Write(buf)
}

// Read 从 r 读取一个完整帧。
//
// 兼容规则（架构书 4.1）：
//  1. 必须先按 Length 读完载荷再判断能否处理，未知 Type 由调用方丢弃并计数，不得断连。
//  2. Length 超过 MaxFrameSize 即协议违规，返回 ErrFrameTooLarge（调用方应关连接）。
//  3. 未知 Flags 位不报错，原样带出。
func Read(r io.Reader) (Frame, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	if hdr[0] != Magic0 || hdr[1] != Magic1 {
		return Frame{}, ErrBadMagic
	}
	// Ver 不匹配不在帧层拒绝，交由 HELLO 阶段的版本协商提前解决；
	// 但仍需读完载荷以保持流对齐，因此这里只记录不报错。

	length := binary.BigEndian.Uint32(hdr[5:9])
	if length > MaxFrameSize {
		return Frame{}, fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, length, MaxFrameSize)
	}

	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return Frame{}, err
		}
	}
	return Frame{Flags: hdr[3], Type: hdr[4], Payload: payload}, nil
}

// PeekType 在只读到头部时用于日志/调试的辅助（不消费流）。
func PeekType(hdr []byte) (byte, bool) {
	if len(hdr) < HeaderSize {
		return 0, false
	}
	return hdr[4], true
}
