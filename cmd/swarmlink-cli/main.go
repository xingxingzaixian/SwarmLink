// Command swarmlink-cli 是 SwarmLink 的命令行前端。
//
// 它同时承担两个角色：
//  1. M1 交付物 —— 验证「identity → discovery → transport → store → 读回」全链路
//  2. 长期调试工具 —— P2P 排障在第一现场最缺的就是一个能直连协议层的入口
//
// 装配逻辑全部在 internal/bootstrap（唯一组合根），本文件只做参数解析与交互。
// 因此 CLI 上跑通的链路，GUI 上必然也通。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/swarmlink/swarmlink/internal/bootstrap"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// cli 只持有装配完成的节点与少量交互状态。
type cli struct {
	node *bootstrap.Node
	self identity.NodeID
}

func main() {
	var (
		name        = flag.String("name", "", "显示名（默认取主机名）")
		dataDir     = flag.String("data-dir", "", "配置/数据目录（默认用户配置目录）")
		downloadDir = flag.String("download-dir", "", "接收文件目录（默认配置中的 download.default_dir）")
		tcpPort     = flag.Int("tcp-port", -1, "TCP 端口（-1 = 用配置值，0 = 自动）")
		udpPort     = flag.Int("udp-port", -1, "UDP 端口（-1 = 用配置值，0 = 自动）")
		useMem      = flag.Bool("mem", false, "使用内存存储（不落盘，便于试验）")
		seedOnly    = flag.Bool("seed-only", false, "关闭自动广播发现，只用种子（VPN/安全敏感环境）")
		selfCheck   = flag.Bool("self-check", false, "启动后执行 P-1/P-2 部署前置条件自检并退出")
		verbose     = flag.Bool("v", false, "输出调试日志")
	)
	var seeds stringList
	flag.Var(&seeds, "seed", "种子地址 ip:udp_port（可重复）")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}

	if err := run(runOptions{
		name: *name, dataDir: *dataDir, downloadDir: *downloadDir,
		tcpPort: *tcpPort, udpPort: *udpPort,
		useMem: *useMem, seedOnly: *seedOnly, selfCheck: *selfCheck,
		seeds: seeds, level: level,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

type runOptions struct {
	name        string
	dataDir     string
	downloadDir string
	tcpPort     int
	udpPort     int
	useMem      bool
	seedOnly    bool
	selfCheck   bool
	seeds       stringList
	level       slog.Level
}

func run(o runOptions) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var extra []peer.SeedAddr
	for _, raw := range o.seeds {
		addr, err := peer.ParseSeedAddr(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "警告: %v\n", err)
			continue
		}
		extra = append(extra, addr)
	}

	node, err := bootstrap.Start(ctx, bootstrap.Options{
		Name:        o.name,
		DataDir:     o.dataDir,
		DownloadDir: o.downloadDir,
		TCPPort:     o.tcpPort,
		UDPPort:     o.udpPort,
		UseMem:      o.useMem,
		SeedOnly:    o.seedOnly,
		ExtraSeeds:  extra,
		Level:       o.level,
	})
	if err != nil {
		return err
	}
	defer node.Close()

	cfg := node.Cfg
	fmt.Printf("SwarmLink CLI\n")
	fmt.Printf("  显示名  : %s\n", cfg.General.DisplayName)
	fmt.Printf("  NodeID  : %s\n", node.KP.NodeID())
	fmt.Printf("  配置目录: %s\n", node.ConfigDir)
	fmt.Printf("  接收目录: %s\n", cfg.Download.DefaultDir)
	if o.useMem {
		fmt.Printf("  存储    : memory（不落盘）\n")
	} else {
		fmt.Printf("  存储    : %s\n", configDBPath(node.ConfigDir))
	}
	for _, notice := range node.Notices {
		fmt.Printf("  注意    : %s\n", notice)
	}
	fmt.Printf("  监听    : TCP :%d / UDP :%d\n", node.TCPPort, node.UDPPort)
	if len(node.Ifaces) > 0 {
		fmt.Printf("  网卡    : %s\n", strings.Join(node.Ifaces, ", "))
	} else {
		fmt.Printf("  网卡    : 未找到可用物理网卡（跨网段场景可忽略）\n")
	}
	if n := node.Registry.Len(); n > 0 {
		list := make([]string, 0, n)
		for _, s := range node.Registry.Snapshot() {
			list = append(list, s.Addr.String())
		}
		fmt.Printf("  种子    : %s\n", strings.Join(list, ", "))
	}
	fmt.Printf("\n输入 /help 查看命令。\n\n")

	c := &cli{node: node, self: node.KP.NodeID()}

	if o.selfCheck {
		c.runSelfCheck(ctx)
		return nil
	}

	c.subscribeEvents()
	c.repl(ctx)
	fmt.Println("正在退出…")
	return nil
}
