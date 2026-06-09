package netlinkmon

import (
	"context"
	"net/netip"
)

// AddrEvent is a kernel notification that an IPv6 address was added or
// removed on an interface.
type AddrEvent struct {
	Iface string
	Addr  netip.Prefix // full host address with prefix length as reported by kernel
	Added bool         // true = added, false = removed
}

// Watcher observes kernel address changes and reports current addresses.
// NewWatcher returns the platform-appropriate implementation.
type Watcher interface {
	// CurrentAddrs returns all global unicast IPv6 addresses currently
	// assigned to iface. Used at startup to seed the prefix manager without
	// waiting for a netlink event.
	CurrentAddrs(iface string) ([]netip.Prefix, error)

	// Subscribe returns a channel that delivers AddrEvents until ctx is
	// cancelled. The channel is closed when the subscription ends.
	Subscribe(ctx context.Context) (<-chan AddrEvent, error)
}
