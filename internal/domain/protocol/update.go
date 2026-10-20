package protocol

import (
	"encoding/binary"
	"fmt"
)

// UpdateChunkSize 是安装包分块推送的单帧字节数。
//
// 256 KiB 是权衡后的值：一帧 16 MiB 上限（MaxFrameSize）下再放大没有收益，
// 而它的下界由「40 MB 的安装包要多少帧」决定 —— 160 帧在千兆内网里
// 连 1 秒都不到，进度事件也不至于把 WebView 打满。
const UpdateChunkSize = 256 << 10

// ---------------------------------------------------------------------------
// 更新包分发载荷（TCP）
// ---------------------------------------------------------------------------
//
// 这里刻意不复用 message/share 的「请求-响应」编号：更新是单机单流程，
// 全局同时只允许一个下载在跑，peer_id 就是天然的关联键
// （protocol.Frame 没有 msg_id 字段，造一个只会多一处两端对齐的负担）。

// UpdateInfoReq 询问对端：你手上有没有与你【当前运行版本】一致的安装包？
//
// Version 是请求方自己的版本，只用于对端日志（"1.0.0 来问包了"），
// 不参与任何判定 —— 该不该给，由对端的本地状态决定。
type UpdateInfoReq struct {
	Version string `json:"version"`
}

// UpdateInfoResp 是对 UpdateInfoReq 的回答。
//
// HasPkg 为 false 时其余字段都可以为空：对端没有缓存安装包、或缓存包的
// 版本与它自己运行的不一致（宁可不让别人升级，也不能发错包 —— 那会造成
// 「装完还是旧版 → 再拉一次」的死循环）。
type UpdateInfoResp struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Version string `json:"version,omitempty"`
	GOOS    string `json:"goos,omitempty"`
	GOARCH  string `json:"goarch,omitempty"`
	Size    int64  `json:"size,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	HasPkg  bool   `json:"hasPkg"`
}

// UpdatePkgReq 请求从 Offset 开始推安装包字节。
//
// 不支持断点续传（总是 0）：内网 40 MB 的包重传一次不过几秒，
// 而位图续传要引入一套与 transfer 重复的落盘状态。
type UpdatePkgReq struct {
	Offset int64 `json:"offset"`
}

// UpdatePkgDone 是分块推送结束标记，携带整包摘要供接收方核对。
type UpdatePkgDone struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// UpdateErr 是推送失败的原因（对端可见的唯一解释）。
type UpdateErr struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// updateChunkHeaderLen 是偏移量的字节数。
const updateChunkHeaderLen = 8

// UpdatePkgChunk 是安装包的一段字节。
//
// 用二进制布局而不是 JSON：40 MB 走 base64 会膨胀 33%，而这段载荷里
// 唯一的元数据就是一个偏移量（用于校验顺序与定位故障）。
type UpdatePkgChunk struct {
	Offset int64
	Data   []byte
}

// Encode 把分块序列化为帧载荷：Offset(8, BigEndian) | Data。
func (c UpdatePkgChunk) Encode() []byte {
	buf := make([]byte, updateChunkHeaderLen+len(c.Data))
	binary.BigEndian.PutUint64(buf[:updateChunkHeaderLen], uint64(c.Offset))
	copy(buf[updateChunkHeaderLen:], c.Data)
	return buf
}

// DecodeUpdatePkgChunk 解析分块载荷。
//
// 长度不足 8 字节即畸形：返回错误而不是 panic —— 这段字节来自网络。
func DecodeUpdatePkgChunk(b []byte) (UpdatePkgChunk, error) {
	if len(b) < updateChunkHeaderLen {
		return UpdatePkgChunk{}, fmt.Errorf("protocol: update chunk 太短（%d 字节）", len(b))
	}
	return UpdatePkgChunk{
		Offset: int64(binary.BigEndian.Uint64(b[:updateChunkHeaderLen])),
		Data:   b[updateChunkHeaderLen:],
	}, nil
}
