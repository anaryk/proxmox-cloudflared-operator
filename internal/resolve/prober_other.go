//go:build !linux

package resolve

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// NewHostProber returns a Prober whose every call fails with
// errors.ErrUnsupported: probing the host needs Linux.
func NewHostProber(arpWindow, dialTimeout time.Duration) Prober {
	return unsupportedProber{}
}

type unsupportedProber struct{}

func (unsupportedProber) Interfaces(context.Context) ([]HostIface, error) {
	return nil, unsupported("Interfaces")
}

func (unsupportedProber) ARP(context.Context, string, netip.Addr) ([]string, error) {
	return nil, unsupported("ARP")
}

func (unsupportedProber) Route(context.Context, netip.Addr) (string, bool, error) {
	return "", false, unsupported("Route")
}

func (unsupportedProber) FDBPorts(context.Context, string, int, string) ([]string, error) {
	return nil, unsupported("FDBPorts")
}

func (unsupportedProber) Dial(context.Context, netip.AddrPort) error {
	return unsupported("Dial")
}

func unsupported(method string) error {
	return fmt.Errorf("host prober %s: %w", method, errors.ErrUnsupported)
}
