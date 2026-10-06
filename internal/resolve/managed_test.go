package resolve_test

import (
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/annotation"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The managed network is named in packages that cannot take the name from
// one another without an import cycle: the gateway's bridge, the id the
// store keeps the network under, and the word of via= the grammar reads are
// the names the resolver looks for.
func TestTheManagedNetworkHasOneNameEverywhere(t *testing.T) {
	require.Equal(t, gateway.ManagedBridge, resolve.ManagedBridge)

	parsed := annotation.Parse("```cf-tunnel\na.example.com -> :80 via=managed\n```")
	require.Len(t, parsed.Entries, 1, "%v", parsed.Errors)
	require.Equal(t, resolve.ViaManaged, parsed.Entries[0].Options.Via)

	base := t.TempDir()
	s, err := store.Open(store.Paths{
		Cluster: filepath.Join(base, "cluster"),
		Private: filepath.Join(base, "private"),
		Local:   filepath.Join(base, "local"),
	})
	require.NoError(t, err)
	require.NoError(t, s.Init())
	managed := store.Network{
		ID: resolve.ManagedBridge, Kind: store.NetworkManaged, Bridge: resolve.ManagedBridge,
		Prefix:    netip.MustParsePrefix("10.203.118.0/24"),
		Legs:      map[string]netip.Addr{"pve1": netip.MustParseAddr("10.203.118.1")},
		LegMAC:    store.LegMACFor(store.NetworkManaged, 1),
		Mark:      1,
		CreatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}
	require.NoError(t, s.SaveNetwork(managed, 0), "the store takes the managed network under this name only")
}
