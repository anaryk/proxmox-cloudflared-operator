package pve

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// The cards of the appliance: those it runs with, and one whose addition
// waits for its next start, which holds its slot too.
func TestNICsAreTheCardsOfAGuestWithTheDigest(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/nodes/pve1/lxc/9201/config?current=1": {http.StatusOK, `{"data":{
			"hostname":"pco","net0":"name=eth0,bridge=vmbr0,hwaddr=BC:24:11:92:01:01,ip=dhcp,type=veth",
			"net1":"name=leg1,bridge=vmbr1,hwaddr=02:50:43:4F:00:01,firewall=0,type=veth","netmask":"x","digest":"d1"}}`},
		"/nodes/pve1/lxc/9201/pending": {http.StatusOK, `{"data":[
			{"key":"hostname","value":"pco"},
			{"key":"net0","value":"name=eth0,bridge=vmbr0,hwaddr=BC:24:11:92:01:01,ip=dhcp,type=veth"},
			{"key":"net1","value":"name=leg1,bridge=vmbr1,hwaddr=02:50:43:4F:00:01,firewall=0,type=veth"},
			{"key":"net3","pending":"name=leg2,bridge=vmbr2,hwaddr=02:50:43:4F:00:02,firewall=0,type=veth"},
			{"key":"digest","value":"d1"}]}`},
	})

	nics, digest, err := c.NICs(context.Background(), "pve1", appliance)

	require.NoError(t, err)
	require.Equal(t, "d1", digest)
	require.Equal(t, map[int]string{
		0: "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:92:01:01,ip=dhcp,type=veth",
		1: "name=leg1,bridge=vmbr1,hwaddr=02:50:43:4F:00:01,firewall=0,type=veth",
		3: "name=leg2,bridge=vmbr2,hwaddr=02:50:43:4F:00:02,firewall=0,type=veth",
	}, nics)
	require.Equal(t, "/api2/json/nodes/pve1/lxc/9201/config?current=1", rec.requests()[0].uri)
}

func TestNICsSaysWhatCouldNotBeRead(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/nodes/pve1/lxc/9201/config?current=1": {http.StatusOK, `{"data":{"net0":"name=eth0","digest":"d1"}}`},
	})
	_, _, err := c.NICs(context.Background(), "pve1", appliance)
	require.ErrorContains(t, err, "pending config of lxc/9201")
}

// A bridge is read from the node's network; an SDN vnet, which the node does
// not list there, from the vnets of the cluster, and only then.
func TestVLANAwareReadsTheBridgeOfTheNodeOrTheVnetOfTheCluster(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/nodes/pve1/network": {http.StatusOK, `{"data":[
			{"iface":"vmbr0","type":"bridge","active":1,"bridge_vlan_aware":1},
			{"iface":"vmbr1","type":"bridge","active":1}
		]}`},
		"/cluster/sdn/vnets": {http.StatusOK, `{"data":[
			{"vnet":"tenants","zone":"vz","vlanaware":1},
			{"vnet":"plain","zone":"sz"}
		]}`},
	})

	for bridge, want := range map[string]bool{"vmbr0": true, "vmbr1": false, "tenants": true, "plain": false} {
		aware, err := c.VLANAware(context.Background(), "pve1", bridge)
		require.NoError(t, err, bridge)
		require.Equal(t, want, aware, bridge)
	}
	_, err := c.VLANAware(context.Background(), "pve1", "vmbr7")
	require.EqualError(t, err, "bridge vmbr7 is no bridge of node pve1, nor a vnet of the cluster")

	seen := len(rec.requests())
	_, err = c.VLANAware(context.Background(), "pve1", "vmbr0")
	require.NoError(t, err)
	require.Len(t, rec.requests(), seen+1, "the vnets are read only for a name the node does not list")
}

func TestAddAndRemoveANICUnderTheDigest(t *testing.T) {
	c, seen := newWriteServer(t, reply{http.StatusOK, `{"data":null}`})

	require.NoError(t, c.AddNIC(t.Context(), "pve1", appliance, 2, "name=leg2,bridge=vmbr2,hwaddr=02:50:43:4f:00:02,firewall=0", "d1"))
	require.NoError(t, c.RemoveNIC(t.Context(), "pve1", appliance, 2, "d2"))

	got := seen()
	require.Len(t, got, 2)
	require.Equal(t, "/api2/json/nodes/pve1/lxc/9201/config", got[0].path)
	require.Equal(t, url.Values{"net2": {"name=leg2,bridge=vmbr2,hwaddr=02:50:43:4f:00:02,firewall=0"}, "digest": {"d1"}}, got[0].form)
	require.Equal(t, url.Values{"delete": {"net2"}, "digest": {"d2"}}, got[1].form)
}

func TestAStaleDigestOfANICWriteIsSaidAsSuch(t *testing.T) {
	c, _ := newWriteServer(t, reply{http.StatusInternalServerError,
		`{"data":null,"message":"detected modified configuration - file changed by other user? Try again.\n"}`})
	err := c.AddNIC(t.Context(), "pve1", appliance, 2, "name=leg2,bridge=vmbr2", "d1")
	require.ErrorIs(t, err, ErrStaleDigest)
}

func TestNICIndexReadsNetKeysOnly(t *testing.T) {
	for key, want := range map[string]int{"net0": 0, "net7": 7, "net31": 31} {
		got, ok := NICIndex(key)
		require.True(t, ok, key)
		require.Equal(t, want, got, key)
	}
	for _, key := range []string{"net", "net01", "net-1", "netmask", "ipconfig0", "network", "0"} {
		_, ok := NICIndex(key)
		require.False(t, ok, key)
	}
}
