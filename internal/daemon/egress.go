package daemon

import (
	"context"
	"net/netip"
	"sync"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
)

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
