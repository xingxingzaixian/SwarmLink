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
	"time"

	"github.com/BurntSushi/toml"

	"github.com/swarmlink/swarmlink/internal/domain/peer"
)

// AppName 是配置目录名。
const AppName = "SwarmLink"

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
		AutoStart   bool   `toml:"auto_start"`
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
		MaxConcurrent       int      `toml:"max_concurrent"`
		WindowSize          int      `toml:"window_size"`
		ChunkSize           int64    `toml:"chunk_size"`
		MaxUploadMbps       int      `toml:"max_upload_mbps"`
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
	c.Transfer.BitmapFlushInterval = Duration(2 * time.Second)
	c.Transfer.HistoryKeep = 50
	c.Transfer.HistoryTTL = Duration(7 * 24 * time.Hour)

	c.Security.RequireAuth = true // 强烈建议保持 true
	c.Security.Encryption = "off"

	return c
}

// Dir 返回应用配置目录。
//
// 注意：macOS 上 os.UserConfigDir() 硬编码为 ~/Library/Application Support，
// 不响应 XDG_CONFIG_HOME —— 这是标准库的既定行为。
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: user config dir: %w", err)
	}
	return filepath.Join(base, AppName), nil
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
