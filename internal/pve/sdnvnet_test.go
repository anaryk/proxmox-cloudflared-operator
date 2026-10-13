package pve

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// sdnAuditOnly answers as Proxmox does a token that holds SDN.Audit and not
// SDN.Allocate: the listings answer, the single zone is refused.
func sdnAuditOnly(routes map[string]reply) map[string]reply {
	forbidden := reply{http.StatusForbidden, `{"data":null,"message":"Permission check failed (/sdn/zones/pco, SDN.Allocate)\n"}`}
	for _, path := range []string{
		"/cluster/sdn/zones/pco?pending=1",
		"/cluster/sdn/zones/pco",
		"/cluster/sdn/vnets/pconet?pending=1",
		"/cluster/sdn/vnets/pconet",
	} {
		routes[path] = forbidden
	}
	return routes
}

func TestSDNVnetReadsOneVnetInThreeCallsOfTheListings(t *testing.T) {
	c, rec := newTestClient(t, sdnAuditOnly(map[string]reply{
		"/cluster/sdn/vnets?pending=1": {http.StatusOK, `{"data":[
			{"vnet":"other","zone":"lab","type":"vnet"},
			{"vnet":"pconet","zone":"pco","isolate-ports":1,"digest":"d","type":"vnet"}
		]}`},
		"/cluster/sdn/zones?pending=1": {http.StatusOK, `{"data":[
			{"zone":"lab","type":"vlan"},
			{"zone":"pco","type":"simple","digest":"d"}
		]}`},
		"/cluster/sdn/vnets/pconet/subnets?pending=1": {http.StatusOK, `{"data":[
			{"vnet":"pconet","cidr":"10.9.0.0/24","subnet":"pco-10.9.0.0-24","state":"deleted"},
			{"vnet":"pconet","cidr":"10.9.1.0/24","subnet":"pco-10.9.1.0-24"}
		]}`},
	}))

	got, err := c.SDNVnet(context.Background(), "pconet")

	require.NoError(t, err)
	require.Equal(t, SDNVnetDetail{
		Vnet:    SDNVnet{Name: "pconet", Zone: "pco", IsolatePorts: true},
		Zone:    SDNZone{Name: "pco", Type: "simple"},
		Subnets: []SDNSubnet{{Vnet: "pconet", CIDR: "10.9.0.0/24", State: SDNDeleted}, {Vnet: "pconet", CIDR: "10.9.1.0/24"}},
		Found:   true,
	}, got)
	require.Len(t, rec.requests(), 3, "three calls whatever the SDN holds")
}

func TestSDNVnetKeepsWhatIsPending(t *testing.T) {
	c, _ := newTestClient(t, sdnAuditOnly(map[string]reply{
		"/cluster/sdn/vnets?pending=1":                {http.StatusOK, `{"data":[{"vnet":"pconet","zone":"pco","state":"new","pending":{"isolate-ports":1}}]}`},
		"/cluster/sdn/zones?pending=1":                {http.StatusOK, `{"data":[{"zone":"pco","type":"simple","state":"new"}]}`},
		"/cluster/sdn/vnets/pconet/subnets?pending=1": {http.StatusOK, `{"data":[]}`},
	}))

	got, err := c.SDNVnet(context.Background(), "pconet")

	require.NoError(t, err)
	require.Equal(t, SDNVnet{Name: "pconet", Zone: "pco", IsolatePorts: true, State: SDNNew}, got.Vnet)
	require.Equal(t, SDNZone{Name: "pco", Type: "simple", State: SDNNew}, got.Zone)
	require.Empty(t, got.Subnets)
}

func TestSDNVnetThatIsNotThere(t *testing.T) {
	c, rec := newTestClient(t, sdnAuditOnly(map[string]reply{
		"/cluster/sdn/vnets?pending=1": {http.StatusOK, `{"data":[{"vnet":"other","zone":"lab"}]}`},
	}))

	got, err := c.SDNVnet(context.Background(), "pconet")

	require.NoError(t, err)
	require.False(t, got.Found)
	require.Len(t, rec.requests(), 1)

	_, err = c.SDNVnet(context.Background(), "../zones")
	require.ErrorContains(t, err, "invalid vnet name")
}

// The listing leaves out a zone the token may not audit, as it leaves out
// one that is not there: the vnet is read without it.
func TestSDNVnetWithoutItsZone(t *testing.T) {
	c, _ := newTestClient(t, sdnAuditOnly(map[string]reply{
		"/cluster/sdn/vnets?pending=1":                {http.StatusOK, `{"data":[{"vnet":"pconet","zone":"pco"}]}`},
		"/cluster/sdn/zones?pending=1":                {http.StatusOK, `{"data":[{"zone":"lab","type":"vlan"}]}`},
		"/cluster/sdn/vnets/pconet/subnets?pending=1": {http.StatusOK, `{"data":[]}`},
	}))

	got, err := c.SDNVnet(context.Background(), "pconet")

	require.NoError(t, err)
	require.True(t, got.Found)
	require.Equal(t, SDNZone{}, got.Zone)
}

func TestSDNVnetFailsOnAnErrorOfTheZones(t *testing.T) {
	c, _ := newTestClient(t, sdnAuditOnly(map[string]reply{
		"/cluster/sdn/vnets?pending=1": {http.StatusOK, `{"data":[{"vnet":"pconet","zone":"pco"}]}`},
		"/cluster/sdn/zones?pending=1": {http.StatusInternalServerError, `{"data":null,"message":"cfs lock timeout"}`},
	}))

	_, err := c.SDNVnet(context.Background(), "pconet")

	require.ErrorContains(t, err, "zone pco")
}

func TestSDNVnetFailsOnTwoVnetsOfOneName(t *testing.T) {
	c, _ := newTestClient(t, sdnAuditOnly(map[string]reply{
		"/cluster/sdn/vnets?pending=1": {http.StatusOK, `{"data":[{"vnet":"pconet","zone":"pco"},{"vnet":"pconet","zone":"lab"}]}`},
	}))

	_, err := c.SDNVnet(context.Background(), "pconet")

	require.ErrorContains(t, err, "vnet pconet")
}
