package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/version"
)

// 局域网更新的时间参数。
//
// 这些数字不是拍脑袋：每一条都在防一种具体的故障。
const (
	// updateFirstCheckDelay 是启动后首次检查的延迟。
	// 太早检查会和握手/目录拉取抢带宽与 CPU，而用户此刻正在等界面出来。
	updateFirstCheckDelay = 60 * time.Second
	// updateCheckInterval 是周期检查间隔。10 分钟足够快（用户不会觉得
	// 「明明有人升级了这边没反应」），又慢到不会变成常驻噪声。
	updateCheckInterval = 10 * time.Minute
	// updateJitterMax 是决定更新后的随机等待上限（错峰）。
	// 没有它，全网 20 台机器会在同一秒互相拉包、同时退出重启。
	updateJitterMax = 10 * time.Minute
	// updateSnooze 是「稍后提醒」的间隔。
	updateSnooze = 30 * time.Minute
	// updateFailCooldown 是失败源的黑名单时长。
	updateFailCooldown = 24 * time.Hour
	// updateMaxAttempts 是同一目标版本连续失败多少次后停止尝试。
	updateMaxAttempts = 3
	// updateDownloadTimeout 是整包下载的总超时（含慢速链路）。
	updateDownloadTimeout = 5 * time.Minute
	// updateWaitBusy 是「有传输在跑，推迟重启」的最长等待。
	updateWaitBusy = 30 * time.Minute
)

// UpdatePhase 是更新流程的阶段（前端按它决定显示什么）。
type UpdatePhase string

const (
	// UpdateIdle 表示当前没有可做的事（没有更高版本，或功能被关闭）。
	UpdateIdle UpdatePhase = "idle"
	// UpdateChecking 表示正在询问对端（通常一闪而过）。
	UpdateChecking UpdatePhase = "checking"
	// UpdateDownloading 表示正在拉安装包。
	UpdateDownloading UpdatePhase = "downloading"
	// UpdateReady 表示包已下载并校验通过，等用户/计划触发安装。
	UpdateReady UpdatePhase = "ready"
	// UpdateScheduled 表示已排定安装时间。
	UpdateScheduled UpdatePhase = "scheduled"
	// UpdateApplying 表示安装器已拉起，进程即将退出。
	UpdateApplying UpdatePhase = "applying"
	// UpdateFailed 表示最近一次尝试失败（Error 里有原因）。
	UpdateFailed UpdatePhase = "failed"
	// UpdateUnsupported 表示当前平台不支持（macOS / Linux，见 ADR）。
	UpdateUnsupported UpdatePhase = "unsupported"
)

// UpdateSettings 是更新功能的运行时配置快照。
//
// 用结构体而不是直接读 *config.Config：app 层不许 import adapters/config
// （依赖方向 R2），组合根负责把配置翻译成这个值。
type UpdateSettings struct {
	Enabled bool
	// Mode 取 "prompt" / "auto" / "notify"。
	Mode string
	// AutoInstallAfter 是 prompt 模式下「无操作即安装」的兜底时长；
	// 0 表示永不自动安装。
	AutoInstallAfter time.Duration
}

// UpdateState 是更新功能的完整状态快照。
//
// 前端只拿这一个结构（配合单事件 EventUpdateStateChanged），
// 而不是拼装多个增量事件 —— 状态机有 8 个阶段，增量事件迟早会拼错。
type UpdateState struct {
	Phase          UpdatePhase `json:"phase"`
	CurrentVersion string      `json:"currentVersion"`
	TargetVersion  string      `json:"targetVersion,omitempty"`
	SourcePeerID   string      `json:"sourcePeerId,omitempty"`
	SourceName     string      `json:"sourceName,omitempty"`
	SourceAddr     string      `json:"sourceAddr,omitempty"`
	Size           int64       `json:"size,omitempty"`
	Downloaded     int64       `json:"downloaded,omitempty"`
	Percent        float64     `json:"percent,omitempty"`
	SHA256         string      `json:"sha256,omitempty"`
	// ScheduledAt 是安排好的安装时刻（毫秒时间戳，0 表示没有排期）。
	ScheduledAt int64 `json:"scheduledAt,omitempty"`
	// PromptPending 为真时前端应弹确认框（已下载完成、等待用户选择）。
	PromptPending bool `json:"promptPending"`
	// AutoInstallAt 是非零值时表示「到点就自动装」（毫秒时间戳）。
	AutoInstallAt int64  `json:"autoInstallAt,omitempty"`
	Error         string `json:"error,omitempty"`
	// PlatformSupported 为假时整个功能只是展示（当前实现仅 Windows）。
	PlatformSupported bool `json:"platformSupported"`
	// CanDistribute 表示本机是否持有「与当前运行版本一致」的安装包，
	// 也就是能不能作为别人的更新源。
	CanDistribute bool `json:"canDistribute"`
	// InstallerPath 是已下载安装包的本地路径（排障用）。
	InstallerPath string `json:"installerPath,omitempty"`
	// LastCheckAt 是最近一次检查的时刻（毫秒时间戳）。
	LastCheckAt int64 `json:"lastCheckAt,omitempty"`
	// HigherPeers 是局域网里版本更高的节点数（设置页显示「局域网最新版本」用）。
	HigherPeers []UpdatePeerInfo `json:"higherPeers,omitempty"`
}

// UpdatePeerInfo 描述一个「版本更高」的候选源。
type UpdatePeerInfo struct {
	PeerID      string `json:"peerId"`
	DisplayName string `json:"displayName"`
	Addr        string `json:"addr"`
	Version     string `json:"version"`
}

// updatePersist 是落盘的最小状态：只有跨进程重启才需要它。
//
// 为什么必须落盘：安装流程是「应用退出 → 安装器跑 → 新版本启动」，
// 新进程是【另一个进程】。没有这份文件，它就不知道自己是刚升级完
// （成功）还是装到一半崩了（失败）—— 而这两种情况的处理完全不同。
type updatePersist struct {
	Phase         string           `json:"phase"`
	TargetVersion string           `json:"target_version"`
	Installer     string           `json:"installer"`
	SHA256        string           `json:"sha256"`
	Size          int64            `json:"size"`
	SourcePeerID  string           `json:"source_peer_id"`
	SourceName    string           `json:"source_name"`
	FailedCount   int              `json:"failed_count"`
	FailedSources map[string]int64 `json:"failed_sources,omitempty"` // peer_id → 过期时刻(unix 秒)
}

// downloadSession 是一次进行中的包下载。
//
// 分块帧由 Router 在【会话读取 goroutine】里处理，所以这里的写文件动作
// 必须足够快：256 KiB 写盘是毫秒级，可以接受；一旦改成「攒起来再写」，
// 就会在内存里缓冲整个安装包。
type downloadSession struct {
	peerID identity.NodeID
	path   string
	file   *os.File
	hasher hash.Hash
	offset int64
	info   protocol.UpdateInfoResp
	done   chan struct{}
	err    error
}

// UpdateApp 编排局域网更新：发现更高版本 → 从对端拉安装包 → 询问/排期 → 静默安装。
//
// 安全边界（已在设计阶段明确接受）：内网被视为可信，安装包没有发布者签名。
// 代码里保留的护栏是「同平台同架构」「版本严格更高」「SHA-256 落盘记录」
// 与「装完自检版本是否真的变了」—— 它们防的是误升级与死循环，不是防投毒。
type UpdateApp struct {
	self identity.NodeID
	dir  ports.PeerDirectory
	conn SessionDialer
	clk  ports.Clock
	lg   *slog.Logger

	// DataDir 是配置目录（updates/ 建在它下面），由组合根注入。
	DataDir string
	// Settings 返回运行时配置，由组合根注入（实时读配置对象，改完立即生效）。
	Settings func() UpdateSettings
	// Busy 报告是否有正在进行的文件传输；为真时推迟安装（不打断用户）。
	Busy func() bool
	// Quit 由外壳注入：安装器拉起后退出进程。
	Quit func()
	// Launch 由外壳注入：拉起安装器（平台相关，放在 adapters 里）。
	Launch func(installer string) error
	// OnChange 在状态变化时被调用（外壳据此向前端推事件），可为 nil。
	OnChange func(UpdateState)

	mu sync.Mutex
	st UpdateState
	dl *downloadSession
	// fail 是「失败源黑名单」：node_id → 解禁时刻。
	fail map[identity.NodeID]time.Time
	// probeWait 是按「单次询问」关联的等待通道。
	//
	// 只支持一个在途询问：更新是单机单流程，全局同时只有一个检查在跑，
	// peer_id 之外的关联键（序号）会让协议多一个两端必须对齐的字段。
	probeWait chan protocol.UpdateInfoResp
	// 安装包摘要缓存（源侧每次请求都要回 SHA-256）。
	cachedSum     string
	cachedSumPath string
	cachedSumSize int64
	// promptSince 是确认框出现的时刻（用于「4 小时无操作自动装」）。
	promptSince time.Time
	// snoozeUntil 是「稍后提醒」的到期时刻。
	snoozeUntil time.Time
	// scheduledAt 是排定的安装时刻。
	scheduledAt time.Time
	// busySince 是开始等待「传输结束」的时刻。
	busySince time.Time
	// attempted 记录同一目标版本已尝试安装的次数。
	attempted int
	// lastTarget 是上一次尝试安装的目标版本。
	lastTarget string
	// probing 防止并发检查。
	probing bool
	// gen 是「本次更新意图」的代号。
	//
	// 用户点「放弃本次」时 gen 自增，正在错峰等待或下载的 goroutine 会发现
	// 自己的 gen 过期而退出。少了它，用户取消之后过 10 分钟包又下好了、
	// 弹框又冒出来 —— 那是最典型的「明明关了它又自己装」。
	gen int

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// SessionDialer 是「按 node_id 取一条已握手会话」的最小依赖。
//
// 只要这一个方法而不是整个 ports.ConnManager：更新流程不需要广播、
// 不需要关闭别人的连接，收窄接口让测试里换掉它只要几行。
type SessionDialer interface {
	Session(ctx context.Context, id identity.NodeID) (ports.Session, error)
}

// UpdateInstallExt 返回安装包在本平台上的扩展名。
//
// 它是「源节点缓存文件的命名约定」，也是接收端落盘名的来源：两端必须用
// 同一个规则，否则接收端会把 .part 改名成一个源节点不认识的名字。
func UpdateInstallExt() string {
	switch runtime.GOOS {
	case "windows":
		return ".exe"
	case "darwin":
		return ".dmg"
	default:
		return ".AppImage"
	}
}

// UpdateInstallerName 返回某个版本的安装包文件名（不含目录）。
//
// 用「文件名里带版本号」而不是读 PE 资源来确认包的身份：NSIS 打包时
// 就把版本写进文件名，两边同源；读 PE 版本要引入平台相关的 syscall，
// 而在「内网可信」的前提下它并不增加安全，只是多一处可能出错的解析。
func UpdateInstallerName(v string) string {
	return fmt.Sprintf("swarmlink-%s-installer%s", v, UpdateInstallExt())
}

// NewUpdateApp 构造更新应用。
func NewUpdateApp(
	self identity.NodeID,
	dir ports.PeerDirectory,
	conn SessionDialer,
	dataDir string,
	clk ports.Clock,
	lg *slog.Logger,
) *UpdateApp {
	a := &UpdateApp{
		self:    self,
		dir:     dir,
		conn:    conn,
		clk:     clk,
		lg:      lg,
		DataDir: dataDir,
		fail:    make(map[identity.NodeID]time.Time),
	}
	a.st = UpdateState{
		Phase:             UpdateIdle,
		CurrentVersion:    version.Version,
		PlatformSupported: updatePlatformSupported(),
	}
	return a
}

// Start 载入持久化状态并启动后台循环。
func (a *UpdateApp) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	a.ctx, a.cancel = context.WithCancel(ctx)

	a.mu.Lock()
	a.loadPersistLocked()
	a.st.CanDistribute = a.installerExistsLocked(version.Version)
	a.mu.Unlock()
	a.notify()

	a.wg.Add(1)
	go a.loop()
}

// Stop 停止后台循环。
func (a *UpdateApp) Stop() {
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
}

// State 返回状态快照。
func (a *UpdateApp) State() UpdateState {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.st
	out.PromptPending = a.promptPendingLocked()
	// 每次读状态都重新判断一次：安装包可能被用户手动删掉（运维清目录）。
	out.CanDistribute = a.installerExistsLocked(version.Version)
	if out.PromptPending {
		out.AutoInstallAt = a.autoInstallAtLocked()
	}
	return out
}

// ---------------------------------------------------------------------------
// 后台循环
// ---------------------------------------------------------------------------

func (a *UpdateApp) loop() {
	defer a.wg.Done()

	first := a.clk.After(updateFirstCheckDelay)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()

	nextCheck := time.Time{}
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-first:
			a.checkAsync()
			nextCheck = a.clk.Now().Add(updateCheckInterval)
		case <-tick.C:
			now := a.clk.Now()
			if !nextCheck.IsZero() && now.After(nextCheck) {
				nextCheck = now.Add(updateCheckInterval)
				a.checkAsync()
			}
			a.tick(now)
		}
	}
}

// tick 处理与时间有关的自动动作：排期到点、兜底超时、auto 模式。
//
// 所有「要不要现在装」的判断都收敛在这里，而不是散落在
// CheckNow / ApplyNow / 前端：判断只存在一处，才不会有「点了没反应」
// 与「没点却装了」这种前后不一致。
func (a *UpdateApp) tick(now time.Time) {
	if !a.settings().Enabled {
		return
	}
	a.mu.Lock()
	due := a.autoInstallAtLocked()
	a.mu.Unlock()
	if due <= 0 || now.UnixMilli() < due {
		return
	}
	if err := a.ApplyNow(); err != nil && a.lg != nil {
		// 「有传输在跑」是预期内的拒绝，不是故障：tick 下一轮会再试。
		a.lg.Debug("局域网更新：到点安装未执行", "err", err)
	}
}

// ---------------------------------------------------------------------------
// 检查与下载
// ---------------------------------------------------------------------------

// CheckNow 立即检查一次（设置页的「立即检查」按钮）。
//
// 返回值是【检查之后】的状态：若已经处于 ready/scheduled/applying，
// 直接原样返回 —— 重复检查不该把用户的排期冲掉。
func (a *UpdateApp) CheckNow() UpdateState {
	a.checkSync()
	return a.State()
}

// CheckAsync 在后台检查一次（界面上的「立即检查」）。
//
// 为什么不直接暴露同步版：检查要对每个候选拨号 + 等应答（最长 10 秒），
// 同步调用会把 WebView 的按钮按住十几秒 —— 用户会以为点了没反应。
func (a *UpdateApp) CheckAsync() { a.checkAsync() }

func (a *UpdateApp) checkAsync() {
	a.mu.Lock()
	if a.probing {
		a.mu.Unlock()
		return
	}
	a.probing = true
	a.mu.Unlock()

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() {
			a.mu.Lock()
			a.probing = false
			a.mu.Unlock()
		}()
		a.checkSync()
	}()
}

func (a *UpdateApp) checkSync() {
	s := a.settings()
	if !s.Enabled || !updatePlatformSupported() {
		return
	}

	a.mu.Lock()
	// 已经有确定的动作时不再检查：否则排期会被下一轮检查覆盖。
	if a.st.Phase == UpdateReady || a.st.Phase == UpdateScheduled || a.st.Phase == UpdateApplying {
		a.mu.Unlock()
		return
	}
	a.st.LastCheckAt = a.clk.Now().UnixMilli()
	a.st.Phase = UpdateChecking
	a.st.HigherPeers = a.higherPeersLocked()
	a.mu.Unlock()
	a.notify()

	cands := a.candidates()
	if len(cands) == 0 {
		a.mu.Lock()
		a.st.Phase = UpdateIdle
		a.st.TargetVersion = ""
		a.st.Error = ""
		a.mu.Unlock()
		a.notify()
		return
	}

	for _, c := range cands {
		info, err := a.probe(c.PeerID)
		if err != nil {
			if a.lg != nil {
				a.lg.Debug("局域网更新：询问对端失败", "peer_id", c.PeerID.String(), "err", err)
			}
			continue
		}
		if !info.HasPkg || info.Size <= 0 || info.SHA256 == "" {
			continue
		}
		// 同平台同架构是硬性条件：一台 Windows 机器拿到 .dmg 毫无意义。
		if info.GOOS != runtime.GOOS || info.GOARCH != runtime.GOARCH {
			continue
		}
		// 以应答里的版本为准（广播里的版本可被篡改，且可能是转述）。
		if !version.IsUpgrade(info.Version, version.Version) {
			continue
		}
		if a.snoozed(c.PeerID) {
			continue
		}

		if a.lg != nil {
			a.lg.Info("局域网更新：发现更高版本",
				"from", c.PeerID.String(), "peer_version", info.Version,
				"self", version.Version, "size", info.Size)
		}
		a.downloadFrom(c, info)
		return
	}

	// 有更高版本但都拉不动：状态留在 idle，但把候选记下来给设置页显示。
	a.mu.Lock()
	a.st.Phase = UpdateIdle
	a.mu.Unlock()
	a.notify()
}

// candidate 是一个候选更新源。
type candidate struct {
	PeerID      identity.NodeID
	DisplayName string
	Addr        string
	Version     string
}

// candidates 返回目录里版本严格高于本机、且处于在线/可拨号状态的节点。
//
// 排序规则：版本高者优先；同版本按 node_id 稳定排序（避免每次检查都换源，
// 换源意味着重新建立连接、重新下载已经下了一半的包）。
func (a *UpdateApp) candidates() []candidate {
	if a.dir == nil {
		return nil
	}
	list := a.dir.List(ports.PeerFilter{OnlineOnly: true})
	out := make([]candidate, 0, len(list))
	for _, p := range list {
		if p.LastAddr == "" || p.AppVersion == "" {
			continue
		}
		if !version.IsUpgrade(p.AppVersion, version.Version) {
			continue
		}
		out = append(out, candidate{
			PeerID:      p.NodeID,
			DisplayName: p.DisplayName,
			Addr:        p.LastAddr,
			Version:     p.AppVersion,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if c := version.Compare(out[i].Version, out[j].Version); c != 0 {
			return c > 0
		}
		return out[i].PeerID.String() < out[j].PeerID.String()
	})
	return out
}

func (a *UpdateApp) higherPeersLocked() []UpdatePeerInfo {
	if a.dir == nil {
		return nil
	}
	list := a.dir.List(ports.PeerFilter{OnlineOnly: true})
	out := make([]UpdatePeerInfo, 0, len(list))
	for _, p := range list {
		if !version.IsUpgrade(p.AppVersion, version.Version) {
			continue
		}
		out = append(out, UpdatePeerInfo{
			PeerID:      p.NodeID.String(),
			DisplayName: p.DisplayName,
			Addr:        p.LastAddr,
			Version:     p.AppVersion,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return version.Compare(out[i].Version, out[j].Version) > 0
	})
	return out
}

// probe 询问对端手上有没有与它自己版本一致的安装包。
func (a *UpdateApp) probe(peerID identity.NodeID) (protocol.UpdateInfoResp, error) {
	if a.conn == nil {
		return protocol.UpdateInfoResp{}, fmt.Errorf("update: 无连接层")
	}
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()

	sess, err := a.conn.Session(ctx, peerID)
	if err != nil {
		return protocol.UpdateInfoResp{}, err
	}
	a.mu.Lock()
	a.probeWait = make(chan protocol.UpdateInfoResp, 1)
	ch := a.probeWait
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.probeWait = nil
		a.mu.Unlock()
	}()

	payload, err := protocol.EncodeJSON(protocol.UpdateInfoReq{Version: version.Version})
	if err != nil {
		return protocol.UpdateInfoResp{}, err
	}
	if err := sess.Send(protocol.New(protocol.TypeUpdateInfoReq, payload)); err != nil {
		return protocol.UpdateInfoResp{}, err
	}

	select {
	case resp := <-ch:
		if !resp.OK {
			return resp, fmt.Errorf("update: 对端拒绝：%s", resp.Error)
		}
		return resp, nil
	case <-a.clk.After(10 * time.Second):
		return protocol.UpdateInfoResp{}, fmt.Errorf("update: 询问超时")
	case <-ctx.Done():
		return protocol.UpdateInfoResp{}, ctx.Err()
	}
}

// downloadFrom 从指定源拉安装包。
//
// 错峰等待在这里生效：决定更新之后先睡一个随机时长（0~10 分钟），
// 让「一台升级完、全网同时开拉」的同步风暴变成一条平缓的扩散曲线。
func (a *UpdateApp) downloadFrom(c candidate, info protocol.UpdateInfoResp) {
	a.mu.Lock()
	a.st.Phase = UpdateDownloading
	a.st.TargetVersion = info.Version
	a.st.SourcePeerID = c.PeerID.String()
	a.st.SourceName = c.DisplayName
	a.st.SourceAddr = c.Addr
	a.st.Size = info.Size
	a.st.SHA256 = info.SHA256
	a.st.Downloaded = 0
	a.st.Percent = 0
	a.st.Error = ""
	a.gen++
	gen := a.gen
	wait := a.jitter()
	a.mu.Unlock()
	a.notify()

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		if wait > 0 {
			select {
			case <-a.ctx.Done():
				return
			case <-a.clk.After(wait):
			}
		}
		if a.stale(gen) {
			return
		}
		if err := a.fetch(c.PeerID, info, gen); err != nil {
			if errors.Is(err, errUpdateStale) {
				return
			}
			a.failDownload(err)
			return
		}
		a.finishDownload(gen)
	}()
}

// errUpdateStale 表示这次下载已被用户取消 / 功能被关闭，属于正常收尾而不是失败。
var errUpdateStale = fmt.Errorf("update: 本次更新已被取消")

// stale 判断一次下载是否已经过期（用户取消、关闭开关、或又发起了新的一次）。
func (a *UpdateApp) stale(gen int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gen != gen
}

// fetch 执行真正的分块下载。
func (a *UpdateApp) fetch(peerID identity.NodeID, info protocol.UpdateInfoResp, gen int) error {
	if a.conn == nil {
		return fmt.Errorf("update: 无连接层")
	}
	dir := filepath.Join(a.DataDir, "updates")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("update: 创建目录失败: %w", err)
	}
	final := filepath.Join(dir, UpdateInstallerName(info.Version))
	part := final + ".part"

	if err := a.cleanupPartials(dir); err != nil && a.lg != nil {
		a.lg.Debug("局域网更新：清理旧临时文件失败", "err", err)
	}

	f, err := os.Create(part)
	if err != nil {
		return fmt.Errorf("update: 创建文件失败: %w", err)
	}

	dl := &downloadSession{
		peerID: peerID,
		path:   part,
		file:   f,
		hasher: sha256.New(),
		info:   info,
		done:   make(chan struct{}),
	}

	a.mu.Lock()
	a.dl = dl
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.dl = nil
		a.mu.Unlock()
		_ = f.Close()
	}()

	ctx, cancel := context.WithTimeout(a.ctx, updateDownloadTimeout)
	defer cancel()

	sess, err := a.conn.Session(ctx, peerID)
	if err != nil {
		return fmt.Errorf("update: 连接更新源失败: %w", err)
	}
	payload, err := protocol.EncodeJSON(protocol.UpdatePkgReq{Offset: 0})
	if err != nil {
		return err
	}
	if err := sess.Send(protocol.New(protocol.TypeUpdatePkgReq, payload)); err != nil {
		return fmt.Errorf("update: 发送下载请求失败: %w", err)
	}

	select {
	case <-dl.done:
		a.mu.Lock()
		err := dl.err
		a.mu.Unlock()
		if err != nil {
			_ = os.Remove(part)
			return err
		}
	case <-ctx.Done():
		_ = os.Remove(part)
		return fmt.Errorf("update: 下载超时")
	}

	if a.stale(gen) {
		_ = os.Remove(part)
		return errUpdateStale
	}

	// 校验：大小与摘要都必须对得上。
	sum := hex.EncodeToString(dl.hasher.Sum(nil))
	if dl.offset != info.Size {
		_ = os.Remove(part)
		return fmt.Errorf("update: 包大小不符（收到 %d，期望 %d）", dl.offset, info.Size)
	}
	if sum != info.SHA256 {
		_ = os.Remove(part)
		return fmt.Errorf("update: 包摘要不符（收到 %s）", sum)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("update: 关闭文件失败: %w", err)
	}
	if err := os.Rename(part, final); err != nil {
		return fmt.Errorf("update: 落盘失败: %w", err)
	}

	a.mu.Lock()
	a.st.InstallerPath = final
	a.st.SHA256 = sum
	a.mu.Unlock()

	if a.lg != nil {
		a.lg.Info("局域网更新：安装包已就绪", "path", final, "sha256", sum, "size", dl.offset)
	}
	return nil
}

func (a *UpdateApp) finishDownload(gen int) {
	if a.stale(gen) {
		// 下载途中被取消：把刚落盘的包删掉，否则它会成为「下次启动就能装」
		// 的遗留状态，而用户已经明确说过不要这次更新。
		a.mu.Lock()
		target := a.st.TargetVersion
		a.mu.Unlock()
		if target != "" && a.DataDir != "" {
			_ = os.Remove(filepath.Join(a.DataDir, "updates", UpdateInstallerName(target)))
		}
		return
	}
	now := a.clk.Now()
	a.mu.Lock()
	a.st.Phase = UpdateReady
	a.st.Downloaded = a.st.Size
	a.st.Percent = 100
	a.promptSince = now
	a.snoozeUntil = time.Time{}
	a.mu.Unlock()
	a.notify()
	a.persist()

	// auto 模式：不等用户，直接进入安装准备（真正执行由 tick 统一裁决，
	// 这样「有传输在跑」这条判断只存在一处）。
	if a.settings().Mode == "auto" {
		a.mu.Lock()
		a.promptSince = now.Add(-24 * time.Hour) // 视为已到期
		a.mu.Unlock()
	}
}

func (a *UpdateApp) failDownload(err error) {
	a.mu.Lock()
	a.st.Phase = UpdateFailed
	a.st.Error = err.Error()
	src := a.st.SourcePeerID
	a.mu.Unlock()

	if src != "" {
		if id, err := identity.ParseNodeID(src); err == nil {
			a.blacklist(id)
		}
	}
	if a.lg != nil {
		a.lg.Warn("局域网更新：下载失败", "err", err)
	}
	a.notify()
}

// ---------------------------------------------------------------------------
// 用户动作
// ---------------------------------------------------------------------------

// ApplyNow 立即安装。会拉起安装器并退出进程。
func (a *UpdateApp) ApplyNow() error {
	a.mu.Lock()
	path := a.st.InstallerPath
	target := a.st.TargetVersion
	if path == "" || target == "" {
		a.mu.Unlock()
		return fmt.Errorf("update: 没有可安装的更新包")
	}
	if a.st.Phase == UpdateApplying {
		a.mu.Unlock()
		return nil
	}
	// 忙时推迟：用户正在传文件，重启会打断一次几十 MB 的传输。
	if a.Busy != nil && a.Busy() {
		if a.busySince.IsZero() {
			a.busySince = a.clk.Now()
		}
		if a.clk.Now().Sub(a.busySince) < updateWaitBusy {
			a.mu.Unlock()
			return fmt.Errorf("update: 有传输任务在进行，稍后自动安装")
		}
	}
	a.busySince = time.Time{}
	a.st.Phase = UpdateApplying
	a.st.ScheduledAt = 0
	a.mu.Unlock()
	a.notify()

	// 先落盘再拉安装器：反过来的话，进程可能在写状态之前就被安装器
	// 结束掉，新版本启动时就不知道自己刚升级过（会被当成「装失败」）。
	a.persist()

	if a.Launch == nil {
		a.mu.Lock()
		a.st.Phase = UpdateFailed
		a.st.Error = "当前平台不支持自动安装"
		a.mu.Unlock()
		a.notify()
		return fmt.Errorf("update: 当前平台不支持自动安装")
	}
	if err := a.Launch(path); err != nil {
		a.mu.Lock()
		a.st.Phase = UpdateFailed
		a.st.Error = err.Error()
		a.mu.Unlock()
		// 覆盖掉刚才写下的 "applying"：否则下次启动会把「安装器根本没跑起来」
		// 误判成「装了但没生效」，进而错误地把这个源拉黑 24 小时。
		a.persist()
		a.notify()
		return err
	}

	if a.Quit != nil {
		a.Quit()
	}
	return nil
}

// Schedule 把安装安排到指定时刻。
func (a *UpdateApp) Schedule(at time.Time) error {
	a.mu.Lock()
	if a.st.Phase != UpdateReady && a.st.Phase != UpdateScheduled {
		a.mu.Unlock()
		return fmt.Errorf("update: 当前没有待安装的更新")
	}
	if !at.After(a.clk.Now().Add(10 * time.Second)) {
		// 排到过去等于「立刻装」：直接走 ApplyNow，避免出现「排期已到但没动」。
		a.mu.Unlock()
		return a.ApplyNow()
	}
	a.scheduledAt = at
	a.st.Phase = UpdateScheduled
	a.st.ScheduledAt = at.UnixMilli()
	a.mu.Unlock()
	a.notify()
	a.persist()
	return nil
}

// Snooze 延后提醒（不改排期，也不取消更新）。
func (a *UpdateApp) Snooze() UpdateState {
	a.mu.Lock()
	a.snoozeUntil = a.clk.Now().Add(updateSnooze)
	a.mu.Unlock()
	a.notify()
	return a.State()
}

// Cancel 放弃本次更新：删除安装包并回到 idle。
//
// 刻意不拉黑来源：用户可能只是这次不想装，下一轮检查（10 分钟后）
// 还应该提示 —— 拉黑是给「装不上」用的，不是给「不想装」用的。
func (a *UpdateApp) Cancel() error {
	a.mu.Lock()
	path := a.st.InstallerPath
	// 自增代号：正在错峰等待 / 下载中的 goroutine 会据此自行退出，
	// 不会再在 10 分钟后把包下回来并弹一次框。
	a.gen++
	// 正在下载的会话也要立刻收掉：只改状态的话，它还会继续把几十 MB 写完。
	a.abortActiveDownloadLocked(errUpdateStale)
	a.st = UpdateState{
		Phase:             UpdateIdle,
		CurrentVersion:    version.Version,
		PlatformSupported: updatePlatformSupported(),
	}
	a.promptSince = time.Time{}
	a.scheduledAt = time.Time{}
	a.snoozeUntil = time.Time{}
	a.mu.Unlock()

	if path != "" {
		_ = os.Remove(path)
	}
	a.notify()
	a.persist()
	return nil
}

// OnSettingsChanged 在配置变化时调用：关闭功能要立刻停手并删包。
func (a *UpdateApp) OnSettingsChanged() {
	s := a.settings()
	if s.Enabled {
		a.mu.Lock()
		a.st.CanDistribute = a.installerExistsLocked(version.Version)
		a.mu.Unlock()
		a.notify()
		return
	}
	_ = a.Cancel()
}

// ---------------------------------------------------------------------------
// 协议处理（入站）
// ---------------------------------------------------------------------------

// HandleInfoReq 是 UPDATE_INFO_REQ 的入站处理（本机作为更新源）。
func (a *UpdateApp) HandleInfoReq(sess ports.Session, f protocol.Frame) error {
	var req protocol.UpdateInfoReq
	if len(f.Payload) > 0 {
		if err := protocol.DecodeJSON(f.Payload, &req); err != nil {
			return a.sendErr(sess, "bad_request", err.Error())
		}
	}

	resp := protocol.UpdateInfoResp{OK: true, Version: version.Version, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	_, size, sum, ok := a.installerInfo()
	if ok {
		resp.HasPkg = true
		resp.Size = size
		resp.SHA256 = sum
	} else {
		resp.HasPkg = false
		// 不是错误：对端只是「没有包可给」，接收方换一个源就好。
		resp.Error = "本机没有与当前版本一致的安装包"
	}
	payload, err := protocol.EncodeJSON(resp)
	if err != nil {
		return err
	}
	if a.lg != nil {
		a.lg.Info("局域网更新：有人来问包",
			"from", sess.PeerID().String(), "their_version", req.Version,
			"has_pkg", resp.HasPkg)
	}
	return sess.Send(protocol.New(protocol.TypeUpdateInfoResp, payload))
}

// HandleInfoResp 是 UPDATE_INFO_RESP 的入站处理（本机作为拉取方）。
func (a *UpdateApp) HandleInfoResp(sess ports.Session, f protocol.Frame) error {
	var resp protocol.UpdateInfoResp
	if err := protocol.DecodeJSON(f.Payload, &resp); err != nil {
		return err
	}
	a.mu.Lock()
	ch := a.probeWait
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

// HandlePkgReq 是 UPDATE_PKG_REQ 的入站处理：流式把安装包推回去。
//
// 必须在独立 goroutine 里推：Router 的 dispatch 跑在会话读取循环上，
// 在里面推 40 MB 会让这条连接上的心跳与聊天全部停摆。
func (a *UpdateApp) HandlePkgReq(sess ports.Session, f protocol.Frame) error {
	var req protocol.UpdatePkgReq
	if err := protocol.DecodeJSON(f.Payload, &req); err != nil {
		return a.sendErr(sess, "bad_request", err.Error())
	}
	path, size, sum, ok := a.installerInfo()
	if !ok {
		return a.sendErr(sess, "no_package", "本机没有可分发的安装包")
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		file, err := os.Open(path)
		if err != nil {
			_ = a.sendErr(sess, "open_failed", err.Error())
			return
		}
		defer file.Close()

		if req.Offset > 0 {
			if _, err := file.Seek(req.Offset, io.SeekStart); err != nil {
				_ = a.sendErr(sess, "seek_failed", err.Error())
				return
			}
		}
		if a.lg != nil {
			a.lg.Info("局域网更新：开始分发安装包",
				"to", sess.PeerID().String(), "size", size)
		}

		buf := make([]byte, protocol.UpdateChunkSize)
		offset := req.Offset
		for {
			n, rerr := file.Read(buf)
			if n > 0 {
				payload := protocol.UpdatePkgChunk{Offset: offset, Data: buf[:n]}.Encode()
				if err := sess.Send(protocol.New(protocol.TypeUpdatePkgChunk, payload)); err != nil {
					if a.lg != nil {
						a.lg.Warn("局域网更新：分发中断", "err", err)
					}
					return
				}
				offset += int64(n)
			}
			if rerr != nil {
				break
			}
		}
		done, err := protocol.EncodeJSON(protocol.UpdatePkgDone{SHA256: sum, Size: size})
		if err != nil {
			return
		}
		_ = sess.Send(protocol.New(protocol.TypeUpdatePkgDone, done))
	}()
	return nil
}

// HandlePkgChunk 是 UPDATE_PKG_CHUNK 的入站处理（本机作为接收方）。
//
// 直接写盘而不是先入队：分块只有 256 KiB，写盘是毫秒级；攒进内存
// 意味着几十 MB 的额外占用，而这个函数跑在会话读取 goroutine 上。
func (a *UpdateApp) HandlePkgChunk(sess ports.Session, f protocol.Frame) error {
	c, err := protocol.DecodeUpdatePkgChunk(f.Payload)
	if err != nil {
		return err
	}

	a.mu.Lock()
	dl := a.dl
	a.mu.Unlock()
	if dl == nil || dl.peerID != sess.PeerID() {
		return nil // 迟到或非本流程的分块：丢弃
	}
	if c.Offset != dl.offset {
		return a.abortDownload(dl, fmt.Errorf("update: 分块偏移错乱（收到 %d，期望 %d）", c.Offset, dl.offset))
	}
	if _, err := dl.file.Write(c.Data); err != nil {
		return a.abortDownload(dl, fmt.Errorf("update: 写入失败: %w", err))
	}
	if _, err := dl.hasher.Write(c.Data); err != nil {
		return a.abortDownload(dl, fmt.Errorf("update: 计算摘要失败: %w", err))
	}
	dl.offset += int64(len(c.Data))

	a.mu.Lock()
	if a.st.Phase == UpdateDownloading {
		a.st.Downloaded = dl.offset
		if a.st.Size > 0 {
			a.st.Percent = float64(dl.offset) * 100 / float64(a.st.Size)
		}
	}
	a.mu.Unlock()
	a.notify()
	return nil
}

// HandlePkgDone 是 UPDATE_PKG_DONE 的入站处理。
func (a *UpdateApp) HandlePkgDone(sess ports.Session, f protocol.Frame) error {
	var done protocol.UpdatePkgDone
	if err := protocol.DecodeJSON(f.Payload, &done); err != nil {
		return err
	}
	a.mu.Lock()
	dl := a.dl
	a.mu.Unlock()
	if dl == nil || dl.peerID != sess.PeerID() {
		return nil
	}
	if done.Size != dl.offset {
		return a.abortDownload(dl, fmt.Errorf("update: 包大小不符（收到 %d，对端声明 %d）", dl.offset, done.Size))
	}
	sum := hex.EncodeToString(dl.hasher.Sum(nil))
	if done.SHA256 != "" && sum != done.SHA256 {
		return a.abortDownload(dl, fmt.Errorf("update: 包摘要不符"))
	}
	close(dl.done)
	return nil
}

// HandleErr 是 UPDATE_ERR 的入站处理。
func (a *UpdateApp) HandleErr(sess ports.Session, f protocol.Frame) error {
	var e protocol.UpdateErr
	if err := protocol.DecodeJSON(f.Payload, &e); err != nil {
		return err
	}
	msg := e.Error
	if msg == "" {
		msg = e.Code
	}
	a.mu.Lock()
	dl := a.dl
	ch := a.probeWait
	a.mu.Unlock()
	if dl != nil && dl.peerID == sess.PeerID() {
		dl.err = fmt.Errorf("update: 对端报错: %s", msg)
		close(dl.done)
		return nil
	}
	if ch != nil {
		select {
		case ch <- protocol.UpdateInfoResp{OK: false, Error: msg}:
		default:
		}
	}
	return nil
}

// abortActiveDownloadLocked 立刻结束正在进行的下载（调用方必须持有 a.mu）。
func (a *UpdateApp) abortActiveDownloadLocked(err error) {
	dl := a.dl
	if dl == nil {
		return
	}
	if dl.err == nil {
		dl.err = err
	}
	select {
	case <-dl.done:
	default:
		close(dl.done)
	}
}

func (a *UpdateApp) abortDownload(dl *downloadSession, err error) error {
	a.mu.Lock()
	if dl.err == nil {
		dl.err = err
	}
	a.mu.Unlock()
	select {
	case <-dl.done:
	default:
		close(dl.done)
	}
	return nil
}

func (a *UpdateApp) sendErr(sess ports.Session, code, msg string) error {
	payload, err := protocol.EncodeJSON(protocol.UpdateErr{Code: code, Error: msg})
	if err != nil {
		return err
	}
	return sess.Send(protocol.New(protocol.TypeUpdateErr, payload))
}

// ---------------------------------------------------------------------------
// 分发侧：本机缓存的安装包
// ---------------------------------------------------------------------------

// installerInfo 返回本机缓存的、与【当前运行版本】一致的安装包信息。
//
// 为什么必须校验版本：广播出去的版本是 version.Version，而对端拿到的包
// 如果是别的版本，就会形成「装完还是旧版 → 再拉一次」的死循环。
// 文件名里带版本号是这条校验的全部实现（见 UpdateInstallerName）。
func (a *UpdateApp) installerInfo() (path string, size int64, sum string, ok bool) {
	a.mu.Lock()
	path = a.installerPathLocked(version.Version)
	a.mu.Unlock()
	if path == "" {
		return "", 0, "", false
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || st.Size() <= 0 {
		return "", 0, "", false
	}
	// 摘要只在第一次要用时算一次并缓存：40 MB 约 100 ms，每次请求都算
	// 在「十几台机器同时来问包」时是可见的卡顿。
	a.mu.Lock()
	if a.cachedSum != "" && a.cachedSumPath == path && a.cachedSumSize == st.Size() {
		sum = a.cachedSum
		a.mu.Unlock()
		return path, st.Size(), sum, true
	}
	a.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return "", 0, "", false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", 0, "", false
	}
	sum = hex.EncodeToString(h.Sum(nil))

	a.mu.Lock()
	a.cachedSum, a.cachedSumPath, a.cachedSumSize = sum, path, st.Size()
	a.mu.Unlock()
	return path, st.Size(), sum, true
}

func (a *UpdateApp) installerPathLocked(v string) string {
	if a.DataDir == "" || v == "" {
		return ""
	}
	return filepath.Join(a.DataDir, "updates", UpdateInstallerName(v))
}

func (a *UpdateApp) installerExistsLocked(v string) bool {
	path := a.installerPathLocked(v)
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && !st.IsDir() && st.Size() > 0
}

// cleanupPartials 清理上次中断留下的 .part 文件。
//
// 只清 .part：已经下载完成的包（可能正是别人要拉的）不能动。
func (a *UpdateApp) cleanupPartials(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".part" {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	return nil
}

// ---------------------------------------------------------------------------
// 状态持久化
// ---------------------------------------------------------------------------

func (a *UpdateApp) persistPath() string {
	if a.DataDir == "" {
		return ""
	}
	return filepath.Join(a.DataDir, "updates", "state.json")
}

func (a *UpdateApp) persist() {
	a.mu.Lock()
	p := updatePersist{
		Phase:         string(a.st.Phase),
		TargetVersion: a.st.TargetVersion,
		Installer:     a.st.InstallerPath,
		SHA256:        a.st.SHA256,
		Size:          a.st.Size,
		SourcePeerID:  a.st.SourcePeerID,
		SourceName:    a.st.SourceName,
		FailedCount:   a.attempted,
		FailedSources: a.failedSnapshotLocked(),
	}
	path := a.persistPath()
	a.mu.Unlock()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// loadPersistLocked 读回上次的状态，并结算「上次是否真的装上」。
//
// 这是唯一可靠的失败信号：安装器有没有跑完、退出码是多少，应用都看不到
// （它已经退出了）。所以只能在新进程里问一句「我现在是哪个版本」。
func (a *UpdateApp) loadPersistLocked() {
	path := a.persistPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var p updatePersist
	if err := json.Unmarshal(data, &p); err != nil {
		return
	}
	now := a.clk.Now()
	for id, exp := range p.FailedSources {
		if time.Unix(exp, 0).After(now) {
			if nid, err := identity.ParseNodeID(id); err == nil {
				a.fail[nid] = time.Unix(exp, 0)
			}
		}
	}
	a.attempted = p.FailedCount
	a.lastTarget = p.TargetVersion

	if p.Phase != string(UpdateApplying) {
		return
	}

	if p.TargetVersion != "" && version.Compare(p.TargetVersion, version.Version) == 0 {
		// 成功：版本已经变成目标版本。
		if a.lg != nil {
			a.lg.Info("局域网更新：升级成功", "version", version.Version)
		}
		_ = os.Remove(path)
		// 升级后本机也成了分发源 —— 安装器会在安装时自缓存，这里只清旧的。
		a.cleanupOldInstallersLocked(p.Installer)
		return
	}

	// 失败：装完版本没变。拉黑来源，避免同一台机器反复被拉进死循环。
	if a.lg != nil {
		a.lg.Warn("局域网更新：升级未生效",
			"want", p.TargetVersion, "got", version.Version, "from", p.SourcePeerID)
	}
	if nid, err := identity.ParseNodeID(p.SourcePeerID); err == nil && !nid.IsZero() {
		a.fail[nid] = now.Add(updateFailCooldown)
	}
	a.attempted = p.FailedCount + 1
	if p.Installer != "" {
		_ = os.Remove(p.Installer)
	}
	a.st.Phase = UpdateFailed
	a.st.TargetVersion = p.TargetVersion
	a.st.Error = fmt.Sprintf("升级未生效（当前仍是 %s）", version.Version)
	_ = os.Remove(path)
	a.persistLockedNow()
}

// cleanupOldInstallersLocked 删除除当前版本以外的安装包缓存。
//
// 不删当前版本那一份：它正是本机作为更新源的「货」。
func (a *UpdateApp) cleanupOldInstallersLocked(keep string) {
	if a.DataDir == "" {
		return
	}
	dir := filepath.Join(a.DataDir, "updates")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	keepName := filepath.Base(keep)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == keepName || name == UpdateInstallerName(version.Version) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// persistLockedNow 在已持有锁时落盘（只给 loadPersistLocked 用）。
func (a *UpdateApp) persistLockedNow() {
	p := updatePersist{
		Phase:         string(a.st.Phase),
		TargetVersion: a.st.TargetVersion,
		SourcePeerID:  a.st.SourcePeerID,
		FailedCount:   a.attempted,
		FailedSources: a.failedSnapshotLocked(),
	}
	path := a.persistPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

func (a *UpdateApp) failedSnapshotLocked() map[string]int64 {
	if len(a.fail) == 0 {
		return nil
	}
	out := make(map[string]int64, len(a.fail))
	now := a.clk.Now()
	for id, exp := range a.fail {
		if exp.After(now) {
			out[id.String()] = exp.Unix()
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

func (a *UpdateApp) settings() UpdateSettings {
	if a.Settings == nil {
		return UpdateSettings{Enabled: false, Mode: "prompt"}
	}
	s := a.Settings()
	if s.Mode == "" {
		s.Mode = "prompt"
	}
	return s
}

// jitter 返回本次更新的错峰等待时长。
//
// 用随机数而不是 node_id 派生：同一批装机是同一时刻完成的，按身份
// 派生会让它们在同一秒一起醒来 —— 那正是要避免的同步风暴。
func (a *UpdateApp) jitter() time.Duration {
	if updateJitterMax <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(updateJitterMax)))
}

// promptPendingLocked 判断前端此刻该不该弹确认框。
func (a *UpdateApp) promptPendingLocked() bool {
	if a.settings().Mode != "prompt" {
		return false
	}
	if a.st.Phase != UpdateReady {
		return false
	}
	now := a.clk.Now()
	if !a.snoozeUntil.IsZero() && now.Before(a.snoozeUntil) {
		return false
	}
	return !a.promptSince.IsZero()
}

// autoInstallAtLocked 返回自动安装的时刻（0 表示不会自动安装）。
func (a *UpdateApp) autoInstallAtLocked() int64 {
	s := a.settings()
	now := a.clk.Now()

	switch a.st.Phase {
	case UpdateScheduled:
		if a.scheduledAt.IsZero() {
			return 0
		}
		return a.scheduledAt.UnixMilli()
	case UpdateReady:
		if s.Mode == "auto" {
			return now.UnixMilli() // 立刻
		}
		if s.Mode != "prompt" || s.AutoInstallAfter <= 0 || a.promptSince.IsZero() {
			return 0
		}
		due := a.promptSince.Add(s.AutoInstallAfter)
		if !a.snoozeUntil.IsZero() && a.snoozeUntil.After(due) {
			// 用户点了「稍后提醒」：兜底时刻跟着顺延，否则 30 分钟后
			// 一边弹框一边自动装，比不提示更糟。
			due = a.snoozeUntil.Add(s.AutoInstallAfter)
		}
		return due.UnixMilli()
	}
	return 0
}

func (a *UpdateApp) snoozed(peerID identity.NodeID) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.fail[peerID]
	if !ok {
		return false
	}
	if a.clk.Now().After(exp) {
		delete(a.fail, peerID)
		return false
	}
	return true
}

func (a *UpdateApp) blacklist(peerID identity.NodeID) {
	if peerID.IsZero() {
		return
	}
	a.mu.Lock()
	a.fail[peerID] = a.clk.Now().Add(updateFailCooldown)
	a.mu.Unlock()
}

func (a *UpdateApp) notify() {
	if a.OnChange == nil {
		return
	}
	a.OnChange(a.State())
}
