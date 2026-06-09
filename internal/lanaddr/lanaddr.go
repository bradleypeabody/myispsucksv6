package lanaddr

import (
	"log/slog"
	"net/netip"
)

// Manager tracks and assigns the router's LAN address for one proxy entry.
// The assigned address is computed as the network bits from the prefix ORed
// with the host bits from Suffix (e.g. prefix=2607::/64, suffix=::1 → 2607::1).
type Manager struct {
	Interface string
	Suffix    netip.Addr // from config lan_host_suffix
	current   netip.Addr // zero when no address is currently assigned
}

// Compute returns the LAN address for a given /64 prefix and host suffix.
// It takes the upper 8 bytes (network) from the masked prefix and the lower
// 8 bytes (host) from suffix.
func Compute(prefix netip.Prefix, suffix netip.Addr) netip.Addr {
	p := prefix.Masked().Addr().As16()
	s := suffix.As16()
	var out [16]byte
	copy(out[:8], p[:8])
	copy(out[8:], s[8:])
	return netip.AddrFrom16(out)
}

// SetPrefix updates the LAN address to match the new prefix.
// If prefix is zero (ActionRemoved), the current address is removed.
// New-before-old ordering: the new address is added before the old one is
// removed, so there is no gap in connectivity.
func (m *Manager) SetPrefix(p netip.Prefix) {
	var newAddr netip.Addr
	if p.IsValid() {
		newAddr = Compute(p, m.Suffix)
	}
	if newAddr == m.current {
		return
	}

	if newAddr.IsValid() {
		if err := addAddr(m.Interface, netip.PrefixFrom(newAddr, 64)); err != nil {
			slog.Warn("lanaddr: failed to add address", "iface", m.Interface, "addr", newAddr, "err", err)
		} else {
			slog.Info("lanaddr: added address", "iface", m.Interface, "addr", newAddr)
		}
	}

	if m.current.IsValid() {
		if err := delAddr(m.Interface, netip.PrefixFrom(m.current, 64)); err != nil {
			slog.Warn("lanaddr: failed to remove address", "iface", m.Interface, "addr", m.current, "err", err)
		} else {
			slog.Info("lanaddr: removed address", "iface", m.Interface, "addr", m.current)
		}
	}

	m.current = newAddr
}

// Current returns the LAN address currently assigned by this manager (zero if none).
func (m *Manager) Current() netip.Addr {
	return m.current
}
