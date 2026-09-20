// Package fsutil 提供安全文件名清洗与原子替换（架构书 G3 的全部规则）。
//
// 路径清洗【必须】收敛在这里：它是接收侧唯一的安全边界，
// 只有一个实现 → 只有一个测试点。
package fsutil

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// 清洗错误。
var (
	ErrEmptyName = errors.New("fsutil: empty file name")
	ErrBadName   = errors.New("fsutil: unsafe file name")
	ErrEscape    = errors.New("fsutil: path escapes target directory")
)

// maxNameBytes 是单文件名的字节上限（按 UTF-8 边界截断，不切坏多字节字符）。
const maxNameBytes = 255

var windowsReserved = map[string]bool{}

func init() {
	for _, n := range []string{"CON", "PRN", "AUX", "NUL"} {
		windowsReserved[n] = true
	}
	for i := 1; i <= 9; i++ {
		windowsReserved[fmt.Sprintf("COM%d", i)] = true
		windowsReserved[fmt.Sprintf("LPT%d", i)] = true
	}
}

// SanitizeFileName 清洗对端提供的文件名。
//
// 规则（架构书 1.3）：
//  1. 取 filepath.Base，丢弃所有目录成分
//  2. 拒绝空、"."、".."、含 "/" "\" 或 NUL 的输入
//  3. 拒绝 Windows 保留设备名与尾随点/空格
//  4. 按 UTF-8 边界截断到 255 字节
func SanitizeFileName(raw string) (string, error) {
	if raw == "" {
		return "", ErrEmptyName
	}
	if strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("%w: contains NUL byte", ErrBadName)
	}

	// 第 1 步：丢弃目录成分。
	// 注意：Unix 上 filepath.Base 只把 "/" 当分隔符，因此 Windows 风格路径
	// "..\\..\\evil.exe" 会原样返回，由第 2 步的 "\" 检查拦下。
	name := filepath.Base(raw)
	if strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("%w: contains path separator", ErrBadName)
	}
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("%w: %q", ErrBadName, name)
	}
	if isWindowsReserved(name) {
		return "", fmt.Errorf("%w: Windows reserved device name %q", ErrBadName, name)
	}

	// Windows 不允许文件名以点或空格结尾
	name = strings.TrimRight(name, " .")
	if name == "" {
		return "", fmt.Errorf("%w: only dots/spaces", ErrBadName)
	}

	name = truncateUTF8(name, maxNameBytes)
	if name == "" {
		return "", ErrBadName
	}
	return name, nil
}

func isWindowsReserved(name string) bool {
	stem := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		stem = name[:i]
	}
	return windowsReserved[strings.ToUpper(stem)]
}

func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// SanitizeToken 把任意字符串收敛为可用于文件名的安全 token（用于 job_id）。
func SanitizeToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
		if b.Len() >= 64 {
			break
		}
	}
	if b.Len() == 0 {
		return "job"
	}
	return b.String()
}
