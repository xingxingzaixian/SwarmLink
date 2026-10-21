// Package wails 是 L4 入站适配器：把应用层能力暴露给 Wails 绑定的前端。
//
// 关键设计：本包【不 import Wails】。
//   - 服务只是普通结构体，方法返回 JSON 友好的 DTO；
//   - 事件通过 Emitter 接口输出，Wails 的具体桥接在根目录 main_wails.go 里注入。
//
// 这样做的收益：服务层可以在没有 Wails 工具链的环境下编译与单测，
// 而 Wails beta 期 API 波动只影响一个薄薄的装配文件。
package wails

import (
	"github.com/swarmlink/swarmlink/internal/app"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/share"
	"github.com/swarmlink/swarmlink/internal/domain/transfer"
)

// MessageDTO 是消息的对外表示。
type MessageDTO struct {
	MsgID     string `json:"msgId"`
	ConvID    string `json:"convId"`
	SenderID  string `json:"senderId"`
	Direction string `json:"direction"`
	Content   string `json:"content"`
	MsgType   string `json:"msgType"`
	FileID    string `json:"fileId,omitempty"`
	JobID     string `json:"jobId,omitempty"`
	State     string `json:"state"`
	SentAt    int64  `json:"sentAt"`
	RecvAt    int64  `json:"recvAt,omitempty"`

	// 群消息的送达统计（单聊恒为 0，omitempty 让单聊的 JSON 保持不变）。
	//
	// PendingCount>0 表示还有成员没确认送达（离线或尚未连接），界面据此显示
	// 「N 人未送达」。消息一旦全员送达，投递记录被清理，这里回到 0。
	MemberCount  int `json:"memberCount,omitempty"`
	PendingCount int `json:"pendingCount,omitempty"`
}

// ToMessageDTO 转换领域消息。
func ToMessageDTO(m message.Message) MessageDTO {
	return MessageDTO{
		MsgID:     m.MsgID,
		ConvID:    m.ConvID,
		SenderID:  m.SenderID.String(),
		Direction: string(m.Direction),
		Content:   m.Content,
		MsgType:   string(m.MsgType),
		FileID:    m.FileID,
		State:     string(m.State),
		SentAt:    m.SentAt.UnixMilli(),
		RecvAt:    m.RecvAt.UnixMilli(),
	}
}

// ToMessageDTOs 批量转换领域消息。
//
// 列表类返回值（历史、搜索结果、跳转窗口）统一走它，不要在调用点各写一遍
// for 循环：那种重复在「消息新增一个字段」时是最典型的漏改点。
func ToMessageDTOs(ms []message.Message) []MessageDTO {
	out := make([]MessageDTO, 0, len(ms))
	for _, m := range ms {
		out = append(out, ToMessageDTO(m))
	}
	return out
}

// ConversationDTO 是会话列表项。
type ConversationDTO struct {
	ConvID   string `json:"convId"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	PeerID   string `json:"peerId,omitempty"`
	State    string `json:"state,omitempty"`
	IsOnline bool   `json:"isOnline"`
	LastTime int64  `json:"lastTime"`
}

// PeerDTO 是节点列表项。
//
// 注意：UI 必须同时展示「节点在线」（announce 维护）与「连接可用」（TCP 维护），
// 否则用户会看到「在线但发不出消息」而困惑（ADR-009 的代价之一）。
type PeerDTO struct {
	NodeID      string `json:"nodeId"`
	ShortID     string `json:"shortId"`
	DisplayName string `json:"displayName"`
	State       string `json:"state"`
	LastAddr    string `json:"lastAddr"`
	Subnet      string `json:"subnet"`
	Source      string `json:"source"`
	LastSeen    int64  `json:"lastSeen"`
	Online      bool   `json:"online"`
	Connected   bool   `json:"connected"`

	// RTTMs 是最近一次心跳测得的往返延迟（毫秒）。0 表示【没有数据】：
	// 当前没有会话，或会话刚建立、心跳还没跑完一轮。
	//
	// 界面必须把「0」显示成「测量中」而不是「0ms」—— 后者是神仙延迟，
	// 拿没数据冒充 0 是编造。
	RTTMs int64 `json:"rttMs"`

	// AppVersion 是对方自述的应用版本，空串表示「对方没报」（旧版本客户端）。
	// 它是「局域网更新」判断谁更新的唯一输入，因此必须给到界面。
	AppVersion string `json:"appVersion,omitempty"`
}

// ToPeerDTO 转换领域节点；connected 与 rttMs 均由连接层提供（rttMs 无数据时传 0）。
func ToPeerDTO(p peer.Peer, connected bool, rttMs int64) PeerDTO {
	short := p.NodeID.String()
	if len(short) > 8 {
		short = short[:8]
	}
	name := p.DisplayName
	if name == "" {
		name = short
	}
	return PeerDTO{
		NodeID:      p.NodeID.String(),
		ShortID:     short,
		DisplayName: name,
		State:       string(p.State),
		LastAddr:    p.LastAddr,
		Subnet:      p.Subnet,
		Source:      p.Source,
		LastSeen:    p.LastSeen.UnixMilli(),
		Online:      p.State == peer.StateOnline,
		Connected:   connected,
		RTTMs:       rttMs,
		AppVersion:  p.AppVersion,
	}
}

// TransferDTO 是传输任务项。
type TransferDTO struct {
	JobID       string  `json:"jobId"`
	PeerID      string  `json:"peerId"`
	FileName    string  `json:"fileName"`
	FileSize    int64   `json:"fileSize"`
	Direction   string  `json:"direction"`
	LocalPath   string  `json:"localPath"`
	Completed   int64   `json:"completed"`
	TotalChunks int     `json:"totalChunks"`
	Percent     float64 `json:"percent"`
	// ResumeFromPercent 是【本次尝试的起点】百分比：>0 表示这条是从上次的
	// 半截续传的，界面显示「续传自 XX%」；0（omitempty 直接省掉）表示从头传。
	//
	// 与 Percent 并列而不是塞进 percent：两者回答的是不同的问题 ——
	// 「现在到哪儿了」与「这次从哪儿接上」。
	ResumeFromPercent float64 `json:"resumeFromPercent,omitempty"`
	Status            string  `json:"status"`
	// WaitReason 非空表示这条任务在等：`peer_offline`（等对端上线）或
	// `batch_queued`（文件夹批次里排队等前一个传完）。
	// 界面靠它区分「等待对方上线」与「排队中」—— 两者在 status 上都是 idle/queued。
	WaitReason string `json:"waitReason,omitempty"`
	// 文件夹批次：BatchID 非空时 RelPath 是它在目录树里的位置（含根名）。
	// 传输列表靠 RelPath 区分同名的文件（a/dup.txt 与 b/dup.txt）。
	BatchID   string `json:"batchId,omitempty"`
	RelPath   string `json:"relPath,omitempty"`
	IsDir     bool   `json:"isDir,omitempty"`
	Error     string `json:"error,omitempty"`
	UpdatedAt int64  `json:"updatedAt"`
}

// ToTransferDTO 转换领域任务。
func ToTransferDTO(j transfer.Job) TransferDTO {
	return TransferDTO{
		JobID:       j.JobID,
		PeerID:      j.PeerID.String(),
		FileName:    j.FileName,
		FileSize:    j.FileSize,
		Direction:   string(j.Direction),
		LocalPath:   j.LocalPath,
		Completed:   j.Completed,
		TotalChunks: j.TotalChunks,
		Percent:     j.Percent(),
		// 起点百分比由领域层算（ResumeFrom / TotalChunks）：界面与用例看的是
		// 同一个数，不在装配层再取一次整。
		ResumeFromPercent: j.ResumePercent(),
		Status:            string(j.Status),
		WaitReason:        string(j.WaitReason),
		BatchID:           j.BatchID,
		RelPath:           j.RelPath,
		IsDir:             j.IsDir,
		Error:             j.Error,
		UpdatedAt:         j.UpdatedAt.UnixMilli(),
	}
}

// FolderDTO 是一次文件夹发送的摘要（聊天框那条卡片的数据）。
//
// 数字由【发送端枚举后】给出并随消息投递给对端：接收端自己数会出现
// 「一端写 32 个、另一端写 30 个」这种对不上的情况。
type FolderDTO struct {
	BatchID    string `json:"batchId"`
	FolderName string `json:"folderName"`
	FileCount  int    `json:"fileCount"`
	TotalSize  int64  `json:"totalSize"`
	Skipped    int    `json:"skipped"`
}

// ---------------------------------------------------------------------------
// 文件共享（FR-14）
// ---------------------------------------------------------------------------

// ShareVisibilityDTO 是共享可见性。
type ShareVisibilityDTO struct {
	// Type 取值：all | users | group。
	Type string `json:"type"`
	// Allowed 是 node_id（users）或群 id（group）列表。
	Allowed []string `json:"allowed,omitempty"`
}

// ShareDTO 是共享列表项：本机发布的与他人发布的共用一种形状。
//
// 为什么共用一个 DTO 而不是两份：界面上的两个分组渲染的是同一张卡片，
// 差异只有「能不能下载 / 能不能取消」两个按钮位 —— 拆成两个类型会立刻
// 出现「改了一边忘了另一边」的不对称。
type ShareDTO struct {
	ShareID   string `json:"shareId"`
	Name      string `json:"name"`
	OwnerID   string `json:"ownerId"`
	OwnerName string `json:"ownerName"`
	FileCount int    `json:"fileCount"`
	TotalSize int64  `json:"totalSize"`

	Visibility ShareVisibilityDTO `json:"visibility"`
	// VisibilityText 是给人看的可见性文案（后端算，保证两处一致）。
	VisibilityText string `json:"visibilityText"`

	// Mine=true 表示这是【本机发布】的共享。
	Mine bool `json:"mine"`
	// RootPath 仅本机共享有值（本地根路径，界面「打开所在文件夹」用）。
	RootPath string `json:"rootPath,omitempty"`

	// Online 仅他人共享有意义：发布者此刻在目录里在线且地址可用。
	Online bool `json:"online"`
	// Downloadable 是后端算好的「现在点下载会成功吗」。
	//
	// 必须由后端给结论：它同时取决于 Mine / Online / 可见性，让前端把三个
	// 字段组合一遍就等于把一条判定规则复制到第二个地方，而两者迟早不一致。
	Downloadable bool `json:"downloadable"`

	CreatedAt int64 `json:"createdAt"`
	UpdatedAt int64 `json:"updatedAt"`
}

// ToShareDTO 把本机共享转成 DTO。
func ToShareDTO(s share.Share) ShareDTO {
	return ShareDTO{
		ShareID:        s.ID,
		Name:           s.Name,
		OwnerID:        s.Owner,
		OwnerName:      s.OwnerName,
		FileCount:      len(s.Items),
		TotalSize:      s.TotalSize,
		Visibility:     ToShareVisibilityDTO(s.Visibility),
		VisibilityText: s.Visibility.String(),
		Mine:           true,
		RootPath:       s.RootPath,
		Online:         true, // 自己的共享当然「在线」
		Downloadable:   false,
		CreatedAt:      s.CreatedAt.UnixMilli(),
		UpdatedAt:      s.UpdatedAt.UnixMilli(),
	}
}

// ToRemoteShareDTO 把他人共享转成 DTO。
func ToRemoteShareDTO(s app.RemoteShare) ShareDTO {
	return ShareDTO{
		ShareID:        s.ShareID,
		Name:           s.Name,
		OwnerID:        s.OwnerID,
		OwnerName:      s.OwnerName,
		FileCount:      s.FileCount,
		TotalSize:      s.TotalSize,
		Visibility:     ToShareVisibilityDTO(s.Visibility),
		VisibilityText: s.Visibility.String(),
		Mine:           false,
		Online:         s.Online,
		Downloadable:   s.Online,
	}
}

// ToShareVisibilityDTO 转可见性。
func ToShareVisibilityDTO(v share.Visibility) ShareVisibilityDTO {
	vis := v.Normalize()
	return ShareVisibilityDTO{Type: string(vis.Type), Allowed: vis.Allowed}
}

// ShareItemDTO 是共享里的一条文件。
type ShareItemDTO struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"isDir,omitempty"`
	MTime int64  `json:"mtime,omitempty"`
}

// ToShareItemDTO 转换条目。
func ToShareItemDTO(it share.Item) ShareItemDTO {
	return ShareItemDTO{Path: it.Path, Name: it.Name, Size: it.Size, IsDir: it.IsDir, MTime: it.MTime}
}

// ShareDownloadDTO 是一次共享下载的受理结果（进度走传输事件）。
type ShareDownloadDTO struct {
	BatchID  string   `json:"batchId"`
	Accepted int      `json:"accepted"`
	Rejected []string `json:"rejected,omitempty"`
}

// GroupMemberDTO 是群成员项。
//
// 票号一起暴露出去，是因为「谁还在群里」在前端也有一份判断需要与后端一致：
// 只给 state 会让前端被迫用派生缓存做判据，而缓存与票号一旦不同步，
// 就会出现「后端说已退出、前端仍显示在群」这种对不上的现象。
type GroupMemberDTO struct {
	NodeID      string `json:"nodeId"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
	State       string `json:"state"`

	// Active 是权威判据（后端按票号算好），前端不要自己用 state 推。
	Active   bool  `json:"active"`
	AdmitSeq int64 `json:"admitSeq"`
	KickSeq  int64 `json:"kickSeq"`
	LeftSeq  int64 `json:"leftSeq"`
}

// GroupDTO 是群项。
type GroupDTO struct {
	GroupID string `json:"groupId"`
	Name    string `json:"name"`
	OwnerID string `json:"ownerId"`
	Epoch   int64  `json:"epoch"`
	// MintSeq 是群主的群级发票计数器（票号来源）。只用于展示/排障：
	// 「为什么某人的 admit_seq 是 13」要靠它解释。
	MintSeq int64 `json:"mintSeq"`
	// State 是群状态（active / dissolved）。解散后前端禁用输入并展示横幅。
	State     string           `json:"state"`
	IsOwner   bool             `json:"isOwner"`
	Members   []GroupMemberDTO `json:"members"`
	CreatedAt int64            `json:"createdAt"`
}

// SelfDTO 是本机身份。
type SelfDTO struct {
	NodeID      string `json:"nodeId"`
	DisplayName string `json:"displayName"`
	TCPPort     int    `json:"tcpPort"`
	UDPPort     int    `json:"udpPort"`
	Subnet      string `json:"subnet"`
}

// DiagnosticsDTO 是诊断面板数据。
type DiagnosticsDTO struct {
	PeersTotal       int       `json:"peersTotal"`
	PeersOnline      int       `json:"peersOnline"`
	ActiveSessions   int       `json:"activeSessions"`
	ActiveTransfers  int       `json:"activeTransfers"`
	SeedCount        int       `json:"seedCount"`
	SeedAvailable    int       `json:"seedAvailable"`
	SeedDetails      []SeedDTO `json:"seedDetails"`
	InterfaceSummary []string  `json:"interfaceSummary"`

	// SameSubnetPeers 是同网段、且发得出消息的邻居数（只计广播/组播来源）。
	//
	// 与 PeersOnline 的区别必须看清：后者包含「经种子学到的跨网段节点」，
	// 因此 PeersOnline > SameSubnetPeers 是正常的。状态灯要回答的是
	// 「我这一段是否通」与「跨网段是否通」两个独立问题。
	SameSubnetPeers int `json:"sameSubnetPeers"`

	// DiscoveryState 是状态灯结论：live / partial / limited
	// （判定规则在 app.ClassifyDiscovery，前端只负责翻译文案）。
	DiscoveryState string `json:"discoveryState"`
}

// SeedDTO 是种子状态项（含退避信息）。
type SeedDTO struct {
	Addr      string `json:"addr"`
	FailCount int    `json:"failCount"`
	NodeID    string `json:"nodeId"`
	TCPPort   int    `json:"tcpPort"`
	Subnet    string `json:"subnet"`

	// LastSeen 是最近一次探测成功的时间（毫秒，0 = 还没探过）。
	// 界面用它区分「可用」与「可用但很久没刷新」——种子列表按随机顺序探，
	// 一颗种子可能几十分钟才轮到一次，这是正常的，不该画成异常。
	LastSeen int64 `json:"lastSeen"`

	// Available 表示这颗种子当前可用（无连续失败，见 udp.SeedState.Available）。
	Available bool `json:"available"`
}

// SettingsDTO 是设置项。
type SettingsDTO struct {
	DisplayName     string   `json:"displayName"`
	DownloadDir     string   `json:"downloadDir"`
	AutoOpen        bool     `json:"autoOpen"`
	TCPPort         int      `json:"tcpPort"`
	UDPPort         int      `json:"udpPort"`
	InterfaceMode   string   `json:"interfaceMode"`
	AllowInterfaces []string `json:"allowInterfaces"`
	DenyInterfaces  []string `json:"denyInterfaces"`
	Seeds           []string `json:"seeds"`
	SeedRefreshSec  int      `json:"seedRefreshSec"`
	SeedsPerRefresh int      `json:"seedsPerRefresh"`
	MaxConcurrent   int      `json:"maxConcurrent"`
	ChunkSize       int64    `json:"chunkSize"`
	WindowSize      int      `json:"windowSize"`
	// MaxFileSize 是接收侧的单文件上限（字节），0 表示不限制。
	// 前端只用它提前拦住明显发不过去的文件，真正的判定在接收端。
	MaxFileSize    int64  `json:"maxFileSize"`
	MaxActiveConns int    `json:"maxActiveConns"`
	IdleTimeoutSec int    `json:"idleTimeoutSec"`
	RequireAuth    bool   `json:"requireAuth"`
	Encryption     string `json:"encryption"`

	// 提示行为（设置页与托盘菜单共用同一份配置，两个入口不会不一致）。
	NotifySound          bool `json:"notifySound"`
	NotifyMinimizeToTray bool `json:"notifyMinimizeToTray"`
	AutoStart            bool `json:"autoStart"`

	// 局域网更新。
	//
	// 模式用字符串而不是枚举：前端要显示三选一，而 Go 的绑定生成器
	// 对自定义字符串类型不友好（会变成 any）。
	UpdateEnabled bool   `json:"updateEnabled"`
	UpdateMode    string `json:"updateMode"`
	// UpdateAutoInstallHours 是「弹框后多久无操作自动安装」，0 表示永不。
	// 用小时而不是 Duration：设置页是数字输入框，而 TOML 里写 "4h"
	// 对运维更直观 —— 两个面各用各的自然单位，服务层做换算。
	UpdateAutoInstallHours int `json:"updateAutoInstallHours"`

	// ScreenshotEnabled 是截屏总开关（关闭后聊天窗口的截屏按钮不可用）。
	ScreenshotEnabled bool `json:"screenshotEnabled"`
	// ScreenshotLastMode 是上次使用的截屏模式：
	// keep_window（保留本窗口）| hide_window（隐藏本窗口）。
	//
	// 它只被【读】：设置页保存时不会写它 —— 写者是 ScreenshotService
	// （用户显式选过模式之后才记），两个写者会互相覆盖。
	ScreenshotLastMode string `json:"screenshotLastMode"`
}

// ScreenshotDTO 是一张抓到的屏幕截图。
type ScreenshotDTO struct {
	// MIME 是图片类型（当前恒为 image/jpeg）。
	MIME string `json:"mime"`
	// Width / Height 是图片的像素尺寸。
	Width  int `json:"width"`
	Height int `json:"height"`
	// DataBase64 是图片字节的 base64。
	//
	// 用 base64 字符串而不是 []byte：Wails 生成器把 []byte 映射成 TS 的
	// `string | null`，参数类型与运行时编码之间会留下一次「猜」的机会。
	DataBase64 string `json:"dataBase64"`
}

// OverlaySessionDTO 是一次交互式截图的会话描述。
type OverlaySessionDTO struct {
	// Displays 是每块显示器的覆盖层窗口布局。
	Displays []OverlayDisplayDTO `json:"displays"`
}

// OverlayDisplayDTO 描述一块显示器上的覆盖层窗口。
type OverlayDisplayDTO struct {
	// Index 是显示器序号（与 GetOverlayImage 的入参对应）。
	Index int `json:"index"`
	// X / Y / Width / Height 是窗口在虚拟桌面上的 DIP 位置与尺寸。
	//
	// 前端主要用它做自检（覆盖层没铺满时能看出来），实际定位由后端完成。
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
	// PhysicalWidth / PhysicalHeight 是这块屏的物理像素尺寸。
	PhysicalWidth  int `json:"physicalWidth"`
	PhysicalHeight int `json:"physicalHeight"`
	// ScaleFactor 是 DIP → 物理像素的倍率（150% 缩放 = 1.5）。
	ScaleFactor float64 `json:"scaleFactor"`
}

// OverlayImageDTO 是覆盖层要显示的底图。
type OverlayImageDTO struct {
	// MIME 是图片类型（当前恒为 image/jpeg）。
	MIME string `json:"mime"`
	// Width / Height 是底图的【物理像素】尺寸。
	Width  int `json:"width"`
	Height int `json:"height"`
	// ScaleFactor 是 DIP → 物理像素的倍率：前端据此把物理像素的画布
	// 按 DIP 尺寸摆进窗口，鼠标坐标也按它换算回物理像素。
	ScaleFactor float64 `json:"scaleFactor"`
	// DataBase64 是底图字节的 base64。
	DataBase64 string `json:"dataBase64"`
}

// PinImageDTO 是一张贴图的内容（贴图窗口按 id 取自己的那一张）。
type PinImageDTO struct {
	// MIME 是图片类型（PNG 或 JPEG，按字节头嗅探得出）。
	MIME string `json:"mime"`
	// Width / Height 是 1:1 显示时的窗口尺寸（DIP 逻辑像素）。
	//
	// 贴图窗口用它做缩放基准：窗口尺寸 = 基准尺寸 × 缩放比。
	Width  int `json:"width"`
	Height int `json:"height"`
	// DataBase64 是图片字节的 base64。
	DataBase64 string `json:"dataBase64"`
}

// UpdatePeerDTO 是「局域网里版本更高」的一个候选源。
type UpdatePeerDTO struct {
	PeerID      string `json:"peerId"`
	DisplayName string `json:"displayName"`
	Addr        string `json:"addr"`
	Version     string `json:"version"`
}

// UpdateStateDTO 是局域网更新的完整状态快照。
//
// 前端只消费这一个结构（配合单一事件），而不是拼装多个增量事件：
// 状态机有 8 个阶段，增量事件迟早会拼出「界面显示 ready 但后端已经 applying」
// 这种自相矛盾的画面。
type UpdateStateDTO struct {
	Phase          string  `json:"phase"`
	CurrentVersion string  `json:"currentVersion"`
	TargetVersion  string  `json:"targetVersion,omitempty"`
	SourcePeerID   string  `json:"sourcePeerId,omitempty"`
	SourceName     string  `json:"sourceName,omitempty"`
	SourceAddr     string  `json:"sourceAddr,omitempty"`
	Size           int64   `json:"size,omitempty"`
	Downloaded     int64   `json:"downloaded,omitempty"`
	Percent        float64 `json:"percent,omitempty"`
	SHA256         string  `json:"sha256,omitempty"`
	// ScheduledAt / AutoInstallAt 是毫秒时间戳，0 表示没有。
	ScheduledAt   int64  `json:"scheduledAt,omitempty"`
	AutoInstallAt int64  `json:"autoInstallAt,omitempty"`
	PromptPending bool   `json:"promptPending"`
	Error         string `json:"error,omitempty"`
	// PlatformSupported 为假时整页只展示说明（当前实现仅 Windows）。
	PlatformSupported bool `json:"platformSupported"`
	// CanDistribute 表示本机持有与当前版本一致的安装包，可被别人拉取。
	CanDistribute bool            `json:"canDistribute"`
	InstallerPath string          `json:"installerPath,omitempty"`
	LastCheckAt   int64           `json:"lastCheckAt,omitempty"`
	HigherPeers   []UpdatePeerDTO `json:"higherPeers,omitempty"`

	// 关于信息：出问题时运维第一个要问的三件事（版本 / 数据在哪 / 日志在哪）。
	DataDir string `json:"dataDir"`
	LogPath string `json:"logPath"`
}

// BackgroundDTO 是自定义背景图的当前状态。
//
// DataURL 用 data: URL 而不是文件路径：WebView 里从 wails:// 源直接引用
// file:/// 会被拦，绕开它要么加自定义协议、要么加本地 HTTP 服务 ——
// 为一张背景图引入一个常驻服务不值得。
type BackgroundDTO struct {
	// DataURL 形如 data:image/jpeg;base64,...；空串 = 没有自定义背景。
	DataURL string `json:"dataUrl"`
	// FileName 是 backgrounds/ 下的文件名（排障用，界面不展示）。
	FileName string `json:"fileName"`
	// Dim 是压在图片上的黑色遮罩百分比（0-100）。
	Dim int `json:"dim"`
	// Blur 是背景图的模糊半径（px，0 = 不模糊）。
	Blur int `json:"blur"`
	// MaxBytes 是允许的图片大小上限，供前端在选择前就给出提示。
	MaxBytes int64 `json:"maxBytes"`
}

// SeedTestDTO 是「测试一颗种子」的结果（D-14）。
//
// UDPOK / TCPOK 分开给：用户最常撞到的是「UDP 通、TCP 被防火墙挡」，
// 一句笼统的「连接失败」会把他引到错误的方向去改。
type SeedTestDTO struct {
	Addr        string `json:"addr"`
	OK          bool   `json:"ok"`
	UDPOK       bool   `json:"udpOk"`
	TCPOK       bool   `json:"tcpOk"`
	RTTMs       int64  `json:"rttMs"`
	NodeID      string `json:"nodeId"`
	DisplayName string `json:"displayName"`
	Subnet      string `json:"subnet"`
	TCPPort     int    `json:"tcpPort"`
	Error       string `json:"error"`
}

// SelfCheckDTO 是部署前置条件（P-1 / P-2）自检结果。
//
// Detail 字段是【给人读的结论】而不是错误码：这两条前提只有部署者能修
// （改网段规划 / 开三层路由），软件侧无能为力，因此必须说清「怎么修」。
type SelfCheckDTO struct {
	Performed bool      `json:"performed"`
	SeedCount int       `json:"seedCount"`
	Learned   int       `json:"learned"`
	P1OK      bool      `json:"p1OK"`
	P1Detail  string    `json:"p1Detail"`
	P2Overlap bool      `json:"p2Overlap"`
	P2Detail  string    `json:"p2Detail"`
	Seeds     []SeedDTO `json:"seeds"`
}
