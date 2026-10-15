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
	"strings"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/config"
	"github.com/swarmlink/swarmlink/internal/app"
	"github.com/swarmlink/swarmlink/internal/domain/group"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/share"
	"github.com/swarmlink/swarmlink/internal/infra/imagecodec"
)

// autostartFuncs 存储自动启动函数（全局变量，避免暴露给 Wails 绑定）。
//
// 只有 enable/disable：查询侧没有消费者 —— 设置页显示的状态来自配置文件
// （SettingsDTO.autoStart），不必再问系统一次，问出来还可能与配置不一致。
var autostartFuncs struct {
	enable  func() error
	disable func() error
}

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
	Share     *app.ShareApp

	PeerDir      ports.PeerDirectory
	TransferRepo ports.TransferRepo
	GroupRepo    ports.GroupRepo
	Deliveries   ports.GroupDeliveryRepo
	Conns        ConnInspector
	Seeds        ports.SeedRegistry
	Interfaces   func() []string
	// SeedSnapshot 由组合根注入（避免本包依赖 udp 适配器的具体类型）。
	SeedSnapshot func() []SeedDTO

	// SetActiveConv 由组合根注入：前端上报「用户正在看哪个会话」，
	// 提示决策据此区分「正看着这个会话（不打扰）」与「别的会话来消息（要提示）」。
	SetActiveConv func(convID string)

	Cfg       *config.Config
	ConfigDir string
}

// ServiceSet 是注册给 Wails 的服务集合。
type ServiceSet struct {
	Settings *SettingsService
	Chat     *ChatService
	Transfer *TransferService
	Peer     *PeerService
	Group    *GroupService
	Notify   *NotifyService
	Share    *ShareService
}

// NewServices 构造服务集合。
func NewServices(d Deps) *ServiceSet {
	return &ServiceSet{
		Settings: &SettingsService{cfg: d.Cfg, dir: d.ConfigDir, d: d},
		Chat:     &ChatService{d: d},
		Transfer: &TransferService{d: d},
		Peer:     &PeerService{d: d},
		Group:    &GroupService{d: d},
		Notify:   &NotifyService{d: d},
		Share:    &ShareService{d: d},
	}
}

// SetAutostartFuncs 设置自动启动函数（由组合根在 app 创建后调用）
func SetAutostartFuncs(enable, disable func() error) {
	autostartFuncs.enable = enable
	autostartFuncs.disable = disable
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
		MaxFileSize:     c.Transfer.MaxFileSize,
		MaxActiveConns:  c.Connection.MaxActiveConns,
		IdleTimeoutSec:  int(c.Connection.IdleConnTimeout.Std() / time.Second),
		RequireAuth:     c.Security.RequireAuth,
		Encryption:      c.Security.Encryption,

		NotifySound:          c.Notify.Sound,
		NotifyMinimizeToTray: c.Notify.MinimizeToTray,
		AutoStart:            c.Startup.AutoStart,
	}
}

// Save 落盘配置。返回需要重启才能生效的项清单。
func (s *SettingsService) Save(in SettingsDTO) ([]string, error) {
	c := s.cfg

	c.General.DisplayName = in.DisplayName
	c.Download.DefaultDir = in.DownloadDir
	c.Download.AutoOpen = in.AutoOpen
	c.Startup.AutoStart = in.AutoStart
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
	// 提示开关【无条件】赋值：bool 没有「未填」这一说，跳过赋值会让
	// 设置页永远关不掉提示音。前端产物是嵌进同一个二进制发布的，
	// 不存在「老前端不带这两个字段」的版本错配。
	c.Notify.Sound = in.NotifySound
	c.Notify.MinimizeToTray = in.NotifyMinimizeToTray

	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := config.Save(s.dir, c); err != nil {
		return nil, err
	}

	// 自动启动设置变更时同步到系统（配置文件已保存，这里只同步系统设置）
	if err := s.syncAutostart(in.AutoStart); err != nil {
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

// syncAutostart 同步自动启动设置到系统（不修改配置文件）
func (s *SettingsService) syncAutostart(enable bool) error {
	if autostartFuncs.enable == nil || autostartFuncs.disable == nil {
		return nil
	}
	if enable {
		return autostartFuncs.enable()
	}
	return autostartFuncs.disable()
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

// SendFileMessage 发送文件消息（通知聊天窗口有文件传输）。
//
// jobId 是传输任务的 ID，fileName 是文件名。这条消息不携带文件本身，
// 只是通知对端「我正在发送一个文件，你可以在传输面板查看进度」。
func (s *ChatService) SendFileMessage(peerID, jobID, fileName string) (MessageDTO, error) {
	id, err := identity.ParseNodeID(peerID)
	if err != nil {
		return MessageDTO{}, fmt.Errorf("无效的 NodeID: %w", err)
	}
	if jobID == "" {
		return MessageDTO{}, fmt.Errorf("缺少传输任务 id")
	}
	if fileName == "" {
		return MessageDTO{}, fmt.Errorf("缺少文件名")
	}
	msg, err := s.d.Chat.SendFile(context.Background(), id, jobID, fileName)
	if err != nil {
		return MessageDTO{}, err
	}
	return ToMessageDTO(msg), nil
}

// SendFolderMessage 发送文件夹消息（通知对端「我正在发送一个文件夹」）。
//
// 与 SendFileMessage 同构：这条消息不携带文件本身、也不携带发送方的本地路径，
// 只让两端聊天框出现【一张】聚合卡片（几百个文件不能塞几百张卡片）。
// fileCount / totalSize / skipped 由发送端枚举后给出，接收端直接显示同一组数字。
func (s *ChatService) SendFolderMessage(
	peerID, batchID, folderName string,
	fileCount int,
	totalSize int64,
	skipped int,
) (MessageDTO, error) {
	id, err := identity.ParseNodeID(peerID)
	if err != nil {
		return MessageDTO{}, fmt.Errorf("无效的 NodeID: %w", err)
	}
	if batchID == "" {
		return MessageDTO{}, fmt.Errorf("缺少批次 id")
	}
	if folderName == "" {
		return MessageDTO{}, fmt.Errorf("缺少文件夹名")
	}
	msg, err := s.d.Chat.SendFolder(
		context.Background(), id, batchID, folderName, fileCount, totalSize, skipped,
	)
	if err != nil {
		return MessageDTO{}, err
	}
	return ToMessageDTO(msg), nil
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

// ClearConversation 清空一个会话的聊天区：消息不删除，只是不再显示。
//
// 参数是 convID 而不是 peerID：清空点是会话级概念，单聊与群聊无差别，
// 而前端手里就是 convId（它也可能是群 ID）。
func (s *ChatService) ClearConversation(convID string) error {
	return s.d.Chat.ClearConversation(context.Background(), convID)
}

// RestoreConversation 撤销清空，让被隐藏的消息重新显示（幂等）。
func (s *ChatService) RestoreConversation(convID string) error {
	return s.d.Chat.RestoreConversation(context.Background(), convID)
}

// ClearedAt 返回会话清空点的毫秒时间戳；0 表示未清空。
//
// 前端据此决定「查看历史」按钮是否可点（0 = 没有可恢复的东西）。
func (s *ChatService) ClearedAt(convID string) (int64, error) {
	return s.d.Chat.ClearedAt(context.Background(), convID)
}

// Conversations 返回会话列表（按最近消息时间倒序）。
//
// 目前直接从节点目录 + 消息推导，避免为 UI 单独维护一张表。
func (s *ChatService) Conversations() ([]ConversationDTO, error) {
	out := make([]ConversationDTO, 0)

	// 会话列表由节点目录派生：被「本地移除」的节点不会出现在这里（List 默认排除 blocked），
	// 它的会话也随之从列表消失 —— 这是刻意的（用户要的就是「列表里别再有它」）。
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

// SendFolder 发送一个文件夹：枚举与落库在返回前完成（只 stat、不哈希），
// 传输由 app 层的排队机制驱动，因此返回后界面上会立刻出现一条文件夹卡片
// 与一批「排队中」的任务。
//
// batchID 在这里生成（随机、全局唯一）：它不是文件夹名 —— 两个对端各发一个
// 「照片」时，同名 batch 会让卡片把两个文件夹合并、整体取消也连带停掉另一批。
func (s *TransferService) SendFolder(peerID, path string) (FolderDTO, error) {
	id, err := identity.ParseNodeID(peerID)
	if err != nil {
		return FolderDTO{}, fmt.Errorf("无效的 NodeID: %w", err)
	}
	if path == "" {
		return FolderDTO{}, fmt.Errorf("请选择文件夹")
	}
	// 最廉价的检查同步做：路径打错、或把文件当文件夹选中，是这里最常见的
	// 失误 —— 当场报错比让用户对着一个不会出现的批次猜要好。
	st, err := os.Stat(path)
	if err != nil {
		return FolderDTO{}, fmt.Errorf("无法访问: %w", err)
	}
	if !st.IsDir() {
		return FolderDTO{}, fmt.Errorf("请选择文件夹（%s 是一个文件）", path)
	}

	sum, err := s.d.Transfer.SendFolderAs(context.Background(), id, path, message.NewID())
	if err != nil {
		return FolderDTO{}, err
	}
	return FolderDTO{
		BatchID:    sum.BatchID,
		FolderName: sum.FolderName,
		FileCount:  sum.FileCount,
		TotalSize:  sum.TotalSize,
		Skipped:    sum.Skipped,
	}, nil
}

// SendFolderTo 把同一个文件夹发给多个对端（群聊用），全批共享一个 batch_id。
//
// 与 SendFolder 的差别只有「一次发给谁」：群聊里聊天框只出现一张卡片，
// 因此所有成员的任务必须落在同一个批次下，否则卡片进度会按「文件数 × 成员数」算。
func (s *TransferService) SendFolderTo(peerIDs []string, path string) (FolderDTO, error) {
	if len(peerIDs) == 0 {
		return FolderDTO{}, fmt.Errorf("没有可发送的成员")
	}
	if path == "" {
		return FolderDTO{}, fmt.Errorf("请选择文件夹")
	}
	ids := make([]identity.NodeID, 0, len(peerIDs))
	for _, raw := range peerIDs {
		id, err := identity.ParseNodeID(raw)
		if err != nil {
			return FolderDTO{}, fmt.Errorf("无效的 NodeID %q: %w", raw, err)
		}
		ids = append(ids, id)
	}
	st, err := os.Stat(path)
	if err != nil {
		return FolderDTO{}, fmt.Errorf("无法访问: %w", err)
	}
	if !st.IsDir() {
		return FolderDTO{}, fmt.Errorf("请选择文件夹（%s 是一个文件）", path)
	}

	sum, err := s.d.Transfer.SendFolderTo(context.Background(), ids, path, message.NewID())
	if err != nil {
		return FolderDTO{}, err
	}
	return FolderDTO{
		BatchID:    sum.BatchID,
		FolderName: sum.FolderName,
		FileCount:  sum.FileCount,
		TotalSize:  sum.TotalSize,
		Skipped:    sum.Skipped,
	}, nil
}

// CancelBatch 取消整个文件夹批次里未结束的任务（含正在传输的那个），返回取消条数。
func (s *TransferService) CancelBatch(batchID string) (int, error) {
	if batchID == "" {
		return 0, fmt.Errorf("请选择批次")
	}
	return s.d.Transfer.CancelBatch(batchID)
}

// ListBatch 返回一个批次的全部任务（文件夹卡片上的聚合进度用）。
//
// 不复用 List()：那个走 ListRecent 的窗口（默认 50 条），一个几百个文件的
// 文件夹会算出缺斤少两的进度。
func (s *TransferService) ListBatch(batchID string) ([]TransferDTO, error) {
	if batchID == "" {
		return nil, fmt.Errorf("请选择批次")
	}
	jobs, err := s.d.TransferRepo.ListByBatch(batchID)
	if err != nil {
		return nil, err
	}
	out := make([]TransferDTO, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, ToTransferDTO(j))
	}
	return out, nil
}

// CancelTransfer 取消一条尚未开始的传输任务（等待中的那种）。
//
// 正在传输的任务也能取消：sender 有中断点，状态由发送路径落成 cancelled，
// 半截临时文件【保留】以便下次按位图续传。
// 仍会被拒绝的只有一种：仓储说「在传」、实际没有在跑的 goroutine（崩溃陈行）
// —— 接受它只会给出一个假动作。
func (s *TransferService) CancelTransfer(jobID string) error {
	if jobID == "" {
		return fmt.Errorf("请选择任务")
	}
	return s.d.Transfer.CancelJob(jobID)
}

// RetryTransfer 立刻重试一条等待中的传输任务（不等退避，也不要求对端在目录里在线）。
func (s *TransferService) RetryTransfer(jobID string) error {
	if jobID == "" {
		return fmt.Errorf("请选择任务")
	}
	return s.d.Transfer.RetryNow(context.Background(), jobID)
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
		sess, connected := s.d.Conns.SessionOf(p.NodeID)
		out = append(out, ToPeerDTO(p, connected, sessionRTTMs(sess, connected)))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return out[i].DisplayName < out[j].DisplayName
	})
	return out
}

// sessionRTTMs 把会话的往返延迟折成毫秒；没有会话或尚未测到一律返回 0（= 无数据）。
//
// 两处细节都是踩过的坑：
//   - 必须用 connected 判断，不能只判 sess == nil —— ConnManager.SessionOf 在
//     未命中时返回的是「装了 nil *session 的非 nil 接口」，判 nil 恒为假，
//     接着调 sess.RTT() 就会在 nil 接收者上取锁崩掉；
//   - RTT 为 0 时不返回 0ms：会话刚建立、心跳还没跑完一轮时它就是 0，
//     那是「还没测到」，与「真的没有延迟」是两回事。
func sessionRTTMs(sess ports.Session, connected bool) int64 {
	if !connected || sess == nil {
		return 0
	}
	d := sess.RTT()
	if d <= 0 {
		return 0
	}
	return int64(d / time.Millisecond)
}

// Remove 从本机列表里移除一个节点（设计文档 §5）。
//
// 三条必须让用户知道的边界：
//   - **只在本机生效**：不发任何网络通告，对方与别人的列表都不受影响；
//   - **不是拉黑**：不拒绝握手，将来做真拉黑要另加一层语义（设计 §6.5）；
//   - **并非永久**：该节点若再次被验证到（有效签名通告或握手成功）会自动回到列表，
//     因为那证明它没死（app.PeerApp 的恢复触发点，见设计 D7）。
//
// 幂等：列表里没有这个 node_id 时返回 nil 而不是报错 —— 用户的意图（「列表里别再有它」）
// 已经满足，报错只会让 UI 弹一个没有意义的失败。
func (s *PeerService) Remove(nodeID string) error {
	id, err := identity.ParseNodeID(nodeID)
	if err != nil {
		// 中文错误：这条会 reject 到前端并可能被直接展示（与本文件其它用户可见错误一致）
		return fmt.Errorf("无效的节点 NodeID %q: %w", nodeID, err)
	}
	if _, ok := s.d.PeerDir.Get(id); !ok {
		return nil
	}
	return s.d.PeerDir.SetSuppressed(id, true, time.Now())
}

// Diagnostics 返回诊断面板数据。
//
// 「常驻连接数」与「无保活流量」是 ADR-009 的两条可观测断言，
// 因此它们必须出现在诊断面板上，而不是只存在于日志里。
func (s *PeerService) Diagnostics() DiagnosticsDTO {
	// IncludeBlocked：诊断面板要看到目录的【真实规模】，包含用户已从列表移除的条目 ——
	// 否则「列表 4 条、目录其实 9 条」这种幽灵堆积在诊断里完全看不出来。
	all := s.d.PeerDir.List(ports.PeerFilter{IncludeBlocked: true})
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

// RemoveMember 由群主把成员移出群（epoch+1 并广播）。
func (s *GroupService) RemoveMember(groupID, memberID string) (GroupDTO, error) {
	id, err := identity.ParseNodeID(memberID)
	if err != nil {
		return GroupDTO{}, fmt.Errorf("无效的成员 NodeID: %w", err)
	}
	g, err := s.d.Group.RemoveMember(context.Background(), groupID, id)
	if err != nil {
		return GroupDTO{}, err
	}
	return toGroupDTO(g, s.d.Self.NodeID), nil
}

// Leave 退出群（本人）。本地立即生效，不依赖群主在线 ——
// 退出权属于本人，把它绑在「群主在线」上正是原设计的硬伤（ADR-012）。
func (s *GroupService) Leave(groupID string) (GroupDTO, error) {
	if groupID == "" {
		return GroupDTO{}, fmt.Errorf("缺少群 id")
	}
	g, err := s.d.Group.Leave(context.Background(), groupID)
	if err != nil {
		return GroupDTO{}, err
	}
	return toGroupDTO(g, s.d.Self.NodeID), nil
}

// Rename 由群主修改群名（epoch+1 并广播）。
func (s *GroupService) Rename(groupID, name string) (GroupDTO, error) {
	g, err := s.d.Group.Rename(context.Background(), groupID, name)
	if err != nil {
		return GroupDTO{}, err
	}
	return toGroupDTO(g, s.d.Self.NodeID), nil
}

// Dissolve 由群主解散群（epoch+1 并广播；成员侧收到后停止收发）。
func (s *GroupService) Dissolve(groupID string) (GroupDTO, error) {
	g, err := s.d.Group.Dissolve(context.Background(), groupID)
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
	return s.withStats(ToMessageDTO(msg), groupID), nil
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
	return s.withStats(ToMessageDTO(msg), groupID), nil
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
	return s.withStats(ToMessageDTO(msg), groupID), nil
}

// SendFile 群发文件消息（在群里放一张文件卡片）。
//
// 这条消息只承载卡片：文件字节由前端对每个在线成员各自发起点对点传输，
// 因此「谁收到了文件」与群消息的送达统计是两件独立的事。
func (s *GroupService) SendFile(groupID, jobID, fileName string) (MessageDTO, error) {
	if groupID == "" {
		return MessageDTO{}, fmt.Errorf("缺少群 id")
	}
	if jobID == "" {
		return MessageDTO{}, fmt.Errorf("缺少传输任务 id")
	}
	if fileName == "" {
		return MessageDTO{}, fmt.Errorf("缺少文件名")
	}
	msg, err := s.d.Group.SendGroupFile(context.Background(), groupID, jobID, fileName)
	if err != nil {
		return MessageDTO{}, err
	}
	return s.withStats(ToMessageDTO(msg), groupID), nil
}

// SendFolder 群发文件夹消息（在群里放一张聚合卡片）。
//
// 与 SendFile 同一套路：卡片只描述「发了什么」，真正的传输是前端按
// 每个在线成员逐个发起（复用文件夹批次的 batch_id）。
func (s *GroupService) SendFolder(
	groupID, batchID, folderName string,
	fileCount int,
	totalSize int64,
	skipped int,
) (MessageDTO, error) {
	if groupID == "" {
		return MessageDTO{}, fmt.Errorf("缺少群 id")
	}
	if batchID == "" {
		return MessageDTO{}, fmt.Errorf("缺少批次 id")
	}
	msg, err := s.d.Group.SendGroupFolder(context.Background(), groupID, batchID, folderName, fileCount, totalSize, skipped)
	if err != nil {
		return MessageDTO{}, err
	}
	return s.withStats(ToMessageDTO(msg), groupID), nil
}

// History 返回群历史消息（新→旧）。
//
// 一次性取回整个会话的送达统计再填入，避免每条消息一次查询。
func (s *GroupService) History(groupID string, limit int, before int64) ([]MessageDTO, error) {
	if limit <= 0 {
		limit = 50
	}
	ms, err := s.d.Chat.History(message.GroupConvID(groupID), limit, before)
	if err != nil {
		return nil, err
	}
	stats := s.statsOf(groupID)
	out := make([]MessageDTO, 0, len(ms))
	for _, m := range ms {
		out = append(out, fillDeliveryStat(ToMessageDTO(m), stats))
	}
	return out, nil
}

// statsOf 取某群的送达统计；没有投递仓储或查询失败时返回 nil（表示不附加统计）。
func (s *GroupService) statsOf(groupID string) map[string]ports.DeliveryStat {
	if s.d.Deliveries == nil {
		return nil
	}
	stats, err := s.d.Deliveries.StatsByConv(message.GroupConvID(groupID))
	if err != nil {
		return nil
	}
	return stats
}

// withStats 给一条群消息补上送达统计。
func (s *GroupService) withStats(dto MessageDTO, groupID string) MessageDTO {
	return fillDeliveryStat(dto, s.statsOf(groupID))
}

// fillDeliveryStat 把统计填进 DTO；该消息没有记录（已送达/非群消息）则原样返回。
func fillDeliveryStat(dto MessageDTO, stats map[string]ports.DeliveryStat) MessageDTO {
	if st, ok := stats[dto.MsgID]; ok {
		dto.MemberCount = st.Total
		dto.PendingCount = st.Pending()
	}
	return dto
}

func toGroupDTO(g group.Group, self identity.NodeID) GroupDTO {
	members := make([]GroupMemberDTO, 0, len(g.Members))
	for _, m := range g.Members {
		members = append(members, GroupMemberDTO{
			NodeID:      m.NodeID.String(),
			DisplayName: m.DisplayName,
			Role:        string(m.Role),
			State:       string(m.State),
			// Active 由后端按票号算好：前端拿它做判据，而不是拿 state 缓存推。
			Active:   m.Active(),
			AdmitSeq: m.AdmitSeq,
			KickSeq:  m.KickSeq,
			LeftSeq:  m.LeftSeq,
		})
	}
	return GroupDTO{
		GroupID:   g.ID,
		Name:      g.Name,
		OwnerID:   g.OwnerID.String(),
		Epoch:     g.Epoch,
		MintSeq:   g.MintSeq,
		State:     string(g.State.StateOrActive()),
		IsOwner:   g.OwnerID == self,
		Members:   members,
		CreatedAt: g.CreatedAt.UnixMilli(),
	}
}

// ---------------------------------------------------------------------------
// NotifyService
// ---------------------------------------------------------------------------

// NotifyService 只承载「前端告诉 Go 当前在看哪个会话」这一件事。
//
// 为什么不塞进 ChatService：它和聊天读写无关，只是提示决策的一个环境输入；
// 挂在 ChatService 上会让「聊天」这个已经很重的服务再多一个不相关的职责。
// 窗口可见性与焦点由 Go 侧直接读 Wails，不需要前端上报。
type NotifyService struct{ d Deps }

// SetActiveConversation 记录前端当前打开的会话 id（未打开任何会话时传空串）。
func (s *NotifyService) SetActiveConversation(convID string) error {
	if s.d.SetActiveConv == nil {
		return nil // 未注入（例如单测）：静默忽略，不影响前端调用
	}
	s.d.SetActiveConv(convID)
	return nil
}

// ---------------------------------------------------------------------------
// ShareService
// ---------------------------------------------------------------------------

// ShareService 暴露文件共享（FR-14）能力。
//
// 与 TransferService 的分工：这里只负责「发布什么、谁能看、要哪些文件」，
// 字节搬运与进度一律复用传输服务与 transfer:* 事件 —— 界面因此不需要
// 为共享下载再写一套进度 UI。
type ShareService struct{ d Deps }

// List 返回共享清单：本机发布的在前，他人发布的在后。
//
// 两个分组一次返回而不是分两个方法：界面同一屏要展示它们，分两次调用
// 会出现两批数据来自不同时刻的经典问题（用户会看到「我发布的没了」
// 这种一闪）。
func (s *ShareService) List() ([]ShareDTO, error) {
	if s.d.Share == nil {
		return nil, fmt.Errorf("共享功能未启用")
	}
	mine := s.d.Share.Mine()
	remote := s.d.Share.Remote()

	out := make([]ShareDTO, 0, len(mine)+len(remote))
	for _, m := range mine {
		out = append(out, ToShareDTO(m))
	}
	for _, r := range remote {
		out = append(out, ToRemoteShareDTO(r))
	}
	return out, nil
}

// Publish 发布一条共享。
//
// 可见性用 (visType, allowed) 两个参数表达而不是传一个结构体：Wails 的
// 绑定生成对「结构体参数」支持是好的，但可见性的两个字段有强耦合的
// 取值规则（all 忽略 allowed、group 只允许一个），拆成参数能让前端的
// 调用点一眼看出这条规则。
func (s *ShareService) Publish(rootPath, name, visType string, allowed []string) (ShareDTO, error) {
	if s.d.Share == nil {
		return ShareDTO{}, fmt.Errorf("共享功能未启用")
	}
	if strings.TrimSpace(rootPath) == "" {
		return ShareDTO{}, fmt.Errorf("请选择要共享的目录或文件")
	}
	vis := share.Visibility{Type: share.VisibilityType(visType), Allowed: allowed}.Normalize()
	if err := vis.Validate(); err != nil {
		return ShareDTO{}, err
	}
	sh, err := s.d.Share.Publish(context.Background(), app.PublishRequest{
		RootPath: rootPath, Name: name, Visibility: vis,
	})
	if err != nil {
		return ShareDTO{}, err
	}
	return ToShareDTO(sh), nil
}

// Unpublish 取消一条本机发布的共享。
func (s *ShareService) Unpublish(shareID string) error {
	if s.d.Share == nil {
		return fmt.Errorf("共享功能未启用")
	}
	if shareID == "" {
		return fmt.Errorf("请选择要取消的共享")
	}
	return s.d.Share.Unpublish(shareID)
}

// Refresh 重新扫描共享根目录（磁盘内容变了之后用）。
func (s *ShareService) Refresh(shareID string) (ShareDTO, error) {
	if s.d.Share == nil {
		return ShareDTO{}, fmt.Errorf("共享功能未启用")
	}
	if shareID == "" {
		return ShareDTO{}, fmt.Errorf("请选择要刷新的共享")
	}
	sh, err := s.d.Share.RefreshIndex(context.Background(), shareID)
	if err != nil {
		return ShareDTO{}, err
	}
	return ToShareDTO(sh), nil
}

// SetVisibility 修改一条本机共享的可见性。
//
// 与 Publish 同一套可见性取值规则（避免前端在两个入口里各写一遍判断），
// 差别只是它作用在已存在的那条共享上 —— 因此不需要新发布、也不会换 share_id。
func (s *ShareService) SetVisibility(shareID, visType string, allowed []string) (ShareDTO, error) {
	if s.d.Share == nil {
		return ShareDTO{}, fmt.Errorf("共享功能未启用")
	}
	if strings.TrimSpace(shareID) == "" {
		return ShareDTO{}, fmt.Errorf("请选择要修改可见性的共享")
	}
	vis := share.Visibility{Type: share.VisibilityType(visType), Allowed: allowed}.Normalize()
	if err := vis.Validate(); err != nil {
		return ShareDTO{}, err
	}
	sh, err := s.d.Share.SetVisibility(shareID, vis)
	if err != nil {
		return ShareDTO{}, err
	}
	return ToShareDTO(sh), nil
}

// Browse 返回一条共享的文件清单（可浏览自己的，也可浏览他人的）。
//
// 他人的清单是现拉现取的（SHARE_INDEX_REQ）：摘要里不带清单，因为清单
// 体积随目录大小增长，而摘要是每 45 秒广播一次的东西。
func (s *ShareService) Browse(shareID string) ([]ShareItemDTO, error) {
	if s.d.Share == nil {
		return nil, fmt.Errorf("共享功能未启用")
	}
	if shareID == "" {
		return nil, fmt.Errorf("请选择共享")
	}
	items, err := s.d.Share.Items(context.Background(), shareID)
	if err != nil {
		return nil, err
	}
	out := make([]ShareItemDTO, 0, len(items))
	for _, it := range items {
		out = append(out, ToShareItemDTO(it))
	}
	return out, nil
}

// Download 请共享方把指定文件推过来（异步传输：返回即代表已受理）。
//
// 返回的 batchId 会出现在随后每一条相关任务的 transfer:* 事件里，
// 界面据此把整批聚合成一张卡片 —— 与文件夹发送是同一套机制。
func (s *ShareService) Download(shareID string, paths []string) (ShareDownloadDTO, error) {
	if s.d.Share == nil {
		return ShareDownloadDTO{}, fmt.Errorf("共享功能未启用")
	}
	if shareID == "" {
		return ShareDownloadDTO{}, fmt.Errorf("请选择共享")
	}
	if len(paths) == 0 {
		return ShareDownloadDTO{}, fmt.Errorf("请选择要下载的文件")
	}
	res, err := s.d.Share.Download(context.Background(), shareID, paths)
	if err != nil {
		return ShareDownloadDTO{}, err
	}
	return ShareDownloadDTO{
		BatchID: res.BatchID, Accepted: res.Accepted, Rejected: res.Rejected,
	}, nil
}
