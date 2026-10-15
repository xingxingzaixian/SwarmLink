// Package diskspace 探测某个路径所在卷对当前用户可用的字节数。
//
// 单独一个包的原因：这件事只有平台调用能做，而平台调用会引入构建约束。
// 把它关在这里，应用层拿到的就是一个普通函数，测试里换成假实现即可 ——
// 「磁盘满」这种状态在真实环境里没法可靠复现，必须能注入。
package diskspace

import (
	"fmt"
	"os"
	"path/filepath"
)

// FreeBytes 返回 path 所在卷当前用户可用的字节数。
//
// path 可以是文件也可以是目录：调用方手里往往是文件路径，取目录这一步
// 由本函数自己做（每个平台调用都要求一个存在的路径）。
//
// path 不存在时返回错误，而不是 0 —— 调用方会把 0 读成「磁盘满了」，
// 于是每次传输都被拒，而真正的原因只是路径写错了。
func FreeBytes(path string) (uint64, error) {
	dir, err := existingDir(path)
	if err != nil {
		return 0, err
	}
	return freeBytes(dir)
}

// existingDir 把「可能是文件、可能不存在」的路径收敛成一个可探测的目录。
func existingDir(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("diskspace: 空路径")
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("diskspace: %w", err)
	}
	if st.IsDir() {
		return path, nil
	}
	return filepath.Dir(path), nil
}
