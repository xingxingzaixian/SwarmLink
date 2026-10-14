// Package share 定义「文件共享」（FR-14）的领域模型：共享、文件条目、
// 可见性规则，以及随发现层传播的共享摘要。
//
// 位置说明：本包【零 I/O】（架构红线 R1），因此这里不做任何目录扫描 ——
// 枚举本地文件、把共享相对路径拼成绝对路径都由 app 层完成（那里还能用
// infra/fsutil 的路径清洗）。本包只负责三件纯逻辑：什么叫一条合法的共享、
// 谁能看见它、以及摘要怎么归一化。
package share

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// MaxItems 是一次共享允许的最大条目数。
//
// 上限不是洁癖：误选 C:\Users 会产生几十万条 item，而元数据要随每次
// ANNOUNCE / 目录拉取传播、按需还要整份发给下载方 —— 没有上限的话，
// 一次误操作就能把发现层拖垮（PRD RK-09）。
const MaxItems = 20000

// MaxNameLen 是共享名的长度上限（按 rune 计）。
const MaxNameLen = 64

// MaxAnnouncedShares 是随一次通告携带的共享摘要数量上限。
//
// 为什么必须有：摘要随 ANNOUNCE 广播、也随 PEER_LIST 逐条目传播，
// 不设上限时一个「每个子目录发布一次」的用法会让目录响应的体积
// 随节点数 × 共享数相乘增长。超出部分只能靠「对方主动来问」发现，
// 这在实现中不存在 —— 因此上限同时是「用户该怎么用这个功能」的约束，
// 界面在发布第 N+1 个共享时会提示（见 ShareApp.Publish）。
const MaxAnnouncedShares = 32

// VisibilityType 是共享可见性种类。
type VisibilityType string

const (
	// VisibilityAll 任何发现者都可见。
	VisibilityAll VisibilityType = "all"
	// VisibilityUsers 仅白名单内的节点可见。
	VisibilityUsers VisibilityType = "users"
	// VisibilityGroup 仅指定群的成员可见。
	VisibilityGroup VisibilityType = "group"
)

// Valid 报告取值是否在枚举范围内。
func (t VisibilityType) Valid() bool {
	switch t {
	case VisibilityAll, VisibilityUsers, VisibilityGroup:
		return true
	default:
		return false
	}
}

// DefaultVisibility 是新发布共享的默认可见性。
//
// 刻意【不是】all：安全默认值应当是「只有我点名的人能看见」，
// 误共享敏感文件的代价远大于「忘了加人所以别人看不到」（PRD RK-10）。
const DefaultVisibility = VisibilityUsers

// Visibility 是一条共享的可见性规则。
//
// Allowed 的语义随 Type 变化：
//   - users：允许访问者的 node_id（16 字符 hex）；
//   - group：允许访问的群 id（32 字符 hex，v1 只支持一个群）；
//   - all  ：忽略（发布时会归一化成空）。
type Visibility struct {
	Type VisibilityType `json:"type"`
	// Allowed 为空表示「还没指定任何人」—— 此时除 owner 自己外无人可见。
	Allowed []string `json:"allowed,omitempty"`
}

// Normalize 归一化：去重、去空、按字典序排序、all 清空白名单。
//
// 排序不是为了好看：可见性会被原样写进共享摘要并参与两端的比较，
// 顺序不定的切片会让「同一份配置」在两次通告里算出不同的字节，
// 从而让任何基于比较的缓存/去重逻辑失效。
func (v Visibility) Normalize() Visibility {
	if v.Type == "" {
		v.Type = DefaultVisibility
	}
	if v.Type == VisibilityAll {
		return Visibility{Type: VisibilityAll}
	}
	seen := make(map[string]struct{}, len(v.Allowed))
	out := make([]string, 0, len(v.Allowed))
	for _, id := range v.Allowed {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return Visibility{Type: v.Type, Allowed: out}
}

// Validate 校验可见性是否可发布。
func (v Visibility) Validate() error {
	if !v.Type.Valid() {
		return fmt.Errorf("share: 未知的可见性类型 %q", v.Type)
	}
	switch v.Type {
	case VisibilityUsers:
		if len(v.Allowed) == 0 {
			return fmt.Errorf("share: 可见性为「指定人」时必须至少选择一个人")
		}
	case VisibilityGroup:
		if len(v.Allowed) != 1 {
			return fmt.Errorf("share: 可见性为「仅群组」时必须且只能指定一个群")
		}
	}
	return nil
}

// Allows 判定某个请求者是否在可见范围内。
//
// groups 是请求者所属的群 id 列表（由调用方从群仓储查出后传入）——
// 本包零 I/O，因此不做任何查询。
//
// owner 永远可见：不可见自己的共享是荒谬的，而且会让「发布后列表里
// 找不到它」变成最难解释的一类现象。
func (v Visibility) Allows(requester string, groups []string, owner string) bool {
	requester = strings.ToLower(strings.TrimSpace(requester))
	if requester == "" {
		return false
	}
	if requester == strings.ToLower(strings.TrimSpace(owner)) {
		return true
	}
	switch v.Type {
	case VisibilityAll:
		return true
	case VisibilityUsers:
		return v.containsAll(requester)
	case VisibilityGroup:
		for _, g := range groups {
			if v.containsAll(g) {
				return true
			}
		}
		return false
	default:
		// 未知类型一律拒绝：可见性判定的失败方向必须是「不给看」。
		return false
	}
}

// containsAll 报告 id 是否在白名单里。
func (v Visibility) containsAll(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return false
	}
	for _, a := range v.Allowed {
		if strings.EqualFold(a, id) {
			return true
		}
	}
	return false
}

// String 返回人类可读的可见性（日志与排障用）。
func (v Visibility) String() string {
	switch v.Type {
	case VisibilityAll:
		return "全部"
	case VisibilityUsers:
		return fmt.Sprintf("指定人(%d)", len(v.Allowed))
	case VisibilityGroup:
		return "仅群组"
	default:
		return string(v.Type)
	}
}

// Item 是共享里的一个文件条目。
//
// 全部是【发布时 stat 出来的元数据】，不含内容；Hash 目前恒为空 ——
// 传输引擎在真要传的那一刻才逐块算哈希（见 TransferApp.sendFile），
// 发布时为一个 10GB 目录预先算哈希会让用户以为程序卡死，
// 而算出来的哈希在传输路径上一个字节也用不到。
type Item struct {
	// Path 是相对共享根的路径（斜杠分隔，不含共享根名）。
	Path string `json:"path"`
	// Name 是文件名（Path 的最后一段，冗余保存便于界面直接渲染）。
	Name string `json:"name"`
	// Size 是字节数。
	Size int64 `json:"size"`
	// Hash 是内容哈希（保留字段，当前恒为空，见类型注释）。
	Hash string `json:"hash,omitempty"`
	// MTime 是修改时间的 Unix 秒。
	MTime int64 `json:"mtime,omitempty"`
	// IsDir 表示这是一个【空目录】占位项（Size 恒为 0）。
	//
	// 为什么必须有：非空目录会因为它的文件被建出来，空目录不会。少了这一项，
	// 共享里的空目录在接收端会凭空消失 —— 而传输引擎的目录条目（IsDir）
	// 本来就是为这件事准备的，这里只是不让它在共享路径上丢失。
	IsDir bool `json:"is_dir,omitempty"`
}

// Share 是一条共享。
type Share struct {
	// ID 是共享标识（UUIDv7 风格字符串，全局唯一）。
	ID string
	// Name 是共享名（展示用，可与目录名不同）。
	Name string
	// Owner 是发布者 node_id（16 字符 hex）。
	Owner string
	// OwnerName 是发布者昵称（展示用；随摘要传播，可能滞后）。
	OwnerName string
	// RootPath 是本地根目录/文件的绝对路径。远端共享此字段为空。
	RootPath string
	// IsDir 表示根是目录（false = 单个文件）。
	IsDir bool
	// Visibility 是可见性规则。
	Visibility Visibility
	// Items 是文件清单（目录条目为文件；空目录会补一条 Size=0 的占位项）。
	Items []Item
	// TotalSize 是 Items 的字节数之和（冗余保存，界面不必每次都求和）。
	TotalSize int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Validate 校验一条共享是否可发布/可持久化。
func (s Share) Validate() error {
	if strings.TrimSpace(s.ID) == "" {
		return fmt.Errorf("share: 共享 id 不能为空")
	}
	name := strings.TrimSpace(s.Name)
	if name == "" {
		return fmt.Errorf("share: 共享名不能为空")
	}
	if len([]rune(name)) > MaxNameLen {
		return fmt.Errorf("share: 共享名不能超过 %d 个字符", MaxNameLen)
	}
	if strings.TrimSpace(s.Owner) == "" {
		return fmt.Errorf("share: 发布者不能为空")
	}
	if err := s.Visibility.Normalize().Validate(); err != nil {
		return err
	}
	if len(s.Items) > MaxItems {
		return fmt.Errorf("share: 共享条目过多（%d > %d），请选择更小的目录", len(s.Items), MaxItems)
	}
	for _, it := range s.Items {
		if _, err := CleanRelPath(it.Path); err != nil {
			return fmt.Errorf("share: 条目路径非法: %w", err)
		}
	}
	return nil
}

// ItemByPath 按相对路径查条目。
func (s Share) ItemByPath(p string) (Item, bool) {
	want, err := CleanRelPath(p)
	if err != nil {
		return Item{}, false
	}
	for _, it := range s.Items {
		if it.Path == want {
			return it, true
		}
	}
	return Item{}, false
}

// Summary 是随发现层传播的共享摘要。
//
// 只带「有多少、多大、谁能看」这类计数与标识，不带文件清单：
// 完整清单由下载方按需发 SHARE_INDEX_REQ 拉取（PRD RK-09）。
type Summary struct {
	ShareID    string     `json:"share_id"`
	Name       string     `json:"name"`
	OwnerID    string     `json:"owner_id"`
	OwnerName  string     `json:"owner_name,omitempty"`
	FileCount  int        `json:"file_count"`
	TotalSize  int64      `json:"total_size"`
	Visibility Visibility `json:"visibility"`
}

// Share 生成摘要。
func (s Share) Summary() Summary {
	return Summary{
		ShareID:    s.ID,
		Name:       s.Name,
		OwnerID:    s.Owner,
		OwnerName:  s.OwnerName,
		FileCount:  len(s.Items),
		TotalSize:  s.TotalSize,
		Visibility: s.Visibility.Normalize(),
	}
}

// VisibleTo 报告某请求者能否看见这条摘要。
func (s Summary) VisibleTo(requester string, groups []string) bool {
	return s.Visibility.Allows(requester, groups, s.OwnerID)
}

// NormalizeSummaries 归一化摘要列表：丢掉畸形项、排序、截断到上限。
//
// 为什么排序：摘要列表会参与 marshal，顺序不定的切片会让「内容没变」
// 的两次通告产生不同的字节流；调用方若按字节比较来判断「要不要通知
// 前端刷新」，就会每次通告都刷一次（45s 一轮的列表闪烁）。
func NormalizeSummaries(in []Summary) []Summary {
	out := make([]Summary, 0, len(in))
	for _, s := range in {
		s.Name = strings.TrimSpace(s.Name)
		s.ShareID = strings.TrimSpace(s.ShareID)
		s.OwnerID = strings.TrimSpace(s.OwnerID)
		s.Visibility = s.Visibility.Normalize()
		if s.ShareID == "" || s.Name == "" || s.OwnerID == "" {
			continue
		}
		if s.FileCount < 0 {
			s.FileCount = 0
		}
		if s.TotalSize < 0 {
			s.TotalSize = 0
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ShareID < out[j].ShareID })
	if len(out) > MaxAnnouncedShares {
		out = out[:MaxAnnouncedShares]
	}
	return out
}

// CleanRelPath 规范化并校验一条共享相对路径。
//
// 与 infra/fsutil.SanitizeRelPath 的分工：那个函数把路径切成「可以安全
// 落盘的名字段」（还管设备名、尾随点、名字长度），是接收端唯一的防线；
// 本函数只回答「这条路径作为共享内的相对路径是否成形」—— 领域层拿不到
// 也不该拿到那些平台细节。
//
// 两者的判定在这几条上必须一致（否则会出现「发布时通过、下载时被拒」）：
// 非空、不以分隔符开头、不含 `..` 与 `.` 段、不含空段、不含冒号（盘符）。
func CleanRelPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("share: 相对路径不能为空")
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return "", fmt.Errorf("share: 相对路径不能是绝对路径: %q", p)
	}
	segs := strings.Split(strings.ReplaceAll(p, `\`, "/"), "/")
	if len(segs) > 32 {
		return "", fmt.Errorf("share: 相对路径层数过多: %q", p)
	}
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("share: 相对路径含非法段 %q", seg)
		}
		if strings.ContainsRune(seg, ':') {
			return "", fmt.Errorf("share: 相对路径含盘符或冒号: %q", seg)
		}
	}
	return strings.Join(segs, "/"), nil
}

// JoinRelPath 把共享根名与条目路径拼成一条「批次内相对路径」。
//
// 传输引擎靠 RelPath 的首段还原目录树（接收端 filesink.CreateIn），
// 因此共享下载必须以【共享名】作为首段：不然同一个下载目录里两个
// 共享的同名文件会互相加 (1)，用户看到的是「文件混在了一起」。
func JoinRelPath(root, rel string) (string, error) {
	r := strings.TrimSpace(root)
	if r == "" {
		return "", fmt.Errorf("share: 共享根名不能为空")
	}
	clean, err := CleanRelPath(rel)
	if err != nil {
		return "", err
	}
	return r + "/" + clean, nil
}
