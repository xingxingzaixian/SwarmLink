package udp

import (
	"fmt"
	"net"
	"strings"
)

// Interface 是一个可用于发现的本机网络接口（含 IPv4 地址与子网广播地址）。
type Interface struct {
	Name      string
	IP        net.IP
	Mask      net.IPMask
	Broadcast net.IP
}

// SubnetString 返回 "ip/maskbits"，用于 ANNOUNCE.subnet（P-2 地址重叠检测）。
func (i Interface) SubnetString() string {
	ones, _ := i.Mask.Size()
	return fmt.Sprintf("%s/%d", i.IP.String(), ones)
}

// Contains 判断 ip 是否属于该接口子网（用于「种子回包时选对 subnet」）。
func (i Interface) Contains(ip net.IP) bool {
	return i.IP.Mask(i.Mask).Equal(ip.Mask(i.Mask))
}

// virtualPrefixes 是常见虚拟网卡名前缀（架构书 4.5 第 2 步）。
var virtualPrefixes = []string{
	"lo", "docker", "br-", "veth", "virbr", "vmnet", "vboxnet",
	"utun", "tun", "tap", "tailscale", "zerotier", "wg", "zt",
	"hyper-v", "vethernet", "npcap", "bridge", "awdl",
}

func looksVirtual(name string) bool {
	l := strings.ToLower(name)
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

func matchesList(name string, patterns []string) bool {
	l := strings.ToLower(name)
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(l, strings.TrimSuffix(p, "*")) {
				return true
			}
			continue
		}
		if l == p {
			return true
		}
	}
	return false
}

// EnumerateInterfaces 枚举可用于发现的接口。
//
// 步骤（架构书 4.5）：
//  1. 排除 loopback / link-local(169.254) / 未 UP
//  2. 排除虚拟网卡（按名称模式）
//  3. allow 非空时只保留命中项；deny 命中即排除
func EnumerateInterfaces(allow, deny []string) ([]Interface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("udp: list interfaces: %w", err)
	}

	var out []Interface
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 {
			continue
		}
		if ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if matchesList(ifi.Name, deny) {
			continue
		}
		if len(allow) > 0 {
			if !matchesList(ifi.Name, allow) {
				continue
			}
		} else if looksVirtual(ifi.Name) {
			continue
		}

		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			if ip4.IsLinkLocalUnicast() {
				continue
			}
			mask := ipnet.Mask
			if len(mask) == 16 {
				mask = mask[12:]
			}
			m := make(net.IPMask, 4)
			copy(m, mask)
			out = append(out, Interface{
				Name:      ifi.Name,
				IP:        ip4,
				Mask:      m,
				Broadcast: subnetBroadcast(ip4, m),
			})
		}
	}
	return out, nil
}

// subnetBroadcast 由 IP + netmask 计算子网广播地址。
// 不使用 255.255.255.255：路由器、IGMP snooping 交换机会丢弃它，
// macOS 也默认禁止非特权进程向它发送。
func subnetBroadcast(ip net.IP, mask net.IPMask) net.IP {
	b := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		b[i] = ip[i] | ^mask[i]
	}
	return b
}
