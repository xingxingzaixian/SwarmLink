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
	// roots 记录每个批次已分配的根目录（batchID → 绝对路径）。
	//
	// 必须按 batchID 而不是文件夹名：两个对端各发一个「照片」，它们的根
	// 必须是两个目录，否则两批文件会混进同一个文件夹。
	roots map[string]string
}

var _ ports.FileSink = (*Sink)(nil)

// New 创建文件接收器；dir 不存在时会创建。
func New(dir string) (*Sink, error) {
	if err := fsutil.EnsureDir(dir); err != nil {
		return nil, err
	}
	return &Sink{
		dir:      dir,
		reserved: make(map[string]bool),
		roots:    make(map[string]string),
	}, nil
}

// CreateIn 按相对路径把文件（或空目录）落到批次目录树里。
func (s *Sink) CreateIn(req ports.SinkRequest) (ports.SinkHandle, error) {
	var h ports.SinkHandle
	if req.BatchID == "" {
		// CreateIn 专门服务批次；单文件路径必须走 Create（行为已被测试锁定）。
		return h, fmt.Errorf("filesink: empty batch id")
	}
	segs, err := fsutil.SanitizeRelPath(req.RelPath)
	if err != nil {
		return h, fmt.Errorf("filesink: %w", err)
	}

	s.mu.Lock()
	root, err := s.rootLocked(req.BatchID, segs[0])
	if err != nil {
		s.mu.Unlock()
		return h, err
	}
	// 中间层：先算出父目录。逐段 SecureJoin，任何一段逃逸都会当场被拒。
	parent := root
	for _, seg := range segs[1 : len(segs)-1] {
		parent, err = fsutil.SecureJoin(parent, seg)
		if err != nil {
			s.mu.Unlock()
			return h, err
		}
	}
	last := segs[len(segs)-1]

	if req.IsDir {
		// 目录条目：只建目录，且【幂等】—— 同一个空目录重复到达、
		// 或先有文件后到目录条目，都是正常情况，不该改名成「空目录 (1)」。
		final, err := fsutil.SecureJoin(parent, last)
		s.mu.Unlock()
		if err != nil {
			return h, err
		}
		if err := fsutil.EnsureDir(final); err != nil {
			return h, fmt.Errorf("filesink: create dir: %w", err)
		}
		return ports.SinkHandle{JobID: req.JobID, FinalPath: final}, nil
	}

	final, err := s.uniqueLocked(parent, last)
	s.mu.Unlock()
	if err != nil {
		return h, err
	}
	if err := fsutil.EnsureDir(parent); err != nil {
		return h, fmt.Errorf("filesink: create parent: %w", err)
	}
	// 临时文件与最终文件【同目录】：跨目录 os.Rename 不保证原子（ADR-005）。
	tmp, err := fsutil.SecureJoin(parent, fsutil.TempPartName(req.JobID))
	if err != nil {
		return h, err
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return h, fmt.Errorf("filesink: create temp: %w", err)
	}
	_ = f.Close()

	return ports.SinkHandle{JobID: req.JobID, TempPath: tmp, FinalPath: final}, nil
}

// AdoptRoot 认领已分配过的批次根目录（重启恢复用）。
//
// 幂等且不校验存在性：路径可能还没建出来（重启后续传的第一个文件才建），
// CreateIn 会在需要时补建。
func (s *Sink) AdoptRoot(batchID, absPath string) {
	if batchID == "" || absPath == "" {
		return
	}
	p, err := filepath.Abs(absPath)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.roots == nil {
		s.roots = make(map[string]string)
	}
	s.roots[batchID] = p
}

// rootLocked 取得（必要时分配）批次的根目录；需要持有 s.mu。
//
// 分配走的是与单文件相同的去重逻辑，因此「照片」已存在时整批退到
// 「照片 (1)」—— 两份内容永不混合。
func (s *Sink) rootLocked(batchID, rootName string) (string, error) {
	if got, ok := s.roots[batchID]; ok {
		return got, nil
	}
	root, err := s.uniqueLocked(s.dir, rootName)
	if err != nil {
		return "", err
	}
	if err := fsutil.EnsureDir(root); err != nil {
		return "", fmt.Errorf("filesink: create batch root: %w", err)
	}
	s.roots[batchID] = root
	return root, nil
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
	final, err := s.uniqueLocked(s.dir, name)
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
//
// baseDir 由调用方给出（单文件是接收目录，批次里是父目录）：reserved 是按
// 【绝对路径】记的，因此跨目录共用一份表也是对的。
func (s *Sink) uniqueLocked(baseDir, name string) (string, error) {
	cand, err := fsutil.SecureJoin(baseDir, name)
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
		cand, err = fsutil.SecureJoin(baseDir, next)
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
