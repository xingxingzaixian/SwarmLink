// Package config 负责 TOML 配置的读写与默认值管理。
//
// 设计要点（架构书 7.4）：
//   - 加载时构造【不可变快照】，运行期通过 config.changed 事件通知各 App 更新
//   - 需要重启才能生效的项（端口、网卡）在 UI 明确标注
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/swarmlink/swarmlink/internal/domain/peer"
)

// AppName 是配置目录名。
const AppName = "SwarmLink"

// EnvDataDir 覆盖配置/数据目录（优先级最高），值是「含 config.toml 的那个目录」，
// 数据库落在其下的 data/app.db。
//
//	SWARMLINK_DATA_DIR=D:\swarmlink-data
const EnvDataDir = "SWARMLINK_DATA_DIR"

// EnvVaultDir 覆盖身份托管目录（vault）：值是「其下含 identity 子目录的那个目录」。
//
// 与 EnvDataDir 对称，且优先级最高 —— 测试与受限部署都需要一个不依赖机器的确定位置。
//
//	SWARMLINK_VAULT_DIR=D:\swarmlink-vault
const EnvVaultDir = "SWARMLINK_VAULT_DIR"

// PortableMarker 是可执行文件同目录下的便携模式标记文件（空文件即可）。
//
// 存在时数据与程序放在一起，便于拷到 U 盘、或在没写用户目录权限的机器上运行。
const PortableMarker = "portable.mode"

// Duration 让 TOML 里可以写 "5s" / "300s" 这样的可读值。
type Duration time.Duration

// UnmarshalText 实现 encoding.TextUnmarshaler。
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("config: invalid duration %q: %w", string(b), err)
	}
	*d = Duration(v)
	return nil
}

// MarshalText 让配置可以被写回 TOML。
func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// Std 返回标准库的 time.Duration。
func (d Duration) Std() time.Duration { return time.Duration(d) }

// SeedsSection 是 [discovery.seeds] 段。
type SeedsSection struct {
	RefreshInterval Duration `toml:"refresh_interval"`
	SeedsPerRefresh int      `toml:"seeds_per_refresh"`
	ProbeTimeout    Duration `toml:"probe_timeout"`
	List            []string `toml:"list"`
}

// Config 是完整配置快照。
type Config struct {
	General struct {
		DisplayName string `toml:"display_name"`
	} `toml:"general"`

	Download struct {
		DefaultDir string `toml:"default_dir"`
		AutoOpen   bool   `toml:"auto_open"`
	} `toml:"download"`

	Network struct {
		TCPPort           int      `toml:"tcp_port"` // 0 = 自动探测（推荐）
		UDPPort           int      `toml:"udp_port"`
		PortFallbackRange int      `toml:"port_fallback_range"`
		InterfaceMode     string   `toml:"interface_mode"` // auto | manual | seed_only
		AllowInterfaces   []string `toml:"allow_interfaces"`
		DenyInterfaces    []string `toml:"deny_interfaces"`
		EnableMulticast   bool     `toml:"enable_multicast"`
	} `toml:"network"`

	Discovery struct {
		AnnounceInterval Duration     `toml:"announce_interval"`
		PeerTTL          Duration     `toml:"peer_ttl"`
		MaxPeers         int          `toml:"max_peers"`
		MaxPeerListSize  int          `toml:"max_peer_list_size"`
		Seeds            SeedsSection `toml:"seeds"`
	} `toml:"discovery"`

	Connection struct {
		IdleConnTimeout         Duration `toml:"idle_conn_timeout"`
		MaxDialConcurrency      int      `toml:"max_dial_concurrency"`
		MaxActiveConns          int      `toml:"max_active_conns"`
		HeartbeatOnlyWhenActive bool     `toml:"heartbeat_only_when_active"`
	} `toml:"connection"`

	Transfer struct {
		MaxConcurrent int   `toml:"max_concurrent"`
		WindowSize    int   `toml:"window_size"`
		ChunkSize     int64 `toml:"chunk_size"`
		MaxUploadMbps int   `toml:"max_upload_mbps"`
		// MaxFileSize 是接收侧的单文件上限（字节），0 表示不限制。
		// 只约束接收：要不要收下这个文件，是收方机器的决定。
		MaxFileSize int64 `toml:"max_file_size"`
		// MaxRetries 是「对端在、但每次尝试都失败」的自动重试上限，
		// 0 表示用默认值。对方【不在线】不算失败，那次等待不受它约束。
		MaxRetries          int      `toml:"max_retries"`
		BitmapFlushInterval Duration `toml:"bitmap_flush_interval"`
		// HistoryKeep / HistoryTTL 是传输记录的清理策略：
		// 启动时只保留最近 HistoryKeep 条已结束的，并丢弃超过 HistoryTTL 的。
		// 每条记录都带 chunk_bitmap，这张表是应用里唯一持续膨胀的东西。
		HistoryKeep int      `toml:"history_keep"`
		HistoryTTL  Duration `toml:"history_ttl"`
	} `toml:"transfer"`

	Security struct {
		RequireAuth bool   `toml:"require_auth"`
		Encryption  string `toml:"encryption"` // off | prefer | require（v1.1 启用）
	} `toml:"security"`

	// Notify 是提示行为设置。
	//
	// 这两个 bool 的默认值是 true，靠 Default() + unmarshal 得到：
	// Load 先把 Default() 填进去再解析文件，文件里没有 [notify] 段时保留 true。
	// 【不要】在 applyDefaults 里用 `if !c.Notify.Sound` 之类的回填 ——
	// false 是用户的合法选择，那样写会让「关掉开关」重启后自己打开。
	Notify struct {
		// Sound 控制新消息 / 传输事件是否发声。
		Sound bool `toml:"sound"`
		// MinimizeToTray 为真时，点关闭按钮把窗口收进托盘而不是退出。
		MinimizeToTray bool `toml:"minimize_to_tray"`
	} `toml:"notify"`

	// Startup 是启动相关设置。
	Startup struct {
		// AutoStart 控制是否开机自启动。
		AutoStart bool `toml:"auto_start"`
	} `toml:"startup"`
}

// Default 返回带默认值的配置。
//
// 默认清单里的种子是示例地址，部署方必须按 0.5 节的 P-1/P-2 替换为真实值。
func Default() *Config {
	c := &Config{}

	c.General.DisplayName = ""
	c.Network.TCPPort = 0
	c.Network.UDPPort = 0
	c.Network.PortFallbackRange = 10
	c.Network.InterfaceMode = "auto"
	c.Network.DenyInterfaces = []string{"utun*", "vmnet*", "vboxnet*", "docker*"}
	c.Network.EnableMulticast = false

	// 45s ± 33%（见 udp.DefaultConfig）→ 实际 30~60s。正常退出会发 BYE，
	// 因此这个周期只决定「崩溃后多久被发现」，不决定「关闭后多久变离线」。
	c.Discovery.AnnounceInterval = Duration(45 * time.Second)
	c.Discovery.PeerTTL = Duration(300 * time.Second)
	c.Discovery.MaxPeers = 512        // 全局约 200 节点，留 2.5× 余量
	c.Discovery.MaxPeerListSize = 256 // 单次响应即可覆盖全量 200 条目（ADR-010）
	c.Discovery.Seeds.RefreshInterval = Duration(300 * time.Second)
	c.Discovery.Seeds.SeedsPerRefresh = 2
	c.Discovery.Seeds.ProbeTimeout = Duration(2 * time.Second)

	c.Connection.IdleConnTimeout = Duration(5 * time.Minute)
	c.Connection.MaxDialConcurrency = 8
	c.Connection.MaxActiveConns = 32
	c.Connection.HeartbeatOnlyWhenActive = true

	c.Transfer.MaxConcurrent = 3
	c.Transfer.WindowSize = 8
	c.Transfer.ChunkSize = 512 * 1024
	// 10 GiB：够大（镜像、视频包）又不至于让一次误操作把接收盘写满。
	// 不需要在 applyDefaults 里回填 0 —— Load 是先填 Default() 再解析文件，
	// 文件里没写这个键时保留的正是这里这个值；写了 0 就是"不限制"。
	c.Transfer.MaxFileSize = 10 << 30
	c.Transfer.MaxRetries = 3
	c.Transfer.BitmapFlushInterval = Duration(2 * time.Second)
	c.Transfer.HistoryKeep = 50
	c.Transfer.HistoryTTL = Duration(7 * 24 * time.Hour)

	c.Security.RequireAuth = true // 强烈建议保持 true
	c.Security.Encryption = "off"

	// 提示默认全开：收进托盘与提示音是「桌面聊天工具的默认预期」，
	// 让用户先去设置里找开关才能收到消息提醒，是更差的第一印象。
	c.Notify.Sound = true
	c.Notify.MinimizeToTray = true

	// 开机自启动默认关闭：避免用户第一次启动就被系统启动吓到
	c.Startup.AutoStart = false

	return c
}

// Dir 返回应用配置目录（其下放 config.toml 与 data/app.db）。
//
// 优先级：
//  1. 环境变量 SWARMLINK_DATA_DIR —— 任何平台都可显式指定；
//  2. 可执行文件同目录存在 portable.mode 标记 —— 便携模式，数据与程序在一起；
//  3. 系统用户配置目录（Windows 是 %APPDATA%\SwarmLink）。
//
// 为什么不【默认】放在可执行文件旁边：装到 C:\Program Files 之后普通用户对它
// 没有写权限（这正是 Windows 要引入 %APPDATA% 的原因），而且同一台机器上的多个
// 用户会共用同一个数据库。所以「放在程序旁边」做成显式选择，而不是默认行为。
//
// 注意：macOS 上 os.UserConfigDir() 硬编码为 ~/Library/Application Support，
// 不响应 XDG_CONFIG_HOME —— 这是标准库的既定行为。
func Dir() (string, error) {
	if d := strings.TrimSpace(os.Getenv(EnvDataDir)); d != "" {
		return d, nil
	}
	if d, ok := portableDir(); ok {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: user config dir: %w", err)
	}
	return filepath.Join(base, AppName), nil
}

// portableDir 在可执行文件同目录发现便携标记时，返回该目录本身
// （于是 config.toml 与它平级，数据库在该目录的 data/ 下，与常规布局一致）。
func portableDir() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	dir := filepath.Dir(exe)
	if _, err := os.Stat(filepath.Join(dir, PortableMarker)); err != nil {
		return "", false
	}
	return dir, true
}

// VaultDir 返回身份托管副本所在目录（与数据目录无关的机器级固定位置）。
//
// 返回 "" 表示本次运行【禁用】托管，调用方据此退回「只用数据目录」的历史行为：
//
//   - 便携模式（portable.mode 存在）：便携 = 自包含。否则把 U 盘插到别的机器上会
//     恢复原机器的身份，同时污染那台机器的托管目录；
//   - 解析结果与数据目录重合：一旦相同，「删数据目录」就等于「删身份」，必须拦住。
//
// 本函数只做路径解析：不创建目录、不读写 vault 文件
// （与 Dir 一样，仅 stat 一下可执行文件旁边有没有便携标记）。
func VaultDir(dataDir string) string {
	if d := strings.TrimSpace(os.Getenv(EnvVaultDir)); d != "" {
		if sameDir(d, dataDir) {
			return ""
		}
		return d
	}
	if _, ok := portableDir(); ok {
		return ""
	}
	base := machineDataDir()
	if base == "" {
		return ""
	}
	d := filepath.Join(base, AppName)
	if sameDir(d, dataDir) {
		return ""
	}
	return d
}

// machineDataDir 返回「与用户无关的机器级数据目录」基座。
//
// 拿不到就返回空串（禁用托管），而不是猜一个可写但没有意义的位置：
// 托管副本放在用户 profile 里就失去了「换数据目录仍能找回身份」的意义。
func machineDataDir() string {
	switch runtime.GOOS {
	case "windows":
		if pd := strings.TrimSpace(os.Getenv("ProgramData")); pd != "" {
			return pd
		}
		if sd := strings.TrimSpace(os.Getenv("SystemDrive")); sd != "" {
			return filepath.Join(sd+string(filepath.Separator), "ProgramData")
		}
		return ""
	case "darwin":
		return "/Library/Application Support"
	default:
		return "/var/lib"
	}
}

// sameDir 判断两个路径是否指向同一目录。Windows 上大小写不敏感。
//
// 刻意不解析符号链接（那需要 I/O）：因此「dataDir 是 vault 的符号链接别名」这种重合
// 不会被拦住。macOS 默认卷同样大小写不敏感，但这里只按 GOOS 判断 —— 一并记作已知边界。
func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}

// Path 返回配置文件路径。
func Path(dir string) string { return filepath.Join(dir, "config.toml") }

// DBPath 返回数据库文件路径。
func DBPath(dir string) string { return filepath.Join(dir, "data", "app.db") }

// Load 读取配置；文件不存在时返回默认值并落盘，保证用户能看到可编辑的样例。
func Load(dir string) (*Config, error) {
	cfg := Default()
	path := Path(dir)

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("config: read %s: %w", path, err)
		}
		if err := cfg.applyDefaults(dir); err != nil {
			return nil, err
		}
		if err := Save(dir, cfg); err != nil {
			return nil, err
		}
		return cfg, nil
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := cfg.applyDefaults(dir); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save 写回配置（原子替换，避免半截文件）。
func Save(dir string, cfg *Config) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("config: mkdir %s: %w", dir, err)
	}
	path := Path(dir)
	tmp := path + ".tmp"

	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("config: create %s: %w", tmp, err)
	}
	if err := toml.NewEncoder(f).Encode(cfg); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("config: encode: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("config: replace %s: %w", path, err)
	}
	return nil
}

// SeedAddrs 解析种子清单。非法条目会被跳过（部署方最容易在这里写错格式）。
func (c *Config) SeedAddrs() ([]peer.SeedAddr, []error) {
	var (
		out  []peer.SeedAddr
		errs []error
	)
	for _, raw := range c.Discovery.Seeds.List {
		addr, err := peer.ParseSeedAddr(raw)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, addr)
	}
	return out, errs
}

// Validate 校验配置的自洽性。
func (c *Config) Validate() error {
	if c.Network.PortFallbackRange < 1 {
		c.Network.PortFallbackRange = 10
	}
	switch c.Network.InterfaceMode {
	case "", "auto":
		c.Network.InterfaceMode = "auto"
	case "manual", "seed_only":
	default:
		return fmt.Errorf("config: invalid interface_mode %q (want auto|manual|seed_only)", c.Network.InterfaceMode)
	}
	if c.Transfer.ChunkSize <= 0 {
		c.Transfer.ChunkSize = 512 * 1024
	}
	if c.Transfer.WindowSize <= 0 {
		c.Transfer.WindowSize = 8
	}
	if c.Discovery.Seeds.SeedsPerRefresh <= 0 {
		c.Discovery.Seeds.SeedsPerRefresh = 2
	}
	if c.Discovery.Seeds.SeedsPerRefresh > 8 {
		// 见 4.5.1：拉更多种子只是成倍增加流量，可靠性并不提高
		return fmt.Errorf("config: seeds_per_refresh=%d 过大（建议 2，见 ADR-011）", c.Discovery.Seeds.SeedsPerRefresh)
	}
	return nil
}

func (c *Config) applyDefaults(dir string) error {
	def := Default()

	if c.General.DisplayName == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "swarmlink-node"
		}
		c.General.DisplayName = host
	}
	if c.Download.DefaultDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("config: user home: %w", err)
		}
		c.Download.DefaultDir = filepath.Join(home, "Downloads", AppName)
	}
	if err := os.MkdirAll(c.Download.DefaultDir, 0o755); err != nil {
		return fmt.Errorf("config: mkdir download dir: %w", err)
	}

	if c.Network.PortFallbackRange == 0 {
		c.Network.PortFallbackRange = def.Network.PortFallbackRange
	}
	if len(c.Network.DenyInterfaces) == 0 {
		c.Network.DenyInterfaces = def.Network.DenyInterfaces
	}
	if c.Discovery.AnnounceInterval == 0 {
		c.Discovery.AnnounceInterval = def.Discovery.AnnounceInterval
	}
	if c.Discovery.PeerTTL == 0 {
		c.Discovery.PeerTTL = def.Discovery.PeerTTL
	}
	if c.Discovery.MaxPeers == 0 {
		c.Discovery.MaxPeers = def.Discovery.MaxPeers
	}
	if c.Discovery.MaxPeerListSize == 0 {
		c.Discovery.MaxPeerListSize = def.Discovery.MaxPeerListSize
	}
	if c.Discovery.Seeds.RefreshInterval == 0 {
		c.Discovery.Seeds.RefreshInterval = def.Discovery.Seeds.RefreshInterval
	}
	if c.Discovery.Seeds.SeedsPerRefresh == 0 {
		c.Discovery.Seeds.SeedsPerRefresh = def.Discovery.Seeds.SeedsPerRefresh
	}
	if c.Discovery.Seeds.ProbeTimeout == 0 {
		c.Discovery.Seeds.ProbeTimeout = def.Discovery.Seeds.ProbeTimeout
	}
	if c.Connection.IdleConnTimeout == 0 {
		c.Connection.IdleConnTimeout = def.Connection.IdleConnTimeout
	}
	if c.Connection.MaxDialConcurrency == 0 {
		c.Connection.MaxDialConcurrency = def.Connection.MaxDialConcurrency
	}
	if c.Connection.MaxActiveConns == 0 {
		c.Connection.MaxActiveConns = def.Connection.MaxActiveConns
	}
	if c.Transfer.MaxConcurrent == 0 {
		c.Transfer.MaxConcurrent = def.Transfer.MaxConcurrent
	}
	if c.Transfer.WindowSize == 0 {
		c.Transfer.WindowSize = def.Transfer.WindowSize
	}
	if c.Transfer.ChunkSize == 0 {
		c.Transfer.ChunkSize = def.Transfer.ChunkSize
	}
	if c.Transfer.BitmapFlushInterval == 0 {
		c.Transfer.BitmapFlushInterval = def.Transfer.BitmapFlushInterval
	}
	if c.Security.Encryption == "" {
		c.Security.Encryption = "off"
	}
	return c.Validate()
}
