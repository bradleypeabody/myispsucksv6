package lanaddr

import (
	"net/netip"
	"testing"
)

func TestCompute(t *testing.T) {
	tests := []struct {
		prefix string
		suffix string
		want   string
	}{
		{"2607:fb90:1234:5678::/64", "::1", "2607:fb90:1234:5678::1"},
		{"2607:fb90:1234:5678::/64", "::dead:beef", "2607:fb90:1234:5678::dead:beef"},
		{"2001:db8::/64", "::1", "2001:db8::1"},
		// Only the lower 8 bytes of suffix are used as the host part.
		{"2607:fb90:1234:5678::/64", "fe80::1", "2607:fb90:1234:5678::1"},
	}
	for _, tt := range tests {
		prefix := netip.MustParsePrefix(tt.prefix)
		suffix := netip.MustParseAddr(tt.suffix)
		want := netip.MustParseAddr(tt.want)
		got := Compute(prefix, suffix)
		if got != want {
			t.Errorf("Compute(%s, %s) = %s, want %s", tt.prefix, tt.suffix, got, want)
		}
	}
}

func TestSetPrefixUpdatesCurrentAndNoOpsOnRepeat(t *testing.T) {
	suffix := netip.MustParseAddr("::1")
	m := &Manager{Interface: "eth0", Suffix: suffix}

	pfx := netip.MustParsePrefix("2607:fb90:1234:5678::/64")
	m.SetPrefix(pfx)

	want := Compute(pfx, suffix)
	if m.current != want {
		t.Errorf("after SetPrefix: current = %s, want %s", m.current, want)
	}

	// Calling again with the same prefix is a no-op (computed addr is identical).
	m.SetPrefix(pfx)
	if m.current != want {
		t.Errorf("after second SetPrefix: current = %s, want %s", m.current, want)
	}
}

func TestSetPrefixZeroClearsCurrent(t *testing.T) {
	suffix := netip.MustParseAddr("::1")
	m := &Manager{Interface: "eth0", Suffix: suffix}

	m.SetPrefix(netip.MustParsePrefix("2607:fb90:1234:5678::/64"))
	m.SetPrefix(netip.Prefix{}) // zero prefix = prefix removed

	if m.current.IsValid() {
		t.Errorf("expected zero current after SetPrefix(zero), got %s", m.current)
	}
}

func TestSetPrefixChange(t *testing.T) {
	suffix := netip.MustParseAddr("::1")
	m := &Manager{Interface: "eth0", Suffix: suffix}

	pfx1 := netip.MustParsePrefix("2607:fb90:1111:2222::/64")
	pfx2 := netip.MustParsePrefix("2607:fb90:aaaa:bbbb::/64")

	m.SetPrefix(pfx1)
	m.SetPrefix(pfx2)

	want := Compute(pfx2, suffix)
	if m.current != want {
		t.Errorf("after prefix change: current = %s, want %s", m.current, want)
	}
}
