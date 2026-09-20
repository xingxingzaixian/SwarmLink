package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SecureJoin 把 name 拼到 dir 下，并校验结果仍在 dir 前缀内。
//
// 这一步是「清洗」之后的第二道闸：即使清洗规则未来被改动，
// 前缀校验也能兜住目录穿越。
func SecureJoin(dir, name string) (string, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("fsutil: abs dir: %w", err)
	}
	abs := filepath.Join(absDir, name)
	if abs != absDir && !strings.HasPrefix(abs, absDir+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrEscape, abs)
	}
	return abs, nil
}

// UniquePath 返回 dir 下不冲突的路径，同名时追加 " (1)"、" (2)"…（架构书 4.6）。
func UniquePath(dir, name string) (string, error) {
	first, err := SecureJoin(dir, name)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(first); os.IsNotExist(err) {
		return first, nil
	}

	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i < 10000; i++ {
		cand := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		p, err := SecureJoin(dir, cand)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p, nil
		}
	}
	return "", fmt.Errorf("fsutil: cannot find a unique name for %q", name)
}

// Replace 原子替换（同目录 rename；跨设备时会退化为复制+删除）。
func Replace(tmp, final string) error {
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("fsutil: replace %s -> %s: %w", tmp, final, err)
	}
	return nil
}

// EnsureDir 创建目录（0755）。
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("fsutil: mkdir %s: %w", dir, err)
	}
	return nil
}

// TempPartName 构造临时文件名：.swarmlink.<job_id>.part
//
// 用 job_id 命名而非 <filename>.tmp：天然免冲突、免路径注入。
func TempPartName(jobID string) string {
	return ".swarmlink." + SanitizeToken(jobID) + ".part"
}
