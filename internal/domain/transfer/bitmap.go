package transfer

import (
	"encoding/base64"
	"math/bits"
)

// ChunkBitmap 用位图记录分块完成状态。
// 1 GB 文件（512 KB 块）= 2048 位 = 256 字节；50 GB = 12.5 KB，可随时整体回传。
type ChunkBitmap struct {
	data []byte
}

// NewChunkBitmap 创建可容纳 n 个块的位图。
func NewChunkBitmap(n int) ChunkBitmap {
	if n <= 0 {
		return ChunkBitmap{}
	}
	return ChunkBitmap{data: make([]byte, (n+7)/8)}
}

// NewChunkBitmapFromBytes 由已持久化的字节构造（长度即容量）。
func NewChunkBitmapFromBytes(b []byte) ChunkBitmap {
	return ChunkBitmap{data: append([]byte(nil), b...)}
}

// Len 返回可寻址的位总数（容量）。
func (b ChunkBitmap) Len() int { return len(b.data) * 8 }

// Bytes 返回底层字节的副本。
func (b ChunkBitmap) Bytes() []byte { return append([]byte(nil), b.data...) }

// Set 置位第 i 块；越界为 no-op（防御性，避免协议异常导致 panic）。
func (b ChunkBitmap) Set(i int) {
	if i < 0 || i/8 >= len(b.data) {
		return
	}
	b.data[i/8] |= 1 << uint(i%8)
}

// Clear 清位第 i 块。
func (b ChunkBitmap) Clear(i int) {
	if i < 0 || i/8 >= len(b.data) {
		return
	}
	b.data[i/8] &^= 1 << uint(i%8)
}

// IsSet 判断第 i 块是否完成。
func (b ChunkBitmap) IsSet(i int) bool {
	if i < 0 || i/8 >= len(b.data) {
		return false
	}
	return b.data[i/8]&(1<<uint(i%8)) != 0
}

// CompletedCount 返回置位数量。
func (b ChunkBitmap) CompletedCount() int {
	n := 0
	for _, by := range b.data {
		n += bits.OnesCount8(by)
	}
	return n
}

// AllSet 判断前 total 块是否全部完成。
func (b ChunkBitmap) AllSet(total int) bool {
	for i := 0; i < total; i++ {
		if !b.IsSet(i) {
			return false
		}
	}
	return true
}

// Missing 返回 [0,total) 中未完成的块号（升序）。
func (b ChunkBitmap) Missing(total int) []int {
	out := make([]int, 0, total)
	for i := 0; i < total; i++ {
		if !b.IsSet(i) {
			out = append(out, i)
		}
	}
	return out
}

// Copy 深拷贝。
func (b ChunkBitmap) Copy() ChunkBitmap {
	return ChunkBitmap{data: append([]byte(nil), b.data...)}
}

// EnsureLen 保证位图容量至少覆盖 total 块（用于续传时按元数据重建）。
func (b *ChunkBitmap) EnsureLen(total int) {
	want := (total + 7) / 8
	if len(b.data) < want {
		grown := make([]byte, want)
		copy(grown, b.data)
		b.data = grown
	}
}

// MarshalBinary 返回裸字节。
func (b ChunkBitmap) MarshalBinary() ([]byte, error) { return b.Bytes(), nil }

// UnmarshalBinary 从裸字节恢复。
func (b *ChunkBitmap) UnmarshalBinary(d []byte) error {
	b.data = append([]byte(nil), d...)
	return nil
}

// EncodeBase64 输出 base64 形式（用于协议载荷与 DB TEXT 兼容路径）。
func (b ChunkBitmap) EncodeBase64() string { return base64.StdEncoding.EncodeToString(b.data) }

// DecodeBase64 解析 base64 位图。
func DecodeBase64(s string) (ChunkBitmap, error) {
	d, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ChunkBitmap{}, err
	}
	return ChunkBitmap{data: d}, nil
}
