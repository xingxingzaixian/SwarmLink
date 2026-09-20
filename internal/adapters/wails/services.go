package wails

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/config"
	"github.com/swarmlink/swarmlink/internal/app"
	"github.com/swarmlink/swarmlink/internal/domain/group"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
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

	Chat     *app.ChatApp
	Transfer *app.TransferApp
	Group    *app.GroupApp
	Peers    *app.PeerApp

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
func (s *TransferService) SendFile(peerID, path string) (string, error) {
	id, err := identity.ParseNodeID(peerID)
	if err != nil {
		return "", fmt.Errorf("无效的 NodeID: %w", err)
	}
	if path == "" {
		return "", fmt.Errorf("请选择文件")
	}

	jobID := message.NewID()
	go func() {
		// 结果通过 transfer.done / transfer.error 事件通知前端；
		// 这里必须脱离请求生命周期，否则前端切页会中断传输。
		_, _ = s.d.Transfer.SendFile(context.Background(), id, path)
	}()
	return jobID, nil
}

// List 返回全部（含已结束的）传输任务。
func (s *TransferService) List() ([]TransferDTO, error) {
	jobs, err := s.d.TransferRepo.ListActive()
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
