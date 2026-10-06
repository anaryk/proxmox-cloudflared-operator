package pve

import (
	"context"
	"net/http"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSubnets(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/cluster/sdn/vnets":                okReply(t, "sdn_vnets.json"),
		"/cluster/sdn/vnets/pcotv1/subnets": okReply(t, "sdn_subnets.json"),
	})
	got, err := c.Subnets(context.Background())
	require.NoError(t, err)
	require.Equal(t, []Subnet{{
		Vnet:    "pcotv1",
		Prefix:  netip.MustParsePrefix("10.92.1.0/24"),
		Gateway: netip.MustParseAddr("10.92.1.1"),
	}}, got)
	require.Len(t, rec.requests(), 2)
}

func TestVNetsHaveTheirZone(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{"/cluster/sdn/vnets": okReply(t, "sdn_vnets.json")})

	got, err := c.VNets(context.Background())

	require.NoError(t, err)
	require.Equal(t, []VNet{{Name: "pcotv1", Zone: "pcotest"}}, got)
	require.Len(t, rec.requests(), 1, "the subnets are not asked")
}

func TestVNetsRejectBadAnswers(t *testing.T) {
	for _, tt := range []struct{ name, vnets string }{
		{"a vnet without a name", `[{"zone":"z"}]`},
		{"a vnet name leaving its segment", `[{"vnet":"../zones","zone":"z"}]`},
		{"a vnet without a zone", `[{"vnet":"a"}]`},
		{"a zone leaving its segment", `[{"vnet":"a","zone":"a/b"}]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, map[string]reply{"/cluster/sdn/vnets": {http.StatusOK, `{"data":` + tt.vnets + `}`}})

			got, err := c.VNets(context.Background())

			require.Error(t, err)
			require.Nil(t, got)
		})
	}
}

func TestVNetsWithoutVnets(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{"/cluster/sdn/vnets": {http.StatusOK, `{"data":[]}`}})

	got, err := c.VNets(context.Background())

	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSubnetsForms(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{
		"/cluster/sdn/vnets": {http.StatusOK, `{"data":[{"vnet":"a","zone":"z"},{"vnet":"b","zone":"z"}]}`},
		"/cluster/sdn/vnets/a/subnets": {http.StatusOK, `{"data":[
			{"cidr":"10.1.0.0/24","vnet":"a"},
			{"cidr":"fd00:1::/64","gateway":"fd00:1::1","vnet":"a"}
		]}`},
		"/cluster/sdn/vnets/b/subnets": {http.StatusOK, `{"data":[]}`},
	})
	got, err := c.Subnets(context.Background())
	require.NoError(t, err)
	require.Equal(t, []Subnet{{Vnet: "a", Prefix: netip.MustParsePrefix("10.1.0.0/24")}}, got,
		"a subnet without a gateway has none; IPv6 subnets are left out")
}

func TestSubnetsWithoutVnets(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{"/cluster/sdn/vnets": {http.StatusOK, `{"data":[]}`}})
	got, err := c.Subnets(context.Background())
	require.NoError(t, err)
	require.Empty(t, got)
	require.Len(t, rec.requests(), 1)
}

func TestSubnetsRejectBadAnswers(t *testing.T) {
	for _, tt := range []struct {
		name, vnets, subnets string
	}{
		{"vnet without a name", `[{"zone":"z"}]`, `[]`},
		{"vnet name leaving its segment", `[{"vnet":"../zones"}]`, `[]`},
		{"bad cidr", `[{"vnet":"a"}]`, `[{"cidr":"10.1.0.0"}]`},
		{"bad gateway", `[{"vnet":"a"}]`, `[{"cidr":"10.1.0.0/24","gateway":"10.1.0.300"}]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, map[string]reply{
				"/cluster/sdn/vnets":           {http.StatusOK, `{"data":` + tt.vnets + `}`},
				"/cluster/sdn/vnets/a/subnets": {http.StatusOK, `{"data":` + tt.subnets + `}`},
			})
			got, err := c.Subnets(context.Background())
			require.Error(t, err)
			require.Nil(t, got)
		})
	}
}
