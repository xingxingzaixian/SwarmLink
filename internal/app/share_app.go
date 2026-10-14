package app

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/domain/share"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
	"github.com/swarmlink/swarmlink/internal/infra/fsutil"
)

// 共享默认参数（可在构造后覆盖，与 TransferApp.MaxConcurrent 同一做法）。
const (
	defaultIndexTimeout = 10 * time.Second
	defaultPullTimeout  = 15 * time.Second
	// defaultMaxPullPaths 是一次拉取请求允许的文件数上限。
	//
	// 上限是必须的：一个请求带上整个清单（几万条）会让共享方在一瞬间
	// 建出几万条传输任务，传输队列与数据库都会先崩 —— 而用户的意图
	// 多半只是「全选下载」，分批请求完全够用（界面按 200 一批切分）。
	defaultMaxPullPaths = 200
)

// ShareApp 编排文件共享（FR-14）：发布本地目录 → 元数据随发现层传播 →
// 他人浏览清单 → P2P 直连拉取（复用传输引擎与断点续传）。
//
// 与其它 App 的分工：
//   - 发现层（PeerApp / DiscoveryApp）负责把【摘要】带到每个人的目录里；
//   - TransferApp 负责真正的字节搬运，本 App 只决定「传哪些、往哪儿放」；
//   - 本 App 的独有职责是【可见性校验】—— 一切访问控制都在这里，
//     因为这里是唯一同时知道「谁在请求」和「这条共享允许谁看」的地方。
type ShareApp struct {
	self identity.NodeID
	repo ports.ShareRepo
	dir  ports.PeerDirectory
	// groups 用于「仅群组」可见性的成员判定（可为 nil：那样群共享对谁都不可见）。
	groups   ports.GroupRepo
	transfer *TransferApp
	bus      ports.EventBus
	clk      ports.Clock
	lg       *slog.Logger

	// SelfName 返回本机昵称（发布时写进共享摘要，供他人列表展示）。
	// 为 nil 时退回「共享落库时记录的昵称」。
	SelfName func() string

	// Announce 让组合根注入「立刻广播一次摘要」的钩子（可为 nil）。
	//
	// 为什么需要它：广播周期是 45 秒，而「发布完别人 45 秒后才看见」
	// 与 FR-14 的验收（元数据传播后 2 秒内出现）差得太远。没有这个钩子，
	// 用户会以为发布失败了，然后反复点。
	Announce func()

	// MaxItems 是单条共享的条目上限（0 = 用 share.MaxItems）。
	MaxItems int
	// MaxPullPaths 是单次拉取请求允许的文件数上限（0 = 默认 200）。
	MaxPullPaths int
	// IndexTimeout / PullTimeout 是等待对端响应的超时。
	IndexTimeout time.Duration
	PullTimeout  time.Duration

	// ctx 是长生命周期上下文：共享方受理拉取请求后，发送是【异步】的，
	// 若用请求自身的 ctx，请求一返回传输就被取消了。
	ctx context.Context

	mu sync.Mutex
	// indexWait 是按 share_id 关联的「等清单响应」通道。
	//
	// 关联键必须是 share_id 而不是某个请求序号：protocol.Frame 没有
	// msg_id 字段，而 SHARE_INDEX_RESP 天然带 share_id —— 造一个序号
	// 只会多一个需要两端对齐的字段。
	indexWait map[string]chan protocol.ShareIndexResp
	// pullWait 按 batch_id 关联「等受理结果」。batch_id 由请求方生成、
	// 全局唯一，且共享方会原样回填，是这里唯一稳定的关联键。
	pullWait map[string]chan protocol.SharePullResp

	// local 是本地共享摘要缓存：ANNOUNCE 构造在广播循环里、每个网卡
	// 都要取一次，不该每次都去查一次数据库。
	local []share.Summary
}

// NewShareApp 构造共享应用。
func NewShareApp(
	self identity.NodeID,
	repo ports.ShareRepo,
	dir ports.PeerDirectory,
	groups ports.GroupRepo,
	transfer *TransferApp,
	bus ports.EventBus,
	clk ports.Clock,
	lg *slog.Logger,
) *ShareApp {
	return &ShareApp{
		self:         self,
		repo:         repo,
		dir:          dir,
		groups:       groups,
		transfer:     transfer,
		bus:          bus,
		clk:          clk,
		lg:           lg,
		ctx:          context.Background(),
		MaxItems:     share.MaxItems,
		MaxPullPaths: defaultMaxPullPaths,
		IndexTimeout: defaultIndexTimeout,
		PullTimeout:  defaultPullTimeout,
		indexWait:    make(map[string]chan protocol.ShareIndexResp),
		pullWait:     make(map[string]chan protocol.SharePullResp),
	}
}

// Start 记录长生命周期上下文并装载本地共享摘要缓存。
func (a *ShareApp) Start(ctx context.Context) {
	if ctx != nil {
		a.ctx = ctx
	}
	a.reloadLocal()
}

// ---------------------------------------------------------------------------
// 发布 / 取消 / 刷新
// ---------------------------------------------------------------------------

// PublishRequest 是发布共享的入参。
type PublishRequest struct {
	// RootPath 是要发布的本地目录或文件（绝对路径）。
	RootPath string
	// Name 是共享名；为空时取目录/文件名。
	Name string
	// Visibility 是可见性规则（零值回落成「指定人」，安全默认值）。
	Visibility share.Visibility
}

// Publish 发布一条共享：扫描根目录、生成清单、落库，并立刻广播一次摘要。
//
// 扫描只 stat 不哈希：为一个 10GB 目录预先算哈希会让用户以为程序卡死，
// 而算出来的哈希在传输路径上一个字节也用不到（那边按块现算）。
func (a *ShareApp) Publish(ctx context.Context, req PublishRequest) (share.Share, error) {
	root := strings.TrimSpace(req.RootPath)
	if root == "" {
		return share.Share{}, fmt.Errorf("share: 请选择要发布的目录或文件")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return share.Share{}, fmt.Errorf("share: 无效路径: %w", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return share.Share{}, fmt.Errorf("share: 无法访问 %s: %w", abs, err)
	}

	existing := a.repo.List()
	for _, s := range existing {
		if filepath.Clean(s.RootPath) == filepath.Clean(abs) {
			return share.Share{}, fmt.Errorf("share: %s 已经发布过了（共享「%s」）", abs, s.Name)
		}
	}
	if len(existing) >= share.MaxAnnouncedShares {
		return share.Share{}, fmt.Errorf(
			"share: 最多同时发布 %d 个共享，请先取消一些", share.MaxAnnouncedShares)
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = filepath.Base(abs)
	}

	items, total, err := a.scan(abs, st.IsDir())
	if err != nil {
		return share.Share{}, err
	}

	now := a.clk.Now()
	sh := share.Share{
		ID:         message.NewID(),
		Name:       name,
		Owner:      a.self.String(),
		OwnerName:  a.selfName(),
		RootPath:   abs,
		IsDir:      st.IsDir(),
		Visibility: req.Visibility.Normalize(),
		Items:      items,
		TotalSize:  total,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := sh.Validate(); err != nil {
		return share.Share{}, err
	}
	if err := a.repo.Upsert(sh); err != nil {
		return share.Share{}, err
	}
	a.afterChange(sh.ID, "published")
	if a.lg != nil {
		a.lg.Info("share: 已发布共享",
			"share_id", sh.ID, "name", sh.Name, "root", abs,
			"items", len(sh.Items), "visibility", sh.Visibility.String())
	}
	return sh, nil
}

// RefreshIndex 重新扫描根目录并整份替换清单。
//
// 存在的理由：共享的内容会随磁盘变化（新增/删除文件），而摘要是发布那一刻
// 的快照。没有刷新入口的话，用户只能「取消再发布」，那会连带换掉 share_id
// —— 别人列表里的条目会闪一下，且历史下载的批次对不上。
func (a *ShareApp) RefreshIndex(ctx context.Context, shareID string) (share.Share, error) {
	sh, ok := a.repo.Get(shareID)
	if !ok {
		return share.Share{}, fmt.Errorf("share: %w", ports.ErrShareNotFound)
	}
	st, err := os.Stat(sh.RootPath)
	if err != nil {
		return share.Share{}, fmt.Errorf("share: 共享根目录已不可访问（%s）: %w", sh.RootPath, err)
	}
	items, total, err := a.scan(sh.RootPath, st.IsDir())
	if err != nil {
		return share.Share{}, err
	}
	sh.Items = items
	sh.TotalSize = total
	sh.UpdatedAt = a.clk.Now()
	if err := sh.Validate(); err != nil {
		return share.Share{}, err
	}
	if err := a.repo.Upsert(sh); err != nil {
		return share.Share{}, err
	}
	a.afterChange(sh.ID, "refreshed")
	return sh, nil
}

// SetVisibility 修改一条本机共享的可见性。
//
// 存在的理由：可见性此前只能在发布时定死，想改只能「取消再发布」—— 那会换掉
// share_id：别人列表里的条目要闪一下，历史下载的批次也对不上号。
//
// 必须重新广播：可见性写在摘要里，对端靠它过滤；不广播的话「改成指定人」
// 在对方列表里要等到下一轮周期通告（最长 45s）才生效，用户会以为没生效。
func (a *ShareApp) SetVisibility(shareID string, vis share.Visibility) (share.Share, error) {
	sh, ok := a.repo.Get(shareID)
	if !ok {
		return share.Share{}, fmt.Errorf("share: %w", ports.ErrShareNotFound)
	}
	next := vis.Normalize()
	if err := next.Validate(); err != nil {
		return share.Share{}, err
	}
	sh.Visibility = next
	sh.UpdatedAt = a.clk.Now()
	if err := sh.Validate(); err != nil {
		return share.Share{}, err
	}
	if err := a.repo.Upsert(sh); err != nil {
		return share.Share{}, err
	}
	a.afterChange(sh.ID, "visibility")
	if a.lg != nil {
		a.lg.Info("share: 已修改可见性",
			"share_id", sh.ID, "name", sh.Name, "visibility", next.String())
	}
	return sh, nil
}

// Unpublish 取消发布（幂等）。
func (a *ShareApp) Unpublish(shareID string) error {
	if _, ok := a.repo.Get(shareID); !ok {
		return nil
	}
	if err := a.repo.Delete(shareID); err != nil {
		return err
	}
	a.afterChange(shareID, "unpublished")
	if a.lg != nil {
		a.lg.Info("share: 已取消共享", "share_id", shareID)
	}
	return nil
}

// Mine 返回本机发布的全部共享（含清单）。
func (a *ShareApp) Mine() []share.Share { return a.repo.List() }

// Summaries 返回本地共享摘要（供 UDP 广播与目录交换携带）。
//
// 返回副本：调用方（广播循环）会在自己的 goroutine 里使用它，
// 直接交出内部切片等于把缓存暴露给一个不持锁的读者。
func (a *ShareApp) Summaries() []share.Summary {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]share.Summary, len(a.local))
	copy(out, a.local)
	return out
}

// reloadLocal 重建本地摘要缓存。
func (a *ShareApp) reloadLocal() {
	name := a.selfName()
	list := a.repo.List()
	out := make([]share.Summary, 0, len(list))
	for _, s := range list {
		sum := s.Summary()
		if name != "" {
			sum.OwnerName = name
		}
		out = append(out, sum)
	}
	out = share.NormalizeSummaries(out)

	a.mu.Lock()
	a.local = out
	a.mu.Unlock()
}

// afterChange 在共享清单变化后统一做三件事：刷缓存、广播、通知前端。
//
// 收在一处而不是每个入口各写一遍：漏掉广播会让「发布完别人看不到」，
// 漏掉事件会让「自己界面上没有」—— 两者都是最难解释的那类现象。
func (a *ShareApp) afterChange(shareID, action string) {
	a.reloadLocal()
	if a.Announce != nil {
		a.Announce()
	}
	if a.bus != nil {
		a.bus.Publish(eventbus.TopicShareUpdated, eventbus.ShareUpdated{
			ShareID: shareID, Action: action,
		})
	}
}

// selfName 取本机昵称（未注入时返回空串，由调用方回落）。
func (a *ShareApp) selfName() string {
	if a.SelfName == nil {
		return ""
	}
	return a.SelfName()
}

// ---------------------------------------------------------------------------
// 清单扫描
// ---------------------------------------------------------------------------

// scan 枚举共享根，产出条目清单与总字节数。
//
// 与 TransferApp.walkFolder 的三条取舍完全一致，且理由相同：
//   - 不跟随符号链接（会引入循环与越权读取，跨平台语义也不一致）；
//   - 只 stat 不读内容；
//   - 空目录也要成为一条：非空目录会因它的文件被建出来，空目录不会。
func (a *ShareApp) scan(root string, isDir bool) ([]share.Item, int64, error) {
	limit := a.maxItems()

	if !isDir {
		st, err := os.Stat(root)
		if err != nil {
			return nil, 0, fmt.Errorf("share: %w", err)
		}
		name := filepath.Base(root)
		return []share.Item{{Path: name, Name: name, Size: st.Size(), MTime: st.ModTime().Unix()}},
			st.Size(), nil
	}

	items := make([]share.Item, 0, 32)
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			// 链接不跟随也不上报：它与「要共享的内容」无关，
			// 而列出来会让界面出现一条永远下不动的条目。
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		clean, err := share.CleanRelPath(rel)
		if err != nil {
			return fmt.Errorf("share: 文件 %s 的路径无法共享: %w", path, err)
		}

		if d.IsDir() {
			empty, err := isEmptyDir(path)
			if err != nil {
				return err
			}
			if empty {
				items = append(items, share.Item{
					Path: clean, Name: filepath.Base(path), IsDir: true,
				})
			}
		} else {
			info, err := d.Info()
			if err != nil {
				return err
			}
			items = append(items, share.Item{
				Path:  clean,
				Name:  info.Name(),
				Size:  info.Size(),
				MTime: info.ModTime().Unix(),
			})
			total += info.Size()
		}
		if len(items) > limit {
			return errTooManyEntries
		}
		return nil
	})
	if err != nil {
		if err == errTooManyEntries {
			return nil, 0, fmt.Errorf("share: 共享条目超过 %d 个，已取消（请选择更小的目录）", limit)
		}
		return nil, 0, fmt.Errorf("share: 枚举 %s: %w", root, err)
	}

	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	return items, total, nil
}

// maxItems 返回单条共享的条目上限。
func (a *ShareApp) maxItems() int {
	if a.MaxItems > 0 {
		return a.MaxItems
	}
	return share.MaxItems
}

// maxPullPaths 返回单次拉取请求的文件数上限。
func (a *ShareApp) maxPullPaths() int {
	if a.MaxPullPaths > 0 {
		return a.MaxPullPaths
	}
	return defaultMaxPullPaths
}

func (a *ShareApp) indexTimeout() time.Duration {
	if a.IndexTimeout > 0 {
		return a.IndexTimeout
	}
	return defaultIndexTimeout
}

func (a *ShareApp) pullTimeout() time.Duration {
	if a.PullTimeout > 0 {
		return a.PullTimeout
	}
	return defaultPullTimeout
}

// ---------------------------------------------------------------------------
// 他人共享（来自节点目录）
// ---------------------------------------------------------------------------

// RemoteShare 是「别人发布且对我可见」的一条共享。
type RemoteShare struct {
	share.Summary
	// Online 表示发布者此刻在目录里是 online / discovered 且有可拨号地址。
	//
	// 这个字段必须由后端算好交给界面：目录里的 state 与 LastAddr 是两个
	// 字段的共同结论，让前端自己组合 = 把一条判定规则复制到第二个地方。
	Online bool
}

// Remote 列出他人共享，按「发布者在线 → 名称」排序。
//
// 遍历节点目录而不是单开一张远端共享表：摘要本来就随 ANNOUNCE 与目录交换
// 落在目录条目上（见 peer.Peer.Shares），再造一张表就要再造一条同步路径。
func (a *ShareApp) Remote() []RemoteShare {
	if a.dir == nil {
		return nil
	}
	groups := a.myGroupIDs()
	peers := a.dir.List(ports.PeerFilter{})
	out := make([]RemoteShare, 0, len(peers))
	for _, p := range peers {
		if len(p.Shares) == 0 || p.NodeID == a.self {
			continue
		}
		online := (p.State == peer.StateOnline || p.State == peer.StateDiscovered) && p.LastAddr != ""
		for _, s := range p.Shares {
			// 摘要里的 owner 与条目本身的 owner 必须一致：不一致说明这条摘要
			// 是别的节点转述错了（或被人塞进来的），宁可不显示也不显示错。
			if !strings.EqualFold(s.OwnerID, p.NodeID.String()) {
				continue
			}
			// 可见性过滤在这里做一次，在 HandleIndexReq 里再做一次（那里是
			// 权威判定）。这里只是「不给我看的就别出现在我列表里」。
			if !s.VisibleTo(a.self.String(), groups) {
				continue
			}
			out = append(out, RemoteShare{Summary: s, Online: online})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ShareID < out[j].ShareID
	})
	return out
}

// findRemote 在目录里按 share_id 找一条对我可见的他人共享。
func (a *ShareApp) findRemote(shareID string) (RemoteShare, bool) {
	for _, s := range a.Remote() {
		if s.ShareID == shareID {
			return s, true
		}
	}
	return RemoteShare{}, false
}

// myGroupIDs 返回本机【当前在群】的群 id（可见性判定用）。
func (a *ShareApp) myGroupIDs() []string {
	if a.groups == nil {
		return nil
	}
	var out []string
	for _, g := range a.groups.List() {
		if m, ok := g.Member(a.self); ok && m.Active() {
			out = append(out, g.ID)
		}
	}
	return out
}

// groupsOf 返回某个节点当前所在的群 id（判定「仅群组」可见性时用）。
func (a *ShareApp) groupsOf(id identity.NodeID) []string {
	if a.groups == nil {
		return nil
	}
	var out []string
	for _, g := range a.groups.List() {
		// 解散的群不算：共享挂在已解散的群上时，成员身份不该再授予任何访问权。
		if g.IsDissolved() {
			continue
		}
		if m, ok := g.Member(id); ok && m.Active() {
			out = append(out, g.ID)
		}
	}
	return out
}

// peerOf 返回目录里对端条目与「是否值得拨号」。
func (a *ShareApp) peerOf(id identity.NodeID) (peer.Peer, bool, bool) {
	if a.dir == nil {
		return peer.Peer{}, false, false
	}
	p, ok := a.dir.Get(id)
	if !ok {
		return peer.Peer{}, false, false
	}
	reachable := (p.State == peer.StateOnline || p.State == peer.StateDiscovered) && p.LastAddr != ""
	return p, true, reachable
}

// ---------------------------------------------------------------------------
// 浏览清单（远端按需拉取）
// ---------------------------------------------------------------------------

// Items 返回一条共享（本地或他人）的文件清单。
//
// 本地直接读库；他人的发 SHARE_INDEX_REQ 按需拉取 —— 摘要里不带清单，
// 因为清单体积随目录大小增长，而摘要是每 45 秒广播一次的东西。
func (a *ShareApp) Items(ctx context.Context, shareID string) ([]share.Item, error) {
	if sh, ok := a.repo.Get(shareID); ok {
		return sh.Items, nil
	}

	remote, ok := a.findRemote(shareID)
	if !ok {
		return nil, fmt.Errorf("share: 找不到这条共享（可能已取消或对我不可见）")
	}
	if !remote.Online {
		return nil, fmt.Errorf("share: 共享方不在线（%s）", a.ownerLabel(remote.OwnerID, remote.OwnerName))
	}
	ownerID, err := identity.ParseNodeID(remote.OwnerID)
	if err != nil {
		return nil, fmt.Errorf("share: 共享方标识非法: %w", err)
	}
	resp, err := a.requestIndex(ctx, ownerID, shareID)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("share: %s", fallbackMsg(resp.Error, "共享方拒绝了清单请求"))
	}

	items := make([]share.Item, 0, len(resp.Items))
	for _, it := range resp.Items {
		clean, err := share.CleanRelPath(it.Path)
		if err != nil {
			// 清单也来自网络：路径畸形的条目直接丢掉，而不是让它一路走到
			// 「拼出本地路径」那一步才拦。丢一条比报错好 —— 一条畸形条目
			// 不该让整条共享无法浏览。
			continue
		}
		items = append(items, share.Item{
			Path: clean, Name: it.Name, Size: it.Size, Hash: it.Hash, MTime: it.MTime,
		})
	}
	return items, nil
}

// requestIndex 向共享方拉取清单。
func (a *ShareApp) requestIndex(ctx context.Context, ownerID identity.NodeID, shareID string) (protocol.ShareIndexResp, error) {
	sess, err := a.transfer.Session(ctx, ownerID)
	if err != nil {
		return protocol.ShareIndexResp{}, fmt.Errorf("share: 连接共享方失败: %w", err)
	}

	ch := make(chan protocol.ShareIndexResp, 1)
	a.mu.Lock()
	a.indexWait[shareID] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.indexWait, shareID)
		a.mu.Unlock()
	}()

	payload, err := protocol.EncodeJSON(protocol.ShareIndexReq{ShareID: shareID})
	if err != nil {
		return protocol.ShareIndexResp{}, err
	}
	if err := sess.Send(protocol.New(protocol.TypeShareIndexReq, payload)); err != nil {
		return protocol.ShareIndexResp{}, fmt.Errorf("share: 发送清单请求失败: %w", err)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-a.clk.After(a.indexTimeout()):
		return protocol.ShareIndexResp{}, fmt.Errorf("share: 请求共享清单超时（共享方未响应）")
	case <-ctx.Done():
		return protocol.ShareIndexResp{}, ctx.Err()
	}
}

// HandleIndexResp 是 SHARE_INDEX_RESP 的入站处理：投递给等待者。
func (a *ShareApp) HandleIndexResp(sess ports.Session, f protocol.Frame) error {
	var resp protocol.ShareIndexResp
	if err := protocol.DecodeJSON(f.Payload, &resp); err != nil {
		return err
	}
	// 归属校验：响应必须来自共享方本人。少了这一步，任何节点都能
	// 伪造一份「清单」把我们导向它自己的文件（虽然它还得先答上握手）。
	if resp.Owner != "" && !strings.EqualFold(resp.Owner, sess.PeerID().String()) {
		return fmt.Errorf("share: 清单响应来自非共享方 %s", sess.PeerID().String())
	}

	a.mu.Lock()
	ch := a.indexWait[resp.ShareID]
	a.mu.Unlock()
	if ch == nil {
		return nil // 超时后的迟到响应：丢弃
	}
	select {
	case ch <- resp:
	default:
	}
	return nil
}

// HandleIndexReq 是 SHARE_INDEX_REQ 的入站处理（本机是共享方）。
//
// 这是【唯一的权威访问控制点】：「仅群组」与「指定人」都在这里判定，
// 越权一律拒绝并把原因回给请求方（对方看不到我们的日志）。
func (a *ShareApp) HandleIndexReq(sess ports.Session, f protocol.Frame) error {
	var req protocol.ShareIndexReq
	if err := protocol.DecodeJSON(f.Payload, &req); err != nil {
		return err
	}
	requester := sess.PeerID()

	sh, ok := a.repo.Get(req.ShareID)
	if !ok {
		return a.sendIndexResp(sess, protocol.ShareIndexResp{
			ShareID: req.ShareID, Error: "共享不存在或已被取消",
		})
	}
	if !a.visibleTo(sh, requester) {
		if a.lg != nil {
			a.lg.Info("share: 拒绝越权访问",
				"share_id", sh.ID, "requester", requester.String())
		}
		return a.sendIndexResp(sess, protocol.ShareIndexResp{
			ShareID: req.ShareID, Name: sh.Name, Owner: a.self.String(),
			Error: "无权访问该共享（不在可见范围内）",
		})
	}

	items := make([]protocol.ShareIndexItem, 0, len(sh.Items))
	for _, it := range sh.Items {
		items = append(items, protocol.ShareIndexItem{
			Path: it.Path, Name: it.Name, Size: it.Size, Hash: it.Hash, MTime: it.MTime,
		})
	}
	return a.sendIndexResp(sess, protocol.ShareIndexResp{
		OK: true, ShareID: sh.ID, Name: sh.Name, Owner: a.self.String(), Items: items,
	})
}

// sendIndexResp 回传清单响应。
func (a *ShareApp) sendIndexResp(sess ports.Session, resp protocol.ShareIndexResp) error {
	payload, err := protocol.EncodeJSON(resp)
	if err != nil {
		return err
	}
	return sess.Send(protocol.New(protocol.TypeShareIndexResp, payload))
}

// visibleTo 判定某节点能否访问一条共享。
func (a *ShareApp) visibleTo(sh share.Share, requester identity.NodeID) bool {
	return sh.Visibility.Allows(requester.String(), a.groupsOf(requester), sh.Owner)
}

// ---------------------------------------------------------------------------
// 下载（P2P 直连拉取）
// ---------------------------------------------------------------------------

// DownloadResult 是一次共享拉取的受理结果。
type DownloadResult struct {
	// BatchID 是这一批传输的批次标识：界面用它把所有相关任务聚合成一张卡片，
	// 也用它取消整批。
	BatchID string
	// Accepted 是共享方受理的文件数。
	Accepted int
	// Rejected 是被跳过的条目及原因（文件已删除、路径非法等）。
	Rejected []string
}

// Download 请共享方把指定文件推过来。
//
// 返回即代表「已受理」：真正的字节传输是异步的，进度/完成/失败走既有的
// transfer:* 事件与传输列表（复用传输引擎的代价与红利都在这里体现）。
func (a *ShareApp) Download(ctx context.Context, shareID string, paths []string) (DownloadResult, error) {
	if len(paths) == 0 {
		return DownloadResult{}, fmt.Errorf("share: 请选择要下载的文件")
	}

	remote, ok := a.findRemote(shareID)
	if !ok {
		// 本地共享不需要「下载」，但把它当作明确错误报出来，
		// 比让界面在这里静默什么都不做要好。
		if _, local := a.repo.Get(shareID); local {
			return DownloadResult{}, fmt.Errorf("share: 这是本机发布的共享，无需下载")
		}
		return DownloadResult{}, fmt.Errorf("share: 找不到这条共享（可能已取消或对我不可见）")
	}
	if !remote.Online {
		return DownloadResult{}, fmt.Errorf("share: 共享方不在线（%s）", a.ownerLabel(remote.OwnerID, remote.OwnerName))
	}
	ownerID, err := identity.ParseNodeID(remote.OwnerID)
	if err != nil {
		return DownloadResult{}, fmt.Errorf("share: 共享方标识非法: %w", err)
	}
	// 目录还原依赖接收端的批次能力：老版本会把整批文件平铺到下载目录，
	// 而「共享目录下载完变成一堆散文件」不是降级，是数据错乱。
	if p, found, _ := a.peerOf(ownerID); found && p.Caps != 0 && !p.Caps.Has(peer.CapsFolderTransfer) {
		return DownloadResult{}, fmt.Errorf("share: 共享方版本不支持目录共享下载，请对方升级")
	}

	batchID := message.NewID()
	req := protocol.SharePullReq{
		ShareID:  shareID,
		Paths:    paths,
		BatchID:  batchID,
		RootName: a.batchRootName(remote),
	}

	sess, err := a.transfer.Session(ctx, ownerID)
	if err != nil {
		return DownloadResult{}, fmt.Errorf("share: 连接共享方失败: %w", err)
	}

	ch := make(chan protocol.SharePullResp, 1)
	a.mu.Lock()
	a.pullWait[batchID] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pullWait, batchID)
		a.mu.Unlock()
	}()

	payload, err := protocol.EncodeJSON(req)
	if err != nil {
		return DownloadResult{}, err
	}
	if err := sess.Send(protocol.New(protocol.TypeSharePullReq, payload)); err != nil {
		return DownloadResult{}, fmt.Errorf("share: 发送下载请求失败: %w", err)
	}

	select {
	case resp := <-ch:
		if !resp.OK {
			return DownloadResult{}, fmt.Errorf("share: %s", fallbackMsg(resp.Error, "共享方拒绝了下载请求"))
		}
		if resp.Accepted == 0 {
			return DownloadResult{}, fmt.Errorf("share: 共享方没有受理任何文件（清单可能已过期，请刷新后重试）")
		}
		return DownloadResult{BatchID: batchID, Accepted: resp.Accepted, Rejected: resp.Rejected}, nil
	case <-a.clk.After(a.pullTimeout()):
		return DownloadResult{}, fmt.Errorf("share: 请求下载超时（共享方未响应）")
	case <-ctx.Done():
		return DownloadResult{}, ctx.Err()
	}
}

// HandlePullResp 是 SHARE_PULL_RESP 的入站处理：按 batch_id 投递给等待者。
func (a *ShareApp) HandlePullResp(sess ports.Session, f protocol.Frame) error {
	var resp protocol.SharePullResp
	if err := protocol.DecodeJSON(f.Payload, &resp); err != nil {
		return err
	}
	a.mu.Lock()
	ch := a.pullWait[resp.BatchID]
	a.mu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case ch <- resp:
	default:
	}
	return nil
}

// HandlePullReq 是 SHARE_PULL_REQ 的入站处理（本机是共享方）。
//
// 六道关口一个都不能少，且顺序是刻意的：
//  1. 共享存在（可能刚被取消）；
//  2. 请求者在可见范围内（权威判定）；
//  3. 请求条数在上限内（防一次请求建出几万条任务）；
//  4. 每条路径必须在【本机索引】里存在（据此取得绝对路径，绝不直接拼接
//     请求方的字符串）；
//  5. 相对路径要能清洗成安全段，且拼接结果必须落在共享根之内；
//  6. 文件此刻确实还在（清单是快照，磁盘变了就得如实拒掉那一条）。
func (a *ShareApp) HandlePullReq(sess ports.Session, f protocol.Frame) error {
	var req protocol.SharePullReq
	if err := protocol.DecodeJSON(f.Payload, &req); err != nil {
		return err
	}
	respond := func(resp protocol.SharePullResp) error {
		resp.BatchID = req.BatchID
		payload, err := protocol.EncodeJSON(resp)
		if err != nil {
			return err
		}
		return sess.Send(protocol.New(protocol.TypeSharePullResp, payload))
	}

	requester := sess.PeerID()
	if req.BatchID == "" {
		return respond(protocol.SharePullResp{Error: "下载请求缺少批次标识"})
	}
	sh, ok := a.repo.Get(req.ShareID)
	if !ok {
		return respond(protocol.SharePullResp{Error: "共享不存在或已被取消"})
	}
	if !a.visibleTo(sh, requester) {
		if a.lg != nil {
			a.lg.Info("share: 拒绝越权下载", "share_id", sh.ID, "requester", requester.String())
		}
		return respond(protocol.SharePullResp{Error: "无权访问该共享（不在可见范围内）"})
	}
	paths := req.Paths
	if len(paths) > a.maxPullPaths() {
		return respond(protocol.SharePullResp{
			Error: fmt.Sprintf("一次最多下载 %d 个文件，请分批选择", a.maxPullPaths()),
		})
	}

	rootName := pullRootName(sh, req.RootName)

	accepted := 0
	var rejected []string
	for _, raw := range paths {
		it, ok := sh.ItemByPath(raw)
		if !ok {
			rejected = append(rejected, raw+"（清单里没有这个文件，可能已被删除）")
			continue
		}
		abs, err := a.resolveLocal(sh, it.Path)
		if err != nil {
			rejected = append(rejected, raw+"（"+err.Error()+"）")
			continue
		}
		if !it.IsDir {
			if st, err := os.Stat(abs); err != nil || st.IsDir() {
				rejected = append(rejected, raw+"（文件已不存在）")
				continue
			}
		}
		rel, err := share.JoinRelPath(rootName, it.Path)
		if err != nil {
			rejected = append(rejected, raw+"（路径非法）")
			continue
		}
		if _, err := a.transfer.SendSharedFile(a.ctx, SharedSendRequest{
			PeerID:    requester,
			LocalPath: abs,
			BatchID:   req.BatchID,
			RelPath:   rel,
			IsDir:     it.IsDir,
		}); err != nil {
			rejected = append(rejected, raw+"（"+err.Error()+"）")
			continue
		}
		accepted++
	}

	if a.lg != nil {
		a.lg.Info("share: 受理共享下载",
			"share_id", sh.ID, "requester", requester.String(),
			"accepted", accepted, "rejected", len(rejected))
	}
	return respond(protocol.SharePullResp{OK: accepted > 0, Accepted: accepted, Rejected: rejected})
}

// resolveLocal 把共享内的相对路径解析成绝对路径，并确认它没有越出共享根。
//
// 两道防线：CleanRelPath 拒绝 `..` / 绝对路径 / 盘符，逐段拼接后
// 再做一次「前缀 + 分隔符」检查。后者不是冗余 —— 前者的规则集合
// 会随平台变化（比如 Windows 的 8.3 短名），而越界写入是不可接受的后果。
func (a *ShareApp) resolveLocal(sh share.Share, relPath string) (string, error) {
	segs, err := fsutil.SanitizeRelPath(relPath)
	if err != nil {
		return "", fmt.Errorf("路径非法")
	}
	root := filepath.Clean(sh.RootPath)
	// 单文件共享：RelPath 就是文件名本身，它的绝对路径是共享根自己。
	if !sh.IsDir {
		return root, nil
	}
	abs := filepath.Join(append([]string{root}, segs...)...)
	if !withinRoot(root, abs) {
		return "", fmt.Errorf("路径越界")
	}
	return abs, nil
}

// withinRoot 报告 abs 是否等于 root 或位于 root 之内。
func withinRoot(root, abs string) bool {
	if abs == root {
		return true
	}
	// 用 filepath.Rel 而不是字符串前缀：前缀比较会把 /a/bc 误判成在 /a/b 内。
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// singleFileRootName 是「共享单个文件」时接收端的容器目录名。
//
// 为什么需要它：批次模型要求相对路径至少两段（根名/文件名），而单文件共享的
// 共享名默认就是文件名 —— 直接用共享名会落到 `<下载目录>/报告.pdf/报告.pdf`。
// 固定一个容器目录后是 `<下载目录>/共享文件/报告.pdf`，多个单文件共享共用它
// （同名文件按既有的「同目录复用 + 同名覆盖」规则处理）。
const singleFileRootName = "共享文件"

// batchRootName 决定接收端批次根目录名。
//
// 注意它只是【请求方】的提议：服务端的 pullRootName 可能不采纳（见那里的说明）。
func (a *ShareApp) batchRootName(remote RemoteShare) string {
	if name := sanitizeDirName(remote.Name); name != "" {
		return name
	}
	return "shared"
}

// pullRootName 决定一次拉取在接收端用的批次根目录名（服务端是唯一的权威）。
//
// 单文件共享【由共享方强行指定】容器目录，不看请求方的提议：摘要里没有 IsDir，
// 请求方根本不知道对方共享的是一个文件还是一个目录，而这个名字决定的是对端
// 磁盘上的落点 —— 只有知道的那一侧能定。强行指定也让旧版请求方（不管它传了
// 什么）都能得到正确的落点，不需要新增协议字段。
func pullRootName(sh share.Share, proposed string) string {
	if !sh.IsDir {
		return singleFileRootName
	}
	rootName := sanitizeDirName(proposed)
	if rootName == "" {
		rootName = sanitizeDirName(sh.Name)
	}
	// 批次根名会经过接收端的路径清洗：先在这里清掉非法字符，
	// 免得整个批次被对端以「路径非法」拒掉。
	if rootName == "" {
		return "shared"
	}
	return rootName
}

// ownerLabel 给共享方一个可读标签（昵称优先，退到短 id）。
func (a *ShareApp) ownerLabel(ownerID, ownerName string) string {
	if strings.TrimSpace(ownerName) != "" {
		return ownerName
	}
	if len(ownerID) > 8 {
		return ownerID[:8]
	}
	return ownerID
}

// sanitizeDirName 把共享名清洗成一个可用的目录名。
func sanitizeDirName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	clean, err := fsutil.SanitizeFileName(name)
	if err != nil || clean == "" {
		return ""
	}
	return clean
}

// fallbackMsg 在消息为空时给一句兜底文案。
func fallbackMsg(msg, fallback string) string {
	if strings.TrimSpace(msg) == "" {
		return fallback
	}
	return msg
}
