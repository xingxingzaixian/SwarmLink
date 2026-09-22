package wails

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/config"
	"github.com/swarmlink/swarmlink/internal/app"
	"github.com/swarmlink/swarmlink/internal/domain/group"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/infra/imagecodec"
)

// ConnInspector 是诊断面板所需的连接信息（由 tcp.Manager 实现）。
type ConnInspector interface {
	SessionCount() int
	SessionOf(id identity.NodeID) (ports.Session, bool)
}

// SelfInfo 描述本机运行参数。
type SelfInfo struct {
	NodeID      identity.NodeID
	DisplayName string
	TCPPort     int
	UDPPort     int
	Subnet      string
}

// Deps 是服务层的依赖集合，全部由组合根注入。
//
// 注意：本包只依赖 app / domain / config，不依赖任何 net/store 适配器，
// 因此服务层可以在没有真实网络的测试里跑。
type Deps struct {
	Self SelfInfo

	Chat      *app.ChatApp
	Transfer  *app.TransferApp
	Group     *app.GroupApp
	Peers     *app.PeerApp
	Discovery *app.DiscoveryApp

	PeerDir      ports.PeerDirectory
	TransferRepo ports.TransferRepo
	GroupRepo    ports.GroupRepo
	Conns        ConnInspector
	Seeds        ports.SeedRegistry
	Interfaces   func() []string
	// SeedSnapshot 由组合根注入（避免本包依赖 udp 适配器的具体类型）。
	SeedSnapshot func() []SeedDTO

	Cfg       *config.Config
	ConfigDir string
}

// ServiceSet 是注册给 Wails 的五个服务。
type ServiceSet struct {
	Settings *SettingsService
	Chat     *ChatService
	Transfer *TransferService
	Peer     *PeerService
	Group    *GroupService
}

// NewServices 构造服务集合。
func NewServices(d Deps) *ServiceSet {
	return &ServiceSet{
		Settings: &SettingsService{cfg: d.Cfg, dir: d.ConfigDir, d: d},
		Chat:     &ChatService{d: d},
		Transfer: &TransferService{d: d},
		Peer:     &PeerService{d: d},
		Group:    &GroupService{d: d},
	}
}

// ---------------------------------------------------------------------------
// SettingsService
// ---------------------------------------------------------------------------

// SettingsService 读写配置。
//
// 重要：端口与网卡相关项【需重启生效】；前端必须明确标注，
// 否则用户改完端口发现没变化会认为是 bug（架构书 7.4）。
type SettingsService struct {
	d   Deps
	cfg *config.Config
	dir string
}

// Get 返回当前配置快照。
func (s *SettingsService) Get() SettingsDTO {
	c := s.cfg
	seeds := make([]string, 0, len(c.Discovery.Seeds.List))
	seeds = append(seeds, c.Discovery.Seeds.List...)

	return SettingsDTO{
		DisplayName:     c.General.DisplayName,
		DownloadDir:     c.Download.DefaultDir,
		AutoOpen:        c.Download.AutoOpen,
		TCPPort:         c.Network.TCPPort,
		UDPPort:         c.Network.UDPPort,
		InterfaceMode:   c.Network.InterfaceMode,
		AllowInterfaces: c.Network.AllowInterfaces,
		DenyInterfaces:  c.Network.DenyInterfaces,
		Seeds:           seeds,
		SeedRefreshSec:  int(c.Discovery.Seeds.RefreshInterval.Std() / time.Second),
		SeedsPerRefresh: c.Discovery.Seeds.SeedsPerRefresh,
		MaxConcurrent:   c.Transfer.MaxConcurrent,
		ChunkSize:       c.Transfer.ChunkSize,
		WindowSize:      c.Transfer.WindowSize,
		MaxActiveConns:  c.Connection.MaxActiveConns,
		IdleTimeoutSec:  int(c.Connection.IdleConnTimeout.Std() / time.Second),
		RequireAuth:     c.Security.RequireAuth,
		Encryption:      c.Security.Encryption,
	}
}

// Save 落盘配置。返回需要重启才能生效的项清单。
func (s *SettingsService) Save(in SettingsDTO) ([]string, error) {
	c := s.cfg

	c.General.DisplayName = in.DisplayName
	c.Download.DefaultDir = in.DownloadDir
	c.Download.AutoOpen = in.AutoOpen
	c.Discovery.Seeds.List = append([]string(nil), in.Seeds...)
	if in.SeedsPerRefresh > 0 {
		c.Discovery.Seeds.SeedsPerRefresh = in.SeedsPerRefresh
	}
	if in.SeedRefreshSec > 0 {
		c.Discovery.Seeds.RefreshInterval = config.Duration(time.Duration(in.SeedRefreshSec) * time.Second)
	}
	if in.MaxConcurrent > 0 {
		c.Transfer.MaxConcurrent = in.MaxConcurrent
	}
	if in.ChunkSize > 0 {
		c.Transfer.ChunkSize = in.ChunkSize
	}
	if in.WindowSize > 0 {
		c.Transfer.WindowSize = in.WindowSize
	}
	if in.IdleTimeoutSec > 0 {
		c.Connection.IdleConnTimeout = config.Duration(time.Duration(in.IdleTimeoutSec) * time.Second)
	}
	if in.Encryption != "" {
		c.Security.Encryption = in.Encryption
	}

	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := config.Save(s.dir, c); err != nil {
		return nil, err
	}

	// 端口/网卡类改动无法热生效
	restart := make([]string, 0, 4)
	if in.TCPPort != c.Network.TCPPort {
		restart = append(restart, "tcp_port")
	}
	if in.UDPPort != c.Network.UDPPort {
		restart = append(restart, "udp_port")
	}
	if in.InterfaceMode != c.Network.InterfaceMode {
		restart = append(restart, "interface_mode")
	}
	if in.MaxActiveConns != c.Connection.MaxActiveConns {
		restart = append(restart, "max_active_conns")
	}
	return restart, nil
}

// DownloadDir 返回当前接收目录。
func (s *SettingsService) DownloadDir() string { return s.cfg.Download.DefaultDir }

// ListInterfaces 返回本机参与发现的网卡摘要（供设置页展示）。
func (s *SettingsService) ListInterfaces() []string {
	if s.d.Interfaces == nil {
		return nil
	}
	return s.d.Interfaces()
}

// RunSelfCheck 执行部署前置条件自检（P-1 / P-2）。
//
// 它会真的发起一轮种子探测与目录拉取，因此最坏要等 3 秒 —— 这是刻意的：
// 「发现不到人」的根因九成在这两条前提上，一次真实探测比任何静态提示都有用。
func (s *SettingsService) RunSelfCheck() SelfCheckDTO {
	if s.d.Discovery == nil {
		return SelfCheckDTO{P1Detail: "自检不可用：发现服务未启动"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r := app.SelfCheck(ctx, app.SelfCheckInput{
		Discovery:  s.d.Discovery,
		Peers:      s.d.PeerDir,
		SelfSubnet: s.d.Self.Subnet,
	})

	out := SelfCheckDTO{
		Performed: r.Performed,
		SeedCount: r.SeedCount,
		Learned:   r.Learned,
		P1OK:      r.P1OK,
		P1Detail:  r.P1Detail,
		P2Overlap: r.P2Overlap,
		P2Detail:  r.P2Detail,
	}
	if s.d.SeedSnapshot != nil {
		out.Seeds = s.d.SeedSnapshot()
	}
	return out
}

// ---------------------------------------------------------------------------
// ChatService
// ---------------------------------------------------------------------------

// ChatService 暴露单聊能力。
type ChatService struct{ d Deps }

// SendMessage 发送单聊消息，返回已入库的消息（状态为 pending，等待 ACK）。
func (s *ChatService) SendMessage(peerID, text string) (MessageDTO, error) {
	id, err := identity.ParseNodeID(peerID)
	if err != nil {
		return MessageDTO{}, fmt.Errorf("无效的 NodeID: %w", err)
	}
	if text == "" {
		return MessageDTO{}, fmt.Errorf("消息内容不能为空")
	}
	msg, err := s.d.Chat.SendMessage(context.Background(), id, text)
	if err != nil {
		return MessageDTO{}, err
	}
	return ToMessageDTO(msg), nil
}

// SendImage 发送一张本地图片：读文件 → 压缩 → 内联进消息。
//
// 为什么压缩放在这里（而不是前端）：前端拿到的是路径或 Blob，压缩需要解码 + 缩放 + 体积控制，
// 放 Go 侧只需实现一次，且不会把大图搬进 WebView 的内存。
func (s *ChatService) SendImage(peerID, path string) (MessageDTO, error) {
	id, err := identity.ParseNodeID(peerID)
	if err != nil {
		return MessageDTO{}, fmt.Errorf("无效的 NodeID: %w", err)
	}
	if err := checkSendableFile(path); err != nil {
		return MessageDTO{}, err
	}
	content, err := prepareImageFile(path)
	if err != nil {
		return MessageDTO{}, err
	}
	msg, err := s.d.Chat.SendImage(context.Background(), id, content)
	if err != nil {
		return MessageDTO{}, err
	}
	return ToMessageDTO(msg), nil
}

// SendImageBytes 发送剪贴板里的图片：浏览器只能拿到 Blob，拿不到本地路径。
//
// dataB64 用 base64 字符串而不是 []byte：Wails 生成器把 []byte 映射成 TS 的
// `string | null`，参数类型与运行时编码之间会留下一次「猜」的机会。
// 显式声明为 base64 字符串，两侧的契约才是自解释的。
func (s *ChatService) SendImageBytes(peerID, name, dataB64 string) (MessageDTO, error) {
	id, err := identity.ParseNodeID(peerID)
	if err != nil {
		return MessageDTO{}, fmt.Errorf("无效的 NodeID: %w", err)
	}
	data, err := decodeImageBytes(dataB64)
	if err != nil {
		return MessageDTO{}, err
	}
	content, err := prepareImageData(data)
	if err != nil {
		return MessageDTO{}, withImageSource(err, name)
	}
	msg, err := s.d.Chat.SendImage(context.Background(), id, content)
	if err != nil {
		return MessageDTO{}, err
	}
	return ToMessageDTO(msg), nil
}

// SaveImage 把一条图片消息另存到指定路径。
func (s *ChatService) SaveImage(msgID, destPath string) error {
	if msgID == "" {
		return fmt.Errorf("缺少消息 id")
	}
	if destPath == "" {
		return fmt.Errorf("请选择保存位置")
	}
	msg, ok := s.d.Chat.Message(msgID)
	if !ok {
		return fmt.Errorf("找不到该图片消息")
	}
	if msg.MsgType != message.MsgTypeImage {
		return fmt.Errorf("这条消息不是图片")
	}
	img, err := message.DecodeInlineImage(msg.Content)
	if err != nil {
		return fmt.Errorf("图片内容已损坏: %w", err)
	}
	raw, err := img.Bytes()
	if err != nil {
		return err
	}
	// 先看目录：目标目录不存在是「用户选错了位置」，与「写盘失败」不是一回事，
	// 分开报错才能给出可行动的信息。
	if dir := filepath.Dir(destPath); dir != "" {
		if st, statErr := os.Stat(dir); statErr != nil || !st.IsDir() {
			return fmt.Errorf("保存位置不存在：%s", dir)
		}
	}
	if err := os.WriteFile(destPath, raw, 0o644); err != nil {
		return fmt.Errorf("保存图片失败: %w", err)
	}
	return nil
}

// decodeImageBytes 把前端传来的 base64 图片数据解码成字节。
func decodeImageBytes(dataB64 string) ([]byte, error) {
	if dataB64 == "" {
		return nil, fmt.Errorf("剪贴板里没有图片数据")
	}
	raw, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return nil, fmt.Errorf("图片数据不是合法的 base64")
	}
	return raw, nil
}

// prepareImageFile 读取并规范化一个本地图片文件。
func prepareImageFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("无法读取该图片: %w", err)
	}
	return prepareImageData(raw)
}

// prepareImageData 把图片字节变成可内联的消息 content。
//
// 错误信息刻意做成用户能读懂并据此行动的中文：图片发送失败的原因
// 只可能是「不是图片 / 太大 / 解不开」这三种，让用户猜是最差的选择。
func prepareImageData(data []byte) (string, error) {
	img, err := imagecodec.Prepare(data, imagecodec.DefaultMaxEdge, imagecodec.DefaultMaxBytes)
	switch {
	case errors.Is(err, imagecodec.ErrNotImage):
		return "", fmt.Errorf("不是可识别的图片格式")
	case errors.Is(err, imagecodec.ErrTooLarge):
		return "", fmt.Errorf("图片过大，压缩后仍超出上限，请改用「发送文件」")
	case errors.Is(err, imagecodec.ErrEmpty):
		return "", fmt.Errorf("图片内容为空")
	case err != nil:
		return "", fmt.Errorf("处理图片失败: %w", err)
	}

	content, err := message.EncodeInlineImage(message.InlineImage{
		MIME: img.MIME,
		W:    img.W,
		H:    img.H,
		B64:  base64.StdEncoding.EncodeToString(img.Data),
	})
	if err != nil {
		return "", err
	}
	return content, nil
}

// withImageSource 给错误补上来源（剪贴板里的文件名/来源路径），
// 否则用户面对「不是可识别的图片格式」时无从判断说的是哪一张。
func withImageSource(err error, source string) error {
	if err == nil || source == "" {
		return err
	}
	return fmt.Errorf("%s：%w", source, err)
}

// History 返回与某对端的最近消息（新→旧）。
func (s *ChatService) History(peerID string, limit int, before int64) ([]MessageDTO, error) {
	id, err := identity.ParseNodeID(peerID)
	if err != nil {
		return nil, fmt.Errorf("无效的 NodeID: %w", err)
	}
	if limit <= 0 {
		limit = 50
	}
	ms, err := s.d.Chat.History(message.DirectConvID(s.d.Self.NodeID, id), limit, before)
	if err != nil {
		return nil, err
	}
	out := make([]MessageDTO, 0, len(ms))
	for _, m := range ms {
		out = append(out, ToMessageDTO(m))
	}
	return out, nil
}

// Conversations 返回会话列表（按最近消息时间倒序）。
//
// 目前直接从节点目录 + 消息推导，避免为 UI 单独维护一张表。
func (s *ChatService) Conversations() ([]ConversationDTO, error) {
	out := make([]ConversationDTO, 0)

	for _, p := range s.d.PeerDir.List(ports.PeerFilter{}) {
		convID := message.DirectConvID(s.d.Self.NodeID, p.NodeID)
		ms, err := s.d.Chat.History(convID, 1, 0)
		if err != nil || len(ms) == 0 {
			continue
		}
		last := ms[0]
		_, connected := s.d.Conns.SessionOf(p.NodeID)
		title := p.DisplayName
		if title == "" {
			title = p.NodeID.String()
		}
		out = append(out, ConversationDTO{
			ConvID:   convID,
			Kind:     "direct",
			Title:    title,
			PeerID:   p.NodeID.String(),
			State:    string(p.State),
			IsOnline: connected,
			LastTime: last.SentAt.UnixMilli(),
		})
	}

	for _, g := range s.d.GroupRepo.List() {
		ms, err := s.d.Chat.History(message.GroupConvID(g.ID), 1, 0)
		if err != nil || len(ms) == 0 {
			continue
		}
		out = append(out, ConversationDTO{
			ConvID:   message.GroupConvID(g.ID),
			Kind:     "group",
			Title:    g.Name,
			LastTime: ms[0].SentAt.UnixMilli(),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].LastTime > out[j].LastTime })
	return out, nil
}

// ---------------------------------------------------------------------------
// TransferService
// ---------------------------------------------------------------------------

// TransferService 暴露文件传输能力。
type TransferService struct{ d Deps }

// SendFile 发送文件（异步：立即返回 job_id，进度通过事件推送）。
//
// 返回值契约：这是【真正的 job_id】，它会原样出现在随后的
// transfer:progress / transfer:done / transfer:error 事件里。
//
// 因此 job_id 在这里生成，再下传给 app 层（TransferApp.SendFileAs）。
// 早先的实现是 app 自己生成 id、这里另造一个占位值返回 —— 界面拿到的 id
// 与任何事件都无关，任务条目永远等不到属于自己的进度。
//
// 仍然不阻塞：文件哈希与传输都在后台 goroutine 里跑。但路径本身在返回前
// 做一次最廉价的检查 —— 路径打错是这里最常见的失误，让它同步报错，
// 用户就不必对着一个「排队中」的任务猜发生了什么。
func (s *TransferService) SendFile(peerID, path string) (string, error) {
	id, err := identity.ParseNodeID(peerID)
	if err != nil {
		return "", fmt.Errorf("无效的 NodeID: %w", err)
	}
	if path == "" {
		return "", fmt.Errorf("请选择文件")
	}
	if err := checkSendableFile(path); err != nil {
		return "", err
	}

	jobID := message.NewID()
	go func() {
		// 结果通过 transfer.done / transfer.error 事件通知前端；
		// 这里必须脱离请求生命周期，否则前端切页会中断传输。
		_, _ = s.d.Transfer.SendFileAs(context.Background(), id, path, jobID)
	}()
	return jobID, nil
}

// Reveal 在系统文件管理器中定位该路径（尽可能选中文件本身）。
//
// 三个平台语义不同：Windows 用 `explorer /select,`、macOS 用 `open -R`
// 都能选中文件；Linux 的 xdg-open 只能打开所在目录。
//
// 一律用 exec.Command 传【参数数组】、不经过 shell ——
// 否则路径里的空格、分号、& 会被当成命令解析（这是最常见的注入口子）。
func (s *TransferService) Reveal(path string) error {
	if path == "" {
		return fmt.Errorf("没有可定位的路径")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("无效路径: %w", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("路径已不存在: %w", err)
	}
	dir := abs
	if !st.IsDir() {
		dir = filepath.Dir(abs)
	}

	switch runtime.GOOS {
	case "windows":
		return exec.Command("explorer", "/select,"+abs).Start()
	case "darwin":
		return exec.Command("open", "-R", abs).Start()
	default:
		return exec.Command("xdg-open", dir).Start()
	}
}

// ClearFinished 清空已结束的传输记录，返回删除条数。
//
// 与启动时的自动清理共用 PurgeFinished：keep=0 表示一条都不保留，
// olderThan=now 表示「比现在更早的都算」—— 合起来即清空全部已结束的。
func (s *TransferService) ClearFinished() (int, error) {
	return s.d.TransferRepo.PurgeFinished(0, time.Now())
}

// checkSendableFile 在建立任务之前做一次廉价的可用性检查。
//
// 与 app 层的重复是有意的：这里决定的是「错误【何时】出现」，不是正确性 ——
// 传输真正开始后仍会按实际读取结果报错。前置检查的价值在于把
// 「路径打错 / 选到目录 / 空文件」这三类失误从异步静默失败变成同步报错。
func checkSendableFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("无法读取文件: %w", err)
	}
	if st.IsDir() {
		return fmt.Errorf("这是一个目录：v1.0 只支持单文件传输")
	}
	if st.Size() == 0 {
		return fmt.Errorf("空文件没有可传输的内容")
	}
	return nil
}

// List 返回最近的传输任务（含已结束的），供界面的传输记录使用。
//
// 这里必须是 ListRecent 而不是 ListActive：后者排除 done/cancelled，
// 用它会导致「文件明明传完了，列表里却没有任何记录」。
func (s *TransferService) List() ([]TransferDTO, error) {
	jobs, err := s.d.TransferRepo.ListRecent(50)
	if err != nil {
		return nil, err
	}
	out := make([]TransferDTO, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, ToTransferDTO(j))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
}

// Get 返回单个传输任务。
func (s *TransferService) Get(jobID string) (TransferDTO, error) {
	j, err := s.d.TransferRepo.GetJob(jobID)
	if err != nil {
		return TransferDTO{}, err
	}
	return ToTransferDTO(j), nil
}

// ---------------------------------------------------------------------------
// PeerService
// ---------------------------------------------------------------------------

// PeerService 暴露节点目录与诊断信息。
type PeerService struct{ d Deps }

// Self 返回本机身份与运行参数。
func (s *PeerService) Self() SelfDTO {
	return SelfDTO{
		NodeID:      s.d.Self.NodeID.String(),
		DisplayName: s.d.Self.DisplayName,
		TCPPort:     s.d.Self.TCPPort,
		UDPPort:     s.d.Self.UDPPort,
		Subnet:      s.d.Self.Subnet,
	}
}

// List 返回节点列表。
func (s *PeerService) List() []PeerDTO {
	all := s.d.PeerDir.List(ports.PeerFilter{})
	out := make([]PeerDTO, 0, len(all))
	for _, p := range all {
		_, connected := s.d.Conns.SessionOf(p.NodeID)
		out = append(out, ToPeerDTO(p, connected))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return out[i].DisplayName < out[j].DisplayName
	})
	return out
}

// Diagnostics 返回诊断面板数据。
//
// 「常驻连接数」与「无保活流量」是 ADR-009 的两条可观测断言，
// 因此它们必须出现在诊断面板上，而不是只存在于日志里。
func (s *PeerService) Diagnostics() DiagnosticsDTO {
	all := s.d.PeerDir.List(ports.PeerFilter{})
	online := 0
	for _, p := range all {
		if p.State == peer.StateOnline {
			online++
		}
	}

	dto := DiagnosticsDTO{
		PeersTotal:      len(all),
		PeersOnline:     online,
		ActiveSessions:  s.d.Conns.SessionCount(),
		ActiveTransfers: s.d.Transfer.ActiveJobs(),
	}
	if s.d.Interfaces != nil {
		dto.InterfaceSummary = s.d.Interfaces()
	}
	if s.d.SeedSnapshot != nil {
		dto.SeedDetails = s.d.SeedSnapshot()
	}
	dto.SeedCount = len(dto.SeedDetails)
	return dto
}

// ---------------------------------------------------------------------------
// GroupService
// ---------------------------------------------------------------------------

// GroupService 暴露群聊能力。
type GroupService struct{ d Deps }

// List 返回本机已知群。
func (s *GroupService) List() []GroupDTO {
	gs := s.d.GroupRepo.List()
	out := make([]GroupDTO, 0, len(gs))
	for _, g := range gs {
		out = append(out, toGroupDTO(g, s.d.Self.NodeID))
	}
	return out
}

// Create 建群（本方为群主，上限 20 人）。
func (s *GroupService) Create(name string, memberIDs []string) (GroupDTO, error) {
	if name == "" {
		return GroupDTO{}, fmt.Errorf("群名不能为空")
	}
	ids := make([]identity.NodeID, 0, len(memberIDs))
	for _, raw := range memberIDs {
		id, err := identity.ParseNodeID(raw)
		if err != nil {
			return GroupDTO{}, fmt.Errorf("无效的成员 NodeID %q: %w", raw, err)
		}
		ids = append(ids, id)
	}
	g, err := s.d.Group.CreateGroup(context.Background(), name, ids)
	if err != nil {
		return GroupDTO{}, err
	}
	return toGroupDTO(g, s.d.Self.NodeID), nil
}

// AddMember 由群主添加成员（epoch+1 并广播）。
func (s *GroupService) AddMember(groupID, memberID string) (GroupDTO, error) {
	id, err := identity.ParseNodeID(memberID)
	if err != nil {
		return GroupDTO{}, fmt.Errorf("无效的成员 NodeID: %w", err)
	}
	g, err := s.d.Group.AddMember(context.Background(), groupID, id)
	if err != nil {
		return GroupDTO{}, err
	}
	return toGroupDTO(g, s.d.Self.NodeID), nil
}

// SendMessage 群发消息（扇出到在线成员；离线成员不暂存）。
func (s *GroupService) SendMessage(groupID, text string) (MessageDTO, error) {
	if text == "" {
		return MessageDTO{}, fmt.Errorf("消息内容不能为空")
	}
	msg, err := s.d.Group.SendGroupMessage(context.Background(), groupID, text)
	if err != nil {
		return MessageDTO{}, err
	}
	return ToMessageDTO(msg), nil
}

// SendImage 群发一张本地图片（与单聊同一套压缩与内联格式）。
func (s *GroupService) SendImage(groupID, path string) (MessageDTO, error) {
	if groupID == "" {
		return MessageDTO{}, fmt.Errorf("缺少群 id")
	}
	if err := checkSendableFile(path); err != nil {
		return MessageDTO{}, err
	}
	content, err := prepareImageFile(path)
	if err != nil {
		return MessageDTO{}, err
	}
	msg, err := s.d.Group.SendGroupImage(context.Background(), groupID, content)
	if err != nil {
		return MessageDTO{}, err
	}
	return ToMessageDTO(msg), nil
}

// SendImageBytes 群发剪贴板里的图片（dataB64 为 base64 字符串）。
func (s *GroupService) SendImageBytes(groupID, name, dataB64 string) (MessageDTO, error) {
	if groupID == "" {
		return MessageDTO{}, fmt.Errorf("缺少群 id")
	}
	data, err := decodeImageBytes(dataB64)
	if err != nil {
		return MessageDTO{}, err
	}
	content, err := prepareImageData(data)
	if err != nil {
		return MessageDTO{}, withImageSource(err, name)
	}
	msg, err := s.d.Group.SendGroupImage(context.Background(), groupID, content)
	if err != nil {
		return MessageDTO{}, err
	}
	return ToMessageDTO(msg), nil
}

// History 返回群历史消息（新→旧）。
func (s *GroupService) History(groupID string, limit int, before int64) ([]MessageDTO, error) {
	if limit <= 0 {
		limit = 50
	}
	ms, err := s.d.Chat.History(message.GroupConvID(groupID), limit, before)
	if err != nil {
		return nil, err
	}
	out := make([]MessageDTO, 0, len(ms))
	for _, m := range ms {
		out = append(out, ToMessageDTO(m))
	}
	return out, nil
}

func toGroupDTO(g group.Group, self identity.NodeID) GroupDTO {
	members := make([]GroupMemberDTO, 0, len(g.Members))
	for _, m := range g.Members {
		members = append(members, GroupMemberDTO{
			NodeID:      m.NodeID.String(),
			DisplayName: m.DisplayName,
			Role:        string(m.Role),
			State:       string(m.State),
		})
	}
	return GroupDTO{
		GroupID:   g.ID,
		Name:      g.Name,
		OwnerID:   g.OwnerID.String(),
		Epoch:     g.Epoch,
		IsOwner:   g.OwnerID == self,
		Members:   members,
		CreatedAt: g.CreatedAt.UnixMilli(),
	}
}
