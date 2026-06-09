package ra

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/mdlayher/ndp"

	"github.com/bradleypeabody/myispsucksv6/internal/lanaddr"
)

var (
	allNodes   = netip.MustParseAddr("ff02::1")
	allRouters = netip.MustParseAddr("ff02::2")
)

// Emitter sends Router Advertisements on a LAN interface. Populate exported
// fields, then call Run. SetPrefix may be called from any goroutine.
type Emitter struct {
	Interface         string
	Suffix            netip.Addr // lan_host_suffix; used to resolve "self" in DNSServers
	Disable           bool       // if true, Run is a no-op (hooks/radvd path)
	IntervalSeconds   int        // max unsolicited RA interval, default 200
	RouterLifetime    int        // router lifetime in seconds, default 1800
	MTU               int        // MTU option value, default 1500
	DNSServers        []string   // ["self"] or list of IPv6 addrs; default ["self"]
	ValidLifetime     int        // prefix valid lifetime in seconds, default 86400
	PreferredLifetime int        // prefix preferred lifetime in seconds, default 14400

	once     sync.Once
	updateCh chan netip.Prefix
}

func (e *Emitter) lazyInit() {
	e.once.Do(func() {
		e.updateCh = make(chan netip.Prefix, 1)
	})
}

// SetPrefix notifies the emitter of a new WAN prefix. Only the most recent
// unprocessed update is kept; older pending values are dropped.
func (e *Emitter) SetPrefix(p netip.Prefix) {
	e.lazyInit()
	select {
	case <-e.updateCh:
	default:
	}
	e.updateCh <- p
}

// Run starts the RA emitter and blocks until ctx is done.
// If Disable is true, Run returns as soon as ctx is done without opening any socket.
func (e *Emitter) Run(ctx context.Context) error {
	e.lazyInit()

	if e.Disable {
		<-ctx.Done()
		return nil
	}

	ifi, err := net.InterfaceByName(e.Interface)
	if err != nil {
		return fmt.Errorf("ra: interface %q: %w", e.Interface, err)
	}

	conn, _, err := ndp.Listen(ifi, ndp.LinkLocal)
	if err != nil {
		return fmt.Errorf("ra: listen on %s: %w", e.Interface, err)
	}

	// Closing conn unblocks any pending ReadFrom.
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	if err := conn.JoinGroup(allRouters); err != nil {
		return fmt.Errorf("ra: join ff02::2 on %s: %w", e.Interface, err)
	}
	slog.Info("ra: emitter started", "iface", e.Interface)

	rsCh := make(chan netip.Addr, 4)
	errCh := make(chan error, 1)
	go func() {
		for {
			msg, _, src, err := conn.ReadFrom()
			if err != nil {
				errCh <- err
				return
			}
			if _, ok := msg.(*ndp.RouterSolicitation); ok {
				select {
				case rsCh <- src:
				default:
				}
			}
		}
	}()

	interval := e.raInterval()

	var (
		current netip.Prefix
		timer   *time.Timer
		timerC  <-chan time.Time
	)

	resetTimer := func() {
		if timer != nil {
			timer.Stop()
		}
		timer = time.NewTimer(interval + jitter(interval))
		timerC = timer.C
	}

	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return nil

		case err := <-errCh:
			if ctx.Err() != nil {
				return nil // conn closed by ctx cancellation
			}
			return fmt.Errorf("ra: read on %s: %w", e.Interface, err)

		case p := <-e.updateCh:
			old := current
			current = p
			if old.IsValid() && old != current {
				slog.Debug("ra: sending deprecation RA", "iface", e.Interface, "prefix", old)
				msg := e.buildRA(ifi, old, 2*time.Minute, 0)
				if err := conn.WriteTo(msg, nil, allNodes); err != nil && ctx.Err() == nil {
					slog.Warn("ra: failed to send deprecation RA", "iface", e.Interface, "prefix", old, "err", err)
				}
			}
			if current.IsValid() {
				dns := e.resolveDNS(current)
				slog.Debug("ra: sending prefix-update RA", "iface", e.Interface, "prefix", current,
					"valid", e.validLifetime(), "preferred", e.preferredLifetime(), "dns", dns)
				msg := e.buildRA(ifi, current, e.validLifetime(), e.preferredLifetime())
				if err := conn.WriteTo(msg, nil, allNodes); err != nil && ctx.Err() == nil {
					slog.Warn("ra: failed to send RA on prefix update", "iface", e.Interface, "prefix", current, "err", err)
				}
				resetTimer()
			}

		case src := <-rsCh:
			if current.IsValid() {
				dns := e.resolveDNS(current)
				slog.Debug("ra: sending solicited RA", "iface", e.Interface, "prefix", current, "dst", src, "dns", dns)
				msg := e.buildRA(ifi, current, e.validLifetime(), e.preferredLifetime())
				if err := conn.WriteTo(msg, nil, src); err != nil && ctx.Err() == nil {
					slog.Warn("ra: failed to send solicited RA", "iface", e.Interface, "src", src, "err", err)
				}
			}

		case <-timerC:
			timerC = nil
			timer = nil
			if current.IsValid() {
				slog.Debug("ra: sending periodic RA", "iface", e.Interface, "prefix", current)
				msg := e.buildRA(ifi, current, e.validLifetime(), e.preferredLifetime())
				if err := conn.WriteTo(msg, nil, allNodes); err != nil && ctx.Err() == nil {
					slog.Warn("ra: failed to send periodic RA", "iface", e.Interface, "err", err)
				}
			}
			resetTimer()
		}
	}
}

// buildRA constructs a RouterAdvertisement for the given prefix and lifetimes.
func (e *Emitter) buildRA(ifi *net.Interface, prefix netip.Prefix, validLifetime, preferredLifetime time.Duration) *ndp.RouterAdvertisement {
	options := []ndp.Option{
		&ndp.PrefixInformation{
			PrefixLength:                   64,
			OnLink:                         true,
			AutonomousAddressConfiguration: true,
			ValidLifetime:                  validLifetime,
			PreferredLifetime:              preferredLifetime,
			Prefix:                         prefix.Masked().Addr(),
		},
		&ndp.LinkLayerAddress{
			Direction: ndp.Source,
			Addr:      ifi.HardwareAddr,
		},
		&ndp.MTU{MTU: uint32(e.mtu())},
	}

	if servers := e.resolveDNS(prefix); len(servers) > 0 {
		options = append(options, &ndp.RecursiveDNSServer{
			Lifetime: e.routerLifetimeDuration(),
			Servers:  servers,
		})
	}

	return &ndp.RouterAdvertisement{
		CurrentHopLimit: 64,
		RouterLifetime:  e.routerLifetimeDuration(),
		Options:         options,
	}
}

// resolveDNS converts the DNSServers config (including "self") into netip.Addrs.
func (e *Emitter) resolveDNS(prefix netip.Prefix) []netip.Addr {
	servers := e.DNSServers
	if len(servers) == 0 {
		servers = []string{"self"}
	}
	out := make([]netip.Addr, 0, len(servers))
	for _, s := range servers {
		if s == "self" {
			if prefix.IsValid() && e.Suffix.IsValid() {
				out = append(out, lanaddr.Compute(prefix, e.Suffix))
			}
		} else {
			a, err := netip.ParseAddr(s)
			if err != nil {
				slog.Warn("ra: invalid DNS server address in config", "addr", s, "err", err)
				continue
			}
			out = append(out, a)
		}
	}
	return out
}

func (e *Emitter) raInterval() time.Duration {
	s := e.IntervalSeconds
	if s <= 0 {
		s = 200
	}
	return time.Duration(s) * time.Second
}

func (e *Emitter) routerLifetimeDuration() time.Duration {
	s := e.RouterLifetime
	if s <= 0 {
		s = 1800
	}
	return time.Duration(s) * time.Second
}

func (e *Emitter) mtu() int {
	if e.MTU <= 0 {
		return 1500
	}
	return e.MTU
}

func (e *Emitter) validLifetime() time.Duration {
	s := e.ValidLifetime
	if s <= 0 {
		s = 86400
	}
	return time.Duration(s) * time.Second
}

func (e *Emitter) preferredLifetime() time.Duration {
	s := e.PreferredLifetime
	if s <= 0 {
		s = 14400
	}
	return time.Duration(s) * time.Second
}

// jitter returns a random offset in [-d/4, +d/4].
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	quarter := int64(d / 4)
	return time.Duration(rand.Int63n(2*quarter) - quarter)
}
