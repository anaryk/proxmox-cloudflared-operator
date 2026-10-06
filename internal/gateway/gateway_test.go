package gateway

import (
	"errors"
	"fmt"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

// The transit link is the first /30 of the service prefix, and service
// addresses are allocated above it.
func TestTheTransitLinkIsTheFirstBlockOfTheServicePrefix(t *testing.T) {
	require.Equal(t, "198.18.0.0/16", ServicePrefix.String())
	transit := netip.MustParsePrefix("198.18.0.0/30")
	for _, a := range []netip.Addr{TransitHost, TransitNS} {
		require.True(t, transit.Contains(a), "%s", a)
		require.True(t, ServicePrefix.Contains(a), "%s", a)
	}
	require.NotEqual(t, TransitHost, TransitNS)
}

// A block is 4096 ports: a guest keeps a source port in TIME_WAIT for 60
// seconds, and a block of 1024 ports refused SYNs above about 17 new
// connections a second to one target. 15 blocks end at 62463, and a 16th would
// pass 65535.
func TestEachMarkHasItsOwnBlockOfSourcePorts(t *testing.T) {
	require.Equal(t, 4096, PortBlockSize)
	require.Equal(t, 15, MaxMark)
	lo, hi := PortBlock(1)
	require.Equal(t, [2]uint16{1024, 5119}, [2]uint16{lo, hi})
	lo, hi = PortBlock(2)
	require.Equal(t, [2]uint16{5120, 9215}, [2]uint16{lo, hi})
	lo, hi = PortBlock(MaxMark)
	require.Equal(t, [2]uint16{58368, 62463}, [2]uint16{lo, hi})
	for m := 2; m <= MaxMark; m++ {
		_, prevHi := PortBlock(m - 1)
		lo, hi := PortBlock(m)
		require.Equal(t, prevHi+1, lo, "mark %d", m)
		require.Equal(t, lo+PortBlockSize-1, hi, "mark %d", m)
	}
	require.Greater(t, 1024+(MaxMark+1)*PortBlockSize-1, 65535, "a block more would pass the last port")
}

func TestALegIsNamedAfterItsMark(t *testing.T) {
	require.Equal(t, "leg7", Leg{Mark: 7}.Iface())
	require.Equal(t, "leg15", LegIface(15))
}

func TestAChangedErrorNamesTheFirstEightDifferences(t *testing.T) {
	var d []string
	for i := range 10 {
		d = append(d, fmt.Sprintf("difference %d", i+1))
	}
	err := error(&ChangedError{Differences: d})
	require.ErrorIs(t, err, ErrChanged)
	require.Equal(t, "difference 1; difference 2; difference 3; difference 4; difference 5; difference 6; "+
		"difference 7; difference 8; and 2 more", err.Error())
	var ce *ChangedError
	require.True(t, errors.As(err, &ce))
	require.Equal(t, "difference 1", (&ChangedError{Differences: d[:1]}).Error())
}
