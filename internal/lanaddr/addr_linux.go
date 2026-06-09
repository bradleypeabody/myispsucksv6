//go:build linux

package lanaddr

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"github.com/vishvananda/netlink"
)

func addAddr(iface string, addr netip.Prefix) error {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("looking up interface %q: %w", iface, err)
	}
	err = netlink.AddrAdd(link, &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   addr.Addr().AsSlice(),
			Mask: net.CIDRMask(addr.Bits(), 128),
		},
	})
	if errors.Is(err, syscall.EEXIST) {
		return nil // address already present — no-op
	}
	return err
}

func delAddr(iface string, addr netip.Prefix) error {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("looking up interface %q: %w", iface, err)
	}
	return netlink.AddrDel(link, &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   addr.Addr().AsSlice(),
			Mask: net.CIDRMask(addr.Bits(), 128),
		},
	})
}
