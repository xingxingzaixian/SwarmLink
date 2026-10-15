//go:build windows

package diskspace

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// freeBytes 用 GetDiskFreeSpaceEx 取卷上对【当前用户】可用的字节数。
//
// 取的是 freeBytesAvailableToCaller 而不是 totalNumberOfFreeBytes：后者是
// 全卷空闲量，在有磁盘配额或卷被别的用户占满时会把可写量算大，
// 据此放行的传输随后照样写失败。
func freeBytes(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, fmt.Errorf("diskspace: %w", err)
	}
	var availableToCaller, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &availableToCaller, &totalBytes, &totalFree); err != nil {
		return 0, fmt.Errorf("diskspace: %w", err)
	}
	return availableToCaller, nil
}
