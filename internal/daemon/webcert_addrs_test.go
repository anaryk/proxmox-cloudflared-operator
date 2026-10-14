package daemon

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

func TestTheAddressesOfTheWebCertificateAreTheNodesOwn(t *testing.T) {
	ip := netip.MustParseAddr
	clusterOf := func(self string) members.Members {
		return members.Members{Self: self, Cluster: "lab", Quorate: true, Nodes: []members.Member{
			{Name: "pve1", ID: 1, Online: true, IP: ip("10.0.0.11")},
			{Name: "pve2", ID: 2, Online: true, IP: ip("10.0.0.12")},
		}}
	}
	standalone := members.Members{Self: "pve1", Standalone: true, Quorate: true, Nodes: []members.Member{{Name: "pve1", Online: true}}}
	// Another node's API marks itself as the local one.
	viaOther := pve.ClusterStatus{Nodes: []pve.ClusterNode{
		{Name: "pve1", Addr: ip("10.0.0.11")},
		{Name: "pve2", Addr: ip("10.0.0.12"), Local: true},
	}}
	own := pve.ClusterStatus{Nodes: []pve.ClusterNode{{Name: "pve1", Addr: ip("192.0.2.10"), Local: true}}}
	tests := []struct {
		name      string
		members   members.Members
		readErr   error
		status    pve.ClusterStatus
		statusErr error
		want      []netip.Addr
		wantErr   string
	}{
		{name: "a member of a cluster", members: clusterOf("pve1"), status: viaOther, want: []netip.Addr{ip("10.0.0.11")}},
		{name: "a members file of another node", members: clusterOf("pve2"), wantErr: "names this node pve2"},
		{name: "a members file that cannot be read", readErr: errors.New("no such file"), wantErr: "no such file"},
		{name: "a member without an address", members: members.Members{Self: "pve1", Cluster: "lab", Nodes: []members.Member{{Name: "pve1", ID: 1}}}, wantErr: "no address for pve1"},
		{name: "a node that stands alone", members: standalone, status: own, want: []netip.Addr{ip("192.0.2.10")}},
		{name: "a node that stands alone, answered by another", members: standalone, status: viaOther, wantErr: "not answered by pve1"},
		{name: "a node that stands alone, its API down", members: standalone, statusErr: errors.New("connection refused"), wantErr: "connection refused"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			read := func() (members.Members, error) { return tt.members, tt.readErr }
			status := func(context.Context) (pve.ClusterStatus, error) { return tt.status, tt.statusErr }
			got, err := ownAddrs(context.Background(), "pve1", read, status)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestTheWebKeeperIssuesNothingWithoutTheNodesOwnAddress(t *testing.T) {
	r := newWebRig(t, webcert.ModeCA)
	old := r.leaf()
	r.k.addrs = func(context.Context) ([]netip.Addr, error) {
		return nil, errors.New("the members file names this node pve2")
	}
	r.now = t0.Add(61 * 24 * time.Hour)

	r.check()

	require.Equal(t, old.Raw, r.leaf().Raw)
	require.Empty(t, r.notes)
	require.Contains(t, r.logs.String(), "names this node pve2")
}
