package pve

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSDNVnetReadsOneVnetInThreeCalls(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/cluster/sdn/vnets/pconet?pending=1": {http.StatusOK, `{"data":{"vnet":"pconet","zone":"pco","isolate-ports":1,"digest":"d"}}`},
		"/cluster/sdn/zones/pco?pending=1":    {http.StatusOK, `{"data":{"zone":"pco","type":"simple","digest":"d"}}`},
		"/cluster/sdn/vnets/pconet/subnets?pending=1": {http.StatusOK, `{"data":[
			{"vnet":"pconet","cidr":"10.9.0.0/24","subnet":"pco-10.9.0.0-24","state":"deleted"},
			{"vnet":"pconet","cidr":"10.9.1.0/24","subnet":"pco-10.9.1.0-24"}
		]}`},
	})

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

func TestSDNVnetThatIsNotThere(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/cluster/sdn/vnets/pconet?pending=1": {http.StatusInternalServerError, `{"data":null,"message":"sdn vnet object 'pconet' does not exist\n"}`},
	})

	got, err := c.SDNVnet(context.Background(), "pconet")

	require.NoError(t, err)
	require.False(t, got.Found)
	require.Len(t, rec.requests(), 1)

	_, err = c.SDNVnet(context.Background(), "../zones")
	require.ErrorContains(t, err, "invalid vnet name")
}

func TestSDNVnetFailsOnAnErrorOfTheZone(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/cluster/sdn/vnets/pconet?pending=1": {http.StatusOK, `{"data":{"vnet":"pconet","zone":"pco"}}`},
		"/cluster/sdn/zones/pco?pending=1":    {http.StatusForbidden, `{"data":null,"message":"Permission check failed"}`},
	})

	_, err := c.SDNVnet(context.Background(), "pconet")

	require.ErrorContains(t, err, "zone pco")
}
