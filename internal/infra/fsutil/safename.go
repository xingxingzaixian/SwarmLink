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
// 规则（架构书 1.3，其中「拒绝」与「中和」的分工见下方第 2 步的顺序注释）：
//  1. 拒绝空、含 NUL、含 `\` 的输入
//  2. 取 filepath.Base 丢弃目录成分：`/` 与绝对路径被【中和】而不是拒绝
//  3. 拒绝 "."、".."、Windows 保留设备名与尾随点/空格
//  4. 按 UTF-8 边界截断到 255 字节
func SanitizeFileName(raw string) (string, error) {
	if raw == "" {
		return "", ErrEmptyName
	}
	if strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("%w: contains NUL byte", ErrBadName)
	}

	// 第 2 步必须在第 1 步【之前】：反斜杠一律按路径分隔符拒绝。
	//
	// 顺序不能颠倒的原因：filepath.Base 的语义随平台变 —— Windows 把 `\` 也当
	// 分隔符，于是 `..\..\Startup\evil.exe` 会被中和成 `evil.exe` 而通过；Unix 只认
	// `/`，同一份输入会保留反斜杠并在后面被拒。也就是【同一个对端报文，在 Windows
	// 接收端与 Unix 接收端得到不同裁决】，而路径清洗是接收侧唯一的安全边界、
	// 输入完全由对端控制（架构书 1.3）。因此这里统一按拒绝处理，与宿主平台无关。
	//
	// 这条顺序在 Windows 上曾经红过（TestSanitizeFileNameRejectsUnsafeNames）：
	// 当时的实现把它交给 Base 之后的分隔符检查，而那一步在 Windows 上已经看不见
	// 反斜杠了。本仓其他地方（fileBase、SanitizeRelPath）本来就是「两种分隔符都
	// 当分隔符」，这里只是与之一致。
	//
	// 不会误伤正常发送端：发送侧的 fileBase 与 filepath.Base 都会剥掉目录成分，
	// 因此线上 FileName 本就不含 `\`；rel_path 走 SanitizeRelPath，它在逐段调用
	// 本函数之前已经把 `\` 拆掉了。
	if strings.ContainsRune(raw, '\\') {
		return "", fmt.Errorf("%w: contains backslash", ErrBadName)
	}

	// 第 1 步：丢弃目录成分（"/" 被 Base 中和，绝对路径同理）。
	name := filepath.Base(raw)
	// 不变量：Base 之后不应再含任何分隔符 —— `\` 已在入口拒绝，`/` 已被 Base 拆掉。
	// 保留这一条作为断言，防止将来调整清洗顺序时静默失效。
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
