package udp

import (
	"net"
	"testing"
)

func TestSubnetBroadcastUsesNetmaskNotLimitedBroadcast(t *testing.T) {
	ip := net.ParseIP("192.168.1.37").To4()
	mask := net.CIDRMask(24, 32)
	got := subnetBroadcast(ip, mask)
	if got.String() != "192.168.1.255" {
		t.Fatalf("want 192.168.1.255 got %s", got)
	}
	if got.Equal(net.IPv4bcast) {
		t.Fatal("must not use 255.255.255.255")
	}

	// /16 也要正确
	got = subnetBroadcast(net.ParseIP("10.20.3.9").To4(), net.CIDRMask(16, 32))
	if got.String() != "10.20.255.255" {
		t.Fatalf("want 10.20.255.255 got %s", got)
	}
}

func TestInterfaceSubnetString(t *testing.T) {
	i := Interface{IP: net.ParseIP("192.168.1.37").To4(), Mask: net.CIDRMask(24, 32)}
	if got := i.SubnetString(); got != "192.168.1.37/24" {
		t.Fatalf("unexpected subnet string %q", got)
	}
}

func TestInterfaceContains(t *testing.T) {
	i := Interface{IP: net.ParseIP("192.168.1.10").To4(), Mask: net.CIDRMask(24, 32)}
	if !i.Contains(net.ParseIP("192.168.1.99").To4()) {
		t.Fatal("should contain same-subnet ip")
	}
	if i.Contains(net.ParseIP("192.168.2.99").To4()) {
		t.Fatal("must not contain different subnet")
	}
}

func TestLooksVirtual(t *testing.T) {
	virtual := []string{"docker0", "veth1234", "utun3", "vmnet8", "vboxnet0", "tailscale0", "wg0", "lo0", "br-abc"}
	for _, n := range virtual {
		if !looksVirtual(n) {
			t.Fatalf("%q should be virtual", n)
		}
	}
	physical := []string{"en0", "eth0", "wlan0", "Wi-Fi", "Ethernet"}
	for _, n := range physical {
		if looksVirtual(n) {
			t.Fatalf("%q should not be virtual", n)
		}
	}
}

func TestMatchesListSupportsWildcards(t *testing.T) {
	if !matchesList("utun3", []string{"utun*"}) {
		t.Fatal("wildcard should match")
	}
	if !matchesList("en0", []string{"en0"}) {
		t.Fatal("exact should match")
	}
	if matchesList("en0", []string{"eth*"}) {
		t.Fatal("must not match")
	}
}

func TestEnumerateInterfacesExcludesLoopbackAndLinkLocal(t *testing.T) {
	// 该测试只要求「不 panic 且不含 loopback / link-local」
	ifaces, err := EnumerateInterfaces(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range ifaces {
		if i.IP.IsLoopback() {
			t.Fatalf("loopback must be excluded: %s", i.Name)
		}
		if i.IP.IsLinkLocalUnicast() {
			t.Fatalf("link-local must be excluded: %s", i.Name)
		}
		if i.Broadcast == nil || i.Broadcast.Equal(net.IPv4bcast) {
			t.Fatalf("broadcast address not derived from netmask: %s", i.Name)
		}
	}
}
