package doctor

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// fakeNft lists the tables it holds.
type fakeNft struct {
	tables map[string]bool
	asked  []string
}

func (f *fakeNft) Apply(context.Context, string) error { return errNotHere }

func (f *fakeNft) List(_ context.Context, table string) ([]byte, error) {
	f.asked = append(f.asked, table)
	if !f.tables[table] {
		return nil, gateway.ErrNotLoaded
	}
	return []byte("{}"), nil
}

// sdnProxmox is a Proxmox client that also lists the SDN.
type sdnProxmox struct {
	fakeProxmox
	sdn pve.SDN
}

func (s *sdnProxmox) SDNPending(context.Context) (pve.SDN, error) { return s.sdn, nil }

func TestTheHostReadsWhetherIPv6IsOnOnPconet(t *testing.T) {
	env, _, _ := hostEnv(t)
	env.Proc = t.TempDir()
	dir := filepath.Join(env.Proc, "sys/net/ipv6/conf/pconet")

	on, err := env.PconetIPv6(t.Context())
	require.NoError(t, err)
	require.False(t, on, "no bridge, no IPv6 on it")

	require.NoError(t, os.MkdirAll(dir, 0o700))
	for _, tt := range []struct {
		value string
		on    bool
	}{{"1\n", false}, {"0\n", true}} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "disable_ipv6"), []byte(tt.value), 0o600))
		on, err := env.PconetIPv6(t.Context())
		require.NoError(t, err)
		require.Equal(t, tt.on, on, tt.value)
	}
}

func TestTheHostListsItsTable(t *testing.T) {
	env, _, _ := hostEnv(t)
	nft := &fakeNft{}
	env.HostNft = nft

	require.ErrorIs(t, env.HostTable(t.Context()), gateway.ErrNotLoaded)
	nft.tables = map[string]bool{"pco_host": true}
	require.NoError(t, env.HostTable(t.Context()))
	require.Equal(t, []string{"pco_host", "pco_host"}, nft.asked)
}

func TestTheHostPassesOnTheFactsOfTheNetworks(t *testing.T) {
	env, _, _ := hostEnv(t)
	pins := []store.Pin{{Network: "vmbr1", Addr: netip.MustParseAddr("10.50.0.10"), MAC: "bc:24:11:00:00:01"}}
	env.Pool = func() (int, int, error) { return 3, 65280, nil }
	env.PinsOf = func() ([]store.Pin, time.Time, error) { return pins, now, nil }
	env.Show = func(_ context.Context, unit, property string) (string, error) { return unit + " " + property, nil }
	env.Pinned = func() error { return errNotHere }
	env.Proxmox = &sdnProxmox{sdn: pve.SDN{Zones: []pve.SDNZone{{Name: "lab", State: pve.SDNNew}}}}

	used, size, err := env.ServicePool(t.Context())
	require.NoError(t, err)
	require.Equal(t, [2]int{3, 65280}, [2]int{used, size})
	got, at, err := env.Pins(t.Context())
	require.NoError(t, err)
	require.Equal(t, pins, got)
	require.Equal(t, now, at)
	prop, err := env.UnitProperty(t.Context(), netnsUnit, "ExecStop")
	require.NoError(t, err)
	require.Equal(t, "pco-netns.service ExecStop", prop)
	require.ErrorIs(t, env.NetnsPinned(t.Context()), errNotHere)
	sdn, err := env.SDNPending(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"zone lab (new)"}, sdn.Pending())
}

// What the daemon did not give the host is an error, not a guess.
func TestTheHostSaysWhatItCannotTell(t *testing.T) {
	env, _, _ := hostEnv(t)

	_, _, err := env.ServicePool(t.Context())
	require.ErrorIs(t, err, errNotAtHand)
	_, _, err = env.Pins(t.Context())
	require.ErrorIs(t, err, errNotAtHand)
	_, err = env.SDNPending(t.Context())
	require.ErrorIs(t, err, errNotAtHand)
}

// A store that does not answer does not hold the doctor.
func TestTheStoreReadsOfTheNetworksHaveADeadline(t *testing.T) {
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	env, _, _ := hostEnv(t)
	env.Timeout = 50 * time.Millisecond
	env.Pool = func() (int, int, error) { <-stuck; return 0, 0, nil }
	env.PinsOf = func() ([]store.Pin, time.Time, error) { <-stuck; return nil, time.Time{}, nil }

	_, _, err := env.ServicePool(t.Context())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, _, err = env.Pins(t.Context())
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
