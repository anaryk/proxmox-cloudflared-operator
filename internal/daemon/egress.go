package daemon

import (
	"context"
	"net/netip"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// watchAgainAfter is how long a watch of the network that failed waits before
// it starts again.
const watchAgainAfter = time.Minute

// watchNetwork runs the watch of the network for the engine until ctx ends:
// an address whose MAC moves is taken out of the egress filter at once and
// verified again. A watch that fails is started again after a while; until
// then a move is seen by the next cycle only.
func watchNetwork(ctx context.Context, eng *engine.Engine,
	watch func(context.Context, func() map[netip.Addr]egress.Pin, func(netip.Addr)) error,
	sleep func(context.Context, time.Duration) error, log zerolog.Logger,
) {
	for {
		err := watch(ctx, eng.Bound, func(addr netip.Addr) { eng.Moved(ctx, addr) })
		if ctx.Err() != nil {
			return
		}
		log.Warn().Err(err).Dur("again", watchAgainAfter).Msg("watching the network for bound addresses that move failed; " +
			"until it runs again, a MAC that moves is seen by the next cycle only")
		if sleep(ctx, watchAgainAfter) != nil {
			return
		}
	}
}

// egressFilter is the filter of the connectors, made once the uid of the
// connector user is known: a user that the package or systemd-sysusers
// creates after the daemon started is taken up by the next call. Until then a
// call fails, unless the admin switched the filter off.
type egressFilter struct {
	nft       egress.Nft
	ov        *egress.Overrides
	uid       func() (uint32, error)
	resolvers func() ([]netip.Addr, error)

	mu sync.Mutex
	f  *egress.Filter
}

func newEgressFilter(nft egress.Nft, local string, uid func() (uint32, error), resolvers func() ([]netip.Addr, error)) *egressFilter {
	return &egressFilter{nft: nft, ov: egress.NewOverrides(local), uid: uid, resolvers: resolvers}
}

// filter returns the filter, made on first use.
func (k *egressFilter) filter() (*egress.Filter, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.f != nil {
		return k.f, nil
	}
	uid, err := k.uid()
	if err != nil {
		return nil, err
	}
	k.f = egress.New(k.nft, uid, k.resolvers, k.ov)
	return k.f, nil
}

// unmade answers a call that found no filter: there is nothing to do while
// the filter is switched off, and the failure stands otherwise.
func (k *egressFilter) unmade(err error) error {
	if _, off, oerr := k.ov.Off(); oerr == nil && off {
		return nil
	}
	return err
}

func (k *egressFilter) Set(ctx context.Context, targets []egress.Target) error {
	f, err := k.filter()
	if err != nil {
		return k.unmade(err)
	}
	return f.Set(ctx, targets)
}

func (k *egressFilter) Remove(ctx context.Context, addr netip.Addr) error {
	f, err := k.filter()
	if err != nil {
		return k.unmade(err)
	}
	return f.Remove(ctx, addr)
}
