package protocol

import (
	"encoding/binary"
	"fmt"
)

// FileChunk 是文件块载荷。
//
// 采用二进制布局而非 JSON + base64：块数据动辄数百 KB，
// base64 会带来 33% 的额外带宽与一次完整拷贝。
//
// 布局：
//
//	[2B jobIDLen][jobID][4B index][8B offset][data...]
type FileChunk struct {
	JobID  string
	Index  int
	Offset int64
	Data   []byte
}

// EncodeFileChunk 序列化文件块。
func EncodeFileChunk(c FileChunk) ([]byte, error) {
	n := len(c.JobID)
	if n > 0xFFFF {
		return nil, fmt.Errorf("protocol: job id too long (%d)", n)
	}
	buf := make([]byte, 0, 2+n+4+8+len(c.Data))
	var tmp [8]byte
	binary.BigEndian.PutUint16(tmp[:2], uint16(n))
	buf = append(buf, tmp[:2]...)
	buf = append(buf, c.JobID...)
	binary.BigEndian.PutUint32(tmp[:4], uint32(c.Index))
	buf = append(buf, tmp[:4]...)
	binary.BigEndian.PutUint64(tmp[:], uint64(c.Offset))
	buf = append(buf, tmp[:]...)
	buf = append(buf, c.Data...)
	return buf, nil
}

// DecodeFileChunk 反序列化文件块。任何越界都返回错误（不 panic）。
func DecodeFileChunk(b []byte) (FileChunk, error) {
	var c FileChunk
	if len(b) < 2 {
		return c, fmt.Errorf("protocol: chunk too short for job id length")
	}
	n := int(binary.BigEndian.Uint16(b[:2]))
	if n > 0xFFFF || len(b) < 2+n+4+8 {
		return c, fmt.Errorf("protocol: chunk header truncated (jobIDLen=%d, len=%d)", n, len(b))
	}
	c.JobID = string(b[2 : 2+n])
	off := 2 + n
	c.Index = int(binary.BigEndian.Uint32(b[off : off+4]))
	c.Offset = int64(binary.BigEndian.Uint64(b[off+4 : off+12]))
	c.Data = append([]byte(nil), b[off+12:]...)
	return c, nil
}
