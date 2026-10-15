//go:build !windows

package diskspace

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// freeBytes 用 statfs 取卷上非特权用户可用的字节数。
//
// 用 Bavail 而不是 Bfree：Bfree 含 root 保留块，按它算会把可写量算大，
// 而这台机器上的普通用户根本写不进那部分空间。
func freeBytes(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("diskspace: %w", err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
