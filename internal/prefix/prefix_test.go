package prefix

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/bradleypeabody/myispsucksv6/internal/netlinkmon"
)

var (
	globalFilter = netip.MustParsePrefix("2000::/3")
	pfx1         = netip.MustParsePrefix("2607:fb90:1111:2222::/64")
	pfx2         = netip.MustParsePrefix("2607:fb90:aaaa:bbbb::/64")
	// same as pfx1 but with a host address (as the kernel would report it)
	pfx1host = netip.MustParsePrefix("2607:fb90:1111:2222:abcd:ef01:2345:6789/64")
	pfx2host = netip.MustParsePrefix("2607:fb90:aaaa:bbbb:dead:beef:cafe:1234/64")
)

type testCollector struct {
	mu      sync.Mutex
	changes []Change
}

func (c *testCollector) record(ch Change) {
	c.mu.Lock()
	c.changes = append(c.changes, ch)
	c.mu.Unlock()
}

func (c *testCollector) snapshot() []Change {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Change, len(c.changes))
	copy(out, c.changes)
	return out
}

func newManager(col *testCollector, initial netip.Prefix, debounce time.Duration) (*Manager, chan netlinkmon.AddrEvent) {
	ch := make(chan netlinkmon.AddrEvent, 16)
	m := &Manager{
		Interface:     "eth0",
		Filter:        globalFilter,
		Debounce:      debounce,
		OnChange:      col.record,
		InitialPrefix: initial,
	}
	return m, ch
}

func runManager(t *testing.T, m *Manager, ch chan netlinkmon.AddrEvent) (context.CancelFunc, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx, ch)
	}()
	wait := func() { cancel(); <-done }
	return cancel, wait
}

// settle gives the manager goroutine time to process buffered events.
func settle() { time.Sleep(20 * time.Millisecond) }

func TestAddedAction(t *testing.T) {
	col := &testCollector{}
	m, ch := newManager(col, netip.Prefix{}, 0)
	_, wait := runManager(t, m, ch)
	defer wait()

	ch <- netlinkmon.AddrEvent{Iface: "eth0", Addr: pfx1host, Added: true}
	settle()

	got := col.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 change, got %d", len(got))
	}
	if got[0].Action != ActionAdded {
		t.Errorf("want ActionAdded, got %s", got[0].Action)
	}
	if got[0].New != pfx1 {
		t.Errorf("want new=%s, got %s", pfx1, got[0].New)
	}
	if got[0].Old.IsValid() {
		t.Errorf("want zero old, got %s", got[0].Old)
	}
}

func TestChangedAction(t *testing.T) {
	// Changed only emerges when the debounce timer collapses a remove+add into
	// a single transition. With Debounce=0, each event fires immediately and
	// produces separate Removed/Added — so use a short debounce here.
	col := &testCollector{}
	m, ch := newManager(col, pfx1, 50*time.Millisecond)
	_, wait := runManager(t, m, ch)
	defer wait()

	ch <- netlinkmon.AddrEvent{Iface: "eth0", Addr: pfx1host, Added: false}
	ch <- netlinkmon.AddrEvent{Iface: "eth0", Addr: pfx2host, Added: true}
	time.Sleep(100 * time.Millisecond)

	got := col.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 change, got %d: %v", len(got), got)
	}
	if got[0].Action != ActionChanged {
		t.Errorf("want ActionChanged, got %s", got[0].Action)
	}
	if got[0].Old != pfx1 {
		t.Errorf("want old=%s, got %s", pfx1, got[0].Old)
	}
	if got[0].New != pfx2 {
		t.Errorf("want new=%s, got %s", pfx2, got[0].New)
	}
}

func TestRemovedAction(t *testing.T) {
	col := &testCollector{}
	m, ch := newManager(col, pfx1, 0)
	_, wait := runManager(t, m, ch)
	defer wait()

	ch <- netlinkmon.AddrEvent{Iface: "eth0", Addr: pfx1host, Added: false}
	settle()

	got := col.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 change, got %d", len(got))
	}
	if got[0].Action != ActionRemoved {
		t.Errorf("want ActionRemoved, got %s", got[0].Action)
	}
}

func TestFilterRejectsOutOfRange(t *testing.T) {
	col := &testCollector{}
	m, ch := newManager(col, netip.Prefix{}, 0)
	_, wait := runManager(t, m, ch)
	defer wait()

	// ULA address — outside 2000::/3
	ula := netip.MustParsePrefix("fd00:1:2:3:abcd::/64")
	ch <- netlinkmon.AddrEvent{Iface: "eth0", Addr: ula, Added: true}
	settle()

	if n := len(col.snapshot()); n != 0 {
		t.Errorf("want 0 changes for ULA, got %d", n)
	}
}

func TestFilterRejectsWrongIface(t *testing.T) {
	col := &testCollector{}
	m, ch := newManager(col, netip.Prefix{}, 0)
	_, wait := runManager(t, m, ch)
	defer wait()

	ch <- netlinkmon.AddrEvent{Iface: "eth1", Addr: pfx1host, Added: true}
	settle()

	if n := len(col.snapshot()); n != 0 {
		t.Errorf("want 0 changes for wrong iface, got %d", n)
	}
}

func TestDebounceCollapsesFastEvents(t *testing.T) {
	col := &testCollector{}
	m, ch := newManager(col, netip.Prefix{}, 100*time.Millisecond)
	_, wait := runManager(t, m, ch)
	defer wait()

	// Rapid flap: add pfx1, then quickly switch to pfx2.
	ch <- netlinkmon.AddrEvent{Iface: "eth0", Addr: pfx1host, Added: true}
	time.Sleep(10 * time.Millisecond)
	ch <- netlinkmon.AddrEvent{Iface: "eth0", Addr: pfx1host, Added: false}
	ch <- netlinkmon.AddrEvent{Iface: "eth0", Addr: pfx2host, Added: true}

	// Before debounce settles: no changes yet.
	time.Sleep(30 * time.Millisecond)
	if n := len(col.snapshot()); n != 0 {
		t.Errorf("want 0 changes before debounce, got %d", n)
	}

	// After debounce settles: exactly one change to pfx2.
	time.Sleep(120 * time.Millisecond)
	got := col.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 change after debounce, got %d: %v", len(got), got)
	}
	if got[0].New != pfx2 {
		t.Errorf("want new=%s, got %s", pfx2, got[0].New)
	}
}

func TestDebounceCancelledWhenPrefixReverts(t *testing.T) {
	col := &testCollector{}
	m, ch := newManager(col, pfx1, 100*time.Millisecond)
	_, wait := runManager(t, m, ch)
	defer wait()

	// Change to pfx2, then immediately revert to pfx1.
	ch <- netlinkmon.AddrEvent{Iface: "eth0", Addr: pfx1host, Added: false}
	ch <- netlinkmon.AddrEvent{Iface: "eth0", Addr: pfx2host, Added: true}
	time.Sleep(20 * time.Millisecond)
	ch <- netlinkmon.AddrEvent{Iface: "eth0", Addr: pfx2host, Added: false}
	ch <- netlinkmon.AddrEvent{Iface: "eth0", Addr: pfx1host, Added: true}

	// Wait well past debounce period — no change should fire.
	time.Sleep(200 * time.Millisecond)
	if n := len(col.snapshot()); n != 0 {
		t.Errorf("want 0 changes (prefix reverted), got %d", n)
	}
}

func TestSeedFromLiveFiresWhenDifferent(t *testing.T) {
	col := &testCollector{}
	m, _ := newManager(col, pfx1, 0)

	// State file said pfx1, but live interface has pfx2.
	m.SeedFromLive([]netip.Prefix{pfx2host})

	got := col.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 immediate change from SeedFromLive, got %d", len(got))
	}
	if got[0].Action != ActionChanged || got[0].New != pfx2 {
		t.Errorf("unexpected change: %+v", got[0])
	}
	if m.InitialPrefix != pfx2 {
		t.Errorf("InitialPrefix not updated: got %s", m.InitialPrefix)
	}
}

func TestSeedFromLiveNoFireWhenSame(t *testing.T) {
	col := &testCollector{}
	m, _ := newManager(col, pfx1, 0)

	m.SeedFromLive([]netip.Prefix{pfx1host})

	if n := len(col.snapshot()); n != 0 {
		t.Errorf("want no change when live matches initial, got %d", n)
	}
}

func TestNormalizeHostAddress(t *testing.T) {
	m := &Manager{Filter: globalFilter}
	p, ok := m.normalize(pfx1host)
	if !ok {
		t.Fatal("expected normalize to succeed for global unicast host addr")
	}
	if p != pfx1 {
		t.Errorf("want %s, got %s", pfx1, p)
	}
}
