//go:build linux

package netlinkmon

import (
	"context"
	"net/netip"

	"github.com/vishvananda/netlink"
)

// NewWatcher returns the Linux netlink-backed Watcher.
func NewWatcher() Watcher { return &nlWatcher{} }

type nlWatcher struct{}

func (w *nlWatcher) CurrentAddrs(iface string) ([]netip.Prefix, error) {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return nil, err
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, a := range addrs {
		ip, ok := netip.AddrFromSlice(a.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if !ip.IsGlobalUnicast() {
			continue
		}
		ones, _ := a.Mask.Size()
		out = append(out, netip.PrefixFrom(ip, ones))
	}
	return out, nil
}

func (w *nlWatcher) Subscribe(ctx context.Context) (<-chan AddrEvent, error) {
	updates := make(chan netlink.AddrUpdate, 32)
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(done)
	}()
	if err := netlink.AddrSubscribe(updates, done); err != nil {
		return nil, err
	}
	out := make(chan AddrEvent, 32)
	go func() {
		defer close(out)
		for u := range updates {
			link, err := netlink.LinkByIndex(u.LinkIndex)
			if err != nil {
				continue
			}
			ip, ok := netip.AddrFromSlice(u.LinkAddress.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			ones, _ := u.LinkAddress.Mask.Size()
			select {
			case out <- AddrEvent{
				Iface: link.Attrs().Name,
				Addr:  netip.PrefixFrom(ip, ones),
				Added: u.NewAddr,
			}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
