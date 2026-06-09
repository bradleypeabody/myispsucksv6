//go:build !linux

package lanaddr

import (
	"log/slog"
	"net/netip"
)

func addAddr(iface string, addr netip.Prefix) error {
	slog.Debug("lanaddr: (stub) would add address", "iface", iface, "addr", addr)
	return nil
}

func delAddr(iface string, addr netip.Prefix) error {
	slog.Debug("lanaddr: (stub) would remove address", "iface", iface, "addr", addr)
	return nil
}
