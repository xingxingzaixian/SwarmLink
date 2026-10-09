package fsutil

import (
	"fmt"
	"strings"
)

// MaxRelPathDepth 是相对路径允许的最大层数。
//
// 给一个上限不是洁癖：远端可以发来 1000 层嵌套的 rel_path，每一层都要
// 建目录、都要写进 DB —— 一条畸形的任务就能把磁盘布局拖垮。
const MaxRelPathDepth = 32

// SanitizeRelPath 把对端发来的相对路径切成【已清洗的段】。
//
// 与 SanitizeFileName 的分工：那个函数负责「一个名字」，本函数负责「一条路径」
// —— 逐段调用它，因此保留设备名（CON/NUL）、尾随点与空格、空字节、
// 名字长度等规则自动继承，不必在这里重写一遍。额外拦住的是路径层面的东西：
// 绝对路径、盘符、`..`、空段、层数上限。
//
// 返回值一定是「可以逐段 SecureJoin 的安全段列表」，调用方不需要再判断。
// 这是接收端的唯一防线（rel_path 完全由对端决定），因此拒绝集只多不少。
func SanitizeRelPath(rel string) ([]string, error) {
	if rel == "" {
		return nil, fmt.Errorf("%w: empty relative path", ErrBadName)
	}

	// 两种分隔符都当分隔符：对端可能是 Windows。
	// 刻意用 Split 而不是 FieldsFunc —— 后者会把连续分隔符悄悄折叠
	// （"a//b" 变成 a/b）。畸形输入应当被拒绝而不是被悄悄修正：
	// 悄悄修正意味着「对端发来的路径」与「我们落盘的路径」不再一一对应。
	segs := strings.Split(strings.ReplaceAll(rel, `\`, "/"), "/")
	if len(segs) > MaxRelPathDepth {
		return nil, fmt.Errorf("%w: depth %d exceeds %d", ErrBadName, len(segs), MaxRelPathDepth)
	}

	out := make([]string, 0, len(segs))
	for _, seg := range segs {
		// 空段（绝对路径开头的空串、双斜杠、尾随斜杠）与点段都在这里被拦下。
		if seg == "" || seg == "." || seg == ".." {
			return nil, fmt.Errorf("%w: bad path segment %q", ErrBadName, seg)
		}
		// 冒号是盘符（C:）的伪装成分，且 Windows 上文件名本就非法。
		// Linux 上冒号合法但极罕见 —— 跨平台一致性优先于保留它。
		if strings.ContainsRune(seg, ':') {
			return nil, fmt.Errorf("%w: colon or drive letter in segment %q", ErrBadName, seg)
		}
		clean, err := SanitizeFileName(seg)
		if err != nil {
			return nil, err
		}
		// 清洗后变空的兜底（例如整段都是空格/点）：空段落到 SecureJoin
		// 就等于往父目录上落，绝不允许。
		if clean == "" {
			return nil, fmt.Errorf("%w: segment %q becomes empty after sanitize", ErrBadName, seg)
		}
		out = append(out, clean)
	}
	return out, nil
}
