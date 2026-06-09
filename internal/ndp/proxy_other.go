//go:build !linux

package ndp

import (
	"context"
	"log/slog"
)

// Run is a no-op on non-Linux platforms.
func (p *Proxy) Run(ctx context.Context) error {
	p.lazyInit()
	slog.Warn("ndp: NDP proxy not supported on this platform")
	<-ctx.Done()
	return nil
}
