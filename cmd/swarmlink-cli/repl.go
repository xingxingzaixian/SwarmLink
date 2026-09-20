package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/swarmlink/swarmlink/internal/adapters/config"
	"github.com/swarmlink/swarmlink/internal/domain/group"
	"github.com/swarmlink/swarmlink/internal/domain/identity"
	"github.com/swarmlink/swarmlink/internal/domain/message"
	"github.com/swarmlink/swarmlink/internal/domain/peer"
	"github.com/swarmlink/swarmlink/internal/domain/ports"
	"github.com/swarmlink/swarmlink/internal/domain/protocol"
	"github.com/swarmlink/swarmlink/internal/infra/eventbus"
)

func configDBPath(dir string) string { return config.DBPath(dir) }

func (c *cli) repl(ctx context.Context) {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	fmt.Print("> ")
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			fmt.Print("> ")
			continue
		}
		if !strings.HasPrefix(line, "/") {
			fmt.Println("（以 / 开头的才是命令，输入 /help 查看全部）")
			fmt.Print("> ")
			continue
		}
		if quit := c.handleCommand(ctx, line); quit {
			return
		}
		fmt.Print("> ")
	}
}

// subscribeEvents 把领域事件打到终端。
//
// 由于所有内部事件都过总线，这个功能几乎零成本 —— 正是「事件驱动核心」
// 带来的那份礼物（架构书 3.5）。
func (c *cli) subscribeEvents() {
	bus := c.node.Bus

	bus.Subscribe(eventbus.TopicPeerDiscovered, func(p any) {
		pp, ok := p.(peer.Peer)
		if !ok {
			return
		}
		fmt.Printf("\r[发现] %s (%s) @ %s\n> ", displayName(pp), shorten(pp.NodeID.String()), orDash(pp.LastAddr))
	})
	bus.Subscribe(eventbus.TopicPeerOnline, func(p any) {
		ev, ok := p.(eventbus.PeerOnline)
		if !ok {
			return
		}
		fmt.Printf("\r[在线] %s (rtt=%v)\n> ", shorten(ev.NodeID), ev.RTT)
	})
	bus.Subscribe(eventbus.TopicPeerOffline, func(p any) {
		ev, ok := p.(eventbus.PeerOffline)
		if !ok {
			return
		}
		// 空闲回收只代表连接不再可用，节点仍可能在线 —— 不打扰用户
		if ev.Reason == "idle_timeout" {
			return
		}
		fmt.Printf("\r[离线] %s (%s)\n> ", shorten(ev.NodeID), ev.Reason)
	})
	bus.Subscribe(eventbus.TopicChatReceived, func(p any) {
		m, ok := p.(message.Message)
		if !ok {
			return
		}
		who := shorten(m.SenderID.String())
		if pp, ok := c.node.PeerDir.Get(m.SenderID); ok && pp.DisplayName != "" {
			who = pp.DisplayName
		}
		where := ""
		if !message.IsDirectConv(m.ConvID) {
			where = fmt.Sprintf(" [群 %s]", shorten(m.ConvID))
		}
		fmt.Printf("\r[消息]%s %s: %s\n> ", where, who, m.Content)
	})
	bus.Subscribe(eventbus.TopicChatDelivered, func(p any) {
		ack, ok := p.(protocol.ChatAck)
		if !ok {
			return
		}
		fmt.Printf("\r[已送达] %s\n> ", shorten(ack.MsgID))
	})
	bus.Subscribe(eventbus.TopicTransferProgress, func(p any) {
		ev, ok := p.(eventbus.TransferProgress)
		if !ok {
			return
		}
		fmt.Printf("\r[传输] %s %.1f%%\n> ", shorten(ev.JobID), ev.Percent)
	})
	bus.Subscribe(eventbus.TopicTransferDone, func(p any) {
		ev, ok := p.(eventbus.TransferDone)
		if !ok {
			return
		}
		fmt.Printf("\r[完成] 文件已保存: %s\n> ", ev.Path)
	})
	bus.Subscribe(eventbus.TopicTransferError, func(p any) {
		ev, ok := p.(eventbus.TransferError)
		if !ok {
			return
		}
		fmt.Printf("\r[传输错误] %s: %v\n> ", shorten(ev.JobID), ev.Err)
	})
	bus.Subscribe(eventbus.TopicNetError, func(p any) {
		ev, ok := p.(eventbus.NetError)
		if !ok {
			return
		}
		fmt.Printf("\r[网络错误] %s: %v\n> ", ev.Op, ev.Err)
	})
	bus.Subscribe(eventbus.TopicGroupUpdated, func(p any) {
		g, ok := p.(group.Group)
		if !ok {
			return
		}
		fmt.Printf("\r[群] %s epoch=%d 成员=%d\n> ", g.Name, g.Epoch, len(g.ActiveMembers()))
	})
}

func (c *cli) handleCommand(ctx context.Context, line string) bool {
	fields := strings.Fields(line)
	cmd := fields[0]
	args := fields[1:]

	switch cmd {
	case "/quit", "/exit", "/q":
		return true
	case "/help", "/?":
		printHelp()
	case "/me":
		fmt.Printf("NodeID = %s\n", c.self)
		fmt.Printf("显示名 = %s\n", c.node.Cfg.General.DisplayName)
		fmt.Printf("监听   = TCP :%d / UDP :%d\n", c.node.TCPPort, c.node.UDPPort)
	case "/peers":
		c.printPeers()
	case "/seeds":
		c.printSeeds()
	case "/diag":
		c.printDiag()
	case "/msg":
		if len(args) < 2 {
			fmt.Println("用法: /msg <对端> <文本>")
			break
		}
		c.cmdMsg(ctx, args[0], strings.Join(args[1:], " "))
	case "/history":
		if len(args) < 1 {
			fmt.Println("用法: /history <对端> [条数]")
			break
		}
		c.cmdHistory(args)
	case "/send":
		if len(args) < 2 {
			fmt.Println("用法: /send <对端> <本地文件路径>")
			break
		}
		c.cmdSend(ctx, args[0], strings.Join(args[1:], " "))
	case "/group":
		c.cmdGroup(ctx, args)
	default:
		fmt.Printf("未知命令 %s（输入 /help）\n", cmd)
	}
	return false
}

func printHelp() {
	fmt.Print(`命令一览：
  /me                         显示本机身份
  /peers                      列出已发现节点
  /seeds                      显示种子状态与退避
  /diag                       运行统计（节点/连接/任务）
  /msg <对端> <文本>           发送单聊消息（对端可用 NodeID 前缀或显示名）
  /history <对端> [条数]       查看本地历史（默认 20 条）
  /send <对端> <路径>          发送文件（支持断点续传）
  /group create <名称> [成员…]  建群（本方为群主，上限 20 人）
  /group list                 列出本机已知群
  /group add <群ID> <对端>     群主添加成员（epoch+1 并广播）
  /group msg <群ID> <文本>     群发消息
  /quit                       退出
`)
}

func (c *cli) printPeers() {
	all := c.node.PeerDir.List(ports.PeerFilter{})
	if len(all) == 0 {
		fmt.Println("尚未发现任何节点。若为跨网段部署，请检查 /seeds。")
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].NodeID.Less(all[j].NodeID) })

	fmt.Printf("%-18s %-14s %-10s %-22s %-18s %-6s %s\n",
		"NodeID", "显示名", "状态", "地址", "子网(P-2)", "已连", "来源")
	for _, p := range all {
		_, connected := c.node.Conn.SessionOf(p.NodeID)
		fmt.Printf("%-18s %-14s %-10s %-22s %-18s %-6s %s\n",
			p.NodeID.String(), truncate(displayName(p), 14), string(p.State),
			orDash(p.LastAddr), orDash(p.Subnet), yesNo(connected), orDash(p.Source))
	}
}

func (c *cli) printSeeds() {
	snap := c.node.Registry.Snapshot()
	if len(snap) == 0 {
		fmt.Println("未配置种子（单网段部署无需种子）")
		return
	}
	fmt.Printf("%-24s %-6s %-18s %-8s %-18s %s\n", "地址", "失败", "NodeID", "TCP", "子网", "已学 epoch")
	for _, s := range snap {
		fmt.Printf("%-24s %-6d %-18s %-8d %-18s %d\n",
			s.Addr.String(), s.FailCnt, orDash(s.NodeID), s.TCPPort, orDash(s.Subnet), s.LastEpoch)
	}
}

func (c *cli) printDiag() {
	online, discovered := 0, 0
	all := c.node.PeerDir.List(ports.PeerFilter{})
	for _, p := range all {
		switch p.State {
		case peer.StateOnline:
			online++
		case peer.StateDiscovered:
			discovered++
		}
	}
	fmt.Printf("节点目录 : %d 条（online=%d, discovered=%d）\n", len(all), online, discovered)
	// ADR-009：在线状态走 UDP announce，TCP 只按需拨号 —— 空闲时该值应为 0
	fmt.Printf("常驻连接 : %d（ADR-009：常态目标 ≤ 10，空闲应为 0）\n", c.node.Conn.SessionCount())
	fmt.Printf("活跃传输 : %d\n", c.node.TransferApp.ActiveJobs())
	fmt.Printf("种子     : %d 颗\n", c.node.Registry.Len())
	fmt.Printf("本机监听 : TCP :%d / UDP :%d\n", c.node.TCPPort, c.node.UDPPort)
}

func (c *cli) cmdMsg(ctx context.Context, ref, text string) {
	p, err := resolvePeer(c.node.PeerDir, ref)
	if err != nil {
		fmt.Println("错误:", err)
		return
	}
	msg, err := c.node.ChatApp.SendMessage(ctx, p.NodeID, text)
	if err != nil {
		fmt.Println("发送失败:", err)
		return
	}
	fmt.Printf("已发送 → %s（msg_id=%s，等待 ACK；未确认消息由 outbox 自动重发）\n",
		displayName(p), shorten(msg.MsgID))
}

func (c *cli) cmdHistory(args []string) {
	p, err := resolvePeer(c.node.PeerDir, args[0])
	if err != nil {
		fmt.Println("错误:", err)
		return
	}
	limit := 20
	if len(args) > 1 {
		if _, err := fmt.Sscanf(args[1], "%d", &limit); err != nil || limit <= 0 {
			limit = 20
		}
	}

	conv := message.DirectConvID(c.self, p.NodeID)
	ms, err := c.node.ChatApp.History(conv, limit, 0)
	if err != nil {
		fmt.Println("查询失败:", err)
		return
	}
	if len(ms) == 0 {
		fmt.Println("（无历史）")
		return
	}
	// 存储返回「新→旧」，展示时翻转
	for i := len(ms) - 1; i >= 0; i-- {
		m := ms[i]
		arrow := "←"
		if m.Direction == message.DirectionOut {
			arrow = "→"
		}
		fmt.Printf("  %s [%s] %s\n", arrow, m.State, m.Content)
	}
}

func (c *cli) cmdSend(ctx context.Context, ref, path string) {
	p, err := resolvePeer(c.node.PeerDir, ref)
	if err != nil {
		fmt.Println("错误:", err)
		return
	}
	if _, err := os.Stat(path); err != nil {
		fmt.Println("无法读取文件:", err)
		return
	}
	fmt.Printf("开始发送 %s → %s（可中断，重发同一文件将自动续传）\n", filepath.Base(path), displayName(p))
	go func() {
		job, err := c.node.TransferApp.SendFile(ctx, p.NodeID, path)
		if err != nil {
			fmt.Printf("\r[发送失败] %v\n> ", err)
			return
		}
		fmt.Printf("\r[发送完成] %s（%d 字节）\n> ", job.FileName, job.FileSize)
	}()
}

func (c *cli) cmdGroup(ctx context.Context, args []string) {
	if len(args) == 0 {
		fmt.Println("用法: /group create|list|add|msg …")
		return
	}
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "create":
		if len(rest) == 0 {
			fmt.Println("用法: /group create <名称> [成员前缀…]")
			return
		}
		name := rest[0]
		var ids []identity.NodeID
		for _, ref := range rest[1:] {
			p, err := resolvePeer(c.node.PeerDir, ref)
			if err != nil {
				fmt.Printf("  跳过 %q: %v\n", ref, err)
				continue
			}
			ids = append(ids, p.NodeID)
		}
		g, err := c.node.GroupApp.CreateGroup(ctx, name, ids)
		if err != nil {
			fmt.Println("建群失败:", err)
			return
		}
		fmt.Printf("已建群 %s（id=%s，成员 %d，epoch=%d）\n", g.Name, g.ID, len(g.ActiveMembers()), g.Epoch)

	case "list":
		gs := c.node.GroupApp.List()
		if len(gs) == 0 {
			fmt.Println("（本机没有群）")
			return
		}
		for _, g := range gs {
			role := "成员"
			if g.OwnerID == c.self {
				role = "群主"
			}
			fmt.Printf("  %s  %s  epoch=%d  成员=%d  [%s]\n", g.ID, g.Name, g.Epoch, len(g.ActiveMembers()), role)
		}

	case "add":
		if len(rest) < 2 {
			fmt.Println("用法: /group add <群ID> <对端>")
			return
		}
		p, err := resolvePeer(c.node.PeerDir, rest[1])
		if err != nil {
			fmt.Println("错误:", err)
			return
		}
		g, err := c.node.GroupApp.AddMember(ctx, rest[0], p.NodeID)
		if err != nil {
			fmt.Println("添加失败:", err)
			return
		}
		fmt.Printf("已加入 %s（成员 %d，epoch=%d）\n", displayName(p), len(g.ActiveMembers()), g.Epoch)

	case "msg":
		if len(rest) < 2 {
			fmt.Println("用法: /group msg <群ID> <文本>")
			return
		}
		gid := c.resolveGroupID(rest[0])
		if gid == "" {
			fmt.Printf("未找到匹配的群 %q\n", rest[0])
			return
		}
		msg, err := c.node.GroupApp.SendGroupMessage(ctx, gid, strings.Join(rest[1:], " "))
		if err != nil {
			fmt.Println("群发失败:", err)
			return
		}
		fmt.Printf("已群发（msg_id=%s，将扇出到在线成员；离线成员不会被暂存）\n", shorten(msg.MsgID))

	default:
		fmt.Println("用法: /group create|list|add|msg …")
	}
}

// resolveGroupID 支持用群 ID 前缀或群名匹配。
func (c *cli) resolveGroupID(ref string) string {
	var matches []string
	for _, g := range c.node.GroupApp.List() {
		if strings.HasPrefix(g.ID, ref) || strings.EqualFold(g.Name, ref) {
			matches = append(matches, g.ID)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

// runSelfCheck 执行 P-1 / P-2 部署前置条件自检。
//
// 这两条是【网络侧的既定事实，代码救不了】，因此必须能在部署前被验证，
// 而不是等到现场才发现跨网段完全不通（架构书 0.5 节）。
func (c *cli) runSelfCheck(ctx context.Context) {
	fmt.Println("=== 部署前置条件自检 ===")

	seeds := c.node.Registry.Snapshot()
	if len(seeds) == 0 {
		fmt.Println("P-1/P-2 未配置种子，跳过（单网段部署无需自检）")
		return
	}

	c.node.DiscApp.RefreshSeeds(ctx)
	// 目录拉取是异步的（UDP 探测 → TCP 拉取），给它一点时间落地
	for i := 0; i < 30; i++ {
		if len(c.node.PeerDir.List(ports.PeerFilter{Source: peer.SourceSeed})) > 0 {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}

	learned := c.node.PeerDir.List(ports.PeerFilter{Source: peer.SourceSeed})
	if len(learned) > 0 {
		fmt.Printf("  [P-1 OK]   已通过种子探通并拉取到 %d 条跨网段目录\n", len(learned))
	} else {
		fmt.Println("  [P-1 FAIL] 无法通过种子获取目录 —— 跨网段将完全失效，只能引入中继。")
		fmt.Println("             请确认任意两网段的种子 IP 之间可 TCP 直连（三层路由可达，非 NAT 隔离）。")
	}

	subnets := map[string]int{}
	for _, p := range learned {
		if p.Subnet != "" {
			subnets[p.Subnet]++
		}
	}
	warned := false
	if c.node.SelfSubnet != "" {
		if n, ok := subnets[c.node.SelfSubnet]; ok && n > 0 {
			fmt.Printf("  [P-2 警告] 通过种子学到的条目与本机处于同一地址段 %s（%d 个节点）。\n"+
				"             若它们实际位于不同物理网段，说明各网段地址段重叠，跨网段寻址会崩溃。\n",
				c.node.SelfSubnet, n)
			warned = true
		}
	}
	if !warned {
		fmt.Println("  [P-2 未发现异常] 未观察到与地址段重叠一致的现象。")
	}

	fmt.Println("  种子状态：")
	for _, s := range c.node.Registry.Snapshot() {
		fmt.Printf("    - %-22s fail=%d node=%s tcp=%d subnet=%s\n",
			s.Addr.String(), s.FailCnt, orDash(s.NodeID), s.TCPPort, orDash(s.Subnet))
	}
}

// resolvePeer 把用户输入（NodeID 前缀 / 显示名 / 显示名子串）解析为唯一节点。
//
// P2P 排障时人手敲 16 位 NodeID 很痛苦，但又不能只按显示名匹配（重名很常见），
// 因此按「前缀 → 精确名 → 名字子串」的优先级解析，并在歧义时明确报错。
func resolvePeer(dir ports.PeerDirectory, ref string) (peer.Peer, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return peer.Peer{}, fmt.Errorf("对端不能为空")
	}
	all := dir.List(ports.PeerFilter{})

	lower := strings.ToLower(ref)
	var byPrefix, byExact, bySubstr []peer.Peer
	for _, p := range all {
		if strings.HasPrefix(p.NodeID.String(), lower) {
			byPrefix = append(byPrefix, p)
		}
		if p.DisplayName != "" && strings.EqualFold(p.DisplayName, ref) {
			byExact = append(byExact, p)
		}
		if p.DisplayName != "" && strings.Contains(strings.ToLower(p.DisplayName), lower) {
			bySubstr = append(bySubstr, p)
		}
	}

	for _, cand := range [][]peer.Peer{byPrefix, byExact, bySubstr} {
		switch len(cand) {
		case 1:
			return cand[0], nil
		case 0:
			continue
		default:
			names := make([]string, 0, len(cand))
			for _, p := range cand {
				names = append(names, fmt.Sprintf("%s(%s)", displayName(p), shorten(p.NodeID.String())))
			}
			return peer.Peer{}, fmt.Errorf("%q 有 %d 个匹配：%s", ref, len(cand), strings.Join(names, ", "))
		}
	}
	return peer.Peer{}, fmt.Errorf("未找到对端 %q（可用 /peers 查看）", ref)
}

func displayName(p peer.Peer) string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return shorten(p.NodeID.String())
}

func shorten(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}
