package prefix

import (
	"context"
	"log/slog"
	"net/netip"
	"time"

	"github.com/bradleypeabody/myispsucksv6/internal/netlinkmon"
)

type Action string

const (
	ActionAdded   Action = "added"
	ActionChanged Action = "changed"
	ActionRemoved Action = "removed"
)

// Change describes a stable prefix transition on an upstream interface.
type Change struct {
	Interface string
	Old       netip.Prefix // zero value if Action == ActionAdded
	New       netip.Prefix // zero value if Action == ActionRemoved
	Action    Action
}

// Manager watches addr events for a single upstream interface, debounces
// them, and calls OnChange when the /64 prefix settles to a new value.
//
// Populate all exported fields, then optionally call SeedFromLive, then
// call Run. SeedFromLive and Run must not be called concurrently.
type Manager struct {
	Interface string
	Filter    netip.Prefix  // only accept prefixes within this range
	Debounce  time.Duration // 0 = fire immediately
	OnChange  func(Change)

	// InitialPrefix is the last-seen prefix from the state file. Used to
	// populate Change.Old on the first event after a daemon restart.
	// Set before calling SeedFromLive or Run.
	InitialPrefix netip.Prefix
}

// SeedFromLive checks addrs (live addresses from watcher.CurrentAddrs) for a
// matching global unicast /64. If one is found and differs from InitialPrefix,
// it fires OnChange immediately (no debounce) and updates InitialPrefix so
// that Run starts with the correct baseline.
func (m *Manager) SeedFromLive(addrs []netip.Prefix) {
	p, ok := m.firstMatch(addrs)
	if !ok {
		return
	}
	if p == m.InitialPrefix {
		return
	}
	m.doChange(m.InitialPrefix, p)
	m.InitialPrefix = p
}

// Run processes addr events from ch, applying debounce, until ctx is done
// or ch is closed.
func (m *Manager) Run(ctx context.Context, ch <-chan netlinkmon.AddrEvent) {
	current := m.InitialPrefix
	pending := m.InitialPrefix

	var timer *time.Timer
	var timerC <-chan time.Time

	cancelTimer := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
			timerC = nil
		}
	}
	resetTimer := func() {
		cancelTimer()
		timer = time.NewTimer(m.Debounce)
		timerC = timer.C
	}
	commit := func() {
		if pending == current {
			return
		}
		m.doChange(current, pending)
		current = pending
	}

	for {
		select {
		case <-ctx.Done():
			cancelTimer()
			return

		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Iface != m.Interface {
				continue
			}
			p, valid := m.normalize(ev.Addr)
			if !valid {
				continue
			}
			if ev.Added {
				pending = p
			} else if pending == p {
				pending = netip.Prefix{}
			}
			if pending == current {
				cancelTimer()
				continue
			}
			if m.Debounce == 0 {
				commit()
			} else {
				resetTimer()
			}

		case <-timerC:
			timerC = nil
			timer = nil
			commit()
		}
	}
}

func (m *Manager) doChange(old, new netip.Prefix) {
	if m.OnChange == nil {
		return
	}
	var action Action
	switch {
	case !old.IsValid() && new.IsValid():
		action = ActionAdded
	case old.IsValid() && new.IsValid():
		action = ActionChanged
	case old.IsValid() && !new.IsValid():
		action = ActionRemoved
	default:
		return
	}
	slog.Info("prefix change", "iface", m.Interface, "action", action, "old", old, "new", new)
	m.OnChange(Change{Interface: m.Interface, Old: old, New: new, Action: action})
}

// normalize converts addr to a masked /64 and validates it against the filter.
func (m *Manager) normalize(addr netip.Prefix) (netip.Prefix, bool) {
	a := addr.Addr()
	if !a.Is6() || !a.IsGlobalUnicast() {
		return netip.Prefix{}, false
	}
	p64 := netip.PrefixFrom(a, 64).Masked()
	if m.Filter.IsValid() && !m.Filter.Contains(p64.Addr()) {
		return netip.Prefix{}, false
	}
	return p64, true
}

func (m *Manager) firstMatch(addrs []netip.Prefix) (netip.Prefix, bool) {
	for _, a := range addrs {
		if p, ok := m.normalize(a); ok {
			return p, true
		}
	}
	return netip.Prefix{}, false
}
