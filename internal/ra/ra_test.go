package ra

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/mdlayher/ndp"
)

var (
	testIfi = &net.Interface{
		Name:         "eth0",
		HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
	}
	testPrefix = netip.MustParsePrefix("2607:fb90:1234:5678::/64")
	testSuffix = netip.MustParseAddr("::1")
)

func TestBuildRAHasCorrectPrefix(t *testing.T) {
	e := &Emitter{
		Interface: "eth0",
		Suffix:    testSuffix,
	}
	msg := e.buildRA(testIfi, testPrefix, e.validLifetime(), e.preferredLifetime())

	var pi *ndp.PrefixInformation
	for _, opt := range msg.Options {
		if p, ok := opt.(*ndp.PrefixInformation); ok {
			pi = p
			break
		}
	}
	if pi == nil {
		t.Fatal("no PrefixInformation option in RA")
	}
	if pi.Prefix != testPrefix.Masked().Addr() {
		t.Errorf("prefix = %s, want %s", pi.Prefix, testPrefix.Masked().Addr())
	}
	if pi.PrefixLength != 64 {
		t.Errorf("prefix length = %d, want 64", pi.PrefixLength)
	}
	if !pi.AutonomousAddressConfiguration {
		t.Error("AutonomousAddressConfiguration should be true")
	}
	if !pi.OnLink {
		t.Error("OnLink should be true")
	}
}

func TestBuildRAHasCorrectLifetimes(t *testing.T) {
	e := &Emitter{ValidLifetime: 7200, PreferredLifetime: 3600}
	msg := e.buildRA(testIfi, testPrefix, e.validLifetime(), e.preferredLifetime())

	for _, opt := range msg.Options {
		pi, ok := opt.(*ndp.PrefixInformation)
		if !ok {
			continue
		}
		if pi.ValidLifetime != 7200*time.Second {
			t.Errorf("valid lifetime = %v, want %v", pi.ValidLifetime, 7200*time.Second)
		}
		if pi.PreferredLifetime != 3600*time.Second {
			t.Errorf("preferred lifetime = %v, want %v", pi.PreferredLifetime, 3600*time.Second)
		}
	}
}

func TestBuildRADeprecation(t *testing.T) {
	e := &Emitter{}
	// Deprecation uses preferred_lifetime=0, short valid_lifetime.
	msg := e.buildRA(testIfi, testPrefix, 2*time.Minute, 0)

	for _, opt := range msg.Options {
		pi, ok := opt.(*ndp.PrefixInformation)
		if !ok {
			continue
		}
		if pi.PreferredLifetime != 0 {
			t.Errorf("deprecation: preferred lifetime = %v, want 0", pi.PreferredLifetime)
		}
		if pi.ValidLifetime != 2*time.Minute {
			t.Errorf("deprecation: valid lifetime = %v, want 2m", pi.ValidLifetime)
		}
	}
}

func TestBuildRAHasLinkLayerOption(t *testing.T) {
	e := &Emitter{}
	msg := e.buildRA(testIfi, testPrefix, e.validLifetime(), e.preferredLifetime())

	for _, opt := range msg.Options {
		lla, ok := opt.(*ndp.LinkLayerAddress)
		if !ok {
			continue
		}
		if lla.Direction != ndp.Source {
			t.Errorf("link-layer direction = %v, want Source", lla.Direction)
		}
		if lla.Addr.String() != testIfi.HardwareAddr.String() {
			t.Errorf("link-layer addr = %s, want %s", lla.Addr, testIfi.HardwareAddr)
		}
		return
	}
	t.Error("no LinkLayerAddress option in RA")
}

func TestBuildRAHasMTUOption(t *testing.T) {
	e := &Emitter{MTU: 9000}
	msg := e.buildRA(testIfi, testPrefix, e.validLifetime(), e.preferredLifetime())

	for _, opt := range msg.Options {
		if m, ok := opt.(*ndp.MTU); ok {
			if m.MTU != 9000 {
				t.Errorf("MTU = %d, want 9000", m.MTU)
			}
			return
		}
	}
	t.Error("no MTU option in RA")
}

func TestResolveDNSSelf(t *testing.T) {
	e := &Emitter{
		Suffix:     testSuffix,
		DNSServers: []string{"self"},
	}
	servers := e.resolveDNS(testPrefix)
	if len(servers) != 1 {
		t.Fatalf("want 1 DNS server, got %d", len(servers))
	}
	want := netip.MustParseAddr("2607:fb90:1234:5678::1")
	if servers[0] != want {
		t.Errorf("DNS server = %s, want %s", servers[0], want)
	}
}

func TestResolveDNSExplicit(t *testing.T) {
	explicit := "2001:db8::53"
	e := &Emitter{DNSServers: []string{explicit}}
	servers := e.resolveDNS(testPrefix)
	if len(servers) != 1 {
		t.Fatalf("want 1 DNS server, got %d", len(servers))
	}
	if servers[0].String() != explicit {
		t.Errorf("DNS server = %s, want %s", servers[0], explicit)
	}
}

func TestResolveDNSDefaultIsSelf(t *testing.T) {
	e := &Emitter{Suffix: testSuffix} // DNSServers empty → default ["self"]
	servers := e.resolveDNS(testPrefix)
	if len(servers) != 1 {
		t.Fatalf("want 1 default DNS server, got %d", len(servers))
	}
}

func TestBuildRAHasRDNSSOption(t *testing.T) {
	e := &Emitter{Suffix: testSuffix}
	msg := e.buildRA(testIfi, testPrefix, e.validLifetime(), e.preferredLifetime())

	for _, opt := range msg.Options {
		if _, ok := opt.(*ndp.RecursiveDNSServer); ok {
			return
		}
	}
	t.Error("no RecursiveDNSServer option in RA")
}

func TestJitterInRange(t *testing.T) {
	d := 200 * time.Second
	for i := 0; i < 1000; i++ {
		j := jitter(d)
		if j < -d/4 || j > d/4 {
			t.Errorf("jitter(%v) = %v, outside [-%v, +%v]", d, j, d/4, d/4)
		}
	}
}

func TestDefaults(t *testing.T) {
	e := &Emitter{}
	if e.raInterval() != 200*time.Second {
		t.Errorf("default interval = %v, want 200s", e.raInterval())
	}
	if e.routerLifetimeDuration() != 1800*time.Second {
		t.Errorf("default router lifetime = %v, want 1800s", e.routerLifetimeDuration())
	}
	if e.mtu() != 1500 {
		t.Errorf("default MTU = %d, want 1500", e.mtu())
	}
	if e.validLifetime() != 86400*time.Second {
		t.Errorf("default valid lifetime = %v, want 86400s", e.validLifetime())
	}
	if e.preferredLifetime() != 14400*time.Second {
		t.Errorf("default preferred lifetime = %v, want 14400s", e.preferredLifetime())
	}
}

func TestSetPrefixDrainAndReplace(t *testing.T) {
	e := &Emitter{}
	p1 := netip.MustParsePrefix("2607:fb90:1111:2222::/64")
	p2 := netip.MustParsePrefix("2607:fb90:aaaa:bbbb::/64")

	e.SetPrefix(p1)
	e.SetPrefix(p2) // should drain p1 and replace with p2

	select {
	case got := <-e.updateCh:
		if got != p2 {
			t.Errorf("got %s, want %s", got, p2)
		}
	default:
		t.Error("channel empty after SetPrefix")
	}
}
