// Package filesink 实现 ports.FileSink：接收侧落盘 + G3 清洗 + 原子替换。
package filesink

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/infra/fsutil"
)

// Sink 把文件写入指定目录。
type Sink struct {
	dir string

	mu sync.Mutex
	// reserved 记录已分配的最终路径，避免同一批并发任务抢到同一个名字。
	reserved map[string]bool
}

var _ ports.FileSink = (*Sink)(nil)

// New 创建文件接收器；dir 不存在时会创建。
func New(dir string) (*Sink, error) {
	if err := fsutil.EnsureDir(dir); err != nil {
		return nil, err
	}
	return &Sink{dir: dir, reserved: make(map[string]bool)}, nil
}

// Dir 返回接收目录。
func (s *Sink) Dir() string { return s.dir }

// Create 清洗文件名、分配不冲突的最终路径，并创建临时文件。
func (s *Sink) Create(jobID, rawName string) (ports.SinkHandle, error) {
	var h ports.SinkHandle

	name, err := fsutil.SanitizeFileName(rawName)
	if err != nil {
		return h, fmt.Errorf("filesink: %w", err)
	}

	s.mu.Lock()
	final, err := s.uniqueLocked(name)
	if err != nil {
		s.mu.Unlock()
		return h, err
	}
	s.mu.Unlock()

	tmp, err := fsutil.SecureJoin(s.dir, fsutil.TempPartName(jobID))
	if err != nil {
		return h, err
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return h, fmt.Errorf("filesink: create temp: %w", err)
	}
	_ = f.Close()

	return ports.SinkHandle{JobID: jobID, TempPath: tmp, FinalPath: final}, nil
}

// Commit 原子替换为最终文件。
func (s *Sink) Commit(h ports.SinkHandle) (string, error) {
	if h.TempPath == "" || h.FinalPath == "" {
		return "", fmt.Errorf("filesink: invalid sink handle")
	}
	if err := fsutil.Replace(h.TempPath, h.FinalPath); err != nil {
		return "", err
	}
	return h.FinalPath, nil
}

// Abort 删除临时文件（不存在时不报错）。
func (s *Sink) Abort(h ports.SinkHandle) error {
	if h.TempPath == "" {
		return nil
	}
	if err := os.Remove(h.TempPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// OpenTemp 以读写方式打开临时文件（接收端用 WriteAt 随机写入）。
func (s *Sink) OpenTemp(h ports.SinkHandle) (*os.File, error) {
	return os.OpenFile(h.TempPath, os.O_RDWR, 0o644)
}

// OpenForResume 打开已存在的临时文件用于续传；不存在时按需创建。
func (s *Sink) OpenForResume(tempPath string, create bool) (*os.File, error) {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	return os.OpenFile(tempPath, flags, 0o644)
}

// uniqueLocked 在已加锁状态下分配不冲突的最终路径。
func (s *Sink) uniqueLocked(name string) (string, error) {
	cand, err := fsutil.SecureJoin(s.dir, name)
	if err != nil {
		return "", err
	}
	if !s.reserved[cand] {
		if _, statErr := os.Stat(cand); os.IsNotExist(statErr) {
			s.reserved[cand] = true
			return cand, nil
		}
	}

	ext := filepath.Ext(name)
	stem := name[:len(name)-len(ext)]
	for i := 1; i < 10000; i++ {
		next := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		cand, err = fsutil.SecureJoin(s.dir, next)
		if err != nil {
			return "", err
		}
		if s.reserved[cand] {
			continue
		}
		if _, statErr := os.Stat(cand); os.IsNotExist(statErr) {
			s.reserved[cand] = true
			return cand, nil
		}
	}
	return "", fmt.Errorf("filesink: cannot find a unique name for %q", name)
}
