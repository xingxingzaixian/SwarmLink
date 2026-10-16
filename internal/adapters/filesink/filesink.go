// Package filesink 实现 ports.FileSink：接收侧落盘 + G3 清洗 + 原子替换。
package filesink

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/infra/fsutil"
)

// batchRoot 是一个批次分配到的落盘根目录。
type batchRoot struct {
	// dir 是绝对路径（同名目录已存在时就是那个已存在的目录）。
	dir string
	// name 是发送端给出的首段名（未改名），只用于校验批次内首段是否一致；
	// 重启认领回来的根没有它（见 AdoptRoot），首次使用时会补上。
	name string
}

// Root 返回接收落点根目录（见 ports.FileSink.Root）。
func (s *Sink) Root() string { return s.dir }

// Sink 把文件写入指定目录。
type Sink struct {
	dir string

	mu sync.Mutex
	// reserved 记录【此刻正有任务在写】的最终路径。
	//
	// 语义是「在写」而不是「用过」：Commit/Abort 会把它清掉（见 release）。
	// 用处有二：一是同一批的并发任务不抢同一个名字；二是「同名文件就地覆盖」
	// 遇到并发的另一个写者时退到唯一名，避免两份内容互相覆盖。
	reserved map[string]bool
	// roots 记录每个批次分配到的根目录（batchID → 根目录）。
	//
	// 必须按 batchID 而不是文件夹名：两个批次各自只分配一次根，中途到达的
	// 文件才不会各算各的。文件夹名相同的两个批次会【复用同一个目录】（见
	// rootLocked）—— 那是刻意的：同名目录就是同一个目录。
	roots map[string]batchRoot
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
		roots:    make(map[string]batchRoot),
	}, nil
}

// CreateIn 按相对路径把文件（或空目录）落到批次目录树里。
func (s *Sink) CreateIn(req ports.SinkRequest) (ports.SinkHandle, error) {
	var h ports.SinkHandle
	if req.BatchID == "" {
		// CreateIn 专门服务批次；单文件路径必须走 Create（行为已被测试锁定）。
		return h, fmt.Errorf("%w: filesink", ports.ErrEmptyBatchID)
	}
	segs, err := fsutil.SanitizeRelPath(req.RelPath)
	if err != nil {
		return h, fmt.Errorf("filesink: %w", err)
	}

	// 路径的分配与占位全部在锁内完成，且只用 defer 解锁。
	// 这里存在会 panic 的切片运算（单段 rel_path），一次 panic 若跳过了手写
	// Unlock，整个 Sink 的锁就永久被占住 —— 之后每条接收任务都卡死，
	// 比直接崩溃更难查（实测 10 分钟超时）。
	alloc, err := s.allocLocked(req, segs)
	if err != nil {
		return h, err
	}

	if req.IsDir {
		if err := fsutil.EnsureDir(alloc.final); err != nil {
			return h, fmt.Errorf("filesink: create dir: %w", err)
		}
		return ports.SinkHandle{JobID: req.JobID, FinalPath: alloc.final}, nil
	}
	if err := fsutil.EnsureDir(alloc.parent); err != nil {
		return h, fmt.Errorf("filesink: create parent: %w", err)
	}
	// 临时文件与最终文件【同目录】：跨目录 os.Rename 不保证原子（ADR-005）。
	tmp, err := fsutil.SecureJoin(alloc.parent, fsutil.TempPartName(req.JobID))
	if err != nil {
		return h, err
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return h, fmt.Errorf("filesink: create temp: %w", err)
	}
	_ = f.Close()

	return ports.SinkHandle{JobID: req.JobID, TempPath: tmp, FinalPath: alloc.final}, nil
}

// sinkAlloc 是一次落盘路径的分配结果。
type sinkAlloc struct {
	parent string // 父目录（文件写在这里，目录条目也建在这里）
	final  string // 最终路径（文件是文件本身；目录条目是那个目录）
}

// allocLocked 算出并占住落盘路径；内部持锁，调用方不必持锁。
func (s *Sink) allocLocked(req ports.SinkRequest, segs []string) (sinkAlloc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 首段是「文件夹名」，只在分配根目录时参与；之后的条目沿用已分配的根。
	root, err := s.rootLocked(req.BatchID, segs[0])
	if err != nil {
		return sinkAlloc{}, err
	}

	// 单段 rel_path = 「文件夹本身」这一条：发送端在文件夹为空时，为根目录
	// 发一条 IsDir 条目（设计 3.3）。它的落点【只有 Sink 知道】—— 根目录
	// 可能已被改名为「空的 (1)」，接收端自己算不出来，所以必须在这里处理，
	// 不能推给调用方拼路径。
	if len(segs) == 1 {
		if !req.IsDir {
			return sinkAlloc{}, fmt.Errorf("filesink: 批次文件至少需要两段相对路径（根名/文件名），得到 %q", req.RelPath)
		}
		return sinkAlloc{parent: filepath.Dir(root), final: root}, nil
	}

	parent := root
	// 中间层：逐段 SecureJoin，任何一段逃逸都会当场被拒。
	for _, seg := range segs[1 : len(segs)-1] {
		parent, err = fsutil.SecureJoin(parent, seg)
		if err != nil {
			return sinkAlloc{}, err
		}
	}
	last := segs[len(segs)-1]

	if req.IsDir {
		// 目录条目：只建目录，且【幂等】—— 同一个空目录重复到达、
		// 或先有文件后到目录条目，都是正常情况，不该改名成「空目录 (1)」。
		final, err := fsutil.SecureJoin(parent, last)
		if err != nil {
			return sinkAlloc{}, err
		}
		return sinkAlloc{parent: parent, final: final}, nil
	}
	final, err := s.overwriteLocked(parent, last)
	if err != nil {
		return sinkAlloc{}, err
	}
	return sinkAlloc{parent: parent, final: final}, nil
}

// overwriteLocked 分配批次内文件的最终路径：同名同路径就地覆盖。
//
// 同名同路径意味着「同一份文件的再次到达」—— 重复下载、对方把同一个文件夹
// 再发一次、上一次传失败后的重试。加「 (1)」后缀会让同一份内容在磁盘上越堆
// 越多，且没有任何手段把它们合并回去（用户只能自己比对删除）。
//
// 唯一的例外是「这条路径此刻正有另一个任务在写」（reserved）：那是并发到达的
// 两条任务，让它们各写各的（第二条退到唯一名）比两份内容互相覆盖更安全。
// 占位在 Commit/Abort 时释放，因此「先后到达」的重复下载仍然覆盖原文件。
func (s *Sink) overwriteLocked(parent, name string) (string, error) {
	cand, err := fsutil.SecureJoin(parent, name)
	if err != nil {
		return "", err
	}
	if s.reserved[cand] {
		return s.uniqueLocked(parent, name)
	}
	s.reserved[cand] = true
	return cand, nil
}

// release 释放最终路径的占位（只在占用它的那次写入结束时调用）。
//
// 必须释放：占位的语义是「此刻有任务在写这条路径」，不是「这条路径曾被用过」。
// 不释放的话，同一进程内的第二次下载会看到自己上一次留下的占位，于是又退回
// 「c (1).txt」—— 「覆盖」就只在重启之后才成立，这种自相矛盾最难排查。
func (s *Sink) release(final string) {
	if final == "" {
		return
	}
	s.mu.Lock()
	delete(s.reserved, final)
	s.mu.Unlock()
}

// AdoptRoot 认领已分配过的批次根目录（重启恢复用）。
//
// 不校验存在性：路径可能还没建出来（重启后续传的第一个文件才建），
// CreateIn 会在需要时补建。
func (s *Sink) AdoptRoot(batchID, absPath string) {
	if batchID == "" || absPath == "" {
		return
	}
	p, err := filepath.Abs(absPath)
	if err != nil {
		return
	}
	base, err := filepath.Abs(s.dir)
	if err != nil {
		return
	}
	// 只认领接收目录【之内】的路径。
	//
	// 认领等于「把整批文件的落点交给调用方」，因此不能只靠「调用方会传对」：
	// 重启恢复时手里同时有发送侧与接收侧的任务，少写一行方向过滤就会把接收
	// 数据写进用户自己的源文件夹。越界认领直接忽略，后果退化成「没认领成功」
	// （重新分配一个根），而不是「写错地方」。
	if p != base && !strings.HasPrefix(p, base+string(filepath.Separator)) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roots[batchID] = batchRoot{dir: p}
}

// rootLocked 取得（必要时分配）批次的根目录；需要持有 s.mu。
//
// 同名目录【已存在就复用它】，而不是退到「照片 (1)」：
//
//	重复下载同一个共享、把同一个文件夹再发一次、上一次传失败后的重试 ——
//	这些场景里用户要的都是「这份内容就在这个目录里」，不是磁盘上堆出
//	照片 / 照片 (1) / 照片 (2)：那样既看不出哪份是新的，也没有任何合并手段。
//
// 代价是「同名同路径的文件会被覆盖」，而这正是同名同路径应有的语义
// （见 overwriteLocked）。
func (s *Sink) rootLocked(batchID, rootName string) (string, error) {
	if got, ok := s.roots[batchID]; ok {
		// 认领回来的根没记首段名（重启后第一次见到该批次）：以本次补上。
		// 之后再出现不同的首段名就要拒绝 —— 同一批次里所有 rel_path 的首段
		// 必须是同一个文件夹名，按已分配的根静默落盘会把两个文件夹合并。
		if got.name == "" {
			got.name = rootName
			s.roots[batchID] = got
			return got.dir, nil
		}
		if got.name != rootName {
			return "", fmt.Errorf("filesink: batch %s 的根名不一致（已分配 %q，本次 %q）",
				batchID, got.name, rootName)
		}
		return got.dir, nil
	}
	root, err := s.mergeOrUniqueDirLocked(s.dir, rootName)
	if err != nil {
		return "", err
	}
	// 根目录必须在锁内建出来：它与 roots 里的登记是一体的，分成两步会让
	// 并发到达的第二条任务拿到一个还不存在的根。
	if err := fsutil.EnsureDir(root); err != nil {
		return "", fmt.Errorf("filesink: create batch root: %w", err)
	}
	s.roots[batchID] = batchRoot{dir: root, name: rootName}
	return root, nil
}

// mergeOrUniqueDirLocked 分配一个「可复用」的目录路径：同名目录已存在就用它。
//
// 只对目录如此。同名【文件】挡在路上时无法合并（更不能把它删掉换上目录），
// 于是退到唯一名（照片 (1)），让这一批至少有地方可落。
func (s *Sink) mergeOrUniqueDirLocked(baseDir, name string) (string, error) {
	cand, err := fsutil.SecureJoin(baseDir, name)
	if err != nil {
		return "", err
	}
	st, statErr := os.Stat(cand)
	switch {
	case statErr == nil && st.IsDir():
		s.reserved[cand] = true
		return cand, nil
	case statErr == nil:
		return s.uniqueLocked(baseDir, name)
	case os.IsNotExist(statErr):
		// 还不存在：就用这个名字（调用方随后的 EnsureDir 会建出来），
		// 并占住它，免得同名的单文件传输把这条路径当成一个可用的文件名。
		s.reserved[cand] = true
		return cand, nil
	default:
		return "", fmt.Errorf("filesink: stat %q: %w", cand, statErr)
	}
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

// Commit 原子替换为最终文件（同名文件在这次替换里被覆盖）。
func (s *Sink) Commit(h ports.SinkHandle) (string, error) {
	if h.TempPath == "" || h.FinalPath == "" {
		return "", fmt.Errorf("filesink: invalid sink handle")
	}
	// 无论成败都要释放占位：失败的 Commit 同样意味着「这次写入结束了」。
	//
	// 只在成功路径释放是个不对称的漏洞：一旦替换失败（目标被占用），这条
	// 路径会在进程余下的生命周期里一直被视为「此刻有任务在写」，后续同路径
	// 的下载全部退化成「c (1).txt」——「同名同路径就地覆盖」这条语义就此失效，
	// 而且失效得毫无提示（用户只看到目录里多出一堆副本）。
	defer s.release(h.FinalPath)

	if err := fsutil.Replace(h.TempPath, h.FinalPath); err != nil {
		return "", err
	}
	return h.FinalPath, nil
}

// Abort 删除临时文件（不存在时不报错）。
func (s *Sink) Abort(h ports.SinkHandle) error {
	// 只有真正建过临时文件的句柄才占着最终路径。目录条目没有临时文件，
	// 它的 final 就是那个目录本身 —— 释放它会把批次根目录的占位一起撤掉。
	if h.TempPath != "" {
		defer s.release(h.FinalPath)
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
