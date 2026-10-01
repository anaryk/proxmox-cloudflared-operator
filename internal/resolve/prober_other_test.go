//go:build !linux

package resolve

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHostProberUnsupported(t *testing.T) {
	p := NewHostProber(0, 0)
	addr := netip.MustParseAddr("10.20.0.5")

	_, err := p.Interfaces(t.Context())
	require.ErrorIs(t, err, errors.ErrUnsupported)
	_, err = p.ARP(t.Context(), "vmbr0", addr)
	require.ErrorIs(t, err, errors.ErrUnsupported)
	_, _, err = p.FDBPort(t.Context(), "vmbr0", 0, "bc:24:11:00:00:01")
	require.ErrorIs(t, err, errors.ErrUnsupported)
	err = p.Dial(t.Context(), netip.AddrPortFrom(addr, 80))
	require.ErrorIs(t, err, errors.ErrUnsupported)
}
