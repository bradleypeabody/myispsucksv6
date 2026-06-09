//go:build !linux

package netlinkmon

import (
	"context"
	"log/slog"
	"net/netip"
	"os"
	"time"
)

// NewWatcher returns a simulator Watcher driven by environment variables.
// Useful for iterating on prefix-pipeline logic on macOS or in CI.
//
// Environment variables:
//
//	MYISPSUCKSV6_SIM_IFACE=enp1s0          interface to simulate events on
//	MYISPSUCKSV6_SIM_PREFIX=2607::/64      initial address reported by CurrentAddrs
//	MYISPSUCKSV6_SIM_CHANGE_TO=2608::/64   if set, simulate a prefix change
//	MYISPSUCKSV6_SIM_CHANGE_DELAY=30s      delay before the change (default 30s)
func NewWatcher() Watcher { return &simWatcher{} }

type simWatcher struct{}

func (w *simWatcher) CurrentAddrs(iface string) ([]netip.Prefix, error) {
	raw := os.Getenv("MYISPSUCKSV6_SIM_PREFIX")
	if raw == "" {
		return nil, nil
	}
	p, err := netip.ParsePrefix(raw)
	if err != nil {
		return nil, err
	}
	return []netip.Prefix{p}, nil
}

func (w *simWatcher) Subscribe(ctx context.Context) (<-chan AddrEvent, error) {
	out := make(chan AddrEvent, 8)

	iface := os.Getenv("MYISPSUCKSV6_SIM_IFACE")
	changeTo := os.Getenv("MYISPSUCKSV6_SIM_CHANGE_TO")

	if iface == "" || changeTo == "" {
		go func() {
			<-ctx.Done()
			close(out)
		}()
		return out, nil
	}

	newPrefix, err := netip.ParsePrefix(changeTo)
	if err != nil {
		close(out)
		return nil, err
	}
	delayStr := os.Getenv("MYISPSUCKSV6_SIM_CHANGE_DELAY")
	if delayStr == "" {
		delayStr = "30s"
	}
	delay, err := time.ParseDuration(delayStr)
	if err != nil {
		close(out)
		return nil, err
	}

	var initialPrefix netip.Prefix
	if raw := os.Getenv("MYISPSUCKSV6_SIM_PREFIX"); raw != "" {
		initialPrefix, _ = netip.ParsePrefix(raw)
	}

	go func() {
		defer close(out)
		slog.Info("sim: scheduled prefix change", "iface", iface, "to", newPrefix, "in", delay)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		if initialPrefix.IsValid() {
			select {
			case out <- AddrEvent{Iface: iface, Addr: initialPrefix, Added: false}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case out <- AddrEvent{Iface: iface, Addr: newPrefix, Added: true}:
		case <-ctx.Done():
			return
		}
		<-ctx.Done()
	}()
	return out, nil
}
