package ndp

import (
	"net/netip"
	"sync"
	"sync/atomic"
)

type proxyError struct{ msg string }

func (e *proxyError) Error() string { return e.msg }

// Proxy is an NDP proxy that watches for Neighbor Solicitations on a WAN
// interface and answers with Neighbor Advertisements when the target is
// confirmed live on the LAN. Populate exported fields, then call Run.
// SetPrefix may be called from any goroutine.
type Proxy struct {
	WANInterface string
	LANInterface string

	prefix atomic.Value // stores netip.Prefix

	once    sync.Once
	updateCh chan netip.Prefix
}

func (p *Proxy) lazyInit() {
	p.once.Do(func() {
		p.updateCh = make(chan netip.Prefix, 1)
	})
}

// SetPrefix sets the proxied prefix. NS for addresses outside this prefix are ignored.
func (p *Proxy) SetPrefix(pfx netip.Prefix) {
	p.lazyInit()
	select {
	case <-p.updateCh:
	default:
	}
	p.updateCh <- pfx
}
