package app

import (
	"fmt"
	"path/filepath"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
)

// spaceRejectReason 预检目标盘剩余空间，返回拒收原因（空串=放行）。
//
// 只判「确定不够」的那些情况，其余一律放行：
//   - 目录条目零字节，没什么可占的；
//   - 没接探测函数（测试、或平台不支持）；
//   - 探测本身失败（网络盘、权限、路径不对）。
//
// 预检的职责是【提前止损】—— 让用户在传输开始前就得到一句人话，
// 而不是等传到 90% 才从系统拿到一句 "no space left on device"。
// 它不是给传输多加一个拒绝理由，所以判不了的时候绝不拦。
func (a *TransferApp) spaceRejectReason(meta protocol.FileMeta, peerID identity.NodeID) string {
	if meta.IsDir || meta.FileSize <= 0 || a.DiskFree == nil || a.sink == nil {
		return ""
	}

	need := meta.FileSize
	dir := a.sink.Root()
	// 续传：临时文件已经在目标盘上了，只补剩下的部分。把已收到的也算进去，
	// 会让「磁盘刚好够补完」这种最该成功的情况被自己误判成空间不足。
	if existing, ok := a.store.FindResumable(peerID, meta.FileID, meta.RelPath); ok && existing.TempPath != "" {
		need = meta.FileSize - existing.Completed
		dir = filepath.Dir(existing.TempPath)
	}
	if need <= 0 || dir == "" {
		return ""
	}

	free, err := a.DiskFree(dir)
	if err != nil {
		return ""
	}
	if free < uint64(need) {
		return fmt.Sprintf("接收目录所在磁盘空间不足（还需要 %s）", formatBytes(need))
	}
	return ""
}
